package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// consoleFlags are the `console` verb's own flags. They live in a struct
// rebuilt per Execute (like nav's), so they reset between `session` lines.
type consoleFlags struct {
	grep        string
	levels      []string
	onlyErrors  bool
	limit       int
	since       time.Duration
	clear       bool
	follow      bool
	failOnMatch bool
}

func (cf *consoleFlags) register(c *cobra.Command) {
	f := c.Flags()
	f.StringVar(&cf.grep, "grep", "", "only messages whose text matches this regex")
	f.StringArrayVar(&cf.levels, "level", nil, "only these levels: debug|log|info|warn|error (repeatable)")
	f.BoolVar(&cf.onlyErrors, "only-errors", false, "shorthand for --level error (uncaught exceptions are reported at error level)")
	f.IntVar(&cf.limit, "limit", 100, "most recent N matching messages")
	f.DurationVar(&cf.since, "since", 0, "only messages newer than this (e.g. 30s)")
	f.BoolVar(&cf.clear, "clear", false, "drop the buffered messages after reading (with no other flag, just drop them)")
	f.BoolVar(&cf.follow, "follow", false, "stream new messages as NDJSON until --timeout or interrupt")
	f.BoolVar(&cf.failOnMatch, "fail-on-match", false, "exit 1 if at least one message is returned (the messages are still reported)")
}

// validate checks the flags and builds the browser options WITHOUT touching
// Chrome: a malformed `console` is usage/exit 2 with no connection attempted,
// and therefore no consent prompt the user never should have seen.
func (cf *consoleFlags) validate(inSession bool) (chrome.ConsoleOpts, *result.Err) {
	usage := func(msg string) (chrome.ConsoleOpts, *result.Err) {
		return chrome.ConsoleOpts{}, &result.Err{Code: result.CodeUsage, Message: msg}
	}
	if cf.follow && cf.failOnMatch {
		return usage("--follow streams forever and --fail-on-match asserts on a finished read; they cannot combine")
	}
	if cf.follow && inSession {
		return usage("console --follow cannot run inside `session`: a streaming command would break session's one-envelope-per-line contract — run it as its own command")
	}
	if cf.grep != "" {
		if _, err := regexp.Compile(cf.grep); err != nil {
			return usage(fmt.Sprintf("--grep is not a valid regex: %v", err))
		}
	}
	levels := make([]string, 0, len(cf.levels)+1)
	for _, l := range cf.levels {
		n, ok := chrome.NormalizeConsoleLevel(l)
		if !ok {
			return usage(fmt.Sprintf("unknown --level %q (want one of: %s)", l, strings.Join(chrome.ConsoleLevels, ", ")))
		}
		levels = append(levels, n)
	}
	if cf.onlyErrors {
		levels = append(levels, "error")
	}
	if cf.since < 0 {
		return usage("--since must be a positive duration (e.g. 30s)")
	}
	if cf.limit < 0 {
		return usage("--limit must be zero (no limit) or positive")
	}
	return chrome.ConsoleOpts{
		Grep:   cf.grep,
		Levels: levels,
		Limit:  cf.limit,
		Since:  cf.since,
		Clear:  cf.clear,
	}, nil
}

func (a *App) cmdConsole() *cobra.Command {
	var cf consoleFlags
	c := &cobra.Command{
		Use:   "console",
		Short: "Read the tab's console output and uncaught exceptions (server-side filtered; --follow streams NDJSON)",
		Long: "Read what the page said: console.* output and uncaught exceptions, with their\n" +
			"stack, retained per tab since the connection attached to it.\n\n" +
			"Capture starts when the connection attaches to a tab, not when `console` first\n" +
			"runs — so the error behind a failed click is already buffered by the time you\n" +
			"look for it. The buffer is bounded (config: console_buffer, console_max_entry);\n" +
			"a nonzero `dropped` in the result means messages were evicted before you read.\n\n" +
			"With --no-daemon no process was alive to receive earlier events, so the read\n" +
			"reports buffered 0 and carries a note saying there is no retained history.\n\n" +
			"  chrome-cdp console --only-errors                       # what broke\n" +
			"  chrome-cdp console --grep \"\\[Checkout\\]\" --limit 20    # one subsystem\n" +
			"  chrome-cdp console --clear                             # reset before an action\n" +
			"  chrome-cdp console --follow --level error              # watch while you work",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			opts, verr := cf.validate(a.inSession)
			if verr != nil {
				a.emitErr("console", verr.Code, verr.Message, nil)
				return nil
			}
			ctx, cancel := a.ctx()
			defer cancel()
			tgt, b, rerr := a.resolveTarget(ctx)
			if rerr != nil {
				a.emitErr("console", rerr.Code, rerr.Message, nil)
				return nil
			}
			if cf.follow {
				a.followConsole(ctx, b, tgt, opts)
				return nil
			}
			res, err := b.Console(ctx, tgt.ID, opts)
			if err != nil {
				a.emitErr("console", classifyActionErr(err), err.Error(), nil)
				return nil
			}
			if cf.failOnMatch {
				if n := consoleCount(res); n > 0 {
					// The assertion tripping must not suppress the evidence: the
					// envelope still carries every matching message, so a CI log
					// shows WHAT failed, not just that something did.
					a.emit(result.Envelope{
						OK: false, Command: "console", Target: tgt, Result: res,
						Error: &result.Err{
							Code:    result.CodeAssertFailed,
							Message: fmt.Sprintf("--fail-on-match: %d console message(s) matched", n),
						},
					})
					return nil
				}
			}
			a.emitOK("console", tgt, res)
			return nil
		},
	}
	cf.register(c)
	return c
}

// followConsole streams one NDJSON envelope per message until the --timeout
// window closes, matching how `session` streams so a caller parses one shape in
// both modes. Reaching the deadline is how a follow ends, so it exits 0.
func (a *App) followConsole(ctx context.Context, b chrome.Browser, tgt *result.TargetInfo, opts chrome.ConsoleOpts) {
	emit := func(v any) error {
		env := result.Envelope{
			OK: true, Command: "console", Target: tgt, Result: v,
			ElapsedMs: time.Since(a.start).Milliseconds(),
		}
		line, err := env.JSON()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.out, string(line))
		return err
	}
	if err := b.ConsoleStream(ctx, tgt.ID, opts, emit); err != nil && !isDeadline(err) {
		a.emitErr("console", classifyActionErr(err), err.Error(), nil)
		return
	}
	a.exitCode = result.ExitOK
}

// isDeadline reports whether an error is just the follow window closing. The
// daemon RPC flattens errors to strings, so the wrapped context error does not
// survive the socket and the text is what there is to match on.
func isDeadline(err error) bool {
	return err != nil && (errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded"))
}

// consoleCount reads the message count out of a console result. It tolerates
// both an int (a direct in-process call) and a float64 (the same value after a
// round-trip through the daemon's JSON), because --fail-on-match's exit code
// must not depend on which connection path served the read.
func consoleCount(res any) int {
	m, ok := res.(map[string]any)
	if !ok {
		return 0
	}
	switch n := m["count"].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	if msgs, ok := m["messages"].([]any); ok {
		return len(msgs)
	}
	return 0
}
