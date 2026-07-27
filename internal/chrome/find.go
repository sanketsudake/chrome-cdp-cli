package chrome

// The `find` verb (RFC-0015): ranked element search from a plain-language
// query. One a11y-tree fetch, an in-process scoring pass (find_score.go), and
// a geometry pass over the RETURNED matches only — never per candidate.
//
// Node selection goes through the shared axFilterNodes/axRef helpers `snap`
// uses, so the two verbs cannot disagree about which nodes exist or what ref
// each one carries: a caller reads with one and acts on the other.
//
// The hidden-tab fallback lives in find_fallback.go.

import (
	"context"
	"math"
	"strconv"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

// Find limits: the CLI validates --limit against these before connecting; the
// driver re-applies the default only for a zero value (a direct caller or the
// deliberately lenient daemon arg decoder).
const (
	DefaultFindLimit = 10
	MaxFindLimit     = 50
)

// FindOpts controls the `find` verb.
type FindOpts struct {
	Role     string  // hard-filter matches to one ARIA role
	Limit    int     // maximum matches returned (default DefaultFindLimit)
	Region   string  // scope to the subtree of a container whose name contains this
	All      bool    // include ignored/hidden nodes (excluded by default)
	Dedupe   bool    // collapse identical role+name matches (keep first)
	MinScore float64 // drop matches scoring below this (0..1)
}

// findGeometry is a match's box in viewport CSS pixels. It is a pointer on
// findMatchNode so "not measured" is a nil check rather than a bool a caller
// can forget to consult.
type findGeometry struct {
	X, Y float64 // centre
	W, H float64

	// Occluded records that the centre pixel resolves to something else — an
	// overlay, a cookie banner, a modal. Reported, never fatal: knowing a
	// coordinate would miss is the point of reporting it.
	Occluded bool
}

// findMatchNode is one ranked match. `backend` is the CDP backend node id; the
// `e<id>` ref is derived from it at envelope time rather than stored twice.
type findMatchNode struct {
	backend  int64
	role     string
	name     string
	value    string
	states   []string
	score    float64
	ignored  bool
	geometry *findGeometry
}

// ref is the stable element ref this match reports, empty when the match has
// no backing DOM node (the DOM fallback, which resolves no a11y nodes).
func (m findMatchNode) ref() string {
	if m.backend == 0 {
		return ""
	}
	return "e" + strconv.FormatInt(m.backend, 10)
}

// limitFindMatches truncates a ranked index list to limit, reporting whether
// anything was dropped. One definition, used by both the a11y and the fallback
// pipeline, so the default-limit policy cannot diverge between them.
func limitFindMatches(ranked []int, limit int) ([]int, bool) {
	if limit <= 0 {
		limit = DefaultFindLimit
	}
	if len(ranked) > limit {
		return ranked[:limit], true
	}
	return ranked, false
}

// buildFindMatches filters and ranks a full a11y tree against a query. It is
// pure — document order in, score order out — so the whole pipeline around the
// scorer is testable without a renderer.
func buildFindMatches(nodes []*accessibility.Node, query string, opts FindOpts) ([]findMatchNode, bool) {
	kept := axFilterNodes(nodes, axFilter{
		Role:   opts.Role,
		Region: opts.Region,
		Dedupe: opts.Dedupe,
		// `find` ranks what a user could act on, so ignored (hidden) nodes are
		// out unless --all asks for them.
		IncludeIgnored: opts.All,
	})

	cands := make([]findCandidate, len(kept))
	for i, n := range kept {
		cands[i] = findCandidate{
			role:      axString(n.Role),
			name:      axString(n.Name),
			value:     axString(n.Value),
			ignored:   n.Ignored,
			disabled:  axHasState(n, "disabled"),
			focusable: axHasState(n, "focusable"),
		}
	}

	ranked := rankFindCandidates(parseFindQuery(query), cands, opts.MinScore)
	idx := make([]int, len(ranked))
	for i, r := range ranked {
		idx[i] = r.Index
	}
	idx, truncated := limitFindMatches(idx, opts.Limit)

	out := make([]findMatchNode, 0, len(idx))
	for pos, i := range idx {
		n := kept[i]
		out = append(out, findMatchNode{
			backend: int64(n.BackendDOMNodeID),
			role:    cands[i].role,
			name:    cands[i].name,
			value:   cands[i].value,
			states:  axStates(n),
			score:   ranked[pos].Score,
			ignored: n.Ignored,
		})
	}
	return out, truncated
}

// Find ranks the page's accessibility nodes against a plain-language query and
// returns the best matches with refs, states, and centre points.
func (c *CDP) Find(ctx context.Context, id, query string, opts FindOpts) (map[string]any, error) {
	var matches []findMatchNode
	var truncated, fallback, regionFound bool
	err := c.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
		nodes, err := accessibility.GetFullAXTree().Do(actx)
		if err != nil {
			return err
		}
		regionFound = opts.Region == "" || findRegion(nodes, opts.Region) != nil
		matches, truncated = buildFindMatches(nodes, query, opts)
		if len(matches) == 0 && tabHidden(actx) {
			// The a11y tree yielded nothing on a hidden tab — Chrome throttles
			// tree computation there. Fall back to the DOM accessible-name
			// path, the same family `--by name` falls back to.
			matches, truncated = findDOMFallback(actx, query, opts)
			fallback = len(matches) > 0
			return nil
		}
		enrichFindCenters(actx, matches)
		return nil
	}))
	if err != nil {
		return nil, err
	}
	return shapeFindResult(query, matches, truncated, fallback, opts.Region != "" && !regionFound), nil
}

// shapeFindResult renders ranked matches as the public envelope. It is pure
// and separate from the traversal on purpose: the JSON shape is API, so it is
// worth being able to reason about (and test) without a browser.
func shapeFindResult(query string, matches []findMatchNode, truncated, fallback, regionMissing bool) map[string]any {
	arr := make([]any, 0, len(matches))
	for _, m := range matches {
		entry := map[string]any{
			"role":  m.role,
			"name":  m.name,
			"score": math.Round(m.score*1000) / 1000,
		}
		if r := m.ref(); r != "" {
			entry["ref"] = r
		}
		if m.value != "" {
			entry["value"] = m.value
		}
		if len(m.states) > 0 {
			entry["states"] = m.states
		}
		visible := !m.ignored
		if g := m.geometry; g != nil {
			entry["center"] = map[string]any{"x": math.Round(g.X), "y": math.Round(g.Y)}
			visible = visible && g.W > 0 && g.H > 0
			if g.Occluded {
				entry["occluded"] = true
			}
		}
		entry["visible"] = visible
		arr = append(arr, entry)
	}
	res := map[string]any{
		"query":     query,
		"matches":   arr,
		"count":     len(arr),
		"truncated": truncated,
	}
	if fallback {
		res["note"] = "dom_fallback"
	}
	// A --region that names no container on the page yields zero matches, the
	// same as `snap`. Reporting it is what keeps that from looking identical to
	// "the region is there and holds nothing" — an agent needs to tell a typo
	// from an empty container.
	if regionMissing {
		res["region_found"] = false
	}
	return res
}

// enrichFindCenters measures each RETURNED match — two CDP calls per match,
// bounded by --limit (10 by default, 50 at most), never per candidate.
//
// It uses the SAME primitive the pointer verbs measure with (geometry.go), in
// its non-scrolling variant: a read verb must not scroll the page under a
// running automation, but it must agree with the pointer verbs about where an
// element is, or a centre reported by `find` would not be a point a click
// lands on.
//
// Best-effort by design: a node that no longer resolves (the document moved
// on) simply keeps no geometry rather than failing the read.
func enrichFindCenters(actx context.Context, matches []findMatchNode) {
	for i := range matches {
		if matches[i].backend == 0 {
			continue
		}
		obj, err := dom.ResolveNode().WithBackendNodeID(cdp.BackendNodeID(matches[i].backend)).Do(actx)
		if err != nil || obj == nil || obj.ObjectID == "" {
			continue
		}
		box, err := measureNode(actx, obj.ObjectID, nodeBoxJS)
		if err != nil {
			continue
		}
		matches[i].geometry = &findGeometry{
			X: box.CX, Y: box.CY, W: box.W, H: box.H, Occluded: box.Occluded,
		}
	}
}
