package encode

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// solidPNG is a w×h frame of one colour, encoded the way a capture arrives:
// as image bytes, not as an image.Image.
func solidPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// noisyPNG is a frame no palette can flatten — the case --max-size exists for.
func noisyPNG(t *testing.T, w, h int, seed int64) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 0xFF,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func decodePNG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	return img
}

func rgbaAt(img image.Image, x, y int) color.RGBA {
	r, g, b, a := img.At(x, y).RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
}

var (
	red   = color.RGBA{R: 0xFF, A: 0xFF}
	green = color.RGBA{G: 0x80, A: 0xFF}
	blue  = color.RGBA{B: 0xFF, A: 0xFF}
)

// TestGIFRoundTripIsExact is VS-16: known solid colours survive the encode, the
// frame count matches, and the loop count is honoured.
//
// Exactness is not incidental — a frame set inside the palette limit is
// reproduced colour for colour, which is what makes every other pixel assertion
// in this file meaningful.
func TestGIFRoundTripIsExact(t *testing.T) {
	t.Parallel()
	base := time.Unix(1700000000, 0)
	frames := []Frame{
		{Data: solidPNG(t, 24, 16, red), TS: base},
		{Data: solidPNG(t, 24, 16, green), TS: base.Add(250 * time.Millisecond)},
		{Data: solidPNG(t, 24, 16, blue), TS: base.Add(500 * time.Millisecond)},
	}
	res, err := Encode(frames, Options{Format: FormatGIF, FPS: 4, Loop: 3})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if res.Frames != 3 {
		t.Errorf("result frames = %d, want 3", res.Frames)
	}
	if res.Width != 24 || res.Height != 16 {
		t.Errorf("dimensions = %dx%d, want 24x16", res.Width, res.Height)
	}
	if res.Bytes != len(res.Data) {
		t.Errorf("bytes = %d, want len(data) = %d", res.Bytes, len(res.Data))
	}

	g, err := gif.DecodeAll(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("the export does not decode as a GIF: %v", err)
	}
	if len(g.Image) != 3 {
		t.Fatalf("decoded frames = %d, want 3 (the envelope reported %d)", len(g.Image), res.Frames)
	}
	// --loop 3 means three PLAYS, which image/gif spells as LoopCount 2.
	// TestLoopPlaysExactlyNTimes owns that conversion; this only pins that the
	// flag reaches the encoder at all.
	if g.LoopCount != 2 {
		t.Errorf("decoded LoopCount = %d, want 2 for --loop 3", g.LoopCount)
	}
	for i, want := range []color.RGBA{red, green, blue} {
		if got := rgbaAt(g.Image[i], 5, 5); got != want {
			t.Errorf("frame %d pixel = %v, want %v (palette must reproduce a small colour set exactly)", i, got, want)
		}
	}
	// 250ms between captures is 25 hundredths of a second.
	if g.Delay[0] != 25 || g.Delay[1] != 25 {
		t.Errorf("delays = %v, want the timestamp-derived 25", g.Delay[:2])
	}
}

// TestLoopPlaysExactlyNTimes is the documented contract on Options.Loop: "n > 0
// plays n times".
//
// It is asserted through gif.DecodeAll rather than on the raw field, because the
// stdlib's LoopCount is not a play count — LoopCount+1 is, and LoopCount < 0 is
// the one-play case that writes no NETSCAPE block at all. Round-tripping the
// same number through both ends would agree with itself and still hand the user
// a GIF that plays one time too many.
func TestLoopPlaysExactlyNTimes(t *testing.T) {
	t.Parallel()
	frames := []Frame{
		{Data: solidPNG(t, 8, 8, red)},
		{Data: solidPNG(t, 8, 8, blue)},
	}
	// plays -> the LoopCount that produces exactly that many plays.
	cases := map[int]int{1: -1, 2: 1, 3: 2, 10: 9}
	for plays, want := range cases {
		t.Run(fmt.Sprintf("%d plays", plays), func(t *testing.T) {
			t.Parallel()
			res, err := Encode(frames, Options{Format: FormatGIF, FPS: 4, Loop: plays})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			g, err := gif.DecodeAll(bytes.NewReader(res.Data))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if g.LoopCount != want {
				t.Errorf("--loop %d encoded LoopCount = %d, want %d (the stdlib plays LoopCount+1 times)",
					plays, g.LoopCount, want)
			}
		})
	}
}

// TestLoopCountInfinite pins the default: 0 means loop forever, which is what a
// README animation needs.
func TestLoopCountInfinite(t *testing.T) {
	t.Parallel()
	frames := []Frame{
		{Data: solidPNG(t, 8, 8, red)},
		{Data: solidPNG(t, 8, 8, blue)},
	}
	res, err := Encode(frames, Options{Format: FormatGIF, FPS: 4, Loop: 0})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if g.LoopCount != 0 {
		t.Errorf("LoopCount = %d, want 0 (infinite)", g.LoopCount)
	}
}

// TestDelays covers the frame-delay computation on its own: timestamps when
// they are usable, the fps fallback when they are not, and the clamps.
func TestDelays(t *testing.T) {
	t.Parallel()
	base := time.Unix(1700000000, 0)
	at := func(ms ...int) []Frame {
		out := make([]Frame, 0, len(ms))
		for _, m := range ms {
			out = append(out, Frame{TS: base.Add(time.Duration(m) * time.Millisecond)})
		}
		return out
	}
	cases := map[string]struct {
		frames []Frame
		fps    float64
		want   []int
	}{
		"timestamps drive the delays":   {at(0, 100, 400), 4, []int{10, 30, 25}},
		"no timestamps falls back":      {[]Frame{{}, {}, {}}, 10, []int{10, 10, 10}},
		"non-monotonic falls back":      {at(500, 0, 100), 4, []int{25, 10, 25}},
		"a long pause is clamped":       {at(0, 60000), 4, []int{maxDelayCS, 25}},
		"a burst is clamped to a floor": {at(0, 1, 2), 4, []int{minDelayCS, minDelayCS, 25}},
		"a single frame is fps only":    {at(0), 2, []int{50}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := delays(c.frames, c.fps)
			if len(got) != len(c.want) {
				t.Fatalf("delays = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("delays = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// TestAnnotationIsOptIn is VS-13, and with it US-5: without --annotate the
// exported frames are the capture's pixels, unchanged.
//
// It asserts on the `frames` (PNG) export because that path is lossless, so
// "identical" can mean identical rather than "close enough after quantisation".
func TestAnnotationIsOptIn(t *testing.T) {
	t.Parallel()
	src := solidPNG(t, 40, 30, green)
	frames := []Frame{{Data: src, Marks: []Mark{{X: 20, Y: 15, Command: "click"}}}}

	res, err := Encode(frames, Options{Format: FormatFrames, FPS: 4})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(res.Files))
	}
	if res.Annotated {
		t.Error("Result.Annotated is true for an export that did not annotate")
	}
	want, got := decodePNG(t, src), decodePNG(t, res.Files[0].Data)
	for y := range 30 {
		for x := range 40 {
			if rgbaAt(got, x, y) != rgbaAt(want, x, y) {
				t.Fatalf("pixel (%d,%d) = %v, want the capture's %v — an un-annotated export must be pixel-identical",
					x, y, rgbaAt(got, x, y), rgbaAt(want, x, y))
			}
		}
	}
}

// TestAnnotationMarksTheClick is VS-12: with --annotate the frame differs from
// the raw capture AT THE MARKER and nowhere far from it. Asserted by pixel
// comparison, never by looking at it.
func TestAnnotationMarksTheClick(t *testing.T) {
	t.Parallel()
	frames := []Frame{{
		Data:  solidPNG(t, 60, 40, green),
		Marks: []Mark{{X: 30, Y: 20, Command: "click"}},
	}}
	res, err := Encode(frames, Options{Format: FormatFrames, FPS: 4, Annotate: true})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !res.Annotated {
		t.Error("Result.Annotated is false for an annotated export")
	}
	img := decodePNG(t, res.Files[0].Data)
	if got := rgbaAt(img, 30, 20); got == green {
		t.Errorf("pixel at the mark = %v, want it changed by the marker", got)
	}
	// A corner far from the mark is untouched: the marker is a marker, not a
	// filter over the whole frame.
	if got := rgbaAt(img, 1, 1); got != green {
		t.Errorf("pixel far from the mark = %v, want the capture's %v", got, green)
	}
}

// TestAnnotationScalesPageCoordinates checks the mapping that makes annotation
// land in the right place on a capture taken at --scale: the mark is in PAGE
// pixels, the frame is smaller, and the marker must follow.
func TestAnnotationScalesPageCoordinates(t *testing.T) {
	t.Parallel()
	// A 50×50 image covering a 100×100 page area: everything halves.
	frames := []Frame{{
		Data:      solidPNG(t, 50, 50, green),
		CSSWidth:  100,
		CSSHeight: 100,
		Marks:     []Mark{{X: 80, Y: 80, Command: "click"}},
	}}
	res, err := Encode(frames, Options{Format: FormatFrames, FPS: 4, Annotate: true})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	img := decodePNG(t, res.Files[0].Data)
	if got := rgbaAt(img, 40, 40); got == green {
		t.Error("the marker did not land at the scaled page coordinate (40,40)")
	}
	if got := rgbaAt(img, 5, 5); got != green {
		t.Errorf("pixel at (5,5) = %v, want the untouched %v", got, green)
	}
}

// TestMixedDimensionsAreLetterboxed: a window resized mid-recording changes the
// frame's aspect ratio, and forcing the later frames onto the first frame's
// canvas would silently distort every one of them.
//
// Padding is the honest answer — a letterboxed frame still shows what the page
// looked like, a stretched one shows something that was never on screen.
func TestMixedDimensionsAreLetterboxed(t *testing.T) {
	t.Parallel()
	frames := []Frame{
		{Data: solidPNG(t, 40, 20, green)}, // sets the canvas: 40x20
		{Data: solidPNG(t, 20, 20, red)},   // square: must not be stretched to 40 wide
	}
	res, err := Encode(frames, Options{Format: FormatFrames, FPS: 4})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if res.Width != 40 || res.Height != 20 {
		t.Fatalf("canvas = %dx%d, want the first frame's 40x20", res.Width, res.Height)
	}
	img := decodePNG(t, res.Files[1].Data)
	// The square frame fits the 20px height, so its content is 20x20 centred at
	// x = 10..29 with pad either side.
	if got := rgbaAt(img, 20, 10); got != red {
		t.Errorf("centre pixel = %v, want the frame's %v", got, red)
	}
	for _, x := range []int{0, 5, 34, 39} {
		if got := rgbaAt(img, x, 10); got == red {
			t.Errorf("pixel (%d,10) = %v: the square frame was stretched across the canvas instead of padded", x, got)
		}
	}
	// The frame that DEFINED the canvas is still exact — letterboxing must not
	// cost the common case anything (VS-13).
	first := decodePNG(t, res.Files[0].Data)
	for _, p := range [][2]int{{0, 0}, {39, 19}, {20, 10}} {
		if got := rgbaAt(first, p[0], p[1]); got != green {
			t.Errorf("first frame pixel %v = %v, want the untouched %v", p, got, green)
		}
	}
}

// TestLetterboxedAnnotationLandsOnTheContent: a mark is in PAGE coordinates, so
// once a frame is padded the marker has to follow the content, not the canvas.
func TestLetterboxedAnnotationLandsOnTheContent(t *testing.T) {
	t.Parallel()
	frames := []Frame{
		{Data: solidPNG(t, 40, 20, green)},
		{
			Data:      solidPNG(t, 20, 20, red),
			CSSWidth:  20,
			CSSHeight: 20,
			Marks:     []Mark{{X: 10, Y: 10, Command: "click"}},
		},
	}
	res, err := Encode(frames, Options{Format: FormatFrames, FPS: 4, Annotate: true})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	img := decodePNG(t, res.Files[1].Data)
	// Content occupies x = 10..29; the page's (10,10) is the content centre,
	// which is canvas (20,10).
	if got := rgbaAt(img, 20, 10); got == red {
		t.Error("the marker did not land at the letterboxed content's centre")
	}
}

// TestStrideCarriesMarksForward: dropping frames for --max-size must not drop
// the action markers with them.
//
// attachMarks goes to real trouble to guarantee a marker never disappears — one
// that has nowhere to land is indistinguishable from the click not having
// happened — and the stride would otherwise quietly undo that, leaving
// `--annotate --max-size` reporting an annotated GIF with no markers on it.
func TestStrideCarriesMarksForward(t *testing.T) {
	t.Parallel()
	frames := make([]Frame, 8)
	imgs := make([]image.Image, 8)
	for i := range frames {
		frames[i] = Frame{Data: solidPNG(t, 8, 8, green)}
		imgs[i] = image.NewRGBA(image.Rect(0, 0, 8, 8))
	}
	frames[1].Marks = []Mark{{X: 1, Y: 1, Command: "click"}}
	frames[3].Marks = []Mark{{X: 3, Y: 3, Command: "type"}}

	kept, keptImgs := stride(frames, imgs, 2)
	if len(kept) != 4 || len(keptImgs) != 4 {
		t.Fatalf("stride kept %d frames / %d images, want 4 of each", len(kept), len(keptImgs))
	}
	total := 0
	for _, f := range kept {
		total += len(f.Marks)
	}
	if total != 2 {
		t.Errorf("the kept frames carry %d marks, want both of the 2 the dropped frames held", total)
	}
	// The caller's frames are shared with the other reduction attempts, so the
	// stride must not mutate them.
	if len(frames[0].Marks) != 0 || len(frames[1].Marks) != 1 {
		t.Errorf("stride mutated its input: %v / %v", frames[0].Marks, frames[1].Marks)
	}
}

// TestAnnotatedReportsWhetherMarkersWereDrawn: `annotated: true` is a claim
// about the pixels, so an export that drew nothing must not make it.
func TestAnnotatedReportsWhetherMarkersWereDrawn(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		marks []Mark
		want  bool
	}{
		"a mark on the frame": {[]Mark{{X: 20, Y: 15}}, true},
		"no marks at all":     {nil, false},
		"a mark off-canvas":   {[]Mark{{X: 4000, Y: 3000}}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res, err := Encode([]Frame{{Data: solidPNG(t, 40, 30, green), Marks: c.marks}},
				Options{Format: FormatFrames, FPS: 4, Annotate: true})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if res.Annotated != c.want {
				t.Errorf("Annotated = %v, want %v", res.Annotated, c.want)
			}
		})
	}
}

// TestMaxSizeReductionTerminates is the property that matters about the
// --max-size loop: it always stops, it reports the values it actually used, and
// it says so when the ceiling could not be met rather than pretending.
func TestMaxSizeReductionTerminates(t *testing.T) {
	t.Parallel()
	frames := make([]Frame, 0, 6)
	base := time.Unix(1700000000, 0)
	for i := range 6 {
		frames = append(frames, Frame{
			Data: noisyPNG(t, 200, 160, int64(i+1)),
			TS:   base.Add(time.Duration(i) * 250 * time.Millisecond),
		})
	}
	full, err := Encode(frames, Options{Format: FormatGIF, FPS: 4})
	if err != nil {
		t.Fatalf("Encode (unbounded): %v", err)
	}
	if full.Reduced || full.Scale != 1 {
		t.Fatalf("an export with no --max-size must not reduce: scale=%v reduced=%v", full.Scale, full.Reduced)
	}

	t.Run("a reachable ceiling is met", func(t *testing.T) {
		t.Parallel()
		ceiling := full.Bytes / 3
		res, err := Encode(frames, Options{Format: FormatGIF, FPS: 4, MaxBytes: ceiling})
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if !res.WithinMaxSize || res.Bytes > ceiling {
			t.Errorf("bytes = %d, want <= %d (WithinMaxSize=%v)", res.Bytes, ceiling, res.WithinMaxSize)
		}
		if !res.Reduced || res.Scale >= 1 {
			t.Errorf("scale = %v, reduced = %v; want a reported reduction", res.Scale, res.Reduced)
		}
		if res.Width >= full.Width {
			t.Errorf("width = %d, want smaller than the unbounded %d", res.Width, full.Width)
		}
		if _, err := gif.DecodeAll(bytes.NewReader(res.Data)); err != nil {
			t.Errorf("the reduced export does not decode: %v", err)
		}
	})

	t.Run("an impossible ceiling stops and says so", func(t *testing.T) {
		t.Parallel()
		res, err := Encode(frames, Options{Format: FormatGIF, FPS: 4, MaxBytes: 1})
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if res.WithinMaxSize {
			t.Error("WithinMaxSize is true for a 1-byte ceiling")
		}
		if res.Scale < minScaleFactor {
			t.Errorf("scale = %v, want it floored at %v rather than reduced to nothing", res.Scale, minScaleFactor)
		}
		if res.Frames < 2 {
			t.Errorf("frames = %d, want at least 2 kept", res.Frames)
		}
		if _, err := gif.DecodeAll(bytes.NewReader(res.Data)); err != nil {
			t.Errorf("the best-effort export does not decode: %v", err)
		}
	})
}

// TestPlanNextStrictlyShrinks is the termination argument as a test: every step
// is strictly smaller than the last, and the ladder eventually refuses.
func TestPlanNextStrictlyShrinks(t *testing.T) {
	t.Parallel()
	first := image.NewRGBA(image.Rect(0, 0, 400, 300))
	p := plan{scale: 1, stride: 1}
	for i := range 64 {
		next, ok := p.next(1<<20, 1, first, 8)
		if !ok {
			if i == 0 {
				t.Fatal("the reduction ladder refused on its first step")
			}
			return
		}
		if next.scale > p.scale || (next.scale == p.scale && next.stride <= p.stride) {
			t.Fatalf("step %d did not shrink: %+v -> %+v", i, p, next)
		}
		p = next
	}
	t.Fatalf("the reduction ladder never refused (ended at %+v)", p)
}

// TestFramesExportIsNumbered is VS-11's pure half: one file per frame, named in
// order, each a decodable PNG.
func TestFramesExportIsNumbered(t *testing.T) {
	t.Parallel()
	frames := []Frame{
		{Data: solidPNG(t, 10, 10, red)},
		{Data: solidPNG(t, 10, 10, green)},
		{Data: solidPNG(t, 10, 10, blue)},
	}
	res, err := Encode(frames, Options{Format: FormatFrames, FPS: 4})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(res.Files) != res.Frames || res.Frames != 3 {
		t.Fatalf("files = %d, frames = %d, want 3 of each", len(res.Files), res.Frames)
	}
	if res.Data != nil {
		t.Error("a frames export must not also carry a single blob")
	}
	want := []string{"frame-00001.png", "frame-00002.png", "frame-00003.png"}
	for i, f := range res.Files {
		if f.Name != want[i] {
			t.Errorf("file %d name = %q, want %q", i, f.Name, want[i])
		}
		if _, err := png.Decode(bytes.NewReader(f.Data)); err != nil {
			t.Errorf("file %d does not decode as PNG: %v", i, err)
		}
	}
}

// TestManyColoursStillEncodes exercises the non-exact palette regime: a frame
// set well past 256 colours must still produce a decodable GIF of the right
// size, approximately matching the source.
func TestManyColoursStillEncodes(t *testing.T) {
	t.Parallel()
	frames := []Frame{
		{Data: noisyPNG(t, 64, 48, 1)},
		{Data: noisyPNG(t, 64, 48, 2)},
	}
	res, err := Encode(frames, Options{Format: FormatGIF, FPS: 4})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(g.Image) != 2 {
		t.Fatalf("decoded frames = %d, want 2", len(g.Image))
	}
	if b := g.Image[0].Bounds(); b.Dx() != 64 || b.Dy() != 48 {
		t.Errorf("frame bounds = %v, want 64x48", b)
	}
}

// TestVideoGeometryReportsWhatFFmpegProduces is the pure half of "the envelope
// must describe the file, not the request".
//
// Two things ffmpeg imposes and the result used to ignore: yuv420p needs even
// dimensions (encodeVideo passes -vf scale=trunc(iw/2)*2:trunc(ih/2)*2, and
// --max-size lands on an odd canvas about half the time), and -framerate is
// floored at 1 — which any recording with pauses in it hits, since that is the
// normal shape of a screencast.
func TestVideoGeometryReportsWhatFFmpegProduces(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		w, h      int
		fps       float64
		frames    int
		wantW     int
		wantH     int
		wantFPS   float64
		wantDurMs int64
	}{
		"even canvas is unchanged": {100, 60, 4, 8, 100, 60, 4, 2000},
		"odd canvas rounds down":   {101, 61, 4, 8, 100, 60, 4, 2000},
		"a slow recording clamps":  {100, 60, 0.25, 5, 100, 60, 1, 5000},
		"exactly 1fps":             {100, 60, 1, 3, 100, 60, 1, 3000},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w, h, fps, dur := videoGeometry(c.w, c.h, c.fps, c.frames)
			if w != c.wantW || h != c.wantH || fps != c.wantFPS || dur != c.wantDurMs {
				t.Errorf("videoGeometry(%d,%d,%v,%d) = %d,%d,%v,%d; want %d,%d,%v,%d",
					c.w, c.h, c.fps, c.frames, w, h, fps, dur, c.wantW, c.wantH, c.wantFPS, c.wantDurMs)
			}
		})
	}
}

// TestVideoResultMatchesTheFile is the same claim checked against ffprobe, which
// is the only authority on what was written. Skipped where ffmpeg is absent —
// the geometry itself is covered above without it.
func TestVideoResultMatchesTheFile(t *testing.T) {
	t.Parallel()
	if err := Available(FormatMP4); err != nil {
		t.Skipf("no ffmpeg: %v", err)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffprobe to check the file with")
	}
	// An odd canvas, and frames 5s apart so the average rate is well under 1fps.
	base := time.Unix(1700000000, 0)
	frames := make([]Frame, 0, 5)
	for i := range 5 {
		frames = append(frames, Frame{
			Data: solidPNG(t, 101, 61, color.RGBA{R: uint8(30 * i), G: 0x40, B: 0x80, A: 0xFF}),
			TS:   base.Add(time.Duration(i) * 5 * time.Second),
		})
	}
	res, err := Encode(frames, Options{Format: FormatMP4, FPS: 4})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "out.mp4")
	if err := os.WriteFile(path, res.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height,r_frame_rate,nb_frames",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		t.Fatalf("ffprobe said %q", out)
	}
	gotW, gotH := fields[0], fields[1]
	if gotW != strconv.Itoa(res.Width) || gotH != strconv.Itoa(res.Height) {
		t.Errorf("the file is %sx%s, the envelope says %dx%d", gotW, gotH, res.Width, res.Height)
	}
	if fields[2] != "1/1" {
		t.Errorf("ffprobe frame rate = %s, want 1/1 (the -framerate floor)", fields[2])
	}
	if res.FPS != 1 {
		t.Errorf("Result.FPS = %v, want the 1 the file was written at", res.FPS)
	}
	if res.DurationMs != 5000 {
		t.Errorf("Result.DurationMs = %d, want 5000 (5 frames at 1fps)", res.DurationMs)
	}
}

func TestEncodeRejectsNoFrames(t *testing.T) {
	t.Parallel()
	if _, err := Encode(nil, Options{Format: FormatGIF}); !errors.Is(err, ErrNoFrames) {
		t.Errorf("Encode(nil) error = %v, want ErrNoFrames", err)
	}
}

func TestParseFormat(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in      string
		want    Format
		wantErr bool
	}{
		"gif":            {"gif", FormatGIF, false},
		"upper":          {"GIF", FormatGIF, false},
		"padded":         {" webm ", FormatWebM, false},
		"mp4":            {"mp4", FormatMP4, false},
		"frames":         {"frames", FormatFrames, false},
		"unknown":        {"avi", "", true},
		"empty":          {"", "", true},
		"not an ext":     {".gif", "", true},
		"nearly a match": {"gifv", "", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseFormat(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("ParseFormat(%q) error = %v, wantErr = %v", c.in, err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("ParseFormat(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestFormatFromPath(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		path string
		want Format
		ok   bool
	}{
		"gif":            {"demo.gif", FormatGIF, true},
		"upper ext":      {"DEMO.GIF", FormatGIF, true},
		"mp4":            {"/tmp/run.mp4", FormatMP4, true},
		"webm":           {"./out/run.webm", FormatWebM, true},
		"no extension":   {"outdir", "", false},
		"unknown":        {"run.mov", "", false},
		"directory-ish":  {"./frames/", "", false},
		"png is a frame": {"shot.png", "", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := FormatFromPath(c.path)
			if ok != c.ok || (ok && got != c.want) {
				t.Errorf("FormatFromPath(%q) = %q, %v; want %q, %v", c.path, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestMissingEncoderNamesTheRequirement is VS-10's pure half: with no ffmpeg on
// PATH the video formats fail with a message that says what to install, and
// they fail from Available — before any caller has drained a recording.
//
// Not parallel: it rewrites PATH for the process.
func TestMissingEncoderNamesTheRequirement(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, f := range []Format{FormatMP4, FormatWebM} {
		err := Available(f)
		if !IsNoEncoder(err) {
			t.Fatalf("Available(%q) = %v, want an ErrNoEncoder", f, err)
		}
		if !strings.Contains(err.Error(), "ffmpeg") {
			t.Errorf("Available(%q) error %q does not name ffmpeg", f, err)
		}
		if _, eerr := Encode([]Frame{{Data: solidPNG(t, 8, 8, red)}}, Options{Format: f, FPS: 4}); !IsNoEncoder(eerr) {
			t.Errorf("Encode(%q) = %v, want an ErrNoEncoder", f, eerr)
		}
	}
	// The dependency-free formats are unaffected — that is the whole point of
	// making GIF the default.
	for _, f := range []Format{FormatGIF, FormatFrames} {
		if err := Available(f); err != nil {
			t.Errorf("Available(%q) = %v, want nil with no ffmpeg installed", f, err)
		}
	}
}
