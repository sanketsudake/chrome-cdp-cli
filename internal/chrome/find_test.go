package chrome

// Pure tests for the `find` traversal core: filtering, region scoping, dedupe,
// limit/truncation, and ref minting over a synthetic accessibility tree — no
// browser. The scoring itself is pinned in find_score_test.go; here the tree
// pipeline around it is the unit.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/cdproto/accessibility"
)

// axV builds an accessibility value the way Chrome encodes them (raw JSON).
func axV(s string) *accessibility.Value {
	return &accessibility.Value{Value: []byte(`"` + s + `"`)}
}

func axProp(name, val string) *accessibility.Property {
	return &accessibility.Property{Name: accessibility.PropertyName(name), Value: axV(val)}
}

// findFixtureNodes is a small page: two duplicate Save buttons, a Save heading,
// a region container holding a Delete button, a page-level Delete button, and
// an ignored (hidden) Submit button.
func findFixtureNodes() []*accessibility.Node {
	return []*accessibility.Node{
		{NodeID: "1", Role: axV("RootWebArea"), Name: axV("Fixture"), ChildIDs: []accessibility.NodeID{"2", "3", "4", "5", "7", "8"}},
		{NodeID: "2", Role: axV("button"), Name: axV("Save"), BackendDOMNodeID: 101,
			Properties: []*accessibility.Property{axProp("focusable", "true")}},
		{NodeID: "3", Role: axV("button"), Name: axV("Save"), BackendDOMNodeID: 102,
			Properties: []*accessibility.Property{axProp("focusable", "true")}},
		{NodeID: "4", Role: axV("heading"), Name: axV("Save area"), BackendDOMNodeID: 103},
		{NodeID: "5", Role: axV("group"), Name: axV("Invoice 4102"), BackendDOMNodeID: 104, ChildIDs: []accessibility.NodeID{"6"}},
		{NodeID: "6", Role: axV("button"), Name: axV("Delete"), BackendDOMNodeID: 105,
			Properties: []*accessibility.Property{axProp("focusable", "true")}},
		{NodeID: "7", Role: axV("button"), Name: axV("Delete"), BackendDOMNodeID: 106,
			Properties: []*accessibility.Property{axProp("focusable", "true")}},
		{NodeID: "8", Role: axV("button"), Name: axV("Submit"), BackendDOMNodeID: 107, Ignored: true},
	}
}

func matchNames(ms []findMatchNode) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.name
	}
	return out
}

func TestBuildFindMatches(t *testing.T) {
	t.Parallel()

	t.Run("ranked with refs and states", func(t *testing.T) {
		t.Parallel()
		ms, truncated := buildFindMatches(findFixtureNodes(), "save button", FindOpts{})
		if truncated {
			t.Fatal("unexpected truncation")
		}
		if len(ms) < 3 {
			t.Fatalf("got %d matches (%v), want the two Save buttons and the heading", len(ms), matchNames(ms))
		}
		if ms[0].name != "Save" || ms[0].role != "button" || ms[0].ref != "e101" {
			t.Errorf("first match = %+v, want the first Save button with ref e101", ms[0])
		}
		if ms[1].ref != "e102" {
			t.Errorf("second match ref = %q, want e102 (document order among equals)", ms[1].ref)
		}
		if ms[0].score <= ms[2].score {
			t.Errorf("button score %.3f not above heading score %.3f", ms[0].score, ms[2].score)
		}
	})

	t.Run("dedupe collapses identical role+name", func(t *testing.T) {
		t.Parallel()
		ms, _ := buildFindMatches(findFixtureNodes(), "save button", FindOpts{Dedupe: true})
		saves := 0
		for _, m := range ms {
			if m.name == "Save" && m.role == "button" {
				saves++
			}
		}
		if saves != 1 {
			t.Errorf("got %d Save buttons after dedupe, want 1 (matches: %v)", saves, matchNames(ms))
		}
	})

	t.Run("region scopes to the container subtree", func(t *testing.T) {
		t.Parallel()
		ms, _ := buildFindMatches(findFixtureNodes(), "delete", FindOpts{Region: "Invoice 4102"})
		if len(ms) != 1 || ms[0].ref != "e105" {
			t.Fatalf("region matches = %v (refs %v), want exactly the in-region Delete e105", matchNames(ms), ms)
		}
	})

	t.Run("ignored nodes need --all", func(t *testing.T) {
		t.Parallel()
		ms, _ := buildFindMatches(findFixtureNodes(), "submit", FindOpts{})
		if len(ms) != 0 {
			t.Errorf("hidden Submit matched without --all: %v", matchNames(ms))
		}
		ms, _ = buildFindMatches(findFixtureNodes(), "submit", FindOpts{All: true})
		if len(ms) != 1 || !ms[0].ignored {
			t.Errorf("with --all got %v, want the ignored Submit flagged ignored", ms)
		}
	})

	t.Run("hard role filter excludes other roles", func(t *testing.T) {
		t.Parallel()
		ms, _ := buildFindMatches(findFixtureNodes(), "save", FindOpts{Role: "heading"})
		for _, m := range ms {
			if m.role != "heading" {
				t.Errorf("role filter leaked a %q (%q)", m.role, m.name)
			}
		}
		if len(ms) != 1 {
			t.Errorf("got %d headings, want 1", len(ms))
		}
	})

	t.Run("limit truncates and says so", func(t *testing.T) {
		t.Parallel()
		ms, truncated := buildFindMatches(findFixtureNodes(), "save", FindOpts{Limit: 1})
		if len(ms) != 1 || !truncated {
			t.Errorf("limit 1: got %d matches, truncated=%v; want 1, true", len(ms), truncated)
		}
	})

	t.Run("min-score drops weak matches without truncation", func(t *testing.T) {
		t.Parallel()
		ms, truncated := buildFindMatches(findFixtureNodes(), "save button", FindOpts{MinScore: 0.8})
		if truncated {
			t.Error("min-score filtering must not report truncation")
		}
		for _, m := range ms {
			if m.score < 0.8 {
				t.Errorf("match %q scored %.3f, below min-score", m.name, m.score)
			}
			if m.role != "button" {
				t.Errorf("weak match %q (%s) survived min-score", m.name, m.role)
			}
		}
		if len(ms) == 0 {
			t.Error("min-score 0.8 dropped everything; the Save buttons should clear it")
		}
	})

	t.Run("no matches is empty not nil-panic", func(t *testing.T) {
		t.Parallel()
		ms, truncated := buildFindMatches(findFixtureNodes(), "flux capacitor", FindOpts{})
		if len(ms) != 0 || truncated {
			t.Errorf("got %v truncated=%v, want none", matchNames(ms), truncated)
		}
	})
}

// Live-Chrome: find ranks real a11y nodes, refs interchange with snap and
// resolve via --by ref, centres honour the coordinate contract, and an empty
// result is an answer (RFC-0015 VS-1/2/6/7/8/9/11).
func TestFindLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Find</title><body>
<h1>Login</h1>
<button id="signin" style="position:fixed;left:100px;top:50px;width:200px;height:40px"
        onclick="window.__clicked='signin'">Sign in to your account</button>
<a href="/help">Login help</a>
<input placeholder="Search projects" id="q">
<div role="region" aria-label="Invoice 4102"><button aria-label="Delete" id="d1">D1</button></div>
<button aria-label="Delete" id="d2">D2</button>
</body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	find := func(query string, opts FindOpts) []map[string]any {
		t.Helper()
		got, err := b.Find(ctx, id, query, opts)
		if err != nil {
			t.Fatalf("Find(%q, %+v): %v", query, opts, err)
		}
		raw, _ := json.Marshal(got["matches"])
		var ms []map[string]any
		_ = json.Unmarshal(raw, &ms)
		return ms
	}

	// VS-1 + VS-11: the purpose query ranks the sign-in button first, with its
	// full accessible name, a ref, and a score.
	ms := find("login button", FindOpts{})
	if len(ms) == 0 {
		t.Fatal("find 'login button' matched nothing")
	}
	first := ms[0]
	if first["role"] != "button" || first["name"] != "Sign in to your account" {
		t.Fatalf("first match = %v, want the sign-in button", first)
	}
	ref, _ := first["ref"].(string)
	if ref == "" {
		t.Fatal("first match has no ref")
	}

	// VS-8: centre matches the fixed-position box (100..300 x 50..90 -> 200,70).
	center, _ := first["center"].(map[string]any)
	if center == nil {
		t.Fatal("first match has no center")
	}
	cx, cy := center["x"].(float64), center["y"].(float64)
	if cx < 198 || cx > 202 || cy < 68 || cy > 72 {
		t.Errorf("center = (%v,%v), want ~(200,70)", cx, cy)
	}

	// VS-7: the ref is a live address for an acting verb.
	if _, err := b.Pointer(ctx, id, ref, PointerOpts{Action: PointerClick, Query: QueryOpts{By: "ref"}}); err != nil {
		t.Fatalf("click --by ref %s: %v", ref, err)
	}
	clicked, err := b.Eval(ctx, id, "window.__clicked", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if v, _ := clicked.(map[string]any)["value"].(string); v != "signin" {
		t.Errorf("ref click landed on %q, want signin", v)
	}

	// Ref interchange: snap mints the same ref for the same node.
	snapGot, err := b.Snapshot(ctx, id, SnapOpts{Role: "button", Grep: "Sign in"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	raw, _ := json.Marshal(snapGot)
	var s struct {
		Nodes []struct{ Ref, Name string } `json:"nodes"`
	}
	_ = json.Unmarshal(raw, &s)
	if len(s.Nodes) != 1 || s.Nodes[0].Ref != ref {
		t.Errorf("snap ref = %+v, want the same %s find minted", s.Nodes, ref)
	}

	// VS-2: placeholder text is evidence.
	ms = find("search bar", FindOpts{})
	if len(ms) == 0 || ms[0]["role"] != "textbox" {
		t.Errorf("find 'search bar' = %v, want the placeholder textbox first", ms)
	}

	// VS-6: region scoping keeps only the in-region Delete.
	ms = find("delete", FindOpts{Region: "Invoice 4102", Role: "button"})
	if len(ms) != 1 {
		t.Errorf("region find = %v, want exactly the in-region Delete", ms)
	}

	// VS-9: nothing found is count 0, not an error.
	if ms = find("flux capacitor", FindOpts{}); len(ms) != 0 {
		t.Errorf("find 'flux capacitor' = %v, want none", ms)
	}
}
