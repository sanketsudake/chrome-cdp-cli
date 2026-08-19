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
