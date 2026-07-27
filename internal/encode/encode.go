// Package encode turns captured frames into a shareable artifact: an animated
// GIF, a numbered PNG directory, or — when ffmpeg is on PATH — an MP4/WebM.
//
// It is deliberately PURE with respect to the browser: the input is a slice of
// already-captured frames plus options, and the output is bytes. There is no
// CDP, no context, and no knowledge of how the frames were produced. That is
// the whole reason RFC-0011 splits it out — frame timing through a real Chrome
// is inherently variable, so the parts that must be exactly right (the palette,
// the frame delays, the annotation compositing, the --max-size reduction) are
// tested here on synthetic frames instead.
//
// Two properties are load-bearing and any change must preserve both:
//
//   - Annotation is composited HERE, at export, never at capture. Without
//     Options.Annotate the exported frames are pixel-identical to what was
//     captured, which is what makes a recording usable as a README asset
//     (RFC-0011 US-5 / VS-13) and what lets one capture be exported both ways.
//
//   - The GIF path uses only the standard library's image/gif. A Go CLI
//     distributed as a single static binary must be able to produce its default
//     format with no external program; ffmpeg is required only for mp4/webm,
//     and its absence is a NAMED error rather than a silent fallback (VS-10).
package encode

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	_ "image/jpeg" // registered so a screencast JPEG frame decodes
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Format names an export format.
type Format string

// The export formats. gif is the default because it is the one that works with
// no external dependency and embeds everywhere a bug report or README does
// (RFC-0011 open question 2).
const (
	FormatGIF    Format = "gif"
	FormatMP4    Format = "mp4"
	FormatWebM   Format = "webm"
	FormatFrames Format = "frames"
)

// ErrNoEncoder reports that the requested format needs a program that is not
// installed. It is a distinct sentinel because the CLI maps it to `usage` /
// exit 2 with a message naming the requirement, and — crucially — checks it
// BEFORE draining the recording, so the frames stay exportable as GIF (VS-10).
var ErrNoEncoder = errors.New("no encoder available for this format")

// ErrNoFrames reports an export with nothing to export.
var ErrNoFrames = errors.New("no frames were captured")

// Bounds on the encoder's own numbers. They are contract, not taste: a GIF
// delay of 0 is rendered as "as fast as possible" by some viewers and as 10
// by others, and a 30-second frame reads as a broken file.
const (
	minDelayCS = 2   // 0.02s — the shortest delay that renders consistently
	maxDelayCS = 500 // 5s — longer than this looks like a stalled animation
	minCanvas  = 16  // px; below this a reduction has destroyed the artifact

	// maxReductionSteps bounds the --max-size loop. See reduce().
	maxReductionSteps = 8

	// paletteSize is a GIF's hard limit; the whole palette is used.
	paletteSize = 256

	// exactPaletteLimit is when a frame set is reproduced EXACTLY: at most 256
	// distinct colours means the palette can hold every one of them, so the
	// decoded GIF matches the capture pixel for pixel. Flat UI screenshots
	// frequently land here, and every synthetic test frame does.
	exactPaletteLimit = paletteSize
)

// Mark is one action to draw on the frames it overlaps: where the pointer
// landed, and which command put it there.
//
// The coordinates are PAGE (CSS) pixels, the same space the pointer verbs
// resolve and report, so nothing here has to know about device scale factors —
// Frame.CSSWidth/CSSHeight carry the mapping onto image pixels.
type Mark struct {
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Command string  `json:"command,omitempty"`
}

// Frame is one captured frame plus what is needed to draw on it.
//
// Data is the image exactly as Chrome produced it (a screencast JPEG, or a PNG
// in tests); it is decoded here and never re-compressed lossily.
type Frame struct {
	Data []byte    `json:"data"`
	TS   time.Time `json:"ts"`

	// CSSWidth/CSSHeight are the frame's size in page CSS pixels. They exist so
	// a Mark's page coordinates map onto image pixels when the capture was
	// scaled down. Zero means "the image's own pixel dimensions", i.e. 1:1.
	CSSWidth  float64 `json:"css_width,omitempty"`
	CSSHeight float64 `json:"css_height,omitempty"`

	// Marks are the actions that landed while this frame was on screen. They are
	// carried, not drawn: Options.Annotate decides at export whether they become
	// pixels.
	Marks []Mark `json:"marks,omitempty"`
}

// Options controls one export.
type Options struct {
	Format Format

	// FPS is the playback cadence used when the frames carry no usable
	// timestamps, and the floor/ceiling is applied to timestamp-derived delays
	// either way.
	FPS float64

	// Loop is the GIF loop count: 0 loops forever, n > 0 plays n times.
	Loop int

	// Annotate composites the position markers. Off by default — a README asset
	// must not carry the tool's own drawing (US-5).
	Annotate bool

	// MaxBytes is a best-effort size ceiling. See reduce(): the export is
	// re-encoded at reduced scale (and then a reduced frame count) until it
	// fits, and the values actually used are reported in Result.
	MaxBytes int
}

// File is one numbered frame of a `frames` export. The caller writes it; this
// package does no I/O of its own for that format.
type File struct {
	Name string
	Data []byte
}

// Result is the artifact plus everything the envelope reports about it. Every
// field is a fact about what was PRODUCED, not what was requested, which is the
// point: --max-size can change the scale and the frame count, and a caller that
// only saw its own flags back would not know.
type Result struct {
	Data  []byte // gif/mp4/webm bytes (nil for FormatFrames)
	Files []File // FormatFrames only

	Format     Format
	Frames     int
	FPS        float64
	Scale      float64 // 1 unless --max-size forced a reduction
	Width      int
	Height     int
	Bytes      int
	DurationMs int64
	Annotated  bool
	Reduced    bool // --max-size changed the scale and/or the frame count

	// WithinMaxSize reports whether the ceiling was actually met. False means
	// the reduction ladder bottomed out first — a best-effort bound, reported
	// rather than silently missed.
	WithinMaxSize bool
}

// ParseFormat resolves a user-supplied format name.
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(strings.TrimSpace(s))); f {
	case FormatGIF, FormatMP4, FormatWebM, FormatFrames:
		return f, nil
	case "":
		return "", fmt.Errorf("empty format")
	default:
		return "", fmt.Errorf("unknown format %q: want gif, mp4, webm or frames", s)
	}
}

// FormatFromPath infers the format from an output path's extension, reporting
// false when the path carries no extension this package recognises.
func FormatFromPath(path string) (Format, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".gif":
		return FormatGIF, true
	case ".mp4":
		return FormatMP4, true
	case ".webm":
		return FormatWebM, true
	}
	return "", false
}

// Extensions lists the file extensions a format is inferred from, for error
// messages.
func Extensions() string { return ".gif, .mp4, .webm" }

// Available reports whether this machine can produce the format, wrapping
// ErrNoEncoder with the missing requirement NAMED.
//
// The CLI calls it before the recording is drained, so a missing ffmpeg costs
// the user an exit 2 and nothing else — the frames are still in the daemon and
// still exportable as GIF (VS-10).
func Available(f Format) error {
	switch f {
	case FormatGIF, FormatFrames:
		return nil
	case FormatMP4, FormatWebM:
		if _, err := exec.LookPath(ffmpegBin); err != nil {
			return fmt.Errorf("%w: %s export needs %q on PATH (install ffmpeg, or export --format gif, which needs nothing)", ErrNoEncoder, f, ffmpegBin)
		}
		return nil
	default:
		return fmt.Errorf("unknown format %q", f)
	}
}

// IsNoEncoder reports whether err is the missing-encoder condition.
func IsNoEncoder(err error) bool { return errors.Is(err, ErrNoEncoder) }

// Encode renders frames into the requested artifact.
func Encode(frames []Frame, opts Options) (Result, error) {
	if len(frames) == 0 {
		return Result{}, ErrNoFrames
	}
	if err := Available(opts.Format); err != nil {
		return Result{}, err
	}
	if opts.FPS <= 0 {
		opts.FPS = 4
	}
	imgs, err := decodeAll(frames)
	if err != nil {
		return Result{}, err
	}
	return reduce(frames, imgs, opts)
}

// reduce runs the export, re-running it at a smaller scale (and then a lower
// frame count) until it fits Options.MaxBytes.
//
// TERMINATION is the property that matters here, because this is a loop whose
// exit condition depends on a compressor's output size — something no
// arithmetic can predict. Three independent bounds guarantee it stops:
//
//  1. every step strictly shrinks the plan (the scale drops by at least 20%,
//     or the frame stride doubles), so the sequence is strictly decreasing;
//  2. the plan is refused once the canvas would fall below minCanvas or fewer
//     than two frames would survive, so the sequence is bounded below;
//  3. maxReductionSteps caps the iteration count regardless.
//
// When it bottoms out without meeting the ceiling the best effort is returned
// with WithinMaxSize false, which is the honest answer: --max-size is
// documented as best-effort, and silently returning something larger without
// saying so would be the failure mode.
func reduce(frames []Frame, imgs []image.Image, opts Options) (Result, error) {
	p := plan{scale: 1, stride: 1}
	for step := 0; ; step++ {
		res, err := encodeAt(frames, imgs, opts, p)
		if err != nil {
			return Result{}, err
		}
		res.Reduced = p.scale < 1 || p.stride > 1
		res.WithinMaxSize = opts.MaxBytes <= 0 || res.Bytes <= opts.MaxBytes
		if res.WithinMaxSize || step >= maxReductionSteps {
			return res, nil
		}
		next, ok := p.next(res.Bytes, opts.MaxBytes, imgs[0], len(imgs))
		if !ok {
			return res, nil
		}
		p = next
	}
}

// plan is one attempt's reduction state: a scale factor on the canvas and a
// stride that keeps every n'th frame.
type plan struct {
	scale  float64
	stride int
}

// next returns the next, strictly smaller plan, or false when there is no
// smaller one worth trying.
//
// Scale is reduced first and frames only after it bottoms out, because dropping
// resolution degrades a recording far more gracefully than dropping frames: a
// slightly soft animation still shows what happened, while a decimated one
// starts skipping the actions the recording exists to show.
func (p plan) next(got, want int, first image.Image, count int) (plan, bool) {
	if p.scale > minScaleFactor {
		// Compressed size tracks pixel AREA, so the linear factor that would
		// hit the target is sqrt(want/got); 0.9 of it leaves headroom for the
		// fact that compression does not scale perfectly linearly, and the
		// [0.3, 0.8] clamp keeps every step both a real reduction and not a
		// wild overshoot.
		f := clampFloat(math.Sqrt(float64(want)/float64(got))*0.9, 0.3, 0.8)
		s := math.Max(p.scale*f, minScaleFactor)
		b := first.Bounds()
		if int(float64(b.Dx())*s) >= minCanvas && int(float64(b.Dy())*s) >= minCanvas && s < p.scale {
			return plan{scale: s, stride: p.stride}, true
		}
	}
	if count/(p.stride*2) >= 2 {
		return plan{scale: p.scale, stride: p.stride * 2}, true
	}
	return plan{}, false
}

// minScaleFactor is the floor on --max-size scale reduction: below a tenth of
// the capture there is nothing recognisable left to look at.
const minScaleFactor = 0.1

// encodeAt renders one attempt at the given plan.
func encodeAt(frames []Frame, imgs []image.Image, opts Options, p plan) (Result, error) {
	kept, keptImgs := stride(frames, imgs, p.stride)
	w, h := scaledSize(imgs[0], p.scale)
	canvas := make([]*image.RGBA, 0, len(keptImgs))
	annotated := false
	for i, src := range keptImgs {
		dst, pl := render(src, w, h)
		if opts.Annotate && drawMarks(dst, kept[i], src, pl) {
			annotated = true
		}
		canvas = append(canvas, dst)
	}
	d := delays(kept, opts.FPS)

	res := Result{
		Format: opts.Format,
		Frames: len(canvas),
		Scale:  p.scale,
		Width:  w,
		Height: h,
		// What was DRAWN, not what was asked for: a recording with no actions in
		// it, or whose only mark fell outside the frame, produces no markers, and
		// reporting `annotated: true` over an unmarked GIF is how a reader
		// concludes the click never happened.
		Annotated: annotated,
	}
	for _, cs := range d {
		res.DurationMs += int64(cs) * 10
	}
	if res.DurationMs > 0 {
		res.FPS = math.Round(float64(len(canvas))/(float64(res.DurationMs)/1000)*100) / 100
	} else {
		res.FPS = opts.FPS
	}

	switch opts.Format {
	case FormatGIF:
		data, err := encodeGIF(canvas, d, opts.Loop)
		if err != nil {
			return Result{}, err
		}
		res.Data, res.Bytes = data, len(data)
	case FormatFrames:
		files, err := encodePNGs(canvas)
		if err != nil {
			return Result{}, err
		}
		res.Files = files
		for _, f := range files {
			res.Bytes += len(f.Data)
		}
	case FormatMP4, FormatWebM:
		data, err := encodeVideo(canvas, opts.Format, res.FPS)
		if err != nil {
			return Result{}, err
		}
		res.Data, res.Bytes = data, len(data)
	default:
		return Result{}, fmt.Errorf("unknown format %q", opts.Format)
	}
	return res, nil
}

// stride keeps every n'th frame (n == 1 keeps all of them).
//
// A dropped frame's MARKS do not go with it: they move to the nearest kept
// frame. attachMarks takes real trouble to guarantee a marker never disappears —
// one with nowhere to land is indistinguishable from the click not having
// happened — and decimating the frames to meet --max-size must not undo that.
//
// The input frames are shared with every other reduction attempt, so the marks
// are appended to copies rather than to the caller's slices.
func stride(frames []Frame, imgs []image.Image, n int) ([]Frame, []image.Image) {
	if n <= 1 {
		return frames, imgs
	}
	outF := make([]Frame, 0, len(frames)/n+1)
	outI := make([]image.Image, 0, len(imgs)/n+1)
	for i := 0; i < len(frames); i += n {
		f := frames[i]
		f.Marks = append([]Mark(nil), f.Marks...)
		outF = append(outF, f)
		outI = append(outI, imgs[i])
	}
	for i := range frames {
		if i%n == 0 || len(frames[i].Marks) == 0 {
			continue
		}
		k := min((i+n/2)/n, len(outF)-1) // the kept frame nearest in time
		outF[k].Marks = append(outF[k].Marks, frames[i].Marks...)
	}
	return outF, outI
}

// decodeAll decodes every captured frame once, so a re-encode at a smaller
// scale costs no second decode.
func decodeAll(frames []Frame) ([]image.Image, error) {
	out := make([]image.Image, 0, len(frames))
	for i, f := range frames {
		img, _, err := image.Decode(bytes.NewReader(f.Data))
		if err != nil {
			return nil, fmt.Errorf("frame %d does not decode as an image: %w", i, err)
		}
		out = append(out, img)
	}
	return out, nil
}

// scaledSize is the canvas size at a scale, never smaller than one pixel.
func scaledSize(first image.Image, scale float64) (int, int) {
	b := first.Bounds()
	w := int(math.Round(float64(b.Dx()) * scale))
	h := int(math.Round(float64(b.Dy()) * scale))
	return max(w, 1), max(h, 1)
}

// delays computes the per-frame delay in hundredths of a second.
//
// The capture's own timestamps are preferred, because a screencast pushes
// frames only when the page changes: a five-second pause on a static page is
// real information, and replacing it with a fixed 1/fps cadence would render
// the recording as if the automation had raced through it. fps is the fallback
// for frames that carry no usable timestamp, and its reciprocal is what the
// last frame gets (there is no next frame to subtract from).
func delays(frames []Frame, fps float64) []int {
	base := clampInt(int(math.Round(100/fps)), minDelayCS, maxDelayCS)
	out := make([]int, len(frames))
	for i := range frames {
		if i == len(frames)-1 {
			out[i] = base
			continue
		}
		a, b := frames[i].TS, frames[i+1].TS
		if a.IsZero() || b.IsZero() || !b.After(a) {
			out[i] = base
			continue
		}
		out[i] = clampInt(int(math.Round(b.Sub(a).Seconds()*100)), minDelayCS, maxDelayCS)
	}
	return out
}

// gifLoopCount converts Options.Loop (a PLAY count) into image/gif's LoopCount,
// which is not one.
//
// The stdlib's contract is that an animation plays LoopCount+1 times, that 0
// means forever, and that a negative LoopCount writes no NETSCAPE block at all —
// which every viewer renders as a single play. Assigning the play count straight
// through, as this once did, gives `--loop 3` a GIF that plays four times; the
// error is invisible in a round-trip test because both ends agree on the wrong
// number.
func gifLoopCount(plays int) int {
	switch {
	case plays <= 0:
		return 0 // forever
	case plays == 1:
		return -1 // no NETSCAPE block: exactly one play
	default:
		return plays - 1
	}
}

// encodeGIF writes the animation with a palette shared across every frame.
func encodeGIF(canvas []*image.RGBA, delay []int, loop int) ([]byte, error) {
	q := newQuantizer(canvas)
	g := &gif.GIF{
		Image:     make([]*image.Paletted, 0, len(canvas)),
		Delay:     delay,
		LoopCount: gifLoopCount(loop),
	}
	for _, c := range canvas {
		g.Image = append(g.Image, q.paletted(c))
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		return nil, fmt.Errorf("gif encoding failed: %w", err)
	}
	return buf.Bytes(), nil
}

// encodePNGs renders the numbered-PNG escape hatch. PNG is lossless, so these
// frames are the capture's pixels exactly — which is what makes VS-13 ("without
// --annotate the export is pixel-identical to the capture") checkable at all.
func encodePNGs(canvas []*image.RGBA) ([]File, error) {
	out := make([]File, 0, len(canvas))
	for i, c := range canvas {
		var buf bytes.Buffer
		if err := png.Encode(&buf, c); err != nil {
			return nil, fmt.Errorf("png encoding failed on frame %d: %w", i, err)
		}
		out = append(out, File{Name: fmt.Sprintf("frame-%05d.png", i+1), Data: buf.Bytes()})
	}
	return out, nil
}

// ffmpegBin is the program the video formats shell out to. Named once so the
// error message, the availability probe, and the invocation cannot drift.
const ffmpegBin = "ffmpeg"

// encodeVideo shells out to ffmpeg over a temporary PNG sequence.
//
// The frames go through the filesystem rather than a pipe because a PNG
// sequence is the one input every ffmpeg build reads identically, and the
// temporary directory is removed whatever happens.
func encodeVideo(canvas []*image.RGBA, f Format, fps float64) ([]byte, error) {
	if err := Available(f); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "chrome-cdp-record-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	files, err := encodePNGs(canvas)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file.Name), file.Data, 0o600); err != nil {
			return nil, err
		}
	}
	out := filepath.Join(dir, "out."+string(f))
	args := []string{
		"-y", "-loglevel", "error",
		"-framerate", fmt.Sprintf("%g", math.Max(fps, 1)),
		"-i", filepath.Join(dir, "frame-%05d.png"),
		// H.264/VP9 in yuv420p require even dimensions, and a capture scaled by
		// an arbitrary factor is odd about half the time.
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
		"-pix_fmt", "yuv420p",
	}
	if f == FormatWebM {
		args = append(args, "-c:v", "libvpx-vp9", "-b:v", "0", "-crf", "34")
	}
	args = append(args, out)

	cmd := exec.Command(ffmpegBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s failed to encode the %s: %v: %s", ffmpegBin, f, err, strings.TrimSpace(stderr.String()))
	}
	return os.ReadFile(out)
}

// markRadius is the marker's inner radius in CANVAS pixels at scale 1. Big
// enough to find in a screenshot, small enough not to hide what was clicked.
const markRadius = 9

// drawMarks composites the position markers for one frame.
//
// A marker is a red disc inside a white ring: two contrasting colours, because
// a single-colour marker disappears against a page that happens to be that
// colour, and a "where did the click land" artifact that is invisible on some
// pages is worse than none. There is no caption bar — RFC-0011 open question 3
// settles on position markers only.
//
// pl is where this frame's content actually sits on the canvas, which is not the
// whole canvas once a differently-shaped frame has been letterboxed. The marks
// are in PAGE coordinates and belong on the content, so the mapping has to go
// through pl rather than through dst.Bounds().
// It reports whether any marker actually put pixels on the canvas, which is
// what Result.Annotated has to mean: a mark resolving to a point outside the
// frame draws nothing, and claiming otherwise tells a reader the click did not
// happen.
func drawMarks(dst *image.RGBA, f Frame, src image.Image, pl placement) bool {
	if len(f.Marks) == 0 {
		return false
	}
	sb := src.Bounds()
	// Page CSS pixels -> canvas pixels. CSSWidth is what the capture covered in
	// page coordinates; without it the source image's own size is the best
	// available assumption (a 1:1 capture).
	cw, ch := f.CSSWidth, f.CSSHeight
	if cw <= 0 {
		cw = float64(sb.Dx())
	}
	if ch <= 0 {
		ch = float64(sb.Dy())
	}
	kx, ky := float64(pl.w)/cw, float64(pl.h)/ch
	r := int(math.Round(markRadius * math.Min(kx, ky)))
	r = max(r, 3)

	red := color.RGBA{R: 0xE1, G: 0x1D, B: 0x48, A: 0xFF}
	white := color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	drew := false
	for _, m := range f.Marks {
		cx := pl.x + int(math.Round(m.X*kx))
		cy := pl.y + int(math.Round(m.Y*ky))
		if disc(dst, cx, cy, r+2, white) {
			drew = true
		}
		disc(dst, cx, cy, r, red)
	}
	return drew
}

// disc fills a filled circle, clipped to the image, reporting whether any pixel
// landed inside it.
func disc(dst *image.RGBA, cx, cy, r int, c color.RGBA) bool {
	drew := false
	b := dst.Bounds()
	for y := cy - r; y <= cy+r; y++ {
		if y < b.Min.Y || y >= b.Max.Y {
			continue
		}
		dy := y - cy
		for x := cx - r; x <= cx+r; x++ {
			if x < b.Min.X || x >= b.Max.X {
				continue
			}
			dx := x - cx
			if dx*dx+dy*dy <= r*r {
				dst.SetRGBA(x, y, c)
				drew = true
			}
		}
	}
	return drew
}

// placement is where one frame's content sits on the export canvas: the origin
// and size in canvas pixels. For a frame with the canvas's own aspect ratio it
// is the whole canvas; for any other it is the letterboxed rectangle inside it.
type placement struct{ x, y, w, h int }

// padColor fills the letterbox bars. Opaque black, both because it is the
// convention every video player uses and because it cannot be mistaken for page
// content the way a white or grey bar can.
var padColor = color.RGBA{A: 0xFF}

// render draws src onto a w×h canvas, PRESERVING its aspect ratio.
//
// A window resized mid-recording changes the frame shape, and every frame after
// the resize would otherwise be stretched onto the first frame's canvas — a
// recording that shows a page which was never on screen. Padding instead keeps
// every frame truthful at the cost of some bars.
//
// The common case (every frame the same shape) fits exactly, pads nothing, and
// costs one comparison — including the 1:1 case VS-13 rests on.
func render(src image.Image, w, h int) (*image.RGBA, placement) {
	pl := fit(src.Bounds(), w, h)
	if pl.x == 0 && pl.y == 0 && pl.w == w && pl.h == h {
		return resize(src, w, h), pl
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.SetRGBA(x, y, padColor)
		}
	}
	content := resize(src, pl.w, pl.h)
	for y := 0; y < pl.h; y++ {
		for x := 0; x < pl.w; x++ {
			dst.SetRGBA(pl.x+x, pl.y+y, content.RGBAAt(x, y))
		}
	}
	return dst, pl
}

// fit is the largest rectangle with src's aspect ratio that fits a w×h canvas,
// centred.
func fit(src image.Rectangle, w, h int) placement {
	sw, sh := src.Dx(), src.Dy()
	if sw <= 0 || sh <= 0 {
		return placement{w: w, h: h}
	}
	// sw/sh <= w/h means the source is the narrower shape, so height is the
	// binding axis and the width is derived from it (and vice versa). Comparing
	// cross-products keeps the decision exact rather than float-rounded.
	if h*sw <= w*sh {
		cw := min(max(int(math.Round(float64(h)*float64(sw)/float64(sh))), 1), w)
		return placement{x: (w - cw) / 2, w: cw, h: h}
	}
	ch := min(max(int(math.Round(float64(w)*float64(sh)/float64(sw))), 1), h)
	return placement{y: (h - ch) / 2, w: w, h: ch}
}

// resize renders src onto a w×h RGBA canvas.
//
// Downscaling box-averages (a nearest-neighbour shrink of a text-heavy page
// turns readable UI into noise); upscaling and the 1:1 case sample directly,
// which keeps "no reduction requested" an exact copy of the capture — the
// property VS-13 rests on.
func resize(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if w == b.Dx() && h == b.Dy() {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dst.Set(x, y, src.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return dst
	}
	sxf := float64(b.Dx()) / float64(w)
	syf := float64(b.Dy()) / float64(h)
	for y := 0; y < h; y++ {
		y0 := b.Min.Y + int(float64(y)*syf)
		y1 := b.Min.Y + int(float64(y+1)*syf)
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0 := b.Min.X + int(float64(x)*sxf)
			x1 := b.Min.X + int(float64(x+1)*sxf)
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var rs, gs, bs, as, n uint64
			for yy := y0; yy < y1 && yy < b.Max.Y; yy++ {
				for xx := x0; xx < x1 && xx < b.Max.X; xx++ {
					r, g, bb, a := src.At(xx, yy).RGBA()
					rs += uint64(r >> 8)
					gs += uint64(g >> 8)
					bs += uint64(bb >> 8)
					as += uint64(a >> 8)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(rs / n), G: uint8(gs / n), B: uint8(bs / n), A: uint8(as / n),
			})
		}
	}
	return dst
}

// quantizer maps the frames' colours onto one shared GIF palette.
//
// Two regimes, and the split is deliberate. A frame set with at most 256
// distinct colours — every flat UI, and every synthetic test frame — gets a
// palette of exactly those colours and an exact lookup, so the decoded GIF is
// the capture pixel for pixel. Anything richer gets the 256 most frequent
// colours and a nearest-colour lookup cached in a 5-bit-per-channel table,
// which keeps a 47-frame export from spending minutes in a linear palette
// search. Dithering is deliberately not applied: it triples the encoded size of
// a screenshot for a page that is mostly flat colour, which is the opposite of
// what an artifact meant to be attached to an issue needs.
type quantizer struct {
	pal   color.Palette
	exact map[color.RGBA]uint8
	cache []int16 // 32768 buckets; -1 = not yet resolved
}

func newQuantizer(canvas []*image.RGBA) *quantizer {
	counts := map[color.RGBA]int{}
	overflow := false
	for _, c := range canvas {
		b := c.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				px := c.RGBAAt(x, y)
				px.A = 0xFF // GIF here is fully opaque; alpha would split colours pointlessly
				if _, seen := counts[px]; !seen && len(counts) >= 1<<16 {
					overflow = true
					continue
				}
				counts[px]++
			}
		}
	}
	q := &quantizer{}
	if !overflow && len(counts) <= exactPaletteLimit {
		q.exact = make(map[color.RGBA]uint8, len(counts))
		for _, c := range sortedColors(counts) {
			q.exact[c] = uint8(len(q.pal))
			q.pal = append(q.pal, c)
		}
		if len(q.pal) == 0 {
			q.pal = color.Palette{color.RGBA{A: 0xFF}}
		}
		return q
	}
	for _, c := range sortedColors(counts) {
		if len(q.pal) >= paletteSize {
			break
		}
		q.pal = append(q.pal, c)
	}
	q.cache = make([]int16, 1<<15)
	for i := range q.cache {
		q.cache[i] = -1
	}
	return q
}

// sortedColors orders the counted colours by frequency, breaking ties on the
// packed RGB value so a palette is deterministic and a test can assert on it.
func sortedColors(counts map[color.RGBA]int) []color.RGBA {
	out := make([]color.RGBA, 0, len(counts))
	for c := range counts {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return packRGB(out[i]) < packRGB(out[j])
	})
	return out
}

func packRGB(c color.RGBA) uint32 {
	return uint32(c.R)<<16 | uint32(c.G)<<8 | uint32(c.B)
}

// paletted converts one canvas frame using the shared palette.
func (q *quantizer) paletted(src *image.RGBA) *image.Paletted {
	b := src.Bounds()
	dst := image.NewPaletted(b, q.pal)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			px := src.RGBAAt(x, y)
			px.A = 0xFF
			dst.SetColorIndex(x, y, q.index(px))
		}
	}
	return dst
}

// index resolves one colour to a palette index.
func (q *quantizer) index(c color.RGBA) uint8 {
	if q.exact != nil {
		if i, ok := q.exact[c]; ok {
			return i
		}
	}
	bucket := int(c.R>>3)<<10 | int(c.G>>3)<<5 | int(c.B>>3)
	if q.cache != nil {
		if i := q.cache[bucket]; i >= 0 {
			return uint8(i)
		}
	}
	i := uint8(q.pal.Index(c))
	if q.cache != nil {
		q.cache[bucket] = int16(i)
	}
	return i
}

func clampInt(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

func clampFloat(v, lo, hi float64) float64 {
	return math.Min(math.Max(v, lo), hi)
}
