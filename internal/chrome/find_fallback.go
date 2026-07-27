package chrome

// `find`'s hidden-tab fallback.
//
// Chrome throttles accessibility-tree computation on a backgrounded tab, so a
// tree read there can come back empty for a page full of controls. This path
// enumerates candidates from the DOM instead, using the SAME accessible-name
// and role derivation `--by name`'s fallback uses (axNameHelpersJS), and ranks
// them with the same scorer.
//
// What it cannot do: mint refs (there is no a11y node behind a match) and
// honour --region (region scoping is an a11y-subtree notion). Both are
// reported rather than silently dropped — the envelope carries
// `note: "dom_fallback"`.

import (
	"context"
	"encoding/json"

	cdpruntime "github.com/chromedp/cdproto/runtime"
)

// findDOMCandidate is one element as the fallback JS reports it.
type findDOMCandidate struct {
	Role      string  `json:"role"`
	Name      string  `json:"name"`
	Value     string  `json:"value"`
	Focusable bool    `json:"focusable"`
	Disabled  bool    `json:"disabled"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	W         float64 `json:"w"`
	H         float64 `json:"h"`
}

// findDOMFallback enumerates candidate elements in JS, ranks them with the
// shared scorer, and returns matches carrying geometry but no refs.
func findDOMFallback(actx context.Context, query string, opts FindOpts) ([]findMatchNode, bool) {
	res, exc, err := cdpruntime.Evaluate(findDOMCandidatesJS).WithReturnByValue(true).Do(actx)
	if err != nil || exc != nil || res == nil || len(res.Value) == 0 {
		return nil, false
	}
	var raw []findDOMCandidate
	if json.Unmarshal([]byte(res.Value), &raw) != nil {
		return nil, false
	}
	return rankDOMCandidates(raw, query, opts)
}

// rankDOMCandidates filters and ranks decoded DOM candidates. It is pure, so
// the fallback's Go half is testable without a backgrounded tab — the one
// condition a headless test cannot arrange.
func rankDOMCandidates(raw []findDOMCandidate, query string, opts FindOpts) ([]findMatchNode, bool) {
	// --role and --dedupe are applied here, mirroring axFilterNodes, so the two
	// paths honour the same flags. (--all is moot: the JS only enumerates
	// visible elements, so there are no ignored candidates to include.)
	var cands []findCandidate
	var kept []findDOMCandidate
	seen := map[string]bool{}
	for _, r := range raw {
		if opts.Role != "" && r.Role != opts.Role {
			continue
		}
		if opts.Dedupe {
			key := r.Role + "\x00" + r.Name
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		cands = append(cands, findCandidate{
			role: r.Role, name: r.Name, value: r.Value,
			focusable: r.Focusable, disabled: r.Disabled,
		})
		kept = append(kept, r)
	}

	ranked := rankFindCandidates(parseFindQuery(query), cands, opts.MinScore)
	idx := make([]int, len(ranked))
	for i, r := range ranked {
		idx[i] = r.Index
	}
	idx, truncated := limitFindMatches(idx, opts.Limit)

	out := make([]findMatchNode, 0, len(idx))
	for pos, i := range idx {
		r := kept[i]
		var states []string
		if r.Disabled {
			states = append(states, "disabled")
		}
		if r.Focusable {
			states = append(states, "focusable")
		}
		out = append(out, findMatchNode{
			role: r.Role, name: r.Name, value: r.Value,
			states:   states,
			score:    ranked[pos].Score,
			geometry: &findGeometry{X: r.X, Y: r.Y, W: r.W, H: r.H},
		})
	}
	return out, truncated
}

// findDOMCandidatesJS enumerates visible, name-bearing elements using the
// shared accessible-name helpers, capped so a pathological page cannot flood
// the RPC.
//
// Focusability and disabled state are read from the element rather than
// assumed: the scorer boosts focusable candidates and penalizes disabled ones,
// so hardcoding either would rank this path's results differently from the
// a11y path's for the same page.
const findDOMCandidatesJS = `(() => {
  const CAP = 2000;` + axNameHelpersJS + `
  const FOCUSABLE_ROLES = ["button", "link", "textbox", "searchbox", "combobox",
    "checkbox", "radio", "tab", "menuitem", "option", "slider", "switch"];
  const focusableOf = el => {
    if (el.hasAttribute("tabindex")) return Number(el.getAttribute("tabindex")) >= 0;
    if (el.disabled) return false;
    return FOCUSABLE_ROLES.includes(roleOf(el));
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
    out.push({role: role, name: name, value: axValueOf(el),
              focusable: focusableOf(el),
              disabled: el.disabled === true || el.getAttribute("aria-disabled") === "true",
              x: r.x + r.width / 2, y: r.y + r.height / 2, w: r.width, h: r.height});
  }
  return out;
})()`
