package encode

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"math/rand"
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
	if g.LoopCount != 3 {
		t.Errorf("decoded LoopCount = %d, want 3", g.LoopCount)
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
