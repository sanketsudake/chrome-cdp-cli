package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// Version is set at build time via -ldflags.
var Version = "dev"

type tabRow struct {
	Idx   int    `json:"idx"`
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

func (a *App) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "chrome-cdp",
		Short:         "Drive your local Chrome from the command line (CDP over chromedp)",
		Version:       Version, // enables `--version`; the `version` subcommand prints it bare
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(*cobra.Command, []string) {
			if a.start.IsZero() {
				a.start = time.Now()
			}
		},
	}
	// d holds the effective flag defaults (built-in, overlaid by the config
	// file and CHROME_CDP_* env); an explicit flag still overrides them.
	d := a.defaults
	pf := root.PersistentFlags()
	pf.BoolVar(&a.jsonOut, "json", d.JSON, "machine-readable output (one JSON value to stdout)")
	pf.StringVar(&a.targetFlag, "target", "", "tab to act on (idprefix | url:<s> | title:<s> | @N)")
	pf.DurationVar(&a.timeout, "timeout", d.Timeout, "max time to wait for the command")
	pf.BoolVar(&a.noLaunch, "no-launch", d.NoLaunch, "don't auto-launch a fallback Chrome")
	pf.BoolVar(&a.noDaemon, "no-daemon", d.NoDaemon, "connect directly instead of via the shared daemon")
	pf.StringVar(&a.profileDir, "profile-dir", d.ProfileDir, "managed-launch Chrome profile dir (else $CHROME_CDP_PROFILE or ~/.cache/chrome-cdp/profile)")
	pf.IntVar(&a.port, "port", d.Port, "explicit Chrome debug port to attach to / launch with (0 = auto)")
	pf.StringVar(&a.byFlag, "by", d.By, "selector syntax: css|id|search|jspath|css-all|name|ref|cell (name = ARIA accessible name; ref = snap e<id>; cell = grid input by [row|]column header)")
	pf.StringVar(&a.waitFlag, "wait", d.Wait, "selector wait condition: visible|ready|enabled")
	pf.StringVar(&a.roleFlag, "role", "", "with --by name: constrain to an ARIA role (button|link|textbox|…)")
	pf.IntVar(&a.nthFlag, "nth", 0, "with --by name: pick the Nth (1-based) match among visible candidates")
	pf.StringVar(&a.matchFlag, "match", "", "with --by name: name match mode: exact (default)|contains|regex")
	pf.BoolVar(&a.noWait, "no-wait", false, "act immediately; fail fast instead of waiting for the element")
	pf.BoolVar(&a.pierce, "pierce", false, "reach into shadow DOM / iframes (via DevTools search)")
	pf.BoolVarP(&a.quiet, "quiet", "q", false, "suppress non-essential output")
	pf.BoolVarP(&a.verbose, "verbose", "v", false, "verbose diagnostics on stderr")
	pf.BoolVar(&a.noColor, "no-color", d.NoColor, "plain output (also honors $NO_COLOR)")
	pf.BoolVar(&a.noInput, "no-input", false, "never prompt (the CLI is non-interactive already)")

	root.AddCommand(
		a.cmdList(), a.cmdOpen(), a.cmdUse(), a.cmdNav(), a.cmdEval(), a.cmdSnap(),
		a.cmdHTML(), a.cmdText(), a.cmdValue(),
		a.cmdClick(), a.cmdType(), a.cmdFill(), a.cmdSelect(), a.cmdGrid(), a.cmdScroll(), a.cmdAttr(), a.cmdScreenshot(), a.cmdPDF(),
		a.cmdCookie(), a.cmdHeaders(), a.cmdEmulate(), a.cmdFrame(), a.cmdWait(), a.cmdRaw(),
		a.cmdSession(), a.cmdDoctor(), a.cmdDaemon(), a.cmdExitCodes(), a.cmdVersion(),
	)
	return root
}

// classifyWithTabHint classifies an action error and, when a name/ref/cell
// resolution times out, probes whether the tab is backgrounded — Chrome throttles
// the accessibility tree on a tab it can't foreground, so those resolutions stall.
// It surfaces that as a `tab_hidden` detail + an actionable message instead of a
// bare timeout, so a caller knows to foreground Chrome (or use --by css).
func (a *App) classifyWithTabHint(b chrome.Browser, id string, err error) (string, string, map[string]any) {
	code := classifyActionErr(err)
	if code != result.CodeTargetTimeout || !a.ariaAddressing() {
		return code, err.Error(), nil
	}
	vctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	v, verr := b.Eval(vctx, id, "document.visibilityState")
	if verr != nil {
		return code, err.Error(), nil
	}
	if vv, _ := v.(map[string]any)["value"].(string); vv == "hidden" {
		return code,
			"tab is backgrounded (visibilityState=hidden); Chrome throttles the accessibility tree there, so --by name/ref/cell stalls — bring Chrome/this tab to the foreground, or use --by css",
			map[string]any{"tab_hidden": true}
	}
	return code, err.Error(), nil
}

// ariaAddressing reports whether the current --by mode resolves via the
// accessibility tree (throttled on a backgrounded tab).
func (a *App) ariaAddressing() bool {
	switch a.byFlag {
	case "name", "ref", "cell":
		return true
	}
	return false
}

func classifyActionErr(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "deadline exceeded"), strings.Contains(s, "timeout"),
		strings.Contains(s, "could not find node"), strings.Contains(s, "no node"):
		return result.CodeTargetTimeout
	default:
		return result.CodeCDP
	}
}

func (a *App) cmdList() *cobra.Command {
	var urlSub, titleSub string
	c := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List open tabs (--url/--title filter by substring)",
		RunE: func(*cobra.Command, []string) error {
			ctx, cancel := a.ctx()
			defer cancel()
			b, berr := a.getBrowser(ctx)
			if berr != nil {
				a.emitErr("list", berr.Code, berr.Message, nil)
				return nil
			}
			tabs, err := b.List(ctx)
			if err != nil {
				a.emitErr("list", "connection_failed", err.Error(), nil)
				return nil
			}
			// The 1-based idx reflects position in the FULL tab list, so it stays a
			// stable @N target even when a filter is applied.
			rows := []tabRow{}
			for i, t := range tabs {
				if urlSub != "" && !strings.Contains(strings.ToLower(t.URL), strings.ToLower(urlSub)) {
					continue
				}
				if titleSub != "" && !strings.Contains(strings.ToLower(t.Title), strings.ToLower(titleSub)) {
					continue
				}
				rows = append(rows, tabRow{Idx: i + 1, ID: t.ID, Title: t.Title, URL: t.URL})
			}
			a.emitOK("list", nil, map[string]any{"tabs": rows})
			return nil
		},
	}
	c.Flags().StringVar(&urlSub, "url", "", "only tabs whose URL contains this substring")
	c.Flags().StringVar(&titleSub, "title", "", "only tabs whose title contains this substring")
	return c
}

func (a *App) cmdUse() *cobra.Command {
	return &cobra.Command{
		Use: "use <target>", Short: "Set the sticky current tab", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, cancel := a.ctx()
			defer cancel()
			saved := a.targetFlag
			a.targetFlag = args[0]
			tgt, _, rerr := a.resolveTarget(ctx)
			a.targetFlag = saved
			if rerr != nil {
				a.emitErr("use", rerr.Code, rerr.Message, nil)
				return nil
			}
			if a.stickySet == nil {
				a.emitErr("use", "generic", "cannot set the current tab: sticky-target state is unavailable", nil)
				return nil
			}
			// Persist the RESOLVED id, not the spec: an ephemeral form like @N
			// must not be re-resolved against a later, reordered tab list.
			if err := a.stickySet(a.connOpts(), tgt.ID); err != nil {
				a.emitErr("use", "generic", "cannot persist the current tab: "+err.Error(), nil)
				return nil
			}
			a.emitOK("use", tgt, map[string]any{"current": tgt.ID})
			return nil
		},
	}
}

func (a *App) cmdOpen() *cobra.Command {
	return &cobra.Command{
		Use: "open <url>", Short: "Open a new tab at a URL and make it the current tab", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, cancel := a.ctx()
			defer cancel()
			b, berr := a.getBrowser(ctx)
			if berr != nil {
				a.emitErr("open", berr.Code, berr.Message, nil)
				return nil
			}
			res, err := b.Open(ctx, args[0])
			if err != nil {
				a.emitErr("open", classifyActionErr(err), err.Error(), nil)
				return nil
			}
			// Make the new tab the sticky current target, like `use`, so the next
			// command acts on it without a --target.
			if id, _ := res["id"].(string); id != "" && a.stickySet != nil {
				_ = a.stickySet(a.connOpts(), id)
			}
			a.emitOK("open", nil, res)
			return nil
		},
	}
}

func (a *App) cmdNav() *cobra.Command {
	return &cobra.Command{
		Use: "nav <url>", Short: "Navigate the target tab and wait for load", Args: cobra.ExactArgs(1),
		RunE: a.targetAction("nav", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
			return b.Navigate(ctx, id, args[0])
		}),
	}
}

func (a *App) cmdEval() *cobra.Command {
	return &cobra.Command{
		Use: "eval <js>", Short: "Evaluate JS in the target tab", Args: cobra.ExactArgs(1),
		RunE: a.targetAction("eval", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
			return b.Eval(ctx, id, args[0])
		}),
	}
}

func (a *App) cmdSnap() *cobra.Command {
	var role, grep, region string
	var dedupe bool
	c := &cobra.Command{
		Use: "snap", Short: "Accessibility-tree snapshot (filter with --role/--grep/--region/--dedupe)",
		RunE: a.targetAction("snap", func(ctx context.Context, b chrome.Browser, id string, _ []string) (any, error) {
			return b.Snapshot(ctx, id, chrome.SnapOpts{Role: role, Grep: grep, Region: region, Dedupe: dedupe})
		}),
	}
	c.Flags().StringVar(&role, "role", "", "only nodes with this ARIA role (button|link|textbox|…)")
	c.Flags().StringVar(&grep, "grep", "", "only nodes whose accessible name matches this regex")
	c.Flags().StringVar(&region, "region", "", "only nodes within a container whose name contains this")
	c.Flags().BoolVar(&dedupe, "dedupe", false, "collapse identical role+name (for virtualized grids)")
	return c
}

func (a *App) cmdHTML() *cobra.Command {
	var inner bool
	c := &cobra.Command{
		Use: "html [selector]", Short: "Outer (or --inner) HTML of the page or a selector", Args: cobra.MaximumNArgs(1),
		RunE: a.targetAction("html", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
			sel := ""
			if len(args) == 1 {
				sel = args[0]
			}
			return b.HTML(ctx, id, sel, inner, a.queryOpts())
		}),
	}
	c.Flags().BoolVar(&inner, "inner", false, "inner HTML instead of outer")
	return c
}

func (a *App) cmdText() *cobra.Command {
	return &cobra.Command{
		Use: "text <selector>", Short: "Visible text of a selector", Args: cobra.ExactArgs(1),
		RunE: a.targetAction("text", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
			return b.Text(ctx, id, args[0], a.queryOpts())
		}),
	}
}

func (a *App) cmdValue() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use: "value <selector>", Short: "Value of a form field (--all: values of every match, as a list)", Args: cobra.ExactArgs(1),
		RunE: a.targetAction("value", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
			if all {
				return b.Values(ctx, id, args[0], a.queryOpts())
			}
			return b.Value(ctx, id, args[0], a.queryOpts())
		}),
	}
	c.Flags().BoolVar(&all, "all", false, "return the value/text of every element matching the CSS selector, as a list")
	return c
}

func (a *App) cmdCookie() *cobra.Command {
	cookie := &cobra.Command{Use: "cookie", Short: "Read and write cookies"}

	cookieSet := func() *cobra.Command {
		var domain, path string
		c := &cobra.Command{
			Use: "set <name> <value>", Short: "Set a cookie", Args: cobra.ExactArgs(2),
			RunE: a.targetAction("cookie", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
				return b.CookieSet(ctx, id, args[0], args[1], domain, path)
			}),
		}
		c.Flags().StringVar(&domain, "domain", "", "cookie domain")
		c.Flags().StringVar(&path, "path", "", "cookie path")
		return c
	}

	cookie.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List cookies for the target tab",
			RunE: a.targetAction("cookie", func(ctx context.Context, b chrome.Browser, id string, _ []string) (any, error) {
				return b.CookieList(ctx, id)
			}),
		},
		cookieSet(),
		&cobra.Command{
			Use: "rm <name>", Short: "Delete a cookie", Args: cobra.ExactArgs(1),
			RunE: a.targetAction("cookie", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
				return b.CookieDelete(ctx, id, args[0])
			}),
		},
		&cobra.Command{
			Use: "clear", Short: "Delete all cookies for the tab",
			RunE: a.targetAction("cookie", func(ctx context.Context, b chrome.Browser, id string, _ []string) (any, error) {
				return b.CookieClear(ctx, id)
			}),
		},
	)
	return cookie
}

func (a *App) cmdAttr() *cobra.Command {
	attr := &cobra.Command{Use: "attr", Short: "Read/write element attributes"}
	attr.AddCommand(
		&cobra.Command{
			Use: "get <selector> <name>", Short: "Get an attribute", Args: cobra.ExactArgs(2),
			RunE: a.targetAction("attr", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
				return b.AttrGet(ctx, id, args[0], args[1], a.queryOpts())
			}),
		},
		&cobra.Command{
			Use: "list <selector>", Short: "List all attributes of an element", Args: cobra.ExactArgs(1),
			RunE: a.targetAction("attr", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
				return b.AttrList(ctx, id, args[0], a.queryOpts())
			}),
		},
		&cobra.Command{
			Use: "set <selector> <name> <value>", Short: "Set an attribute", Args: cobra.ExactArgs(3),
			RunE: a.targetAction("attr", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
				return b.AttrSet(ctx, id, args[0], args[1], args[2], a.queryOpts())
			}),
		},
		&cobra.Command{
			Use: "rm <selector> <name>", Short: "Remove an attribute", Args: cobra.ExactArgs(2),
			RunE: a.targetAction("attr", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
				return b.AttrRemove(ctx, id, args[0], args[1], a.queryOpts())
			}),
		},
	)
	return attr
}

func (a *App) cmdHeaders() *cobra.Command {
	headers := &cobra.Command{Use: "headers", Short: "Extra HTTP request headers"}
	headers.AddCommand(&cobra.Command{
		Use: "set <k=v>...", Short: "Set extra request headers", Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			hs := map[string]string{}
			for _, kv := range args {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					a.emitErr("headers", result.CodeUsage, "header must be k=v: "+kv, nil)
					return nil
				}
				hs[k] = v
			}
			a.runResolved("headers", func(ctx context.Context, b chrome.Browser, id string) (any, error) {
				return b.SetHeaders(ctx, id, hs)
			})
			return nil
		},
	})
	return headers
}

func (a *App) cmdEmulate() *cobra.Command {
	emu := &cobra.Command{Use: "emulate", Short: "Emulate viewport / geolocation"}
	emu.AddCommand(
		&cobra.Command{
			Use: "viewport <width> <height>", Short: "Emulate a viewport size", Args: cobra.ExactArgs(2),
			RunE: func(_ *cobra.Command, args []string) error {
				w, e1 := strconv.ParseInt(args[0], 10, 64)
				h, e2 := strconv.ParseInt(args[1], 10, 64)
				if e1 != nil || e2 != nil {
					a.emitErr("emulate", result.CodeUsage, "viewport needs integer <width> <height>", nil)
					return nil
				}
				a.runResolved("emulate", func(ctx context.Context, b chrome.Browser, id string) (any, error) {
					return b.EmulateViewport(ctx, id, w, h)
				})
				return nil
			},
		},
		&cobra.Command{
			Use: "geo <lat> <lon>", Short: "Override geolocation", Args: cobra.ExactArgs(2),
			RunE: func(_ *cobra.Command, args []string) error {
				lat, e1 := strconv.ParseFloat(args[0], 64)
				lon, e2 := strconv.ParseFloat(args[1], 64)
				if e1 != nil || e2 != nil {
					a.emitErr("emulate", result.CodeUsage, "geo needs numeric <lat> <lon>", nil)
					return nil
				}
				a.runResolved("emulate", func(ctx context.Context, b chrome.Browser, id string) (any, error) {
					return b.EmulateGeo(ctx, id, lat, lon)
				})
				return nil
			},
		},
		&cobra.Command{
			Use: "reset", Short: "Clear viewport/geolocation overrides",
			RunE: a.targetAction("emulate", func(ctx context.Context, b chrome.Browser, id string, _ []string) (any, error) {
				return b.EmulateReset(ctx, id)
			}),
		},
	)
	return emu
}

func (a *App) cmdWait() *cobra.Command {
	var url, visible, gone, text string
	var stable, idle bool
	var forDur time.Duration
	c := &cobra.Command{
		Use: "wait", Short: "Wait for a condition: --url/--visible/--gone/--text/--stable/--idle, or a fixed --for",
		RunE: func(*cobra.Command, []string) error {
			// --for is a fixed sleep — no tab needed (e.g. settle after a redirect
			// when no condition is cleaner).
			if forDur > 0 {
				time.Sleep(forDur)
				a.emitOK("wait", nil, map[string]any{"waited": "for:" + forDur.String()})
				return nil
			}
			if url == "" && visible == "" && gone == "" && text == "" && !stable && !idle {
				a.emitErr("wait", result.CodeUsage, "wait needs one of --url, --visible, --gone, --text, --stable, --idle, --for", nil)
				return nil
			}
			cond := chrome.WaitCond{URL: url, Visible: visible, Gone: gone, Text: text, Stable: stable, Idle: idle, Query: a.queryOpts()}
			a.runResolved("wait", func(ctx context.Context, b chrome.Browser, id string) (any, error) {
				return b.Wait(ctx, id, cond)
			})
			return nil
		},
	}
	c.Flags().StringVar(&url, "url", "", "wait until the target tab's URL contains this substring")
	c.Flags().StringVar(&visible, "visible", "", "wait until this selector is visible")
	c.Flags().StringVar(&gone, "gone", "", "wait until this selector is gone")
	c.Flags().StringVar(&text, "text", "", "wait until the page (accessibility tree) contains this text, e.g. a 'Success' toast")
	c.Flags().BoolVar(&stable, "stable", false, "wait until the accessibility tree stops changing (the page settled)")
	c.Flags().BoolVar(&idle, "idle", false, "wait until network activity settles (no in-flight requests) — for SPA loads")
	c.Flags().DurationVar(&forDur, "for", 0, "wait a fixed duration (e.g. 3s) — a fallback; prefer a condition")
	return c
}

func (a *App) cmdFrame() *cobra.Command {
	frame := &cobra.Command{Use: "frame", Short: "Inspect frames"}
	frame.AddCommand(&cobra.Command{
		Use: "list", Short: "List the frame tree of the target tab",
		RunE: a.targetAction("frame", func(ctx context.Context, b chrome.Browser, id string, _ []string) (any, error) {
			return b.Frames(ctx, id)
		}),
	})
	return frame
}

// runResolved resolves the target and runs a pre-bound action (one whose args
// were already parsed by the caller), then emits. It shares targetAction's
// resolve→run→classify→emit core, ignoring the unused cobra args.
func (a *App) runResolved(command string, fn func(ctx context.Context, b chrome.Browser, id string) (any, error)) {
	_ = a.targetAction(command, func(ctx context.Context, b chrome.Browser, id string, _ []string) (any, error) {
		return fn(ctx, b, id)
	})(nil, nil)
}

func (a *App) cmdClick() *cobra.Command {
	return a.withWaitText(&cobra.Command{
		Use: "click <selector>", Short: "Click an element (auto-waits)", Args: cobra.ExactArgs(1),
		RunE: a.targetAction("click", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
			return b.Click(ctx, id, args[0], a.queryOpts())
		}),
	})
}

func (a *App) cmdType() *cobra.Command {
	return a.withWaitText(&cobra.Command{
		Use: "type <selector> <text>", Short: "Type text via real keystrokes (appends)", Args: cobra.ExactArgs(2),
		RunE: a.targetAction("type", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
			return b.Type(ctx, id, args[0], args[1], a.queryOpts())
		}),
	})
}

func (a *App) cmdFill() *cobra.Command {
	return a.withWaitText(&cobra.Command{
		Use: "fill <selector> <value>", Short: "Set a field to a value, replacing existing content (clears then types)", Args: cobra.ExactArgs(2),
		RunE: a.targetAction("fill", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
			return b.Fill(ctx, id, args[0], args[1], a.queryOpts())
		}),
	})
}

func (a *App) cmdSelect() *cobra.Command {
	var filter, optMatch, sep string
	c := &cobra.Command{
		Use:   "select <field> <option>",
		Short: "Choose an option in a prompt/combobox/cascade widget (drives Workday portal prompts)",
		Long: "Choose an option in a prompt, combobox, cascade, or native <select>.\n\n" +
			"The field is addressed by ARIA accessible name (default --by name; add --role\n" +
			"textbox to disambiguate an input from a same-named column header). The option\n" +
			"is matched by substring by default (--option-match), and a cascade path is\n" +
			"given with '>' between levels, e.g.:\n\n" +
			"  chrome-cdp select \"Time Type\" \"Project Plan Tasks > ShiftLeft: Qwiet\" --role textbox",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := a.queryOpts()
			// The field is addressed by accessible name unless the user chose another
			// --by explicitly (select's whole point is name/role addressing).
			if !cmd.Flags().Changed("by") {
				q.By = "name"
			}
			opts := chrome.SelectOpts{Query: q, Filter: filter, OptionMatch: optMatch, Sep: sep}
			a.runResolved("select", func(ctx context.Context, b chrome.Browser, id string) (any, error) {
				return b.Select(ctx, id, args[0], args[1], opts)
			})
			return nil
		},
	}
	c.Flags().StringVar(&filter, "filter", "", "type this text into the prompt to narrow options before selecting")
	c.Flags().StringVar(&optMatch, "option-match", "", "option match mode: contains (default)|exact|regex")
	c.Flags().StringVar(&sep, "sep", ">", "cascade path separator between option levels")
	return a.withWaitText(c)
}

func (a *App) cmdGrid() *cobra.Command {
	return &cobra.Command{
		Use:   "grid [selector]",
		Short: "Read a table/grid as structured {headers, rows} from the accessibility tree",
		Args:  cobra.MaximumNArgs(1),
		RunE: a.targetAction("grid", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
			sel := ""
			if len(args) == 1 {
				sel = args[0]
			}
			return b.Grid(ctx, id, sel, a.queryOpts())
		}),
	}
}

func (a *App) cmdScroll() *cobra.Command {
	var dx, dy float64
	var into, wheel bool
	c := &cobra.Command{
		Use:   "scroll [selector]",
		Short: "Scroll by --dx/--dy (a selector's box or the window), --to a selector into view, or --wheel",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			sel := ""
			if len(args) == 1 {
				sel = args[0]
			}
			opts := chrome.ScrollOpts{Dx: dx, Dy: dy, Into: into, Wheel: wheel, Query: a.queryOpts()}
			a.runResolved("scroll", func(ctx context.Context, b chrome.Browser, id string) (any, error) {
				return b.Scroll(ctx, id, sel, opts)
			})
			return nil
		},
	}
	c.Flags().Float64Var(&dx, "dx", 0, "horizontal scroll delta in pixels")
	c.Flags().Float64Var(&dy, "dy", 0, "vertical scroll delta in pixels (positive scrolls down)")
	c.Flags().BoolVar(&into, "to", false, "scroll the selector into view instead of by a delta")
	c.Flags().BoolVar(&wheel, "wheel", false, "dispatch a real mouse wheel (for grids that render on wheel, not scroll)")
	return c
}

func (a *App) cmdScreenshot() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use: "screenshot", Short: "Screenshot the target tab to a PNG (cwd, or -o)",
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel := a.ctx()
			defer cancel()
			tgt, b, rerr := a.resolveTarget(ctx)
			if rerr != nil {
				a.emitErr("screenshot", rerr.Code, rerr.Message, nil)
				return nil
			}
			png, err := b.Screenshot(ctx, tgt.ID)
			if err != nil {
				a.emitErr("screenshot", classifyActionErr(err), err.Error(), nil)
				return nil
			}
			a.emitArtifact("screenshot", tgt, png, out, "png")
			return nil
		},
	}
	c.Flags().StringVarP(&out, "output", "o", "", "output path, or - for stdout (default ./screenshot-<timestamp>.png)")
	return c
}

func (a *App) cmdPDF() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use: "pdf", Short: "Print the target tab to PDF (cwd, or -o)",
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel := a.ctx()
			defer cancel()
			tgt, b, rerr := a.resolveTarget(ctx)
			if rerr != nil {
				a.emitErr("pdf", rerr.Code, rerr.Message, nil)
				return nil
			}
			pdf, err := b.PDF(ctx, tgt.ID)
			if err != nil {
				a.emitErr("pdf", classifyActionErr(err), err.Error(), nil)
				return nil
			}
			a.emitArtifact("pdf", tgt, pdf, out, "pdf")
			return nil
		},
	}
	c.Flags().StringVarP(&out, "output", "o", "", "output path, or - for stdout (default ./pdf-<timestamp>.pdf)")
	return c
}

// emitArtifact writes binary output: raw to stdout for "-o -", else to `out`
// (or a default ./<command>-<ts>.<ext> with a collision counter), then emits.
func (a *App) emitArtifact(command string, tgt *result.TargetInfo, data []byte, out, ext string) {
	if out == "-" {
		_, _ = a.out.Write(data)
		if !a.quiet {
			fmt.Fprintf(a.err, "wrote %d bytes to stdout\n", len(data))
		}
		return
	}
	path := out
	if path == "" {
		// The default name gets a collision counter; an explicit -o path is
		// honored as-is (overwrite), as the user named it.
		path = uniquePath(fmt.Sprintf("./%s-%s.%s", command, time.Now().Format("20060102-150405"), ext))
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		a.emitErr(command, result.CodeGeneric, err.Error(), nil)
		return
	}
	a.emitOK(command, tgt, map[string]any{"path": path, "bytes": len(data)})
}

// uniquePath returns path if free, else inserts -1, -2, … before the extension.
func uniquePath(path string) string {
	if _, err := os.Stat(path); err != nil {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(cand); err != nil {
			return cand
		}
	}
}

func (a *App) cmdRaw() *cobra.Command {
	var browserLevel, listDomains bool
	c := &cobra.Command{
		Use: "raw <domain.method> [params-json]", Short: "Call any raw CDP method", Args: cobra.MaximumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, cancel := a.ctx()
			defer cancel()

			// --list enumerates the connected Chrome's live protocol.
			if listDomains {
				b, berr := a.getBrowser(ctx)
				if berr != nil {
					a.emitErr("raw", berr.Code, berr.Message, nil)
					return nil
				}
				a.emitRaw(ctx, nil, b, "", "Schema.getDomains", nil)
				return nil
			}
			if len(args) == 0 {
				a.emitErr("raw", "usage", "raw requires <domain.method> (or --list)", nil)
				return nil
			}

			var params json.RawMessage
			if len(args) == 2 {
				if !json.Valid([]byte(args[1])) {
					a.emitErr("raw", "usage", fmt.Sprintf("params is not valid JSON: %s", args[1]), nil)
					return nil
				}
				params = json.RawMessage(args[1])
			}

			// --browser routes Browser.* / Target.* methods to the browser-level
			// executor (no page target required).
			if browserLevel {
				b, berr := a.getBrowser(ctx)
				if berr != nil {
					a.emitErr("raw", berr.Code, berr.Message, nil)
					return nil
				}
				a.emitRaw(ctx, nil, b, "", args[0], params)
				return nil
			}
			tgt, b, rerr := a.resolveTarget(ctx)
			if rerr != nil {
				a.emitErr("raw", rerr.Code, rerr.Message, nil)
				return nil
			}
			a.emitRaw(ctx, tgt, b, tgt.ID, args[0], params)
			return nil
		},
	}
	c.Flags().BoolVar(&browserLevel, "browser", false, "run at the browser level (for Browser.* / Target.* methods, no tab needed)")
	c.Flags().BoolVar(&listDomains, "list", false, "list the connected Chrome's CDP domains (Schema.getDomains)")
	return c
}

func (a *App) emitRaw(ctx context.Context, tgt *result.TargetInfo, b chrome.Browser, id, method string, params json.RawMessage) {
	v, err := b.Raw(ctx, id, method, params)
	if err != nil {
		a.emitErr("raw", "cdp_error", err.Error(), nil)
		return
	}
	a.emitOK("raw", tgt, v)
}

// targetAction wires a target-taking command: resolve the tab, run fn against
// the connected browser, emit.
func (a *App) targetAction(command string, fn func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error)) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		ctx, cancel := a.ctx()
		defer cancel()
		tgt, b, rerr := a.resolveTarget(ctx)
		if rerr != nil {
			a.emitErr(command, rerr.Code, rerr.Message, nil)
			return nil
		}
		res, err := fn(ctx, b, tgt.ID, args)
		if err != nil {
			code, msg, details := a.classifyWithTabHint(b, tgt.ID, err)
			a.emitErr(command, code, msg, details)
			return nil
		}
		// --wait-text folds "act, then confirm the write landed" into one call:
		// after the action succeeds, block until the page contains the text.
		if a.actWaitText != "" {
			if _, werr := b.Wait(ctx, tgt.ID, chrome.WaitCond{Text: a.actWaitText}); werr != nil {
				a.emitErr(command, classifyActionErr(werr), "action ok but wait-text failed: "+werr.Error(), nil)
				return nil
			}
			if m, ok := res.(map[string]any); ok {
				m["waited_text"] = a.actWaitText
			}
		}
		a.emitOK(command, tgt, res)
		return nil
	}
}

// withWaitText registers the shared --wait-text flag on an action command.
func (a *App) withWaitText(c *cobra.Command) *cobra.Command {
	c.Flags().StringVar(&a.actWaitText, "wait-text", "", "after the action, wait until the page contains this text (e.g. a 'Saved' toast)")
	return c
}

func (a *App) cmdDoctor() *cobra.Command {
	return &cobra.Command{
		Use: "doctor", Short: "Check the Chrome connection and explain how to fix it",
		RunE: func(*cobra.Command, []string) error {
			pf := browser.FindPortFile("")
			if pf == "" {
				a.emitErr("doctor", "connection_failed", "no DevToolsActivePort found — enable chrome://inspect/#remote-debugging on your Chrome, or run a command without --no-launch to auto-launch a managed Chrome", nil)
				return nil
			}
			ws, err := browser.WSURLFromPortFile(pf)
			if err != nil {
				a.emitErr("doctor", "connection_failed", "port file unreadable: "+err.Error(), map[string]any{"port_file": pf})
				return nil
			}
			// Probe reachability — a stale port file must not report "ready".
			if !chrome.Reachable(ws) {
				a.emitErr("doctor", "connection_failed", "port file present but the debug endpoint is not reachable (stale) — re-enable chrome://inspect/#remote-debugging", map[string]any{"port_file": pf, "ws": ws})
				return nil
			}
			a.emitOK("doctor", nil, map[string]any{"port_file": pf, "ws": ws, "status": "debug endpoint reachable — Path B attach ready"})
			return nil
		},
	}
}

func (a *App) cmdDaemon() *cobra.Command {
	daemon := &cobra.Command{Use: "daemon", Short: "Manage the background CDP connection"}
	emit := func(res map[string]any, err error) {
		if err != nil {
			a.emitErr("daemon", result.CodeDaemon, err.Error(), nil)
			return
		}
		a.emitOK("daemon", nil, res)
	}
	daemon.AddCommand(
		&cobra.Command{
			Use: "start", Short: "Start (or reuse) the background daemon",
			RunE: func(*cobra.Command, []string) error {
				if a.daemonStart == nil {
					a.emitErr("daemon", result.CodeDaemon, "daemon control unavailable", nil)
					return nil
				}
				emit(a.daemonStart(a.connOpts()))
				return nil
			},
		},
		&cobra.Command{
			Use: "stop", Short: "Stop the background daemon",
			RunE: func(*cobra.Command, []string) error {
				if a.daemonStop == nil {
					a.emitErr("daemon", result.CodeDaemon, "daemon control unavailable", nil)
					return nil
				}
				emit(a.daemonStop(a.connOpts()))
				return nil
			},
		},
		&cobra.Command{
			Use: "status", Short: "Show whether the daemon is running",
			RunE: func(*cobra.Command, []string) error {
				if a.daemonStatus == nil {
					a.emitOK("daemon", nil, map[string]any{"mode": "direct-connect"})
					return nil
				}
				emit(a.daemonStatus(a.connOpts()))
				return nil
			},
		},
	)
	return daemon
}

func (a *App) cmdSession() *cobra.Command {
	return &cobra.Command{
		Use:   "session",
		Short: "Batch mode: read NDJSON argv commands on stdin, run each over one held connection, emit NDJSON results",
		Long: "Read one command per stdin line as a JSON array of argv, run it against a\n" +
			"single held Chrome connection (no per-command process spawn or reconnect),\n" +
			"and print each result as one JSON line. Comment lines (#) and blank lines are\n" +
			"skipped. Combine with `snap`'s element refs and `--by ref` to act on nodes\n" +
			"without re-resolving them:\n\n" +
			"  printf '%s\\n' '[\"use\",\"url:workday\"]' '[\"snap\"]' '[\"click\",\"e42\",\"--by\",\"ref\"]' | chrome-cdp session",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			// Each result line is a JSON envelope (NDJSON) regardless of the global
			// --json default.
			a.defaults.JSON = true
			r := a.in
			if r == nil {
				r = os.Stdin
			}
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tolerate long lines
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				var argv []string
				if err := json.Unmarshal([]byte(line), &argv); err != nil {
					a.emitErr("session", result.CodeUsage, "each line must be a JSON array of argv strings: "+err.Error(), nil)
					continue
				}
				if len(argv) == 0 {
					continue
				}
				// Reuse the full command tree per line; the browser connection is
				// cached on the App, so only the first line pays the connect cost.
				a.Execute(argv...)
			}
			if err := sc.Err(); err != nil {
				a.emitErr("session", result.CodeGeneric, err.Error(), nil)
				return nil
			}
			// The session itself succeeded (stdin drained cleanly); per-line success
			// or failure is carried in each NDJSON envelope, not the process exit.
			a.exitCode = result.ExitOK
			return nil
		},
	}
}

func (a *App) cmdExitCodes() *cobra.Command {
	return &cobra.Command{
		Use: "exit-codes", Short: "Print the exit-code contract",
		RunE: func(*cobra.Command, []string) error {
			for _, e := range result.ExitCodes() {
				fmt.Fprintf(a.out, "%d  %s\n", e.Code, e.Desc)
			}
			return nil
		},
	}
}

func (a *App) cmdVersion() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print the version",
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintln(a.out, Version)
			return nil
		},
	}
}
