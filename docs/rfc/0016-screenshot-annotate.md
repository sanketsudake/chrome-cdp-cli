# RFC-0016: `screenshot --annotate` — numbered element labels with a legend in the envelope

- **Status:** Draft
- **Priority:** P2
- **Area:** capture
- **Depends on:** RFC-0008 (`ShotOpts`, the clip/scale model the labels are mapped through), RFC-0011 (`internal/encode` marker drawing)
- **Related:** RFC-0014 (`center` follows its coordinate contract), RFC-0015 (`find` shares the a11y filter and ref minting the labels use)

## Summary

Add `--annotate` to `screenshot`.
It draws a numbered label `[N]` at the centre of every *actionable* element in the captured area and appends a legend to the envelope — `result.annotations: [{n, ref, role, name, center:{x,y}}]` — so a reader of the image can name what they see, and an agent can act on it with `--by ref` or `--at` without a second read.

The labels come from the same accessibility pass `snap` and `find` already use — the same node filter, the same `e<id>` refs — and the markers are drawn by `internal/encode`, so a screenshot label and a `record` marker share one visual language.
Without `--annotate` the verb is byte-for-byte what it is today.

`record --annotate` is a different thing and must not be conflated: it draws *pointer marks* on *frames* (where an action landed, RFC-0011).
This RFC draws *numbered element labels* on *one still* (what could be acted on).
They share the disc-and-ring drawing code in `internal/encode`, and nothing else.

## Motivation

A screenshot and a snapshot each answer half the question.

- **The image shows layout but names nothing.**
  A model or a human looking at a capture sees a button, but not its accessible name, its ref, or whether it is the second of three identical-looking icons.
  Acting on what was seen means a separate `snap`/`find` and a guess at which node is which.
- **The snapshot names everything but shows nothing.**
  On a dense page `snap` returns hundreds of nodes, and nothing in the list says where they sit or how they relate visually.
  The two are correlated by hand today, and that correlation is where mistakes happen.
- **Coordinate-first surfaces already do this.**
  The capability gap analysis behind RFC-0014/0015 named "set-of-mark" screenshots — numbered overlays with a legend — as the remaining reading gap.
  It is what makes a single image actionable: the reader says "3", and "3" is a ref.
- **Every piece exists.**
  `snap` has the tree, the filter and the refs; `find` has the per-node centre measurement through the shared geometry primitive; `encode` has the marker drawing; `Screenshot` already knows its clip and scale.
  This RFC wires them together behind one bool.

## User stories

**US-1 — See what I can act on.**
As an agent, I want every actionable element in a screenshot numbered so that I can refer to it without a second read.
*Acceptance:* `chrome-cdp screenshot --annotate` produces an image with numbered labels and an envelope whose `annotations` array has one entry per label.

**US-2 — Act on what I saw.**
As an agent, I want each label to carry a ref and a centre so that my next command addresses it directly.
*Acceptance:* `annotations[2].ref` is an `e<id>` that `click --by ref` accepts; `annotations[2].center` is a viewport coordinate `--at` accepts when the element is on screen.

**US-3 — Labels on a cropped capture.**
As an automation author, I want `--annotate` to work with `--selector`, `--region` and `--full-page` so that a cropped or long capture is labelled too.
*Acceptance:* with `--selector "#toolbar"` only elements inside the toolbar's clip are labelled and listed; an element outside the clip appears in neither the image nor the legend.

**US-4 — Never lose the screenshot.**
As a script author, I want `--annotate` to degrade rather than fail so that a backgrounded tab still gives me the picture.
*Acceptance:* on a hidden tab the command exits 0 with the plain image, `annotated: false` and `reason: "tab_hidden"`.

**US-5 — Stay bounded on a huge page.**
As an agent, I want a hard cap on labels so that a page with 500 controls produces a legible image and a bounded envelope.
*Acceptance:* at most 200 labels are drawn; `truncated: true` reports that the cap was hit.

**US-6 — The plain capture is untouched.**
As a user relying on today's behaviour, I want a screenshot without `--annotate` to be exactly what it was.
*Acceptance:* without the flag, the bytes and the envelope shape are unchanged, and no accessibility read happens.

## Proposed CLI surface

```
chrome-cdp screenshot [--annotate] [existing flags…]
```

| Flag | Default | Purpose |
|------|---------|---------|
| `--annotate` | off | number every actionable element in the captured area and list them in `result.annotations`; on a backgrounded tab the labels are skipped (`annotated: false`) — run `activate` first |

The flag's help text says the last clause verbatim: the hidden-tab degradation is the one thing a caller needs to know before reaching for it.

`--annotate` composes with every existing mode and flag, with two exceptions that are usage errors before connecting:

- `--annotate --format webp`: the labels are drawn in-process and Go's standard library cannot encode WebP (and `imageDims` already documents that it cannot decode it); use `png` or `jpeg`.
- `--annotate -o -`: the legend lives in the envelope and `-o -` emits none, only bytes; a flag whose result would be silently dropped is refused, for the same reason `--quality` with `png` is (RFC-0008).

Examples:

```sh
chrome-cdp screenshot --annotate
chrome-cdp screenshot --annotate --selector "#toolbar" --padding 8 -o toolbar.png
chrome-cdp screenshot --annotate --format jpeg --quality 60 --scale 0.5
chrome-cdp screenshot --annotate --full-page -o report.png
```

## Result envelope

```json
{ "ok": true, "command": "screenshot",
  "target": {"id":"…","title":"…","url":"…"},
  "result": { "path": "./screenshot-20260819-101500.png", "bytes": 51220,
              "width": 1280, "height": 800, "format": "png",
              "scale": 1, "mode": "viewport",
              "clip": {"x": 0, "y": 0, "width": 1280, "height": 800},
              "annotated": true,
              "annotations": [
                { "n": 1, "ref": "e41", "role": "link", "name": "Home",
                  "center": {"x": 64, "y": 22} },
                { "n": 2, "ref": "e57", "role": "button", "name": "Save",
                  "states": ["disabled"], "center": {"x": 1180, "y": 22} },
                { "n": 3, "ref": "e88", "role": "textbox", "name": "Search",
                  "center": {"x": 640, "y": 22}, "occluded": true }
              ],
              "truncated": false },
  "elapsed_ms": 210 }
```

Every RFC-0008 field is unchanged.
With `--annotate` the result gains:

| Field | Meaning |
|-------|---------|
| `annotated` | `true` when at least one label put pixels on the image — the same meaning `encode.Result.Annotated` has for a recording |
| `annotations` | the legend, in label order; always present with `--annotate`, possibly empty |
| `truncated` | `true` when more actionable nodes existed than the cap allowed |
| `reason` | present only when `annotated` is `false`: `tab_hidden` \| `tree_unavailable` \| `no_actionable_nodes` (none in the captured area) |

A legend entry carries `n` (the number drawn on the image), `ref`, `role`, `name`, and `center`, plus `states` (omitted when empty) and `occluded: true` (omitted when false) using exactly `snap`'s and `find`'s vocabulary.
`center` is the element's centre in **viewport CSS pixels at capture time**, the RFC-0014 contract `find` already reports — the coordinate `--at` takes.
For a `--full-page` capture a below-the-fold centre is therefore outside the viewport and `--at` will refuse it; use `--by ref` for those, which is why every entry carries one.

Without `--annotate` none of these fields appear.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| `--annotate` with `--format webp` | `usage` | 2 |
| `--annotate` with `-o -` | `usage` | 2 |
| Tab hidden, tree timed out, or no actionable nodes | *not an error* — `ok: true`, plain image, `annotated: false` with `reason` | 0 |
| Every existing RFC-0008 failure (`target_timeout`, `zero_area`, `cdp_error`, unwritable path) | unchanged | unchanged |

No new codes.
The annotation pass can never turn a successful capture into a failure.

## Design notes

### Interface: one bool, no RPC change

`chrome.ShotOpts` gains `Annotate bool`.
`Screenshot`'s signature is unchanged; the legend travels in the metadata map it already returns, which the CLI already merges into the envelope (`emitArtifact`).
The daemon's `argShot` decodes `ShotOpts` generically (`arg[chrome.ShotOpts]`), and `capture()` forwards bytes and metadata as JSON, so **neither half of the RPC changes**; `TestDispatchCoversBrowser` stays green without a new case.
`chrometest.StubBrowser.Screenshot` passes `ShotOpts` through unchanged and needs no edit.

### Which nodes get labels: the actionable predicate

`snap` has no `--interactive` flag; `Snapshot` reports the whole tree, ignored nodes included.
So "actionable" is a **new pure predicate over the same nodes**, not a second traversal and not a second grammar.
Tree acquisition is the same `accessibility.GetFullAXTree` call, filtered through the shared `axFilterNodes`/`axRef` helpers `snap` and `find` both use (with `IncludeIgnored: false`, like `find`), so a labelled node's ref is exactly the ref `snap` would print for it.
Calling `Snapshot` itself is not the mechanism — it shapes an envelope, not a node list — but the node set and the refs are the ones it would report.

A node is actionable when **all** of:

1. it is not ignored and has a backend DOM node (`axRef(n) != ""`);
2. either its role is in `annotateRoles`, **or** it carries the `focusable` property and its role is not in `annotateStructuralRoles`.

`annotateRoles` (the things a person clicks, types into, or toggles), verbatim:
`button`, `link`, `textbox`, `searchbox`, `combobox`, `checkbox`, `radio`, `switch`, `slider`, `spinbutton`, `tab`, `menuitem`, `menuitemcheckbox`, `menuitemradio`, `option`, `treeitem`.

`annotateStructuralRoles` (containers Chrome may mark focusable — keyboard-focusable scrollers, the document root — that are never the thing to act on), verbatim:
`generic`, `none`, `presentation`, `RootWebArea`, `WebArea`, `document`, `application`, `region`, `main`, `group`, `list`, `table`, `grid`.

`cell` and `gridcell` are deliberately **not** in `annotateRoles`: a data grid would consume the whole cap in cells, and grid cells already have first-class addressing (`--by cell`).
A focusable gridcell (Workday's editable cells carry `tabindex`) still qualifies through rule 2, which is the behaviour wanted: an editable cell is actionable, a read-only one is not.
Disabled nodes are included and reported with `states: ["disabled"]`, as `find` does — "the Save button is disabled" is an answer the image should give.

Both lists live in one file, `internal/chrome/annotate.go`, beside the predicate `annotateActionable(n *accessibility.Node) bool`, pinned by a table-driven test (`TestAnnotateActionable`) so a change to either is a visible diff.

### Numbering, cap, and drops

1. Candidates are the actionable nodes **in tree (document) order**, the order `snap` prints.
2. The first **200** candidates are measured; the rest are dropped and `truncated: true` is set.
   200 is a legibility and latency bound: the measurement costs two CDP calls per node (below), so the cap bounds the pass at ~400 round trips — well under a second on a local socket — and more than 200 labels on one image are not readable anyway.
3. Measurement drops a candidate that is detached, has a zero-area box (`w < 1 || h < 1`), or whose centre maps outside the image (below).
4. **`n` is assigned 1..K after the drop pass**, so the numbers on the image and the legend are contiguous and exactly 1:1.
   Drops are not backfilled from beyond the cap.

Hence `len(annotations) == K`, `annotations[i].n == i+1`, and every legend entry is a label that is on the image.
Because every surviving centre is inside the image, `AnnotateImage`'s per-label report (below) is expected to be all `true`; `annotated` is simply "any entry true", and no further drop happens after numbering.

### Geometry: one primitive, one formula

Each candidate is measured exactly as `find` measures its matches (`enrichFindCenters`): `DOM.resolveNode` by backend id, then `measureNode(…, nodeBoxJS)` — the **non-scrolling** variant of the shared geometry primitive in `geometry.go`.
That is the whole point of reusing it: a label's `center` must be the same point `find` reports and a pointer verb lands on.
The pass must not scroll the page: it is a read, and the capture already happened.

`measureNode` returns viewport CSS coordinates (`cx, cy`, unclamped).
The image is in page-coordinate clip space at some pixel density.
The mapping, with `vp` = the visual viewport origin in page coordinates from `layoutRects` **re-read at measurement time** (the element mode may have scrolled), `clip` = the rectangle `Screenshot` resolved, and `W, H` = the decoded image's pixel dimensions (`imageDims`):

```
page_x = cx + vp.X                  page_y = cy + vp.Y
img_x  = (page_x − clip.X) × W / clip.Width
img_y  = (page_y − clip.Y) × H / clip.Height
```

The `W / clip.Width` ratio absorbs both `--scale` and the device scale factor, which is why the formula is stated against the decoded size and not against `opts.Scale`.
A candidate whose `img_x ∉ [0, W)` or `img_y ∉ [0, H)` lies outside the clip and is dropped (US-3); this is the only place `--selector`, `--region` and `--full-page` differ from the viewport case, and they differ only through `clip`.

In the viewport mode without a clip (`scale == 1`) `clip` is the visual viewport rectangle `Screenshot` already computes, so the same formula holds.

Inside `Screenshot` the order is: resolve clip → capture → annotation pass → encode.
The pass runs inside the same `c.run` action, after the capture, under its own bounded context (below), and stores the labels in **CSS pixels relative to the clip's top-left** (`page − clip.origin`) together with `clip.Width/Height`, which is the `encode.Mark`/`Frame.CSSWidth` convention — so `encode` performs the last multiplication with the same arithmetic `drawMarks` uses.

### Drawing: `internal/encode`, the same marker

`internal/encode` is pure and imports nothing from the repo, so `internal/chrome` may import it (its tests already do).
It gains:

```go
// Label is a numbered position marker: the disc-and-ring of a Mark plus the
// number N. X, Y are CSS pixels from the capture's top-left, Mark's space.
type Label struct {
    N    int
    X, Y float64
}

// AnnotateImage decodes data (png or jpeg), draws each label, and re-encodes
// as format ("png" | "jpeg"; quality applies to jpeg only). It reports, per
// label, whether it put any pixel on the canvas — the meaning Annotated has
// for a recording. It is pure: synthetic-image tests pin the drawing.
func AnnotateImage(data []byte, format string, quality int, cssW, cssH float64, labels []Label) ([]byte, []bool, error)
```

A label is drawn as:

- the **same disc-and-ring** `drawMarks` draws for a recording — red `#E11D48` disc inside a white ring, radius `markRadius` (9) scaled by pixels-per-CSS-pixel, minimum 3 — centred on the point, through the existing `disc` helper;
- plus a **badge**: a filled rectangle in the same red with a 1-pixel white border, anchored at the disc's upper-right (`+r, −r` from the centre) and shifted inward when it would leave the image, carrying `N` in white digits.
  Digits come from a built-in 5×7 bitmap font for `0`–`9` (about sixty bytes of table; no font dependency, which keeps the stdlib-only property `encode`'s package comment makes load-bearing), drawn at integer scale `k = max(1, round(r / 6))` so a label is ~14 px tall at scale 1 and grows with density.

Only the disc-and-ring is shared code; the badge is new and belongs in `encode` beside `drawMarks` so the two stay visually aligned.
`TestDrawLabels` draws onto a synthetic image and samples pixels: the disc colour at the centre, white inside the badge, and nothing drawn for a label outside the canvas (its `[]bool` entry is `false`).

### Formats

- `png`: capture PNG, draw, re-encode PNG.
- `jpeg`: capture **PNG** from Chrome (lossless), draw, encode JPEG in Go at `--quality` — one lossy generation rather than two.
  `bytes` and `format` then describe Go's encoder output, not Chrome's; `width`/`height` are unchanged.
- `webp`: usage error with `--annotate` (no standard-library encoder).

The CLI rejects the webp combination in `shotOpts`, before connecting, and the driver never re-checks it.

### Bounded, never blocking: the hidden-tab rule

The accessibility tree is throttled on a backgrounded tab and `--by name`/`ref`/`cell` stall there (`classifyWithTabHint`, `tab_hidden`).
An annotation pass inherits that and must degrade, following the precedent `key` sets for its best-effort `focused` read (`focusedReadTimeout`, `key.go`):

- the whole pass (tree read plus measurements) runs under `annotateTimeout = 3 * time.Second`, a child of the action context;
- when the pass times out, errors, or yields **zero actionable nodes**, the driver probes `tabHidden(actx)` — on the action context, not the expired child — and if the tab is hidden the pass returns no labels and `reason: "tab_hidden"`;
- otherwise a timeout or error on a visible tab is `reason: "tree_unavailable"`;
- otherwise a clean pass that ends with no label on a visible tab — no actionable node at all, or none left inside the captured area after the drop pass — is `reason: "no_actionable_nodes"`.

In every one of those cases the bytes are the plain capture and `annotated: false`; the command exits 0.
No `dom_fallback` is attempted: `find`'s DOM fallback resolves no refs, and a label without a ref is a number with nothing behind it.

### Policy, MCP, session

- **Policy:** `screenshot` stays `Reading`; `--annotate` reads the tree and draws on bytes in-process, and dispatches nothing.
  No `verbClass` change.
- **MCP:** the existing `chrome_cdp_screenshot` tool gains one boolean argument, `annotate`, mapped to `--annotate`.
  No new tool — the default set is capped at 18 (RFC-0004), and an argument on an existing tool is free.
  The legend reaches the client through `structuredContent` (the envelope's `result`) alongside the image block, so an MCP client gets picture and legend in one call.
  `annotate` with `format: "webp"` is the same `usage` refusal, raised by the CLI the server runs.
- **`session`:** nothing special; `screenshot` is an ordinary argv verb and the new flag is re-registered per line like every other.
- **`record`:** untouched.
  `record start --annotate` / `record stop --annotate` keep their meaning (pointer marks at export); this RFC reuses their drawing and shares no flag state with them.

### Stub, docs, skill

- `chrometest.StubBrowser`: unchanged; the stub-backed CLI test installs its own `Screenshot` override returning a metadata map with `annotated` and `annotations`, exactly as `TestScreenshotEnvelopeCarriesMetadata` does for `clip`.
- `docs/cli-reference.md` screenshot section: the flag row, the two usage-error combinations, the legend fields, the hidden-tab degradation.
- `skills/drive-chrome-cdp/references/core.md` "Reading the page → screenshot": one clause — "`--annotate` numbers the actionable elements and lists them as `annotations[].ref`/`center`; `activate` the tab first".

## Verification scenarios

**VS-1 — Three buttons, three labels.**
Given a fixture with exactly three buttons
When `screenshot --annotate` runs on a foreground tab
Then `annotated` is `true`, `len(annotations) == 3`, `annotations[i].n == i+1`, each `ref` is a non-empty `e<id>`, each `role` is `button`, and each `center` lies inside `[0,width) × [0,height)` after the viewport-to-image mapping (which at scale 1 with DPR 1 is the identity).

**VS-2 — Legend entries are live addresses.**
Given VS-1
When `click --by ref <annotations[1].ref>` runs in the same `session`
Then the click lands on the second button (the fixture records it in `window.__log`).

**VS-3 — Cropped capture drops outside nodes.**
Given a fixture with two buttons inside `#toolbar` and one below it
When `screenshot --annotate --selector "#toolbar"` runs
Then `len(annotations) == 2` and neither entry names the outside button.

**VS-4 — Scale maps correctly.**
Given VS-1
When `screenshot --annotate --scale 0.5` runs
Then each label's drawn position equals its scale-1 position halved — assert by sampling the disc colour at `(center × 0.5)` in the decoded image, never by pixel equality.

**VS-5 — Hidden tab degrades.**
Given the fixture tab pushed to the background (the `TestScreenshotOffscreenElementOnBackgroundTab` rig)
When `screenshot --annotate` runs
Then exit 0, the image decodes, `annotated` is `false`, `reason` is `tab_hidden`, `annotations` is empty, and the command returned within the bounded window.

**VS-6 — Cap and truncation.**
Given a fixture with 250 buttons
When `screenshot --annotate --full-page` runs
Then `len(annotations) == 200`, `truncated` is `true`, and the numbers are contiguous 1..200.

**VS-7 — Usage combinations.**
Table: `--annotate --format webp` → `usage` exit 2, no browser call; `--annotate -o -` → `usage` exit 2, no browser call; `--annotate --format jpeg --quality 60` → ok and `format: "jpeg"`.

**VS-8 — Flag reaches the driver and legend reaches the envelope.**
Stub-backed: `--annotate` sets `ShotOpts.Annotate`; a stub returning `annotated: true` and an `annotations` array sees both in `result`.

**VS-9 — The plain capture is untouched.**
Without `--annotate`, `ShotOpts.Annotate` is false, no accessibility read happens, and the envelope carries none of `annotated`/`annotations`/`truncated`/`reason`.
The existing RFC-0008 tests already guard the bytes and the remaining fields.

**VS-10 — MCP argument.**
`chrome_cdp_screenshot` with `annotate: true` builds `screenshot --annotate`, and the result's `structuredContent` carries `annotations`.

## Test plan

**Pure (`t.Parallel()`).**
`TestAnnotateActionable` (`internal/chrome/annotate_test.go`): a table over synthetic `accessibility.Node`s covering each role in `annotateRoles`, a focusable `generic`, a focusable `gridcell`, a non-focusable `gridcell`, an ignored button, and a node with no backend id.
`TestDrawLabels` (`internal/encode/encode_test.go`): `AnnotateImage` on a synthetic PNG — disc colour at the centre, white within the badge, a label outside the canvas reports `false`, jpeg output decodes with `image/jpeg`.

**Stub-backed (`internal/cli/capture_test.go`).**
`TestScreenshotAnnotateFlagAndLegend` (VS-8, pattern of `TestScreenshotFlagsReachTheDriver` + `TestScreenshotEnvelopeCarriesMetadata`); `TestScreenshotAnnotateRejectedCombos` (VS-7, the `noCallBrowser` table); VS-9 as an assertion added to `TestScreenshotNoFlagsWritesPNG`.

**MCP (`internal/mcp/tools_test.go`).**
The `annotate` argument builds `--annotate` (VS-10), alongside the existing arg-mapping cases.

**Live Chrome (`internal/chrome/screenshot_annotate_test.go`, `testing.Short()`-guarded, not parallel).**
`TestScreenshotAnnotateThreeButtons` (VS-1, VS-2, VS-4) on a `captureFixture` page with three buttons; `TestScreenshotAnnotateSelectorDropsOutsideNodes` (VS-3); `TestScreenshotAnnotateOnBackgroundTab` (VS-5) on the existing background-tab rig; `TestScreenshotAnnotateCap` (VS-6).
Decode with `image/png`/`image/jpeg` and assert on counts, bounds, and sampled colours — never byte-for-byte image equality.

## Out of scope

- Labelling non-actionable nodes (headings, static text, images without a role that acts).
- Drawing bounding boxes, connectors, or a caption bar; the label is a marker and a number.
- A `--interactive` filter for `snap`; if one is ever wanted it should reuse `annotateActionable`, not redefine it.
- A DOM fallback for the hidden-tab case.
- Labels on `record` frames.

## Open questions

None.
The choices above are decisions; the implementing PR records any departure in this section.
