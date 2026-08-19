package chrome

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// dialogState is the last Page.javascriptDialogOpening this connection
// received for a tab and has not yet seen closed. One per tab, replaced by the
// next opening and deleted on javascriptDialogClosed (RFC-0018).
type dialogState struct {
	Type          string
	Message       string
	DefaultPrompt string
	FrameURL      string
	OpenedAt      time.Time
}

// ErrNoDialog reports an accept/dismiss with nothing retained as open.
var ErrNoDialog = errors.New("no native dialog is open on this tab")

// IsNoDialog reports whether err is ErrNoDialog, by type or by message — the
// daemon returns errors as strings (see errIs in pointer.go).
func IsNoDialog(err error) bool { return errIs(err, ErrNoDialog) }

// dialogUnwatchedNote is the honest answer when nothing was listening before
// this command started: Chrome does not replay a dialog that is already open,
// and a session that was not attached when it opened can neither see nor close
// it.
const dialogUnwatchedNote = "nothing was listening to this tab before this command started, so a dialog that opened earlier " +
	"is neither visible to nor closable by this command; close it in the Chrome window, or close the tab. " +
	"Use the daemon (drop --no-daemon) so dialogs are retained from the moment it attaches."

// chromeNoDialogShowing is the text Chrome answers Page.handleJavaScriptDialog
// with when the dialog it names has already closed — a race between the
// retained event and the real world, benign and self-healing (see
// DialogHandle).
const chromeNoDialogShowing = "No dialog is showing"

// dialogEvent applies one CDP event to the retained per-tab state: it stores
// the opening on javascriptDialogOpening (replacing whatever was there — there
// is at most one open dialog per tab) and deletes it on javascriptDialogClosed.
// It takes the event as `any` so a pure test can feed it synthetic values with
// no Chrome, and is safe to call from chromedp's event loop: it only writes a
// map under dialogMu, never issues a CDP command.
func (c *CDP) dialogEvent(id string, ev any) {
	switch e := ev.(type) {
	case *page.EventJavascriptDialogOpening:
		c.dialogMu.Lock()
		c.dialogs[id] = dialogState{
			Type:          string(e.Type),
			Message:       e.Message,
			DefaultPrompt: e.DefaultPrompt,
			FrameURL:      e.URL,
			OpenedAt:      time.Now().UTC().Truncate(time.Millisecond),
		}
		c.dialogMu.Unlock()
	case *page.EventJavascriptDialogClosed:
		c.clearDialog(id)
	}
}

// dialog reads the retained dialog for a tab, if any.
func (c *CDP) dialog(id string) (dialogState, bool) {
	c.dialogMu.Lock()
	defer c.dialogMu.Unlock()
	st, ok := c.dialogs[id]
	return st, ok
}

// clearDialog discards the retained dialog for a tab, if any.
func (c *CDP) clearDialog(id string) {
	c.dialogMu.Lock()
	delete(c.dialogs, id)
	c.dialogMu.Unlock()
}

// listenDialog retains the dialog that is open on a tab. It is registered from
// listenCapture, BEFORE the attach and on the long-lived tab context, for the
// same reason listenConsole is: the process holding the connection has to
// already be listening when the dialog opens, because Chrome tells nobody
// later. It runs on chromedp's event loop and only writes the map; it never
// issues a CDP command.
func (c *CDP) listenDialog(tctx context.Context, id string) {
	chromedp.ListenTarget(tctx, func(ev any) { c.dialogEvent(id, ev) })
}

// dialogStatusResult renders the retained dialog state as the envelope's
// result payload — the pure core of DialogStatus, so its shape is pinned by a
// test that needs no Chrome.
func dialogStatusResult(st dialogState, ok, fresh bool) map[string]any {
	res := map[string]any{"open": ok}
	if ok {
		res["type"] = st.Type
		res["message"] = st.Message
		res["default_prompt"] = st.DefaultPrompt
		res["frame_url"] = st.FrameURL
		// Left as a time.Time, not pre-formatted: encoding/json's default
		// time.Time marshaling IS RFC 3339 UTC (RFC3339Nano, and OpenedAt is
		// already truncated to millisecond precision) — exactly the encoding
		// console.go's `ts` field gets from the same default, so there is one
		// authored copy of "how a retained-event timestamp looks in the
		// envelope" rather than a second one that could drift from it.
		res["opened_at"] = st.OpenedAt
	}
	if fresh {
		res["note"] = dialogUnwatchedNote
	}
	return res
}

// DialogStatus reports the native dialog retained for a tab (RFC-0018). On an
// already-attached tab it issues NO CDP traffic at all: it attaches (a no-op
// if already attached) and reads the map this connection has been keeping
// since it started watching the tab.
func (c *CDP) DialogStatus(ctx context.Context, id string) (map[string]any, error) {
	// Asked BEFORE attaching, exactly as Console does: afterwards every tab
	// looks like it was always watched.
	fresh := !c.attached(id)
	if err := c.run(ctx, id); err != nil {
		return nil, err
	}
	st, ok := c.dialog(id)
	return dialogStatusResult(st, ok, fresh), nil
}

// DialogHandle closes the dialog retained for a tab — accept or dismiss — with
// Page.handleJavaScriptDialog, the one command that works while the renderer
// is blocked. It never issues that command blind: with nothing retained, a
// session that did not see the dialog open would hang on it rather than fail,
// so ErrNoDialog is returned instead (RFC-0018).
func (c *CDP) DialogHandle(ctx context.Context, id string, accept bool, text string) (map[string]any, error) {
	fresh := !c.attached(id)
	if err := c.run(ctx, id); err != nil {
		return nil, err
	}
	st, ok := c.dialog(id)
	if !ok {
		if fresh {
			return nil, fmt.Errorf("%w; %s", ErrNoDialog, dialogUnwatchedNote)
		}
		return nil, ErrNoDialog
	}

	isPrompt := st.Type == string(page.DialogTypePrompt)
	p := page.HandleJavaScriptDialog(accept)
	if text != "" && isPrompt {
		// The text is not sent for the other types: Chrome echoes whatever
		// promptText it was given in the closed event's userInput even for a
		// confirm, and a retained text_ignored:true is a clearer record than a
		// misleading echo.
		p = p.WithPromptText(text)
	}
	if err := c.run(ctx, id, p); err != nil {
		if strings.Contains(err.Error(), chromeNoDialogShowing) {
			// The dialog closed between the retained event and now (the
			// closed event is in flight, or was missed): the race is benign
			// and self-healing.
			c.clearDialog(id)
			return nil, ErrNoDialog
		}
		return nil, err
	}
	// Eager clear: the closed event will also do this, but a caller reading
	// DialogStatus right after must see open:false without waiting on the
	// event to arrive.
	c.clearDialog(id)

	action := "dismiss"
	if accept {
		action = "accept"
	}
	res := map[string]any{
		"handled": true, "action": action,
		"type": st.Type, "message": st.Message,
	}
	switch {
	case isPrompt && accept:
		// "" when none was given — accept with no text answers a prompt with
		// the empty string, because Page.handleJavaScriptDialog with no
		// promptText does; a caller who wants the default passes
		// default_prompt back explicitly.
		res["text"] = text
	case text != "" && !isPrompt:
		res["text_ignored"] = true
	}
	return res, nil
}
