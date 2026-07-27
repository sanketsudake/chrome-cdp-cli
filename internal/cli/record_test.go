package cli

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// recordBrowser is a recording state machine over the stub: start/stop/status/
// cancel behave the way the driver does, so the CLI's own transitions (and the
// exit codes they map to) are testable without Chrome.
type recordBrowser struct {
	fakeBrowser
	mu        sync.Mutex
	recording bool
	starts    int
	stops     int
	cancels   int
	frames    int            // frames RecordStop hands back
	meta      map[string]any // extra fields the driver would report
	lastOpts  chrome.RecordOpts
}

func newRecordBrowser(t *testing.T) *recordBrowser {
	t.Helper()
	return &recordBrowser{
		fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "https://example.com/"}}},
		frames:      3,
	}
}

func (b *recordBrowser) RecordStart(_ context.Context, _ string, opts chrome.RecordOpts) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.recording {
		return nil, chrome.ErrAlreadyRecording
	}
	b.recording, b.starts, b.lastOpts = true, b.starts+1, opts
	return map[string]any{"action": "start", "recording": true, "fps": opts.FPS, "scale": opts.Scale}, nil
}

func (b *recordBrowser) RecordStop(context.Context, string) ([]chrome.Frame, map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.recording {
		return nil, nil, chrome.ErrNotRecording
	}
	b.recording, b.stops = false, b.stops+1
	base := time.Unix(1700000000, 0)
	out := make([]chrome.Frame, 0, b.frames)
	for i := range b.frames {
		out = append(out, chrome.Frame{
			Data:      testFramePNG(uint8(40 * i)),
			TS:        base.Add(time.Duration(i) * 250 * time.Millisecond),
			Width:     20,
			Height:    16,
			CSSWidth:  40,
			CSSHeight: 32,
			Marks:     []chrome.FrameMark{{X: 20, Y: 16, Command: "click", TS: base}},
		})
	}
	meta := map[string]any{"action": "stop", "frames": len(out), "fps": 4.0, "dropped_frames": 0, "truncated": false}
	for k, v := range b.meta {
		meta[k] = v
	}
	return out, meta, nil
}

func (b *recordBrowser) RecordStatus(context.Context, string) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.recording {
		return map[string]any{"action": "status", "recording": false, "frames": 0}, nil
	}
	return map[string]any{"action": "status", "recording": true, "frames": b.frames, "dropped_frames": 0}, nil
}

func (b *recordBrowser) RecordCancel(context.Context, string) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.recording {
		return nil, chrome.ErrNotRecording
	}
	b.recording, b.cancels = false, b.cancels+1
	return map[string]any{"action": "cancel", "recording": false, "discarded": b.frames}, nil
}

// testFramePNG is one 20x16 frame of a flat colour.
func testFramePNG(shade uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 20, 16))
	for y := range 16 {
		for x := range 20 {
			img.SetRGBA(x, y, color.RGBA{R: shade, G: 0x40, B: 0x80, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func resultOf(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	res, ok := env["result"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no result object: %v", env)
	}
	return res
}

// TestRecordLifecycle is VS-2 through VS-5: the state machine and the exit
// codes it produces. Both mistakes are exit 2, because both are statements
// about the caller's sequence of commands rather than about the page.
func TestRecordLifecycle(t *testing.T) {
	t.Parallel()
	b := newRecordBrowser(t)
	out := filepath.Join(t.TempDir(), "run.gif")

	// VS-4: stop with nothing active.
	env, _, code := run(t, b, "record", "stop", "-o", out, "--target", "aa11", "--json")
	if code != 2 || env["error"].(map[string]any)["code"] != "usage" {
		t.Fatalf("stop with no recording: exit %d, error %v; want 2/usage", code, env["error"])
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a failed stop wrote a file")
	}

	if _, _, code = run(t, b, "record", "start", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("record start exit = %d, want 0", code)
	}

	// VS-2: status reflects the live state.
	env, _, code = run(t, b, "record", "status", "--target", "aa11", "--json")
	if code != 0 || resultOf(t, env)["recording"] != true {
		t.Fatalf("status while recording = %v (exit %d), want recording:true", env["result"], code)
	}

	// VS-3: a double start is refused and the existing recording is untouched.
	env, _, code = run(t, b, "record", "start", "--target", "aa11", "--json")
	if code != 2 || env["error"].(map[string]any)["code"] != "usage" {
		t.Fatalf("double start: exit %d, error %v; want 2/usage", code, env["error"])
	}
	if b.starts != 1 {
		t.Errorf("the driver saw %d starts, want 1", b.starts)
	}
	env, _, _ = run(t, b, "record", "status", "--target", "aa11", "--json")
	if resultOf(t, env)["recording"] != true {
		t.Error("the double start disturbed the existing recording")
	}

	if _, _, code = run(t, b, "record", "stop", "-o", out, "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("record stop exit = %d, want 0", code)
	}
	env, _, _ = run(t, b, "record", "status", "--target", "aa11", "--json")
	if resultOf(t, env)["recording"] != false {
		t.Errorf("status after stop = %v, want recording:false", env["result"])
	}

	// VS-5: cancel discards, writes nothing, and leaves nothing to stop.
	cancelled := filepath.Join(t.TempDir(), "cancelled.gif")
	if _, _, code = run(t, b, "record", "start", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("record start (for cancel) exit = %d", code)
	}
	if _, _, code = run(t, b, "record", "cancel", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("record cancel exit = %d, want 0", code)
	}
	if _, err := os.Stat(cancelled); err == nil {
		t.Error("cancel wrote a file")
	}
	env, _, code = run(t, b, "record", "stop", "-o", cancelled, "--target", "aa11", "--json")
	if code != 2 || env["error"].(map[string]any)["code"] != "usage" {
		t.Fatalf("stop after cancel: exit %d, error %v; want 2/usage", code, env["error"])
	}
}

// TestRecordStopWritesAGIF is VS-1's CLI half: the file decodes, and the
// envelope's frame count is the decoded frame count rather than a hopeful
// number.
func TestRecordStopWritesAGIF(t *testing.T) {
	t.Parallel()
	b := newRecordBrowser(t)
	out := filepath.Join(t.TempDir(), "demo.gif")
	if _, _, code := run(t, b, "record", "start", "--target", "aa11", "--json"); code != 0 {
		t.Fatal("record start failed")
	}
	env, _, code := run(t, b, "record", "stop", "-o", out, "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %v", code, env["error"])
	}
	res := resultOf(t, env)
	if res["path"] != out {
		t.Errorf("result path = %v, want %q", res["path"], out)
	}
	if res["format"] != "gif" {
		t.Errorf("format = %v, want gif", res["format"])
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the export does not decode as a GIF: %v", err)
	}
	if len(g.Image) < 2 {
		t.Errorf("decoded %d frames, want more than one", len(g.Image))
	}
	if got := res["frames"].(float64); int(got) != len(g.Image) {
		t.Errorf("envelope frames = %v, decoded %d — they must agree", got, len(g.Image))
	}
	if res["bytes"].(float64) != float64(len(data)) {
		t.Errorf("envelope bytes = %v, file is %d", res["bytes"], len(data))
	}
}

// TestRecordStopPrintsWhatItWrote covers the privacy-adjacent requirement: in
// human mode it must never be ambiguous that a file was written, or what is in
// it.
func TestRecordStopPrintsWhatItWrote(t *testing.T) {
	t.Parallel()
	b := newRecordBrowser(t)
	out := filepath.Join(t.TempDir(), "demo.gif")
	run(t, b, "record", "start", "--target", "aa11", "--json")
	_, stderr, code := run(t, b, "record", "stop", "-o", out, "--target", "aa11")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stderr, "3 frames") || !strings.Contains(stderr, out) {
		t.Errorf("human output %q does not report the frame count and the path", stderr)
	}
}

// TestRecordAnnotationDefaultsFromStart: `record start --annotate` decides for a
// plain `record stop`, and `record stop --annotate` overrides either way — the
// capture keeps the marks regardless, so both exports are available from one
// recording.
func TestRecordAnnotationDefaultsFromStart(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		startAnnotated bool
		stopArgs       []string
		want           bool
	}{
		"neither":            {false, nil, false},
		"start only":         {true, nil, true},
		"stop only":          {false, []string{"--annotate"}, true},
		"stop overrides off": {true, []string{"--annotate=false"}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := newRecordBrowser(t)
			b.meta = map[string]any{"annotate": c.startAnnotated}
			run(t, b, "record", "start", "--target", "aa11", "--json")
			args := append([]string{"record", "stop", "-o", filepath.Join(t.TempDir(), "x.gif"), "--target", "aa11", "--json"}, c.stopArgs...)
			env, _, code := run(t, b, args...)
			if code != 0 {
				t.Fatalf("exit = %d: %v", code, env["error"])
			}
			if got := resultOf(t, env)["annotated"]; got != c.want {
				t.Errorf("annotated = %v, want %v", got, c.want)
			}
		})
	}
}

// TestRecordFramesFormat is VS-11: a numbered PNG per frame, and a count that
// matches the envelope.
func TestRecordFramesFormat(t *testing.T) {
	t.Parallel()
	b := newRecordBrowser(t)
	dir := filepath.Join(t.TempDir(), "out")
	run(t, b, "record", "start", "--target", "aa11", "--json")
	env, _, code := run(t, b, "record", "stop", "--format", "frames", "-o", dir, "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d: %v", code, env["error"])
	}
	res := resultOf(t, env)
	if res["path"] != dir || res["format"] != "frames" {
		t.Errorf("result = %v, want path %q and format frames", res, dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the frames dir: %v", err)
	}
	if len(entries) != int(res["frames"].(float64)) {
		t.Fatalf("%d files written, envelope says %v", len(entries), res["frames"])
	}
	for i, e := range entries {
		if e.Name() != "frame-0000"+string(rune('1'+i))+".png" {
			t.Errorf("file %d is %q, want a numbered frame", i, e.Name())
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			t.Errorf("%s does not decode as PNG: %v", e.Name(), err)
		}
	}
}

// TestRecordFormatConflicts is VS-9, plus the cases the RFC's table implies:
// the extension decides, --format may say so explicitly, and the two
// disagreeing is an error rather than a guess.
func TestRecordFormatConflicts(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		format, out string
		want        string
		wantErr     bool
	}{
		"extension decides":        {"", "x.gif", "gif", false},
		"webm by extension":        {"", "x.webm", "webm", false},
		"explicit agrees":          {"webm", "x.webm", "webm", false},
		"explicit conflicts":       {"webm", "x.gif", "", true},
		"no output is gif":         {"", "", "gif", false},
		"stdout is gif":            {"", "-", "gif", false},
		"extensionless is gif":     {"", "outfile", "gif", false},
		"unknown extension":        {"", "x.mov", "", true},
		"frames wants a directory": {"frames", "x.gif", "", true},
		"frames with a dir":        {"frames", "./out", "frames", false},
		"unknown format":           {"avi", "x.avi", "", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveExportFormat(c.format, c.out)
			if (err != nil) != c.wantErr {
				t.Fatalf("resolveExportFormat(%q, %q) = %q, %v; wantErr %v", c.format, c.out, got, err, c.wantErr)
			}
			if !c.wantErr && string(got) != c.want {
				t.Errorf("resolveExportFormat(%q, %q) = %q, want %q", c.format, c.out, got, c.want)
			}
		})
	}

	// End to end, the conflict is exit 2 with the browser never contacted.
	env, _, code := run(t, noCall(t), "record", "stop", "-o", "x.gif", "--format", "webm", "--target", "aa11", "--json")
	if code != 2 || env["error"].(map[string]any)["code"] != "usage" {
		t.Errorf("conflicting format: exit %d, error %v; want 2/usage", code, env["error"])
	}
}

// TestRecordMissingEncoderKeepsTheFrames is VS-10, and the reason the encoder
// probe runs before the recording is drained: the user asked for a format this
// machine cannot produce, and must still have their frames afterwards.
//
// Not parallel: it rewrites PATH for the process.
func TestRecordMissingEncoderKeepsTheFrames(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	b := newRecordBrowser(t)
	run(t, b, "record", "start", "--target", "aa11", "--json")

	env, _, code := run(t, b, "record", "stop", "--format", "mp4", "-o", filepath.Join(t.TempDir(), "x.mp4"), "--target", "aa11", "--json")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)", code)
	}
	e := env["error"].(map[string]any)
	if e["code"] != "usage" || !strings.Contains(e["message"].(string), "ffmpeg") {
		t.Errorf("error = %v, want a usage error naming ffmpeg", e)
	}
	if b.stops != 0 {
		t.Fatal("the recording was drained before the encoder was checked — the frames would have been lost")
	}

	// The whole point: the GIF export still works afterwards.
	out := filepath.Join(t.TempDir(), "fallback.gif")
	env, _, code = run(t, b, "record", "stop", "-o", out, "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("the GIF export after a missing encoder failed: exit %d, %v", code, env["error"])
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("no file written: %v", err)
	}
}

// TestRecordReportsTruncation is VS-6 at the CLI boundary: what the driver's
// bounds discarded reaches the envelope, so a partial recording cannot be
// mistaken for a complete one.
func TestRecordReportsTruncation(t *testing.T) {
	t.Parallel()
	b := newRecordBrowser(t)
	b.meta = map[string]any{"dropped_frames": 40, "truncated": true, "reason": "max_frames"}
	run(t, b, "record", "start", "--target", "aa11", "--json")
	env, _, code := run(t, b, "record", "stop", "-o", filepath.Join(t.TempDir(), "x.gif"), "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d: %v", code, env["error"])
	}
	res := resultOf(t, env)
	if res["dropped_frames"].(float64) != 40 || res["truncated"] != true || res["reason"] != "max_frames" {
		t.Errorf("result = %v, want the capture's truncation reported", res)
	}
}

// TestRecordEmptyCaptureIsAnError: a recording that captured nothing must say
// so, not write a zero-frame file that looks like a successful export.
func TestRecordEmptyCaptureIsAnError(t *testing.T) {
	t.Parallel()
	b := newRecordBrowser(t)
	b.frames = 0
	out := filepath.Join(t.TempDir(), "empty.gif")
	run(t, b, "record", "start", "--target", "aa11", "--json")
	env, _, code := run(t, b, "record", "stop", "-o", out, "--target", "aa11", "--json")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (generic)", code)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("an empty recording still wrote a file")
	}
	if msg := env["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "no frames") {
		t.Errorf("message %q does not explain that nothing was captured", msg)
	}
}

// TestRecordValidationBeforeConnect is the exit-2 contract: every malformed
// invocation is rejected with Chrome never contacted.
func TestRecordValidationBeforeConnect(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"fps too low":       {"record", "start", "--fps", "0"},
		"fps too high":      {"record", "start", "--fps", "200"},
		"scale zero":        {"record", "start", "--scale", "0"},
		"scale above one":   {"record", "start", "--scale", "2"},
		"max-frames of one": {"record", "start", "--max-frames", "1"},
		"zero duration":     {"record", "start", "--max-duration", "0s"},
		"negative loop":     {"record", "stop", "--loop", "-1"},
		"bad max-size":      {"record", "stop", "--max-size", "plenty"},
		"unknown format":    {"record", "stop", "--format", "avi"},
		"frames to stdout":  {"record", "stop", "--format", "frames", "-o", "-"},
		"session record":    {"session", "--record", "run.mov"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			args = append(args, "--target", "aa11", "--json")
			env, _, code := run(t, noCall(t), args...)
			if code != 2 {
				t.Fatalf("%v exit = %d, want 2", args, code)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("%v error = %v, want usage", args, env["error"])
			}
		})
	}
}

// TestRecordStopSurvivesAClosedTab is US-7's sharpest case: the automation died
// and took its tab with it. The frames were never held by that tab, so they are
// still exportable — the recording is complete even though the tab is not.
func TestRecordStopSurvivesAClosedTab(t *testing.T) {
	t.Parallel()
	b := newRecordBrowser(t)
	run(t, b, "record", "start", "--target", "aa11", "--json")
	// The tab is gone from the list, exactly as a closed tab would be.
	b.tabs = nil

	out := filepath.Join(t.TempDir(), "crash.gif")
	env, _, code := run(t, b, "record", "stop", "-o", out, "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %v", code, env["error"])
	}
	res := resultOf(t, env)
	if res["tab_closed"] != true {
		t.Errorf("result = %v, want tab_closed reported", res)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("the stranded recording wrote no file: %v", err)
	}

	// An ephemeral spec names a tab by something that no longer exists, so it
	// stays a target error rather than being re-interpreted against a changed
	// tab list.
	run(t, b, "record", "start", "--target", "aa11", "--json")
	_, _, code = run(t, b, "record", "stop", "-o", filepath.Join(t.TempDir(), "y.gif"), "--target", "url:example", "--json")
	if code != 4 {
		t.Errorf("stop by url: spec on a closed tab = exit %d, want 4", code)
	}
}

// TestSessionRecord is VS-15: `session --record` writes the file when the batch
// finishes, reports it in the output, and does so even when a step failed —
// which is when a recording is worth the most.
func TestSessionRecord(t *testing.T) {
	t.Parallel()
	b := &sessionRecordBrowser{recordBrowser: *newRecordBrowser(t)}
	out := filepath.Join(t.TempDir(), "batch.gif")
	steps := strings.Join([]string{
		`["use","aa11"]`,
		`["text","#ok"]`,
		`["click","#boom"]`,
		`["text","#ok"]`,
	}, "\n")

	var stdout, stderr bytes.Buffer
	app := New(b, &stdout, &stderr).WithInput(strings.NewReader(steps))
	app.WithStickyTarget(func(ConnOpts) string { return "aa11" }, func(ConnOpts, string) error { return nil })
	code := app.Execute("session", "--record", out)
	if code != 0 {
		t.Fatalf("session exit = %d, want 0\n%s", code, stderr.String())
	}
	if b.starts != 1 || b.stops != 1 {
		t.Errorf("driver saw %d starts and %d stops, want 1 of each", b.starts, b.stops)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("the batch recording wrote no file: %v", err)
	}

	// The batch's own output has to say where the recording went, or a caller
	// parsing NDJSON has no way to learn it.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"command":"record"`) || !strings.Contains(last, out) {
		t.Errorf("the final NDJSON line does not report the recording: %s", last)
	}
	// And the failing step is still a failing step.
	if !strings.Contains(stdout.String(), `"ok":false`) {
		t.Error("the failing step did not report a failure")
	}
}

// sessionRecordBrowser fails one verb, so the batch above has a failure in it.
type sessionRecordBrowser struct {
	recordBrowser
}

func (b *sessionRecordBrowser) Pointer(context.Context, string, string, chrome.PointerOpts) (map[string]any, error) {
	return nil, errors.New("could not find node for #boom")
}

// TestSessionRecordWithoutATargetSaysSo: a batch that never resolves a tab has
// nothing to record, and must say that rather than silently writing nothing.
func TestSessionRecordWithoutATarget(t *testing.T) {
	t.Parallel()
	b := newRecordBrowser(t)
	var stdout, stderr bytes.Buffer
	app := New(b, &stdout, &stderr).WithInput(strings.NewReader("[\"version\"]\n"))
	code := app.Execute("session", "--record", filepath.Join(t.TempDir(), "nothing.gif"))
	if code != 0 {
		t.Fatalf("session exit = %d, want 0\n%s", code, stderr.String())
	}
	if b.starts != 0 {
		t.Errorf("a recording was started with no target: %d starts", b.starts)
	}
	if !strings.Contains(stdout.String(), `"command":"record"`) || !strings.Contains(stdout.String(), `"ok":false`) {
		t.Errorf("the batch did not report that nothing was recorded:\n%s", stdout.String())
	}
}

func TestParseByteSize(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in      string
		want    int
		wantErr bool
	}{
		"plain bytes":   {"1500000", 1500000, false},
		"with b":        {"2048b", 2048, false},
		"kilobytes":     {"64kb", 64 << 10, false},
		"bare k":        {"64k", 64 << 10, false},
		"megabytes":     {"2MB", 2 << 20, false},
		"fractional mb": {"1.5mb", 1572864, false},
		"gigabytes":     {"1g", 1 << 30, false},
		"padded":        {" 2 mb ", 2 << 20, false},
		"zero":          {"0", 0, true},
		"negative":      {"-5", 0, true},
		"words":         {"plenty", 0, true},
		"empty":         {"", 0, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := parseByteSize(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("parseByteSize(%q) = %d, %v; wantErr %v", c.in, got, err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("parseByteSize(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}
