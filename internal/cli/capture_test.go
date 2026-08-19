package cli

// Stub-backed tests for the capture verbs (RFC-0008): screenshot and pdf.
//
// The VS-12 group is a regression guard on the contract that existed BEFORE the
// options landed — a no-flag viewport PNG, `-o -` writing raw bytes to stdout,
// and a default filename that never overwrites. They are written against
// behaviour, not against the stub's payload, so they hold across the signature
// change and the feature work on top of it.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// captureFake returns known artifact bytes for a single tab and records the
// options it was handed, so a test can assert both that exactly the driver's
// bytes reached the file or stdout and that the parsed flags reached the driver.
type captureFake struct {
	stubBrowser
	data []byte
	meta map[string]any

	shot     chrome.ShotOpts
	pdf      chrome.PDFOpts
	shotCall int
	pdfCall  int
}

func newCaptureFake(data []byte) *captureFake { return &captureFake{data: data} }

func (f *captureFake) List(context.Context) ([]target.Info, error) {
	return []target.Info{{ID: "aa11", Title: "A", URL: "u"}}, nil
}

func (f *captureFake) Screenshot(_ context.Context, _ string, opts chrome.ShotOpts) ([]byte, map[string]any, error) {
	f.shot, f.shotCall = opts, f.shotCall+1
	return f.data, f.meta, nil
}

func (f *captureFake) PDF(_ context.Context, _ string, opts chrome.PDFOpts) ([]byte, map[string]any, error) {
	f.pdf, f.pdfCall = opts, f.pdfCall+1
	return f.data, f.meta, nil
}

var _ chrome.Browser = (*captureFake)(nil)

// VS-12: `screenshot` with no flags still writes a viewport PNG, and the
// envelope still reports the path and byte count.
func TestScreenshotNoFlagsWritesPNG(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "out.png")
	b := newCaptureFake([]byte("PNGBYTES"))
	env, _, code := run(t, b, "screenshot", "-o", p, "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	res, _ := env["result"].(map[string]any)
	if res["path"] != p {
		t.Errorf("result.path = %v, want %q", res["path"], p)
	}
	if res["bytes"] != float64(len(b.data)) {
		t.Errorf("result.bytes = %v, want %d", res["bytes"], len(b.data))
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if !bytes.Equal(got, b.data) {
		t.Errorf("file = %q, want the driver's bytes %q", got, b.data)
	}
}

// VS-12: `-o -` still writes the raw image bytes to stdout — and nothing else,
// so a caller can pipe it straight into a viewer.
func TestScreenshotToStdoutWritesRawBytes(t *testing.T) {
	t.Parallel()
	b := newCaptureFake([]byte("PNGBYTES"))
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	if code := app.Execute("screenshot", "-o", "-", "--target", "aa11"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !bytes.Equal(out.Bytes(), b.data) {
		t.Errorf("stdout = %q, want exactly the driver's bytes %q", out.Bytes(), b.data)
	}
}

// VS-12: the default filename never overwrites an earlier capture — the
// collision counter is what keeps a loop of screenshots from producing one file.
func TestScreenshotDefaultNameNeverOverwrites(t *testing.T) {
	t.Chdir(t.TempDir()) // implies non-parallel

	first := captureDefaultPath(t)
	second := captureDefaultPath(t)
	if first == second {
		t.Fatalf("both captures wrote %q — the second overwrote the first", first)
	}
	// Same second, so the counter (not the timestamp) is what separated them.
	if base := strings.TrimSuffix(first, ".png"); second == base+"-1.png" {
		return
	}
	// A second boundary fell between the two runs; the timestamps differ, which
	// is the other half of the same contract. Both files must still exist.
	for _, p := range []string{first, second} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%q is missing: %v", p, err)
		}
	}
}

// captureDefaultPath runs `screenshot` with no -o and returns the path it chose.
func captureDefaultPath(t *testing.T) string {
	t.Helper()
	env, _, code := run(t, newCaptureFake([]byte("PNGBYTES")), "screenshot", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	p, _ := env["result"].(map[string]any)["path"].(string)
	if p == "" {
		t.Fatalf("no result.path in %v", env)
	}
	return p
}

// TestUniquePath pins the collision counter itself: the pure function the
// default-name contract rests on.
func TestUniquePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "shot.png")

	// free path returns unchanged
	if got := uniquePath(p); got != p {
		t.Fatalf("free path: got %q, want %q", got, p)
	}

	// on collision, a counter is inserted before the extension
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := uniquePath(p)
	if want := filepath.Join(dir, "shot-1.png"); got != want {
		t.Errorf("first collision: got %q, want %q", got, want)
	}

	if err := os.WriteFile(got, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got2, want := uniquePath(p), filepath.Join(dir, "shot-2.png"); got2 != want {
		t.Errorf("second collision: got %q, want %q", got2, want)
	}
}

// VS-9: the three capture modes are mutually exclusive. Each alone is accepted;
// any two together is a usage error raised BEFORE the browser is contacted,
// which is what noCall proves — asserting only the exit code would also pass for
// a command that connected first and validated afterwards.
func TestScreenshotModesAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	alone := map[string][]string{
		"selector":  {"--selector", "#box"},
		"full page": {"--full-page"},
		"region":    {"--region", "0,0,10,10"},
	}
	for name, args := range alone {
		t.Run("alone/"+name, func(t *testing.T) {
			t.Parallel()
			b := newCaptureFake([]byte("X"))
			argv := append([]string{"screenshot", "-o", filepath.Join(t.TempDir(), "o.png"), "--target", "aa11", "--json"}, args...)
			_, _, code := run(t, b, argv...)
			if code != 0 {
				t.Errorf("exit = %d, want 0 for %s alone", code, name)
			}
			if b.shotCall != 1 {
				t.Errorf("driver called %d times, want 1", b.shotCall)
			}
		})
	}

	pairs := map[string][]string{
		"selector + full page": {"--selector", "#box", "--full-page"},
		"selector + region":    {"--selector", "#box", "--region", "0,0,10,10"},
		"full page + region":   {"--full-page", "--region", "0,0,10,10"},
		"all three":            {"--selector", "#box", "--full-page", "--region", "0,0,10,10"},
	}
	for name, args := range pairs {
		t.Run("together/"+name, func(t *testing.T) {
			t.Parallel()
			argv := append([]string{"screenshot", "--target", "aa11", "--json"}, args...)
			env, _, code := run(t, noCall(t), argv...)
			if code != 2 {
				t.Errorf("exit = %d, want 2 (usage) for %s", code, name)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("error.code = %v, want usage", env["error"])
			}
		})
	}
}

// VS-10: malformed and out-of-range values are usage errors, and none of them
// touches Chrome.
func TestScreenshotBadValuesAreUsageErrors(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"region with three values":  {"--region", "1,2,3"},
		"region with five values":   {"--region", "1,2,3,4,5"},
		"region that is not number": {"--region", "a,b,c,d"},
		"region negative width":     {"--region", "0,0,-5,10"},
		"region zero height":        {"--region", "0,0,5,0"},
		"region negative origin":    {"--region", "-1,0,5,5"},
		"region empty":              {"--region", ","},
		"quality above range":       {"--format", "jpeg", "--quality", "200"},
		"quality below range":       {"--format", "jpeg", "--quality", "-1"},
		"quality with png":          {"--quality", "60"},
		"quality with explicit png": {"--format", "png", "--quality", "60"},
		"scale zero":                {"--scale", "0"},
		"scale too large":           {"--scale", "9"},
		"scale negative":            {"--scale", "-1"},
		"unknown format":            {"--format", "gif"},
		"negative padding":          {"--padding", "-4"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			argv := append([]string{"screenshot", "--target", "aa11", "--json"}, args...)
			env, _, code := run(t, noCall(t), argv...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage)", code)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("error.code = %v, want usage", env["error"])
			}
		})
	}
}

// --quality is legal for the lossy formats, and only for those.
func TestScreenshotQualityIsAcceptedForLossyFormats(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"jpeg", "webp"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			b := newCaptureFake([]byte("X"))
			_, _, code := run(t, b, "screenshot", "--format", format, "--quality", "40",
				"-o", filepath.Join(t.TempDir(), "o.img"), "--target", "aa11", "--json")
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if b.shot.Format != format || b.shot.Quality != 40 {
				t.Errorf("driver got format %q quality %d, want %q / 40", b.shot.Format, b.shot.Quality, format)
			}
		})
	}
}

// The parsed flags reach the driver as the driver's own types — the CLI is where
// every spelling is resolved, so nothing downstream has to re-parse.
func TestScreenshotFlagsReachTheDriver(t *testing.T) {
	t.Parallel()
	b := newCaptureFake([]byte("X"))
	_, _, code := run(t, b, "screenshot", "--region", "10,20,400,300", "--scale", "0.5", "--padding", "8",
		"--format", "webp", "--quality", "70", "-o", filepath.Join(t.TempDir(), "o.webp"), "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	want := chrome.Rect{X: 10, Y: 20, Width: 400, Height: 300}
	if b.shot.Region == nil || *b.shot.Region != want {
		t.Errorf("Region = %v, want %+v", b.shot.Region, want)
	}
	if b.shot.Scale != 0.5 || b.shot.Padding != 8 || b.shot.Format != "webp" || b.shot.Quality != 70 {
		t.Errorf("opts = %+v, want scale 0.5, padding 8, webp, quality 70", b.shot)
	}
}

// --selector inherits the shared addressing flags rather than inventing its own.
func TestScreenshotSelectorUsesSharedQueryFlags(t *testing.T) {
	t.Parallel()
	b := newCaptureFake([]byte("X"))
	_, _, code := run(t, b, "screenshot", "--selector", "Summary card", "--by", "name", "--role", "region",
		"--nth", "2", "--pierce", "-o", filepath.Join(t.TempDir(), "o.png"), "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.shot.Selector != "Summary card" {
		t.Errorf("Selector = %q", b.shot.Selector)
	}
	q := b.shot.Query
	if q.By != "name" || q.Role != "region" || q.Nth != 2 || !q.Pierce {
		t.Errorf("QueryOpts = %+v, want the shared --by/--role/--nth/--pierce flags", q)
	}
}

// VS-7 (CLI half) and US-7: the driver's metadata is merged into the envelope,
// and the default filename's extension follows the format.
func TestScreenshotEnvelopeCarriesMetadata(t *testing.T) {
	t.Chdir(t.TempDir()) // the default filename is written to cwd

	b := newCaptureFake([]byte("IMAGE"))
	b.meta = map[string]any{
		"width": 812, "height": 344, "format": "jpeg", "scale": 1.0, "mode": "element",
		"clip": chrome.Rect{X: 210, Y: 940, Width: 812, Height: 344},
	}
	env, _, code := run(t, b, "screenshot", "--selector", "#x", "--format", "jpeg", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	res := env["result"].(map[string]any)
	for k, want := range map[string]any{
		"bytes": float64(5), "width": float64(812), "height": float64(344),
		"format": "jpeg", "mode": "element", "scale": float64(1),
	} {
		if res[k] != want {
			t.Errorf("result.%s = %v, want %v", k, res[k], want)
		}
	}
	clip, ok := res["clip"].(map[string]any)
	if !ok || clip["x"] != float64(210) || clip["width"] != float64(812) {
		t.Errorf("result.clip = %v, want the resolved rectangle", res["clip"])
	}
	if path, _ := res["path"].(string); !strings.HasSuffix(path, ".jpg") {
		t.Errorf("path = %q, want the default name to take the jpeg extension", path)
	}
}

// RFC-0016 VS-8: --annotate sets ShotOpts.Annotate, and the stub's meta map
// (the shape the driver returns once it implements the annotation pass)
// reaches the envelope unchanged — the CLI does no interpretation of it.
func TestScreenshotAnnotateFlagAndLegend(t *testing.T) {
	t.Parallel()
	b := newCaptureFake([]byte("IMAGE"))
	b.meta = map[string]any{
		"width": 100, "height": 100, "format": "png", "scale": 1.0, "mode": "viewport",
		"clip":      chrome.Rect{X: 0, Y: 0, Width: 100, Height: 100},
		"annotated": true, "truncated": false,
		"annotations": []any{
			map[string]any{"n": 1.0, "ref": "e41", "role": "button", "name": "Save",
				"center": map[string]any{"x": 50.0, "y": 50.0}},
		},
	}
	env, _, code := run(t, b, "screenshot", "--annotate", "-o", filepath.Join(t.TempDir(), "o.png"), "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !b.shot.Annotate {
		t.Error("ShotOpts.Annotate did not reach the driver")
	}
	res := env["result"].(map[string]any)
	if res["annotated"] != true {
		t.Errorf("result.annotated = %v, want true", res["annotated"])
	}
	arr, ok := res["annotations"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("result.annotations = %v, want a one-entry legend", res["annotations"])
	}
	entry := arr[0].(map[string]any)
	if entry["ref"] != "e41" || entry["role"] != "button" || entry["name"] != "Save" {
		t.Errorf("legend entry = %v, want the stub's e41/button/Save", entry)
	}
}

// RFC-0016 VS-9: without --annotate, ShotOpts.Annotate is false and the
// envelope carries none of annotated/annotations/truncated/reason — the plain
// capture is byte-for-byte and shape-for-shape what it was before this RFC.
func TestScreenshotWithoutAnnotateFlagOmitsLegendFields(t *testing.T) {
	t.Parallel()
	b := newCaptureFake([]byte("IMAGE"))
	b.meta = map[string]any{"width": 100, "height": 100, "format": "png", "scale": 1.0, "mode": "viewport",
		"clip": chrome.Rect{X: 0, Y: 0, Width: 100, Height: 100}}
	env, _, code := run(t, b, "screenshot", "-o", filepath.Join(t.TempDir(), "o.png"), "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.shot.Annotate {
		t.Error("ShotOpts.Annotate is true without --annotate")
	}
	res := env["result"].(map[string]any)
	for _, k := range []string{"annotated", "annotations", "truncated", "reason"} {
		if _, present := res[k]; present {
			t.Errorf("result.%s is present without --annotate: %v", k, res[k])
		}
	}
}

// RFC-0016 VS-7: the two combinations --annotate cannot honour are usage
// errors raised before the browser is contacted.
func TestScreenshotAnnotateRejectedCombos(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"annotate with webp": {"--annotate", "--format", "webp"},
		"annotate to stdout": {"--annotate", "-o", "-"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			argv := append([]string{"screenshot", "--target", "aa11", "--json"}, args...)
			env, _, code := run(t, noCall(t), argv...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage)", code)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("error.code = %v, want usage", env["error"])
			}
		})
	}
}

// RFC-0016 VS-7: --annotate composes with jpeg (the only lossy format it
// supports).
func TestScreenshotAnnotateAcceptsJPEG(t *testing.T) {
	t.Parallel()
	b := newCaptureFake([]byte("IMAGE"))
	b.meta = map[string]any{"format": "jpeg", "annotated": true, "annotations": []any{}, "truncated": false}
	env, _, code := run(t, b, "screenshot", "--annotate", "--format", "jpeg", "--quality", "60",
		"-o", filepath.Join(t.TempDir(), "o.jpg"), "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !b.shot.Annotate || b.shot.Format != "jpeg" || b.shot.Quality != 60 {
		t.Errorf("opts = %+v, want annotate + jpeg + quality 60", b.shot)
	}
	if env["result"].(map[string]any)["format"] != "jpeg" {
		t.Errorf("result.format = %v, want jpeg", env["result"])
	}
}

// The default filename's extension follows --format for every format.
func TestScreenshotDefaultExtensionFollowsFormat(t *testing.T) {
	cases := map[string]string{"png": ".png", "jpeg": ".jpg", "webp": ".webp"}
	for format, ext := range cases {
		t.Run(format, func(t *testing.T) {
			t.Chdir(t.TempDir())
			env, _, code := run(t, newCaptureFake([]byte("X")), "screenshot", "--format", format, "--target", "aa11", "--json")
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if path, _ := env["result"].(map[string]any)["path"].(string); !strings.HasSuffix(path, ext) {
				t.Errorf("path = %q, want the %s extension", path, ext)
			}
		})
	}
}

// VS-11 (CLI half): a zero-area element is exit 4 with `zero_area`, not a bare
// timeout — the selector was right, so "fix your selector" would be wrong advice.
func TestScreenshotZeroAreaIsTargetErrorWithDetail(t *testing.T) {
	t.Parallel()
	b := &zeroAreaBrowser{}
	env, _, code := run(t, b, "screenshot", "--selector", "#hidden", "--target", "aa11", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (target)", code)
	}
	e := env["error"].(map[string]any)
	if e["code"] != "target_timeout" {
		t.Errorf("error.code = %v, want target_timeout", e["code"])
	}
	// Details are flattened into the error object (see result.Err.MarshalJSON).
	if e["zero_area"] != true {
		t.Errorf("error = %v, want zero_area: true alongside the code", e)
	}
}

// zeroAreaBrowser fails a capture the way a display:none element does.
type zeroAreaBrowser struct {
	stubBrowser
}

func (zeroAreaBrowser) List(context.Context) ([]target.Info, error) {
	return []target.Info{{ID: "aa11", Title: "A", URL: "u"}}, nil
}

func (zeroAreaBrowser) Screenshot(context.Context, string, chrome.ShotOpts) ([]byte, map[string]any, error) {
	return nil, nil, chrome.ErrZeroArea
}

// The pdf flags are parsed into numbers here, so the driver never sees a paper
// name, a margin unit, or an unvalidated range.
func TestPDFFlagsReachTheDriverAsNumbers(t *testing.T) {
	t.Parallel()
	b := newCaptureFake([]byte("%PDF-"))
	_, _, code := run(t, b, "pdf", "--landscape", "--paper", "A4", "--margin", "1cm", "--background",
		"--pages", "1-3,5", "--scale", "0.8", "--footer", "<span></span>",
		"-o", filepath.Join(t.TempDir(), "o.pdf"), "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got := b.pdf
	if !got.Landscape || !got.Background {
		t.Errorf("opts = %+v, want landscape and background set", got)
	}
	if got.PaperWidth != 8.27 || got.PaperHeight != 11.69 {
		t.Errorf("paper = %gx%g, want A4 in inches (case-insensitively)", got.PaperWidth, got.PaperHeight)
	}
	if diff := got.MarginTop - 1/2.54; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("MarginTop = %g, want 1cm in inches", got.MarginTop)
	}
	if got.MarginTop != got.MarginLeft || got.MarginTop != got.MarginRight || got.MarginTop != got.MarginBottom {
		t.Errorf("margins = %+v, want one value applied to all four sides", got)
	}
	if got.Pages != "1-3,5" || got.Scale != 0.8 || got.Footer != "<span></span>" {
		t.Errorf("opts = %+v, want the ranges, scale and footer forwarded", got)
	}
}

// pdf's own bad values are usage errors, raised before connecting.
func TestPDFBadValuesAreUsageErrors(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"unknown paper":        {"--paper", "foolscap"},
		"paper with bad dims":  {"--paper", "8.5xzz"},
		"paper zero dimension": {"--paper", "0x11"},
		"two margins":          {"--margin", "1,2"},
		"three margins":        {"--margin", "1,2,3"},
		"margin not a length":  {"--margin", "wide"},
		"negative margin":      {"--margin", "-1in"},
		"margin bad unit":      {"--margin", "3furlongs"},
		"pages not a number":   {"--pages", "one"},
		"pages zero":           {"--pages", "0-2"},
		"pages reversed":       {"--pages", "5-2"},
		"pages empty range":    {"--pages", "1,,3"},
		"pages trailing comma": {"--pages", "1,"},
		"scale zero":           {"--scale", "0"},
		"scale too large":      {"--scale", "5"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			argv := append([]string{"pdf", "--target", "aa11", "--json"}, args...)
			env, _, code := run(t, noCall(t), argv...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage)", code)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("error.code = %v, want usage", env["error"])
			}
		})
	}
}

// The pdf envelope reports the page count alongside path and bytes.
func TestPDFEnvelopeReportsPages(t *testing.T) {
	t.Parallel()
	b := newCaptureFake([]byte("%PDF-"))
	b.meta = map[string]any{"pages": 4}
	p := filepath.Join(t.TempDir(), "out.pdf")
	env, _, code := run(t, b, "pdf", "-o", p, "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	res := env["result"].(map[string]any)
	if res["pages"] != float64(4) || res["path"] != p {
		t.Errorf("result = %v, want pages 4 at %q", res, p)
	}
}

// parsePaper: every named size, case-insensitively, plus the WxH inches form.
func TestParsePaper(t *testing.T) {
	t.Parallel()
	ok := map[string][2]float64{
		"letter":  {8.5, 11},
		"LETTER":  {8.5, 11},
		"Legal":   {8.5, 14},
		"tabloid": {11, 17},
		"ledger":  {17, 11},
		"a0":      {33.11, 46.81},
		"A1":      {23.39, 33.11},
		"a2":      {16.54, 23.39},
		"a3":      {11.69, 16.54},
		"A4":      {8.27, 11.69},
		"a5":      {5.83, 8.27},
		"a6":      {4.13, 5.83},
		"8.5x11":  {8.5, 11},
		"11X17":   {11, 17},
		" a4 ":    {8.27, 11.69},
	}
	for in, want := range ok {
		t.Run("ok/"+in, func(t *testing.T) {
			t.Parallel()
			w, h, err := parsePaper(in)
			if err != nil {
				t.Fatalf("parsePaper(%q): %v", in, err)
			}
			if w != want[0] || h != want[1] {
				t.Errorf("parsePaper(%q) = %gx%g, want %gx%g", in, w, h, want[0], want[1])
			}
		})
	}
	for _, in := range []string{"foolscap", "a7", "8.5x", "x11", "0x11", "8.5x0", "-1x5", "axb"} {
		t.Run("rejected/"+in, func(t *testing.T) {
			t.Parallel()
			if _, _, err := parsePaper(in); err == nil {
				t.Errorf("parsePaper(%q) was accepted, want a usage error", in)
			}
		})
	}
}

// parseMargins: the one-value and four-value forms, and the units.
func TestParseMargins(t *testing.T) {
	t.Parallel()
	cases := map[string][4]float64{
		"0.4in":             {0.4, 0.4, 0.4, 0.4},
		"0.4":               {0.4, 0.4, 0.4, 0.4},
		"0":                 {0, 0, 0, 0},
		"1,2,3,4":           {1, 2, 3, 4},
		"1in, 2in, 3in,4in": {1, 2, 3, 4},
		"96px":              {1, 1, 1, 1},
		"72pt":              {1, 1, 1, 1},
		"2.54cm":            {1, 1, 1, 1},
		"25.4mm":            {1, 1, 1, 1},
	}
	for in, want := range cases {
		t.Run("ok/"+in, func(t *testing.T) {
			t.Parallel()
			got, err := parseMargins(in)
			if err != nil {
				t.Fatalf("parseMargins(%q): %v", in, err)
			}
			for i := range want {
				if d := got[i] - want[i]; d > 1e-9 || d < -1e-9 {
					t.Errorf("parseMargins(%q) = %v, want %v", in, got, want)
					break
				}
			}
		})
	}
	for _, in := range []string{"", "1,2", "1,2,3", "1,2,3,4,5", "wide", "-1", "1 in 2", "3furlongs"} {
		t.Run("rejected/"+in, func(t *testing.T) {
			t.Parallel()
			if _, err := parseMargins(in); err == nil {
				t.Errorf("parseMargins(%q) was accepted, want a usage error", in)
			}
		})
	}
}

// validatePageRanges: the `1-3,5` grammar.
func TestValidatePageRanges(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "1", "1-3", "1-3,5", "2, 4 , 6-9", "10-10"} {
		t.Run("ok/"+in, func(t *testing.T) {
			t.Parallel()
			if err := validatePageRanges(in); err != nil {
				t.Errorf("validatePageRanges(%q): %v", in, err)
			}
		})
	}
	for _, in := range []string{"0", "-1", "1-", "-3", "a", "1,,2", "1,", "5-2", "1.5", "1-2-3"} {
		t.Run("rejected/"+in, func(t *testing.T) {
			t.Parallel()
			if err := validatePageRanges(in); err == nil {
				t.Errorf("validatePageRanges(%q) was accepted, want a usage error", in)
			}
		})
	}
}

// parseRegion: the rectangle grammar, including the values that must be refused.
func TestParseRegion(t *testing.T) {
	t.Parallel()
	got, err := parseRegion(" 10, 20 ,400,300 ")
	if err != nil {
		t.Fatalf("parseRegion: %v", err)
	}
	if want := (chrome.Rect{X: 10, Y: 20, Width: 400, Height: 300}); *got != want {
		t.Errorf("parseRegion = %+v, want %+v", *got, want)
	}
	for _, in := range []string{"", "1,2,3", "1,2,3,4,5", "a,b,c,d", "0,0,-1,5", "0,0,5,-1", "-1,0,5,5", "0,0,0,5", "1,2,3,NaN"} {
		t.Run("rejected/"+in, func(t *testing.T) {
			t.Parallel()
			if _, err := parseRegion(in); err == nil {
				t.Errorf("parseRegion(%q) was accepted, want a usage error", in)
			}
		})
	}
}

// The capture grammars are small parsers over arbitrary user input: whatever
// they are handed, they either parse it or reject it — never panic.
func FuzzCaptureGrammars(f *testing.F) {
	for _, s := range []string{"", "0,0,10,10", "1-3,5", "a4", "8.5x11", "0.4in", "1,2,3,4", "-", ",,,", "1e999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = parseRegion(s)
		_, _ = parseMargins(s)
		_, _, _ = parsePaper(s)
		_ = validatePageRanges(s)
	})
}
