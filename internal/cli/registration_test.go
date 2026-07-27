package cli

import (
	"bytes"
	"testing"
)

// Cobra silently accepts the same command name twice — the later registration
// just shadows the earlier one. That makes a duplicated AddCommand entry
// invisible: the CLI still works, so no behavioural test catches it, and the
// shadowed constructor's flags quietly stop existing.
//
// This is a real hazard here because the RFC work merges many branches that each
// append to one AddCommand list, and a botched conflict resolution duplicates a
// block rather than removing one.
func TestNoDuplicateCommandRegistrations(t *testing.T) {
	t.Parallel()

	var out, errb bytes.Buffer
	app := New(&fakeBrowser{}, &out, &errb)
	seen := map[string]int{}
	for _, c := range app.newRoot().Commands() {
		seen[c.Name()]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("command %q is registered %d times in newRoot — remove the duplicate AddCommand entry", name, n)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no commands registered; the test is not exercising newRoot")
	}
}
