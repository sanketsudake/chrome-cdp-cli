package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// fakeBrowser implements chrome.Browser so the command boundary is tested
// in-process, with no real Chrome.
type fakeBrowser struct {
	tabs []target.Info
}

func (f *fakeBrowser) List(context.Context) ([]target.Info, error) { return f.tabs, nil }
func (f *fakeBrowser) Navigate(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"url": "https://example.com/", "status": 200}, nil
}
func (f *fakeBrowser) Eval(context.Context, string, string) (any, error) {
	return map[string]any{"value": 2}, nil
}
func (f *fakeBrowser) Snapshot(context.Context, string) (any, error) { return map[string]any{}, nil }
func (f *fakeBrowser) Click(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"clicked": true}, nil
}
func (f *fakeBrowser) Type(context.Context, string, string, string) (map[string]any, error) {
	return map[string]any{"typed": true}, nil
}
func (f *fakeBrowser) Screenshot(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"path": "./shot.png"}, nil
}
func (f *fakeBrowser) Raw(context.Context, string, string, json.RawMessage) (any, error) {
	return map[string]any{}, nil
}
func (f *fakeBrowser) Close() error { return nil }

var _ chrome.Browser = (*fakeBrowser)(nil)

func run(t *testing.T, b chrome.Browser, args ...string) (env map[string]any, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	code = app.Execute(args...)
	if s := strings.TrimSpace(out.String()); strings.HasPrefix(s, "{") {
		if err := json.Unmarshal([]byte(s), &env); err != nil {
			t.Fatalf("stdout is not one JSON value: %v\n%s", err, s)
		}
	}
	return env, errb.String(), code
}

func TestListJSONEnvelope(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{
		{ID: "aa11", Title: "GitHub", URL: "https://github.com/"},
		{ID: "bb22", Title: "Inbox", URL: "https://mail.google.com/"},
	}}
	env, _, code := run(t, b, "list", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if env["ok"] != true || env["command"] != "list" {
		t.Fatalf("envelope = %v", env)
	}
	tabs := env["result"].(map[string]any)["tabs"].([]any)
	if len(tabs) != 2 {
		t.Errorf("tabs = %d, want 2", len(tabs))
	}
}

func TestEvalWithoutTargetIsTargetError(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "X", URL: "u"}, {ID: "bb22", Title: "Y", URL: "v"}}}
	env, _, code := run(t, b, "eval", "1+1", "--json")
	if code != 4 {
		t.Errorf("exit = %d, want 4 (target)", code)
	}
	if env["ok"] != false {
		t.Fatalf("ok = %v, want false", env["ok"])
	}
	if env["error"].(map[string]any)["code"] != "no_current_target" {
		t.Errorf("error.code = %v, want no_current_target", env["error"])
	}
}

func TestRawBadParamsIsUsageError(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "X", URL: "u"}}}
	env, _, code := run(t, b, "raw", "Foo.bar", "not-json", "--target", "aa11", "--json")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
	if env["error"].(map[string]any)["code"] != "usage" {
		t.Errorf("error.code = %v, want usage", env["error"])
	}
}

func TestEvalResolvedTargetSucceeds(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "X", URL: "u"}, {ID: "bb22", Title: "Y", URL: "v"}}}
	env, _, code := run(t, b, "eval", "1+1", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if env["target"].(map[string]any)["id"] != "aa11" {
		t.Errorf("echoed target = %v, want aa11", env["target"])
	}
}

func TestExitCodesCommand(t *testing.T) {
	_, _, code := run(t, &fakeBrowser{}, "exit-codes")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}
