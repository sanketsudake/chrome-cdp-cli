package chrome

// Coordinate-space interaction (RFC-0014): acting at a viewport point with no
// element resolution, which is the only way to drive canvas/WebGL surfaces the
// accessibility tree cannot see.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// coordFixture serves a page that records every pointer event into
// window.__log, with a canvas and a button at known fixed positions.
func coordFixture(t *testing.T) (*CDP, context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Coord</title><body style="margin:0">
<canvas id="c" width="400" height="300" style="position:fixed;left:0;top:0"></canvas>
<button id="b" style="position:fixed;left:420px;top:40px;width:100px;height:30px">Go</button>
<p id="para" style="position:fixed;left:0;top:320px;width:380px">One two three. Four five six.</p>
<script>
window.__log = [];
for (const t of ["mousedown","mouseup","click","dblclick","contextmenu","mousemove","wheel"]) {
  document.addEventListener(t, e => window.__log.push(
    {t: e.type, x: e.clientX, y: e.clientY, d: e.detail,
     dx: e.deltaX, dy: e.deltaY, tag: (e.target.id || e.target.tagName)}), true);
}
</script></body>`)
	}))
	t.Cleanup(srv.Close)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	return b, ctx, id
}

type logEvent struct {
	T   string  `json:"t"`
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
	D   int     `json:"d"`
	DX  float64 `json:"dx"`
	DY  float64 `json:"dy"`
	Tag string  `json:"tag"`
}

func readLog(t *testing.T, b *CDP, ctx context.Context, id string) []logEvent {
	t.Helper()
	v, err := b.Eval(ctx, id, "window.__log", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval log: %v", err)
	}
	raw, _ := json.Marshal(v.(map[string]any)["value"])
	var out []logEvent
	_ = json.Unmarshal(raw, &out)
	return out
}

func resetLog(t *testing.T, b *CDP, ctx context.Context, id string) {
	t.Helper()
	if _, err := b.Eval(ctx, id, "window.__log = []", EvalOpts{}); err != nil {
		t.Fatalf("Eval reset: %v", err)
	}
}

// VS-1 + VS-2: a coordinate click lands where asked and reports what was under it.
func TestPointerAtClicksCoordinate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id := coordFixture(t)

	res, err := b.Pointer(ctx, id, "", PointerOpts{Action: PointerClick, At: &Point{X: 200, Y: 150}})
	if err != nil {
		t.Fatalf("click --at: %v", err)
	}
	if res["x"] != 200.0 || res["y"] != 150.0 {
		t.Errorf("result = %v, want x=200 y=150", res)
	}
	hit, _ := res["hit"].(map[string]any)
	if hit == nil || hit["tag"] != "CANVAS" {
		t.Errorf("hit = %v, want the canvas", res["hit"])
	}
	got := readLog(t, b, ctx, id)
	var clicked bool
	for _, e := range got {
		if e.T == "click" && e.X == 200 && e.Y == 150 && e.Tag == "c" {
			clicked = true
		}
	}
	if !clicked {
		t.Errorf("no click at (200,150) on the canvas: %+v", got)
	}
}

// VS-3: a coordinate outside the viewport is refused before anything is
// dispatched — a wrong-sized window must not become a mis-click.
func TestPointerAtRejectsOutOfViewport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id := coordFixture(t)
	resetLog(t, b, ctx, id)

	_, err := b.Pointer(ctx, id, "", PointerOpts{Action: PointerClick, At: &Point{X: 99999, Y: 10}})
	if err == nil {
		t.Fatal("a coordinate far outside the viewport was accepted")
	}
	if !IsCoordinateOOB(err) {
		t.Errorf("error %v is not classified as out-of-bounds", err)
	}
	if got := readLog(t, b, ctx, id); len(got) != 0 {
		t.Errorf("events were dispatched despite the refusal: %+v", got)
	}
}

// VS-6: a coordinate drag emits a full interpolated sequence between two points.
func TestPointerAtDragsBetweenCoordinates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id := coordFixture(t)
	resetLog(t, b, ctx, id)

	_, err := b.Pointer(ctx, id, "", PointerOpts{
		Action: PointerDrag,
		At:     &Point{X: 20, Y: 40},
		ToAt:   &Point{X: 300, Y: 40},
		Steps:  6,
	})
	if err != nil {
		t.Fatalf("drag --at: %v", err)
	}
	got := readLog(t, b, ctx, id)
	var downs, ups, moves int
	for _, e := range got {
		switch e.T {
		case "mousedown":
			downs++
		case "mouseup":
			ups++
		case "mousemove":
			moves++
		}
	}
	if downs != 1 || ups != 1 {
		t.Errorf("got %d mousedown / %d mouseup, want 1 each: %+v", downs, ups, got)
	}
	if moves < 6 {
		t.Errorf("got %d mousemove, want at least the 6 interpolated steps", moves)
	}
	var released *logEvent
	for i, e := range got {
		if e.T == "mouseup" {
			released = &got[i]
		}
	}
	if released == nil || released.X != 300 || released.Y != 40 {
		t.Errorf("drag released at %+v, want mouseup at (300,40)", released)
	}
}

// VS-5: triple-click selects the paragraph, and the page sees detail==3.
func TestTripleClickSelectsParagraph(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id := coordFixture(t)
	resetLog(t, b, ctx, id)

	if _, err := b.Pointer(ctx, id, "#para", PointerOpts{Action: PointerTripleClick}); err != nil {
		t.Fatalf("tripleclick: %v", err)
	}
	v, err := b.Eval(ctx, id, "window.getSelection().toString()", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval selection: %v", err)
	}
	sel, _ := v.(map[string]any)["value"].(string)
	if len(sel) < len("One two three.") {
		t.Errorf("selection = %q, want the whole paragraph", sel)
	}
	var sawThree bool
	for _, e := range readLog(t, b, ctx, id) {
		if e.T == "click" && e.D == 3 {
			sawThree = true
		}
	}
	if !sawThree {
		t.Error("no click with detail==3; the page never saw a triple-click")
	}
}

// VS-8: a wheel anchored at a coordinate is delivered at that point.
func TestScrollWheelAtCoordinate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id := coordFixture(t)
	resetLog(t, b, ctx, id)

	if _, err := b.Scroll(ctx, id, "", ScrollOpts{Wheel: true, At: &Point{X: 120, Y: 90}, Dy: -240}); err != nil {
		t.Fatalf("scroll --wheel --at: %v", err)
	}
	// Chrome delivers wheel events asynchronously — the dispatch call returns
	// before the page sees it — so poll rather than reading once.
	var ok bool
	deadline := time.Now().Add(5 * time.Second)
	var last []logEvent
	for !ok && time.Now().Before(deadline) {
		last = readLog(t, b, ctx, id)
		for _, e := range last {
			if e.T == "wheel" && e.X == 120 && e.Y == 90 && e.DY == -240 {
				ok = true
			}
		}
		if !ok {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !ok {
		t.Errorf("no wheel at (120,90) with dy=-240: %+v", last)
	}
}

// THE coordinate contract: a coordinate read off a `screenshot --scale 1`
// capture means the same thing to `--at`. This is the test that catches the
// contract breaking on a HiDPI machine, where the capture's device pixels and
// the page's CSS pixels differ by the device scale factor.
func TestScreenshotScale1MatchesCoordinateSpace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id := coordFixture(t)

	// The fixture's button occupies CSS pixels x=420..520, y=40..70.
	png, meta, err := b.Screenshot(ctx, id, ShotOpts{Scale: 1})
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if len(png) == 0 {
		t.Fatal("empty capture")
	}
	v, err := b.Eval(ctx, id, "({w: window.innerWidth, h: window.innerHeight})", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval viewport: %v", err)
	}
	vp, _ := v.(map[string]any)["value"].(map[string]any)
	wantW, _ := vp["w"].(float64)
	wantH, _ := vp["h"].(float64)

	gotW, gotH := metaDim(meta["width"]), metaDim(meta["height"])
	if gotW != wantW || gotH != wantH {
		t.Errorf("--scale 1 capture is %dx%d but the viewport is %gx%g; a coordinate read off this image would not map 1:1 onto --at",
			int(gotW), int(gotH), wantW, wantH)
	}

	// And the centre of that image region really is the button: act there.
	resetLog(t, b, ctx, id)
	if _, err := b.Pointer(ctx, id, "", PointerOpts{Action: PointerClick, At: &Point{X: 470, Y: 55}}); err != nil {
		t.Fatalf("click --at on the button: %v", err)
	}
	var onButton bool
	for _, e := range readLog(t, b, ctx, id) {
		if e.T == "click" && e.Tag == "b" {
			onButton = true
		}
	}
	if !onButton {
		t.Errorf("a click at the button's CSS-pixel centre did not land on it: %+v", readLog(t, b, ctx, id))
	}
}

// VS-11/VS-12: the window resizes and reports what actually resulted.
func TestWindowResizeRoundTrips(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id := coordFixture(t)

	before, err := b.Window(ctx, id, WindowOpts{})
	if err != nil {
		t.Fatalf("window info: %v", err)
	}
	if before.Width == 0 || before.Height == 0 {
		t.Fatalf("window info returned no size: %+v", before)
	}

	got, err := b.Window(ctx, id, WindowOpts{Width: 1100, Height: 700})
	if err != nil {
		t.Fatalf("window size: %v", err)
	}
	if got.Width != 1100 || got.Height != 700 {
		t.Errorf("window bounds = %dx%d, want 1100x700", got.Width, got.Height)
	}
	if got.State != "normal" {
		t.Errorf("window state = %q, want normal", got.State)
	}
	// The page agrees: this is a real window change, not viewport emulation.
	v, err := b.Eval(ctx, id, "window.outerWidth", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval outerWidth: %v", err)
	}
	if ow, _ := v.(map[string]any)["value"].(float64); ow != 1100 {
		t.Errorf("window.outerWidth = %v, want 1100", ow)
	}
}

// metaDim normalizes a capture metadata dimension, which travels as an int
// in-process and as a float64 after a JSON round trip.
func metaDim(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}
