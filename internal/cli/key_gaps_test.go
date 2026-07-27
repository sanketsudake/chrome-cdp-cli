package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// timeoutKeyBrowser fails Key the way an unresolvable selector does.
type timeoutKeyBrowser struct {
	fakeBrowser
}

func (b *timeoutKeyBrowser) Key(context.Context, string, string, []chrome.KeyStroke, chrome.KeyOpts) (map[string]any, error) {
	return nil, errors.New(`selector "Nope" not found: context deadline exceeded`)
}

// RFC-0001 VS-9: a selector that never resolves is target_timeout / exit 4, not
// a usage error — the keyspec was fine, the page just didn't have the element.
// Keeping those distinct is what lets a caller tell "retry/wait" from "fix your
// call".
func TestKeyUnresolvableSelectorIsTargetTimeout(t *testing.T) {
	t.Parallel()
	b := &timeoutKeyBrowser{fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "X", URL: "u"}}}}
	env, _, code := run(t, b, "key", "Nope", "Enter", "--by", "name", "--target", "aa11", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (target/timeout)", code)
	}
	if got := env["error"].(map[string]any)["code"]; got != "target_timeout" {
		t.Errorf("error.code = %v, want target_timeout", got)
	}
}

// RFC-0001 VS-10: key is an ordinary argv verb, so it composes inside a session
// batch over one held connection. This is the property that matters for agent
// use — a verb that only worked standalone would silently break every batched
// interaction.
func TestKeyInsideSessionBatch(t *testing.T) {
	t.Parallel()
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	in := strings.NewReader(
		`["key","Escape","--target","aa11"]` + "\n" +
			`["snap","--target","aa11"]` + "\n",
	)
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithInput(in)
	if code := app.Execute("session"); code != 0 {
		t.Fatalf("session exit = %d, want 0", code)
	}

	var envs []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("session line is not one JSON envelope: %q (%v)", line, err)
		}
		envs = append(envs, e)
	}
	if len(envs) != 2 {
		t.Fatalf("got %d NDJSON envelopes, want 2: %v", len(envs), envs)
	}
	if envs[0]["command"] != "key" || envs[0]["ok"] != true {
		t.Errorf("first envelope = %v, want an ok key result", envs[0])
	}
	if envs[1]["command"] != "snap" || envs[1]["ok"] != true {
		t.Errorf("second envelope = %v, want an ok snap result", envs[1])
	}
}
