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
