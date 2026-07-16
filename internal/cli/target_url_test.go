package cli

// Regression: the envelope's target must reflect the URL the tab settled at
// after a nav / redirect wait, not the pre-action URL.

import (
	"context"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// navWaitBrowser returns a settled URL from nav/wait so the CLI can echo the
// post-action URL in the envelope's target.
type navWaitBrowser struct {
	fakeBrowser
	settled string
}

func (n *navWaitBrowser) Navigate(_ context.Context, _, _ string) (map[string]any, error) {
	return map[string]any{"url": n.settled}, nil
}
func (n *navWaitBrowser) Wait(_ context.Context, _ string, _ chrome.WaitCond) (map[string]any, error) {
	return map[string]any{"waited": "url:after", "url": n.settled}, nil
}

func TestNavReportsSettledURL(t *testing.T) {
	t.Parallel()
	b := &navWaitBrowser{
		fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "https://before/"}}},
		settled:     "https://after/redirected",
	}
	env, _, code := run(t, b, "nav", "https://before/", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := env["target"].(map[string]any)["url"]; got != "https://after/redirected" {
		t.Errorf("target.url = %v, want the settled URL (not the pre-nav URL)", got)
	}
}

func TestWaitReportsSettledURL(t *testing.T) {
	t.Parallel()
	b := &navWaitBrowser{
		fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "https://before/"}}},
		settled:     "https://after/home",
	}
	env, _, code := run(t, b, "wait", "--url", "home", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := env["target"].(map[string]any)["url"]; got != "https://after/home" {
		t.Errorf("target.url = %v, want the settled URL", got)
	}
}
