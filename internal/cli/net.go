package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// netMatchFlags are the request-matching flags `net`, `net wait`, and
// `wait --request` all share. One struct, so the three entry points cannot
// drift into three subtly different grammars.
type netMatchFlags struct {
	url      string
	methods  []string
	status   string
	types    []string
	xhr      bool
	failed   bool
	headers  bool
	body     bool
	noRedact bool
}

// register adds the matching flags to c. The URL matcher is NOT registered here:
// it is --url under `net` and `net wait`, but --request under `wait`, where
// --url already means "the tab's URL".
func (mf *netMatchFlags) register(c *cobra.Command) {
	f := c.Flags()
	f.StringArrayVar(&mf.methods, "method", nil, "only these HTTP methods, e.g. POST (repeatable)")
	f.StringVar(&mf.status, "status", "", "only these statuses: 200 | 2xx | >=400 | !2xx")
	f.StringArrayVar(&mf.types, "type", nil, "only these resource types: document|xhr|fetch|script|stylesheet|image|font|websocket|other (repeatable)")
	f.BoolVar(&mf.xhr, "xhr", false, "shorthand for --type xhr --type fetch")
	f.BoolVar(&mf.failed, "failed", false, "only non-2xx responses and network-level failures")
	f.BoolVar(&mf.headers, "headers", false, "include request and response headers (sensitive values redacted)")
	f.BoolVar(&mf.body, "body", false, "include request and response bodies (size-capped by net_max_body)")
	f.BoolVar(&mf.noRedact, "no-redact", false, "do NOT redact credential-shaped headers and URL parameters")
}

// validate compiles the matching flags without touching Chrome. statusSet says
// whether --status was given at all, so an unset flag means "any status" while
// an explicitly empty one is the usage error VS-4 asks for.
func (mf *netMatchFlags) validate(statusSet bool) (chrome.NetOpts, *result.Err) {
	usage := func(msg string) (chrome.NetOpts, *result.Err) {
		return chrome.NetOpts{}, &result.Err{Code: result.CodeUsage, Message: msg}
	}
	if _, err := chrome.NetURLMatcher(mf.url); err != nil {
		return usage(err.Error())
	}
	if statusSet {
		if _, err := chrome.ParseNetStatus(mf.status); err != nil {
			return usage(err.Error())
		}
	}
	methods := make([]string, 0, len(mf.methods))
	for _, m := range mf.methods {
		n := strings.ToUpper(strings.TrimSpace(m))
		if n == "" {
			return usage("--method needs an HTTP method, e.g. POST")
		}
		methods = append(methods, n)
	}
	types := make([]string, 0, len(mf.types)+2)
	for _, t := range mf.types {
		n, ok := chrome.NormalizeNetType(t)
		if !ok {
			return usage(fmt.Sprintf("unknown --type %q (want one of: %s)", t, strings.Join(chrome.NetTypes, ", ")))
		}
		types = append(types, n)
	}
	if mf.xhr {
		types = append(types, "xhr", "fetch")
	}
	status := mf.status
	if !statusSet {
		status = ""
	}
	return chrome.NetOpts{
		URL: mf.url, Methods: methods, Status: status, Types: types, Failed: mf.failed,
		Headers: mf.headers, Body: mf.body, NoRedact: mf.noRedact,
	}, nil
}

// cond renders the validated match flags as the blocking condition.
func (mf *netMatchFlags) cond(opts chrome.NetOpts) chrome.NetCond {
	return chrome.NetCond{
		URL: opts.URL, Methods: opts.Methods, Status: opts.Status, Types: opts.Types,
		Failed: opts.Failed, Headers: opts.Headers, Body: opts.Body, NoRedact: opts.NoRedact,
	}
}

// netFlags are the `net` verb's own flags: the shared matchers plus the
// listing-only ones. Rebuilt per Execute (like console's), so they reset between
// `session` lines.
type netFlags struct {
	netMatchFlags
	limit       int
	since       time.Duration
	clear       bool
	follow      bool
	failOnMatch bool
}

func (nf *netFlags) register(c *cobra.Command) {
	nf.netMatchFlags.register(c)
	f := c.Flags()
	f.StringVar(&nf.url, "url", "", "only requests whose URL contains this substring (re:<pattern> for a regex)")
	f.IntVar(&nf.limit, "limit", 100, "most recent N matching requests")
	f.DurationVar(&nf.since, "since", 0, "only requests newer than this (e.g. 30s)")
	f.BoolVar(&nf.clear, "clear", false, "drop the buffered requests after reading (with no other flag, just drop them)")
	f.BoolVar(&nf.follow, "follow", false, "stream completed requests as NDJSON until --timeout or interrupt")
	f.BoolVar(&nf.failOnMatch, "fail-on-match", false, "exit 1 if at least one request is returned (the requests are still reported)")
}

// validate checks the flags and builds the browser options WITHOUT touching
// Chrome: a malformed `net` is usage/exit 2 with no connection attempted, and
// therefore no consent prompt the user never should have seen.
func (nf *netFlags) validate(inSession, statusSet bool) (chrome.NetOpts, *result.Err) {
	usage := func(msg string) (chrome.NetOpts, *result.Err) {
		return chrome.NetOpts{}, &result.Err{Code: result.CodeUsage, Message: msg}
	}
	if nf.follow && nf.failOnMatch {
		return usage("--follow streams forever and --fail-on-match asserts on a finished read; they cannot combine")
	}
	if nf.follow && inSession {
		return usage("net --follow cannot run inside `session`: a streaming command would break session's one-envelope-per-line contract — run it as its own command")
	}
	if nf.since < 0 {
		return usage("--since must be a positive duration (e.g. 30s)")
	}
	if nf.limit < 0 {
		return usage("--limit must be zero (no limit) or positive")
	}
	opts, verr := nf.netMatchFlags.validate(statusSet)
	if verr != nil {
		return chrome.NetOpts{}, verr
	}
	opts.Limit, opts.Since, opts.Clear = nf.limit, nf.since, nf.clear
	return opts, nil
}

func (a *App) cmdNet() *cobra.Command {
	var nf netFlags
	c := &cobra.Command{
		Use:   "net",
		Short: "Read the tab's HTTP requests (server-side filtered; --follow streams NDJSON)",
		Long: "Read what the page requested: method, URL, status, timing, and sizes for every\n" +
			"HTTP request the tab made, retained since the connection attached to it.\n\n" +
			"Capture starts when the connection attaches to a tab, not when `net` first runs,\n" +
			"so the 401 behind an empty screen is already buffered by the time you look for\n" +
			"it. Headers and bodies are omitted unless --headers / --body ask for them, and\n" +
			"credential-shaped headers and URL parameters are redacted unless --no-redact.\n\n" +
			"  chrome-cdp net --xhr --limit 20                       # recent API calls\n" +
			"  chrome-cdp net --failed                               # what broke\n" +
			"  chrome-cdp net --url /api/save --method POST --body   # inspect the payload\n" +
			"  chrome-cdp net --clear                                # reset before an action",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts, verr := nf.validate(a.inSession, cmd.Flags().Changed("status"))
			if verr != nil {
				a.emitErr("net", verr.Code, verr.Message, nil)
				return nil
			}
			ctx, cancel := a.ctx()
			defer cancel()
			tgt, b, rerr := a.resolveTarget(ctx)
			if rerr != nil {
				a.emitErr("net", rerr.Code, rerr.Message, nil)
				return nil
			}
			if nf.follow {
				a.followNet(ctx, b, tgt, opts)
				return nil
			}
			res, err := b.Net(ctx, tgt.ID, opts)
			if err != nil {
				a.emitErr("net", classifyActionErr(err), err.Error(), nil)
				return nil
			}
			if nf.failOnMatch {
				if n := netCount(res); n > 0 {
					// The assertion tripping must not suppress the evidence: the
					// envelope still carries every matching request, so a CI log
					// shows WHAT failed, not just that something did.
					a.emit(result.Envelope{
						OK: false, Command: "net", Target: tgt, Result: res,
						Error: &result.Err{
							Code:    result.CodeAssertFailed,
							Message: fmt.Sprintf("--fail-on-match: %d request(s) matched", n),
						},
					})
					return nil
				}
			}
			a.emitOK("net", tgt, res)
			return nil
		},
	}
	nf.register(c)
	c.AddCommand(a.cmdNetWait())
	return c
}

// cmdNetWait is the alias form of `wait --request`, kept so `net`'s flags stay
// together for somebody who already has a `net` invocation to turn into a wait.
func (a *App) cmdNetWait() *cobra.Command {
	var mf netMatchFlags
	c := &cobra.Command{
		Use:   "wait",
		Short: "Block until a matching request completes (alias for `wait --request`)",
		Long: "Block until one specific request completes, rather than until the whole page\n" +
			"settles — the sharper tool for \"did my save actually POST?\" on a page whose\n" +
			"polling or long-lived stream makes `wait --idle` unreliable.\n\n" +
			"Already-buffered requests are matched FIRST, so a request that completed\n" +
			"between the action and this call is not missed.\n\n" +
			"  chrome-cdp net wait --url /api/save --status 2xx\n" +
			"  chrome-cdp wait --request /api/save --status 2xx     # the primary form",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.runNetWait(cmd, "net", &mf)
			return nil
		},
	}
	mf.register(c)
	c.Flags().StringVar(&mf.url, "url", "", "block until a request whose URL contains this substring completes (re:<pattern> for a regex)")
	return c
}

// runNetWait validates the match flags and blocks on the request, shared by
// `net wait` and `wait --request` so both report identically.
func (a *App) runNetWait(cmd *cobra.Command, command string, mf *netMatchFlags) {
	// --method / --status / --type only NARROW a match; on their own they would
	// block on "the next request of any kind", which is never what anybody means.
	// A wait needs a URL to identify the request, or --failed to say "whatever
	// breaks next".
	if mf.url == "" && !mf.failed {
		a.emitErr(command, result.CodeUsage,
			"waiting for a request needs something to identify it: a URL substring (--request/--url), or --failed; "+
				"--method/--status/--type only narrow a match", nil)
		return
	}
	opts, verr := mf.validate(cmd.Flags().Changed("status"))
	if verr != nil {
		a.emitErr(command, verr.Code, verr.Message, nil)
		return
	}
	cond := mf.cond(opts)
	a.runResolved(command, func(ctx context.Context, b chrome.Browser, id string) (any, error) {
		return b.NetWait(ctx, id, cond)
	})
}

// followNet streams one NDJSON envelope per completed request until the
// --timeout window closes, matching how `session` and `console --follow` stream
// so a caller parses one shape everywhere. Reaching the deadline is how a follow
// ends, so it exits 0.
func (a *App) followNet(ctx context.Context, b chrome.Browser, tgt *result.TargetInfo, opts chrome.NetOpts) {
	emit := func(v any) error {
		env := result.Envelope{
			OK: true, Command: "net", Target: tgt, Result: v,
			ElapsedMs: time.Since(a.start).Milliseconds(),
		}
		line, err := env.JSON()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.out, string(line))
		return err
	}
	if err := b.NetStream(ctx, tgt.ID, opts, emit); err != nil && !isDeadline(err) {
		a.emitErr("net", classifyActionErr(err), err.Error(), nil)
		return
	}
	a.exitCode = result.ExitOK
}

// netCount reads the request count out of a net result. It tolerates both an int
// (a direct in-process call) and a float64 (the same value after a round-trip
// through the daemon's JSON), because --fail-on-match's exit code must not
// depend on which connection path served the read.
func netCount(res any) int {
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
	if reqs, ok := m["requests"].([]any); ok {
		return len(reqs)
	}
	return 0
}
