package chrome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGridReadsTable(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>Grid</title><body>
<table>
  <thead><tr><th>Day</th><th>Hours</th></tr></thead>
  <tbody>
    <tr><td>Mon</td><td>8</td></tr>
    <tr><td>Tue</td><td>0</td></tr>
    <tr><td>Wed</td><td>8</td></tr>
  </tbody>
</table></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	got, err := b.Grid(ctx, id, "", QueryOpts{})
	if err != nil {
		t.Fatalf("Grid: %v", err)
	}
	res := got.(map[string]any)

	headers := toStrings(res["headers"])
	if len(headers) != 2 || headers[0] != "Day" || headers[1] != "Hours" {
		t.Errorf("headers = %v, want [Day Hours]", headers)
	}
	rows, _ := res["rows"].([][]string)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (%v)", len(rows), res["rows"])
	}
	if rows[0][0] != "Mon" || rows[0][1] != "8" || rows[2][0] != "Wed" {
		t.Errorf("rows = %v, want Mon/8 … Wed/8", rows)
	}
}

func TestScrollIntoViewAndWheel(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>Scroll</title><body style="margin:0">
<div style="height:4000px"></div>
<div id="target" style="height:40px">BOTTOM</div>
<div style="height:1000px"></div></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if _, err := b.EmulateViewport(ctx, id, 800, 600); err != nil {
		t.Fatalf("EmulateViewport: %v", err)
	}

	// --to scrolls the far element into the viewport.
	if _, err := b.Scroll(ctx, id, "#target", ScrollOpts{Into: true}); err != nil {
		t.Fatalf("Scroll --to: %v", err)
	}
	got, err := b.Eval(ctx, id, "(() => { const r = document.getElementById('target').getBoundingClientRect(); return r.top >= 0 && r.top <= window.innerHeight; })()", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if v := got.(map[string]any)["value"]; v != true {
		t.Errorf("target not in viewport after scroll --to (in-view=%v)", v)
	}

	// A default (JS) delta scroll moves the page deterministically.
	if _, err := b.Eval(ctx, id, "window.scrollTo(0,0)", EvalOpts{}); err != nil {
		t.Fatalf("reset scroll: %v", err)
	}
	if _, err := b.Scroll(ctx, id, "", ScrollOpts{Dy: 600}); err != nil {
		t.Fatalf("Scroll by: %v", err)
	}
	got, err = b.Eval(ctx, id, "window.scrollY", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval scrollY: %v", err)
	}
	if v, _ := got.(map[string]any)["value"].(float64); v < 500 {
		t.Errorf("scrollY after scroll --dy 600 = %v, want ~600", v)
	}
}

func toStrings(v any) []string {
	if ss, ok := v.([]string); ok {
		return ss
	}
	if xs, ok := v.([]any); ok {
		out := make([]string, len(xs))
		for i, x := range xs {
			out[i], _ = x.(string)
		}
		return out
	}
	return nil
}
