# RFC-0014: Coordinate-space interaction — `--at` addressing, `tripleclick`, drop-zone upload, and real window sizing

- **Status:** Accepted (in part) — `--at`, `tripleclick`, and `window` implemented in [#22](https://github.com/sanketsudake/chrome-cdp-cli/pull/22); `upload --drop` still outstanding
- **Priority:** P0 (coordinate addressing, `tripleclick`) · P1 (drop-zone upload, `window`)
- **Area:** input
- **Depends on:** RFC-0005 (extends its `Pointer` primitive), RFC-0006 (extends `upload`)
- **Related:** RFC-0008 (defines the screenshot pixel space these coordinates must match), RFC-0015 (`find` returns centre points that feed `--at`), RFC-0012 (policy governs drop-zone upload paths)

## Summary

Give every pointer verb a coordinate form: `--at <x,y>` acts at a viewport point with no element resolution, and `drag` gains `--to-at <x,y>` for its endpoint.
Expose `tripleclick` as a first-class verb (the primitive already exists inside `fill`).
Extend `upload` with a `--drop` mode that delivers files to drag-and-drop targets that have no `<input type=file>`.
Add a `window` verb that resizes the real Chrome window via `Browser.setWindowBounds`, which `emulate viewport` does not do.

Together these give the CLI a complete pixel space: act at a point, drop at a point, and control the window the points live in.

## Motivation

Every verb today addresses an *element* — CSS, id, accessible name, ref, cell, label.
That is the right default, but an entire application category has no elements to address:

- **Canvas and WebGL apps.**
  Drawing tools, diagram editors, maps, charts, games, and PDF viewers render into a single `<canvas>`.
  The a11y tree sees one node; `snap` and every `--by` mode are blind past it.
  Today the only path is `raw Input.dispatchMouseEvent`, which forfeits the envelope, occlusion reporting, and `session` ergonomics.
- **Coordinate-first agents.**
  Computer-use models are trained to look at a screenshot and emit `click 512,340`.
  A CLI that cannot accept a coordinate cannot be driven by that loop, which shuts out a large class of agent users the tool should serve.
- **Text selection by pointer.**
  `tripleclick` selects a paragraph — the standard prelude to copy or overwrite in rich-text editors.
  `fill` uses the primitive internally, but a caller who wants *only* the selection (before `key cmd+c`) cannot reach it.
- **Drop-zone-only uploads.**
  Modern apps (Google Docs, Figma-style canvases, chat composers) increasingly accept files *only* via drag-and-drop, with no `<input type=file>` in the DOM.
  RFC-0006 declared this out of scope; real apps now require it, so the scope decision deserves reversal.
- **Deterministic pixel space.**
  Coordinates from a screenshot are only reusable if the window has not changed size between look and act.
  `emulate viewport` changes what the *page* sees but not the actual window, so screenshots of the real session and responsive testing both need `Browser.setWindowBounds`.

## Coordinate contract

One definition, used by every flag in this RFC:

- Coordinates are **CSS pixels**, origin at the top-left of the layout viewport, x right, y down.
- This is the same space `snap` geometry, the occlusion-verified centre resolution, and `Input.dispatchMouseEvent` already use.
- A screenshot taken with `screenshot --scale 1` maps 1:1 onto this space; the docs must state that coordinate workflows should capture with `--scale 1` (on HiDPI displays the default capture is device pixels, which do not map 1:1).
- A coordinate outside the current viewport is an error, not a silent clamp — a wrong-sized window is the most common cause, and clamping would turn it into a mis-click.

## User stories

**US-1 — Click a canvas control.**
As an automation author, I want to click at a point inside a canvas app so that I can press a button the DOM cannot see.
*Acceptance:* `chrome-cdp click --at 512,340` dispatches a trusted click at that point and reports what element was hit.

**US-2 — Drive the CLI from a screenshot loop.**
As an agent, I want to look at a screenshot, pick coordinates, and act on them so that I can operate pages with no usable a11y tree.
*Acceptance:* `screenshot --scale 1` → `click --at x,y` lands where the screenshot showed, in one `session`.

**US-3 — Draw or drag on a canvas.**
As an automation author, I want to drag between two points so that I can draw a stroke, lasso a selection, or pan a map.
*Acceptance:* `chrome-cdp drag --at 100,200 --to-at 400,200 --steps 30` produces intermediate move events the app renders as a stroke.

**US-4 — Select a paragraph.**
As an automation author, I want to triple-click a text block so that the whole paragraph is selected for a following copy or overwrite.
*Acceptance:* `chrome-cdp tripleclick "p.abstract"` leaves `window.getSelection()` spanning the paragraph.

**US-5 — Zoom a map at a point.**
As an automation author, I want to send wheel events anchored at a coordinate so that the map zooms around that point.
*Acceptance:* `chrome-cdp scroll --wheel --at 512,340 --dy -240` zooms in centred on the point.

**US-6 — Upload to a drop zone.**
As an automation author, I want to drop a file onto a drop target with no file input so that the app receives it as if I dragged it from Finder.
*Acceptance:* `chrome-cdp upload --drop ".dropzone" ./report.pdf` results in the app listing `report.pdf` as received.

**US-7 — Set the real window size.**
As an automation author, I want the actual Chrome window at exact dimensions so that screenshots and coordinates are reproducible across runs.
*Acceptance:* `chrome-cdp window size 1280 800` reports the resulting bounds, and `window.outerWidth` reflects them.

**US-8 — Know the window size before computing coordinates.**
As an agent, I want to read the current window bounds so that I can decide whether my cached coordinates are still valid.
*Acceptance:* `chrome-cdp window info` returns `{left, top, width, height, state}`.

## Proposed CLI surface

```
chrome-cdp click|dblclick|rclick|tripleclick|hover  (--at <x,y> | <selector>)
chrome-cdp drag  (--at <x,y> | <selector>)  (--to-at <x,y> | --to <selector> | --dx --dy)
chrome-cdp scroll --wheel --at <x,y> [--dx --dy]
chrome-cdp upload (--drop <selector> | --drop-at <x,y>) <path>...
chrome-cdp window size <width> <height>
chrome-cdp window info
```

New flags and rules:

| Flag | Applies to | Purpose |
|------|-----------|---------|
| `--at <x,y>` | all pointer verbs, `scroll --wheel` | act at a viewport coordinate; mutually exclusive with a selector and with every element-addressing flag (`--by`, `--role`, `--nth`, `--match`, `--in-row`) |
| `--to-at <x,y>` | `drag` | coordinate drop point; mutually exclusive with `--to` and `--dx/--dy` |
| `--drop <selector>` | `upload` | deliver files via synthesized drag-and-drop onto the element; mutually exclusive with the positional input-element selector |
| `--drop-at <x,y>` | `upload` | drop at a coordinate instead of an element |

`tripleclick` takes the full `QueryOpts` addressing set, exactly like `dblclick`.
`--modifiers` applies to `tripleclick` and to every `--at` click form.
`--wait-text` and `--on-dialog` compose with all of the above unchanged.
Mixed forms are allowed where they make sense: `drag "#card" --to-at 900,300` (element start, coordinate end) and `drag --at 100,100 --to "Trash" --to-by name` are both valid.

Examples:

```sh
chrome-cdp screenshot --scale 1 -o look.png
chrome-cdp click --at 512,340
chrome-cdp tripleclick "p.abstract"
chrome-cdp drag --at 120,400 --to-at 620,400 --steps 30
chrome-cdp scroll --wheel --at 512,340 --dy -240
chrome-cdp upload --drop "[data-testid=dropzone]" ./report.pdf
chrome-cdp window size 1280 800
```

## Result envelope

Coordinate actions report the point and what was under it:

```json
{ "ok": true, "command": "click",
  "target": {"id":"…","title":"…","url":"…"},
  "result": { "x": 512, "y": 340,
              "hit": {"tag": "canvas", "role": "img", "name": "Floor plan"},
              "modifiers": [] },
  "elapsed_ms": 74 }
```

`hit` is best-effort observability (`document.elementFromPoint` plus a11y lookup), not a precondition — a canvas hit is still a valid click.
Selector-form results keep their existing shape from RFC-0005, so `--at` adds a field but never changes an existing one.

`upload --drop` reports what was delivered and where:

```json
{ "ok": true, "command": "upload",
  "result": { "mode": "drop", "dropped_on": {"tag":"div","name":"Upload files"},
              "files": [{"name":"report.pdf","size":48213}] },
  "elapsed_ms": 210 }
```

`window size` and `window info` both report the settled bounds:

```json
{ "ok": true, "command": "window",
  "result": { "left": 0, "top": 25, "width": 1280, "height": 800, "state": "normal" },
  "elapsed_ms": 96 }
```

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| `--at` combined with a selector or any element-addressing flag; malformed `x,y`; both `--to` and `--to-at`; `--drop` with a positional selector; non-positive window dimensions | `usage` | 2 |
| Coordinate outside the current viewport | `coordinate_out_of_bounds` (new `Code*` + `codeToExit` entry) | 4 |
| `--drop` selector never resolves | `target_timeout` | 4 |
| Drop file path outside the policy upload allow-list | `policy_denied` | existing policy exit |
| `Input.dispatchMouseEvent` / `Browser.setWindowBounds` rejected | `cdp_error` | 5 |

`coordinate_out_of_bounds` reports the measured `viewport` on the error object (`{"viewport": {"width":…, "height":…}}`), so an agent can immediately re-screenshot at the right size instead of guessing.
It rides on the error object rather than a nested `details`, matching how `occluded` and `tab_hidden` are already reported.
All usage errors are rejected before connecting to Chrome, per the standing convention.

## Design notes

- **`--at` is a resolution bypass, not a new primitive.**
  `PointerOpts` gains `At *Point` and `ToAt *Point`; when `At` is set, the element-resolution and occlusion-verification stage is skipped and the existing dispatch sequence runs at the given point.
  The dispatch code (`mouseMoved` → `mousePressed` → `mouseReleased`, drag interpolation, modifier bitmasks) is untouched — this is why the RFC is cheap.
- **No occlusion check for coordinates, by design.**
  Occlusion verification exists to protect *element-intent* ("click the thing named X, wherever it is").
  A coordinate *is* the intent; second-guessing it would break canvas apps where `elementFromPoint` always returns the canvas.
  The `hit` field gives the caller the evidence to verify instead.
- **`tripleclick` reuses `fill`'s primitive.**
  `fill` already performs triple-click-select-all; factor that sequence out and expose it, exactly as RFC-0005 factored out `click`'s centre resolution.
  `Pointer` gains `Action: "tripleclick"` rather than a new interface method.
  **As implemented** it routes through `pointerClickSeq` — the primitive `dblclick` already uses — rather than `fill`'s single `clickCount: 3` event, because that dispatches the escalating 1-then-2-then-3 sequence a human triple-click actually puts on the wire, which is what Blink selects a paragraph on.
  `fill` is left as it is: it works, and changing a shipped write path to share this was not worth the risk.
- **Drop-zone mechanism: real files, synthetic events.**
  Primary approach: inject a temporary hidden `<input type=file>`, attach the files with `DOM.setFileInputFiles` (real CDP file attachment, same as RFC-0006), then in page JS move `input.files` into a `DataTransfer` and dispatch `dragenter` → `dragover` → `drop` on the target, then remove the input.
  The events are untrusted but the `File` objects are real, and drop handlers read `dataTransfer.files` — this is the approach known to work with the common drop-zone libraries.
  `Input.dispatchDragEvent` is the trusted-event alternative; see Open Questions.
- **Drop-zone paths obey RFC-0012.**
  `--drop` is still a file *upload*; the policy upload-path allow-list applies identically to both modes, checked before any page interaction.
- **`window` needs `windowState: normal` first.**
  `Browser.setWindowBounds` rejects size changes on a maximized/fullscreen window; the implementation sets state to `normal` first when needed, then applies bounds, then reads them back — the envelope reports what actually resulted, since the OS may clamp to screen size.
- **`window` is not `emulate viewport`, and the docs must say so.**
  `emulate viewport` lies to the page; `window size` moves the real window.
  For coordinate workflows the rule of thumb is: `window size` for reproducing what the user sees, `emulate viewport` for testing breakpoints — put this in `docs/cli-reference.md` next to both verbs.
- **Interface additions.**
  `chrome.Browser` gains `Window(ctx, targetID string, opts WindowOpts) (WindowBounds, error)` (info when `opts` is zero, resize otherwise); `UploadOpts` gains `Drop string` / `DropAt *Point`; `PointerOpts` gains the two points and the `tripleclick` action.
  Each needs its stub default in `chrometest.StubBrowser`, the daemon `remoteBrowser` forwarder, and the `dispatch` case — `TestDispatchCoversBrowser` enforces the last two.
- **MCP surface.**
  `click`, `pointer`, and `scroll` tools gain an optional `at` argument, and `pointer` gains `to_at` plus the `tripleclick` action; `upload` gains `drop`.
  **As implemented**, window sizing did NOT become its own tool: the default set is capped at 18 (RFC-0004 US-4), and a 19th would have broken a shipped, tested invariant.
  It is folded into the existing `tabs` tool as `action: "window_info"` / `"window_size"`, which is the same family — the browser surface being driven — and keeps the CLI's own `window` verb unchanged.
- **Stub defaults:** `Pointer` with `At` returns `{"x": at.X, "y": at.Y, "hit": null}`; `Window` returns `{0, 0, 1280, 800, "normal"}`; `Upload` with `Drop` returns `{"mode": "drop"}`.

## Verification scenarios

**VS-1 — Coordinate click lands where asked.**
Given a fixture canvas recording click coordinates into `window.__log`
When `click --at 200,150` runs
Then the log shows a trusted click at (200,150) and the envelope's `hit.tag` is `canvas`.

**VS-2 — Coordinate click reports the hit element.**
Given a fixture with a button occupying (100,100)–(200,140)
When `click --at 150,120` runs
Then `hit.role` is `button` and the button's handler fired.

**VS-3 — Out-of-viewport coordinate errors before dispatch.**
Given a viewport of 1280×800
When `click --at 2000,100` runs
Then exit 4, `coordinate_out_of_bounds`, `error.details.viewport` populated, and the fixture log records no event.

**VS-4 — `--at` with element flags never connects.**
When `click "#x" --at 10,10` or `click --at 10,10 --by name` runs
Then exit 2, `usage`, no browser method called.

**VS-5 — Triple-click selects the paragraph.**
Given a fixture with a multi-sentence `<p>`
When `tripleclick "p"` runs
Then `eval "window.getSelection().toString().length"` spans the paragraph, and the fixture's click listener saw `detail == 3`.

**VS-6 — Canvas drag produces a stroke.**
Given a fixture canvas recording every `mousemove` while the button is down
When `drag --at 100,200 --to-at 400,200 --steps 30` runs
Then at least 30 recorded points interpolate between the endpoints, bracketed by one mousedown and one mouseup.

**VS-7 — Mixed drag forms resolve both ends.**
Given a draggable element and a coordinate target
When `drag "#card" --to-at 600,300` runs
Then the drag starts at the element's centre and releases at (600,300).

**VS-8 — Anchored wheel zooms at the point.**
Given a fixture recording `wheel` events with coordinates
When `scroll --wheel --at 512,340 --dy -240` runs
Then the events carry (512,340) and the given delta.

**VS-9 — Drop-zone upload delivers real files.**
Given a fixture drop zone that reads `event.dataTransfer.files` into `window.__files`
When `upload --drop ".dropzone" ./fixture.pdf` runs
Then `__files` records the correct name and byte size, and no `<input type=file>` remains in the DOM afterwards.

**VS-10 — Drop respects the policy allow-list.**
Given a policy whose upload allow-list excludes the path
When `upload --drop ".dropzone" /etc/hosts` runs
Then the policy denial fires before any page interaction.

**VS-11 — Window resize round-trips.**
When `window size 1100 700` runs
Then the envelope bounds match `Browser.getWindowBounds`, and `eval "window.outerWidth"` agrees.

**VS-12 — Resize from maximized recovers.**
Given a maximized window
When `window size 1100 700` runs
Then the final state is `normal` at the requested size.

**VS-13 — Everything works inside `session`.**
Given a `session` of `["window","size","1280","800"]`, `["screenshot","--scale","1"]`, `["click","--at","512","340"]`
Then all three succeed over one connection.

## Test plan

**Unit — flag validation (`internal/cli`, stub failing on any browser call, `t.Parallel()`).**
VS-4, malformed coordinates, `--to`/`--to-at`/`--dx` exclusivity table, `--drop` exclusivity, non-positive window dimensions, and coordinate parsing (`512,340`, spaces, negatives).

**Unit — command boundary (`chrometest.StubBrowser`).**
Each `--at` form maps to `Pointer` with `At` set and resolution skipped; `tripleclick` maps to `Action: "tripleclick"`; `window size`/`info` map to `Window` with/without opts; `upload --drop` maps to `UploadOpts.Drop`; envelope shapes match this RFC.

**Live Chrome (`internal/chrome`, `testing.Short()`-guarded).**
VS-1, VS-2, VS-3, VS-5 through VS-9, VS-11 against `data:`/fixture-server pages recording events into `window.__log`.
VS-12 may be skipped under headless if the environment cannot maximize; assert the state-normalization call instead.

**Coordinate-contract regression.**
A test asserting that a `screenshot --scale 1` of a fixture with a marker at a known CSS position shows the marker at exactly those pixel coordinates — this is the test that keeps the coordinate contract honest on HiDPI CI machines.

**Session integration (`internal/cli`).**
VS-13 through the existing `session` harness.

## Out of scope

- Touch, pinch, and multi-touch gestures (unchanged from RFC-0005).
- Automatic device-pixel↔CSS-pixel translation for screenshots taken at other scales — the contract is `--scale 1`, documented.
- Window *positioning* (`--left/--top`), minimize/maximize verbs, and multi-monitor placement — `window size` covers the reproducibility need; revisit if a real workflow demands placement.
- Drag between two tabs or out of the browser.
- The OS-native file picker — still permanently out of scope; `--drop` exists precisely so it never has to open.

## Open questions

1. Should the drop mechanism prefer `Input.dispatchDragEvent` (trusted events, but file support varies by Chrome version) over injected-input-plus-synthetic-events (untrusted events, real `File` objects, known to work with common libraries)?
   **Recommendation:** ship the injected-input approach first with a fixture per major drop-zone pattern; switch or add `dispatchDragEvent` only if a live fixture proves a target that rejects untrusted events.
2. Should a coordinate click warn (in the envelope, not by failing) when `hit` resolves to a `disabled` control?
   **Recommendation:** yes — populate `hit.states`; never block, since canvas apps are the whole point.
3. Should out-of-viewport coordinates offer an explicit `--clamp` escape hatch for pages with scroll-linked layouts?
   **Recommendation:** no; `scroll` then `click --at` is the composable answer, and clamping hides real bugs.
4. Should `window size` accept named presets (`--preset mobile|laptop`)?
   **Recommendation:** no; presets belong in recipes, which exist for exactly this.
