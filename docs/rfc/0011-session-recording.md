# RFC-0011: Session recording — `record` and GIF export

- **Status:** Draft
- **Priority:** P2
- **Area:** capture
- **Depends on:** RFC-0008 (shares capture options), RFC-0002 (daemon-side buffering pattern)

## Summary

Add a `record` command that captures frames while other commands drive the browser, and exports them as an animated GIF (or an MP4/WebM, or a numbered frame directory) with optional action annotations.

## Motivation

This RFC is honest about being the least functionally important in the set and one of the most valuable for adoption.

**Why it is low priority functionally:** nothing is unautomatable without it.
Every workflow that works after this lands also worked before.

**Why it is worth building anyway:**

- **A README that shows the tool working is worth more than one that lists its flags.**
  The single most effective asset an open-source CLI can have is a short animation of it doing something real.
  For a browser automation tool, that animation is *the browser being driven* — which the tool can produce about itself.
  A project that cannot demonstrate itself visually is at a permanent disadvantage against ones that can.
- **Bug reports become reproducible.**
  "The click lands in the wrong place on this app" is nearly unactionable as text and immediately actionable as six seconds of video.
  For a tool driving pages maintainers cannot access — internal, authenticated apps — a recording is often the *only* way a report can be understood.
- **Automation review.**
  Before letting a recipe (RFC-0009) run unattended against a real system, watching one recorded run is a far better check than reading envelopes.
- **Documentation of internal workflows.**
  Users automating an internal app frequently need to show a colleague what the automation does.
  Today that means a manual screen recording.

The cost is bounded: `Page.startScreencast` already streams frames, and encoding is a well-served problem in Go.

## User stories

**US-1 — Record a run.**
As a user, I want to record while my automation runs so that I have a visual artifact of what happened.
*Acceptance:* `chrome-cdp record start`, then any commands, then `record stop -o run.gif` produces an animation of those commands.

**US-2 — Record a batch in one call.**
As a script author, I want to wrap a `session` in a recording so that I do not have to bracket it manually.
*Acceptance:* `chrome-cdp session --record run.gif < steps.ndjson` records the whole batch.

**US-3 — See what the automation did.**
As a maintainer reading a bug report, I want actions annotated on the frames so that I can see where a click landed.
*Acceptance:* with `--annotate`, click positions are marked and each frame carries the command that produced it.

**US-4 — Keep the file small.**
As a user attaching to an issue, I want to control size so that the artifact is small enough to upload.
*Acceptance:* `--fps`, `--scale`, `--max-duration`, and `--max-size` bound the output, and the envelope reports the final size.

**US-5 — Produce a README asset.**
As a maintainer, I want a clean recording without the tool's own branding so that I can use it in documentation.
*Acceptance:* annotations and any watermark are opt-in, not defaults.

**US-6 — Not silently lose a long recording.**
As a user recording a long run, I want to know when limits truncated the output so that I do not present a partial recording as complete.
*Acceptance:* the envelope reports `truncated`, `dropped_frames`, and the reason.

**US-7 — Recover from a crash.**
As a user whose run died mid-recording, I want the frames captured so far so that the failure itself is what I have a recording of.
*Acceptance:* `record stop` after a failed run still exports; frames are not held solely in a process that died with the run.

## Proposed CLI surface

```
chrome-cdp record start [--fps <n>] [--scale <f>] [--annotate]
                        [--max-duration <dur>] [--max-frames <n>]
chrome-cdp record stop  [-o <path>] [--format gif|mp4|webm|frames]
                        [--max-size <bytes>] [--loop <n>]
chrome-cdp record status
chrome-cdp record cancel

chrome-cdp session --record <path> [--record-fps <n>] [--record-annotate]
```

| Flag | Default | Purpose |
|------|---------|---------|
| `--fps <n>` | `4` | frames per second; GIFs rarely need more |
| `--scale <f>` | `0.5` | capture scale; halves dimensions by default |
| `--annotate` | off | mark click/type positions and label frames with the command |
| `--max-duration <dur>` | `2m` | stop capturing after this |
| `--max-frames <n>` | `600` | ring-buffer cap |
| `--format` | from extension | `gif` \| `mp4` \| `webm` \| `frames` |
| `--max-size <bytes>` | — | reduce fps/scale on export to fit |
| `--loop <n>` | `0` (infinite) | GIF loop count |

Examples:

```sh
chrome-cdp record start --annotate
chrome-cdp click --by name "Save"
chrome-cdp record stop -o demo.gif

chrome-cdp session --record run.gif < steps.ndjson
```

## Result envelope

```json
{ "ok": true, "command": "record",
  "result": { "action": "stop", "path": "./demo.gif", "format": "gif",
              "frames": 47, "fps": 4, "duration_ms": 11750,
              "width": 700, "height": 450, "bytes": 1841204,
              "dropped_frames": 0, "truncated": false, "annotated": true },
  "elapsed_ms": 890 }
```

`record status` reports `{"recording": true, "frames": 23, "elapsed_ms": 5800, "target": {...}}`.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| `record start` while already recording; `stop`/`status` with none active; unknown format; out-of-range fps/scale; format conflicting with the output extension | `usage` | 2 |
| Encoder unavailable for the requested format | `usage` | 2, with a message naming the requirement |
| Output path not writable; encoding failed | `generic` | 1 |
| `Page.startScreencast` rejected | `cdp_error` | 5 |
| Recording active but the tab was closed | `target_not_found` | 4, with whatever frames exist still exportable |

## Design notes

- **Frames live in the daemon**, for the same reason console and network buffers do (RFC-0002): recording spans many CLI invocations, so no per-command process can hold them.
  This also satisfies US-7 — a crashed automation does not take the frames with it, because the process that died was never holding them.
- **Bounded by construction.**
  A ring buffer of `--max-frames`, JPEG frames at `--scale`, and a total memory cap.
  Screencast frames are large; without hard caps a long run would balloon the daemon.
  Eviction increments `dropped_frames` and sets `truncated`, so US-6 is satisfied by reporting rather than by silence.
- **`Page.startScreencast`, not repeated screenshots.**
  Chrome pushes frames only when the page actually changes, which is both cheaper and produces better-looking output than fixed-interval polling of a static page.
- **Annotation is a compositing step at export**, not at capture.
  The daemon records `(timestamp, command, coordinates)` alongside frames, and the exporter draws markers.
  Keeping capture free of drawing means `--annotate` can be decided at `stop` time and a single capture can be exported both ways.
- **Where annotation data comes from:** the pointer verbs (RFC-0005) and `click` already resolve a centre point and report it in the envelope.
  The recorder subscribes to that, so annotation requires no new resolution logic — one more reason this lands after RFC-0005.
- **Encoding:** GIF via the standard library's `image/gif` with a quantised palette — no external dependency, which matters for a Go CLI distributed as a single binary.
  MP4/WebM require `ffmpeg` on `PATH`; if absent, that format is a `usage` error naming the requirement rather than a silent fallback.
  `frames` (a numbered PNG directory) always works and gives users an escape hatch to encode however they like.
- **`--max-size`** re-encodes at reduced fps and scale until the target is met, reporting the final values.
  A best-effort bound, documented as such.
- **Privacy note worth stating in the docs:** this records the user's real, logged-in browser.
  A recording made to attach to a public bug report may contain the user's own data, and the CLI cannot know what is sensitive.
  The docs should say so plainly at the point of use, and `record stop` should print the frame count and path in human mode so it is never ambiguous that a file was written.
- **Interface:** ```go RecordStart(ctx context.Context, targetID string, opts RecordOpts) (map[string]any, error) RecordStop(ctx context.Context, targetID string) ([]Frame, map[string]any, error) RecordStatus(ctx context.Context, targetID string) (map[string]any, error) RecordCancel(ctx context.Context, targetID string) (map[string]any, error) ``` Encoding lives in a separate `internal/encode` package taking `[]Frame` and options — pure, and therefore properly unit-testable without a browser, which is the main reason to split it out.

## Verification scenarios

**VS-1 — Start, act, stop produces a valid GIF** Given a fixture with an animated element When `record start`, two commands, and `record stop -o out.gif` run Then the file decodes as a GIF with more than one frame, and the envelope's `frames` matches the decoded count.

**VS-2 — Status reflects state** Given an active recording Then `record status` reports `recording: true` with a nonzero frame count; after `stop`, it reports `recording: false`.

**VS-3 — Double start is rejected** Given an active recording When `record start` runs again Then exit 2 with `usage`, and the existing recording is unaffected — assert the frame count keeps rising.

**VS-4 — Stop with none active** When `record stop` runs with no recording Then exit 2 with `usage`.

**VS-5 — Cancel discards** Given an active recording When `record cancel` runs Then no file is written, `status` reports not recording, and a subsequent `stop` is a `usage` error.

**VS-6 — Ring buffer truncates and says so** Given `--max-frames 10` and a page producing 50 frames When stopped Then `frames == 10`, `dropped_frames == 40`, `truncated` is true, and the retained frames are the most recent.

**VS-7 — Max duration** Given `--max-duration 2s` and a continuously animating page Then capture stops at ~2s and `truncated` is true.

**VS-8 — Scale** Given a 1000×800 viewport and `--scale 0.5` Then the reported and decoded dimensions are ~500×400.

**VS-9 — Format from extension and explicit conflict** Table: `-o x.gif` → gif; `-o x.webm --format webm` → webm; `-o x.gif --format webm` → `usage` exit 2.

**VS-10 — Missing encoder is a clear error** Given `ffmpeg` absent from `PATH` When `record stop --format mp4` runs Then exit 2 with a message naming `ffmpeg`, and — importantly — the frames are still available for a subsequent `--format gif` export rather than being discarded.

**VS-11 — `frames` format** When `record stop --format frames -o ./out/` runs Then a numbered PNG per frame is written and the count matches the envelope.

**VS-12 — Annotation marks a click** Given `record start --annotate` and a click at a known coordinate When exported Then the frame at that timestamp differs from the unannotated export at that coordinate — assert by pixel comparison at the marker location, not by visual inspection.

**VS-13 — Annotation is opt-in** Without `--annotate`, exported frames are pixel-identical to the raw capture.
This is what makes US-5 verifiable.

**VS-14 — Frames survive a failed run** Given a recording active and a command that fails When `record stop` runs afterwards Then frames captured before the failure are exported.

**VS-15 — `session --record`** Given a `session` batch run with `--record` Then the file is written on completion and the summary reports it, including when a mid-batch step fails.

**VS-16 — Encoder unit correctness** Given a synthetic `[]Frame` of known solid colours When encoded to GIF Then the decoded frames' pixel values and count match, and the loop count is honoured.

## Test plan

**Unit — encoder (`internal/encode`, `t.Parallel()`, no browser)** VS-16 plus palette quantisation, frame-delay computation from fps, `--max-size` reduction loop convergence (assert it terminates and reports the values used), and the annotation compositing (VS-12/VS-13 can be tested here on synthetic frames far more reliably than through a browser).
This is the largest and most valuable part of the suite, and it runs under `go test -short`.

**Unit — state machine (`internal/cli` + daemon, `chrometest.StubBrowser`)** VS-2 through VS-5, VS-9, VS-10, VS-14: start/stop/status/cancel transitions, double-start, stop-without-start, format resolution, and that a failed export preserves frames.

**Unit — ring buffer (`internal/eventbuf` or alongside it, `t.Parallel()`)** VS-6 with synthetic frames: eviction order, `dropped_frames` accounting, memory cap.

**Live Chrome (`internal/chrome`, `testing.Short()`-guarded, not parallel)** VS-1, VS-7, VS-8, VS-11, VS-15 against an animating `data:` fixture.
Keep these few — frame timing is inherently variable, so assert on ranges and structural properties (decodes, frame count > 1, dimensions within tolerance), never on exact counts or byte equality.

**Artifact hygiene** Every test writes into `t.TempDir()`.
`*.gif`, `*.mp4`, and `*.webm` are added to `.gitignore` alongside the existing `*.png`/`*.pdf` entries in the same PR — a recording test that leaves files in the repo would be an easy and annoying regression.

## Out of scope

- Audio.
- Recording more than one tab into one output.
- Live streaming to a viewer.
- Editing, trimming, or splicing recordings after capture.
- Replaying a recording as an automation (a recording is an artifact, not a script — recipes in RFC-0009 are the scriptable form).

## Open questions

1. Should recording be per-target or per-daemon?
   Per-target is more predictable but complicates "record everything my batch did" when a batch opens tabs.
   **Recommendation:** per-target, with the limitation documented; a multi-tab recording is a follow-up.
2. Should the default output format be GIF or WebM?
   GIF is universally embeddable in READMEs and issue trackers, which is the primary use case; WebM is far smaller and better quality but needs `ffmpeg`.
   **Recommendation:** GIF default, because the dependency-free path should be the one that works out of the box.
3. Should `--annotate` include a caption bar with the command text?
   Useful for demos, but it is the kind of detail that invites endless tuning.
   **Recommendation:** ship position markers only; add captions only if the demo asset actually needs them.
4. Should `record` be excluded from MCP mode (RFC-0004)?
   An agent silently recording the user's browser is a surprising capability.
   **Recommendation:** exclude from the `default` tool set; available under `--tools full`.
