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

// stubBrowser implements chrome.Browser with permissive defaults so tests
// override only what they assert on; a new interface method gets a default here.
type stubBrowser struct{}

func (stubBrowser) List(context.Context) ([]target.Info, error) { return nil, nil }
func (stubBrowser) Navigate(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"url": "https://example.com/", "status": 200}, nil
}
func (stubBrowser) Eval(context.Context, string, string) (any, error) {
	return map[string]any{"value": 2}, nil
}
func (stubBrowser) Snapshot(context.Context, string) (any, error) { return map[string]any{}, nil }
func (stubBrowser) Click(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"clicked": true}, nil
}
func (stubBrowser) Type(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"typed": true}, nil
}
func (stubBrowser) HTML(context.Context, string, string, bool, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"html": "<div></div>"}, nil
}
func (stubBrowser) Text(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"text": "hello"}, nil
}
func (stubBrowser) Value(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"value": "v"}, nil
}
func (stubBrowser) Screenshot(context.Context, string) ([]byte, error) { return []byte("PNGDATA"), nil }
func (stubBrowser) PDF(context.Context, string) ([]byte, error)        { return []byte("%PDF-"), nil }
func (stubBrowser) CookieList(context.Context, string) (any, error) {
	return map[string]any{"cookies": []any{}}, nil
}
func (stubBrowser) CookieSet(context.Context, string, string, string, string, string) (map[string]any, error) {
	return map[string]any{"set": "x"}, nil
}
func (stubBrowser) CookieDelete(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"deleted": "x"}, nil
}
func (stubBrowser) CookieClear(context.Context, string) (map[string]any, error) {
	return map[string]any{"cleared": true}, nil
}
func (stubBrowser) AttrGet(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"name": "n", "value": "v", "present": true}, nil
}
func (stubBrowser) AttrList(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"attributes": map[string]any{}}, nil
}
func (stubBrowser) AttrSet(context.Context, string, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"set": "n"}, nil
}
func (stubBrowser) AttrRemove(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"removed": "n"}, nil
}
func (stubBrowser) SetHeaders(context.Context, string, map[string]string) (map[string]any, error) {
	return map[string]any{"headers": 1}, nil
}
func (stubBrowser) EmulateViewport(context.Context, string, int64, int64) (map[string]any, error) {
	return map[string]any{"width": 100, "height": 100}, nil
}
func (stubBrowser) EmulateGeo(context.Context, string, float64, float64) (map[string]any, error) {
	return map[string]any{"lat": 1.0, "lon": 2.0}, nil
}
func (stubBrowser) EmulateReset(context.Context, string) (map[string]any, error) {
	return map[string]any{"reset": true}, nil
}
func (stubBrowser) Raw(context.Context, string, string, json.RawMessage) (any, error) {
	return map[string]any{}, nil
}
func (stubBrowser) Close() error { return nil }

// fakeBrowser adds a tab list on top of the stub defaults.
type fakeBrowser struct {
	stubBrowser
	tabs []target.Info
}

func (f *fakeBrowser) List(context.Context) ([]target.Info, error) { return f.tabs, nil }

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
	var out, errb bytes.Buffer
	app := New(&fakeBrowser{}, &out, &errb)
	code := app.Execute("exit-codes")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	// The output must list every exit code in the contract (0..6).
	for _, n := range []string{"0", "1", "2", "3", "4", "5", "6"} {
		if !strings.Contains(out.String(), n+"  ") {
			t.Errorf("exit-codes output is missing code %s:\n%s", n, out.String())
		}
	}
}
