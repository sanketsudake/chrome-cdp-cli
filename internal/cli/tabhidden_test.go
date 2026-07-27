package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// hiddenTabBrowser times out a name-addressed action and reports a hidden tab.
type hiddenTabBrowser struct {
	fakeBrowser
	visibility string
}

func (h *hiddenTabBrowser) Pointer(context.Context, string, string, chrome.PointerOpts) (map[string]any, error) {
	return nil, errors.New("context deadline exceeded")
}
func (h *hiddenTabBrowser) Eval(_ context.Context, _, expr string) (any, error) {
	if expr == "document.visibilityState" {
		return map[string]any{"value": h.visibility}, nil
	}
	return map[string]any{"value": nil}, nil
}

func TestTabHiddenHintOnNameTimeout(t *testing.T) {
	t.Parallel()
	base := fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}

	// Hidden tab + --by name timeout -> tab_hidden detail + actionable message.
	h := &hiddenTabBrowser{fakeBrowser: base, visibility: "hidden"}
	env, _, code := run(t, h, "click", "Save", "--by", "name", "--target", "aa11", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (target/timeout)", code)
	}
	e := env["error"].(map[string]any)
	if e["tab_hidden"] != true {
		t.Errorf("expected error.tab_hidden=true, got error=%v", e)
	}

	// Same timeout but the tab is visible -> no tab_hidden hint.
	v := &hiddenTabBrowser{fakeBrowser: base, visibility: "visible"}
	env, _, _ = run(t, v, "click", "Save", "--by", "name", "--target", "aa11", "--json")
	e = env["error"].(map[string]any)
	if e["tab_hidden"] == true {
		t.Errorf("visible tab should not report tab_hidden: %v", e)
	}

	// A css-addressed timeout never probes visibility (querySelector isn't throttled).
	c := &hiddenTabBrowser{fakeBrowser: base, visibility: "hidden"}
	env, _, _ = run(t, c, "click", "#save", "--by", "css", "--target", "aa11", "--json")
	e = env["error"].(map[string]any)
	if e["tab_hidden"] == true {
		t.Errorf("css addressing should not report tab_hidden: %v", e)
	}
}
