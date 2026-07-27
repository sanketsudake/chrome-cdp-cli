package chrome

// The one element-geometry primitive, shared by the verbs that need a point on
// an element: the pointer verbs (which click it) and the reading verbs (which
// report it).
//
// There is one definition because a coordinate is a contract between verbs — a
// caller reads a centre from `find` and expects a click there to land on the
// same element. Two implementations of "where is this element" would let those
// two answers drift apart, and RFC-0014's `--at` addressing makes that contract
// explicit.
//
// The one thing that CANNOT be shared is scrolling. A pointer verb scrolls its
// target into view because it is about to act on it; a read verb must not, or
// reading a page would move it under a running automation. That is the only
// difference between the two variants below.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/chromedp/cdproto/runtime"
)

// nodeBox is an element's geometry as measured in the page.
type nodeBox struct {
	// X, Y are the clamped, hit-testable aim point — the pixel a pointer verb
	// dispatches at, kept inside the viewport so elementFromPoint is valid.
	X float64 `json:"x"`
	Y float64 `json:"y"`
	// CX, CY are the element's TRUE viewport centre, unclamped. A reader wants
	// this one: for an element scrolled out of view it says where the element
	// actually is rather than where the probe had to look.
	CX float64 `json:"cx"`
	CY float64 `json:"cy"`

	W float64 `json:"w"`
	H float64 `json:"h"`

	// OK means the box is real AND the aim point resolves to this element or a
	// descendant — what a pointer verb requires before dispatching.
	OK bool `json:"ok"`
	// Occluded means the aim point resolved to something else (an overlay, a
	// covering panel). Reported rather than fatal for reads.
	Occluded bool `json:"occluded"`
}

// axBoxCoreJS measures `this` and hit-tests its centre. It is a statement list
// spliced into a function body, so both variants below share every line of the
// measurement and differ only in what happens before it.
const axBoxCoreJS = `
  const r = this.getBoundingClientRect();
  if (r.width < 1 || r.height < 1) {
    return { ok: false, x: 0, y: 0, cx: 0, cy: 0, w: r.width, h: r.height, occluded: false };
  }
  const tx = r.left + r.width / 2, ty = r.top + r.height / 2;
  const cx = Math.max(0, Math.min(Math.round(tx), window.innerWidth - 1));
  const cy = Math.max(0, Math.min(Math.round(ty), window.innerHeight - 1));
  const at = document.elementFromPoint(cx, cy);
  const hit = !!at && (at === this || this.contains(at));
  return { ok: hit, x: cx, y: cy, cx: tx, cy: ty, w: r.width, h: r.height, occluded: !hit };
`

// nodeCoordJS scrolls the element into view, then measures — the pointer path.
const nodeCoordJS = `function() {
  try { this.scrollIntoView({block:"center", inline:"nearest"}); } catch (e) {}` +
	axBoxCoreJS + `}`

// nodeBoxJS measures without scrolling — the reading path.
const nodeBoxJS = `function() {` + axBoxCoreJS + `}`

// measureNode runs one of the two variants against a resolved remote object.
func measureNode(ctx context.Context, objID runtime.RemoteObjectID, fnJS string) (nodeBox, error) {
	var b nodeBox
	raw, err := callOnObject(ctx, objID, fnJS)
	if err != nil {
		return b, err
	}
	if len(raw) == 0 {
		return b, nil
	}
	return b, json.Unmarshal(raw, &b)
}

// pointProbe answers everything a coordinate gesture needs to know before it
// dispatches, in ONE round trip: how big the viewport is (to reject a point
// outside it) and what sits under the point (the evidence a caller gets
// instead of an occlusion check).
//
// It lives here, beside the element-geometry primitive, because both answer
// "where is this, in the page's coordinate space" — the contract this file
// exists to keep single-valued.
type pointProbe struct {
	VW  float64        `json:"vw"`
	VH  float64        `json:"vh"`
	Hit map[string]any `json:"hit"`
}

// probePoint measures the viewport and, when wantHit is set, describes the
// topmost element at p.
//
// The hit description is observability, never a precondition: a canvas app's
// every coordinate resolves to the same <canvas>, which is exactly the case
// coordinate addressing exists to serve. Skipping it when the caller has
// nowhere to report it (a drag) saves the page-side walk entirely.
func probePoint(ctx context.Context, p Point, wantHit bool) (pointProbe, error) {
	expr := fmt.Sprintf(`(() => {
  const want = %t, px = %g, py = %g;
  const out = {vw: window.innerWidth, vh: window.innerHeight, hit: null};
  if (!want) return out;
  const el = document.elementFromPoint(px, py);
  if (!el) return out;`+axNameHelpersJS+`
  out.hit = {tag: el.tagName, id: el.id || undefined,
             role: roleOf(el) || undefined, name: norm(accName(el)) || undefined};
  return out;
})()`, wantHit, p.X, p.Y)

	var out pointProbe
	res, exc, err := runtime.Evaluate(expr).WithReturnByValue(true).Do(ctx)
	if err != nil {
		return out, err
	}
	if exc != nil {
		return out, fmt.Errorf("coordinate probe: %s", exc.Text)
	}
	if res == nil || len(res.Value) == 0 {
		return out, errors.New("coordinate probe returned no value")
	}
	return out, json.Unmarshal([]byte(res.Value), &out)
}
