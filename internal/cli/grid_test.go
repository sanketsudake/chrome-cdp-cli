package cli

import (
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

func TestGridAndScrollCommands(t *testing.T) {
	t.Parallel()
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	cases := []struct {
		name    string
		args    []string
		wantKey string
	}{
		{"grid default", []string{"grid", "--target", "aa11", "--json"}, "headers"},
		{"grid with selector", []string{"grid", "table.week", "--target", "aa11", "--json"}, "headers"},
		{"scroll by dy", []string{"scroll", "--dy", "400", "--target", "aa11", "--json"}, "scrolled"},
		{"scroll to element", []string{"scroll", "#list", "--to", "--target", "aa11", "--json"}, "scrolled"},
		{"scroll wheel", []string{"scroll", ".grid", "--dy", "300", "--wheel", "--target", "aa11", "--json"}, "scrolled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env, _, code := run(t, b, c.args...)
			if code != 0 {
				t.Fatalf("%v exit = %d, want 0", c.args, code)
			}
			if _, ok := env["result"].(map[string]any)[c.wantKey]; !ok {
				t.Errorf("%v: result missing key %q (%v)", c.args, c.wantKey, env["result"])
			}
		})
	}
}
