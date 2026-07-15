package cli

import (
	"context"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// selectCapture records the args the select command passes through, so the test
// asserts on flag wiring (field addressing default, cascade separator, option
// match) without a real browser.
type selectCapture struct {
	fakeBrowser
	gotField  string
	gotOption string
	gotOpts   chrome.SelectOpts
}

func (s *selectCapture) Select(_ context.Context, _, field, option string, o chrome.SelectOpts) (map[string]any, error) {
	s.gotField, s.gotOption, s.gotOpts = field, option, o
	return map[string]any{"field": field, "selected": option, "widget": "prompt"}, nil
}

func TestSelectCommandWiring(t *testing.T) {
	b := &selectCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}}
	env, _, code := run(t, b,
		"select", "Time Type", "Project Plan Tasks > ShiftLeft: Qwiet",
		"--role", "textbox", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := env["result"].(map[string]any)["selected"]; got != "Project Plan Tasks > ShiftLeft: Qwiet" {
		t.Errorf("selected = %v", got)
	}
	if b.gotField != "Time Type" || b.gotOption != "Project Plan Tasks > ShiftLeft: Qwiet" {
		t.Errorf("field/option = %q / %q", b.gotField, b.gotOption)
	}
	// The field defaults to accessible-name addressing (select's whole point),
	// with --role carried through and the default cascade separator.
	if b.gotOpts.Query.By != "name" {
		t.Errorf("Query.By = %q, want name (default for select)", b.gotOpts.Query.By)
	}
	if b.gotOpts.Query.Role != "textbox" {
		t.Errorf("Query.Role = %q, want textbox", b.gotOpts.Query.Role)
	}
	if b.gotOpts.Sep != ">" {
		t.Errorf("Sep = %q, want >", b.gotOpts.Sep)
	}
}

func TestSelectCommandOverrides(t *testing.T) {
	b := &selectCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}}
	_, _, code := run(t, b,
		"select", "Region", "APAC/India",
		"--by", "css", "--option-match", "exact", "--sep", "/", "--filter", "Ind",
		"--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	// An explicit --by is honored (not overridden to name).
	if b.gotOpts.Query.By != "css" {
		t.Errorf("Query.By = %q, want css (explicit --by honored)", b.gotOpts.Query.By)
	}
	if b.gotOpts.OptionMatch != "exact" {
		t.Errorf("OptionMatch = %q, want exact", b.gotOpts.OptionMatch)
	}
	if b.gotOpts.Sep != "/" {
		t.Errorf("Sep = %q, want /", b.gotOpts.Sep)
	}
	if b.gotOpts.Filter != "Ind" {
		t.Errorf("Filter = %q, want Ind", b.gotOpts.Filter)
	}
}
