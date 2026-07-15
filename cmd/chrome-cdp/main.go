// Command chrome-cdp drives the user's local Chrome over CDP.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/cli"
	"github.com/sanketsudake/chrome-cdp-cli/internal/state"
)

func main() {
	// Resolve the DevToolsActivePort path once and reuse it for both the state
	// key and the connection (so discovery doesn't run twice per invocation).
	portFile := browser.FindPortFile("")

	app := cli.New(nil, os.Stdout, os.Stderr)

	// Lazy state: only build the store when a command actually reads or writes
	// the sticky target, keyed by the endpoint so distinct Chromes don't share it.
	var st *state.Store
	store := func() *state.Store {
		if st == nil {
			st, _ = state.New(endpointKey(portFile))
		}
		return st
	}
	app.WithStickyTarget(
		func() string {
			if s := store(); s != nil {
				return s.CurrentTarget()
			}
			return ""
		},
		func(v string) error {
			s := store()
			if s == nil {
				return fmt.Errorf("sticky-target state store is unavailable")
			}
			return s.SetCurrentTarget(v)
		},
	)
	app.WithConnector(func(ctx context.Context, noLaunch bool, profileDir string) (chrome.Browser, error) {
		return chrome.Connect(ctx, chrome.Options{PortFile: portFile, NoLaunch: noLaunch, ProfileDir: profileDir})
	})

	// os.Exit skips deferred calls, so tear the browser down explicitly first
	// (closes a managed Chrome; a no-op for an attached real Chrome).
	code := app.Execute(os.Args[1:]...)
	app.Close()
	os.Exit(code)
}

// endpointKey identifies the debug endpoint (host:port) so the sticky current
// target is keyed to the actual Chrome instance, not a fixed port.
func endpointKey(portFile string) string {
	if portFile != "" {
		if ws, err := browser.WSURLFromPortFile(portFile); err == nil {
			if hp, ok := browser.HostPort(ws); ok {
				return hp
			}
		}
	}
	return "default"
}
