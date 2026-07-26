package cli

// Tab-lifecycle verbs: `close`, `activate`, and nav's history/reload flags.
// See docs/rfc/0007-tab-lifecycle.md.

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// cmdActivate foregrounds a tab. It is the command `cli-reference.md` has always
// told users to run when a name/ref/cell resolution fails with `tab_hidden`, so
// an agent can recover from that on its own instead of asking a human to switch
// tabs by hand.
func (a *App) cmdActivate() *cobra.Command {
	return &cobra.Command{
		Use:   "activate [target]",
		Short: "Bring a tab (and its window) to the foreground — the fix for tab_hidden",
		Long: "Foreground a tab within its window AND raise that window above other\n" +
			"applications. Chrome throttles the accessibility tree on a tab it cannot\n" +
			"foreground, which is what makes --by name/ref/cell stall with\n" +
			"`tab_hidden: true`; running activate and retrying clears it.\n\n" +
			"The result reports what actually happened: was_active tells a retry loop\n" +
			"whether the tab was already foreground (so tab_hidden has another cause),\n" +
			"and window_focused is false when the OS refused to raise the window.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, cancel := a.ctx()
			defer cancel()
			tgt, b, rerr := a.resolvePositional(ctx, args)
			if rerr != nil {
				a.emitErr("activate", rerr.Code, rerr.Message, rerr.Details)
				return nil
			}
			res, err := b.Activate(ctx, tgt.ID)
			if err != nil {
				a.emitErr("activate", classifyActionErr(err), err.Error(), nil)
				return nil
			}
			a.emitOK("activate", tgt, res)
			return nil
		},
	}
}

// cmdClose closes tabs, so an automation that opens them can clean up after
// itself rather than leaving debris in the browser the user actually works in.
func (a *App) cmdClose() *cobra.Command {
	var urlSub, titleSub string
	var all bool
	c := &cobra.Command{
		Use:   "close [target]",
		Short: "Close a tab, or every tab matching --url/--title (with --all)",
		Long: "Close the tab named positionally, the sticky current tab when none is\n" +
			"given, or every tab matching the --url/--title substring filters.\n\n" +
			"A filter that matches more than one tab is an error unless --all is\n" +
			"passed, and nothing is closed in that case: a destructive verb must not\n" +
			"guess which fuzzy match you meant. Closing the current tab clears it and\n" +
			"reports sticky_cleared, so later commands fail with no_current_target\n" +
			"rather than against a dead tab id.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			// Validated before connecting: a positional target and a filter are two
			// different ways to name the tab, and giving both is a mistake rather
			// than a refinement.
			if len(args) == 1 && (urlSub != "" || titleSub != "") {
				a.emitErr("close", result.CodeUsage, "close takes either a target argument or --url/--title filters, not both", nil)
				return nil
			}
			ctx, cancel := a.ctx()
			defer cancel()
			victims, b, rerr := a.closeVictims(ctx, args, urlSub, titleSub, all)
			if rerr != nil {
				a.emitErr("close", rerr.Code, rerr.Message, rerr.Details)
				return nil
			}
			ids := make([]string, 0, len(victims))
			for _, v := range victims {
				ids = append(ids, v.ID)
			}
			res, err := b.CloseTabs(ctx, ids)
			if err != nil {
				a.emitErr("close", classifyActionErr(err), err.Error(), nil)
				return nil
			}
			// Only the tabs the driver actually closed count. CloseTabs succeeds on
			// a partial failure and lists the refused tabs under `failed`; treating
			// a refused tab as closed would wipe the sticky pointer at a tab that
			// is still open and still listed, stranding every later command on
			// no_current_target — the inversion of what US-7 exists to prevent.
			res["sticky_cleared"] = a.clearStickyIfClosed(closedIDs(res))
			// The envelope's `target` describes exactly one tab, and a bulk close has
			// none — `closed` carries the list instead.
			var tgt *result.TargetInfo
			if len(victims) == 1 {
				t := victims[0]
				tgt = &t
			}
			a.emitOK("close", tgt, res)
			return nil
		},
	}
	c.Flags().StringVar(&urlSub, "url", "", "close tabs whose URL contains this substring")
	c.Flags().StringVar(&titleSub, "title", "", "close tabs whose title contains this substring")
	c.Flags().BoolVar(&all, "all", false, "close every tab matching the filter (required when more than one matches)")
	return c
}

// closeVictims decides which tabs `close` acts on: the positional (or sticky)
// target when no filter is given, else every tab matching the --url/--title
// substrings, using the same case-insensitive matching as `list`.
//
// It returns an error — closing nothing — when a filter matches several tabs
// without --all, or matches none.
func (a *App) closeVictims(ctx context.Context, args []string, urlSub, titleSub string, all bool) ([]result.TargetInfo, chrome.Browser, *result.Err) {
	if urlSub == "" && titleSub == "" {
		tgt, b, rerr := a.resolvePositional(ctx, args)
		if rerr != nil {
			return nil, nil, rerr
		}
		return []result.TargetInfo{*tgt}, b, nil
	}
	b, berr := a.getBrowser(ctx)
	if berr != nil {
		return nil, nil, berr
	}
	tabs, err := b.List(ctx)
	if err != nil {
		return nil, nil, &result.Err{Code: result.CodeConnection, Message: err.Error()}
	}
	var hits []result.TargetInfo
	for _, t := range tabs {
		if !containsFold(t.URL, urlSub) || !containsFold(t.Title, titleSub) {
			continue
		}
		hits = append(hits, result.TargetInfo{ID: t.ID, Title: t.Title, URL: t.URL})
	}
	switch {
	case len(hits) == 0:
		return nil, nil, &result.Err{
			Code:    result.CodeTargetNotFound,
			Message: fmt.Sprintf("no tab matches %s", filterDesc(urlSub, titleSub)),
		}
	case len(hits) > 1 && !all:
		matches := make([]any, 0, len(hits))
		for _, h := range hits {
			matches = append(matches, map[string]any{"id": h.ID, "title": h.Title, "url": h.URL})
		}
		return nil, nil, &result.Err{
			Code: result.CodeAmbiguous,
			Message: fmt.Sprintf("%d tabs match %s and nothing was closed — pass --all to close them all, or narrow the filter",
				len(hits), filterDesc(urlSub, titleSub)),
			Details: map[string]any{"matches": matches},
		}
	}
	return hits, b, nil
}

// resolvePositional resolves an optional positional <target>, falling back to
// --target and then the sticky current tab like every other verb. The flag is
// restored afterwards because `session` reuses the App across lines.
func (a *App) resolvePositional(ctx context.Context, args []string) (*result.TargetInfo, chrome.Browser, *result.Err) {
	saved := a.targetFlag
	if len(args) == 1 && args[0] != "" {
		a.targetFlag = args[0]
	}
	tgt, b, rerr := a.resolveTarget(ctx)
	a.targetFlag = saved
	return tgt, b, rerr
}

// clearStickyIfClosed clears the persisted current target when it was among the
// tabs just closed, and reports whether it did.
//
// Leaving it set would point every later command at a dead tab id and surface a
// confusing CDP error instead of the no_current_target the state machine already
// models. The setter is absent in tests and wherever no state store is wired; in
// that case there is nothing to clear and this honestly reports false.
func (a *App) clearStickyIfClosed(ids []string) bool {
	cur := a.sticky()
	if cur == "" || a.stickySet == nil {
		return false
	}
	for _, id := range ids {
		if id != cur {
			continue
		}
		if err := a.stickySet(a.connOpts(), ""); err != nil {
			fmt.Fprintf(a.err, "warning: closed the current tab but could not clear it: %v\n", err)
			return false
		}
		return true
	}
	return false
}

// closedIDs reads back the ids CloseTabs reports as actually closed. It is
// deliberately tolerant of a payload that has been through the daemon's JSON
// round-trip (where every entry is a map[string]any) and of a driver that closed
// nothing at all.
func closedIDs(res map[string]any) []string {
	entries, _ := res["closed"].([]any)
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		m, _ := e.(map[string]any)
		if id, _ := m["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// containsFold reports whether s contains sub, case-insensitively. An empty sub
// is "no constraint", matching how `list` treats an unset filter.
func containsFold(s, sub string) bool {
	return sub == "" || strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func filterDesc(urlSub, titleSub string) string {
	var parts []string
	if urlSub != "" {
		parts = append(parts, fmt.Sprintf("url ~ %q", urlSub))
	}
	if titleSub != "" {
		parts = append(parts, fmt.Sprintf("title ~ %q", titleSub))
	}
	return strings.Join(parts, " and ")
}

// navFlags carries `nav`'s non-URL modes. History navigation is not the same as
// re-navigating to a URL — that loses form state, discards SPA history, and is
// simply wrong for a POST result — and reload is not the same either, since it
// keeps scroll and history position.
type navFlags struct {
	back    bool
	forward bool
	reload  bool
	hard    bool
}

// register adds the flags to the nav command.
func (n *navFlags) register(c *cobra.Command) {
	c.Flags().BoolVar(&n.back, "back", false, "go back one history entry instead of navigating")
	c.Flags().BoolVar(&n.forward, "forward", false, "go forward one history entry instead of navigating")
	c.Flags().BoolVar(&n.reload, "reload", false, "reload the current page instead of navigating")
	c.Flags().BoolVar(&n.hard, "hard", false, "with --reload: bypass the cache (refetch)")
}

// validate checks the flag combination against argc positional arguments and
// returns a usage message, or "" when the invocation is legal.
//
// It is a pure function of the parsed flags so `nav` can reject a malformed call
// with exit 2 before the browser is contacted at all.
func (n *navFlags) validate(argc int) string {
	var modes []string
	if argc > 0 {
		modes = append(modes, "<url>")
	}
	if n.back {
		modes = append(modes, "--back")
	}
	if n.forward {
		modes = append(modes, "--forward")
	}
	if n.reload {
		modes = append(modes, "--reload")
	}
	switch {
	case len(modes) == 0:
		return "nav needs a <url>, or one of --back, --forward, --reload"
	case len(modes) > 1:
		return fmt.Sprintf("nav takes exactly one of <url>, --back, --forward, --reload — got %s", strings.Join(modes, " and "))
	case n.hard && !n.reload:
		return "--hard applies only to --reload (a fresh navigation never serves from cache)"
	}
	return ""
}

// act runs the browser operation the validated flags select.
func (n *navFlags) act(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
	switch {
	case n.back:
		return b.History(ctx, id, -1)
	case n.forward:
		return b.History(ctx, id, +1)
	case n.reload:
		return b.Reload(ctx, id, n.hard)
	default:
		return b.Navigate(ctx, id, args[0])
	}
}
