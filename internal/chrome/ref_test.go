package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// snap issues a stable element ref per node; `--by ref` acts on that exact
// element without re-resolving it by name.
func TestElementRefAddressing(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>Ref</title><body>
<button aria-label="Save" onclick="window.__saved=(window.__saved||0)+1">S</button>
<button aria-label="Cancel">C</button></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	snap, err := b.Snapshot(ctx, id)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	raw, _ := json.Marshal(snap)
	var s struct {
		Nodes []struct{ Ref, Role, Name string } `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	var saveRef string
	for _, n := range s.Nodes {
		if n.Role == "button" && n.Name == "Save" {
			saveRef = n.Ref
			break
		}
	}
	if saveRef == "" {
		t.Fatalf("no ref for the Save button in snapshot: %s", raw)
	}

	// Click by ref resolves the exact node snap reported.
	if _, err := b.Click(ctx, id, saveRef, QueryOpts{By: "ref"}); err != nil {
		t.Fatalf("Click --by ref %q: %v", saveRef, err)
	}
	got, err := b.Eval(ctx, id, "window.__saved")
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if v := got.(map[string]any)["value"]; fmt.Sprintf("%v", v) != "1" {
		t.Errorf("window.__saved = %v, want 1 (clicked the Save node by ref)", v)
	}

	// A malformed ref is a clear error, not a silent miss (bounded so a bad ref
	// fails fast instead of polling to the outer deadline).
	badCtx, badCancel := context.WithTimeout(ctx, 3*time.Second)
	defer badCancel()
	if _, err := b.Click(badCtx, id, "not-a-ref", QueryOpts{By: "ref"}); err == nil {
		t.Error("Click --by ref with a bad ref returned nil error, want an error")
	}
}
