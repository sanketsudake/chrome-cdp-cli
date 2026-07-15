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
	noLaunch   bool
	profileDir string
	port       int
	byFlag     string
	waitFlag   string
	roleFlag   string
	nthFlag    int
	matchFlag  string
	noWait     bool
	actWaitText string // --wait-text: after an action verb succeeds, wait until this text appears
	pierce     bool
	noDaemon   bool
	quiet      bool
	verbose    bool
	noColor    bool
	noInput    bool

	// effective flag defaults (built-in unless main injects config+env via
	// WithDefaults); read once when the flags are registered.
	defaults config.Defaults

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
	NoLaunch   bool
	ProfileDir string
	Port       int
	NoDaemon   bool
}

func (a *App) connOpts() ConnOpts {
	return ConnOpts{NoLaunch: a.noLaunch, ProfileDir: a.profileDir, Port: a.port, NoDaemon: a.noDaemon}
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
	return chrome.QueryOpts{By: a.byFlag, Wait: a.waitFlag, Pierce: a.pierce, Role: a.roleFlag, Nth: a.nthFlag, Match: a.matchFlag}
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
	return &result.TargetInfo{ID: info.ID, Title: info.Title, URL: info.URL}, b, nil
}

func (a *App) emit(env result.Envelope) {
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
