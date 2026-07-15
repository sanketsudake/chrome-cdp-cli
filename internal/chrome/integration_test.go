package chrome

// Integration test against a MANAGED headless Chrome (Path A) — a throwaway
// browser, never the user's live Chrome. Skips when -short or when Chrome can't
// be launched (e.g. CI without a browser).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagedChromeDrivesAPage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Drive a managed headless Chrome directly (Path A), independent of the
	// connection ladder — so this runs even when the dev's real Chrome is up.
	b, err := launch(true, t.TempDir(), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Fixture</title><body><button id="go">Go</button></body>`)
	}))
	defer srv.Close()

	tabs, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tabs) == 0 {
		t.Fatal("List returned no page targets")
	}
	id := tabs[0].ID

	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	got, err := b.Eval(ctx, id, "document.title")
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if v := got.(map[string]any)["value"]; v != "Fixture" {
		t.Errorf("document.title = %v, want Fixture", v)
	}

	// raw passthrough: Runtime.evaluate returning a value.
	rawRes, err := b.Raw(ctx, id, "Runtime.evaluate", []byte(`{"expression":"1+1","returnByValue":true}`))
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	if !strings.Contains(fmt.Sprintf("%v", rawRes), "2") {
		t.Errorf("raw Runtime.evaluate result = %v, want it to contain 2", rawRes)
	}

	// frame enumeration returns the real frame tree.
	fr, err := b.Frames(ctx, id)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	frames := fr.(map[string]any)["frames"].([]map[string]any)
	if len(frames) == 0 {
		t.Fatal("Frames returned no frames")
	}
}
