package chrome

// Session recording (RFC-0011) — the capture half.
//
// Three decisions shape this file, and each is the answer to a failure mode:
//
//   - The frames live HERE, on the object that holds the connection, which in
//     normal use is owned by the daemon. A recording spans many CLI
//     invocations, so a per-command process could not hold them; and because
//     the process that dies with a failed automation was never holding them,
//     `record stop` after a crash still exports what was captured (US-7).
//
//   - Capture is Page.startScreencast, not a screenshot loop. Chrome pushes a
//     frame only when the page actually changes, which is both cheaper and
//     better-looking than polling a static page — and it comes with one hard
//     obligation: EVERY frame must be acknowledged, or Chrome stops sending
//     after the first. The ack cannot happen on the event loop (it is a CDP
//     call), so frames are handed to a pump goroutine that acks and stores.
//
//   - Bounds are structural, and their effects are REPORTED. A ring of
//     MaxFrames, a total byte ceiling, and a max duration each stop the daemon
//     growing; each one that fires increments dropped_frames or sets truncated,
//     because a partial recording presented as complete is the failure US-6
//     exists to prevent.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	_ "image/jpeg" // registered so a screencast frame's header decodes
	"math"
	"sync"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/sanketsudake/chrome-cdp-cli/internal/eventbuf"
)

// Capture defaults. They are defaults, not policy: the `record` flags override
// the per-recording ones and the `record_buffer` / `record_max_bytes` config
// keys override the daemon-wide bounds.
const (
	// DefaultRecordFrames is the ring size. At the default scale and quality a
	// frame is a few tens of KB, so 600 frames is a couple of minutes of a
	// moving page for well under a hundred MB.
	DefaultRecordFrames = 600

	// DefaultRecordMaxBytes is the ceiling on retained frame bytes per tab. The
	// frame count alone does not bound memory — a 4K viewport frame is an order
	// of magnitude larger than a laptop one — so the byte ceiling is what makes
	// the worst case a constant rather than a function of the user's monitor.
	DefaultRecordMaxBytes = 96 << 20

	// DefaultRecordFPS is the capture cadence ceiling. GIFs rarely need more,
	// and every frame above it is memory spent on something nobody will see.
	DefaultRecordFPS = 4

	// DefaultRecordScale halves the captured dimensions, which quarters the
	// bytes for output that is still perfectly readable in an issue thread.
	DefaultRecordScale = 0.5

	// DefaultRecordQuality is the JPEG quality of a captured frame. High enough
	// that text stays legible, low enough that a long recording fits the cap.
	DefaultRecordQuality = 60

	// DefaultRecordMaxDuration stops a capture that was never stopped by hand.
	DefaultRecordMaxDuration = 2 * time.Minute

	// recordQueueDepth is the handoff depth between the CDP event loop and the
	// pump. Deep enough that ordinary processing never overflows it; when it
	// does overflow, the frame is dropped but its ACK is still queued, because
	// an unacknowledged frame stalls the screencast permanently.
	recordQueueDepth = 256

	// recordMarkLinger is how long an action's marker stays on the frames after
	// it: long enough to be visible at 4fps, short enough that a batch of clicks
	// does not smear into one another.
	recordMarkLinger = 1200 * time.Millisecond

	// recordMarkPreroll attaches a marker to frames captured just BEFORE the
	// action too. A click that changes nothing produces no new frame, and a
	// marker with no frame to land on would silently vanish.
	recordMarkPreroll = 400 * time.Millisecond

	// recordMaxMarks bounds the action log of one recording.
	recordMaxMarks = 2000

	// recordMinRetained is the floor the byte ceiling may shrink a ring to. Two
	// frames is the least that is still an animation.
	recordMinRetained = 2

	// recordPokeFloor bounds how often the recorder nudges the page into
	// rendering (see poke). Never more often than the capture cadence, and never
	// faster than this.
	recordPokeFloor = 100 * time.Millisecond

	// recordStrandedTTL is how long a recording whose TAB has closed is held
	// before the daemon releases it. Long enough for the `record stop` a human
	// types after noticing a run died (US-7), short enough that an abandoned
	// recording is not a permanent 96MB hole in a long-lived daemon.
	recordStrandedTTL = 10 * time.Minute
)

// Recording lifecycle errors. They are sentinels because the CLI maps both to
// `usage` / exit 2, and — like every other sentinel here — they are matched
// through errIs, so they survive the daemon RPC flattening them to a string.
var (
	// ErrAlreadyRecording reports a second `record start` on a tab that is
	// already recording. It is deliberately not a no-op: silently adopting the
	// existing recording would make `--fps 30` look like it took effect.
	ErrAlreadyRecording = errors.New("a recording is already active on this tab")

	// ErrNotRecording reports a stop/cancel with nothing to stop.
	ErrNotRecording = errors.New("no recording is active on this tab")
)

// IsAlreadyRecording reports whether err is ErrAlreadyRecording.
func IsAlreadyRecording(err error) bool { return errIs(err, ErrAlreadyRecording) }

// IsNotRecording reports whether err is ErrNotRecording.
func IsNotRecording(err error) bool { return errIs(err, ErrNotRecording) }

// The reasons a capture stopped early, reported as `reason` in the envelope.
const (
	recordReasonDuration = "max_duration"
	recordReasonBytes    = "max_bytes"
	recordReasonFrames   = "max_frames"
)

// configureRecordCapture sizes the recording bounds from the resolved config.
// Like configureCapture it runs in Connect, before any tab is attached. Zero
// (an unset config key, or a direct launch in a test) means the default.
func (c *CDP) configureRecordCapture(maxFrames, maxBytes int) {
	if maxFrames <= 0 {
		maxFrames = DefaultRecordFrames
	}
	if maxBytes <= 0 {
		maxBytes = DefaultRecordMaxBytes
	}
	c.recMaxFrames, c.recMaxBytes = maxFrames, maxBytes
}

// pendingFrame is one screencast frame on its way from the event loop to the
// pump. AckOnly frames were dropped by the handoff bound and carry no data:
// they exist purely so their acknowledgement still reaches Chrome.
type pendingFrame struct {
	sessionID int64
	data      string
	meta      *page.ScreencastFrameMetadata
	ackOnly   bool
}

// recorder is one tab's live recording.
type recorder struct {
	opts    RecordOpts
	started time.Time
	tctx    context.Context

	mu   sync.Mutex
	buf  *eventbuf.Buffer[Frame]
	max  int   // the ring's current size (the byte ceiling may shrink it)
	size []int // retained frame sizes, oldest first — parallel to the ring

	bytes    int
	maxBytes int
	// evicted counts frames the ring lost across rebuilds, plus the ones the
	// handoff dropped. The live ring's own evictions are read from the buffer.
	evicted   int
	marks     []FrameMark
	lastKept  time.Time
	stopped   bool
	truncated bool
	reason    string

	// restored is the accounting a re-seated recording carries (see
	// RecordRestore). Non-nil means this recorder never captured anything itself
	// and must report the original capture's numbers rather than its own.
	restored map[string]any

	// release forgets this recording from the CDP that owns it, and strandedTTL
	// is how long after the TAB closes that happens. See pump's tctx.Done case.
	release     func()
	strandedTTL time.Duration

	queue     []pendingFrame
	lastFrame time.Time // when a frame last ARRIVED, retained or not
	lastData  []byte    // the last retained frame's bytes, for duplicate rejection
	wake      chan struct{}
	done      chan struct{} // closed by stopCapture: "stop capturing"
	exited    chan struct{} // closed by the pump: "no goroutine is still storing"
	stopOnce  sync.Once
}

// withDefaults fills the zero fields a non-CLI caller left behind.
func (o RecordOpts) withDefaults(maxFrames int) RecordOpts {
	if o.FPS <= 0 {
		o.FPS = DefaultRecordFPS
	}
	if o.Scale <= 0 {
		o.Scale = DefaultRecordScale
	}
	if o.Quality <= 0 {
		o.Quality = DefaultRecordQuality
	}
	if o.MaxFrames <= 0 {
		o.MaxFrames = maxFrames
	}
	if o.MaxDuration <= 0 {
		o.MaxDuration = DefaultRecordMaxDuration
	}
	return o
}

// scaleBox turns a viewport and a scale factor into the pixel cap
// startScreencast takes — it wants a maximum SIZE, not a factor.
//
// Split out so the arithmetic half of "--scale halves the capture" is testable
// without a browser: the live half (Chrome honours the cap) needs a renderer,
// this half does not, and keeping them together made the whole claim hostage to
// a viewport that was observed moving mid-test.
func scaleBox(vw, vh, scale float64) (int64, int64) {
	return int64(math.Round(vw * scale)), int64(math.Round(vh * scale))
}

// pageAgreesViewport reports whether the page's own innerWidth/innerHeight
// matches the box CDP just reported, to within a pixel of rounding.
//
// The two are independent measures that settle independently, so a disagreement
// means the viewport is mid-resize and NEITHER is trustworthy yet. A read error
// counts as agreement: this is a cross-check, not a gate, and a tab that cannot
// answer JS is a reason to fall back to the CDP number rather than to refuse to
// record.
func pageAgreesViewport(ctx context.Context, v Rect) bool {
	var wh []float64
	if err := chromedp.Evaluate(`[innerWidth, innerHeight]`, &wh).Do(ctx); err != nil || len(wh) != 2 {
		return true
	}
	return math.Abs(wh[0]-v.Width) <= 1 && math.Abs(wh[1]-v.Height) <= 1
}

// startRecordCapture registers the screencast listener for a freshly attached
// tab. It is called from listenCapture, under c.mu, exactly once per tab.
//
// The listener is registered at ATTACH rather than at `record start` for the
// same reason the console's is: chromedp listeners cannot be unregistered, so
// starting and stopping a recording repeatedly would otherwise stack a new
// listener each time. It routes to whatever recording is active for the tab,
// and does nothing at all when there is none — which is the normal case, since
// Chrome sends no screencast frames until one asks for them.
func (c *CDP) startRecordCapture(tctx context.Context, id string) {
	chromedp.ListenTarget(tctx, func(ev any) {
		f, ok := ev.(*page.EventScreencastFrame)
		if !ok {
			return
		}
		if r := c.recorder(id); r != nil {
			r.offer(f)
		}
	})
}

// recorder returns the active recording for a tab, or nil.
func (c *CDP) recorder(id string) *recorder {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	return c.rec[id]
}

// RecordStart begins recording a tab.
func (c *CDP) RecordStart(ctx context.Context, id string, opts RecordOpts) (map[string]any, error) {
	opts = opts.withDefaults(c.recMaxFrames)
	// Refuse a double start BEFORE touching the tab: bringing a window to the
	// front and reading its layout are visible to the user, and an invocation
	// that was always going to be refused should cost them nothing. The
	// authoritative check is the one under the lock below.
	if c.recorder(id) != nil {
		return nil, ErrAlreadyRecording
	}

	// The tab is attached (and its listener registered) before the recorder is
	// published, so no frame can arrive with nothing to receive it.
	tctx, err := c.on(id)
	if err != nil {
		return nil, err
	}

	// The viewport is read first because startScreencast takes a maximum size in
	// PIXELS, not a scale factor.
	//
	// It has to be a SETTLED read. A recording started right after a nav can
	// otherwise size itself from a viewport the page has not laid out yet, and
	// the max box then has the wrong aspect for the whole recording — CI caught
	// exactly that, sizing a 756x413 surface into a square 207x207. So poll
	// until two consecutive reads agree, poking a frame each time for the same
	// reason element capture does: a backgrounded tab runs no rendering steps on
	// its own, so waiting alone would never converge there.
	var vw, vh float64
	if err := c.run(ctx, id, bringToFront(), chromedp.ActionFunc(func(actx context.Context) error {
		t := time.NewTicker(60 * time.Millisecond)
		defer t.Stop()
		// Three consecutive agreeing reads, not two: a viewport mid-resize can
		// hold a transient value across a single 60ms gap, and sizing the whole
		// recording from it gives every frame the wrong aspect.
		const wantAgree = 3
		var prev Rect
		agree := 0
		for {
			pokeFrame(actx)
			m, err := layoutRects(actx)
			if err != nil {
				return err
			}
			v := m.viewport
			// Cross-check against the page's OWN view of the box. CDP's visual
			// viewport and innerWidth/innerHeight settle independently after a
			// resize, and CI caught them disagreeing — the page reported the new
			// 800x600 while getLayoutMetrics still said 600x600, so the whole
			// recording was sized from a square that never existed. Stability in
			// one measure is not evidence; agreement between two is.
			if v.Width > 0 && v.Height > 0 && !pageAgreesViewport(actx, v) {
				agree, prev = 0, Rect{}
				select {
				case <-actx.Done():
					return actx.Err()
				case <-t.C:
				}
				continue
			}
			if v.Width > 0 && v.Height > 0 && sameRect(prev, v) {
				if agree++; agree >= wantAgree-1 {
					vw, vh = v.Width, v.Height
					return nil
				}
			} else {
				agree = 0
			}
			prev = v
			select {
			case <-actx.Done():
				// Best effort beats refusing to record: a non-zero read is
				// still usable, and only the max box is at stake.
				if v.Width > 0 && v.Height > 0 {
					vw, vh = v.Width, v.Height
					return nil
				}
				return actx.Err()
			case <-t.C:
			}
		}
	})); err != nil {
		return nil, err
	}
	maxW, maxH := scaleBox(vw, vh, opts.Scale)

	r := newRecorder(opts, tctx, c.recMaxBytes)
	// Set before the recorder is published, so nothing can read it concurrently.
	r.release = func() { c.forgetRecorder(id, r) }
	c.recMu.Lock()
	if _, ok := c.rec[id]; ok {
		c.recMu.Unlock()
		return nil, ErrAlreadyRecording
	}
	c.rec[id] = r
	c.recMu.Unlock()

	if err := c.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
		p := page.StartScreencast().
			WithFormat(page.ScreencastFormatJpeg).
			WithQuality(int64(opts.Quality)).
			WithEveryNthFrame(1)
		if maxW > 0 && maxH > 0 {
			p = p.WithMaxWidth(maxW).WithMaxHeight(maxH)
		}
		return p.Do(actx)
	})); err != nil {
		// A refused screencast must not leave a recording nothing will ever
		// feed, or the next `record start` would fail with "already recording" —
		// nor a screencast running with no recording behind it.
		c.abandonRecorder(id, r)
		return nil, err
	}
	go r.pump()

	return map[string]any{
		"action":          "start",
		"recording":       true,
		"fps":             opts.FPS,
		"scale":           opts.Scale,
		"quality":         opts.Quality,
		"max_frames":      opts.MaxFrames,
		"max_duration_ms": opts.MaxDuration.Milliseconds(),
		"annotate":        opts.Annotate,
		// The caps requested of Chrome, not a promise about the frames: a
		// screencast frame is never UPSCALED to fill them, so the real
		// dimensions are whatever the compositor produces within this box and
		// are reported per-frame at `record stop`.
		"max_width":  maxW,
		"max_height": maxH,
	}, nil
}

// RecordStop ends the recording and returns its frames.
//
// The recording is removed whether or not the caller can encode what comes
// back: the frames are handed over in full, so nothing is lost by the daemon
// forgetting them. Whether the requested format can be produced at all is
// checked by the CLI BEFORE this call, which is what keeps a missing ffmpeg
// from costing the user their recording (VS-10).
func (c *CDP) RecordStop(ctx context.Context, id string) ([]Frame, map[string]any, error) {
	r := c.takeRecorder(id)
	if r == nil {
		return nil, nil, ErrNotRecording
	}
	r.finish(ctx, "")
	frames, meta := r.drain()
	meta["action"] = "stop"
	return frames, meta, nil
}

// RecordRestore re-seats a drained recording so a failed export can be retried.
//
// RecordStop is destructive by design — the frames are handed over in full, so
// nothing is lost by the daemon forgetting them — but that is only true when the
// caller succeeds in writing them. Everything the CLI can check before draining
// it does check (the encoder's availability, the output path), and this covers
// what is left: a full disk, an ffmpeg that dies, a frame that will not encode.
// Without it, one transient failure ends the recording and the retry answers
// "no recording is active on this tab".
//
// The restored recording is stopped: it holds frames for an export, it is not
// capturing. A recording that started on the tab meanwhile wins — clobbering a
// live capture to make room for a dead one would be the worse trade.
func (c *CDP) RecordRestore(_ context.Context, id string, frames []Frame, meta map[string]any) error {
	if len(frames) == 0 {
		return ErrNotRecording
	}
	r := restoredRecorder(frames, meta)
	c.recMu.Lock()
	defer c.recMu.Unlock()
	if _, ok := c.rec[id]; ok {
		return ErrAlreadyRecording
	}
	c.rec[id] = r
	return nil
}

// restoredRecorder builds a recorder that only holds frames.
//
// It has no tab context and no pump, so stopCapture must never reach Chrome:
// consuming stopOnce here is what guarantees that, and it also makes finish()
// return at once since exited is already closed.
func restoredRecorder(frames []Frame, meta map[string]any) *recorder {
	r := &recorder{
		started:  time.Now(),
		buf:      eventbuf.New[Frame](len(frames)),
		max:      len(frames),
		stopped:  true,
		restored: meta,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
	}
	r.stopOnce.Do(func() {})
	close(r.done)
	close(r.exited)
	for _, f := range frames {
		r.buf.Add(f)
		r.size = append(r.size, len(f.Data))
		r.bytes += len(f.Data)
	}
	return r
}

// RecordStatus reports the live state of a recording.
func (c *CDP) RecordStatus(_ context.Context, id string) (map[string]any, error) {
	r := c.recorder(id)
	if r == nil {
		return map[string]any{"action": "status", "recording": false, "frames": 0}, nil
	}
	m := r.stat()
	m["action"] = "status"
	m["recording"] = true
	return m, nil
}

// RecordCancel discards a recording.
func (c *CDP) RecordCancel(ctx context.Context, id string) (map[string]any, error) {
	r := c.takeRecorder(id)
	if r == nil {
		return nil, ErrNotRecording
	}
	r.finish(ctx, "")
	m := r.stat()
	m["action"] = "cancel"
	m["recording"] = false
	m["discarded"] = m["frames"]
	delete(m, "frames")
	return m, nil
}

// takeRecorder removes and returns a tab's recording.
func (c *CDP) takeRecorder(id string) *recorder {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	r := c.rec[id]
	delete(c.rec, id)
	return r
}

// forgetRecorder removes a recording only if it is still the one r names, so a
// failed start cannot delete a recording someone else began meanwhile.
func (c *CDP) forgetRecorder(id string, r *recorder) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	if c.rec[id] == r {
		delete(c.rec, id)
	}
}

// abandonRecorder tears down a recording that never got off the ground.
//
// Forgetting it is not enough. Page.startScreencast can time out CLIENT side
// after Chrome has already enabled it, and a screencast nobody acknowledges
// stays on for the life of the connection — the tab keeps composing frames for a
// recording that no longer exists. Stopping the capture is best-effort by
// design: if the call that failed was the enable, there is nothing to turn off.
func (c *CDP) abandonRecorder(id string, r *recorder) {
	c.forgetRecorder(id, r)
	r.stopCapture("")
}

// noteRecordMark records that an action landed at (x, y) on a tab, if that tab
// is being recorded.
//
// Marks are recorded whether or not --annotate was passed at start: they cost a
// few floats, and recording them unconditionally is what lets one capture be
// exported both annotated and clean (RFC-0011 design notes).
func (c *CDP) noteRecordMark(id, command string, x, y float64) {
	r := c.recorder(id)
	if r == nil {
		return
	}
	r.mark(FrameMark{X: x, Y: y, Command: command, TS: time.Now()})
}

func newRecorder(opts RecordOpts, tctx context.Context, maxBytes int) *recorder {
	return &recorder{
		opts:        opts,
		started:     time.Now(),
		tctx:        tctx,
		buf:         eventbuf.New[Frame](opts.MaxFrames),
		max:         opts.MaxFrames,
		maxBytes:    maxBytes,
		strandedTTL: recordStrandedTTL,
		wake:        make(chan struct{}, 1),
		done:        make(chan struct{}),
		exited:      make(chan struct{}),
	}
}

// releaseAfter forgets this recording once d has passed, if the CDP that owns it
// registered a way to.
func (r *recorder) releaseAfter(d time.Duration) {
	if r.release != nil && d > 0 {
		time.AfterFunc(d, r.release)
	}
}

// offer hands a frame from the CDP event loop to the pump. It must never block
// and must never issue a CDP call — it runs on chromedp's event loop.
func (r *recorder) offer(ev *page.EventScreencastFrame) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.lastFrame = time.Now()
	p := pendingFrame{sessionID: ev.SessionID, data: ev.Data, meta: ev.Metadata}
	if len(r.queue) >= recordQueueDepth {
		// Drop the payload, keep the acknowledgement. An unacked frame stalls
		// the screencast for good, so the one thing that may never be dropped is
		// the ack — and the loss is counted, not hidden.
		p.data, p.meta, p.ackOnly = "", nil, true
		r.evicted++
		r.truncated = true
	}
	r.queue = append(r.queue, p)
	r.mu.Unlock()

	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// mark appends an action marker, bounded.
func (r *recorder) mark(m FrameMark) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	if len(r.marks) >= recordMaxMarks {
		r.marks = r.marks[1:]
	}
	r.marks = append(r.marks, m)
}

// pump acknowledges and stores frames until the recording ends or its maximum
// duration elapses.
//
// It exists because the acknowledgement is a CDP call and the listener runs on
// chromedp's event loop, where issuing one would deadlock. Everything expensive
// — base64, image header decode, the byte accounting — happens here too, so the
// event loop only ever appends to a slice.
func (r *recorder) pump() {
	defer close(r.exited)
	deadline := time.NewTimer(r.opts.MaxDuration)
	defer deadline.Stop()
	gap := max(time.Duration(float64(time.Second)/r.opts.FPS), recordPokeFloor)
	nudge := time.NewTicker(gap)
	defer nudge.Stop()
	for {
		r.drainQueue()
		select {
		case <-r.wake:
		case <-nudge.C:
			r.pokeIfStalled(gap)
		case <-deadline.C:
			// The capture stops; the RECORDING does not. Whatever was captured
			// stays exportable, flagged truncated with the reason.
			r.stopCapture(recordReasonDuration)
			return
		case <-r.done:
			return
		case <-r.tctx.Done():
			// The tab closed. The frames survive it — they are held here, not
			// in the tab — so `record stop` still exports them. But not forever:
			// nothing else would ever remove this recording, and an abandoned
			// one holds up to record_max_bytes (96MB by default) for the life of
			// the daemon. The grace period is generous because the `record stop`
			// this exists for is the one a human types after a run died.
			r.stopCapture("")
			r.releaseAfter(r.strandedTTL)
			return
		}
	}
}

// pokeIfStalled nudges the page into producing a frame when it has stopped
// producing them on its own.
//
// This is the recording half of the problem pokeFrame already solves for
// element capture: a tab that is not the frontmost one — which is most tabs,
// for a tool that drives the user's real browser — produces no compositor
// frames, so a screencast of it goes silent even while the page is animating.
// Measured on a headless tab: three frames at startup, then nothing for twenty
// seconds. Waiting for a frame that is never coming would make `record` a
// feature that works only on the tab you are looking at.
//
// The nudge is a 1x1 screenshot, exactly as pokeFrame does, and it fires only
// when no frame has arrived for a whole cadence gap — so an actively rendering
// page is never poked. A page that genuinely did not change answers with an
// identical frame, which store() then discards as a duplicate: the promise that
// a static page costs nothing survives the nudge.
func (r *recorder) pokeIfStalled(gap time.Duration) {
	r.mu.Lock()
	stalled := r.stopped || time.Since(r.lastFrame) >= gap
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	if !stalled {
		return
	}
	ctx, cancel := context.WithTimeout(r.tctx, 5*time.Second)
	defer cancel()
	_ = chromedp.Run(ctx, chromedp.ActionFunc(func(actx context.Context) error {
		pokeFrame(actx)
		return nil
	}))
}

// drainQueue acks and stores everything the event loop has handed over.
func (r *recorder) drainQueue() {
	for {
		r.mu.Lock()
		if len(r.queue) == 0 || r.stopped {
			r.mu.Unlock()
			return
		}
		p := r.queue[0]
		r.queue = r.queue[1:]
		r.mu.Unlock()

		r.ack(p.sessionID)
		if !p.ackOnly {
			r.store(p)
		}
	}
}

// ack tells Chrome the frame arrived. Without it the screencast stops after the
// first frame, which is why this is the first thing done with every frame —
// including the ones the bounds discard.
func (r *recorder) ack(sessionID int64) {
	ctx, cancel := context.WithTimeout(r.tctx, 5*time.Second)
	defer cancel()
	// Best-effort: a failed ack means at worst a stalled screencast, which the
	// user sees as a short recording — never a failed command.
	_ = chromedp.Run(ctx, chromedp.ActionFunc(func(actx context.Context) error {
		return page.ScreencastFrameAck(sessionID).Do(actx)
	}))
}

// store retains one frame, applying the cadence throttle and both bounds.
func (r *recorder) store(p pendingFrame) {
	now := frameTime(p.meta)

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	// The cadence throttle. A frame skipped here is NOT a dropped frame: the
	// caller asked for this many frames a second, and counting the surplus as
	// loss would report every recording of a busy page as truncated.
	//
	// lastKept is deliberately NOT advanced here: the clock measures the gap
	// since the last RETAINED frame, and a frame that turns out to be
	// undecodable or a byte-for-byte duplicate (which is what a static page
	// answers a nudge with) retains nothing. Charging those to the budget
	// throttles away the genuine change arriving in the next gap.
	minGap := time.Duration(float64(time.Second) / r.opts.FPS)
	if !r.lastKept.IsZero() && now.Sub(r.lastKept) < minGap {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	raw, err := base64.StdEncoding.DecodeString(p.data)
	if err != nil || len(raw) == 0 {
		return
	}
	f := Frame{Data: raw, TS: now}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
		f.Width, f.Height = cfg.Width, cfg.Height
	}
	if p.meta != nil {
		f.CSSWidth, f.CSSHeight = p.meta.DeviceWidth, p.meta.DeviceHeight
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	// A frame identical to the last retained one is not a frame: it is what a
	// page that did not change answers a nudge with (see pokeIfStalled). Not a
	// drop either — nothing was lost.
	if bytes.Equal(r.lastData, raw) {
		return
	}
	// Retained: now the cadence clock advances.
	r.lastKept = now
	r.lastData = raw
	r.buf.Add(f)
	r.size = append(r.size, len(raw))
	r.bytes += len(raw)
	if len(r.size) > r.max {
		r.bytes -= r.size[0]
		r.size = r.size[1:]
		r.truncated = true
		if r.reason == "" {
			r.reason = recordReasonFrames
		}
	}
	r.enforceBytesLocked()
}

// enforceBytesLocked shrinks the ring until the retained frames fit the byte
// ceiling.
//
// The ring is REBUILT at a smaller size rather than evicted one frame at a
// time, because eventbuf's bound is a frame count and this bound is a byte
// total; converting one into the other is exactly what this does. The smaller
// size then persists for the rest of the recording, which is the honest reading
// of the ceiling: this tab's frames are large, so fewer of them fit.
func (r *recorder) enforceBytesLocked() {
	if r.maxBytes <= 0 || r.bytes <= r.maxBytes || len(r.size) <= recordMinRetained {
		return
	}
	// How many of the MOST RECENT frames fit — recency is what a recording is
	// for; the end of a run is the part someone wants to watch.
	keep, total := 0, 0
	for i := len(r.size) - 1; i >= 0; i-- {
		if total+r.size[i] > r.maxBytes && keep >= recordMinRetained {
			break
		}
		total += r.size[i]
		keep++
	}
	keep = max(keep, recordMinRetained)
	if keep >= len(r.size) {
		return
	}

	res := r.buf.Query(eventbuf.Query[Frame]{Limit: keep})
	// Everything the old ring had evicted, plus what this rebuild discards, is
	// carried forward: a fresh buffer's counter starts at zero and the total
	// must not.
	r.evicted += r.buf.Dropped() + (res.Buffered - len(res.Entries))
	r.buf = eventbuf.New[Frame](keep)
	for _, f := range res.Entries {
		r.buf.Add(f)
	}
	r.size = r.size[len(r.size)-len(res.Entries):]
	r.bytes = 0
	for _, s := range r.size {
		r.bytes += s
	}
	r.max = keep
	r.truncated = true
	r.reason = recordReasonBytes
}

// finish stops the capture and waits for the pump to leave, so nothing is
// still writing into the buffer when the caller drains it.
func (r *recorder) finish(ctx context.Context, reason string) {
	r.stopCapture(reason)
	select {
	case <-r.exited:
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
	}
}

// stopCapture turns the screencast off, once.
func (r *recorder) stopCapture(reason string) {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.stopped = true
		if reason != "" {
			r.truncated, r.reason = true, reason
		}
		r.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.tctx), 5*time.Second)
		defer cancel()
		_ = chromedp.Run(ctx, chromedp.ActionFunc(func(actx context.Context) error {
			return page.StopScreencast().Do(actx)
		}))
		close(r.done)
	})
}

// drain returns the retained frames with their marks attached, plus the
// accounting the envelope reports.
func (r *recorder) drain() ([]Frame, map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.buf.Query(eventbuf.Query[Frame]{})
	frames := attachMarks(res.Entries, r.marks)
	m := r.statLocked()
	m["frames"] = len(frames)
	return frames, m
}

// stat is the live snapshot `record status` reports.
func (r *recorder) stat() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statLocked()
}

func (r *recorder) statLocked() map[string]any {
	if r.restored != nil {
		// A re-seated recording reports the CAPTURE's accounting: how much the
		// ring evicted is a fact about the run, and recomputing it from a buffer
		// that was filled by a restore would report a clean recording that was
		// not one.
		out := make(map[string]any, len(r.restored)+1)
		for k, v := range r.restored {
			out[k] = v
		}
		out["capturing"] = false
		out["restored"] = true
		return out
	}
	dropped := r.evicted + r.buf.Dropped()
	return map[string]any{
		"frames":         r.buf.Len(),
		"dropped_frames": dropped,
		"truncated":      r.truncated || dropped > 0,
		"reason":         r.reason,
		"elapsed_ms":     time.Since(r.started).Milliseconds(),
		"marks":          len(r.marks),
		"fps":            r.opts.FPS,
		"scale":          r.opts.Scale,
		"annotate":       r.opts.Annotate,
		"capturing":      !r.stopped,
	}
}

// attachMarks distributes the action markers over the frames they overlap.
//
// A mark belongs on every frame captured from shortly before the action until
// recordMarkLinger after it, so the marker is visible at 4fps rather than for a
// single frame nobody will pause on. When no frame falls in that window — the
// action changed nothing, so the page produced no frame — it lands on the
// nearest frame instead: a marker with nowhere to go would silently disappear,
// which for an annotated recording is indistinguishable from the click not
// having happened.
//
// It is a pure function over frames and marks, and is tested as one.
func attachMarks(frames []Frame, marks []FrameMark) []Frame {
	if len(frames) == 0 || len(marks) == 0 {
		return frames
	}
	out := make([]Frame, len(frames))
	copy(out, frames)
	for _, m := range marks {
		hit := false
		for i := range out {
			ts := out[i].TS
			if ts.IsZero() {
				continue
			}
			if !ts.Before(m.TS.Add(-recordMarkPreroll)) && !ts.After(m.TS.Add(recordMarkLinger)) {
				out[i].Marks = append(out[i].Marks, m)
				hit = true
			}
		}
		if hit {
			continue
		}
		if i := nearestFrame(out, m.TS); i >= 0 {
			out[i].Marks = append(out[i].Marks, m)
		}
	}
	return out
}

// nearestFrame returns the index of the frame closest in time to ts, or -1 when
// no frame carries a timestamp.
func nearestFrame(frames []Frame, ts time.Time) int {
	best, bestD := -1, time.Duration(math.MaxInt64)
	for i := range frames {
		if frames[i].TS.IsZero() {
			continue
		}
		d := frames[i].TS.Sub(ts)
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// frameTime is a screencast frame's wall-clock capture time, defaulting to now
// for a frame that carries no metadata timestamp.
func frameTime(meta *page.ScreencastFrameMetadata) time.Time {
	if meta != nil && meta.Timestamp != nil {
		if t := meta.Timestamp.Time(); !t.IsZero() {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}
