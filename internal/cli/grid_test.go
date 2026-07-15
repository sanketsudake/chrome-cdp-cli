package cli

import (
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

func TestGridAndScrollCommands(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	cases := []struct {
		args    []string
		wantKey string
	}{
		{[]string{"grid", "--target", "aa11", "--json"}, "headers"},
		{[]string{"grid", "table.week", "--target", "aa11", "--json"}, "headers"},
		{[]string{"scroll", "--dy", "400", "--target", "aa11", "--json"}, "scrolled"},
		{[]string{"scroll", "#list", "--to", "--target", "aa11", "--json"}, "scrolled"},
		{[]string{"scroll", ".grid", "--dy", "300", "--wheel", "--target", "aa11", "--json"}, "scrolled"},
	}
	for _, c := range cases {
		env, _, code := run(t, b, c.args...)
		if code != 0 {
			t.Errorf("%v exit = %d, want 0", c.args, code)
			continue
		}
		if _, ok := env["result"].(map[string]any)[c.wantKey]; !ok {
			t.Errorf("%v: result missing key %q (%v)", c.args, c.wantKey, env["result"])
		}
	}
}
