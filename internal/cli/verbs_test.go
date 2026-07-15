package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

func TestCookieCommands(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	cases := []struct {
		args    []string
		wantKey string
	}{
		{[]string{"cookie", "list", "--target", "aa11", "--json"}, "cookies"},
		{[]string{"cookie", "set", "sid", "abc", "--target", "aa11", "--json"}, "set"},
		{[]string{"cookie", "rm", "sid", "--target", "aa11", "--json"}, "deleted"},
		{[]string{"cookie", "clear", "--target", "aa11", "--json"}, "cleared"},
	}
	for _, c := range cases {
		env, _, code := run(t, b, c.args...)
		if code != 0 {
			t.Errorf("%v exit = %d, want 0", c.args, code)
			continue
		}
		if _, ok := env["result"].(map[string]any)[c.wantKey]; !ok {
			t.Errorf("%v: result missing key %q", c.args, c.wantKey)
		}
	}
}

func TestAttrHeadersEmulate(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	cases := []struct {
		args []string
		key  string
	}{
		{[]string{"attr", "get", "#x", "href", "--target", "aa11", "--json"}, "value"},
		{[]string{"attr", "list", "#x", "--target", "aa11", "--json"}, "attributes"},
		{[]string{"attr", "set", "#x", "data-y", "1", "--target", "aa11", "--json"}, "set"},
		{[]string{"attr", "rm", "#x", "data-y", "--target", "aa11", "--json"}, "removed"},
		{[]string{"headers", "set", "X-Foo=bar", "--target", "aa11", "--json"}, "headers"},
		{[]string{"emulate", "viewport", "1280", "800", "--target", "aa11", "--json"}, "width"},
		{[]string{"emulate", "geo", "12.9", "77.5", "--target", "aa11", "--json"}, "lat"},
		{[]string{"emulate", "reset", "--target", "aa11", "--json"}, "reset"},
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

func TestHeadersBadFormatIsUsage(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	env, _, code := run(t, b, "headers", "set", "noequals", "--target", "aa11", "--json")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
	if env["error"].(map[string]any)["code"] != "usage" {
		t.Errorf("error.code = %v, want usage", env["error"])
	}
}

func TestFrameList(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	env, _, code := run(t, b, "frame", "list", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, ok := env["result"].(map[string]any)["frames"]; !ok {
		t.Errorf("result missing frames: %v", env["result"])
	}
}

// wait --for is a fixed sleep needing no tab; it emits its own envelope.
func TestWaitForDuration(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	env, _, code := run(t, b, "wait", "--for", "10ms", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := env["result"].(map[string]any)["waited"]; got != "for:10ms" {
		t.Errorf("waited = %v, want for:10ms", got)
	}
}

// wait with no condition is a usage error.
func TestWaitNoConditionIsUsage(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	_, _, code := run(t, b, "wait", "--target", "aa11", "--json")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}

func TestPierceFlagThreads(t *testing.T) {
	b := &queryCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}}
	_, _, code := run(t, b, "click", "#x", "--target", "aa11", "--pierce", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !b.gotQ.Pierce {
		t.Error("--pierce did not thread to QueryOpts.Pierce")
	}
}

func TestPDFToFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.pdf")
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	env, _, code := run(t, b, "pdf", "-o", p, "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if env["result"].(map[string]any)["path"] != p {
		t.Errorf("result.path = %v, want %q", env["result"], p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("pdf not written: %v", err)
	}
}
