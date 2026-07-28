package cli

import (
	"strings"
	"testing"
)

// An empty selector is rejected as usage, BEFORE Chrome is contacted.
//
// `type "" "8"` used to be a well-formed cobra invocation that travelled all
// the way to the browser and came back as `cdp_error: DOM Error while querying
// (-32000)` — a leaked protocol error naming neither the argument at fault nor
// the fix, and arriving as exit 5 rather than exit 2. Agents branch on the exit
// code, so "your call was wrong, don't retry" was being reported as "the
// protocol failed", which invites exactly the blind retry the contract exists
// to prevent.
//
// noCall fails the test if the CLI reaches for Chrome at all, so this asserts
// the ORDERING too, not merely the code.
func TestEmptySelectorIsUsageBeforeConnecting(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"type with empty selector":      {"type", "", "8"},
		"fill with empty selector":      {"fill", "", "8"},
		"type with whitespace selector": {"type", "   ", "8"},
		"fill with whitespace selector": {"fill", "\t", "8"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, _, code := run(t, noCall(t), append(args, "--target", "aa11", "--json")...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage)", code)
			}
			e, ok := env["error"].(map[string]any)
			if !ok {
				t.Fatalf("envelope has no error object: %#v", env)
			}
			if e["code"] != "usage" {
				t.Errorf("error.code = %v, want usage", e["code"])
			}
			// The message has to point at `key`, which is the verb that DOES
			// work without a selector — that confusion is why this is reachable.
			msg, _ := e["message"].(string)
			if msg == "" {
				t.Fatal("usage error carries no message")
			}
			if !strings.Contains(msg, "key") {
				t.Errorf("message %q does not name `key` as the selector-less verb", msg)
			}
		})
	}
}
