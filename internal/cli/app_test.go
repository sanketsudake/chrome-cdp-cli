package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrometest"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// stubBrowser is the shared permissive chrome.Browser double; tests override
// only the methods they assert on.
type stubBrowser = chrometest.StubBrowser

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	var out, errb bytes.Buffer
	app := New(&fakeBrowser{}, &out, &errb)
	code := app.Execute("exit-codes")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	// The output must list every exit code in the contract (0..7).
	for _, n := range []string{"0", "1", "2", "3", "4", "5", "6", "7"} {
		if !strings.Contains(out.String(), n+"  ") {
			t.Errorf("exit-codes output is missing code %s:\n%s", n, out.String())
		}
	}
}

// TestFreezeConnDefaults pins the ONE list of connection-shaped flags that
// survive into re-entrant Execute calls (session lines, recipe steps, MCP tool
// calls). Before the helper existed, session, recipe run and mcp each carried
// their own hand-maintained copy of this list and they drifted: ConsentTimeout
// was frozen only by session, Timeout only by mcp and recipe. A field missing
// here means one of the three silently resets that flag to the config default
// on every re-entrant line.
func TestFreezeConnDefaults(t *testing.T) {
	t.Parallel()
	app := New(&fakeBrowser{}, &bytes.Buffer{}, &bytes.Buffer{})
	app.endpoint = "ws://10.0.0.5:9222/devtools/browser/x"
	app.port = 9333
	app.profileDir = "/tmp/p"
	app.noLaunch = true
	app.noDaemon = true
	app.session = "agent-1"
	app.timeout = 5 * time.Second
	app.consentTimeout = 7 * time.Minute
	before := app.defaults

	restore := app.freezeConnDefaults()
	d := app.defaults
	if d.Endpoint != app.endpoint || d.Port != 9333 || d.ProfileDir != "/tmp/p" ||
		!d.NoLaunch || !d.NoDaemon || d.Session != "agent-1" ||
		d.Timeout != 5*time.Second || d.ConsentTimeout != 7*time.Minute || !d.JSON {
		t.Errorf("frozen defaults = %+v, want every connection-shaped flag folded in", d)
	}
	restore()
	if !reflect.DeepEqual(app.defaults, before) {
		t.Errorf("restore() left defaults = %+v, want %+v", app.defaults, before)
	}

	// Zero durations mean "flags never parsed": the built-in default stands
	// rather than becoming an instantly-expired deadline on every call.
	app.timeout, app.consentTimeout = 0, 0
	defer app.freezeConnDefaults()()
	if app.defaults.Timeout != before.Timeout || app.defaults.ConsentTimeout != before.ConsentTimeout {
		t.Errorf("zero durations must not clobber the built-in defaults: %+v", app.defaults)
	}
}
