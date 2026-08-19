package chrome

// The `screenshot --annotate` predicate and node pass (RFC-0016): which nodes
// get a numbered label, and how their geometry is measured and mapped onto the
// captured image. Drawing itself lives in internal/encode; this file decides
// WHICH nodes and WHERE, over the same a11y-tree fetch `snap` and `find` use.

import (
	"context"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
)

// maxAnnotations bounds the label pass: a legibility and latency bound (two
// CDP calls per candidate, so 200 candidates is ~400 round trips — well under
// a second on a local socket — and more labels than that are not readable on
// one image anyway).
const maxAnnotations = 200

// annotateTimeout bounds the WHOLE pass (tree read plus every measurement).
// The a11y tree is throttled on a backgrounded tab, and the capture has
// already succeeded by the time this runs — a --annotate that blocks the
// whole command would turn "give me the picture" into "give me the picture,
// eventually, maybe." Mirrors the precedent key.go's focusedReadTimeout sets
// for its own best-effort accessibility read.
const annotateTimeout = 3 * time.Second

// annotateRoles are the roles a person clicks, types into, or toggles — the
// things --annotate numbers outright, regardless of the focusable property.
var annotateRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true,
	"combobox": true, "checkbox": true, "radio": true, "switch": true,
	"slider": true, "spinbutton": true, "tab": true, "menuitem": true,
	"menuitemcheckbox": true, "menuitemradio": true, "option": true, "treeitem": true,
}

// annotateStructuralRoles are containers Chrome may mark focusable — a
// keyboard-focusable scroller, the document root — that are never the thing to
// act on. A focusable node in one of these roles does NOT qualify through rule
// 2 of annotateActionable.
//
// cell and gridcell are deliberately absent from both lists: a data grid would
// otherwise consume the whole cap in cells, which already have first-class
// addressing (--by cell). A focusable gridcell (an editable Workday cell, say)
// still qualifies through rule 2, because it is not in this set.
var annotateStructuralRoles = map[string]bool{
	"generic": true, "none": true, "presentation": true, "RootWebArea": true,
	"WebArea": true, "document": true, "application": true, "region": true,
	"main": true, "group": true, "list": true, "table": true, "grid": true,
}

// annotateActionable reports whether a node is something --annotate should
// number: not ignored, backed by a real DOM node, and either a role a person
// acts on directly or a focusable node outside the structural-container roles.
//
// The "not ignored" / has-a-backend-node checks are defense-in-depth: every
// candidate reaching this from Screenshot has already passed
// axFilterNodes(IncludeIgnored: false), which drops both. They matter for a
// direct caller — this predicate's own table test — that skips that step.
func annotateActionable(n *accessibility.Node) bool {
	if n == nil || n.Ignored || axRef(n) == "" {
		return false
	}
	role := axString(n.Role)
	if annotateRoles[role] {
		return true
	}
	return axHasState(n, "focusable") && !annotateStructuralRoles[role]
}

// annotateCandidates returns the actionable nodes in tree (document) order —
// the same order snap prints, over the same axFilterNodes selection (all
// nodes, IncludeIgnored: false, exactly what find uses) filtered further by
// annotateActionable.
func annotateCandidates(nodes []*accessibility.Node) []*accessibility.Node {
	kept := axFilterNodes(nodes, axFilter{IncludeIgnored: false})
	out := make([]*accessibility.Node, 0, len(kept))
	for _, n := range kept {
		if annotateActionable(n) {
			out = append(out, n)
		}
	}
	return out
}

// annotateDegradeReason picks the RFC-0016 degrade reason for a pass that
// completed with no labels: ok reports whether collectAnnotateLabels ran to
// completion (false on a tree-read error or an expired pass), hidden reports
// tabHidden(actx).
//
// It is a pure function, deliberately separated from the tabHidden probe
// itself, for the same reason find's DOM-fallback ranking was pulled out into
// a pure, directly-testable function (see rankDOMCandidates / find_test.go):
// live-triggering actual accessibility-tree throttling on a backgrounded tab
// is not reliable across Chrome builds and test harnesses, but the BRANCHING
// this function does is exactly, and cheaply, table-testable on its own.
func annotateDegradeReason(ok, hidden bool) string {
	switch {
	case hidden:
		return "tab_hidden"
	case !ok:
		return "tree_unavailable"
	default:
		return "no_actionable_nodes"
	}
}

// annotateLabel is one numbered candidate that survived measurement and the
// clip drop, ready to become both a drawn label and a legend entry.
type annotateLabel struct {
	n      int
	ref    string
	role   string
	name   string
	states []string

	// cssX, cssY are CSS pixels relative to the CAPTURE CLIP's top-left — the
	// encode.Label / Frame.CSSWidth convention drawMarks already uses.
	cssX, cssY float64

	// centerX, centerY are the element's viewport CSS-pixel centre — the
	// find/--at contract, reported verbatim in the envelope's `center`.
	centerX, centerY float64
	occluded         bool
}

// collectAnnotateLabels reads the accessibility tree, measures every
// actionable node's geometry (find's non-scrolling primitive), and maps each
// surviving one onto the captured image via the RFC-0016 clip formula.
//
// pctx is expected to be a bounded child of the action context (annotateTimeout
// in Screenshot); ok is false when the tree read failed or the pass could not
// run to completion before pctx expired — the caller (Screenshot) is
// responsible for turning that into a degrade rather than treating a partial
// legend as complete.
func collectAnnotateLabels(pctx context.Context, clip Rect, imgW, imgH int) (labels []annotateLabel, truncated, ok bool) {
	nodes, err := accessibility.GetFullAXTree().Do(pctx)
	if err != nil {
		return nil, false, false
	}
	cands := annotateCandidates(nodes)
	truncated = len(cands) > maxAnnotations
	if truncated {
		cands = cands[:maxAnnotations]
	}

	// Re-read the visual viewport origin AT MEASUREMENT TIME: the element mode
	// may have scrolled the page since the capture's own layout read.
	metrics, err := layoutRects(pctx)
	if err != nil {
		return nil, false, false
	}

	n := 0
	for _, node := range cands {
		if pctx.Err() != nil {
			// The pass ran out of time partway through. Returning what was
			// measured so far would look like a complete, small legend rather
			// than what it is — a pass that did not finish — so the whole
			// result is discarded in favour of a degrade.
			return nil, false, false
		}
		obj, err := dom.ResolveNode().WithBackendNodeID(cdp.BackendNodeID(node.BackendDOMNodeID)).Do(pctx)
		if err != nil || obj == nil || obj.ObjectID == "" {
			continue // detached between the tree read and now: drop, not fatal
		}
		box, err := measureNode(pctx, obj.ObjectID, nodeBoxJS)
		if err != nil || box.Detached || box.W < 1 || box.H < 1 {
			continue
		}
		pageX, pageY := box.CX+metrics.viewport.X, box.CY+metrics.viewport.Y
		imgX := (pageX - clip.X) * float64(imgW) / clip.Width
		imgY := (pageY - clip.Y) * float64(imgH) / clip.Height
		if imgX < 0 || imgX >= float64(imgW) || imgY < 0 || imgY >= float64(imgH) {
			continue // outside the captured clip (US-3)
		}
		n++
		labels = append(labels, annotateLabel{
			n: n, ref: axRef(node), role: axString(node.Role), name: axString(node.Name),
			states:   axStates(node),
			cssX:     pageX - clip.X,
			cssY:     pageY - clip.Y,
			centerX:  box.CX,
			centerY:  box.CY,
			occluded: box.Occluded,
		})
	}
	return labels, truncated, true
}
