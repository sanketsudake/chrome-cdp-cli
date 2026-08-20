// Package cli builds chrome-cdp's cobra command tree and translates every
// command into the uniform result envelope + exit-code contract.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// App carries per-invocation state: the Browser port, output streams, parsed
// global flags, and the resulting exit code.
type App struct {
	browser chrome.Browser
	out     io.Writer
	err     io.Writer
	in      io.Reader // stdin for `session` NDJSON commands (defaults to os.Stdin)

	// global flags
	jsonOut    bool
	targetFlag string
	timeout    time.Duration
	// consentTimeout is how long the connection holder waits out Chrome's
	// browser-modal consent prompt. It is NOT --timeout: a command deadline
	// bounds work, this one bounds a human finding a dialog.
	consentTimeout time.Duration
	noLaunch       bool
	profileDir     string
	port           int
	// endpoint is an explicit --endpoint URL (ws:// or http://); wins over
	// port and the DevToolsActivePort file. See browser.FindEndpoint.
	endpoint string
	// session namespaces the sticky current tab (--session /
	// CHROME_CDP_SESSION / TOML session), so several agents can share one
	// Chrome without stealing each other's current tab. It does NOT
	// namespace the daemon connection: see ConnOpts.Session.
	session     string
	byFlag      string
	waitFlag    string
	roleFlag    string
	nthFlag     int
	matchFlag   string
	inRowFlag   string // --in-row: scope a --by name match to the row whose text contains this
	onDialog    string // --on-dialog: auto-handle a native dialog opened during an action (accept|dismiss)
	noWait      bool
	actWaitText string // --wait-text: after an action verb succeeds, wait until this text appears
	pierce      bool
	noDaemon    bool
	quiet       bool
	verbose     bool
	noColor     bool
	noInput     bool
	allowFlag   []string // --allow: one-off origin allow-list, replacing the configured one
	policyOff   bool     // --policy-off: explicit, logged, never implicit

	// verbPath is the running command's full cobra path minus the root
	// ("click", "cookie set"), captured per Execute in PersistentPreRun. It is
	// the key the policy classification table is written in.
	verbPath string
	// policyOffNoted keeps one command's bypass to one warning and one audit
	// record, even when it is checked more than once (nav checks both its
	// destination and the tab it starts on). Reset per Execute alongside
	// verbPath.
	policyOffNoted bool

	// mcpLock is set for the life of an MCP server and freezes the flags its
	// boundary is made of. Nil everywhere else, so a shell invocation is
	// unaffected. See internal/cli/mcp.go.
	mcpLock *mcpLock

	// policy test seams: the interactive check and the question itself. Real
	// runs leave both nil, so an unconfigured CLI carries no policy state at
	// all; a test sets them to drive the on_violation = "prompt" path without a
	// terminal.
	policyTTY func() bool
	policyAsk func(question string) bool

	// effective flag defaults (built-in unless main injects config+env via
	// WithDefaults); read once when the flags are registered.
	defaults config.Defaults

	// inSession is set while `session` is draining stdin and re-entering the
	// command tree per line. It is NOT a flag: it is how a streaming verb
	// (console --follow, and RFC-0003's net --follow) knows it would break
	// session's one-envelope-per-line contract and must fail as usage instead.
	inSession bool

	// inRecipe is set while `recipe run` is executing a plan's steps through
	// the command tree. Like inSession it is not a flag; it is how a step that
	// re-enters `recipe run` is refused. The load-time reserved-verb check can
	// only see one file, so recursion through the exec path — which cost 8 GB
	// of RSS in 8 seconds before this existed — has to be caught by the runner.
	inRecipe bool

	// injected sticky-target store, keyed lazily by the connection so distinct
	// endpoints (--port) don't share a current target (nil in tests).
	stickyGet func(ConnOpts) string
	stickySet func(ConnOpts, string) error

	// lazy Browser connector (nil in tests, where browser is injected directly)
	connect func(ctx context.Context, o ConnOpts) (chrome.Browser, error)

	// daemon control (nil in tests / direct-connect mode); each takes the
	// connection options so it addresses the right per-endpoint daemon.
	daemonStart  func(ConnOpts) (map[string]any, error)
	daemonStop   func(ConnOpts) (map[string]any, error)
	daemonStatus func(ConnOpts) (map[string]any, error)

	start    time.Time
	exitCode int
}

// New builds an App around a Browser and output streams. The --timeout flag's
// default is the single source of truth for the timeout (see newRoot).
func New(b chrome.Browser, out, errw io.Writer) *App {
	return &App{browser: b, out: out, err: errw, in: os.Stdin, defaults: config.Builtin()}
}

// WithInput overrides the stdin reader used by `session` (used by tests).
func (a *App) WithInput(r io.Reader) *App {
	a.in = r
	return a
}

// WithDefaults overrides the built-in flag defaults with values resolved from
// the config file + environment (used by main()); tests keep the built-ins.
func (a *App) WithDefaults(d config.Defaults) *App {
	a.defaults = d
	return a
}

// WithStickyTarget wires the persisted current-target store (used by main()).
// get/set take the connection options so the store is keyed per endpoint.
func (a *App) WithStickyTarget(get func(ConnOpts) string, set func(ConnOpts, string) error) *App {
	a.stickyGet, a.stickySet = get, set
	return a
}

// ConnOpts are the connection-related flags handed to the connector.
type ConnOpts struct {
	NoLaunch bool
	// Endpoint is an explicit --endpoint URL (ws:// or http://); wins over
	// Port and the DevToolsActivePort file. See browser.FindEndpoint.
	Endpoint   string
	ProfileDir string
	Port       int
	NoDaemon   bool
	// Session namespaces the sticky current tab (--session /
	// CHROME_CDP_SESSION), so the caller building a state.Store key can
	// append it to the endpoint key. It does NOT key the daemon socket:
	// sessions on the same endpoint share one connection and its
	// console/net event buffers by design (see cmd/chrome-cdp/main.go's
	// socketFor, which reads Endpoint/Port only).
	Session string
	// ConsentTimeout travels with the connection options because it is the
	// daemon that does the waiting, and the daemon is spawned from these. It is
	// always normalised — see connOpts.
	ConsentTimeout time.Duration
}

func (a *App) connOpts() ConnOpts {
	return ConnOpts{
		NoLaunch: a.noLaunch, ProfileDir: a.profileDir, Port: a.port, Endpoint: a.endpoint,
		NoDaemon: a.noDaemon, Session: a.session,
		// The flag is the last of the three ways this value gets set (config
		// resolution clamps the file and the environment), so it is clamped
		// here — with the same function, so no layer can read the number
		// differently. An explicit `--consent-timeout 0s` otherwise reached
		// daemon.Ensure as a literal zero while the daemon it spawned resolved
		// the same key to 120s, and the client reported a failure that had not
		// happened.
		ConsentTimeout: chrome.ClampConsentTimeout(a.consentTimeout),
	}
}

// freezeConnDefaults folds the connection-shaped flags this invocation was
// actually given into a.defaults, so re-entrant Execute calls (session lines,
// recipe steps, MCP tool calls) re-parse against them instead of silently
// resetting to the config defaults. It is the ONE list of what
// "connection-shaped" means; session, recipe run and mcp all call it, so the
// next such flag is one edit here instead of three that can drift (Timeout
// and ConsentTimeout each used to be frozen at some sites and not others).
//
// Timeout and ConsentTimeout freeze only when positive: zero means the flags
// were never parsed (a caller that built the runner directly), and the
// built-in default should stand rather than become an instantly-expired
// deadline on every call — an explicit `--consent-timeout 0s` is normalised
// by ClampConsentTimeout in connOpts either way.
//
// Selector semantics (--by, --role, …) are deliberately NOT frozen: they
// belong in each line's own argv, where a reader of the batch can see them.
// Re-entrant output is one JSON envelope per line/step/call, so JSON is
// always frozen on.
//
// The caller owns the defaults' lifetime: session and recipe run BORROW them
// (defer the returned restore), mcp keeps the freeze for the server's life.
func (a *App) freezeConnDefaults() (restore func()) {
	saved := a.defaults
	if a.timeout > 0 {
		a.defaults.Timeout = a.timeout
	}
	if a.consentTimeout > 0 {
		a.defaults.ConsentTimeout = a.consentTimeout
	}
	a.defaults.Endpoint = a.endpoint
	a.defaults.Port = a.port
	a.defaults.ProfileDir = a.profileDir
	a.defaults.NoLaunch = a.noLaunch
	a.defaults.NoDaemon = a.noDaemon
	a.defaults.Session = a.session
	a.defaults.JSON = true
	return func() { a.defaults = saved }
}

// WithConnector wires a lazy Browser connector (used by main()); it is invoked
// only when a command actually needs Chrome.
func (a *App) WithConnector(fn func(ctx context.Context, o ConnOpts) (chrome.Browser, error)) *App {
	a.connect = fn
	return a
}

// WithDaemonCtl wires the daemon start/stop/status operations (used by main()).
func (a *App) WithDaemonCtl(start, stop, status func(ConnOpts) (map[string]any, error)) *App {
	a.daemonStart, a.daemonStop, a.daemonStatus = start, stop, status
	return a
}

// getBrowser returns the injected Browser, or lazily connects one and caches it.
func (a *App) getBrowser(ctx context.Context) (chrome.Browser, *result.Err) {
	if a.browser != nil {
		return a.browser, nil
	}
	if a.connect == nil {
		return nil, &result.Err{Code: "connection_failed", Message: "no browser configured"}
	}
	b, err := a.connect(ctx, a.connOpts())
	if err != nil {
		code := "connection_failed"
		var ce *chrome.ConnectError
		if errors.As(err, &ce) {
			code = ce.Code
		}
		return nil, &result.Err{Code: code, Message: err.Error()}
	}
	a.browser = b
	return b, nil
}

// Execute runs the command tree for args and returns the process exit code.
func (a *App) Execute(args ...string) int {
	a.exitCode = result.ExitOK
	a.start = time.Now()
	root := a.newRoot()
	root.SetArgs(args)
	root.SetOut(a.err)
	root.SetErr(a.err)
	root.SilenceUsage = true
	root.SilenceErrors = true
	if err := root.Execute(); err != nil && a.exitCode == result.ExitOK {
		// cobra parse/usage failure that no command handled.
		a.emitErr("", "usage", err.Error(), nil)
	}
	return a.exitCode
}

func (a *App) ctx() (context.Context, context.CancelFunc) {
	t := a.timeout
	if a.noWait && t > time.Second {
		// --no-wait: act immediately, failing fast rather than waiting out --timeout.
		t = time.Second
	}
	return context.WithTimeout(context.Background(), t)
}

// queryOpts builds the selector-syntax / wait / pierce options from the flags.
func (a *App) queryOpts() chrome.QueryOpts {
	return chrome.QueryOpts{By: a.byFlag, Wait: a.waitFlag, Pierce: a.pierce, Role: a.roleFlag, Nth: a.nthFlag, Match: a.matchFlag, InRow: a.inRowFlag, OnDialog: a.onDialog}
}

// Close tears down a Browser that this App lazily connected (no-op for an
// injected Browser, e.g. in tests).
func (a *App) Close() {
	if a.connect != nil && a.browser != nil {
		_ = a.browser.Close()
	}
}

func (a *App) sticky() string {
	if a.stickyGet != nil {
		return a.stickyGet(a.connOpts())
	}
	return ""
}

// resolveTarget maps --target (or the sticky current target) to a concrete tab
// and returns the connected Browser to act on it with.
func (a *App) resolveTarget(ctx context.Context) (*result.TargetInfo, chrome.Browser, *result.Err) {
	// A policy the CLI could not read refuses before anything else: it cannot
	// have permitted this command, so there is nothing worth connecting for.
	if perr := a.checkPolicyConfig(a.policyVerb()); perr != nil {
		return nil, nil, perr
	}
	// Precedence: explicit --target > sticky current target (set by `use`) >
	// a persisted config/env default target.
	spec := a.targetFlag
	if spec == "" {
		spec = a.sticky()
	}
	if spec == "" {
		spec = a.defaults.Target
	}
	// Validate a target was given BEFORE connecting, so a forgotten --target
	// never launches or touches Chrome.
	if spec == "" {
		return nil, nil, &result.Err{Code: "no_current_target", Message: target.NoCurrentTargetMsg}
	}
	b, berr := a.getBrowser(ctx)
	if berr != nil {
		return nil, nil, berr
	}
	tabs, err := b.List(ctx)
	if err != nil {
		return nil, nil, &result.Err{Code: "connection_failed", Message: err.Error()}
	}
	info, rerr := target.Resolve(spec, tabs)
	if rerr != nil {
		return nil, nil, &result.Err{Code: rerr.Code, Message: rerr.Message}
	}
	// The policy check goes HERE — after the target is known (we need its
	// origin) and before any caller can act on it. Every verb that touches a
	// tab comes through this function, so a new one cannot bypass the boundary
	// by forgetting to call the hook; a redirect is caught too, because the
	// origin checked is the tab's SETTLED url at the moment the command runs.
	if perr := a.checkPolicy(a.policyVerb(), info.URL); perr != nil {
		return nil, nil, perr
	}
	return &result.TargetInfo{ID: info.ID, Title: info.Title, URL: info.URL}, b, nil
}

func (a *App) emit(env result.Envelope) {
	// One redaction point for every envelope, so an Exempt verb that resolves a
	// target (use, activate, close, policy init) cannot hand back the full URL
	// and title of a tab the policy does not cover just by not thinking about it.
	env.Target = a.redactTarget(env.Target)
	env.ElapsedMs = time.Since(a.start).Milliseconds()
	a.exitCode = env.ExitCode()
	if a.jsonOut {
		b, _ := env.JSON()
		fmt.Fprintln(a.out, string(b))
		return
	}
	a.renderHuman(env)
}

func (a *App) emitOK(command string, tgt *result.TargetInfo, res any) {
	if res == nil {
		// Success always carries a result field (the envelope contract), even
		// when a command's payload is a JSON null.
		res = map[string]any{}
	}
	a.emit(result.Envelope{OK: true, Command: command, Target: tgt, Result: res})
}

func (a *App) emitErr(command, code, msg string, details map[string]any) {
	a.emit(result.Envelope{
		OK:      false,
		Command: command,
		Error:   &result.Err{Code: code, Message: msg, Details: details},
	})
}

// colorless reports whether plain (symbol-free) output is requested.
func (a *App) colorless() bool {
	return a.noColor || os.Getenv("NO_COLOR") != ""
}

// renderHuman writes a brief human-readable rendering (result to stdout,
// errors to stderr), per the clig.dev contract.
func (a *App) renderHuman(env result.Envelope) {
	okMark, errMark := "✓", "✗"
	if a.colorless() {
		okMark, errMark = "OK:", "ERR:"
	}
	if !env.OK {
		if !a.quiet {
			fmt.Fprintf(a.err, "%s %s\n", errMark, env.Error.Message)
		}
		return
	}
	switch res := env.Result.(type) {
	case map[string]any:
		if tabs, ok := res["tabs"].([]tabRow); ok {
			for i, tr := range tabs {
				fmt.Fprintf(a.out, "@%d  %-10s  %s  %s\n", i+1, short(tr.ID), tr.Title, tr.URL)
			}
			return
		}
		fmt.Fprintf(a.out, "%s %s\n", okMark, oneLine(res))
	default:
		fmt.Fprintf(a.out, "%s %v\n", okMark, env.Result)
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// oneLine renders a result map as a single human-mode line, using the first
// present key in priority order.
func oneLine(m map[string]any) string {
	for _, k := range []string{"url", "value", "text", "html", "path", "clicked", "typed"} {
		if v, ok := m[k]; ok {
			return fmt.Sprintf("%s: %v", k, v)
		}
	}
	return fmt.Sprintf("%v", m)
}
