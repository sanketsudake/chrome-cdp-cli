package chrome

// Live-Chrome tests for the capture drivers (RFC-0008 VS-1..VS-8, VS-11,
// VS-13, VS-14), plus the pure helpers they lean on.
//
// Captures are asserted on DIMENSIONS and SAMPLED PIXELS, never on byte-for-byte
// equality: PNG encoders, font rasterisation and colour management all drift
// across Chrome versions and platforms, and a golden-bytes test would fail on
// every upgrade while still not proving the right region was captured. Sampling
// a pixel at a computed coordinate does prove exactly that — an equally-sized
// capture of the wrong rectangle comes back the wrong colour.
//
// Skipped under -short, and never parallel: they share a spawned browser.

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// captureFixture serves one HTML page for the length of a test.
func captureFixture(t *testing.T, html string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, html)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// captureNumber evaluates a numeric expression in the tab.
func captureNumber(ctx context.Context, t *testing.T, b *CDP, id, expr string) float64 {
	t.Helper()
	v, err := b.Eval(ctx, id, expr, EvalOpts{})
	if err != nil {
		t.Fatalf("eval %s: %v", expr, err)
	}
	n, ok := v.(map[string]any)["value"].(float64)
	if !ok {
		t.Fatalf("eval %s = %v, want a number", expr, v)
	}
	return n
}

// decodeCapture decodes an artifact and returns the image plus its format name.
func decodeCapture(t *testing.T, buf []byte) (image.Image, string) {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("decoding a %d-byte capture: %v", len(buf), err)
	}
	return img, format
}

// metaClip reads the reported clip back out of the metadata map.
func metaClip(t *testing.T, meta map[string]any) Rect {
	t.Helper()
	r, ok := meta["clip"].(Rect)
	if !ok {
		t.Fatalf("meta.clip = %v (%T), want a Rect", meta["clip"], meta["clip"])
	}
	return r
}

// metaInt reads an int-valued metadata field.
func metaInt(t *testing.T, meta map[string]any, key string) int {
	t.Helper()
	n, ok := meta[key].(int)
	if !ok {
		t.Fatalf("meta.%s = %v (%T), want an int", key, meta[key], meta[key])
	}
	return n
}

// sampleIs reports whether the pixel at (x,y) is want, within tol per channel —
// JPEG is lossy and antialiasing bleeds, so an exact match would be brittle.
func sampleIs(img image.Image, x, y int, want color.RGBA, tol int) bool {
	r, g, b, _ := img.At(img.Bounds().Min.X+x, img.Bounds().Min.Y+y).RGBA()
	near := func(got uint32, w uint8) bool { return math.Abs(float64(got>>8)-float64(w)) <= float64(tol) }
	return near(r, want.R) && near(g, want.G) && near(b, want.B)
}

func sampleString(img image.Image, x, y int) string {
	r, g, b, _ := img.At(img.Bounds().Min.X+x, img.Bounds().Min.Y+y).RGBA()
	return fmt.Sprintf("rgb(%d,%d,%d)", r>>8, g>>8, b>>8)
}

// parsePDFFloat reads one number out of a PDF token.
func parsePDFFloat(t *testing.T, b []byte) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		t.Fatalf("parsing %q from the PDF: %v", b, err)
	}
	return v
}

var (
	fixtureRed  = color.RGBA{R: 0xff, G: 0x00, B: 0x00, A: 0xff}
	fixtureBlue = color.RGBA{R: 0x00, G: 0x80, B: 0xff, A: 0xff}
)

const captureGeometryFixture = `<!doctype html><title>Capture</title>
<style>
  html, body { margin:0; background:#ffffff; }
  body { height: 5000px; }
  #box    { position:absolute; left:120px; top:80px;  width:300px; height:200px; background:rgb(255,0,0); }
  #corner { position:absolute; left:0;     top:0;     width:100px; height:60px;  background:rgb(0,200,0); }
  #hidden { display:none; }
  #far    { position:absolute; left:50px;  top:3000px; width:200px; height:100px; background:rgb(0,128,255); }
</style>
<body>
<div id="box"></div>
<div id="corner"></div>
<div id="hidden">nothing to see</div>
<div id="far"></div>
<script>
// The offscreen element MOVES the first time the page scrolls — the cheapest
// stand-in for the sticky headers and lazy-loading reflow that make a rect read
// before scrolling into view a lie.
addEventListener("scroll", () => { document.getElementById("far").style.top = "3200px"; }, { once: true });
</script>
</body>`

// VS-1, VS-2, VS-6, VS-8, VS-11: element/region geometry against a fixture with
// known offsets and solid colour blocks.
func TestScreenshotGeometryModes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := captureFixture(t, captureGeometryFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	// The image comes back in device pixels; the clip is in CSS pixels.
	dpr := captureNumber(ctx, t, b, id, "window.devicePixelRatio")

	// VS-1: the capture is bounded to the element, the clip is its page box, and
	// the decoded image matches — in device pixels.
	buf, meta, err := b.Screenshot(ctx, id, ShotOpts{Selector: "#box"})
	if err != nil {
		t.Fatalf("element screenshot: %v", err)
	}
	if meta["mode"] != string(ShotElement) {
		t.Errorf("mode = %v, want element", meta["mode"])
	}
	if clip := metaClip(t, meta); clip != (Rect{X: 120, Y: 80, Width: 300, Height: 200}) {
		t.Errorf("clip = %+v, want the element's box {120 80 300 200}", clip)
	}
	img, format := decodeCapture(t, buf)
	if format != "png" {
		t.Errorf("decoded format = %q, want png", format)
	}
	wantW, wantH := int(math.Round(300*dpr)), int(math.Round(200*dpr))
	if got := img.Bounds().Dx(); got != wantW {
		t.Errorf("decoded width = %d, want %d (300 css px at dpr %g)", got, wantW, dpr)
	}
	if got := img.Bounds().Dy(); got != wantH {
		t.Errorf("decoded height = %d, want %d", got, wantH)
	}
	if metaInt(t, meta, "width") != img.Bounds().Dx() || metaInt(t, meta, "height") != img.Bounds().Dy() {
		t.Errorf("reported %vx%v but decoded %dx%d", meta["width"], meta["height"], img.Bounds().Dx(), img.Bounds().Dy())
	}
	// Bounded to the element means every sampled pixel is the element's colour,
	// including the corners — a capture one pixel off would show white there.
	for _, p := range [][2]int{{1, 1}, {wantW / 2, wantH / 2}, {wantW - 2, wantH - 2}} {
		if !sampleIs(img, p[0], p[1], fixtureRed, 8) {
			t.Errorf("pixel at (%d,%d) = %s, want the element's red", p[0], p[1], sampleString(img, p[0], p[1]))
		}
	}

	// VS-2: padding at the page's top-left corner expands the clip but cannot
	// push its origin negative — it is clamped to the room actually available.
	_, meta, err = b.Screenshot(ctx, id, ShotOpts{Selector: "#corner", Padding: 20})
	if err != nil {
		t.Fatalf("padded screenshot: %v", err)
	}
	clip := metaClip(t, meta)
	if clip.X < 0 || clip.Y < 0 {
		t.Errorf("clip = %+v, want a non-negative origin after clamping", clip)
	}
	if clip.X != 0 || clip.Y != 0 {
		t.Errorf("clip origin = (%g,%g), want (0,0): the element is at the corner", clip.X, clip.Y)
	}
	// It grew by the room available: 20px to the right and below, none above/left.
	if clip.Width != 120 || clip.Height != 80 {
		t.Errorf("clip size = %gx%g, want 120x80 (100x60 plus the available padding)", clip.Width, clip.Height)
	}

	// VS-6: an explicit region is captured verbatim and echoed back.
	buf, meta, err = b.Screenshot(ctx, id, ShotOpts{Region: &Rect{X: 10, Y: 20, Width: 400, Height: 300}})
	if err != nil {
		t.Fatalf("region screenshot: %v", err)
	}
	if meta["mode"] != string(ShotRegion) {
		t.Errorf("mode = %v, want region", meta["mode"])
	}
	if clip := metaClip(t, meta); clip != (Rect{X: 10, Y: 20, Width: 400, Height: 300}) {
		t.Errorf("clip = %+v, want the requested region", clip)
	}
	img, _ = decodeCapture(t, buf)
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != int(math.Round(400*dpr)) || h != int(math.Round(300*dpr)) {
		t.Errorf("region decoded %dx%d, want %gx%g", w, h, 400*dpr, 300*dpr)
	}
	// #box starts at (120,80); in a region anchored at (10,20) that is (110,60).
	if px, py := int(115*dpr), int(65*dpr); !sampleIs(img, px, py, fixtureRed, 8) {
		t.Errorf("pixel at (%d,%d) = %s, want the red box — the region is anchored wrong",
			px, py, sampleString(img, px, py))
	}

	// VS-8: scale halves the output, and does it in the renderer rather than by
	// resampling afterwards.
	_, halfMeta, err := b.Screenshot(ctx, id, ShotOpts{Region: &Rect{X: 10, Y: 20, Width: 400, Height: 300}, Scale: 0.5})
	if err != nil {
		t.Fatalf("scaled screenshot: %v", err)
	}
	if got, want := metaInt(t, halfMeta, "width"), metaInt(t, meta, "width")/2; got != want {
		t.Errorf("scaled width = %d, want %d (half the unscaled capture)", got, want)
	}
	if got, want := metaInt(t, halfMeta, "height"), metaInt(t, meta, "height")/2; got != want {
		t.Errorf("scaled height = %d, want %d", got, want)
	}
	if halfMeta["scale"] != 0.5 {
		t.Errorf("reported scale = %v, want 0.5", halfMeta["scale"])
	}

	// VS-11: an element with no box is a distinct, named failure — not a bare
	// timeout, and not a zero-width clip handed to Chrome.
	zeroCtx, zeroCancel := context.WithTimeout(ctx, 5*time.Second)
	defer zeroCancel()
	if _, _, err := b.Screenshot(zeroCtx, id, ShotOpts{Selector: "#hidden"}); !IsZeroArea(err) {
		t.Errorf("display:none element: err = %v, want one IsZeroArea recognises", err)
	}
}

// VS-3 and VS-4: an offscreen element is scrolled into view and captured from
// the box it has AFTER the scroll.
//
// The fixture moves #far the first time the page scrolls, so an implementation
// that reads the box before scrolling into view produces a capture of exactly
// the right size showing exactly the wrong 200x100 patch of page. That is the
// bug this pair exists for; the pixel sample is what tells the two apart.
func TestScreenshotOffscreenElementUsesPostScrollBox(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := captureFixture(t, captureGeometryFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	dpr := captureNumber(ctx, t, b, id, "window.devicePixelRatio")

	buf, meta, err := b.Screenshot(ctx, id, ShotOpts{Selector: "#far"})
	if err != nil {
		t.Fatalf("offscreen element screenshot: %v", err)
	}

	// VS-4: the reported clip is where the element ended up, read from the page
	// itself rather than from the test's expectations.
	wantY := captureNumber(ctx, t, b, id,
		"document.getElementById('far').getBoundingClientRect().top + window.scrollY")
	clip := metaClip(t, meta)
	if math.Abs(clip.Y-wantY) > 0.5 {
		t.Errorf("clip.y = %g, want %g — the box was read before the scroll, not after", clip.Y, wantY)
	}
	if clip.Y == 3000 {
		t.Error("clip.y is the element's PRE-scroll position: the rect is stale")
	}

	// VS-3: and the capture really contains the element.
	img, _ := decodeCapture(t, buf)
	cx, cy := img.Bounds().Dx()/2, img.Bounds().Dy()/2
	if !sampleIs(img, cx, cy, fixtureBlue, 8) {
		t.Errorf("centre pixel = %s, want the element's blue — a differently-placed 200x100 patch was captured",
			sampleString(img, cx, cy))
	}
	if w := img.Bounds().Dx(); w != int(math.Round(200*dpr)) {
		t.Errorf("decoded width = %d, want %g", w, 200*dpr)
	}
}

// The same stale-rect regression, made deterministic: on a BACKGROUND tab the
// page runs no rendering steps of its own, so the scroll event that moves #far
// is never dispatched at all — not late, never — until something makes the page
// render. An implementation that waits for the page to come to it therefore reads
// the pre-scroll box every single time here, where on a foreground tab it only
// does so when the machine is loaded enough to miss a frame.
//
// This is the same defect as VS-4 and the same fixture; it is a separate test
// because it fails 100% of the time rather than under contention, and because
// element capture on a tab the user is not looking at is the normal case for a
// tool that drives a real browser.
func TestScreenshotOffscreenElementOnBackgroundTab(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := captureFixture(t, captureGeometryFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	// Push the tab under test to the background by activating another one.
	other, err := b.Open(ctx, "about:blank")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := b.Raw(ctx, other["id"].(string), "Page.bringToFront", nil); err != nil {
		t.Fatalf("bringToFront on the second tab: %v", err)
	}

	buf, meta, err := b.Screenshot(ctx, id, ShotOpts{Selector: "#far"})
	if err != nil {
		t.Fatalf("element screenshot on a background tab: %v", err)
	}

	wantY := captureNumber(ctx, t, b, id,
		"document.getElementById('far').getBoundingClientRect().top + window.scrollY")
	clip := metaClip(t, meta)
	if math.Abs(clip.Y-wantY) > 0.5 {
		t.Errorf("clip.y = %g, want %g — a hidden tab's scroll was never processed before the box was read", clip.Y, wantY)
	}
	img, _ := decodeCapture(t, buf)
	cx, cy := img.Bounds().Dx()/2, img.Bounds().Dy()/2
	if !sampleIs(img, cx, cy, fixtureBlue, 8) {
		t.Errorf("centre pixel = %s, want the element's blue", sampleString(img, cx, cy))
	}
}

const captureFullPageFixture = `<!doctype html><title>Long</title>
<style>
  html, body { margin:0; background:#ffffff; }
  #tall { height: 300vh; background: linear-gradient(#fff, #ccc); }
  /* In normal flow, so it sits at the END OF THE DOCUMENT — an absolutely
     positioned "bottom:0" would only reach the bottom of the viewport. */
  #bottom { width:120px; height:80px; background:rgb(255,0,0); }
</style>
<body><div id="tall"></div><div id="bottom"></div></body>`

// VS-5: full-page capture reaches past the fold, without resizing the page
// under the capture.
func TestScreenshotFullPage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := captureFixture(t, captureFullPageFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	innerH := captureNumber(ctx, t, b, id, "window.innerHeight")
	dpr := captureNumber(ctx, t, b, id, "window.devicePixelRatio")

	buf, meta, err := b.Screenshot(ctx, id, ShotOpts{FullPage: true})
	if err != nil {
		t.Fatalf("full-page screenshot: %v", err)
	}
	if meta["mode"] != string(ShotFullPage) {
		t.Errorf("mode = %v, want full_page", meta["mode"])
	}
	if h := float64(metaInt(t, meta, "height")); h <= innerH*dpr {
		t.Errorf("height = %g, want more than the viewport's %g", h, innerH*dpr)
	}
	if clip := metaClip(t, meta); clip.Height <= innerH {
		t.Errorf("clip = %+v, want the full content height (viewport is %g)", clip, innerH)
	}

	// The block anchored to the bottom of the document is inside the capture,
	// which a viewport-only shot could never contain.
	img, _ := decodeCapture(t, buf)
	x, y := int(60*dpr), img.Bounds().Dy()-int(40*dpr)
	if !sampleIs(img, x, y, fixtureRed, 8) {
		t.Errorf("pixel near the bottom = %s, want the page-bottom marker's red", sampleString(img, x, y))
	}

	// The page must not have been left scrolled or resized by the capture.
	if got := captureNumber(ctx, t, b, id, "window.innerHeight"); got != innerH {
		t.Errorf("viewport height is %g after the capture, was %g — the page was resized under it", got, innerH)
	}
}

const capturePhotoFixture = `<!doctype html><title>Noise</title>
<style>html,body{margin:0}</style>
<body><canvas id="c" width="800" height="600"></canvas>
<script>
// Photograph-like content: smooth, continuous tone with fine detail — the case
// where PNG is several times the size of an equivalent JPEG. Deterministic, so
// the size comparison does not wander between runs.
const ctx = document.getElementById("c").getContext("2d");
const img = ctx.createImageData(800, 600);
let s = 12345;
for (let y = 0, i = 0; y < 600; y++) {
  for (let x = 0; x < 800; x++, i += 4) {
    s = (s * 1103515245 + 12345) & 0x7fffffff;
    const n = ((s >> 16) & 15) - 8;
    img.data[i]   = 128 + 100 * Math.sin(x / 37) + n;
    img.data[i+1] = 128 + 100 * Math.sin(y / 23 + x / 211) + n;
    img.data[i+2] = 128 + 100 * Math.sin((x + y) / 17) + n;
    img.data[i+3] = 255;
  }
}
ctx.putImageData(img, 0, 0);
</script></body>`

// VS-7 and the US-3 size sanity check: format selection is real, and the lossy
// artifact is dramatically smaller for photographic content.
func TestScreenshotFormatsAndSize(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := captureFixture(t, capturePhotoFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	region := &Rect{X: 0, Y: 0, Width: 800, Height: 600}
	pngBuf, pngMeta, err := b.Screenshot(ctx, id, ShotOpts{Region: region})
	if err != nil {
		t.Fatalf("png screenshot: %v", err)
	}
	jpgBuf, jpgMeta, err := b.Screenshot(ctx, id, ShotOpts{Region: region, Format: "jpeg", Quality: 50})
	if err != nil {
		t.Fatalf("jpeg screenshot: %v", err)
	}

	if pngMeta["format"] != "png" || jpgMeta["format"] != "jpeg" {
		t.Errorf("formats = %v / %v, want png / jpeg", pngMeta["format"], jpgMeta["format"])
	}
	pngImg, pngFmt := decodeCapture(t, pngBuf)
	jpgImg, jpgFmt := decodeCapture(t, jpgBuf)
	if pngFmt != "png" || jpgFmt != "jpeg" {
		t.Errorf("decoded formats = %q / %q, want png / jpeg", pngFmt, jpgFmt)
	}
	if pngImg.Bounds() != jpgImg.Bounds() {
		t.Errorf("same region decoded to %v and %v", pngImg.Bounds(), jpgImg.Bounds())
	}
	t.Logf("png %d bytes, jpeg(q50) %d bytes", len(pngBuf), len(jpgBuf))
	// Generous margin: the claim is "substantially smaller", not a fixed ratio,
	// and encoders change between Chrome versions.
	if len(jpgBuf) >= len(pngBuf)*2/3 {
		t.Errorf("jpeg is %d bytes against png's %d — expected the lossy artifact to be far smaller",
			len(jpgBuf), len(pngBuf))
	}

	// A half-scale JPEG is the artifact an agent actually wants: smaller again.
	smallBuf, _, err := b.Screenshot(ctx, id, ShotOpts{Region: region, Format: "jpeg", Quality: 50, Scale: 0.5})
	if err != nil {
		t.Fatalf("scaled jpeg screenshot: %v", err)
	}
	if len(smallBuf) >= len(jpgBuf) {
		t.Errorf("half-scale jpeg is %d bytes, full-scale %d — scale did not reach the encoder",
			len(smallBuf), len(jpgBuf))
	}
}

// The default, flagless capture is still a viewport PNG — the same call it has
// always been, now reporting what it did.
func TestScreenshotViewportDefault(t *testing.T) {
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

	srv := captureFixture(t, captureGeometryFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	innerW := captureNumber(ctx, t, b, id, "window.innerWidth")
	dpr := captureNumber(ctx, t, b, id, "window.devicePixelRatio")

	buf, meta, err := b.Screenshot(ctx, id, ShotOpts{})
	if err != nil {
		t.Fatalf("viewport screenshot: %v", err)
	}
	if meta["mode"] != string(ShotViewport) || meta["format"] != "png" {
		t.Errorf("meta = %v, want a viewport png", meta)
	}
	img, format := decodeCapture(t, buf)
	if format != "png" {
		t.Errorf("decoded format = %q, want png", format)
	}
	if w := img.Bounds().Dx(); math.Abs(float64(w)-innerW*dpr) > 2 {
		t.Errorf("decoded width = %d, want the viewport's %g", w, innerW*dpr)
	}
}

const capturePDFFixture = `<!doctype html><title>Report</title>
<style>
  @page { margin: 0; }
  .page { page-break-after: always; height: 50px; }
</style>
<body>
<div class="page">One</div>
<div class="page">Two</div>
<div class="page">Three</div>
<div class="page">Four</div>
</body>`

var mediaBoxRe = regexp.MustCompile(`/MediaBox\s*\[\s*[\d.]+\s+[\d.]+\s+([\d.]+)\s+([\d.]+)\s*\]`)

// VS-13 and VS-14: the pdf options reach Chrome's printer. Asserted by reading
// the PDF's own tokens — page count and the first MediaBox — because file size
// would "pass" for a PDF laid out entirely wrong.
func TestPDFOptions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := captureFixture(t, capturePDFFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// VS-13: A4 landscape is 841.9 x 595.3 pt — wider than tall, and A4-sized.
	buf, meta, err := b.PDF(ctx, id, PDFOpts{Landscape: true, PaperWidth: 8.27, PaperHeight: 11.69})
	if err != nil {
		t.Fatalf("pdf: %v", err)
	}
	m := mediaBoxRe.FindSubmatch(buf)
	if m == nil {
		t.Fatalf("no /MediaBox in the %d-byte PDF", len(buf))
	}
	w, h := parsePDFFloat(t, m[1]), parsePDFFloat(t, m[2])
	if w <= h {
		t.Errorf("MediaBox is %gx%g — not landscape", w, h)
	}
	if math.Abs(w-841.9) > 2 || math.Abs(h-595.3) > 2 {
		t.Errorf("MediaBox is %gx%g pt, want A4 landscape (841.9x595.3)", w, h)
	}
	pages := meta["pages"].(int)
	if pages < 4 {
		t.Fatalf("pages = %d, want the fixture's 4", pages)
	}

	// VS-14: a page range narrows the document.
	ranged, rmeta, err := b.PDF(ctx, id, PDFOpts{Pages: "1-2"})
	if err != nil {
		t.Fatalf("pdf --pages: %v", err)
	}
	if got := rmeta["pages"].(int); got != 2 {
		t.Errorf("pages = %d with --pages 1-2, want 2", got)
	}
	if got := PDFPageCount(ranged); got != 2 {
		t.Errorf("PDFPageCount = %d, want 2", got)
	}

	// Portrait letter is the default, so the options are doing the work above and
	// not merely echoing Chrome's own defaults.
	def, _, err := b.PDF(ctx, id, PDFOpts{})
	if err != nil {
		t.Fatalf("default pdf: %v", err)
	}
	if m := mediaBoxRe.FindSubmatch(def); m != nil {
		if w, h := parsePDFFloat(t, m[1]), parsePDFFloat(t, m[2]); w >= h {
			t.Errorf("default MediaBox is %gx%g, want portrait", w, h)
		}
	}
}

// PDFPageCount is a pure reader over PDF bytes, so it is table-tested without a
// browser: a capture whose page count is silently 0 is worse than one that says so.
func TestPDFPageCount(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   string
		want int
	}{
		"page tree count":      {"<< /Type /Pages /Kids [1 0 R 2 0 R] /Count 2 >>", 2},
		"count before type":    {"<< /Count 7 /Kids [] /Type /Pages >>", 7},
		"no spaces":            {"<</Type/Pages/Count 3/Kids[]>>", 3},
		"page objects only":    {"<< /Type /Page /X 1 >> << /Type /Page /X 2 >>", 2},
		"empty":                {"", 0},
		"not a pdf":            {"hello world", 0},
		"pages token only":     {"<< /Type /Pages >>", 0},
		"single page document": {"<< /Type /Pages /Count 1 >>", 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := PDFPageCount([]byte(c.in)); got != c.want {
				t.Errorf("PDFPageCount(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// padRect is the clamping arithmetic VS-2 exercises live; the edge cases are
// cheaper to pin here.
func TestPadRectClampsToPage(t *testing.T) {
	t.Parallel()
	page := Rect{X: 0, Y: 0, Width: 1000, Height: 2000}
	cases := map[string]struct {
		in   Rect
		pad  float64
		want Rect
	}{
		"no padding is identity":  {Rect{10, 20, 100, 50}, 0, Rect{10, 20, 100, 50}},
		"interior grows on all 4": {Rect{100, 100, 100, 50}, 10, Rect{90, 90, 120, 70}},
		"top-left corner clamps":  {Rect{0, 0, 100, 60}, 20, Rect{0, 0, 120, 80}},
		"bottom edge clamps":      {Rect{0, 1950, 100, 50}, 20, Rect{0, 1930, 120, 70}},
		"right edge clamps":       {Rect{950, 0, 50, 50}, 20, Rect{930, 0, 70, 70}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := padRect(c.in, c.pad, page)
			if got != c.want {
				t.Errorf("padRect(%+v, %g) = %+v, want %+v", c.in, c.pad, got, c.want)
			}
			if got.X < 0 || got.Y < 0 {
				t.Errorf("padRect produced a negative origin: %+v", got)
			}
		})
	}
}
