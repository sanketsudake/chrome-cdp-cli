package chrome

// Live-Chrome tests for the pointer verbs (RFC-0005 VS-1..VS-8, VS-11, VS-12).
// They drive a managed headless Chrome against fixture pages that record the
// events they receive into window.__log, then read the log back with Eval — the
// only way to prove that what reached the page is real, trusted input in the
// right order. Skipped under -short, and never parallel: they share a browser.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// clickVia dispatches the `click` verb the way the CLI does — through the one
// Pointer method that backs every pointer gesture. There is no Browser.Click:
// keeping click on the same driver method as hover/dblclick/rclick/drag is what
// gives it --modifiers without a second implementation to keep in step.
func clickVia(ctx context.Context, b *CDP, id, selector string, q QueryOpts) (map[string]any, error) {
	return b.Pointer(ctx, id, selector, PointerOpts{Action: PointerClick, Query: q})
}

// pointerFixture serves one HTML page for the length of a test.
func pointerFixture(t *testing.T, html string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, html)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// pointerEval evaluates expr in the tab and returns its JSON value.
func pointerEval(ctx context.Context, t *testing.T, b *CDP, id, expr string) any {
	t.Helper()
	v, err := b.Eval(ctx, id, expr, EvalOpts{})
	if err != nil {
		t.Fatalf("eval %s: %v", expr, err)
	}
	return v.(map[string]any)["value"]
}

// pointerLog reads window.__log back as a list of event records.
func pointerLog(ctx context.Context, t *testing.T, b *CDP, id string) []map[string]any {
	t.Helper()
	items, _ := pointerEval(ctx, t, b, id, "window.__log").([]any)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// pointerClearLog empties the fixture's event log between phases.
func pointerClearLog(ctx context.Context, t *testing.T, b *CDP, id string) {
	t.Helper()
	pointerEval(ctx, t, b, id, "window.__log.length = 0")
}

// pointerCentre returns the viewport centre of a fixture element, as the page
// itself computes it.
func pointerCentre(ctx context.Context, t *testing.T, b *CDP, id, sel string) (float64, float64) {
	t.Helper()
	v := pointerEval(ctx, t, b, id, fmt.Sprintf(
		`(() => { const r = document.querySelector(%q).getBoundingClientRect();
		  return [Math.round(r.left + r.width/2), Math.round(r.top + r.height/2)]; })()`, sel))
	pair, ok := v.([]any)
	if !ok || len(pair) != 2 {
		t.Fatalf("centre of %s = %v, want a [x,y] pair", sel, v)
	}
	return pair[0].(float64), pair[1].(float64)
}

// pointerCountType counts logged events of one type.
func pointerCountType(log []map[string]any, typ string) int {
	n := 0
	for _, e := range log {
		if e["type"] == typ {
			n++
		}
	}
	return n
}

// pointerLastOfType returns the last logged event of one type.
func pointerLastOfType(log []map[string]any, typ string) map[string]any {
	var found map[string]any
	for _, e := range log {
		if e["type"] == typ {
			found = e
		}
	}
	return found
}

// The envelope reports the modifiers that were held, in the same spellings the
// CLI accepts — so a mask never round-trips into a name a caller would be
// rejected for passing back. Pure arithmetic: no browser.
func TestPointerModifierNames(t *testing.T) {
	t.Parallel()
	cases := map[int64][]string{
		0:             {},
		2:             {"ctrl"},
		1 | 8:         {"alt", "shift"},
		4 | 8:         {"shift", "cmd"},
		1 | 2 | 4 | 8: {"ctrl", "alt", "shift", "cmd"},
	}
	for mask, want := range cases {
		got := modifierNames(mask)
		if len(got) != len(want) {
			t.Errorf("modifierNames(%d) = %v, want %v", mask, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("modifierNames(%d) = %v, want %v", mask, got, want)
				break
			}
		}
	}
}

const pointerEventsFixture = `<!doctype html><title>Pointer</title>
<style>
  body { margin: 0; font: 14px sans-serif; }
  #row  { position:absolute; left:20px;  top:20px;  width:300px; height:40px; background:#eee; }
  #del  { display:none; }
  #row:hover #del { display:inline-block; }
  #cell { position:absolute; left:20px;  top:120px; width:160px; height:40px; background:#cfc; }
  #item { position:absolute; left:20px;  top:200px; width:160px; height:40px; background:#ccf; }
  #cover{ position:fixed; inset:0; z-index:9999; display:none; background:rgba(0,0,0,.1); }
</style>
<body>
<div id="row">Row 1 <button id="del" aria-label="Delete" onclick="window.__deleted=true">Delete</button></div>
<div id="cell">Cell</div>
<div id="item">Item</div>
<div id="cover"></div>
<script>
window.__log = [];
const rec = e => window.__log.push({type:e.type, id:e.target.id, detail:e.detail, button:e.button,
  alt:e.altKey, ctrl:e.ctrlKey, meta:e.metaKey, shift:e.shiftKey});
for (const t of ["mouseover","mousedown","mouseup","click","dblclick","contextmenu"])
  document.addEventListener(t, rec, true);
document.addEventListener("contextmenu", e => e.preventDefault());
</script>
</body>`

// hover / dblclick / rclick against a page that records what it received:
// VS-1 (hover fires mouseover), VS-2 (hover reveals a hidden control),
// VS-3 (one dblclick with detail 2, not two clicks), VS-4 (contextmenu with
// button 2), VS-5 (modifiers reach the page), VS-11 (occlusion is reported).
func TestPointerVerbsDriveRealEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := pointerFixture(t, pointerEventsFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// VS-2, first half: the row's action button is display:none until the row is
	// hovered, so clicking it without hovering must fail rather than land
	// somewhere else. Bounded so it fails fast.
	noHoverCtx, noHoverCancel := context.WithTimeout(ctx, 3*time.Second)
	defer noHoverCancel()
	if _, err := clickVia(noHoverCtx, b, id, "#del", QueryOpts{}); err == nil {
		t.Error("clicking a hover-only button without hovering succeeded, want a failure")
	}

	// VS-1: hover dispatches a real mouseover at the element.
	pointerClearLog(ctx, t, b, id)
	if _, err := b.Pointer(ctx, id, "#row", PointerOpts{Action: PointerHover}); err != nil {
		t.Fatalf("hover: %v", err)
	}
	if e := pointerLastOfType(pointerLog(ctx, t, b, id), "mouseover"); e == nil || e["id"] != "row" {
		t.Errorf("mouseover = %v, want one on #row", e)
	}

	// VS-2, second half: with the row hovered, the revealed button is clickable.
	if _, err := clickVia(ctx, b, id, "#del", QueryOpts{}); err != nil {
		t.Fatalf("click on the hover-revealed button: %v", err)
	}
	if got := pointerEval(ctx, t, b, id, "window.__deleted"); got != true {
		t.Errorf("window.__deleted = %v, want true (the revealed button was clicked)", got)
	}

	// VS-3: a double-click is one dblclick whose click reports detail 2 — not two
	// independent clicks, which would fire no dblclick at all.
	pointerClearLog(ctx, t, b, id)
	res, err := b.Pointer(ctx, id, "#cell", PointerOpts{Action: PointerDblClick})
	if err != nil {
		t.Fatalf("dblclick: %v", err)
	}
	log := pointerLog(ctx, t, b, id)
	if n := pointerCountType(log, "dblclick"); n != 1 {
		t.Errorf("dblclick count = %d, want 1 (log: %v)", n, log)
	}
	if e := pointerLastOfType(log, "click"); e == nil || e["detail"] != 2.0 {
		t.Errorf("final click event = %v, want detail 2", e)
	}
	if res["action"] != "dblclick" || res["name"] != "#cell" {
		t.Errorf("dblclick result = %v, want action dblclick on #cell", res)
	}

	// VS-5: modifiers are set on the dispatched events, so the page's handlers
	// see event.metaKey / event.shiftKey — this is what makes a modified click
	// multi-select instead of replacing the selection.
	pointerClearLog(ctx, t, b, id)
	if _, err := b.Pointer(ctx, id, "#cell", PointerOpts{Action: PointerDblClick, Modifiers: 4 | 8}); err != nil {
		t.Fatalf("dblclick with modifiers: %v", err)
	}
	e := pointerLastOfType(pointerLog(ctx, t, b, id), "mousedown")
	if e == nil || e["meta"] != true || e["shift"] != true {
		t.Errorf("mousedown modifiers = %v, want meta and shift true", e)
	}
	if e["ctrl"] == true || e["alt"] == true {
		t.Errorf("mousedown = %v, want ctrl/alt unset", e)
	}

	// RFC-0005 US-6 / VS-5 for `click` itself — the case the whole flag exists
	// for: cmd-clicking a row must reach the page with metaKey set, or the app
	// replaces the selection instead of adding to it. Asserted on the `click`
	// event (detail 1: still a single click, not a double).
	pointerClearLog(ctx, t, b, id)
	res, err = b.Pointer(ctx, id, "#cell", PointerOpts{Action: PointerClick, Modifiers: 4})
	if err != nil {
		t.Fatalf("click with modifiers: %v", err)
	}
	if res["clicked"] != "#cell" {
		t.Errorf("click result = %v, want {clicked: #cell} — click's payload is public API", res)
	}
	e = pointerLastOfType(pointerLog(ctx, t, b, id), "click")
	switch {
	case e == nil:
		t.Error("a modified click fired no click event")
	case e["meta"] != true:
		t.Errorf("click event = %v, want metaKey true (--modifiers cmd)", e)
	case e["detail"] != 1.0:
		t.Errorf("click event detail = %v, want 1 — a modified click is still one click", e["detail"])
	case e["shift"] == true || e["ctrl"] == true || e["alt"] == true:
		t.Errorf("click event = %v, want only meta held", e)
	}

	// VS-4: right-click raises contextmenu with the right button.
	pointerClearLog(ctx, t, b, id)
	if _, err := b.Pointer(ctx, id, "#item", PointerOpts{Action: PointerRClick}); err != nil {
		t.Fatalf("rclick: %v", err)
	}
	e = pointerLastOfType(pointerLog(ctx, t, b, id), "contextmenu")
	if e == nil || e["id"] != "item" || e["button"] != 2.0 {
		t.Errorf("contextmenu = %v, want one on #item with button 2", e)
	}

	// VS-11: an element covered by a full-viewport overlay has no unoccluded
	// centre; that must be reported, not mis-clicked through the overlay.
	pointerEval(ctx, t, b, id, `document.getElementById("cover").style.display = "block"`)
	occCtx, occCancel := context.WithTimeout(ctx, 3*time.Second)
	defer occCancel()
	_, err = b.Pointer(occCtx, id, "#cell", PointerOpts{Action: PointerDblClick})
	if err == nil {
		t.Fatal("dblclick on a fully-occluded element succeeded, want an occlusion failure")
	}
	if !IsOccluded(err) {
		t.Errorf("dblclick on an occluded element: err = %v, want one IsOccluded recognises", err)
	}
}

const pointerDragFixture = `<!doctype html><title>Drag</title>
<style>
  body { margin: 0; font: 14px sans-serif; }
  #a { position:absolute; left:20px;  top:20px;  width:100px; height:60px; background:#fcc; }
  #b { position:absolute; left:400px; top:20px;  width:100px; height:60px; background:#cfc; }
  #slider { position:absolute; left:20px; top:120px; width:300px; }
  #src { position:absolute; left:20px;  top:200px; width:140px; height:50px; }
  #dst { position:absolute; left:400px; top:200px; width:140px; height:50px; }
  #list { position:absolute; left:20px; top:300px; margin:0; padding:0; list-style:none; width:200px; }
  #list li { height:40px; background:#eee; border-bottom:1px solid #999; }
</style>
<body>
<div id="a">A</div>
<div id="b">B</div>
<input type="range" id="slider" min="0" max="100" value="0">
<button id="src" aria-label="Task A">Task A</button>
<button id="dst" aria-label="Done">Done</button>
<ul id="list"><li id="i1">One</li><li id="i2">Two</li></ul>
<script>
window.__log = [];
const rec = t => e => window.__log.push({type:t, x:e.clientX, y:e.clientY});
document.addEventListener("mousedown", rec("down"), true);
document.addEventListener("mousemove", rec("move"), true);
document.addEventListener("mouseup",   rec("up"),   true);
document.getElementById("dst").addEventListener("mouseup", () => { window.__dropped = true; });
// A minimal sortable — no library, just the behaviour every drag implementation
// shares: it reorders on pointer MOVEMENT, so a press-then-release at two points
// does nothing at all.
let dragEl = null;
document.getElementById("list").addEventListener("mousedown", e => { dragEl = e.target.closest("li"); });
document.addEventListener("mousemove", e => {
  if (!dragEl) return;
  const at = document.elementFromPoint(e.clientX, e.clientY);
  const over = at && at.closest ? at.closest("li") : null;
  if (!over || over === dragEl) return;
  const r = over.getBoundingClientRect();
  if (e.clientY >= r.top + r.height / 2) over.after(dragEl); else over.before(dragEl);
});
document.addEventListener("mouseup", () => { dragEl = null; });
</script>
</body>`

// drag against a page that logs the whole pointer stream: VS-6 (full sequence),
// VS-7 (pixel delta moves a slider), VS-8 (a real sortable reorders),
// VS-12 (--to-by addresses the drop target by accessible name).
func TestPointerDragDrivesRealSequence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := pointerFixture(t, pointerDragFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// VS-6: one press, at least --steps moves, one release — in that order, with
	// the release at the drop target's centre.
	pointerClearLog(ctx, t, b, id)
	res, err := b.Pointer(ctx, id, "#a", PointerOpts{Action: PointerDrag, To: "#b", Steps: 5})
	if err != nil {
		t.Fatalf("drag: %v", err)
	}
	log := pointerLog(ctx, t, b, id)
	if n := pointerCountType(log, "down"); n != 1 {
		t.Errorf("mousedown count = %d, want 1", n)
	}
	if n := pointerCountType(log, "up"); n != 1 {
		t.Errorf("mouseup count = %d, want 1", n)
	}
	downAt, upAt, moves := -1, -1, 0
	for i, e := range log {
		switch e["type"] {
		case "down":
			downAt = i
		case "up":
			upAt = i
		case "move":
			if downAt >= 0 && upAt < 0 {
				moves++
			}
		}
	}
	if downAt < 0 || upAt < downAt {
		t.Fatalf("event order = %v, want a mousedown then a mouseup", log)
	}
	if moves < 5 {
		t.Errorf("moves between press and release = %d, want at least the 5 requested steps", moves)
	}
	bx, by := pointerCentre(ctx, t, b, id, "#b")
	up := pointerLastOfType(log, "up")
	if up["x"] != bx || up["y"] != by {
		t.Errorf("release at (%v,%v), want #b's centre (%v,%v)", up["x"], up["y"], bx, by)
	}
	to := res["to"].(map[string]any)
	if to["x"] != bx || to["y"] != by || to["name"] != "#b" {
		t.Errorf("result.to = %v, want #b's centre (%v,%v)", to, bx, by)
	}
	if res["steps"] != 5 {
		t.Errorf("result.steps = %v, want 5", res["steps"])
	}

	// VS-7: a pixel-delta drag moves a slider that has no text input.
	before := pointerEval(ctx, t, b, id, "Number(document.getElementById('slider').value)")
	if _, err := b.Pointer(ctx, id, "#slider", PointerOpts{Action: PointerDrag, Dx: 100, Steps: 10}); err != nil {
		t.Fatalf("drag by delta: %v", err)
	}
	after := pointerEval(ctx, t, b, id, "Number(document.getElementById('slider').value)")
	if after.(float64) <= before.(float64) {
		t.Errorf("slider value %v -> %v, want it to increase", before, after)
	}

	// VS-8: a real (if minimal) sortable reorders — this is the case a press and
	// a release with no movement between them silently fails.
	if _, err := b.Pointer(ctx, id, "#i1", PointerOpts{Action: PointerDrag, To: "#i2", Steps: 8}); err != nil {
		t.Fatalf("drag to reorder: %v", err)
	}
	order := pointerEval(ctx, t, b, id, "[...document.querySelectorAll('#list li')].map(e => e.id)")
	got, _ := order.([]any)
	if len(got) != 2 || got[0] != "i2" || got[1] != "i1" {
		t.Errorf("list order = %v, want [i2 i1] (the drag reordered it)", order)
	}

	// VS-12: the drop target is addressed by accessible name via ToQuery.
	if _, err := b.Pointer(ctx, id, "Task A", PointerOpts{
		Action:  PointerDrag,
		To:      "Done",
		Query:   QueryOpts{By: "name"},
		ToQuery: QueryOpts{By: "name"},
		Steps:   5,
	}); err != nil {
		t.Fatalf("drag with name-addressed endpoints: %v", err)
	}
	if got := pointerEval(ctx, t, b, id, "window.__dropped"); got != true {
		t.Errorf("window.__dropped = %v, want true (the release landed on the named drop target)", got)
	}
}

const pointerGeometryFixture = `<!doctype html><title>Geometry</title>
<style>body { margin:0 }
  #target { position:absolute; left:137px; top:211px; width:83px; height:37px; background:#ccc; }
</style>
<body>
<div id="target">T</div>
<script>
window.__pts = [];
document.addEventListener("mousedown", e => window.__pts.push([e.clientX, e.clientY]), true);
</script>
</body>`

// The regression guard for the factored-out centre resolution: click and the
// pointer verbs must aim at the IDENTICAL point for the same element under the
// same QueryOpts. If a pointer verb ever recomputes geometry of its own, this is
// the test that catches the drift — including the hidden-tab case, where a
// box-model read returns different numbers than settledNodePoint's JS.
func TestPointerSharesClickGeometry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := pointerFixture(t, pointerGeometryFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	q := QueryOpts{}
	if _, err := clickVia(ctx, b, id, "#target", q); err != nil {
		t.Fatalf("Click: %v", err)
	}
	res, err := b.Pointer(ctx, id, "#target", PointerOpts{Action: PointerDblClick, Query: q})
	if err != nil {
		t.Fatalf("dblclick: %v", err)
	}

	pts, _ := pointerEval(ctx, t, b, id, "window.__pts").([]any)
	if len(pts) < 2 {
		t.Fatalf("recorded points = %v, want at least the click's and the dblclick's", pts)
	}
	clickPt := pts[0].([]any)
	dblPt := pts[1].([]any)
	if clickPt[0] != dblPt[0] || clickPt[1] != dblPt[1] {
		t.Errorf("click aimed at (%v,%v) but dblclick at (%v,%v) — the shared centre resolution has drifted",
			clickPt[0], clickPt[1], dblPt[0], dblPt[1])
	}
	if res["x"] != dblPt[0] || res["y"] != dblPt[1] {
		t.Errorf("reported point (%v,%v) is not where the page was hit (%v,%v)",
			res["x"], res["y"], dblPt[0], dblPt[1])
	}
}

// A resolved-but-covered element must be reported as ErrOccluded even when the
// command's deadline expires while a geometry read is in flight.
//
// This is a regression test for a CI failure that only appeared on a loaded
// machine: settledNodePoint kept just the LAST error, so the context error from
// the final poll masked the occlusion it had already observed, and the verb
// reported a bare timeout for an element it had successfully measured and found
// under an overlay. The classification has to survive load, not merely hold on
// an idle box — a caller that sees "timeout" rewrites its selector, where
// "occluded" tells it to dismiss the overlay.
func TestOccludedSurvivesDeadlineDuringGeometryRead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Occluded</title><body>
<button id="under" style="position:absolute;left:50px;top:50px;width:100px;height:40px">Under</button>
<div id="over" style="position:absolute;left:0;top:0;width:400px;height:300px;background:#000"></div>
</body>`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// A deadline short enough that it lands mid-poll, which is exactly the
	// window that used to produce a context error instead of the diagnosis.
	for _, d := range []time.Duration{250 * time.Millisecond, 1 * time.Second} {
		actx, acancel := context.WithTimeout(ctx, d)
		_, err := b.Pointer(actx, id, "#under", PointerOpts{Action: PointerDblClick})
		acancel()
		if err == nil {
			t.Fatalf("deadline %s: click on a covered element succeeded, want an error", d)
		}
		if !IsOccluded(err) {
			t.Errorf("deadline %s: err = %v, want one IsOccluded recognises", d, err)
		}
	}
}

// The occluded error carries its evidence in the MESSAGE (errors cross the daemon
// as strings), while still matching IsOccluded — so both the human and the agent
// learn what sat on top without re-running under instrumentation.
func TestOccludedErrorMessage(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err  error
		want string
	}{
		"no box": {
			err:  &OccludedError{},
			want: "element has no settled, unoccluded clickable centre; it measured 0x0 (no box to click)",
		},
		"covered by an overlay": {
			err:  &OccludedError{CoveredBy: map[string]any{"tag": "DIV", "id": "", "name": "modalOverlay"}},
			want: `element has no settled, unoccluded clickable centre; its centre is covered by DIV name="modalOverlay"`,
		},
		"covered by an identified element": {
			err:  &OccludedError{CoveredBy: map[string]any{"tag": "INPUT", "id": "q", "role": "textbox"}},
			want: `element has no settled, unoccluded clickable centre; its centre is covered by INPUT#q role="textbox"`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
			if !IsOccluded(tc.err) {
				t.Errorf("IsOccluded(%v) = false, want true", tc.err)
			}
			// And after a trip through the daemon, where only the string survives.
			if !IsOccluded(errors.New(tc.err.Error())) {
				t.Errorf("IsOccluded(string-only copy) = false, want true")
			}
		})
	}
}
