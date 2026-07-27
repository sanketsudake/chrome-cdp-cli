package chrome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Fill replaces a field's existing content (select-all + type), unlike Type which
// appends keystrokes.
func TestFillReplacesValue(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>Fill</title><body>
<input aria-label="Hours" id="h" value="0">
<input aria-label="Name" id="n" value="old name"></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// Fill a numeric cell that already shows "0" — must become "8", not "80"/"08".
	if _, err := b.Fill(ctx, id, "Hours", "8", QueryOpts{By: "name"}); err != nil {
		t.Fatalf("Fill Hours: %v", err)
	}
	if got := evalString(ctx, t, b, id, "document.getElementById('h').value"); got != "8" {
		t.Errorf("Hours = %q, want 8 (replaced, not appended)", got)
	}

	// Fill a text field that has existing content.
	if _, err := b.Fill(ctx, id, "Name", "new name", QueryOpts{By: "name"}); err != nil {
		t.Fatalf("Fill Name: %v", err)
	}
	if got := evalString(ctx, t, b, id, "document.getElementById('n').value"); got != "new name" {
		t.Errorf("Name = %q, want 'new name'", got)
	}

	// Contrast: Type appends to the existing value.
	if _, err := b.Fill(ctx, id, "Hours", "5", QueryOpts{By: "name"}); err != nil {
		t.Fatalf("reset Hours: %v", err)
	}
	if _, err := b.Type(ctx, id, "Hours", "5", QueryOpts{By: "name"}); err != nil {
		t.Fatalf("Type Hours: %v", err)
	}
	if got := evalString(ctx, t, b, id, "document.getElementById('h').value"); got != "55" {
		t.Errorf("after Type = %q, want 55 (Type appends)", got)
	}
}

// Values reads every matching element's value/text in one round trip.
func TestValuesReadsAll(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>Vals</title><body>
<input class="hr" value="8"><input class="hr" value="0"><input class="hr" value="8">
<span class="pill">Alpha</span><span class="pill">Beta</span></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	got, err := b.Values(ctx, id, "input.hr", QueryOpts{})
	if err != nil {
		t.Fatalf("Values inputs: %v", err)
	}
	vals := toStrings(got["values"])
	if len(vals) != 3 || vals[0] != "8" || vals[1] != "0" || vals[2] != "8" {
		t.Errorf("input values = %v, want [8 0 8]", vals)
	}

	got, err = b.Values(ctx, id, "span.pill", QueryOpts{})
	if err != nil {
		t.Fatalf("Values pills: %v", err)
	}
	pills := toStrings(got["values"])
	if len(pills) != 2 || pills[0] != "Alpha" || pills[1] != "Beta" {
		t.Errorf("pill texts = %v, want [Alpha Beta]", pills)
	}
}

func evalString(ctx context.Context, t *testing.T, b Browser, id, js string) string {
	t.Helper()
	got, err := b.Eval(ctx, id, js, EvalOpts{})
	if err != nil {
		t.Fatalf("Eval %q: %v", js, err)
	}
	v, _ := got.(map[string]any)["value"].(string)
	return v
}
