package chrome

// The `find` verb (RFC-0015): ranked element search from a plain-language
// query. One a11y-tree fetch, an in-process scoring pass (find_score.go), and
// a bounded center-point enrichment for the returned matches — no
// per-candidate round trips.
//
// Tree acquisition, ref minting, and the --region/--dedupe semantics
// deliberately mirror Snapshot's: the refs must be interchangeable between
// `find` and `snap`, and two traversals would drift.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	cdpruntime "github.com/chromedp/cdproto/runtime"
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

// findMatchNode is one ranked match as the traversal produces it, before the
// driver enriches it with geometry and shapes the result maps.
type findMatchNode struct {
	ref     string
	backend int64
	role    string
	name    string
	value   string
	states  []string
	score   float64
	ignored bool

	// center/size, filled by the enrichment pass; hasCenter guards them.
	hasCenter bool
	cx, cy    float64
	w, h      float64
}

// buildFindMatches filters and ranks a full a11y tree against a query. It is
// pure — document order in, score order out — so the whole pipeline around the
// scorer is testable without a renderer.
func buildFindMatches(nodes []*accessibility.Node, query string, opts FindOpts) ([]findMatchNode, bool) {
	fq := parseFindQuery(query)

	byID := make(map[accessibility.NodeID]*accessibility.Node, len(nodes))
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	var inRegion map[accessibility.NodeID]bool
	if opts.Region != "" {
		inRegion = map[accessibility.NodeID]bool{}
		if rn := findRegion(nodes, opts.Region); rn != nil {
			markSubtree(byID, rn, inRegion)
		}
	}

	var cands []findCandidate
	var kept []*accessibility.Node
	seen := map[string]bool{}
	for _, n := range nodes {
		role, name := axString(n.Role), axString(n.Name)
		if role == "" && name == "" {
			continue
		}
		if n.Ignored && !opts.All {
			continue
		}
		if opts.Role != "" && role != opts.Role {
			continue
		}
		if inRegion != nil && !inRegion[n.NodeID] {
			continue
		}
		if opts.Dedupe {
			key := role + "\x00" + name
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		cands = append(cands, findCandidate{
			role:      role,
			name:      name,
			value:     axString(n.Value),
			ignored:   n.Ignored,
			disabled:  axHasState(n, "disabled"),
			focusable: axHasState(n, "focusable"),
		})
		kept = append(kept, n)
	}

	ranked, scores := rankFindCandidates(fq, cands, opts.MinScore)

	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultFindLimit
	}
	truncated := len(ranked) > limit
	if truncated {
		ranked = ranked[:limit]
	}

	out := make([]findMatchNode, 0, len(ranked))
	for _, i := range ranked {
		n := kept[i]
		m := findMatchNode{
			role:    cands[i].role,
			name:    cands[i].name,
			value:   cands[i].value,
			states:  axStates(n),
			score:   scores[i],
			ignored: n.Ignored,
		}
		if n.BackendDOMNodeID != 0 {
			m.backend = int64(n.BackendDOMNodeID)
			m.ref = fmt.Sprintf("e%d", n.BackendDOMNodeID)
		}
		out = append(out, m)
	}
	return out, truncated
}

// Find ranks the page's accessibility nodes against a plain-language query and
// returns the best matches with refs, states, and centre points.
func (c *CDP) Find(ctx context.Context, id, query string, opts FindOpts) (map[string]any, error) {
	var matches []findMatchNode
	var truncated, fallback bool
	err := c.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
		nodes, err := accessibility.GetFullAXTree().Do(actx)
		if err != nil {
			return err
		}
		matches, truncated = buildFindMatches(nodes, query, opts)
		if len(matches) == 0 && tabHidden(actx) {
			// The a11y tree yielded nothing on a hidden tab — Chrome throttles
			// tree computation there. Fall back to the DOM accessible-name path
			// (same family as --by name's fallback): scored the same way, but
			// without refs, and --region cannot be honoured.
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

	arr := make([]any, 0, len(matches))
	for _, m := range matches {
		entry := map[string]any{
			"role":  m.role,
			"name":  m.name,
			"score": math.Round(m.score*1000) / 1000,
		}
		if m.ref != "" {
			entry["ref"] = m.ref
		}
		if m.value != "" {
			entry["value"] = m.value
		}
		if len(m.states) > 0 {
			entry["states"] = m.states
		}
		visible := !m.ignored
		if m.hasCenter {
			entry["center"] = map[string]any{"x": math.Round(m.cx), "y": math.Round(m.cy)}
			visible = visible && m.w > 0 && m.h > 0
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
	return res, nil
}

// enrichFindCenters fills each match's centre point and box size from the live
// DOM. Best-effort by design: a node that no longer resolves (the document
// moved on) simply keeps no center — enrichment must degrade to omitted, never
// block the result.
func enrichFindCenters(actx context.Context, matches []findMatchNode) {
	const centerJS = `function() {
	  const r = this.getBoundingClientRect();
	  return {x: r.x + r.width / 2, y: r.y + r.height / 2, w: r.width, h: r.height};
	}`
	for i := range matches {
		if matches[i].backend == 0 {
			continue
		}
		obj, err := dom.ResolveNode().WithBackendNodeID(cdp.BackendNodeID(matches[i].backend)).Do(actx)
		if err != nil || obj == nil || obj.ObjectID == "" {
			continue
		}
		res, exc, err := cdpruntime.CallFunctionOn(centerJS).
			WithObjectID(obj.ObjectID).
			WithReturnByValue(true).
			Do(actx)
		if err != nil || exc != nil || res == nil || len(res.Value) == 0 {
			continue
		}
		var box struct{ X, Y, W, H float64 }
		if json.Unmarshal([]byte(res.Value), &box) != nil {
			continue
		}
		matches[i].hasCenter = true
		matches[i].cx, matches[i].cy = box.X, box.Y
		matches[i].w, matches[i].h = box.W, box.H
	}
}

// findDOMFallback enumerates candidate elements in JS — computed accessible
// name, derived role, geometry — and ranks them with the same scorer. It runs
// only when the a11y tree came back empty on a hidden tab; the matches carry
// centres but no refs (there is no a11y node to mint one from).
func findDOMFallback(actx context.Context, query string, opts FindOpts) ([]findMatchNode, bool) {
	res, exc, err := cdpruntime.Evaluate(findDOMCandidatesJS).WithReturnByValue(true).Do(actx)
	if err != nil || exc != nil || res == nil || len(res.Value) == 0 {
		return nil, false
	}
	var raw []struct {
		Role  string  `json:"role"`
		Name  string  `json:"name"`
		Value string  `json:"value"`
		X     float64 `json:"x"`
		Y     float64 `json:"y"`
		W     float64 `json:"w"`
		H     float64 `json:"h"`
	}
	if json.Unmarshal([]byte(res.Value), &raw) != nil {
		return nil, false
	}
	fq := parseFindQuery(query)
	cands := make([]findCandidate, len(raw))
	for i, r := range raw {
		cands[i] = findCandidate{role: r.Role, name: r.Name, value: r.Value, focusable: true}
	}
	ranked, scores := rankFindCandidates(fq, cands, opts.MinScore)

	var filtered []int
	for _, i := range ranked {
		if opts.Role != "" && raw[i].Role != opts.Role {
			continue
		}
		filtered = append(filtered, i)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultFindLimit
	}
	truncated := len(filtered) > limit
	if truncated {
		filtered = filtered[:limit]
	}
	out := make([]findMatchNode, 0, len(filtered))
	for _, i := range filtered {
		out = append(out, findMatchNode{
			role: raw[i].Role, name: raw[i].Name, value: raw[i].Value,
			score:     scores[i],
			hasCenter: true, cx: raw[i].X, cy: raw[i].Y, w: raw[i].W, h: raw[i].H,
		})
	}
	return out, truncated
}

// findDOMCandidatesJS enumerates visible, name-bearing elements with the same
// simplified accessible-name and role derivation domNameLocatorJS uses, capped
// so a pathological page cannot flood the RPC.
const findDOMCandidatesJS = `(() => {
  const CAP = 2000;
  const norm = s => (s || "").replace(/\s+/g, " ").trim();
  const visible = el => {
    if (el.getAttribute("aria-hidden") === "true") return false;
    const cs = getComputedStyle(el);
    if (cs.visibility === "hidden" || cs.display === "none") return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const roleOf = el => {
    const ex = el.getAttribute("role"); if (ex) return ex;
    const tag = el.tagName.toLowerCase();
    if (tag === "button") return "button";
    if (tag === "a" && el.hasAttribute("href")) return "link";
    if (tag === "select") return "combobox";
    if (tag === "textarea") return "textbox";
    if (/^h[1-6]$/.test(tag)) return "heading";
    if (tag === "input") {
      const ty = (el.getAttribute("type") || "text").toLowerCase();
      if (["button", "submit", "reset"].includes(ty)) return "button";
      if (ty === "checkbox") return "checkbox";
      if (ty === "radio") return "radio";
      return "textbox";
    }
    return "";
  };
  const textRoles = ["button", "link", "heading", "option", "menuitem", "menuitemradio", "menuitemcheckbox", "tab", "treeitem", "cell", "columnheader", "rowheader"];
  const accName = el => {
    const al = el.getAttribute("aria-label"); if (al) return al;
    const lb = el.getAttribute("aria-labelledby");
    if (lb) {
      const t = lb.split(/\s+/).map(id => { const e = document.getElementById(id); return e ? e.textContent : ""; }).join(" ");
      if (norm(t)) return t;
    }
    if (el.id) { try { const lab = document.querySelector('label[for="' + CSS.escape(el.id) + '"]'); if (lab && norm(lab.textContent)) return lab.textContent; } catch (e) {} }
    const wrap = el.closest("label"); if (wrap && norm(wrap.textContent)) return wrap.textContent;
    if (textRoles.includes(roleOf(el)) && norm(el.textContent)) return el.textContent;
    const ph = el.getAttribute("placeholder"); if (ph) return ph;
    const ti = el.getAttribute("title"); if (ti) return ti;
    const alt = el.getAttribute("alt"); if (alt) return alt;
    return "";
  };
  const out = [];
  for (const el of document.querySelectorAll("*")) {
    if (out.length >= CAP) break;
    const role = roleOf(el);
    if (!role) continue;
    if (!visible(el)) continue;
    const name = norm(accName(el));
    if (!name) continue;
    const r = el.getBoundingClientRect();
    out.push({role: role, name: name, value: norm(el.value || ""),
              x: r.x + r.width / 2, y: r.y + r.height / 2, w: r.width, h: r.height});
  }
  return out;
})()`
