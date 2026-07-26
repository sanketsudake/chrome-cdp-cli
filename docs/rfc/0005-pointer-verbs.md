# RFC-0005: Pointer verbs — `hover`, `dblclick`, `rclick`, `drag`

- **Status:** Draft
- **Priority:** P1
- **Area:** input
- **Depends on:** —
- **Related:** RFC-0001 (completes the input surface together)

## Summary

Add four pointer verbs alongside `click`: `hover` (move without pressing), `dblclick` (double-click), `rclick` (right-click / context menu), and `drag` (press, move, release between two points).
All four reuse the existing `QueryOpts` addressing and the occlusion-verified centre-point resolution `click` already performs, and all gain a `--modifiers` flag that `click` also acquires.

## Motivation

`click` is the only pointer verb, which leaves common interactions with no expressible form:

- **Hover-revealed UI.**
  Row action menus, tooltips, nav flyouts, and chart tooltips only render on `mouseover`.
  Clicking where the button *will* appear fails, because it is not there yet.
- **Double-click to edit.**
  Data grids and spreadsheet-like UIs — precisely the applications `--by cell` and `grid` were built for — commonly enter edit mode on double-click.
- **Context menus.**
  Right-click is the only path to some actions in file managers, tree views, and internal admin tools.
- **Drag.**
  Reordering lists, kanban cards, sliders, range pickers, and resizable panes are all drag-only.
- **Modified clicks.**
  `ctrl+click` / `cmd+click` for multi-select in a list, and `shift+click` for range-select, are standard in table UIs and unreachable today.

`eval` is not a workaround: synthetic `MouseEvent` dispatch is untrusted input, and most frameworks that matter here either ignore it or behave differently for it.
Real CDP `Input.dispatchMouseEvent` is what makes these work, and that is exactly what `click` already uses.

Together with RFC-0001 this closes the input surface: after both, every ordinary human interaction with a page has a verb.

## User stories

**US-1 — Reveal a hover-only action.**
As an automation author, I want to hover a table row so that its action buttons render and become clickable.
*Acceptance:* `chrome-cdp hover --by name "Invoice 4102"` then `click --by name "Delete" --in-row "Invoice 4102"` succeeds, where the click alone fails.

**US-2 — Edit a grid cell.**
As an automation author, I want to double-click a cell so that the grid enters edit mode and accepts typing.
*Acceptance:* `chrome-cdp dblclick --by cell "Mon, 7/13"` puts the cell into an editable state, confirmed by `snap` reporting a focused textbox.

**US-3 — Open a context menu.**
As an automation author, I want to right-click an item so that I can choose an action only available from its context menu.
*Acceptance:* `chrome-cdp rclick --by name "report.pdf"` makes the menu appear in `snap`, and a following `click` selects an item.

**US-4 — Reorder by dragging.**
As an automation author, I want to drag one element onto another so that I can reorder a list or move a card between columns.
*Acceptance:* `chrome-cdp drag --by name "Task A" --to-by name --to "Done"` moves the item, confirmed by `snap`.

**US-5 — Set a slider.**
As an automation author, I want to drag by a pixel delta so that I can move a slider that has no text input.
*Acceptance:* `chrome-cdp drag "#slider" --dx 120` changes the reported value.

**US-6 — Multi-select rows.**
As an automation author, I want a modified click so that I can select several rows before invoking a bulk action.
*Acceptance:* `chrome-cdp click --by name "Row 2" --modifiers cmd` adds to the selection rather than replacing it.

**US-7 — Hover that persists long enough to observe.**
As an automation author, I want the hover state to still be active when my next command runs so that the tooltip has not vanished before I read it.
*Acceptance:* documented behaviour — the pointer stays where it was left; the next `snap` in the same `session` sees the revealed nodes.

## Proposed CLI surface

```
chrome-cdp hover   <selector>
chrome-cdp dblclick <selector>
chrome-cdp rclick  <selector>
chrome-cdp drag    <selector> (--to <selector> | --dx <px> --dy <px>)
```

Shared flags — all `QueryOpts` flags (`--by`, `--wait`, `--role`, `--nth`, `--match`, `--in-row`, `--pierce`), plus `--on-dialog` and `--wait-text` where an action can raise a dialog or need confirmation.

New flags:

| Flag | Applies to | Purpose |
|------|-----------|---------|
| `--modifiers <list>` | `click`, `dblclick`, `rclick` | `+`-joined `ctrl`/`shift`/`alt`/`cmd` held during the press |
| `--to <selector>` | `drag` | drop target |
| `--to-by <mode>` | `drag` | `--by` mode for the drop target (defaults to `--by`) |
| `--dx`, `--dy <px>` | `drag` | pixel delta form; mutually exclusive with `--to` |
| `--steps <n>` | `drag` | intermediate move events, default `10` |
| `--hold <dur>` | `drag` | pause after press before moving, for long-press-to-drag UIs |

Examples:

```sh
chrome-cdp hover --by name "Invoice 4102"
chrome-cdp dblclick --by cell "Mon, 7/13"
chrome-cdp rclick --by name "report.pdf" --role link
chrome-cdp drag --by name "Task A" --to "Done" --to-by name
chrome-cdp drag "#volume" --dx 80 --steps 20
chrome-cdp click --by name "Row 2" --modifiers cmd
```

## Result envelope

```json
{ "ok": true, "command": "drag",
  "target": {"id":"…","title":"…","url":"…"},
  "result": { "from": {"x": 412, "y": 260, "name": "Task A"},
              "to":   {"x": 903, "y": 260, "name": "Done"},
              "steps": 10, "modifiers": [] },
  "elapsed_ms": 148 }
```

`hover`, `dblclick`, and `rclick` report `{"x":…, "y":…, "name":…, "modifiers":[…]}` and their action name, matching `click`'s existing shape so a caller parses one thing.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| Unknown modifier name; both `--to` and `--dx/--dy`; neither given for `drag`; `--steps` out of range | `usage` | 2 |
| Source or drop-target selector never resolves | `target_timeout` | 4 |
| Ambiguous `--by name` match | `ambiguous_target` | 4 |
| Element resolves but is fully occluded at its centre | `target_timeout` with `occluded: true` in details | 4 |
| `Input.dispatchMouseEvent` rejected | `cdp_error` | 5 |

## Design notes

- **One interface method, not four.**
  Extend `chrome.Browser` with: ```go Pointer(ctx context.Context, targetID, selector string, opts PointerOpts) (map[string]any, error) ``` where `PointerOpts{Action string; Modifiers int64; To string; ToQuery QueryOpts; Dx, Dy float64; Steps int; Hold time.Duration; Query QueryOpts}` and `Action ∈ {hover, dblclick, rclick, drag}`.
  Four CLI verbs, one driver method, one stub default — this is the shape that keeps `StubBrowser` from growing four near-identical entries.
- **Reuse the existing centre resolution.**
  `click` already computes an occlusion-verified centre point; that code must be factored out and shared rather than reimplemented, or the four new verbs will drift from `click`'s behaviour on overlapped elements.
- **`click --modifiers`** is a change to an existing verb.
  It is additive (default empty) and does not alter the envelope when unset, so it does not break the contract.
- **Drag event sequence:** `mouseMoved` to source → `mousePressed` → optional hold → *n* interpolated `mouseMoved` events → `mouseMoved` to target → `mouseReleased`.
  The intermediate moves are not cosmetic: HTML5 drag-and-drop libraries and most JS drag implementations require movement events to register a drag at all, and a press-then-release at two points is silently a click.
- **Native HTML5 drag-and-drop** (`draggable="true"` + `dragstart`/`drop`) may not respond to synthesized mouse movement in all cases.
  If live testing shows a fixture failing, add a documented `--html5` mode that dispatches the `DragEvent` sequence with a `DataTransfer` instead.
  Decide this from test evidence, not up front — see Open Questions.
- **Hover has no natural completion signal.**
  It dispatches a move and returns; whether the app rendered something is the caller's problem, which is what `--wait-text` and `wait --visible` are for.
  The verb must not silently sleep.
- **`tab_hidden` inheritance:** the a11y-backed addressing modes (`name`, `ref`, `cell`) carry the same backgrounded-tab throttling caveat as elsewhere; RFC-0007's `activate` verb is the mitigation and should be cross-referenced in the docs for these verbs.
- **Stub:** `Pointer` returns `{"action": opts.Action, "x": 0, "y": 0}`.

## Verification scenarios

**VS-1 — Hover fires mouseover** Given a fixture recording `mouseover` targets into `window.__hovered` When `hover "#target"` runs Then `eval "window.__hovered"` contains the target's id.

**VS-2 — Hover reveals a hidden control** Given a fixture where a button has `display:none` until its row is hovered When `hover` the row then `click` the button Then the click succeeds; and given the same fixture, the click alone fails with `target_timeout`.

**VS-3 — Double-click is one dblclick, not two clicks** Given a fixture counting `click` and `dblclick` events separately When `dblclick "#cell"` runs Then `dblclick` count is 1 and `detail` on the final click event is 2.

**VS-4 — Right-click fires contextmenu** Given a fixture recording `contextmenu` When `rclick "#item"` runs Then the event fired with `button == 2`.

**VS-5 — Modified click reports modifiers to the page** Given a fixture recording `metaKey`/`ctrlKey`/`shiftKey` on click When `click "#row" --modifiers cmd+shift` runs Then both `metaKey` and `shiftKey` are true.

**VS-6 — Drag emits a full sequence** Given a fixture logging `mousedown`, every `mousemove`, and `mouseup` When `drag "#a" --to "#b" --steps 5` runs Then the log shows one mousedown, at least 5 moves, one mouseup, in that order, with the final position at `#b`'s centre.

**VS-7 — Drag by delta** Given a range input at value 0 When `drag "#slider" --dx 100` runs Then the input's value increased.

**VS-8 — Drag reorders a real sortable list** Given a fixture using a common drag implementation with two items When the first is dragged onto the second Then the DOM order is swapped.

**VS-9 — Mutually exclusive drag targeting** Table: `--to` only → ok; `--dx` only → ok; both → `usage` exit 2; neither → `usage` exit 2.
No browser call in the failing cases.

**VS-10 — Bad modifier never connects** When `click "#x" --modifiers command` runs (not a valid name) Then exit 2, `usage`, no browser method called.

**VS-11 — Occlusion is reported, not silently mis-clicked** Given an element fully covered by an overlay When `dblclick` targets it Then exit 4 with `occluded: true` in `error.details`.

**VS-12 — Drop target resolution honours `--to-by`** Given a drop target addressable only by accessible name When `drag "#a" --to "Done" --to-by name` runs Then it resolves and the drop lands there.

**VS-13 — Works inside `session`, hover state persists** Given a `session` of `["hover","--by","name","Row 1"]` then `["snap"]` Then the snapshot includes the hover-revealed node.

## Test plan

**Unit — flag validation (`internal/cli`, stub failing on any browser call, `t.Parallel()`)** VS-9, VS-10, `--steps` bounds, `--hold` parsing, and that `--to-by` defaults to `--by`.

**Unit — command boundary (`chrometest.StubBrowser`)** Each verb maps to `Pointer` with the right `Action`; envelope `command` name matches the verb; `--wait-text` composes; `--modifiers` parses to the expected bitmask (table-driven over every combination — this is pure arithmetic and worth exhausting).

**Live Chrome (`internal/chrome`, `testing.Short()`-guarded, not parallel)** VS-1 through VS-8, VS-11, VS-12 against `data:` fixtures that record events into `window.__log`, read back with `eval`.
VS-8 needs a fixture with a real drag implementation rather than a bare event log — a small self-contained sortable in the fixture page, not a vendored library.

**Shared-centre-resolution regression** A test asserting `click` and `dblclick` resolve the same coordinates for the same element under the same `QueryOpts`.
This is the test that catches the factored-out centre logic drifting.

**Session integration (`internal/cli`)** VS-13 through the existing `session` test harness.

## Out of scope

- Touch and multi-touch gestures (pinch, swipe).
- Pointer events with pressure, tilt, or non-mouse pointer types.
- Drag between two different tabs or out of the browser.
- Drag-and-drop of files from the host filesystem — that is RFC-0006's territory.

## Open questions

1. Should `drag` auto-detect native HTML5 drag-and-drop and switch strategies, or require an explicit `--html5` flag?
   Auto-detection is friendlier but unpredictable when it guesses wrong.
   **Recommendation:** ship mouse-event dragging only, add `--html5` if and only if a live fixture proves a real case that mouse events cannot drive.
2. Should `hover` support a `--hold <dur>` to keep the pointer in place for delayed tooltips?
   The pointer stays put anyway, but a hold makes the intent explicit and gives slow tooltips time without a separate `wait --for`.
   **Recommendation:** yes, cheap and self-documenting.
3. Should `rclick` optionally auto-dismiss the context menu on failure so a wedged menu does not break the next command?
   **Recommendation:** no implicit behaviour; document `key Escape` (RFC-0001) as the cleanup, which is why these two RFCs pair.
4. Should `dblclick` be spelled `double-click` or aliased?
   **Recommendation:** `dblclick` primary, matching the DOM event name users already know, with no alias.
