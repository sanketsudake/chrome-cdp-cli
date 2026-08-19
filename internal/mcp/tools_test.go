package mcp

import (
	"strings"
	"testing"
)

// RFC-0011 Open Question 4 and RFC-0004's tool table both exclude `record` from
// the exposed surface, and `recipe` likewise.
//
// The reasons differ and both are deliberate. A recording is a capture of the
// user's real, logged-in browser, and an agent that can silently start one is a
// surprising capability to hand out over a protocol. A recipe is meant to be
// authored and reviewed at a shell before it drives anything — exposing it would
// let an agent run a file the user never read.
//
// The registry is an explicit allow-list, so today they are excluded by simply
// not being listed. This test states the intent, so adding either becomes a
// deliberate act with a failing test to answer rather than an oversight.
func TestRecordAndRecipeAreNotExposed(t *testing.T) {
	t.Parallel()

	withheld := map[string]string{
		"record": "captures the user's real browser; not something an agent should start unprompted",
		"recipe": "a recipe is authored and reviewed at a shell before it drives anything",
	}
	for _, tl := range registry() {
		for _, v := range tl.verbs {
			root := v
			if i := strings.IndexByte(root, ' '); i >= 0 {
				root = root[:i]
			}
			if why, bad := withheld[root]; bad {
				t.Errorf("tool %q exposes the %q verb, which is deliberately withheld: %s", tl.name, v, why)
			}
		}
	}
}

// TestNetworkToolDoesNotExposeHar is RFC-0017's stated intent (Design notes,
// "Policy and MCP"): an MCP client is on the other side of a protocol from
// the server's disk, so a path in the response is unusable to it. `--har` has
// no inline form, unlike `screenshot`'s `output`, which is an ALSO beside the
// image returned inline — so the registry, an explicit allow-list, simply
// never gets a `har` argument added to it.
func TestNetworkToolDoesNotExposeHar(t *testing.T) {
	t.Parallel()
	for _, tl := range registry() {
		if tl.name != prefix+"network" {
			continue
		}
		for _, a := range tl.args {
			if a.name == "har" || a.flag == "har" {
				t.Fatalf("the network tool exposes `har`, which RFC-0017 deliberately withholds: an MCP client cannot use a path on the server's disk")
			}
		}
		return
	}
	t.Fatal("the network tool was not found in the registry")
}
