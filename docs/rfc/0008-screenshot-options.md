# RFC-0008: Screenshot options — element, full-page, region, format

- **Status:** Accepted — implemented in [#11](https://github.com/sanketsudake/chrome-cdp-cli/pull/11)
- **Priority:** P1
- **Area:** capture
- **Depends on:** —
- **Related:** RFC-0004 (MCP returns screenshots as image content blocks)

## Summary

Extend `screenshot` beyond the current full-viewport PNG with `--selector` (element capture), `--full-page` (beyond the fold), `--region` (explicit rectangle), `--format`/`--quality` (JPEG/WebP), and `--scale`.
Extend `pdf` with the page-setup options CDP already supports.

## Motivation

`screenshot` today captures the viewport, as PNG, at device scale, with no options.
That is the right default and the wrong ceiling.

- **Element capture is the common ask.**
  When something looks wrong, the useful artifact is that component, not a 1400×900 page containing it.
  For an agent, the difference is larger still: a viewport PNG can be hundreds of kilobytes of mostly-irrelevant pixels charged against a context budget, where a cropped element is a few kilobytes of signal.
- **Full-page capture has no substitute.**
  Long report or dashboard pages cannot be captured at all today, and stitching viewport shots by scrolling is both lossy and defeated by sticky headers and lazy-loading.
- **Format matters for size.**
  PNG of a photo-heavy page is several times the size of an equivalent JPEG.
  For bug reports, CI artifacts, and agent context, that ratio is the difference between usable and not.
- **`pdf` is similarly bare.**
  No landscape, margins, paper size, page ranges, background printing, or header/footer — all of which CDP's `Page.printToPDF` exposes for free, and all of which the "export this internal report to PDF" use case immediately wants.

None of this needs new infrastructure; it is surfacing parameters `Page.captureScreenshot` and `Page.printToPDF` already accept, plus element-rect resolution that `click` already performs.

## User stories

**US-1 — Capture one component.**
As a developer filing a bug, I want a screenshot of just the broken component so that the image shows the problem without cropping by hand.
*Acceptance:* `chrome-cdp screenshot --selector "#invoice-table" -o bug.png` produces an image bounded to that element.

**US-2 — Capture a long page.**
As an automation author, I want the entire scrollable page so that a report longer than the viewport is captured in one image.
*Acceptance:* `chrome-cdp screenshot --full-page` produces an image taller than the viewport with the full content.

**US-3 — Keep the artifact small.**
As an agent, I want a JPEG at reduced scale so that a visual check costs a fraction of the tokens a full-resolution PNG would.
*Acceptance:* `chrome-cdp screenshot --format jpeg --quality 60 --scale 0.5` yields a substantially smaller file, and the envelope reports the byte size and dimensions.

**US-4 — Capture an offscreen element.**
As an automation author, I want an element below the fold captured without scrolling manually so that my script does not need a scroll-then-shoot dance.
*Acceptance:* `screenshot --selector` on an offscreen element succeeds and the image contains it.

**US-5 — Capture an exact rectangle.**
As an automation author, I want to specify a pixel rectangle so that I can capture a region that no single element bounds.
*Acceptance:* `chrome-cdp screenshot --region 0,0,400,300` produces a 400×300 image (before `--scale`).

**US-6 — Export a report properly.**
As a user archiving an internal report, I want landscape orientation, A4 paper, and printed backgrounds so that the PDF matches what I see.
*Acceptance:* `chrome-cdp pdf --landscape --paper A4 --background` produces a correctly laid-out PDF.

**US-7 — Know what I got.**
As a script author, I want the envelope to report dimensions, format, and byte size so that I can assert on the artifact without opening it.
*Acceptance:* the envelope includes `width`, `height`, `format`, `bytes`, and `path`.

## Proposed CLI surface

```
chrome-cdp screenshot [--selector <sel>] [--full-page] [--region x,y,w,h]
                      [--format png|jpeg|webp] [--quality <n>] [--scale <f>]
                      [--padding <px>] [-o <path>]

chrome-cdp pdf [--landscape] [--paper <name>] [--margin <spec>] [--scale <f>]
               [--background] [--pages <ranges>] [--header <tpl>] [--footer <tpl>]
               [-o <path>]
```

**screenshot**

| Flag | Default | Purpose |
|------|---------|---------|
| `--selector <sel>` | — | capture this element's bounding box; all `QueryOpts` flags apply |
| `--full-page` | off | capture the full scrollable page |
| `--region x,y,w,h` | — | explicit page-coordinate rectangle |
| `--format` | `png` | `png` \| `jpeg` \| `webp` |
| `--quality <n>` | `80` | 0–100; jpeg/webp only |
| `--scale <f>` | `1` | output scale factor, 0.1–3 |
| `--padding <px>` | `0` | expand an element capture by this many pixels |

`--selector`, `--full-page`, and `--region` are mutually exclusive.

**pdf**

| Flag | Default | Purpose |
|------|---------|---------|
| `--landscape` | off | orientation |
| `--paper <name>` | `letter` | `letter` \| `legal` \| `tabloid` \| `a0`–`a6`, or `WxH` in inches |
| `--margin <spec>` | `0.4in` | one value, or `top,right,bottom,left` |
| `--scale <f>` | `1` | 0.1–2 |
| `--background` | off | print background graphics |
| `--pages <ranges>` | all | e.g. `1-3,5` |
| `--header <tpl>` / `--footer <tpl>` | — | HTML templates |

Examples:

```sh
chrome-cdp screenshot --selector "#invoice-table" --padding 8 -o invoice.png
chrome-cdp screenshot --full-page -o report.png
chrome-cdp screenshot --format jpeg --quality 60 --scale 0.5 -o small.jpg
chrome-cdp screenshot --by name "Summary card" --role region
chrome-cdp pdf --landscape --paper a4 --background -o report.pdf
```

## Result envelope

```json
{ "ok": true, "command": "screenshot",
  "target": {"id":"…","title":"…","url":"…"},
  "result": { "path": "./invoice.png", "bytes": 48213,
              "width": 812, "height": 344, "format": "png",
              "scale": 1, "mode": "element",
              "clip": {"x": 210, "y": 940, "width": 812, "height": 344} },
  "elapsed_ms": 96 }
```

`mode` is one of `viewport` \| `element` \| `full_page` \| `region`, so a caller can confirm which path ran.
`clip` is the resolved rectangle in page coordinates — the thing most useful for debugging a capture that came out wrong.
The existing `-o -` stdout behaviour and default `./screenshot-<timestamp>.<ext>` naming with the collision counter are preserved; only the extension follows `--format`.

`pdf` reports `path`, `bytes`, and `pages`.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| More than one of `--selector`/`--full-page`/`--region`; malformed `--region`; `--quality` with `png`; out-of-range `--quality`/`--scale`; unknown `--format`/`--paper`; malformed `--margin`/`--pages` | `usage` | 2 |
| `--selector` never resolves | `target_timeout` | 4 |
| Element resolves but has a zero-area box (`display:none`, collapsed) | `target_timeout` with `zero_area: true` | 4 |
| Full-page capture exceeds Chrome's texture limits | `cdp_error` | 5 |
| Output path not writable | `generic` | 1 |

No new codes.

## Design notes

- **Interface** — replace the current signature rather than adding parallel methods: ```go Screenshot(ctx context.Context, targetID string, opts ShotOpts) ([]byte, map[string]any, error) PDF(ctx context.Context, targetID string, opts PDFOpts) ([]byte, map[string]any, error) ``` Both now return metadata alongside the bytes, because US-7 needs dimensions the CLI cannot know without decoding the image.
  This is a breaking change to `chrome.Browser` and to `chrometest.StubBrowser`, contained entirely in-tree.
- **Element capture** resolves the node's box with `DOM.getBoxModel` (or the box `click`'s centre resolution already computes — reuse it) and passes it as `clip` to `Page.captureScreenshot`.
  For an offscreen element (US-4), scroll it into view first using the existing `scroll --to` path, then re-read the box; capturing a stale rect is the obvious bug here and the reason VS-4 exists.
- **`--padding`** expands the clip and is then clamped to the page bounds, so padding near an edge does not produce a negative origin.
- **Full-page** uses `captureBeyondViewport: true` with a clip derived from the layout metrics rather than the older resize-then-restore trick.
  The old approach mutates the page under capture, breaks sticky elements, and leaves the viewport wrong if the process dies mid-capture.
- **Lazy-loaded content is a known limitation** of full-page capture: images below the fold that load on scroll may be blank.
  Document it, and point at `scroll` plus `wait --idle` as the remedy rather than trying to auto-scroll invisibly, which would be slow and still unreliable.
- **`--scale`** maps to `captureScreenshot`'s scale, not a post-capture resize, so text stays crisp.
- **`--quality` with PNG is a usage error, not a silent ignore.**
  Accepting a flag that does nothing trains users to believe it worked.
- **`pdf --paper`** accepts named sizes case-insensitively and a `WxH` form in inches, converted in the CLI so the driver receives numbers only.
  Named-size parsing is a pure function and belongs in a table-driven unit test.
- **Stub:** returns a 1×1 PNG's bytes plus `{"width":1,"height":1,"format":"png","mode":"viewport"}`.

## Verification scenarios

**VS-1 — Element capture is bounded to the element** Given a fixture with a 300×200 element at a known offset When `screenshot --selector "#box"` runs Then the envelope reports `width: 300`, `height: 200` (times device scale), `mode: "element"`, and the decoded PNG's dimensions match.

**VS-2 — Padding expands and clamps** Given the same element at the page's top-left corner When `--padding 20` is added Then the clip's origin is not negative and the reported size grew by at most the available room.

**VS-3 — Offscreen element is scrolled into view and captured correctly** Given an element 3000px down the page When `screenshot --selector` targets it Then the capture contains it — assert by sampling a known pixel colour rather than by eyeballing.

**VS-4 — Stale-rect regression** Given the same offscreen element When captured Then the reported `clip` matches the element's box **after** scrolling, not before.
This is the specific bug the implementation is most likely to have; it deserves a named test.

**VS-5 — Full-page exceeds the viewport** Given a fixture 3× the viewport height When `screenshot --full-page` runs Then the reported height is greater than the viewport height and `mode` is `full_page`.

**VS-6 — Region capture** When `screenshot --region 10,20,400,300` runs Then the decoded image is 400×300 (times scale) and `clip` echoes the request.

**VS-7 — Format and size** When the same page is captured as PNG and as `--format jpeg --quality 50` Then both decode successfully, the reported `format` differs, the default output extension follows the format, and the JPEG is smaller.

**VS-8 — Scale** When `--scale 0.5` is used Then the decoded dimensions are half those of the unscaled capture.

**VS-9 — Mutually exclusive modes** Table: each of the three alone → ok; any two together → `usage` exit 2, with no browser call.

**VS-10 — Malformed region and out-of-range values** Table over `--region "1,2,3"`, `"a,b,c,d"`, negative width, `--quality 200`, `--scale 0`, `--quality` with `--format png`, unknown `--format`.
All `usage` exit 2, no browser call.

**VS-11 — Zero-area element** Given `<div id=x style="display:none">` When `screenshot --selector "#x"` runs Then exit 4 with `zero_area: true`.

**VS-12 — Existing behaviour is preserved** `screenshot` with no flags still produces a viewport PNG; `-o -` still writes bytes to stdout; the default filename still gets a collision counter.
This is a regression guard on the current contract, and should be written before the feature.

**VS-13 — PDF options take effect** When `pdf --landscape --paper a4` runs Then the produced PDF's first-page MediaBox is A4 landscape — assert by parsing the PDF header, not by file size.

**VS-14 — PDF page ranges** Given a fixture spanning 4 printed pages When `pdf --pages "1-2"` runs Then the result has 2 pages.

## Test plan

**Unit — flag and value parsing (`internal/cli`, stub failing on any browser call, `t.Parallel()`)** VS-9, VS-10, plus `--paper` name table (every named size and the `WxH` form), `--margin` one-value and four-value forms, `--pages` range grammar.
The `--region`, `--margin`, `--pages`, and `--paper` parsers are all small grammars and good fuzz targets: any input either parses or is rejected, never panics.

**Unit — command boundary (`chrometest.StubBrowser`)** VS-12's regression guards, envelope field presence, extension selection from `--format`, and that `mode` reflects the chosen path.

**Live Chrome (`internal/chrome`, `testing.Short()`-guarded, not parallel)** VS-1 through VS-8, VS-11 against `data:` fixtures with known geometry and solid colour blocks.
Decode captures with `image/png` and `image/jpeg` from the standard library and assert on dimensions and sampled pixels — never on byte-for-byte equality, which is not stable across Chrome versions or platforms.
VS-3 and VS-4 sample a pixel at a computed coordinate; that is the only way to prove the right region was captured rather than an equally-sized wrong one.

**PDF structural tests** VS-13 and VS-14 parse the output enough to read page count and MediaBox.
If a lightweight PDF reader is not already an acceptable dependency, assert on the raw `/MediaBox` and `/Count` tokens in the PDF bytes — crude but dependency-free and sufficient.

**Size sanity (US-3)** Assert the JPEG-at-scale-0.5 artifact is meaningfully smaller than the PNG baseline for a photo-heavy fixture, with a generous margin so the test is not brittle across Chrome versions.

## Out of scope

- Video capture (RFC-0011 covers recording).
- Visual diffing or baseline comparison.
- Capturing a specific iframe as its own image beyond what `--pierce` addressing already reaches.
- Auto-scrolling to force lazy-loaded content before a full-page capture.

## Open questions

1. Should `--full-page` attempt to trigger lazy-loading by scrolling through the page first?
   It would make the common "capture this dashboard" case work more often, at the cost of a slow, page-mutating capture.
   **Recommendation:** no by default; add `--full-page --settle` later if real usage demands it.
2. Should `--format webp` ship in the first pass?
   Chrome supports it and it is smaller than both alternatives, but it is less universally viewable.
   **Recommendation:** include it — the cost is one enum value, and agents benefit most from the size.
3. Should element capture fail or fall back to viewport when the element is larger than the viewport?
   **Recommendation:** capture it fully using `captureBeyondViewport`, same as full-page — a tall table is exactly the thing users want captured whole.
4. Should the `Screenshot`/`PDF` signature change be split into its own preparatory commit?
   **Recommendation:** yes — a mechanical, reviewable interface-and-stub change first, then the features on top, so the diff that matters is small.
