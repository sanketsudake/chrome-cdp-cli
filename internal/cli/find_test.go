package cli

// Command-boundary tests for the `find` verb (RFC-0015): validation before
// connecting, flag→FindOpts mapping, and the envelope contract — including
// that an empty result is ok:true, exit 0.

import (
	"context"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// findCapture records what the find verb asked the browser for.
type findCapture struct {
	fakeBrowser
	query string
	opts  chrome.FindOpts
}

func (f *findCapture) Find(_ context.Context, _, query string, opts chrome.FindOpts) (map[string]any, error) {
	f.query = query
	f.opts = opts
	return map[string]any{
		"query": query,
		"matches": []any{map[string]any{
			"ref": "e101", "role": "button", "name": "Sign in", "score": 0.91, "visible": true,
		}},
		"count": 1, "truncated": false,
	}, nil
}

func TestFindValidationBeforeConnect(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"empty query":        {"find", ""},
		"blank query":        {"find", "   "},
		"limit zero":         {"find", "save", "--limit", "0"},
		"limit negative":     {"find", "save", "--limit", "-3"},
		"limit above max":    {"find", "save", "--limit", "51"},
		"min-score negative": {"find", "save", "--min-score", "-0.1"},
		"min-score above 1":  {"find", "save", "--min-score", "1.5"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, _, code := run(t, noCall(t), append(args, "--json")...)
			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("error.code = %v, want usage", env["error"])
			}
		})
	}
}

func TestFindFlagsMapToOpts(t *testing.T) {
	t.Parallel()
	b := &findCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "t1", Title: "Fixture", URL: "https://example.com/"}}}}
	env, _, code := run(t, b,
		"find", "save button",
		"--role", "button", "--limit", "5", "--region", "Invoices",
		"--all", "--dedupe", "--min-score", "0.25", "--target", "t1", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, env = %v", code, env)
	}
	if b.query != "save button" {
		t.Errorf("query = %q", b.query)
	}
	want := chrome.FindOpts{Role: "button", Limit: 5, Region: "Invoices", All: true, Dedupe: true, MinScore: 0.25}
	if b.opts != want {
		t.Errorf("opts = %+v, want %+v", b.opts, want)
	}
	if env["command"] != "find" {
		t.Errorf("command = %v", env["command"])
	}
	res := env["result"].(map[string]any)
	if res["count"] != 1.0 {
		t.Errorf("count = %v", res["count"])
	}
	m := res["matches"].([]any)[0].(map[string]any)
	if m["ref"] != "e101" || m["role"] != "button" {
		t.Errorf("match = %v", m)
	}
}

func TestFindDefaultLimit(t *testing.T) {
	t.Parallel()
	b := &findCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "t1", Title: "Fixture", URL: "https://example.com/"}}}}
	if _, _, code := run(t, b, "find", "save", "--target", "t1", "--json"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if b.opts.Limit != chrome.DefaultFindLimit {
		t.Errorf("default limit = %d, want %d", b.opts.Limit, chrome.DefaultFindLimit)
	}
}

// VS-9: nothing found is an answer, not an error.
func TestFindEmptyResultIsOK(t *testing.T) {
	t.Parallel()
	b := &fakeBrowser{tabs: []target.Info{{ID: "t1", Title: "Fixture", URL: "https://example.com/"}}}
	env, _, code := run(t, b, "find", "flux capacitor", "--target", "t1", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if env["ok"] != true {
		t.Errorf("ok = %v", env["ok"])
	}
	res := env["result"].(map[string]any)
	if res["count"] != 0.0 {
		t.Errorf("count = %v, want 0", res["count"])
	}
}
