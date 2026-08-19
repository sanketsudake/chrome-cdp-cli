package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// dialogBrowser is a stub-backed double that records DialogHandle's arguments
// and lets a test script DialogStatus's answers across successive calls, the
// way recordBrowser scripts a recording's lifecycle.
type dialogBrowser struct {
	fakeBrowser
	// statusResults is popped one per DialogStatus call; the last entry repeats
	// once exhausted.
	statusResults []map[string]any
	statusCalls   int

	handleErr error
	// handleResult, when handleErr is nil, is returned from DialogHandle.
	handleResult map[string]any
	// lastAccept/lastText record what DialogHandle was actually called with.
	lastAccept bool
	lastText   string
	handled    int
}

func newDialogBrowser(t *testing.T) *dialogBrowser {
	t.Helper()
	return &dialogBrowser{
		fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "https://example.com/"}}},
	}
}

func (b *dialogBrowser) DialogStatus(context.Context, string) (map[string]any, error) {
	i := b.statusCalls
	if i >= len(b.statusResults) {
		i = len(b.statusResults) - 1
	}
	b.statusCalls++
	if i < 0 {
		return map[string]any{"open": false}, nil
	}
	return b.statusResults[i], nil
}

func (b *dialogBrowser) DialogHandle(_ context.Context, _ string, accept bool, text string) (map[string]any, error) {
	b.handled++
	b.lastAccept, b.lastText = accept, text
	if b.handleErr != nil {
		return nil, b.handleErr
	}
	if b.handleResult != nil {
		return b.handleResult, nil
	}
	action := "dismiss"
	if accept {
		action = "accept"
	}
	return map[string]any{"handled": true, "action": action, "type": "confirm", "message": "Sure?"}, nil
}

var _ chrome.Browser = (*dialogBrowser)(nil)

// TestDialogStatusEnvelope is RFC-0018 VS-10 (status half): the open map's six
// keys cross the envelope unchanged, and a closed answer carries no `type`.
func TestDialogStatusEnvelope(t *testing.T) {
	t.Parallel()
	b := newDialogBrowser(t)
	b.statusResults = []map[string]any{
		{
			"open": true, "type": "confirm", "message": "Delete 3 items?",
			"default_prompt": "", "frame_url": "https://example.com/items",
			"opened_at": "2026-08-19T10:15:00.412Z",
		},
		{"open": false},
	}

	env, _, code := run(t, b, "dialog", "status", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if env["command"] != "dialog" {
		t.Errorf("command = %v, want dialog", env["command"])
	}
	res := env["result"].(map[string]any)
	for _, k := range []string{"open", "type", "message", "default_prompt", "frame_url", "opened_at"} {
		if _, ok := res[k]; !ok {
			t.Errorf("result missing %q: %v", k, res)
		}
	}
	if res["open"] != true {
		t.Errorf("open = %v, want true", res["open"])
	}

	env2, _, code2 := run(t, b, "dialog", "status", "--target", "aa11", "--json")
	if code2 != 0 {
		t.Fatalf("second exit = %d, want 0", code2)
	}
	res2 := env2["result"].(map[string]any)
	if res2["open"] != false {
		t.Errorf("second open = %v, want false", res2["open"])
	}
	if _, ok := res2["type"]; ok {
		t.Errorf("closed result should carry no type: %v", res2)
	}
}

// TestDialogHandlePassesActionAndText is RFC-0018 VS-10 (handle half): accept
// with text and dismiss without both reach DialogHandle with the right args,
// and the envelope reports the resolved action.
func TestDialogHandlePassesActionAndText(t *testing.T) {
	t.Parallel()
	b := newDialogBrowser(t)

	env, _, code := run(t, b, "dialog", "accept", "bob", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("accept exit = %d, want 0", code)
	}
	if !b.lastAccept || b.lastText != "bob" {
		t.Errorf("DialogHandle saw (%v, %q), want (true, \"bob\")", b.lastAccept, b.lastText)
	}
	if got := env["result"].(map[string]any)["action"]; got != "accept" {
		t.Errorf("result.action = %v, want accept", got)
	}

	env2, _, code2 := run(t, b, "dialog", "dismiss", "--target", "aa11", "--json")
	if code2 != 0 {
		t.Fatalf("dismiss exit = %d, want 0", code2)
	}
	if b.lastAccept || b.lastText != "" {
		t.Errorf("DialogHandle saw (%v, %q), want (false, \"\")", b.lastAccept, b.lastText)
	}
	if got := env2["result"].(map[string]any)["action"]; got != "dismiss" {
		t.Errorf("result.action = %v, want dismiss", got)
	}
}

// TestDialogHandleNoDialogIsTargetNotFound is RFC-0018 VS-11: nothing retained
// is target_not_found/exit 4 with details.dialog == "none", both for the bare
// sentinel and for the wrapped form the daemon delivers as a flattened string
// (ErrNoDialog wrapped with dialogUnwatchedNote survives the RPC only by
// message match, per errIs).
func TestDialogHandleNoDialogIsTargetNotFound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"bare sentinel", chrome.ErrNoDialog},
		{"wrapped as the daemon would stringify it", fmt.Errorf("%s", fmt.Errorf("%w; nothing was listening to this tab before this command started", chrome.ErrNoDialog).Error())},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			b := newDialogBrowser(t)
			b.handleErr = c.err
			env, _, code := run(t, b, "dialog", "accept", "--target", "aa11", "--json")
			if code != 4 {
				t.Fatalf("exit = %d, want 4", code)
			}
			e := env["error"].(map[string]any)
			if e["code"] != "target_not_found" {
				t.Errorf("code = %v, want target_not_found", e["code"])
			}
			if e["dialog"] != "none" {
				t.Errorf("error.dialog = %v, want \"none\"", e["dialog"])
			}
			if e["message"] != c.err.Error() {
				t.Errorf("message = %v, want %q", e["message"], c.err.Error())
			}
		})
	}
}

// TestDialogArityNeverConnects is RFC-0018 VS-12: a wrong arg count is
// usage/exit 2 before the browser is ever touched (cobra's NoArgs /
// MaximumNArgs(1), enforced by noCall).
func TestDialogArityNeverConnects(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"dialog", "status", "x"},
		{"dialog", "dismiss", "x"},
		{"dialog", "accept", "a", "b"},
	}
	for _, args := range cases {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			t.Parallel()
			env, _, code := run(t, noCall(t), append(args, "--target", "aa11", "--json")...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 for %v: %v", code, args, env)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("code = %v, want usage", env["error"])
			}
		})
	}
}

// TestDialogInsideSessionIsOneEnvelopePerLine is RFC-0018 VS-13: dialog status
// and dialog accept each run as an ordinary argv line inside `session`,
// producing exactly one envelope apiece.
func TestDialogInsideSessionIsOneEnvelopePerLine(t *testing.T) {
	t.Parallel()
	b := newDialogBrowser(t)
	b.statusResults = []map[string]any{{"open": false}}
	in := strings.NewReader(
		`["dialog","status","--target","aa11"]` + "\n" +
			`["dialog","accept","--target","aa11"]` + "\n",
	)
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithInput(in)
	if code := app.Execute("session"); code != 0 {
		t.Fatalf("session exit = %d, want 0", code)
	}

	var envs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("output line is not JSON: %q (%v)", line, err)
		}
		envs = append(envs, e)
	}
	if len(envs) != 2 {
		t.Fatalf("got %d envelopes, want 2: %v", len(envs), envs)
	}
	if envs[0]["command"] != "dialog" || envs[0]["ok"] != true {
		t.Errorf("envelope 0 = %v, want ok dialog (status)", envs[0])
	}
	if envs[1]["command"] != "dialog" || envs[1]["ok"] != true {
		t.Errorf("envelope 1 = %v, want ok dialog (accept)", envs[1])
	}
}
