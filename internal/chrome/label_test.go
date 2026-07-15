package chrome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --by label resolves a form control by its visible label text, across the
// common patterns: <label for>, a wrapping <label>, and a sibling label element
// next to the control (the Engage case, where the <select> has no accessible name).
func TestLabelAddressing(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>Label</title><body>
<label for="em">Email</label><input id="em">
<label>Comments <textarea id="cm"></textarea></label>
<div class="field"><span>Activity Category</span>
  <select id="cat"><option>Select…</option><option>Direct Revenue</option><option>Recruiting</option></select></div>
<div class="field"><div class="lbl">Notes</div><textarea id="nt"></textarea></div>
</body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// <label for> input.
	if _, err := b.Fill(ctx, id, "Email", "a@b.com", QueryOpts{By: "label"}); err != nil {
		t.Fatalf("Fill --by label Email: %v", err)
	}
	if v := evalString(ctx, t, b, id, "document.getElementById('em').value"); v != "a@b.com" {
		t.Errorf("Email = %q, want a@b.com", v)
	}

	// Wrapping <label> around a textarea.
	if _, err := b.Fill(ctx, id, "Comments", "hello", QueryOpts{By: "label"}); err != nil {
		t.Fatalf("Fill --by label Comments: %v", err)
	}
	if v := evalString(ctx, t, b, id, "document.getElementById('cm').value"); v != "hello" {
		t.Errorf("Comments = %q, want hello", v)
	}

	// Sibling label next to a native <select> with no accessible name (Engage
	// pattern) — resolve it and select an option via `select --by label`.
	if _, err := b.Select(ctx, id, "Activity Category", "Direct Revenue", SelectOpts{Query: QueryOpts{By: "label"}}); err != nil {
		t.Fatalf("Select --by label Activity Category: %v", err)
	}
	if v := evalString(ctx, t, b, id, "(() => { const s=document.getElementById('cat'); return s.options[s.selectedIndex].text; })()"); v != "Direct Revenue" {
		t.Errorf("Category = %q, want Direct Revenue", v)
	}

	// Sibling non-<label> element (a div) labelling a textarea.
	if _, err := b.Fill(ctx, id, "Notes", "note text", QueryOpts{By: "label"}); err != nil {
		t.Fatalf("Fill --by label Notes: %v", err)
	}
	if v := evalString(ctx, t, b, id, "document.getElementById('nt').value"); v != "note text" {
		t.Errorf("Notes = %q, want 'note text'", v)
	}
}
