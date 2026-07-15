package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// session runs each stdin line as a JSON argv over one held connection and emits
// one JSON envelope per line (NDJSON).
func TestSessionNDJSON(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	in := strings.NewReader(
		`["list"]` + "\n" +
			`# a comment line is skipped` + "\n" +
			"\n" + // blank line skipped
			`["snap","--target","aa11"]` + "\n" +
			`not-json` + "\n",
	)
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithInput(in)
	if code := app.Execute("session"); code != 0 {
		t.Fatalf("session exit = %d, want 0", code)
	}

	var envs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("output line is not JSON: %q (%v)", line, err)
		}
		envs = append(envs, e)
	}
	// list ok, snap ok, and the malformed line as a usage error = 3 envelopes.
	if len(envs) != 3 {
		t.Fatalf("got %d NDJSON envelopes, want 3: %v", len(envs), envs)
	}
	if envs[0]["command"] != "list" || envs[0]["ok"] != true {
		t.Errorf("envelope 0 = %v, want ok list", envs[0])
	}
	if envs[1]["command"] != "snap" || envs[1]["ok"] != true {
		t.Errorf("envelope 1 = %v, want ok snap", envs[1])
	}
	if envs[2]["ok"] != false {
		t.Errorf("envelope 2 = %v, want an error for the malformed line", envs[2])
	}
}
