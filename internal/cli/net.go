package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/encode"
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
	har         string
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
	f.StringVar(&nf.har, "har", "", "write the matching requests to this path as HAR 1.2 (headers included, redacted "+
		"unless --no-redact; add --body for payloads) and print a summary instead of the listing")
}

// harUsageErr checks the two --har failures that are pure grammar (RFC-0017):
// an empty path, and combining with --follow, which streams forever and has
// no end to write a file at. Both must be caught before nf.validate runs
// anything else, so they are reported the same way regardless of what other
// flags happen to be set.
func (nf *netFlags) harUsageErr(harSet bool) *result.Err {
	if !harSet {
		return nil
	}
	if nf.follow {
		return &result.Err{Code: result.CodeUsage, Message: "--har cannot combine with --follow: a stream has no end to write a file at"}
	}
	if nf.har == "" {
		return &result.Err{Code: result.CodeUsage, Message: "--har needs a path to write the HAR to"}
	}
	return nil
}

// checkHarTarget reports whether the HAR could actually be written, BEFORE
// resolveTarget — the same split `record`'s checkExportTarget makes: an
// existing directory or a missing/non-directory parent is detected with
// os.Stat alone and is `generic` because the outcome depends on the
// environment, not the form of the invocation.
//
// With --clear the buffer is dropped inside the read itself, before the file
// is written, so a write failure after that point loses data rather than
// just costing a free retry. The controller ruling on RFC-0017 (Open
// Questions) is to run `record stop -o`'s CreateTemp-style writability probe
// in that case, so the likely cause of a late failure is removed up front;
// without --clear the stat-only check is enough, as the RFC specifies.
func checkHarTarget(path string, clear bool) *result.Err {
	generic := func(format string, args ...any) *result.Err {
		return &result.Err{Code: result.CodeGeneric, Message: fmt.Sprintf(format, args...)}
	}
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return generic("cannot write the HAR to %q: it is a directory", path)
	}
	dir := filepath.Dir(path)
	fi, err := os.Stat(dir)
	if err != nil {
		return generic("cannot write the HAR to %q: %v", path, err)
	}
	if !fi.IsDir() {
		return generic("cannot write the HAR to %q: %q is not a directory", path, dir)
	}
	if !clear {
		return nil
	}
	f, err := os.CreateTemp(dir, ".chrome-cdp-har-*")
	if err != nil {
		return generic("cannot write the HAR to %q: %v (--clear drops the buffer before the write — retry with a writable path)", path, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
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
		return usage("net --follow cannot run inside `session` or a recipe: a streaming command would break the one-envelope-per-line contract a batch promises — run it as its own command")
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
			harSet := cmd.Flags().Changed("har")
			if verr := nf.harUsageErr(harSet); verr != nil {
				a.emitErr("net", verr.Code, verr.Message, nil)
				return nil
			}
			opts, verr := nf.validate(a.inSession, cmd.Flags().Changed("status"))
			if verr != nil {
				a.emitErr("net", verr.Code, verr.Message, nil)
				return nil
			}
			if harSet {
				if verr := checkHarTarget(nf.har, nf.clear); verr != nil {
					a.emitErr("net", verr.Code, verr.Message, nil)
					return nil
				}
				// RFC-0017: `--har` is an output mode of the existing read, not a
				// new capability — it forces the headers the export needs
				// regardless of whether --headers was also passed.
				opts.Headers = true
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
			out := res
			if harSet {
				summary, herr := writeNetHar(res, nf.har, opts)
				if herr != nil {
					a.emitErr("net", herr.Code, herr.Message, nil)
					return nil
				}
				out = summary
			}
			if nf.failOnMatch {
				if n := netCount(res); n > 0 {
					// The assertion tripping must not suppress the evidence: the
					// file (or the listing) still carries every matching request,
					// so a CI log shows WHAT failed, not just that something did.
					// The file was already written above, so a tripped assertion
					// never loses the export.
					a.emit(result.Envelope{
						OK: false, Command: "net", Target: tgt, Result: out,
						Error: &result.Err{
							Code:    result.CodeAssertFailed,
							Message: fmt.Sprintf("--fail-on-match: %d request(s) matched", n),
						},
					})
					return nil
				}
			}
			a.emitOK("net", tgt, out)
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

// netHarSummaryPassthrough are the listing's own accounting keys, carried
// into the HAR summary unchanged in meaning (RFC-0017's result envelope): the
// daemon's JSON round trip may hand them back as float64 rather than int, and
// this is deliberately NOT retyped — the envelope only has to reproduce
// whatever the read itself reported.
var netHarSummaryPassthrough = []string{"pending", "buffered", "dropped", "truncated", "note"}

// writeNetHar builds the HAR from a net result's rows and writes it to path,
// returning the summary envelope RFC-0017 documents. It is called AFTER the
// read (so the filters, redaction and --clear have already run exactly as
// they do for the listing) and after checkHarTarget (so the browser was
// never contacted over a bad path).
func writeNetHar(res any, path string, opts chrome.NetOpts) (map[string]any, *result.Err) {
	generic := func(format string, args ...any) *result.Err {
		return &result.Err{Code: result.CodeGeneric, Message: fmt.Sprintf(format, args...)}
	}
	m, ok := res.(map[string]any)
	if !ok {
		return nil, generic("the network read returned an unexpected shape")
	}
	entries, err := encode.DecodeNetEntries(m["requests"])
	if err != nil {
		return nil, generic("%s", err.Error())
	}
	data, err := encode.HAR(entries, encode.HAROpts{Version: Version, Now: time.Now()})
	if err != nil {
		return nil, generic("%s", err.Error())
	}
	// The user named the path; an existing file is overwritten, as `record`
	// and `screenshot -o` already do. 0600, not 0644: a HAR is a record of the
	// user's logged-in session and, with --no-redact, holds live credentials.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, generic("%s", err.Error())
	}
	truncatedBodies := 0
	for _, e := range entries {
		if e.BodyTruncated {
			truncatedBodies++
		}
	}
	out := map[string]any{
		"path": path, "bytes": len(data), "entries": len(entries),
		"redacted": !opts.NoRedact, "with_content": opts.Body,
		"truncated_bodies": truncatedBodies,
	}
	for _, k := range netHarSummaryPassthrough {
		if v, has := m[k]; has {
			out[k] = v
		}
	}
	return out, nil
}
