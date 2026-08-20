package cli

// The `record` verbs and `session --record` (RFC-0011).
//
// Everything a user can spell — the format, the size ceiling, the loop count,
// the fps and scale bounds — is validated HERE, before a target is resolved and
// before Chrome is contacted. That is not decoration: `record stop --format mp4`
// on a machine with no ffmpeg has to be exit 2 with the recording UNTOUCHED, so
// the frames stay exportable as a GIF (VS-10). Draining the recording first and
// then discovering the encoder is missing would destroy the very thing the user
// was trying to keep.

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/encode"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// Bounds on the record flags. Like the capture verbs' bounds these are
// contract: --scale 0 records zero-pixel frames and --fps 500 fills the ring
// with frames nobody will ever see, and both are better refused up front.
const (
	recordFPSMin    float64 = 0.1
	recordFPSMax    float64 = 60
	recordScaleMin  float64 = 0.1
	recordScaleMax  float64 = 1
	recordFramesMin         = 2
	recordFramesMax         = 100000
)

// recordPrivacyNote is the sentence this feature owes the user. It records the
// REAL browser, logged into the real accounts, and the CLI cannot know which
// pixels are sensitive — so it says so where the command is read.
const recordPrivacyNote = "This records your real, logged-in browser. A recording attached to a public\n" +
	"issue may contain your own data — your tabs, your name, whatever the page was\n" +
	"showing. Nothing here can know what is sensitive; look at the file before you\n" +
	"share it."

func (a *App) cmdRecord() *cobra.Command {
	c := &cobra.Command{
		Use:   "record",
		Short: "Record the tab while other commands drive it, and export it as a GIF",
		Long: "Record the target tab while other commands drive it, then export the frames\n" +
			"as an animated GIF (or an MP4/WebM, or a directory of numbered PNGs).\n\n" +
			"  chrome-cdp record start --annotate\n" +
			"  chrome-cdp click --by name \"Save\"\n" +
			"  chrome-cdp record stop -o demo.gif\n\n" +
			"The frames are held by the background daemon, not by the command that\n" +
			"started the recording — so a run that crashes half way still has a recording\n" +
			"of the failure, and `record stop` afterwards still writes it.\n\n" +
			"Recording is per-tab. A batch that opens new tabs records the one it started\n" +
			"on; a multi-tab recording is not supported.\n\n" + recordPrivacyNote,
	}
	c.AddCommand(a.cmdRecordStart(), a.cmdRecordStop(), a.cmdRecordStatus(), a.cmdRecordCancel())
	return a.runnableGroup(c, "record", "record needs an action (start|stop|status|cancel)")
}

func (a *App) cmdRecordStart() *cobra.Command {
	var fps, scale float64
	var maxFrames int
	var maxDuration time.Duration
	var annotate bool
	c := &cobra.Command{
		Use:   "start",
		Short: "Start recording the target tab",
		Long: "Start recording the target tab.\n\n" +
			"Frames arrive only when the page actually changes, so a static page costs\n" +
			"almost nothing and --fps is a ceiling rather than a fixed interval. Capture\n" +
			"stops by itself after --max-duration, and the ring keeps the most recent\n" +
			"--max-frames; both are reported at `record stop` as `truncated` with a\n" +
			"reason, never applied silently.\n\n" + recordPrivacyNote,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			opts, rerr := recordStartOpts(fps, scale, maxFrames, maxDuration, annotate)
			if rerr != nil {
				a.emitErr("record", rerr.Code, rerr.Message, rerr.Details)
				return nil
			}
			a.runRecord("record", func(ctx context.Context, b chrome.Browser, id string) (map[string]any, error) {
				return b.RecordStart(ctx, id, opts)
			})
			return nil
		},
	}
	f := c.Flags()
	f.Float64Var(&fps, "fps", chrome.DefaultRecordFPS, fmt.Sprintf("frames per second to retain, %g-%g", recordFPSMin, recordFPSMax))
	f.Float64Var(&scale, "scale", chrome.DefaultRecordScale, fmt.Sprintf("capture scale relative to the viewport, %g-%g", recordScaleMin, recordScaleMax))
	f.IntVar(&maxFrames, "max-frames", 0, "ring-buffer size in frames (default from record_buffer, else 600)")
	f.DurationVar(&maxDuration, "max-duration", chrome.DefaultRecordMaxDuration, "stop capturing after this long")
	f.BoolVar(&annotate, "annotate", false, "mark action positions by default when exporting (overridable at `record stop`)")
	return c
}

// recordStartOpts validates the capture flags and reduces them to driver
// options, before anything connects.
func recordStartOpts(fps, scale float64, maxFrames int, maxDuration time.Duration, annotate bool) (chrome.RecordOpts, *result.Err) {
	if fps < recordFPSMin || fps > recordFPSMax || math.IsNaN(fps) {
		return chrome.RecordOpts{}, usageErr("--fps must be between %g and %g, got %g", recordFPSMin, recordFPSMax, fps)
	}
	if scale < recordScaleMin || scale > recordScaleMax || math.IsNaN(scale) {
		return chrome.RecordOpts{}, usageErr("--scale must be between %g and %g, got %g (a recording cannot be captured larger than the viewport)", recordScaleMin, recordScaleMax, scale)
	}
	if maxFrames != 0 && (maxFrames < recordFramesMin || maxFrames > recordFramesMax) {
		return chrome.RecordOpts{}, usageErr("--max-frames must be between %d and %d, got %d", recordFramesMin, recordFramesMax, maxFrames)
	}
	if maxDuration <= 0 {
		return chrome.RecordOpts{}, usageErr("--max-duration must be positive, got %s", maxDuration)
	}
	return chrome.RecordOpts{
		FPS: fps, Scale: scale, MaxFrames: maxFrames, MaxDuration: maxDuration, Annotate: annotate,
	}, nil
}

func (a *App) cmdRecordStop() *cobra.Command {
	var out, format, maxSize string
	var loop int
	var annotate bool
	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop recording and write the animation (default ./record-<timestamp>.gif)",
		Long: "Stop the recording and export its frames.\n\n" +
			"  chrome-cdp record stop -o demo.gif\n" +
			"  chrome-cdp record stop -o demo.gif --annotate --max-size 2MB\n" +
			"  chrome-cdp record stop --format frames -o ./out/\n\n" +
			"The format comes from the output extension unless --format says otherwise;\n" +
			"the two disagreeing is an error rather than a guess. gif and frames need\n" +
			"nothing installed; mp4 and webm need ffmpeg on PATH, and its absence is\n" +
			"reported BEFORE the recording is touched, so the frames survive to be\n" +
			"exported as a GIF instead.\n\n" +
			"--max-size re-encodes at a smaller scale (and then fewer frames) until the\n" +
			"file fits. It is best-effort: the envelope reports the scale and frame count\n" +
			"actually used, and says so when the ceiling could not be met.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts, rerr := recordExportOpts(cmd, out, format, maxSize, loop, annotate)
			if rerr != nil {
				a.emitErr("record", rerr.Code, rerr.Message, rerr.Details)
				return nil
			}
			a.runRecordStop(opts)
			return nil
		},
	}
	f := c.Flags()
	f.StringVarP(&out, "output", "o", "", "output path, or - for stdout (default ./record-<timestamp>.<ext>)")
	f.StringVar(&format, "format", "", "gif|mp4|webm|frames (default: from the output extension, else gif)")
	f.StringVar(&maxSize, "max-size", "", "best-effort size ceiling, e.g. 2MB or 1500000")
	f.IntVar(&loop, "loop", 0, "GIF loop count (0 = forever)")
	f.BoolVar(&annotate, "annotate", false, "draw the action markers (overrides what `record start` was given)")
	return c
}

func (a *App) cmdRecordStatus() *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Report whether the target tab is being recorded, and how much it holds",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			a.runRecord("record", func(ctx context.Context, b chrome.Browser, id string) (map[string]any, error) {
				return b.RecordStatus(ctx, id)
			})
			return nil
		},
	}
}

func (a *App) cmdRecordCancel() *cobra.Command {
	return &cobra.Command{
		Use: "cancel", Short: "Discard the recording without writing anything",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			a.runRecord("record", func(ctx context.Context, b chrome.Browser, id string) (map[string]any, error) {
				return b.RecordCancel(ctx, id)
			})
			return nil
		},
	}
}

// runRecord resolves the target and runs a record verb, classifying its errors
// the way the record contract requires.
func (a *App) runRecord(command string, fn func(context.Context, chrome.Browser, string) (map[string]any, error)) {
	ctx, cancel := a.ctx()
	defer cancel()
	tgt, b, rerr := a.resolveTarget(ctx)
	if rerr != nil {
		a.emitErr(command, rerr.Code, rerr.Message, rerr.Details)
		return
	}
	res, err := fn(ctx, b, tgt.ID)
	if err != nil {
		code, msg, details := classifyRecordErr(err)
		a.emitErr(command, code, msg, details)
		return
	}
	a.emitOK(command, tgt, res)
}

// classifyRecordErr maps a recording failure onto the error contract.
//
// The two lifecycle mistakes are `usage`, not a target error: "you are already
// recording" and "there is nothing to stop" are both statements about the
// caller's sequence of commands, and an agent has to be able to tell them from
// "the page did not cooperate" — one means fix your script, the other means
// retry.
func classifyRecordErr(err error) (string, string, map[string]any) {
	switch {
	case chrome.IsAlreadyRecording(err):
		return result.CodeUsage, err.Error() + " — stop it (`record stop`) or discard it (`record cancel`) first", map[string]any{"recording": true}
	case chrome.IsNotRecording(err):
		return result.CodeUsage, err.Error() + " — start one with `record start`", map[string]any{"recording": false}
	default:
		return classifyActionErr(err), err.Error(), nil
	}
}

// exportOpts is one validated `record stop`.
type exportOpts struct {
	out      string
	format   encode.Format
	loop     int
	annotate bool
	// annotateSet distinguishes "--annotate was not passed" from "--annotate
	// false", so `record start --annotate` still annotates a plain `record stop`.
	annotateSet bool
	maxBytes    int
}

// recordExportOpts validates every export flag, including whether this machine
// can produce the format at all — before the recording is drained.
func recordExportOpts(cmd *cobra.Command, out, format, maxSize string, loop int, annotate bool) (exportOpts, *result.Err) {
	o := exportOpts{out: out, loop: loop, annotate: annotate}
	if cmd != nil {
		o.annotateSet = cmd.Flags().Changed("annotate")
	}
	f, err := resolveExportFormat(format, out)
	if err != nil {
		return exportOpts{}, usageErr("%s", err.Error())
	}
	o.format = f
	if err := encode.Available(f); err != nil {
		// The frames are still in the daemon: this ran before RecordStop.
		return exportOpts{}, usageErr("%s", err.Error())
	}
	if loop < 0 {
		return exportOpts{}, usageErr("--loop must not be negative (0 loops forever), got %d", loop)
	}
	if maxSize != "" {
		n, err := parseByteSize(maxSize)
		if err != nil {
			return exportOpts{}, usageErr("%s", err.Error())
		}
		o.maxBytes = n
	}
	if f == encode.FormatFrames && out == "-" {
		return exportOpts{}, usageErr("--format frames writes a directory of PNGs, which cannot go to stdout — give -o a directory")
	}
	if err := checkExportTarget(o); err != nil {
		// Also before RecordStop, and for the same reason the encoder probe is:
		// a typo'd directory must cost the user an error, not their recording.
		return exportOpts{}, &result.Err{Code: result.CodeGeneric, Message: err.Error()}
	}
	return o, nil
}

// checkExportTarget reports whether the artifact could actually be written,
// BEFORE the recording is drained.
//
// This is the same guarantee VS-10 gives the missing-encoder case, extended to
// the failure that is far likelier to happen: `record stop -o /nonexistent/demo.gif`
// used to drain the recording, discover the directory at write time, and leave
// the user with exit 1 and nothing to retry with. Writability is probed by
// actually creating a file, because the permission bits alone do not answer the
// question on every platform or filesystem.
func checkExportTarget(o exportOpts) error {
	if o.out == "-" {
		return nil
	}
	dir := "."
	switch {
	case o.format == encode.FormatFrames:
		// The directory itself is created at write time, so the nearest existing
		// ancestor is what has to be writable.
		if o.out != "" {
			dir = o.out
			for {
				if fi, err := os.Stat(dir); err == nil {
					if !fi.IsDir() {
						return fmt.Errorf("cannot write the frames to %q: it is a file, not a directory", dir)
					}
					break
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
			}
		}
	case o.out != "":
		if fi, err := os.Stat(o.out); err == nil && fi.IsDir() {
			return fmt.Errorf("cannot write the recording to %q: it is a directory", o.out)
		}
		dir = filepath.Dir(o.out)
	}
	if err := probeWritable(dir, ".chrome-cdp-record-*"); err != nil {
		return fmt.Errorf("cannot write the recording to %q: %w (the recording is untouched — retry with a writable path)", dir, err)
	}
	return nil
}

// resolveExportFormat decides the export format from --format and the output
// path, refusing to guess when the two disagree.
//
// Silently honouring --format webm for a file named demo.gif would leave the
// user with a WebM whose extension says GIF, which nothing they upload it to
// will play. So the conflict is an error (VS-9), and so is an extension this
// package cannot name.
func resolveExportFormat(formatFlag, out string) (encode.Format, error) {
	byExt, hasExt := encode.FormatFromPath(out)
	if strings.TrimSpace(formatFlag) == "" {
		switch {
		case hasExt:
			return byExt, nil
		case out == "" || out == "-" || filepath.Ext(out) == "":
			return encode.FormatGIF, nil
		default:
			return "", fmt.Errorf("cannot infer the format from %q: pass --format, or use an extension (%s)", out, encode.Extensions())
		}
	}
	f, err := encode.ParseFormat(formatFlag)
	if err != nil {
		return "", fmt.Errorf("--format: %w", err)
	}
	if f == encode.FormatFrames && hasExt {
		return "", fmt.Errorf("--format frames writes a directory of numbered PNGs, but -o %q names a %s file", out, byExt)
	}
	if hasExt && byExt != f {
		return "", fmt.Errorf("--format %s conflicts with the output extension of %q (%s): pass one or the other", f, out, byExt)
	}
	return f, nil
}

// parseByteSize parses a size ceiling: plain bytes, or a K/M/G suffix.
func parseByteSize(s string) (int, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	if t == "" {
		return 0, fmt.Errorf("--max-size is empty")
	}
	mult := 1
	for suffix, m := range map[string]int{"kb": 1 << 10, "k": 1 << 10, "mb": 1 << 20, "m": 1 << 20, "gb": 1 << 30, "g": 1 << 30} {
		if strings.HasSuffix(t, suffix) {
			// Longest suffix wins, so "mb" is not read as "b" plus junk.
			if m > mult || (m == mult && len(suffix) > 1) {
				mult = m
				t = strings.TrimSpace(strings.TrimSuffix(t, suffix))
			}
		}
	}
	if mult == 1 {
		t = strings.TrimSpace(strings.TrimSuffix(t, "b"))
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || n <= 0 || math.IsInf(n, 0) {
		return 0, fmt.Errorf("--max-size %q is not a size: write bytes (1500000) or a suffix (2MB)", s)
	}
	v := int(n * float64(mult))
	if v <= 0 {
		return 0, fmt.Errorf("--max-size %q is too small to hold a frame", s)
	}
	return v, nil
}

// runRecordStop stops the recording, exports it, and writes the artifact.
func (a *App) runRecordStop(opts exportOpts) {
	ctx, cancel := a.ctx()
	defer cancel()
	tgt, b, rerr := a.resolveTarget(ctx)
	if rerr != nil {
		// A recording whose TAB has gone is exactly the case US-7 is about: the
		// frames are held by the daemon, not by the tab, so they are still
		// exportable when the tab that produced them is not.
		if id, ok := a.strandedRecordingID(rerr); ok {
			a.stopStrandedRecording(ctx, id, opts, rerr)
			return
		}
		a.emitErr("record", rerr.Code, rerr.Message, rerr.Details)
		return
	}
	frames, meta, err := b.RecordStop(ctx, tgt.ID)
	if err != nil {
		code, msg, details := classifyRecordErr(err)
		a.emitErr("record", code, msg, details)
		return
	}
	a.exportRecording(ctx, b, "record", tgt, frames, meta, opts)
}

// strandedRecordingID reports the concrete target id to try anyway when target
// resolution failed because the tab is gone.
//
// Only a literal id qualifies: an ephemeral spec (@2, url:…, title:…) names a
// tab by something that no longer exists, and re-interpreting it against a
// changed tab list is how a command ends up acting on the wrong one.
func (a *App) strandedRecordingID(rerr *result.Err) (string, bool) {
	if rerr.Code != result.CodeTargetNotFound {
		return "", false
	}
	spec := a.targetFlag
	if spec == "" {
		spec = a.sticky()
	}
	if spec == "" || strings.ContainsAny(spec, ":@") {
		return "", false
	}
	return spec, true
}

// stopStrandedRecording exports a recording whose tab has closed.
//
// The policy check runs with NO origin, exactly as `raw --browser` does: the
// tab whose origin a rule would name is gone, and a policy that cannot identify
// what it is deciding about refuses. That keeps the escape hatch from also
// being a way around the boundary.
func (a *App) stopStrandedRecording(ctx context.Context, id string, opts exportOpts, rerr *result.Err) {
	if perr := a.checkPolicy(a.policyVerb(), ""); perr != nil {
		a.emitErr("record", perr.Code, perr.Message, perr.Details)
		return
	}
	b, berr := a.getBrowser(ctx)
	if berr != nil {
		a.emitErr("record", berr.Code, berr.Message, nil)
		return
	}
	frames, meta, err := b.RecordStop(ctx, id)
	if err != nil {
		// Nothing was being recorded on that id either: report the original
		// "tab is gone", which is the more useful of the two answers.
		if chrome.IsNotRecording(err) {
			a.emitErr("record", rerr.Code, rerr.Message, rerr.Details)
			return
		}
		code, msg, details := classifyRecordErr(err)
		a.emitErr("record", code, msg, details)
		return
	}
	if meta == nil {
		meta = map[string]any{}
	}
	// Say what happened: the recording is complete, the tab is not.
	meta["tab_closed"] = true
	a.exportRecording(ctx, b, "record", &result.TargetInfo{ID: id}, frames, meta, opts)
}

// exportRecording encodes the frames, writes them, and emits the envelope.
//
// Everything after this point is operating on a recording the daemon has already
// let go of, so every failure below hands the frames back (see restoreRecording)
// rather than ending the recording over a full disk.
func (a *App) exportRecording(ctx context.Context, b chrome.Browser, command string, tgt *result.TargetInfo, frames []chrome.Frame, meta map[string]any, opts exportOpts) {
	if len(frames) == 0 {
		a.emitErr(command, result.CodeGeneric,
			"the recording captured no frames — a screencast only produces them while the tab is rendering, "+
				"so a fully backgrounded tab records nothing; bring it to the front (`activate`) and record again",
			map[string]any{"frames": 0})
		return
	}
	annotate := opts.annotate
	if !opts.annotateSet {
		// `record start --annotate` decides when `record stop` did not.
		annotate, _ = meta["annotate"].(bool)
	}
	res, err := encode.Encode(ctx, toEncodeFrames(frames), encode.Options{
		Format:   opts.format,
		FPS:      metaFloat(meta, "fps", chrome.DefaultRecordFPS),
		Loop:     opts.loop,
		Annotate: annotate,
		MaxBytes: opts.maxBytes,
	})
	if err != nil {
		code := result.CodeGeneric
		if encode.IsNoEncoder(err) {
			code = result.CodeUsage
		}
		a.emitErr(command, code, a.restoreRecording(ctx, b, tgt, frames, meta, err), nil)
		return
	}

	payload := recordResultPayload(meta, res, opts)
	path, toStdout, werr := a.writeExport(res, opts)
	if werr != nil {
		a.emitErr(command, result.CodeGeneric, a.restoreRecording(ctx, b, tgt, frames, meta, werr), nil)
		return
	}
	if toStdout {
		// `-o -` means the artifact IS the stream, so nothing else may go on it —
		// `chrome-cdp --json record stop -o - > demo.gif` must be a GIF and not a
		// GIF with a JSON line stuck on the end. capture.go's emitArtifact answers
		// this the same way for screenshot/pdf, and the two have to agree.
		return
	}
	if path != "" {
		payload["path"] = path
		// Human mode says what was written and how much of it, so it is never
		// ambiguous that a file now exists and what is in it.
		if !a.jsonOut && !a.quiet {
			fmt.Fprintf(a.err, "wrote %d frames (%s) to %s\n", res.Frames, humanBytes(res.Bytes), path)
		}
	}
	a.emitOK(command, tgt, payload)
}

// restoreRecording hands the frames back to the daemon after a failed export and
// returns the message to report.
//
// The recording was drained by RecordStop and the export then failed, so without
// this the user's answer to a full disk is a lost recording and a `record stop`
// that says there is nothing to stop. A restore that itself fails is worth
// saying out loud — it is the difference between "try again" and "it is gone".
func (a *App) restoreRecording(ctx context.Context, b chrome.Browser, tgt *result.TargetInfo, frames []chrome.Frame, meta map[string]any, cause error) string {
	if b == nil || tgt == nil || tgt.ID == "" {
		return cause.Error()
	}
	if err := b.RecordRestore(ctx, tgt.ID, frames, meta); err != nil {
		return fmt.Sprintf("%s (and the %d captured frames could not be handed back: %s)", cause.Error(), len(frames), err.Error())
	}
	return fmt.Sprintf("%s — the %d captured frames are still held, so `record stop` can be retried", cause.Error(), len(frames))
}

// recordResultPayload merges what the driver knows (how much was captured, what
// the bounds discarded) with what the encoder knows (what was produced).
func recordResultPayload(meta map[string]any, res encode.Result, opts exportOpts) map[string]any {
	out := map[string]any{
		"action":      "stop",
		"format":      string(res.Format),
		"frames":      res.Frames,
		"fps":         res.FPS,
		"duration_ms": res.DurationMs,
		"width":       res.Width,
		"height":      res.Height,
		"bytes":       res.Bytes,
		"annotated":   res.Annotated,
		// Frames the encoder could not read. Always present, because "how much of
		// the capture reached the file" is not a question a caller should have to
		// infer from a missing key.
		"decode_failures": res.DecodeFailures,
	}
	// The capture's own accounting travels with the export: a caller has to be
	// able to see that the ring evicted frames even though the file it got looks
	// perfectly fine (US-6).
	for _, k := range []string{"dropped_frames", "truncated", "reason", "elapsed_ms", "scale", "tab_closed"} {
		if v, ok := meta[k]; ok {
			out[k] = v
		}
	}
	if v, ok := meta["frames"]; ok {
		out["captured_frames"] = v
	}
	if res.Reduced {
		out["reduced"] = true
		out["export_scale"] = res.Scale
	}
	// Whether the ceiling was met is a fact about the CEILING, not about the
	// reduction: the ladder can refuse at step 0 for a small canvas with few
	// frames, and gating the report on `reduced` is what let a 16x miss be
	// reported as a plain byte count with nothing said about it.
	if opts.maxBytes > 0 {
		out["within_max_size"] = res.WithinMaxSize
		if res.ReductionTimedOut {
			// The ladder was cut short by --timeout, not by its own bounds: what
			// came back is the best COMPLETE attempt, and a longer timeout might
			// have done better. Saying so is the difference between "this is as
			// small as it gets" and "give it more time".
			out["max_size_timed_out"] = true
		}
	}
	return out
}

// writeExport writes the artifact, returning the path it landed at and whether
// it went to stdout instead (in which case the caller must emit no envelope).
func (a *App) writeExport(res encode.Result, opts exportOpts) (string, bool, error) {
	if opts.format == encode.FormatFrames {
		dir := opts.out
		if dir == "" {
			dir = uniquePath(fmt.Sprintf("./record-%s-frames", time.Now().Format("20060102-150405")))
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", false, err
		}
		if err := clearFrameDir(dir); err != nil {
			return "", false, err
		}
		for _, f := range res.Files {
			if err := os.WriteFile(filepath.Join(dir, f.Name), f.Data, 0o644); err != nil {
				return "", false, err
			}
		}
		return dir, false, nil
	}
	if opts.out == "-" {
		if _, err := a.out.Write(res.Data); err != nil {
			return "", false, err
		}
		if !a.quiet {
			fmt.Fprintf(a.err, "wrote %d frames (%s) to stdout\n", res.Frames, humanBytes(res.Bytes))
		}
		return "", true, nil
	}
	path := opts.out
	if path == "" {
		path = uniquePath(fmt.Sprintf("./record-%s.%s", time.Now().Format("20060102-150405"), res.Format))
	}
	if err := os.WriteFile(path, res.Data, 0o644); err != nil {
		return "", false, err
	}
	return path, false, nil
}

// clearFrameDir removes the numbered PNGs a previous export left in dir.
//
// A shorter recording written over a longer one would otherwise leave the tail
// of the old one in place, and the directory — whose whole contract is "a
// numbered PNG per frame" — would hold more frames than the envelope reports,
// with the extra ones coming from a different run. Only the exact names this
// command writes are removed, and only if they are files: anything else the
// user keeps in that directory is theirs.
func clearFrameDir(dir string) error {
	old, err := filepath.Glob(filepath.Join(dir, "frame-[0-9]*.png"))
	if err != nil {
		return err
	}
	for _, p := range old {
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

// toEncodeFrames converts the driver's frames into the encoder's. The two types
// are deliberately separate: the driver's is a wire type crossing the daemon
// RPC, and the encoder must stay free of any dependency on the browser layer.
func toEncodeFrames(frames []chrome.Frame) []encode.Frame {
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

// metaFloat reads a number out of the driver's metadata, which arrives as
// float64 over the daemon RPC and as an int or float64 in-process.
func metaFloat(meta map[string]any, key string, def float64) float64 {
	switch v := meta[key].(type) {
	case float64:
		if v > 0 {
			return v
		}
	case int:
		if v > 0 {
			return float64(v)
		}
	}
	return def
}

// sessionRecordFlags are `session --record`'s own flags.
type sessionRecordFlags struct {
	path     string
	fps      float64
	annotate bool
}

func (f *sessionRecordFlags) register(c *cobra.Command) {
	fl := c.Flags()
	fl.StringVar(&f.path, "record", "", "record the whole batch and write it here (RFC-0011; .gif unless the extension says otherwise)")
	fl.Float64Var(&f.fps, "record-fps", chrome.DefaultRecordFPS, "with --record: frames per second to retain")
	fl.BoolVar(&f.annotate, "record-annotate", false, "with --record: mark action positions on the exported frames")
}

// sessionRecorder brackets a `session` batch in a recording.
//
// It exists because bracketing by hand is exactly the tedium US-2 is about, and
// because the interesting case — a batch that FAILS half way — is the one a
// user would forget to bracket. The nil recorder is the "no --record" case and
// every method on it is a no-op, so the session loop reads the same either way.
type sessionRecorder struct {
	a       *App
	opts    chrome.RecordOpts
	export  exportOpts
	started bool
	// gaveUp stops a batch of a hundred lines from retrying (and reporting) a
	// start that is never going to work.
	gaveUp   bool
	targetID string
	tgt      *result.TargetInfo
}

// newSessionRecorder validates the --record flags. It returns a nil recorder
// when --record was not given, which is the common case and costs nothing.
func (a *App) newSessionRecorder(cmd *cobra.Command, f sessionRecordFlags) (*sessionRecorder, *result.Err) {
	if f.path == "" {
		return nil, nil
	}
	opts, rerr := recordStartOpts(f.fps, chrome.DefaultRecordScale, 0, chrome.DefaultRecordMaxDuration, f.annotate)
	if rerr != nil {
		return nil, rerr
	}
	// The export is validated NOW — before a single line of the batch runs — so
	// `session --record run.mp4` on a machine with no ffmpeg fails immediately
	// rather than after the batch has already driven the browser.
	export, rerr := recordExportOpts(nil, f.path, "", "", 0, f.annotate)
	if rerr != nil {
		return nil, rerr
	}
	export.annotate, export.annotateSet = f.annotate, true
	return &sessionRecorder{a: a, opts: opts, export: export}, nil
}

// ensureStarted starts the recording once a target is resolvable.
func (r *sessionRecorder) ensureStarted() {
	if r == nil || r.started || r.gaveUp {
		return
	}
	ctx, cancel := r.a.ctx()
	defer cancel()
	tgt, b, rerr := r.a.resolveTarget(ctx)
	if rerr != nil {
		// No current target yet: the batch's first line is probably `use`, so
		// try again on the next one. Anything else is a real failure.
		if rerr.Code == result.CodeNoTarget {
			return
		}
		r.fail(rerr.Code, rerr.Message, rerr.Details)
		return
	}
	if _, err := b.RecordStart(ctx, tgt.ID, r.opts); err != nil {
		code, msg, details := classifyRecordErr(err)
		r.fail(code, msg, details)
		return
	}
	r.started, r.targetID, r.tgt = true, tgt.ID, tgt
}

// finish stops the recording and writes the file, emitting one extra result
// line so the batch's own output says where the recording went (VS-15).
func (r *sessionRecorder) finish() {
	if r == nil || r.gaveUp {
		return
	}
	if !r.started {
		r.fail(result.CodeNoTarget,
			"--record captured nothing: no line in this batch resolved a tab, so there was never anything to record", nil)
		return
	}
	ctx, cancel := r.a.ctx()
	defer cancel()
	b, berr := r.a.getBrowser(ctx)
	if berr != nil {
		r.fail(berr.Code, berr.Message, nil)
		return
	}
	frames, meta, err := b.RecordStop(ctx, r.targetID)
	if err != nil {
		code, msg, details := classifyRecordErr(err)
		r.fail(code, msg, details)
		return
	}
	r.a.exportRecording(ctx, b, "record", r.tgt, frames, meta, r.export)
}

// fail reports a recording problem as its own envelope and stops trying.
//
// It is deliberately not fatal to the batch: the commands are what the user
// asked for, and losing the recording of them is not a reason to stop running
// them. The failure is still an envelope, so it is visible in the NDJSON rather
// than only on a terminal nobody is watching.
func (r *sessionRecorder) fail(code, msg string, details map[string]any) {
	r.gaveUp = true
	r.a.emitErr("record", code, msg, details)
}

// humanBytes renders a size the way the human line reports it.
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
