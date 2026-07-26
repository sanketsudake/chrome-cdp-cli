package chrome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// Select chooses an option in a prompt / combobox / cascade widget, or a native
// <select>. It exists because a plain `click` cannot drive Workday's
// portal-rendered menus and React cascade prompts: those open on a real pointer
// sequence, mount briefly collapsed (a zero-scale transform) then animate open,
// render options in a detached subtree with delegated (capture-phase) handlers,
// and change geometry between the open and the option click. See
// .scratch/cdp-ergonomics/e3-rootcause.md.
//
// The whole choreography runs inside ONE held CDP action (a single c.run), so
// geometry is re-read between steps against the live DOM — no multi-command
// staleness. Each interaction is a coordinate Input.dispatchMouseEvent
// (mouseMoved→pressed→released) at the element's live, occlusion-verified centre,
// which is what actually drives the delegated widgets where chromedp's node-click
// (one box-model read, no settle/occlusion check) misses.
func (c *CDP) Select(ctx context.Context, id, field, option string, opts SelectOpts) (map[string]any, error) {
	sep := opts.Sep
	if sep == "" {
		sep = ">"
	}
	steps := splitPath(option, sep)
	if len(steps) == 0 {
		return nil, fmt.Errorf("select needs an option")
	}
	optMatch := opts.OptionMatch
	if optMatch == "" {
		optMatch = "contains"
	}

	var out map[string]any
	err := c.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
		// Bring the tab to the front: Chrome drops synthetic Input.dispatchMouseEvent
		// on a background/inactive tab, so every coordinate click below would be a
		// silent no-op otherwise.
		_ = page.BringToFront().Do(actx)

		// Resolve the FIELD control by accessible name (reusing the E1/E2 matcher,
		// so --role/--match/--nth disambiguate the input from a same-named header).
		nid, node, err := resolveField(actx, field, opts.Query)
		if err != nil {
			return err
		}

		// Native <select>: set the option directly (no popup choreography). This
		// is the sub-mode for OS-rendered selects that don't drive via click.
		if strings.EqualFold(node.NodeName, "SELECT") {
			leaf := steps[len(steps)-1]
			if err := setNativeSelect(actx, nid, leaf, optMatch); err != nil {
				return err
			}
			out = map[string]any{"field": field, "selected": leaf, "widget": "native-select"}
			return nil
		}

		// Open the prompt — but only if it isn't already open. Clicking the field
		// toggles the popup, so if the first segment's options are already rendered
		// (a prompt Workday left open), a click would close it. Probe first; open
		// only when the options aren't already there.
		if probe, _ := locateOption(actx, steps[0], optMatch); !probe.Found || !probe.OK {
			fx, fy, err := nodeCenter(actx, nid)
			if err != nil {
				return fmt.Errorf("field %q is not clickable: %w", field, err)
			}
			if err := coordClick(actx, fx, fy); err != nil {
				return err
			}
			// Optional filter typing to narrow a long list before selecting.
			if opts.Filter != "" {
				if err := typeKeys(actx, opts.Filter); err != nil {
					return err
				}
			}
		}

		// Walk the cascade: click each segment's row, waiting for its options to
		// render and settle before acting. Clicking a category row drills into it;
		// clicking a leaf row selects it — Workday decides which from the node type,
		// so every level is the same centre-click (the chevron does NOT reliably
		// drill in every tenant, so we don't aim at it).
		for _, seg := range steps {
			if err := clickOption(actx, seg, optMatch); err != nil {
				return fmt.Errorf("option %q: %w", seg, err)
			}
		}

		// A button field is a MENU trigger: the final option fires an action
		// (opens a modal / navigates) rather than committing a selectable value, so
		// there's nothing to confirm — the aim-point-text check already guaranteed
		// the click landed on the right row. Only prompts/comboboxes commit a value.
		widget := "menu"
		if !strings.EqualFold(opts.Query.Role, "button") && !strings.EqualFold(node.NodeName, "BUTTON") {
			// Confirm a value actually committed. The final segment may resolve to a
			// category (drilling instead of selecting) when the cascade path is
			// incomplete — without this check select would report a false success and
			// a caller would enter data against no selection.
			if err := confirmSelection(actx, steps[len(steps)-1], optMatch); err != nil {
				return err
			}
			widget = "prompt"
		}

		out = map[string]any{"field": field, "selected": option, "widget": widget}
		return nil
	}))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// splitPath splits a cascade option path on sep, trimming each segment and
// dropping empties (so "A > B" and "A>B" both yield ["A","B"]).
func splitPath(option, sep string) []string {
	var out []string
	for p := range strings.SplitSeq(option, sep) {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// resolveField resolves the field control and returns its node id + described
// node (for tag checks). By default it addresses by accessible name (with
// role/match/nth), but honours an explicit --by (css/id/search/ref) — which also
// makes select work on a backgrounded tab where the a11y tree is throttled.
func resolveField(ctx context.Context, field string, q QueryOpts) (cdp.NodeID, *cdp.Node, error) {
	if q.By != "" && q.By != "name" {
		nid, err := resolveNodeReady(ctx, field, q)
		if err != nil {
			return 0, nil, err
		}
		node, derr := dom.DescribeNode().WithNodeID(nid).Do(ctx)
		if derr != nil {
			return 0, nil, derr
		}
		return nid, node, nil
	}
	match := axNameQuery(field, q.Role, q.Nth, q.Match)
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	for {
		ids, err := match(ctx, nil)
		if err == nil && len(ids) > 0 {
			node, derr := dom.DescribeNode().WithNodeID(ids[0]).Do(ctx)
			if derr != nil {
				return 0, nil, derr
			}
			return ids[0], node, nil
		}
		select {
		case <-ctx.Done():
			return 0, nil, fmt.Errorf("field %q not found", field)
		case <-t.C:
		}
	}
}

// nodeCenter returns the viewport centre of a node's first content quad — the
// same geometry chromedp uses to click, but we recompute it live and dispatch a
// real pointer sequence rather than relying on a one-shot node click.
func nodeCenter(ctx context.Context, nid cdp.NodeID) (float64, float64, error) {
	if err := dom.ScrollIntoViewIfNeeded().WithNodeID(nid).Do(ctx); err != nil {
		return 0, 0, err
	}
	quads, err := dom.GetContentQuads().WithNodeID(nid).Do(ctx)
	if err != nil {
		return 0, 0, err
	}
	if len(quads) == 0 || len(quads[0]) < 8 {
		return 0, 0, fmt.Errorf("no content box")
	}
	q := quads[0]
	x := (q[0] + q[2] + q[4] + q[6]) / 4
	y := (q[1] + q[3] + q[5] + q[7]) / 4
	return x, y, nil
}

// coordClick dispatches a real trusted pointer sequence at (x,y): move, press,
// release. This is what drives Workday's delegated menu/prompt handlers where a
// single chromedp node-click misses.
func coordClick(ctx context.Context, x, y float64) error {
	return coordClickN(ctx, x, y, 1)
}

// coordClickN is coordClick with an explicit click count — count 3 is a
// triple-click, which selects all text in an input (so a fill can replace, not
// append to, the existing value).
func coordClickN(ctx context.Context, x, y float64, count int64) error {
	if err := input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx); err != nil {
		return err
	}
	press := input.DispatchMouseEvent(input.MousePressed, x, y).
		WithButton(input.Left).WithClickCount(count)
	if err := press.Do(ctx); err != nil {
		return err
	}
	release := input.DispatchMouseEvent(input.MouseReleased, x, y).
		WithButton(input.Left).WithClickCount(count)
	return release.Do(ctx)
}

// typeKeys sends text as real keystrokes to the focused element (the just-opened
// prompt), for filter narrowing.
func typeKeys(ctx context.Context, text string) error {
	return chromedp.KeyEvent(text).Do(ctx)
}

// resolveNodeReady resolves a selector to one node using a PRESENT (not visible)
// wait — so it doesn't hang on a background/hidden tab whose box model isn't
// computed. It honours every --by mode (css/id/search/name/ref) via byFor.
func resolveNodeReady(ctx context.Context, selector string, q QueryOpts) (cdp.NodeID, error) {
	var nodes []*cdp.Node
	opts := append(byFor(selector, q), chromedp.NodeReady)
	if err := chromedp.Nodes(selector, &nodes, opts...).Do(ctx); err != nil {
		return 0, err
	}
	if len(nodes) == 0 {
		return 0, fmt.Errorf("selector %q not found", selector)
	}
	return nodes[0].NodeID, nil
}

// coordClickNode clicks a resolved node at its live, occlusion-verified centre via
// a coordinate pointer sequence. Unlike chromedp's node-click it computes geometry
// in JS (getBoundingClientRect / elementFromPoint work even when the tab is
// hidden) and dispatches Input events — so it lands on a background/inactive tab
// (after bringToFront) where a box-model node-click would poll until it times out.
// It waits for the centre to settle on the target (or a descendant) — an occluding
// overlay or a mid-animation element is waited out rather than mis-clicked.
func coordClickNode(ctx context.Context, nid cdp.NodeID) error {
	return coordClickNodeN(ctx, nid, 1)
}

// coordClickNodeN is coordClickNode with a click count (3 = triple-click to
// select all text in an input).
func coordClickNodeN(ctx context.Context, nid cdp.NodeID, count int64) error {
	x, y, err := settledNodePoint(ctx, nid)
	if err != nil {
		return err
	}
	return coordClickN(ctx, x, y, count)
}

// settledNodePoint waits for a node's centre to settle on the node itself (or a
// descendant) and returns that viewport point. It is the geometry half of
// coordClickNode, factored out so every pointer verb — click, hover, dblclick,
// right-click, and both ends of a drag — targets the identical, occlusion-
// verified point rather than each recomputing its own.
//
// The centre is computed in JS (getBoundingClientRect / elementFromPoint), which
// works on a hidden tab where the box model isn't laid out; an occluding overlay
// or a mid-animation element is waited out rather than mis-targeted.
func settledNodePoint(ctx context.Context, nid cdp.NodeID) (float64, float64, error) {
	obj, err := dom.ResolveNode().WithNodeID(nid).Do(ctx)
	if err != nil {
		return 0, 0, err
	}
	if obj == nil || obj.ObjectID == "" {
		return 0, 0, fmt.Errorf("node has no remote object")
	}
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	// sawOccluded records that at least one CLEAN geometry read reported the
	// centre covered. That is the diagnosis, and it must outlive whatever error
	// the final poll returns: once the deadline expires, the in-flight
	// nodeCoord fails with a context error, and reporting THAT would tell the
	// caller "protocol/timeout problem" for an element we successfully measured
	// and found under an overlay. Preferring the diagnosis is also what keeps
	// the classification stable under load rather than only on an idle machine.
	var sawOccluded bool
	var lastErr error
	for {
		x, y, ok, err := nodeCoord(ctx, obj.ObjectID)
		switch {
		case err == nil && ok:
			return x, y, nil
		case err == nil:
			sawOccluded = true
		default:
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if sawOccluded || lastErr == nil {
				return 0, 0, ErrOccluded
			}
			return 0, 0, lastErr
		case <-t.C:
		}
	}
}

// ErrOccluded reports that an element resolved but never presented an unoccluded
// centre — it is covered by an overlay, or it never stopped animating. Pointer
// verbs surface it as `occluded: true` in the error details, so a caller can tell
// "covered by an overlay" from "not found"; match it with IsOccluded rather than
// errors.Is at call sites.
var ErrOccluded = errors.New("element has no settled, unoccluded clickable centre")

// nodeCoord returns the element's clamped centre and whether that point is
// hit-testable on the element (or a descendant) — i.e. not occluded.
func nodeCoord(ctx context.Context, objID runtime.RemoteObjectID) (float64, float64, bool, error) {
	res, err := callOnObject(ctx, objID, nodeCoordJS)
	if err != nil {
		return 0, 0, false, err
	}
	var v struct {
		OK bool    `json:"ok"`
		X  float64 `json:"x"`
		Y  float64 `json:"y"`
	}
	if err := json.Unmarshal(res, &v); err != nil {
		return 0, 0, false, err
	}
	return v.X, v.Y, v.OK, nil
}

// nodeCoordJS scrolls the element into view and returns its viewport centre
// (clamped so elementFromPoint is valid) plus whether that pixel resolves to the
// element or a descendant — the occlusion check.
const nodeCoordJS = `function() {
  try { this.scrollIntoView({block:"center", inline:"nearest"}); } catch (e) {}
  const r = this.getBoundingClientRect();
  if (r.width < 1 || r.height < 1) return { ok: false, x: 0, y: 0 };
  let cx = Math.round(r.left + r.width / 2), cy = Math.round(r.top + r.height / 2);
  cx = Math.max(0, Math.min(cx, window.innerWidth - 1));
  cy = Math.max(0, Math.min(cy, window.innerHeight - 1));
  const at = document.elementFromPoint(cx, cy);
  const ok = !!at && (at === this || this.contains(at));
  return { ok, x: cx, y: cy };
}`

// locateResult is the coordinate + occlusion verdict returned by the JS option
// locator.
type locateResult struct {
	Found bool    `json:"found"`
	OK    bool    `json:"ok"` // the chosen point is hit-testable on the matching row
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

// clickOption finds a rendered option whose text matches seg and coordinate-clicks
// its row centre. It clicks only once the aim point has been hit-testable ON THE
// MATCHING ROW for two consecutive polls at a stable coordinate — so a menu still
// animating open (its rows sliding) is waited out, and the click can't land on an
// adjacent row (the "Clear" next to "Enter Time by Type" hazard).
func clickOption(ctx context.Context, seg, match string) error {
	t := time.NewTicker(120 * time.Millisecond)
	defer t.Stop()
	var prevX, prevY float64
	havePrev := false
	for {
		res, err := locateOption(ctx, seg, match)
		switch {
		case err == nil && res.Found && res.OK && havePrev && absF(res.X-prevX) <= 3 && absF(res.Y-prevY) <= 3:
			// Stable across two polls on the matching row — safe to click.
			return coordClick(ctx, res.X, res.Y)
		case err == nil && res.Found && res.OK:
			prevX, prevY, havePrev = res.X, res.Y, true
		default:
			havePrev = false
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return err
			}
			return fmt.Errorf("did not render / settle")
		case <-t.C:
		}
	}
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// locateOption evals the JS locator for one option segment.
func locateOption(ctx context.Context, seg, match string) (locateResult, error) {
	segJSON, _ := json.Marshal(seg)
	matchJSON, _ := json.Marshal(match)
	expr := fmt.Sprintf(optionLocatorJS, string(segJSON), string(matchJSON))
	var res locateResult
	if err := chromedp.Evaluate(expr, &res).Do(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// confirmSelection polls until a value has committed — a selected pill / checked
// / aria-selected element, or a field value, whose text matches the final
// segment. Errors if nothing commits, which is what a false success (the final
// segment resolving to a category rather than a leaf) looks like.
func confirmSelection(ctx context.Context, lastSeg, match string) error {
	segJSON, _ := json.Marshal(lastSeg)
	matchJSON, _ := json.Marshal(match)
	expr := fmt.Sprintf(confirmJS, string(segJSON), string(matchJSON))
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	// A shorter window than the caller's full deadline — a commit is near-instant.
	deadline := time.Now().Add(4 * time.Second)
	for {
		var ok bool
		if err := chromedp.Evaluate(expr, &ok).Do(ctx); err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("selection did not commit — the final path segment %q may be a category, not a selectable value", lastSeg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// confirmJS reports whether a value matching the final segment has been selected:
// a committed pill / checked / aria-selected element, or a form field value. Args:
// %[1]s segment JSON, %[2]s match-mode JSON.
const confirmJS = `(() => {
  const seg = %[1]s, mode = %[2]s;
  const norm = s => (s || "").replace(/\s+/g, " ").trim();
  const cmp = (a, b) => {
    a = norm(a);
    if (mode === "contains") return a.toLowerCase().includes(b.toLowerCase());
    if (mode === "regex") { try { return new RegExp(b).test(a); } catch (e) { return false; } }
    return a === b;
  };
  const committed = [...document.querySelectorAll(
    "[data-automation-id=selectedItem],[aria-selected=true],[data-automation-checked=Checked]")]
    .some(e => cmp(e.textContent, seg));
  const fielded = [...document.querySelectorAll("input,select,[role=textbox],[role=combobox]")]
    .some(e => cmp(e.value || e.textContent, seg));
  return committed || fielded;
})()`

// optionLocatorJS finds the best-matching visible, option-like row for a segment
// and reports the coordinate to click at its centre plus whether that point is
// hit-testable ON THE MATCHING ROW. Args: %[1]s want-text JSON, %[2]s match mode
// JSON.
//
// "Option-like" = ARIA role option/menuitem*/treeitem, or a Workday
// dropdown-option/promptOption/promptLeafNode, or a descendant of an open
// listbox/menu/tree popup. It prefers the deepest (most specific) match. `ok` is
// true only when elementFromPoint at the row centre resolves to an option row
// whose text still matches the segment — so a collapsed [0x0] box, an occluding
// overlay, or a mid-animation menu (where the point lands on the header or an
// adjacent row) all fail, and the caller polls until the widget settles. This is
// the safety property that stops select clicking the wrong option (the "Clear"
// next to "Enter Time by Type" hazard).
const optionLocatorJS = `(() => {
  const want = %[1]s, mode = %[2]s;
  const norm = s => (s || "").replace(/\s+/g, " ").trim();
  const cmp = (a, b) => {
    a = norm(a);
    if (mode === "contains") return a.toLowerCase().includes(b.toLowerCase());
    if (mode === "regex") { try { return new RegExp(b).test(a); } catch (e) { return false; } }
    return a === b;
  };
  const optSel = "[role=option],[role=menuitem],[role=menuitemradio],[role=menuitemcheckbox],[role=treeitem],[data-automation-id=dropdown-option],[data-automation-id=promptOption],[data-automation-id=promptLeafNode]";
  const visible = el => {
    const r = el.getBoundingClientRect();
    if (r.width < 1 || r.height < 1) return null;
    const cs = getComputedStyle(el);
    if (cs.visibility === "hidden" || cs.display === "none") return null;
    return r;
  };
  // Candidate rows: actual option rows (an optSel ancestor must exist — this
  // excludes breadcrumbs and stray labels) whose text matches.
  const all = [...document.querySelectorAll("[role],[data-automation-id],li,div,span,a")];
  let rows = [];
  for (const el of all) {
    if (!cmp(el.textContent, want)) continue;
    const row = el.closest(optSel);
    if (!row || !visible(row)) continue;
    rows.push(row);
  }
  rows = [...new Set(rows)];
  if (!rows.length) return { found: false };
  // Rank: an exact-text row first, then the shortest text — so a generic segment
  // like "Project" picks the row that IS "Project", not "Project Plan Tasks".
  const rowText = r => norm(r.textContent);
  rows.sort((a, b) => {
    const ea = rowText(a).toLowerCase() === want.toLowerCase() ? 0 : 1;
    const eb = rowText(b).toLowerCase() === want.toLowerCase() ? 0 : 1;
    return (ea - eb) || (rowText(a).length - rowText(b).length);
  });
  const row = rows[0];
  const rr = row.getBoundingClientRect();
  const cx = Math.round(rr.x + rr.width / 2), cy = Math.round(rr.y + rr.height / 2);
  const at = document.elementFromPoint(cx, cy);
  let ok = false;
  if (at) {
    const hitRow = at.closest(optSel) || ((row.contains(at) || at === row) ? row : null);
    ok = !!hitRow && cmp(hitRow.textContent, want);
  }
  return { found: true, ok, x: cx, y: cy };
})()`

// setNativeSelect sets a native <select> to the option whose visible text matches
// leaf, then fires input+change so listeners react (the G7 native-select case).
func setNativeSelect(ctx context.Context, nid cdp.NodeID, leaf, match string) error {
	obj, err := dom.ResolveNode().WithNodeID(nid).Do(ctx)
	if err != nil {
		return err
	}
	leafJSON, _ := json.Marshal(leaf)
	matchJSON, _ := json.Marshal(match)
	fn := fmt.Sprintf(`function(){
	  const want=%[1]s, mode=%[2]s;
	  const norm=s=>(s||"").replace(/\s+/g," ").trim();
	  const cmp=(a,b)=>{a=norm(a); if(mode==="contains")return a.toLowerCase().includes(b.toLowerCase()); if(mode==="regex"){try{return new RegExp(b).test(a)}catch(e){return false}} return a===b;};
	  const opt=[...this.options].find(o=>cmp(o.textContent,want)||cmp(o.value,want));
	  if(!opt) return false;
	  this.value=opt.value;
	  this.dispatchEvent(new Event("input",{bubbles:true}));
	  this.dispatchEvent(new Event("change",{bubbles:true}));
	  return true;
	}`, string(leafJSON), string(matchJSON))
	res, err := callOnObject(ctx, obj.ObjectID, fn)
	if err != nil {
		return err
	}
	var okv bool
	_ = json.Unmarshal(res, &okv)
	if !okv {
		return fmt.Errorf("option %q not found in <select>", leaf)
	}
	return nil
}

// callOnObject runs a JS function (with `this` bound to the object) via
// Runtime.callFunctionOn and returns its JSON value.
func callOnObject(ctx context.Context, objID runtime.RemoteObjectID, fn string) (json.RawMessage, error) {
	res, exc, err := runtime.CallFunctionOn(fn).
		WithObjectID(objID).WithReturnByValue(true).Do(ctx)
	if err != nil {
		return nil, err
	}
	if exc != nil {
		return nil, fmt.Errorf("callFunctionOn: %s", exc.Text)
	}
	if res == nil {
		return nil, nil
	}
	return json.RawMessage(res.Value), nil
}
