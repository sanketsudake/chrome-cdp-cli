package cli

// Regression tests for code-review findings.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// rawCapture records the target id Raw was called with and returns a fixed value.
type rawCapture struct {
	fakeBrowser
	gotID string
	ret   any
}

func (r *rawCapture) Raw(_ context.Context, id, _ string, _ json.RawMessage) (any, error) {
	r.gotID = id
	return r.ret, nil
}

// `use @N` must persist the RESOLVED id, not the ephemeral spec.
func TestUsePersistsResolvedID(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{
		{ID: "aa11", Title: "A", URL: "u"}, {ID: "bb22", Title: "B", URL: "v"},
	}}
	var persisted string
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	app.WithStickyTarget(func() string { return "" }, func(s string) error { persisted = s; return nil })
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
