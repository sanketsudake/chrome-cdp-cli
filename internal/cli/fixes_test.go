package cli

// Regression tests for code-review findings.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// queryCapture records the QueryOpts passed to Click.
type queryCapture struct {
	fakeBrowser
	gotQ chrome.QueryOpts
}

func (q *queryCapture) Click(_ context.Context, _, _ string, opts chrome.QueryOpts) (map[string]any, error) {
	q.gotQ = opts
	return map[string]any{"clicked": true}, nil
}

// --by and --wait thread through to the selector verb.
func TestByAndWaitFlagsThread(t *testing.T) {
	b := &queryCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}}
	_, _, code := run(t, b, "click", "#x", "--target", "aa11", "--by", "search", "--wait", "ready", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.gotQ.By != "search" || b.gotQ.Wait != "ready" {
		t.Errorf("query opts = %+v, want {By:search Wait:ready}", b.gotQ)
	}
}

// html/text/value produce their result key.
func TestExtractVerbs(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	cases := []struct {
		args []string
		key  string
	}{
		{[]string{"html", "--target", "aa11", "--json"}, "html"},
		{[]string{"text", "#x", "--target", "aa11", "--json"}, "text"},
		{[]string{"value", "#x", "--target", "aa11", "--json"}, "value"},
	}
	for _, c := range cases {
		env, _, code := run(t, b, c.args...)
		if code != 0 {
			t.Errorf("%v exit = %d, want 0", c.args, code)
			continue
		}
		if _, ok := env["result"].(map[string]any)[c.key]; !ok {
			t.Errorf("%v: result missing key %q", c.args, c.key)
		}
	}
}

// rawCapture records what Raw was called with and returns a fixed value.
type rawCapture struct {
	fakeBrowser
	gotID     string
	gotMethod string
	ret       any
}

func (r *rawCapture) Raw(_ context.Context, id, method string, _ json.RawMessage) (any, error) {
	r.gotID = id
	r.gotMethod = method
	return r.ret, nil
}

// raw --list enumerates the connected Chrome's protocol via Schema.getDomains.
func TestRawList(t *testing.T) {
	b := &rawCapture{ret: map[string]any{"domains": []any{map[string]any{"name": "Network"}}}}
	env, _, code := run(t, b, "raw", "--list", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.gotMethod != "Schema.getDomains" {
		t.Errorf("raw --list called %q, want Schema.getDomains", b.gotMethod)
	}
	if b.gotID != "" {
		t.Errorf("raw --list used target %q, want browser-level (empty)", b.gotID)
	}
	if env["ok"] != true {
		t.Errorf("ok = %v", env["ok"])
	}
}

// `use @N` must persist the RESOLVED id, not the ephemeral spec.
func TestUsePersistsResolvedID(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{
		{ID: "aa11", Title: "A", URL: "u"}, {ID: "bb22", Title: "B", URL: "v"},
	}}
	var persisted string
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	app.WithStickyTarget(func(ConnOpts) string { return "" }, func(_ ConnOpts, s string) error { persisted = s; return nil })
	if code := app.Execute("use", "@2", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if persisted != "bb22" {
		t.Errorf("persisted %q, want the resolved id bb22 (not the @2 spec)", persisted)
	}
}

// `use` must fail loudly (not claim success) when persistence is unavailable.
func TestUseWithoutStateErrors(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	env, _, code := run(t, b, "use", "aa11", "--json")
	if code != 1 {
		t.Errorf("exit = %d, want 1 (generic)", code)
	}
	if env["ok"] != false {
		t.Errorf("ok = %v, want false", env["ok"])
	}
}

// raw --browser routes to the browser-level executor (empty target id, no tab).
func TestRawBrowserLevel(t *testing.T) {
	b := &rawCapture{fakeBrowser: fakeBrowser{}, ret: map[string]any{"product": "Chrome"}}
	env, _, code := run(t, b, "raw", "--browser", "Browser.getVersion", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.gotID != "" {
		t.Errorf("Raw called with id %q, want browser-level (empty)", b.gotID)
	}
	if _, has := env["target"]; has {
		t.Error("browser-level raw should not echo a target")
	}
}

// A success envelope always carries a result field, even for a nil payload.
func TestSuccessAlwaysHasResult(t *testing.T) {
	b := &rawCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}, ret: nil}
	env, _, code := run(t, b, "raw", "Foo.bar", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, has := env["result"]; !has {
		t.Error("success envelope must always contain a result field")
	}
}
