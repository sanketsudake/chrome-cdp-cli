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
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// App carries per-invocation state: the Browser port, output streams, parsed
// global flags, and the resulting exit code.
type App struct {
	browser chrome.Browser
	out     io.Writer
	err     io.Writer

	// global flags
	jsonOut    bool
	targetFlag string
	timeout    time.Duration
	noLaunch   bool
	profileDir string
	port       int
	byFlag     string
	waitFlag   string
	noWait     bool
	quiet      bool
	verbose    bool
	noColor    bool
	noInput    bool

	// injected sticky-target source (nil in tests => no current target)
	currentTarget func() string
	setCurrent    func(string) error

	// lazy Browser connector (nil in tests, where browser is injected directly)
	connect func(ctx context.Context, noLaunch bool, profileDir string, port int) (chrome.Browser, error)

	start    time.Time
	exitCode int
}

// New builds an App around a Browser and output streams. The --timeout flag's
// default is the single source of truth for the timeout (see newRoot).
func New(b chrome.Browser, out, errw io.Writer) *App {
	return &App{browser: b, out: out, err: errw}
}

// WithStickyTarget wires the persisted current-target source (used by main()).
func (a *App) WithStickyTarget(get func() string, set func(string) error) *App {
	a.currentTarget, a.setCurrent = get, set
	return a
}

// WithConnector wires a lazy Browser connector (used by main()); it is invoked
// only when a command actually needs Chrome.
func (a *App) WithConnector(fn func(ctx context.Context, noLaunch bool, profileDir string, port int) (chrome.Browser, error)) *App {
	a.connect = fn
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
	b, err := a.connect(ctx, a.noLaunch, a.profileDir, a.port)
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

// queryOpts builds the selector-syntax / wait options from the global flags.
func (a *App) queryOpts() chrome.QueryOpts {
	return chrome.QueryOpts{By: a.byFlag, Wait: a.waitFlag}
}

// Close tears down a Browser that this App lazily connected (no-op for an
// injected Browser, e.g. in tests).
func (a *App) Close() {
	if a.connect != nil && a.browser != nil {
		_ = a.browser.Close()
	}
}

func (a *App) sticky() string {
	if a.currentTarget != nil {
		return a.currentTarget()
	}
	return ""
}

// resolveTarget maps --target (or the sticky current target) to a concrete tab
// and returns the connected Browser to act on it with.
func (a *App) resolveTarget(ctx context.Context) (*result.TargetInfo, chrome.Browser, *result.Err) {
	spec := a.targetFlag
	if spec == "" {
		spec = a.sticky()
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
