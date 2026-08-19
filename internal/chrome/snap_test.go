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

// snap filters the tree server-side: --role, --grep (name regex), --region
// (subtree scope), and --dedupe (collapse identical role+name).
func TestSnapFiltering(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Snap</title><body>
<div role="region" aria-label="Calendar">
  <button aria-label="Event Standup">A</button>
  <button aria-label="Event Review">B</button>
</div>
<button aria-label="Save">S1</button>
<button aria-label="Save">S2</button>
<a href="/home">Home</a>
<input aria-label="Search" id="s"></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	names := func(o SnapOpts) []string {
		t.Helper()
		got, err := b.Snapshot(ctx, id, o)
		if err != nil {
			t.Fatalf("Snapshot %+v: %v", o, err)
		}
		raw, _ := json.Marshal(got)
		var s struct {
			Nodes []struct{ Role, Name string } `json:"nodes"`
		}
		_ = json.Unmarshal(raw, &s)
		var out []string
		for _, n := range s.Nodes {
			out = append(out, n.Role+":"+n.Name)
		}
		return out
	}

	// --role button -> only the 4 buttons (2 in region + 2 Save).
	buttons := names(SnapOpts{Role: "button"})
	if len(buttons) != 4 {
		t.Errorf("--role button = %v, want 4 buttons", buttons)
	}
	for _, n := range buttons {
		if n[:7] != "button:" {
			t.Errorf("--role button returned a non-button: %q", n)
		}
	}

	// --grep matches the accessible name (regex).
	events := names(SnapOpts{Grep: "^Event "})
	if len(events) != 2 {
		t.Errorf("--grep '^Event ' = %v, want 2 (Standup, Review)", events)
	}

	// --region scopes to the Calendar container's subtree (Save excluded).
	inCal := names(SnapOpts{Region: "Calendar", Role: "button"})
	if len(inCal) != 2 {
		t.Errorf("--region Calendar --role button = %v, want 2 (the region's events)", inCal)
	}
	for _, n := range inCal {
		if n == "button:Save" {
			t.Errorf("--region Calendar leaked a node outside the region: %q", n)
		}
	}

	// --dedupe collapses the two identical "Save" buttons into one.
	saves := 0
	for _, n := range names(SnapOpts{Role: "button", Dedupe: true}) {
		if n == "button:Save" {
			saves++
		}
	}
	if saves != 1 {
		t.Errorf("--dedupe left %d 'Save' buttons, want 1", saves)
	}
}
