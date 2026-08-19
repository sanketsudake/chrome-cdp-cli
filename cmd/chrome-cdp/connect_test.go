package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/cli"
	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
)

// TestDirectConnectAnnouncesThePendingPrompt is US-2 on the --no-daemon path.
//
// chrome.Options.OnConsentPending was assigned in exactly one place, inside the
// daemon, and the --no-daemon connect passed a ConsentTimeout with no hook. So
// that path waited out the prompt in complete silence — up to two minutes with
// a frozen browser and nothing on stderr — and no test caught it, because the
// tests that cover the hook supply their own.
func TestDirectConnectAnnouncesThePendingPrompt(t *testing.T) {
	var buf bytes.Buffer
	opts := directConnectOptions("", cli.ConnOpts{}, config.Builtin(), &buf)
	if opts.OnConsentPending == nil {
		t.Fatal("--no-daemon connects with no pending hook: the user is told nothing for the whole consent budget")
	}
	opts.OnConsentPending()
	if !strings.Contains(buf.String(), browser.ConsentPromptAdvice) {
		t.Errorf("the --no-daemon notice does not carry browser.ConsentPromptAdvice:\n%s", buf.String())
	}
}

// TestDirectConnectOptionsThreadsEndpoint: --endpoint has to reach
// chrome.Connect on the --no-daemon path the same way --port does, or an
// explicit endpoint would work through the daemon and silently stop working
// with --no-daemon.
func TestDirectConnectOptionsThreadsEndpoint(t *testing.T) {
	var buf bytes.Buffer
	o := cli.ConnOpts{Endpoint: "ws://127.0.0.1:9222/devtools/browser/abc"}
	opts := directConnectOptions("", o, config.Builtin(), &buf)
	if opts.Endpoint != o.Endpoint {
		t.Errorf("Options.Endpoint = %q, want %q", opts.Endpoint, o.Endpoint)
	}
}

// TestDirectConnectOptionsThreadsBrowserBin: CHROME_CDP_BROWSER_BIN has no
// --browser-bin flag, so it has to reach chrome.Options through defs
// (config.Defaults.BrowserBin) rather than cli.ConnOpts — the same path as
// the ConsoleBuffer-style capture bounds, not the same path as --endpoint or
// --port.
func TestDirectConnectOptionsThreadsBrowserBin(t *testing.T) {
	var buf bytes.Buffer
	defs := config.Builtin()
	defs.BrowserBin = "/opt/chromium/chrome"
	opts := directConnectOptions("", cli.ConnOpts{}, defs, &buf)
	if opts.BrowserBin != defs.BrowserBin {
		t.Errorf("Options.BrowserBin = %q, want %q", opts.BrowserBin, defs.BrowserBin)
	}
}

// TestSessionSuffix pins the exact shape stateFor builds its state.New key
// from: sessionSuffix("") == "" (a no-session invocation keys its sticky
// target exactly as before this flag existed) and sessionSuffix("a") == "/a"
// (the separator state.go's sanitize proof assumes).
func TestSessionSuffix(t *testing.T) {
	if got := sessionSuffix(""); got != "" {
		t.Errorf("sessionSuffix(\"\") = %q, want empty", got)
	}
	if got := sessionSuffix("a"); got != "/a" {
		t.Errorf("sessionSuffix(\"a\") = %q, want \"/a\"", got)
	}
}
