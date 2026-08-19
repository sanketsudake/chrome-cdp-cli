package chrome

// Live-Chrome tests for `screenshot --annotate` (RFC-0016).
//
// Assertions are on COUNTS, BOUNDS and SAMPLED PIXELS, never byte-for-byte
// image equality — the same discipline capture_test.go follows and for the
// same reason: encoders and font rasterisation drift across platforms and
// Chrome versions.
//
// Skipped under -short, and never parallel: they share a spawned browser.

import (
	"context"
	"math"
	"testing"
	"time"
)

const annotateThreeButtonsFixture = `<!doctype html><title>Buttons</title>
<style>
  html, body { margin:0; background:#fff; }
  button { position:absolute; width:80px; height:30px; }
  #a { left:20px; top:20px; }
  #b { left:20px; top:80px; }
  #c { left:20px; top:140px; }
</style>
<body>
<button id="a" onclick="(window.__log=window.__log||[]).push('a')">A</button>
<button id="b" onclick="(window.__log=window.__log||[]).push('b')">B</button>
<button id="c" onclick="(window.__log=window.__log||[]).push('c')">C</button>
</body>`

// annotationsOf reads the `annotations` field back out of a driver metadata
// map as []map[string]any, failing the test if the shape is not what
// annotatePass builds.
func annotationsOf(t *testing.T, meta map[string]any) []map[string]any {
	t.Helper()
	raw, ok := meta["annotations"].([]any)
	if !ok {
		t.Fatalf("meta.annotations = %v (%T), want []any", meta["annotations"], meta["annotations"])
	}
	out := make([]map[string]any, len(raw))
	for i, a := range raw {
		m, ok := a.(map[string]any)
		if !ok {
			t.Fatalf("annotations[%d] = %v (%T), want a map", i, a, a)
		}
		out[i] = m
	}
	return out
}

func annotationCenter(t *testing.T, a map[string]any) (float64, float64) {
	t.Helper()
	c, ok := a["center"].(map[string]any)
	if !ok {
		t.Fatalf("annotation center = %v, want a map", a["center"])
	}
	x, _ := c["x"].(float64)
	y, _ := c["y"].(float64)
	return x, y
}

// VS-1, VS-2, VS-4: three buttons produce three contiguous labels, each ref is
// a live address a click can act on, and scale halves the drawn position.
func TestScreenshotAnnotateThreeButtons(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := captureFixture(t, annotateThreeButtonsFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	dpr := captureNumber(ctx, t, b, id, "window.devicePixelRatio")

	buf, meta, err := b.Screenshot(ctx, id, ShotOpts{Annotate: true})
	if err != nil {
		t.Fatalf("annotated screenshot: %v", err)
	}
	if meta["annotated"] != true {
		t.Fatalf("meta.annotated = %v, want true", meta["annotated"])
	}
	if meta["truncated"] != false {
		t.Errorf("meta.truncated = %v, want false for three buttons", meta["truncated"])
	}
	anns := annotationsOf(t, meta)
	if len(anns) != 3 {
		t.Fatalf("len(annotations) = %d, want 3", len(anns))
	}

	img, _ := decodeCapture(t, buf)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	for i, a := range anns {
		if n, _ := a["n"].(int); n != i+1 {
			t.Errorf("annotations[%d].n = %v, want %d (contiguous 1..K)", i, a["n"], i+1)
		}
		role, _ := a["role"].(string)
		if role != "button" {
			t.Errorf("annotations[%d].role = %q, want button", i, role)
		}
		ref, _ := a["ref"].(string)
		if ref == "" || ref[0] != 'e' {
			t.Errorf("annotations[%d].ref = %q, want a non-empty e<id>", i, ref)
		}
		cx, cy := annotationCenter(t, a)
		if cx < 0 || cx >= float64(w) || cy < 0 || cy >= float64(h) {
			// center is viewport CSS pixels; at scale 1 / dpr 1 that maps
			// identically onto the image, so this also pins the identity case.
			if dpr == 1 {
				t.Errorf("annotations[%d].center = (%g,%g), want inside [0,%d)x[0,%d)", i, cx, cy, w, h)
			}
		}
	}

	// VS-2: the second annotation's ref is a live address — clicking it lands
	// on the button whose id is "b", recorded by the fixture into __log.
	second := anns[1]["ref"].(string)
	if _, err := b.Pointer(ctx, id, second, PointerOpts{Action: PointerClick, Query: QueryOpts{By: "ref"}}); err != nil {
		t.Fatalf("click --by ref %s: %v", second, err)
	}
	logv, err := b.Eval(ctx, id, "(window.__log||[]).join(',')", EvalOpts{})
	if err != nil {
		t.Fatalf("reading __log: %v", err)
	}
	got, _ := logv.(map[string]any)["value"].(string)
	if got != "b" {
		t.Errorf("click on annotations[1].ref landed on __log=%q, want \"b\" (the second button)", got)
	}

	// VS-4: at half scale, the disc lands at half the reported centre (scaled
	// by the device pixel ratio, the same way the unscaled capture is).
	buf2, meta2, err := b.Screenshot(ctx, id, ShotOpts{Annotate: true, Scale: 0.5})
	if err != nil {
		t.Fatalf("annotated screenshot at scale 0.5: %v", err)
	}
	anns2 := annotationsOf(t, meta2)
	if len(anns2) != 3 {
		t.Fatalf("len(annotations) at scale 0.5 = %d, want 3", len(anns2))
	}
	img2, _ := decodeCapture(t, buf2)
	markRed := struct{ R, G, B uint32 }{0xE1, 0x1D, 0x48}
	found := false
	for _, a := range anns2 {
		cx, cy := annotationCenter(t, a)
		px := int(math.Round(cx * 0.5 * dpr))
		py := int(math.Round(cy * 0.5 * dpr))
		r, g, bl, _ := img2.At(px, py).RGBA()
		if near(r>>8, markRed.R, 24) && near(g>>8, markRed.G, 24) && near(bl>>8, markRed.B, 24) {
			found = true
			break
		}
	}
	if !found {
		t.Error("no label disc found at any annotation's half-scaled centre")
	}
}

func near(got, want uint32, tol uint32) bool {
	if got > want {
		return got-want <= tol
	}
	return want-got <= tol
}

const annotateToolbarFixture = `<!doctype html><title>Toolbar</title>
<style>
  html, body { margin:0; background:#fff; }
  #toolbar { position:absolute; left:0; top:0; width:300px; height:60px; background:#eee; }
  #in1 { position:absolute; left:10px; top:10px; width:80px; height:30px; }
  #in2 { position:absolute; left:110px; top:10px; width:80px; height:30px; }
  #outside { position:absolute; left:10px; top:200px; width:80px; height:30px; }
</style>
<body>
<div id="toolbar">
  <button id="in1">In1</button>
  <button id="in2">In2</button>
</div>
<button id="outside">Out</button>
</body>`

// VS-3: a cropped capture (--selector) only labels and lists the actionable
// nodes inside its own clip.
func TestScreenshotAnnotateSelectorDropsOutsideNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := captureFixture(t, annotateToolbarFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	_, meta, err := b.Screenshot(ctx, id, ShotOpts{Selector: "#toolbar", Annotate: true})
	if err != nil {
		t.Fatalf("annotated element screenshot: %v", err)
	}
	anns := annotationsOf(t, meta)
	if len(anns) != 2 {
		t.Fatalf("len(annotations) = %d, want 2 (in1, in2 only)", len(anns))
	}
	for _, a := range anns {
		if name, _ := a["name"].(string); name == "Out" {
			t.Error("the outside button's name (\"Out\") appears in a --selector \"#toolbar\" legend")
		}
	}
}

// VS-5: a backgrounded tab must never block or fail the capture. US-4's exact
// acceptance is `annotated: false` with `reason: "tab_hidden"`, which rests on
// Chrome actually throttling the accessibility tree once the tab is
// backgrounded (key.go's focusedReadTimeout comment, and find's DOM-fallback
// path, document the same premise).
//
// That premise does not reliably reproduce under Page.bringToFront in a
// headless test harness — no real window manager backgrounds the target, and
// this test's own probe confirms document.visibilityState does flip to
// "hidden" while the accessibility tree keeps answering normally regardless.
// find's own DOM-fallback ranking hit the identical wall and was made
// pure-testable instead (see TestRankDOMCandidates in find_test.go); this RFC
// does the same for its reason branching (TestAnnotateDegradeReason).
//
// So this live test asserts what IS reliably true on a backgrounded tab —
// the command never hangs or errors, the returned image always decodes, and
// whichever outcome the pass reaches is INTERNALLY CONSISTENT with the
// envelope contract — rather than asserting the specific tab_hidden branch.
func TestScreenshotAnnotateOnBackgroundTab(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := captureFixture(t, annotateThreeButtonsFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	other, err := b.Open(ctx, "about:blank")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := b.Raw(ctx, other["id"].(string), "Page.bringToFront", nil); err != nil {
		t.Fatalf("bringToFront on the second tab: %v", err)
	}

	start := time.Now()
	buf, meta, err := b.Screenshot(ctx, id, ShotOpts{Annotate: true})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("annotated screenshot on a background tab: %v", err)
	}
	// Comfortably above annotateTimeout (3s) plus the capture itself, so this
	// still catches a real hang without being sensitive to CI scheduling noise.
	if elapsed > 20*time.Second {
		t.Errorf("annotated screenshot on a background tab took %s, want well under the annotateTimeout bound", elapsed)
	}
	// The plain (or annotated) image always decodes — the capture is never lost.
	decodeCapture(t, buf)

	switch annotated := meta["annotated"]; annotated {
	case false:
		if reason, _ := meta["reason"].(string); reason == "" {
			t.Error("meta.reason is missing on a degraded (annotated:false) capture")
		}
		if arr, ok := meta["annotations"].([]any); !ok || len(arr) != 0 {
			t.Errorf("meta.annotations = %v, want an empty array on a degrade", meta["annotations"])
		}
	case true:
		if _, has := meta["reason"]; has {
			t.Error("meta.reason is present on a successful (annotated:true) capture")
		}
		if anns := annotationsOf(t, meta); len(anns) == 0 {
			t.Error("meta.annotated is true but annotations is empty")
		}
	default:
		t.Fatalf("meta.annotated = %v (%T), want a bool", annotated, annotated)
	}
}

const annotateManyButtonsFixture = `<!doctype html><title>Many</title>
<style>
  html, body { margin:0; background:#fff; }
  #wrap { display:flex; flex-wrap:wrap; width:900px; }
  #wrap button { width:24px; height:24px; margin:1px; font-size:8px; }
</style>
<body><div id="wrap"></div>
<script>
  const wrap = document.getElementById('wrap');
  for (let i = 0; i < 250; i++) {
    const btn = document.createElement('button');
    btn.textContent = i;
    wrap.appendChild(btn);
  }
</script>
</body>`

// VS-6: a page with more actionable nodes than the cap allows still produces a
// bounded, contiguous legend, and reports that it was truncated.
func TestScreenshotAnnotateCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := captureFixture(t, annotateManyButtonsFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	_, meta, err := b.Screenshot(ctx, id, ShotOpts{Annotate: true, FullPage: true})
	if err != nil {
		t.Fatalf("annotated full-page screenshot: %v", err)
	}
	if meta["truncated"] != true {
		t.Errorf("meta.truncated = %v, want true for 250 buttons against a 200 cap", meta["truncated"])
	}
	anns := annotationsOf(t, meta)
	if len(anns) != maxAnnotations {
		t.Fatalf("len(annotations) = %d, want %d", len(anns), maxAnnotations)
	}
	for i, a := range anns {
		if n, _ := a["n"].(int); n != i+1 {
			t.Fatalf("annotations[%d].n = %v, want %d — numbers must be contiguous 1..%d", i, a["n"], i+1, maxAnnotations)
		}
	}
}
