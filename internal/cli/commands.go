package cli

import (
	"context"
	"encoding/json"
	"fmt"
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
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(*cobra.Command, []string) {
			if a.start.IsZero() {
				a.start = time.Now()
			}
		},
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&a.jsonOut, "json", false, "machine-readable output (one JSON value to stdout)")
	pf.StringVar(&a.targetFlag, "target", "", "tab to act on (idprefix | url:<s> | title:<s> | @N)")
	pf.DurationVar(&a.timeout, "timeout", 30*time.Second, "max time to wait for the command")
	pf.BoolVar(&a.noLaunch, "no-launch", false, "don't auto-launch a fallback Chrome")
	pf.BoolVarP(&a.quiet, "quiet", "q", false, "suppress non-essential output")

	root.AddCommand(
		a.cmdList(), a.cmdUse(), a.cmdNav(), a.cmdEval(), a.cmdSnap(),
		a.cmdClick(), a.cmdType(), a.cmdScreenshot(), a.cmdRaw(),
		a.cmdDoctor(), a.cmdDaemon(), a.cmdExitCodes(), a.cmdVersion(),
	)
	return root
}

func classifyActionErr(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "deadline exceeded"), strings.Contains(s, "timeout"),
		strings.Contains(s, "could not find node"), strings.Contains(s, "no node"):
		return "target_timeout"
	default:
		return "cdp_error"
	}
}

func (a *App) cmdList() *cobra.Command {
	return &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List open tabs",
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
			rows := make([]tabRow, len(tabs))
			for i, t := range tabs {
				rows[i] = tabRow{Idx: i + 1, ID: t.ID, Title: t.Title, URL: t.URL}
			}
			a.emitOK("list", nil, map[string]any{"tabs": rows})
			return nil
		},
	}
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
			if a.setCurrent == nil {
				a.emitErr("use", "generic", "cannot set the current tab: sticky-target state is unavailable", nil)
				return nil
			}
			// Persist the RESOLVED id, not the spec: an ephemeral form like @N
			// must not be re-resolved against a later, reordered tab list.
			if err := a.setCurrent(tgt.ID); err != nil {
				a.emitErr("use", "generic", "cannot persist the current tab: "+err.Error(), nil)
				return nil
			}
			a.emitOK("use", tgt, map[string]any{"current": tgt.ID})
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
	return &cobra.Command{
		Use: "snap", Short: "Accessibility-tree snapshot of the target tab",
		RunE: a.targetAction("snap", func(ctx context.Context, b chrome.Browser, id string, _ []string) (any, error) {
			return b.Snapshot(ctx, id)
		}),
	}
}

func (a *App) cmdClick() *cobra.Command {
	return &cobra.Command{
		Use: "click <selector>", Short: "Click an element (auto-waits)", Args: cobra.ExactArgs(1),
		RunE: a.targetAction("click", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
			return b.Click(ctx, id, args[0])
		}),
	}
}

func (a *App) cmdType() *cobra.Command {
	return &cobra.Command{
		Use: "type <selector> <text>", Short: "Type text via real keystrokes", Args: cobra.ExactArgs(2),
		RunE: a.targetAction("type", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
			return b.Type(ctx, id, args[0], args[1])
		}),
	}
}

func (a *App) cmdScreenshot() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use: "screenshot", Short: "Screenshot the target tab to a PNG in cwd",
		RunE: a.targetAction("screenshot", func(ctx context.Context, b chrome.Browser, id string, _ []string) (any, error) {
			path := out
			if path == "" {
				path = fmt.Sprintf("./screenshot-%s.png", time.Now().Format("20060102-150405"))
			}
			return b.Screenshot(ctx, id, path)
		}),
	}
	c.Flags().StringVarP(&out, "output", "o", "", "output path (default ./screenshot-<timestamp>.png)")
	return c
}

func (a *App) cmdRaw() *cobra.Command {
	var browserLevel bool
	c := &cobra.Command{
		Use: "raw <domain.method> [params-json]", Short: "Call any raw CDP method", Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			var params json.RawMessage
			if len(args) == 2 {
				if !json.Valid([]byte(args[1])) {
					a.emitErr("raw", "usage", fmt.Sprintf("params is not valid JSON: %s", args[1]), nil)
					return nil
				}
				params = json.RawMessage(args[1])
			}
			ctx, cancel := a.ctx()
			defer cancel()

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
			a.emitErr(command, classifyActionErr(err), err.Error(), nil)
			return nil
		}
		a.emitOK(command, tgt, res)
		return nil
	}
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
	report := func(state string) func(*cobra.Command, []string) error {
		return func(*cobra.Command, []string) error {
			a.emitOK("daemon", nil, map[string]any{"mode": "direct-connect", "state": state,
				"note": "shared daemon not yet implemented — commands connect per invocation"})
			return nil
		}
	}
	daemon.AddCommand(
		&cobra.Command{Use: "status", Short: "Show daemon state", RunE: report("n/a")},
		&cobra.Command{Use: "start", Short: "Start the daemon", RunE: report("noop")},
		&cobra.Command{Use: "stop", Short: "Stop the daemon", RunE: report("noop")},
	)
	return daemon
}

func (a *App) cmdExitCodes() *cobra.Command {
	return &cobra.Command{
		Use: "exit-codes", Short: "Print the exit-code contract",
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprint(a.out, `0  success
1  generic / unclassified
2  usage (bad flags/args)
3  connection (attach/launch failed)
4  target/timeout (selector not found, timeout, ambiguous/unknown target)
5  cdp protocol error
6  daemon error
`)
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
