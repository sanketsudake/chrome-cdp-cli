package chrome

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"

	"github.com/sanketsudake/chrome-cdp-cli/internal/encode"
)

// synthFrame is one screencast frame as the event loop would hand it over: a
// base64 PNG plus the metadata Chrome attaches.
func synthFrame(t *testing.T, ts time.Time, w, h int, shade uint8) pendingFrame {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{R: shade, G: shade, B: shade, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode synthetic frame: %v", err)
	}
	e := cdp.TimeSinceEpoch(ts)
	return pendingFrame{
		data: base64.StdEncoding.EncodeToString(buf.Bytes()),
		meta: &page.ScreencastFrameMetadata{
			DeviceWidth: float64(w) * 2, DeviceHeight: float64(h) * 2, Timestamp: &e,
		},
	}
}

// testRecorder builds a recorder with no browser behind it. store() is pure
// with respect to CDP — it decodes, accounts, and retains — so every bound this
// feature rests on is testable without Chrome, which is where they should be
// tested: frame timing through a real browser is not reproducible.
func testRecorder(t *testing.T, opts RecordOpts, maxBytes int) *recorder {
	t.Helper()
	r := newRecorder(opts.withDefaults(DefaultRecordFrames), t.Context(), maxBytes)
	t.Cleanup(func() {
		r.mu.Lock()
		r.stopped = true
		r.mu.Unlock()
	})
	return r
}

// TestRingTruncatesAndSaysSo is VS-6: a recording longer than the ring keeps
// the MOST RECENT frames, and reports exactly how many it lost.
//
// The reporting is the point. Silent truncation would let a caller present a
// partial recording as a complete one, which is the failure US-6 names.
func TestRingTruncatesAndSaysSo(t *testing.T) {
	t.Parallel()
	base := time.Unix(1700000000, 0).UTC()
	r := testRecorder(t, RecordOpts{FPS: 4, MaxFrames: 10}, 0)

	for i := range 50 {
		// 250ms apart, which is exactly the 4fps cadence: nothing is thrown away
		// by the throttle, so every drop below is the ring's doing.
		r.store(synthFrame(t, base.Add(time.Duration(i)*250*time.Millisecond), 8, 6, uint8(i)))
	}

	st := r.stat()
	if st["frames"] != 10 {
		t.Errorf("frames = %v, want 10 (the ring size)", st["frames"])
	}
	if st["dropped_frames"] != 40 {
		t.Errorf("dropped_frames = %v, want 40", st["dropped_frames"])
	}
	if st["truncated"] != true {
		t.Errorf("truncated = %v, want true", st["truncated"])
	}
	if st["reason"] != recordReasonFrames {
		t.Errorf("reason = %v, want %q", st["reason"], recordReasonFrames)
	}

	frames, meta := r.drain()
	if len(frames) != 10 || meta["frames"] != 10 {
		t.Fatalf("drained %d frames, meta says %v; want 10", len(frames), meta["frames"])
	}
	// The retained window is the END of the recording — the part someone
	// actually wants to watch.
	wantFirst := base.Add(40 * 250 * time.Millisecond)
	if !frames[0].TS.Equal(wantFirst) {
		t.Errorf("oldest retained frame ts = %v, want %v (the most recent 10 must survive)", frames[0].TS, wantFirst)
	}
	for i := 1; i < len(frames); i++ {
		if !frames[i].TS.After(frames[i-1].TS) {
			t.Fatalf("frames are not in capture order at %d: %v then %v", i, frames[i-1].TS, frames[i].TS)
		}
	}
}

// TestByteCeilingShrinksTheRing covers the bound a frame COUNT cannot express:
// the same 600 frames are a few megabytes on a laptop and hundreds on a 4K
// monitor, so the byte ceiling has to be able to keep fewer of them.
func TestByteCeilingShrinksTheRing(t *testing.T) {
	t.Parallel()
	base := time.Unix(1700000000, 0).UTC()
	// One frame's worth of bytes, measured rather than guessed.
	probe := testRecorder(t, RecordOpts{FPS: 4, MaxFrames: 100}, 0)
	probe.store(synthFrame(t, base, 40, 30, 1))
	one := probe.bytes
	if one <= 0 {
		t.Fatalf("probe retained %d bytes", one)
	}

	r := testRecorder(t, RecordOpts{FPS: 4, MaxFrames: 100}, one*4)
	for i := range 20 {
		r.store(synthFrame(t, base.Add(time.Duration(i)*250*time.Millisecond), 40, 30, uint8(i)))
	}

	st := r.stat()
	frames := st["frames"].(int)
	if frames < recordMinRetained || frames > 5 {
		t.Errorf("frames = %d, want the byte ceiling to have held it near 4", frames)
	}
	if r.bytes > one*4 {
		t.Errorf("retained bytes = %d, want <= %d", r.bytes, one*4)
	}
	if st["truncated"] != true || st["reason"] != recordReasonBytes {
		t.Errorf("truncated/reason = %v/%v, want true/%q", st["truncated"], st["reason"], recordReasonBytes)
	}
	// Everything the rebuilds discarded is still counted: a fresh ring's own
	// counter starts at zero, and the total must not.
	if dropped := st["dropped_frames"].(int); dropped != 20-frames {
		t.Errorf("dropped_frames = %d, want %d (every frame not retained)", dropped, 20-frames)
	}
}

// TestCadenceThrottleIsNotLoss pins a distinction the envelope depends on: a
// frame skipped because the caller asked for 4fps is not a DROPPED frame.
// Counting it as one would report every recording of a busy page as truncated.
func TestCadenceThrottleIsNotLoss(t *testing.T) {
	t.Parallel()
	base := time.Unix(1700000000, 0).UTC()
	r := testRecorder(t, RecordOpts{FPS: 4, MaxFrames: 100}, 0)
	// 60 frames at 60fps over one second: at 4fps only about four are wanted.
	for i := range 60 {
		r.store(synthFrame(t, base.Add(time.Duration(i)*16*time.Millisecond), 8, 6, uint8(i)))
	}
	st := r.stat()
	if n := st["frames"].(int); n < 3 || n > 6 {
		t.Errorf("frames = %d, want ~4 (one second at 4fps)", n)
	}
	if st["dropped_frames"] != 0 {
		t.Errorf("dropped_frames = %v, want 0 — the cadence is not data loss", st["dropped_frames"])
	}
	if st["truncated"] != false {
		t.Errorf("truncated = %v, want false", st["truncated"])
	}
}

// TestRecordRestoreMakesTheExportRetryable is the driver half of "a failed
// export must not cost the recording": RecordStop is destructive, so the CLI
// hands the frames back when the write fails, and the next `record stop` gets
// them.
func TestRecordRestoreMakesTheExportRetryable(t *testing.T) {
	t.Parallel()
	c := &CDP{rec: map[string]*recorder{}}
	base := time.Unix(1700000000, 0).UTC()
	frames := []Frame{
		{Data: []byte("one"), TS: base},
		{Data: []byte("two"), TS: base.Add(250 * time.Millisecond)},
	}
	meta := map[string]any{"dropped_frames": 40, "truncated": true, "reason": recordReasonFrames, "fps": 4.0}

	if err := c.RecordRestore(t.Context(), "aa11", frames, meta); err != nil {
		t.Fatalf("RecordRestore: %v", err)
	}
	got, gotMeta, err := c.RecordStop(t.Context(), "aa11")
	if err != nil {
		t.Fatalf("RecordStop after a restore: %v", err)
	}
	if len(got) != 2 || !bytes.Equal(got[0].Data, []byte("one")) || !bytes.Equal(got[1].Data, []byte("two")) {
		t.Fatalf("got %d frames back: %v", len(got), got)
	}
	// The CAPTURE's accounting survives: a restored recording that reported a
	// clean run would erase the fact that the ring evicted 40 frames.
	if gotMeta["dropped_frames"] != 40 || gotMeta["truncated"] != true || gotMeta["reason"] != recordReasonFrames {
		t.Errorf("meta = %v, want the original capture's accounting", gotMeta)
	}
	if gotMeta["restored"] != true || gotMeta["frames"] != 2 {
		t.Errorf("meta = %v, want restored:true and frames:2", gotMeta)
	}
	// And it is gone again, so a second retry is the ordinary "nothing to stop".
	if _, _, err := c.RecordStop(t.Context(), "aa11"); !IsNotRecording(err) {
		t.Errorf("second stop = %v, want ErrNotRecording", err)
	}
}

// TestRecordRestoreDoesNotClobberALiveRecording: a new recording started while
// the export was failing wins. Overwriting a live capture with a dead one to
// make room for a retry is the worse trade.
func TestRecordRestoreDoesNotClobberALiveRecording(t *testing.T) {
	t.Parallel()
	live := testRecorder(t, RecordOpts{FPS: 4, MaxFrames: 10}, 0)
	c := &CDP{rec: map[string]*recorder{"aa11": live}}
	err := c.RecordRestore(t.Context(), "aa11", []Frame{{Data: []byte("x")}}, nil)
	if !IsAlreadyRecording(err) {
		t.Errorf("RecordRestore over a live recording = %v, want ErrAlreadyRecording", err)
	}
	if c.recorder("aa11") != live {
		t.Error("the live recording was replaced")
	}
}

// TestAttachMarks is the correlation half of annotation: which frames an action
// lands on. Drawing is internal/encode's job; deciding WHERE is this one's.
func TestAttachMarks(t *testing.T) {
	t.Parallel()
	base := time.Unix(1700000000, 0).UTC()
	frames := func() []Frame {
		out := make([]Frame, 0, 8)
		for i := range 8 {
			out = append(out, Frame{TS: base.Add(time.Duration(i) * 250 * time.Millisecond)})
		}
		return out
	}

	t.Run("a mark covers the frames around it", func(t *testing.T) {
		t.Parallel()
		// The action lands with frame 2 (t=500ms) on screen.
		got := attachMarks(frames(), []FrameMark{{X: 10, Y: 20, Command: "click", TS: base.Add(500 * time.Millisecond)}})
		marked := 0
		for i, f := range got {
			if len(f.Marks) == 0 {
				continue
			}
			marked++
			if f.TS.Before(base.Add(100*time.Millisecond)) || f.TS.After(base.Add(1700*time.Millisecond)) {
				t.Errorf("frame %d at %v carries a mark from t=500ms", i, f.TS)
			}
		}
		if marked < 2 {
			t.Errorf("%d frames marked, want the marker visible across several at 4fps", marked)
		}
	})

	t.Run("a mark with no frame in its window lands on the nearest", func(t *testing.T) {
		t.Parallel()
		// A click that changed nothing: the page produced no frame near it.
		late := base.Add(30 * time.Second)
		got := attachMarks(frames(), []FrameMark{{X: 1, Y: 2, Command: "click", TS: late}})
		total := 0
		for _, f := range got {
			total += len(f.Marks)
		}
		if total != 1 {
			t.Fatalf("%d marks attached, want exactly 1 — a marker must never be silently lost", total)
		}
		if len(got[len(got)-1].Marks) != 1 {
			t.Error("the orphan mark did not land on the nearest (last) frame")
		}
	})

	t.Run("no marks leaves the frames alone", func(t *testing.T) {
		t.Parallel()
		got := attachMarks(frames(), nil)
		for i, f := range got {
			if len(f.Marks) != 0 {
				t.Fatalf("frame %d gained a mark from nowhere", i)
			}
		}
	})
}

// TestRecordOptsDefaults checks the driver fills what a lenient caller (the
// daemon's arg decoders, or a direct API user) left at zero.
func TestRecordOptsDefaults(t *testing.T) {
	t.Parallel()
	got := RecordOpts{}.withDefaults(42)
	if got.FPS != DefaultRecordFPS || got.Scale != DefaultRecordScale ||
		got.Quality != DefaultRecordQuality || got.MaxDuration != DefaultRecordMaxDuration {
		t.Errorf("defaults = %+v", got)
	}
	if got.MaxFrames != 42 {
		t.Errorf("MaxFrames = %d, want the configured 42", got.MaxFrames)
	}
	kept := RecordOpts{FPS: 10, Scale: 0.25, Quality: 90, MaxFrames: 7, MaxDuration: time.Second, Annotate: true}
	if kept.withDefaults(42) != kept {
		t.Errorf("withDefaults overwrote explicit values: %+v", kept.withDefaults(42))
	}
}

// recordFixture is a page that keeps changing, so the compositor keeps pushing
// screencast frames. A static page produces one frame and then nothing, which
// is the whole reason a screencast beats a screenshot loop — and the reason a
// live recording test needs something moving.
const recordFixture = `<!doctype html><title>Recording</title>
<body style="margin:0">
<div id="box" style="width:100%;height:100%;background:#123"></div>
<button id="go" aria-label="Go" style="position:fixed;left:40px;top:40px">Go</button>
<script>
let n = 0;
setInterval(() => {
  n = (n + 37) % 255;
  document.getElementById('box').style.background = 'rgb(' + n + ',' + ((n*3)%255) + ',120)';
}, 60);
</script>`

// TestRecordLive covers VS-1, VS-3, VS-7 and VS-8 against a real Chrome.
//
// Everything asserted here is a RANGE or a structural property — it decodes, it
// has more than one frame, the dimensions are within tolerance. Frame timing
// through a real browser is not reproducible, and macOS CI is slower and more
// contended than Linux, so an exact frame count would be a test that fails
// somewhere else for no reason.
func TestRecordLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, recordFixture)
	}))
	defer srv.Close()

	// No viewport emulation: an emulated viewport and the window's real surface
	// are different sizes in headless, and --scale caps what the compositor
	// actually produces. Emulating one here would make the scale assertion below
	// a test of viewport emulation instead.
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	if _, err := b.RecordStart(ctx, id, RecordOpts{FPS: 8, Scale: 0.5, MaxFrames: 200}); err != nil {
		t.Fatalf("RecordStart: %v", err)
	}

	// VS-3: a second start is refused and the existing recording carries on.
	if _, err := b.RecordStart(ctx, id, RecordOpts{}); !IsAlreadyRecording(err) {
		t.Errorf("second RecordStart error = %v, want ErrAlreadyRecording", err)
	}

	// Drive something, so there is an action to annotate and a reason for the
	// page to change beyond its own animation.
	if _, err := b.Pointer(ctx, id, "Go", PointerOpts{Action: PointerClick, Query: QueryOpts{By: "name", Role: "button"}}); err != nil {
		t.Fatalf("click: %v", err)
	}

	// Drive the change from HERE rather than trusting the page's own timer: a
	// headless or backgrounded tab throttles setInterval, so a test that waited
	// for the fixture to animate itself would be timing out on Chrome's power
	// management rather than on anything this feature does. Polling (instead of
	// sleeping a tuned amount) keeps it honest on slower, more contended CI.
	deadline := time.Now().Add(30 * time.Second)
	var frames int
	for i := 0; time.Now().Before(deadline); i++ {
		if _, err := b.Eval(ctx, id, fmt.Sprintf("document.body.style.background='rgb(%d,40,%d)'", i%200, (i*7)%200), EvalOpts{}); err != nil {
			t.Fatalf("Eval: %v", err)
		}
		st, err := b.RecordStatus(ctx, id)
		if err != nil {
			t.Fatalf("RecordStatus: %v", err)
		}
		if st["recording"] != true {
			t.Fatalf("status says not recording while a recording is active: %v", st)
		}
		frames = st["frames"].(int)
		if frames >= 3 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if frames < 3 {
		t.Fatalf("only %d frames while the page kept changing — the screencast is not delivering", frames)
	}

	got, meta, err := b.RecordStop(ctx, id)
	if err != nil {
		t.Fatalf("RecordStop: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("stopped with %d frames, want more than one", len(got))
	}
	if meta["frames"] != len(got) {
		t.Errorf("meta frames = %v, want %d (the envelope must match what came back)", meta["frames"], len(got))
	}

	// VS-8: --scale halves the captured frame. The reference is a capture of the
	// same page at scale 1, not the emulated viewport: a headless window's real
	// surface and its emulated viewport need not agree, and this assertion is
	// about the scale factor rather than about viewport emulation.
	full := recordOneFrame(ctx, t, b, id, RecordOpts{FPS: 8, Scale: 1})
	if !within(got[0].Width, float64(full.Width)*0.5, 0.15) || !within(got[0].Height, float64(full.Height)*0.5, 0.15) {
		t.Errorf("--scale 0.5 produced %dx%d against an unscaled %dx%d, want about half",
			got[0].Width, got[0].Height, full.Width, full.Height)
	}

	// VS-1: what came back really is an animation.
	res, err := encode.Encode(toEncodeFrames(got), encode.Options{Format: encode.FormatGIF, FPS: 8})
	if err != nil {
		t.Fatalf("encode the captured frames: %v", err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("the export does not decode as a GIF: %v", err)
	}
	if len(g.Image) != len(got) {
		t.Errorf("decoded %d GIF frames, want the captured %d", len(g.Image), len(got))
	}

	// VS-2's second half: after stopping there is nothing to stop.
	st, err := b.RecordStatus(ctx, id)
	if err != nil {
		t.Fatalf("RecordStatus after stop: %v", err)
	}
	if st["recording"] != false {
		t.Errorf("status after stop = %v, want recording:false", st)
	}
	if _, _, err := b.RecordStop(ctx, id); !IsNotRecording(err) {
		t.Errorf("second RecordStop error = %v, want ErrNotRecording", err)
	}

	// VS-7: the maximum duration stops the CAPTURE and says why, while leaving
	// the frames exportable.
	if _, err := b.RecordStart(ctx, id, RecordOpts{FPS: 8, Scale: 0.5, MaxDuration: time.Second}); err != nil {
		t.Fatalf("RecordStart (max-duration): %v", err)
	}
	for i := range 6 {
		if _, err := b.Eval(ctx, id, fmt.Sprintf("document.body.style.background='rgb(200,%d,10)'", i*30), EvalOpts{}); err != nil {
			t.Fatalf("Eval: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	dur, dmeta, err := b.RecordStop(ctx, id)
	if err != nil {
		t.Fatalf("RecordStop (max-duration): %v", err)
	}
	if dmeta["truncated"] != true || dmeta["reason"] != recordReasonDuration {
		t.Errorf("truncated/reason = %v/%v, want true/%q", dmeta["truncated"], dmeta["reason"], recordReasonDuration)
	}
	if len(dur) == 0 {
		t.Error("a duration-truncated recording exported nothing; the frames captured before the limit must survive")
	}

	// VS-5: cancel discards, and leaves nothing to stop.
	if _, err := b.RecordStart(ctx, id, RecordOpts{FPS: 8, Scale: 0.5}); err != nil {
		t.Fatalf("RecordStart (cancel): %v", err)
	}
	if _, err := b.RecordCancel(ctx, id); err != nil {
		t.Fatalf("RecordCancel: %v", err)
	}
	if _, _, err := b.RecordStop(ctx, id); !IsNotRecording(err) {
		t.Errorf("RecordStop after cancel = %v, want ErrNotRecording", err)
	}
}

// recordOneFrame records just long enough to capture a frame, and returns it.
func recordOneFrame(ctx context.Context, t *testing.T, b *CDP, id string, opts RecordOpts) Frame {
	t.Helper()
	if _, err := b.RecordStart(ctx, id, opts); err != nil {
		t.Fatalf("RecordStart(%+v): %v", opts, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		if _, err := b.Eval(ctx, id, fmt.Sprintf("document.body.style.background='rgb(10,%d,90)'", i%200), EvalOpts{}); err != nil {
			t.Fatalf("Eval: %v", err)
		}
		st, err := b.RecordStatus(ctx, id)
		if err != nil {
			t.Fatalf("RecordStatus: %v", err)
		}
		if st["frames"].(int) >= 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	frames, _, err := b.RecordStop(ctx, id)
	if err != nil {
		t.Fatalf("RecordStop: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("a reference recording captured no frames")
	}
	return frames[0]
}

// within reports whether got is within tol (a fraction) of want.
func within(got int, want, tol float64) bool {
	return math.Abs(float64(got)-want) <= want*tol
}

// toEncodeFrames is the same conversion the CLI does, kept here so the live
// test exercises the real export path rather than a shortcut.
func toEncodeFrames(frames []Frame) []encode.Frame {
	out := make([]encode.Frame, 0, len(frames))
	for _, f := range frames {
		ef := encode.Frame{Data: f.Data, TS: f.TS, CSSWidth: f.CSSWidth, CSSHeight: f.CSSHeight}
		for _, m := range f.Marks {
			ef.Marks = append(ef.Marks, encode.Mark{X: m.X, Y: m.Y, Command: m.Command})
		}
		out = append(out, ef)
	}
	return out
}
