package chrome

// find reports whether a match's centre is actually hit-testable, using the
// same occlusion probe the pointer verbs use. A coordinate that a click would
// miss must not look identical to one that would land.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFindReportsOcclusion(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>Occlusion</title><body style="margin:0">
<button id="clear" style="position:fixed;left:10px;top:10px;width:120px;height:40px">Save clear</button>
<button id="under" style="position:fixed;left:10px;top:100px;width:120px;height:40px">Save covered</button>
<div style="position:fixed;left:0;top:90px;width:400px;height:60px;background:#000"></div>
</body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	got, err := b.Find(ctx, id, "save", FindOpts{Role: "button"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	raw, _ := json.Marshal(got["matches"])
	var ms []map[string]any
	_ = json.Unmarshal(raw, &ms)
	if len(ms) != 2 {
		t.Fatalf("got %d matches, want both buttons: %v", len(ms), ms)
	}
	by := map[string]map[string]any{}
	for _, m := range ms {
		by[m["name"].(string)] = m
	}
	if occ, ok := by["Save clear"]["occluded"]; ok && occ == true {
		t.Errorf("an unobstructed button was reported occluded: %v", by["Save clear"])
	}
	if occ, _ := by["Save covered"]["occluded"].(bool); !occ {
		t.Errorf("a covered button was not reported occluded: %v", by["Save covered"])
	}
	// Occlusion is reported, never fatal: the match is still returned with a centre.
	if by["Save covered"]["center"] == nil {
		t.Errorf("covered match lost its centre: %v", by["Save covered"])
	}
}
