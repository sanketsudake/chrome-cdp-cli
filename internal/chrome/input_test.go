package chrome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Click and Type resolve the target and drive it via a coordinate pointer
// sequence at its occlusion-verified centre (the same primitive as select), so
// they work without relying on chromedp's box-model node-click.
func TestClickAndTypeCoordinate(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>Input</title><body>
<input aria-label="Search" id="q">
<button aria-label="Go" onclick="window.__q=document.getElementById('q').value">Go</button>
</body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// Type focuses the input via a coordinate click, then sends real keystrokes.
	if _, err := b.Type(ctx, id, "Search", "hello world", QueryOpts{By: "name"}); err != nil {
		t.Fatalf("Type: %v", err)
	}
	got, err := b.Eval(ctx, id, "document.getElementById('q').value")
	if err != nil {
		t.Fatalf("Eval value: %v", err)
	}
	if v := got.(map[string]any)["value"]; v != "hello world" {
		t.Errorf("input value = %v, want 'hello world'", v)
	}

	// Click drives the button (which snapshots the typed value).
	if _, err := b.Click(ctx, id, "Go", QueryOpts{By: "name", Role: "button"}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	got, err = b.Eval(ctx, id, "window.__q")
	if err != nil {
		t.Fatalf("Eval __q: %v", err)
	}
	if v := got.(map[string]any)["value"]; v != "hello world" {
		t.Errorf("window.__q = %v, want 'hello world' (click fired onclick)", v)
	}

	// An occluded target has no clickable centre — bounded so it fails fast.
	if _, err := b.Eval(ctx, id, `(() => { const o=document.createElement('div'); o.style.cssText='position:fixed;inset:0;z-index:9999'; o.id='cover'; document.body.appendChild(o); })()`); err != nil {
		t.Fatalf("add overlay: %v", err)
	}
	occCtx, occCancel := context.WithTimeout(ctx, 3*time.Second)
	defer occCancel()
	if _, err := b.Click(occCtx, id, "Go", QueryOpts{By: "name", Role: "button"}); err == nil {
		t.Error("Click on a fully-occluded button returned nil error, want a not-clickable failure")
	}
}
