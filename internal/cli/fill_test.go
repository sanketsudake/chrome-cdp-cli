package cli

import (
	"context"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// fillCapture records Fill/Wait so the test asserts the fill command dispatches
// and that --wait-text triggers a follow-up wait.
type fillCapture struct {
	fakeBrowser
	filledSel, filledVal string
	waitedText           string
}

func (f *fillCapture) Fill(_ context.Context, _, sel, val string, _ chrome.QueryOpts) (map[string]any, error) {
	f.filledSel, f.filledVal = sel, val
	return map[string]any{"filled": sel, "value": val}, nil
}
func (f *fillCapture) Wait(_ context.Context, _ string, c chrome.WaitCond) (map[string]any, error) {
	f.waitedText = c.Text
	return map[string]any{"waited": "text:" + c.Text}, nil
}

func TestFillAndWaitTextCommand(t *testing.T) {
	t.Parallel()
	b := &fillCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}}
	env, _, code := run(t, b,
		"fill", "#h", "8", "--wait-text", "Saved", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.filledSel != "#h" || b.filledVal != "8" {
		t.Errorf("fill got sel=%q val=%q", b.filledSel, b.filledVal)
	}
	res := env["result"].(map[string]any)
	if res["filled"] != "#h" {
		t.Errorf("result.filled = %v", res["filled"])
	}
	// --wait-text ran a follow-up wait and annotated the envelope.
	if b.waitedText != "Saved" {
		t.Errorf("wait-text = %q, want Saved", b.waitedText)
	}
	if res["waited_text"] != "Saved" {
		t.Errorf("result.waited_text = %v, want Saved", res["waited_text"])
	}
}
