package chrome

// Tab lifecycle: closing tabs, foregrounding one, and moving through its
// history. See docs/rfc/0007-tab-lifecycle.md.

import (
	"context"
	"errors"
	"fmt"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	cdptarget "github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// ErrNoHistory reports that a history navigation had no entry in the requested
// direction. Going nowhere is deliberately an error rather than a silent no-op:
// a wizard script that quietly failed to go back would carry on against the
// wrong page.
//
// Its message is part of the contract: match it with IsNoHistory rather than
// errors.Is at call sites.
var ErrNoHistory = errors.New("no history entry in that direction")

// IsNoHistory reports whether err is ErrNoHistory.
func IsNoHistory(err error) bool { return errIs(err, ErrNoHistory) }

// CloseTabs closes each tab by id (Target.closeTarget at the browser level) and
// reports what went, so a caller that closed by filter can see exactly which
// tabs it destroyed. The url/title are read BEFORE closing — afterwards there is
// nothing left to describe.
//
// A failure part-way through does not discard the tabs that did close: those are
// still reported, with the failures listed alongside. Only a close that achieved
// nothing is an error, so the caller sees `cdp_error` for "it didn't work"
// rather than for "it half worked".
func (c *CDP) CloseTabs(ctx context.Context, targetIDs []string) (map[string]any, error) {
	known := map[string]target.Info{}
	if tabs, err := c.List(ctx); err == nil {
		for _, t := range tabs {
			known[t.ID] = t
		}
	}

	closed := make([]any, 0, len(targetIDs))
	closedIDs := make([]string, 0, len(targetIDs))
	var failed []any
	var firstErr error
	for _, id := range targetIDs {
		if err := c.closeTarget(ctx, id); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			failed = append(failed, map[string]any{"id": id, "error": err.Error()})
			continue
		}
		c.forget(id)
		closedIDs = append(closedIDs, id)
		t := known[id]
		closed = append(closed, map[string]any{"id": id, "url": t.URL, "title": t.Title})
	}
	if len(closed) == 0 && firstErr != nil {
		return nil, firstErr
	}
	// Only the tabs that actually closed are worth waiting on: a tab whose close
	// FAILED never leaves the list, so including it would burn the whole cap
	// before returning a result already known.
	c.awaitGone(ctx, closedIDs)
	res := map[string]any{"closed": closed, "count": len(closed)}
	if len(failed) > 0 {
		res["failed"] = failed
	}
	return res, nil
}

// Activate foregrounds a tab and raises its window — the documented remedy for
// the accessibility-tree throttling that makes --by name/ref/cell stall with
// `tab_hidden`.
//
// It reports what actually happened rather than what was attempted:
//   - was_active is sampled BEFORE acting, so a retry loop can tell "I fixed it"
//     from "it was already foreground, so tab_hidden has another cause".
//   - activated re-reads visibilityState afterwards instead of trusting the
//     call, because bringToFront is silent about refusal.
//   - window_focused is false when the OS or the (headless) build refused to
//     raise the window. Overpromising would make the retry loop this verb exists
//     to enable unreliable.
func (c *CDP) Activate(ctx context.Context, targetID string) (map[string]any, error) {
	wasActive := c.tabVisible(ctx, targetID)

	if err := c.run(ctx, targetID, bringToFront()); err != nil {
		return nil, err
	}

	// Page.bringToFront only picks the tab WITHIN its window; it does not raise
	// that window above other applications, and on macOS a backgrounded window
	// throttles just as a backgrounded tab does. Restoring the window to
	// "normal" is the second, load-bearing half of this verb.
	windowFocused := c.raiseWindow(ctx, targetID) == nil

	return map[string]any{
		"activated":      c.tabVisible(ctx, targetID),
		"was_active":     wasActive,
		"window_focused": windowFocused,
	}, nil
}

// History navigates the tab by delta entries (-1 back, +1 forward) and reports
// the settled URL. Unlike Reload it reports no `status`: a history move restored
// from the back/forward cache issues no request, so there is no HTTP response of
// its own to report — reporting the previous page's status would be a lie.
//
// The entry is looked up with Page.getNavigationHistory BEFORE navigating, so
// "there is nothing in that direction" is a clean typed error instead of a
// navigation that never fires a load event and dies at the caller's timeout.
func (c *CDP) History(ctx context.Context, targetID string, delta int) (map[string]any, error) {
	if delta == 0 {
		return nil, errors.New("history delta must be non-zero (-1 back, +1 forward)")
	}
	var cur int64
	var entries []*page.NavigationEntry
	if err := c.run(ctx, targetID, chromedp.NavigationEntries(&cur, &entries)); err != nil {
		return nil, err
	}
	idx := cur + int64(delta)
	if idx < 0 || idx >= int64(len(entries)) {
		return nil, fmt.Errorf("%w: delta %+d from entry %d of %d", ErrNoHistory, delta, cur+1, len(entries))
	}
	from := entries[cur].URL
	if err := c.run(ctx, targetID, page.NavigateToHistoryEntry(entries[idx].ID)); err != nil {
		return nil, err
	}
	return c.settleAt(ctx, targetID, entries[idx].URL, from)
}

// Reload reloads the tab, bypassing the cache when hard is set (the
// shift-reload the browser UI offers), and reports the settled URL.
func (c *CDP) Reload(ctx context.Context, targetID string, hard bool) (map[string]any, error) {
	return c.settle(ctx, targetID, page.Reload().WithIgnoreCache(hard))
}

// settle runs a navigating action, waits for the load it triggers, and reports
// the URL the tab actually ended at — matching Navigate, where a redirect
// updates target.url rather than echoing what was requested. status is omitted
// for a navigation with no HTTP response of its own (a data: URL, say).
func (c *CDP) settle(ctx context.Context, id string, act chromedp.Action) (map[string]any, error) {
	tctx, err := c.on(id)
	if err != nil {
		return nil, err
	}
	rc, cancel := deadline(ctx, tctx)
	defer cancel()
	resp, err := chromedp.RunResponse(rc, act)
	if err != nil {
		return nil, err
	}
	var loc string
	if err := chromedp.Run(rc, chromedp.Location(&loc)); err != nil {
		return nil, err
	}
	out := map[string]any{"url": loc}
	if resp != nil {
		out["status"] = resp.Status
	}
	return out, nil
}

// settleAt waits for a history move to come to rest and reports the URL the tab
// settled at.
//
// A history move cannot be awaited the way a fresh navigation can: restoring a
// page from the back/forward cache issues no request and fires no load event, so
// waiting for one hangs until the caller's timeout. Polling the document settles
// on both paths. It is done when the document is complete and the tab is either
// at the entry's own URL or somewhere other than the page it left — the latter
// covering a history entry that redirects.
func (c *CDP) settleAt(ctx context.Context, id, want, from string) (map[string]any, error) {
	var loc string
	act := chromedp.ActionFunc(func(actx context.Context) error {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			var got string
			err := chromedp.Evaluate(`document.readyState === "complete" ? document.location.toString() : ""`, &got).Do(actx)
			if err == nil && got != "" && (got == want || got != from) {
				loc = got
				return nil
			}
			select {
			case <-actx.Done():
				return actx.Err()
			case <-t.C:
			}
		}
	})
	if err := c.run(ctx, id, act); err != nil {
		return nil, err
	}
	return map[string]any{"url": loc}, nil
}

// awaitGone waits until the browser stops listing the given tabs.
//
// Target.closeTarget only ASKS the browser to close a tab and returns before the
// target is torn down, so reporting a close the instant the command is accepted
// would let the caller's very next `list` still show the tab it just closed. The
// wait is bounded twice over — by a short cap and by the caller's context — so a
// browser that dies with its last tab cannot wedge this.
func (c *CDP) awaitGone(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	until := time.Now().Add(3 * time.Second)
	for {
		tabs, err := c.List(ctx)
		if err != nil {
			return // the browser is gone, which is a stronger form of "closed"
		}
		live := make(map[string]bool, len(tabs))
		for _, t := range tabs {
			live[t.ID] = true
		}
		remaining := false
		for _, id := range ids {
			if live[id] {
				remaining = true
				break
			}
		}
		if !remaining || time.Now().After(until) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (c *CDP) closeTarget(ctx context.Context, id string) error {
	return c.onBrowser(ctx, func(bctx context.Context) error {
		return cdptarget.CloseTarget(cdptarget.ID(id)).Do(bctx)
	})
}

// raiseWindow restores the tab's browser window to the "normal" state, which is
// what lifts it above other applications. The error is returned rather than
// swallowed so Activate can report window_focused honestly.
func (c *CDP) raiseWindow(ctx context.Context, id string) error {
	return c.onBrowser(ctx, func(bctx context.Context) error {
		wid, _, err := cdpbrowser.GetWindowForTarget().WithTargetID(cdptarget.ID(id)).Do(bctx)
		if err != nil {
			return err
		}
		return cdpbrowser.SetWindowBounds(wid, &cdpbrowser.Bounds{WindowState: cdpbrowser.WindowStateNormal}).Do(bctx)
	})
}

// onBrowser runs fn against the BROWSER-level executor — the one Target.* and
// Browser.* methods need, since they take no page session — bounded by the
// caller's deadline. Same shape as Open's browser-level create.
func (c *CDP) onBrowser(ctx context.Context, fn func(context.Context) error) error {
	rc, cancel := deadline(ctx, c.base)
	defer cancel()
	return chromedp.Run(rc, chromedp.ActionFunc(func(actx context.Context) error {
		cc := chromedp.FromContext(actx)
		return fn(cdp.WithExecutor(actx, cc.Browser))
	}))
}

// tabVisible reports document.visibilityState == "visible" — Chrome's own view
// of whether the tab is foreground, and the signal that governs the throttling
// Activate exists to lift. An unreachable tab counts as not visible.
func (c *CDP) tabVisible(ctx context.Context, id string) bool {
	var vs string
	if err := c.run(ctx, id, chromedp.Evaluate(`document.visibilityState`, &vs)); err != nil {
		return false
	}
	return vs == "visible"
}

// forget drops the cached attach for a tab that no longer exists, so a later
// command can't reuse a dead session and the context is released.
func (c *CDP) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tabs[id]; ok {
		t.stop()
		delete(c.tabs, id)
	}
}
