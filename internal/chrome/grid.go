package chrome

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// Grid reads a table/grid as structured {headers, rows} from the accessibility
// tree — replacing hand-parsing the a11y snapshot (or a screenshot) for the
// calendar / task-list / timesheet grids. selector optionally picks the grid by
// ARIA accessible name (with q); empty selects the first grid/table in the tree.
func (c *CDP) Grid(ctx context.Context, id, selector string, q QueryOpts) (any, error) {
	var nodes []*accessibility.Node
	err := c.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
		var e error
		nodes, e = accessibility.GetFullAXTree().Do(actx)
		return e
	}))
	if err != nil {
		return nil, err
	}
	byID := make(map[accessibility.NodeID]*accessibility.Node, len(nodes))
	for _, n := range nodes {
		byID[n.NodeID] = n
	}

	grid := pickGrid(nodes, selector, q)
	if grid == nil {
		if selector != "" {
			return nil, fmt.Errorf("no grid/table matching %q", selector)
		}
		return nil, fmt.Errorf("no grid/table found in the accessibility tree")
	}

	headers, rows := extractGrid(byID, grid)
	return map[string]any{"headers": headers, "rows": rows, "count": len(rows)}, nil
}

// gridRoles are the ARIA roles that name a tabular container.
func isGridRole(role string) bool {
	switch role {
	case "table", "grid", "treegrid", "LayoutTable":
		return true
	}
	return false
}

func isRowRole(role string) bool { return role == "row" || role == "LayoutTableRow" }

func isCellRole(role string) bool {
	switch role {
	case "cell", "gridcell", "columnheader", "rowheader", "LayoutTableCell":
		return true
	}
	return false
}

// pickGrid returns the target grid node: the first exposed grid whose accessible
// name matches selector (per q.Match), or simply the first grid when selector is
// empty.
func pickGrid(nodes []*accessibility.Node, selector string, q QueryOpts) *accessibility.Node {
	for _, n := range nodes {
		if n.Ignored || !isGridRole(axString(n.Role)) {
			continue
		}
		if selector == "" {
			return n
		}
		if nameMatches(axString(n.Name), selector, q.Match) {
			return n
		}
	}
	return nil
}

// extractGrid walks a grid's rows and cells. The first row whose cells are all
// column headers becomes headers; every other non-empty row becomes a data row.
// Cell text falls back to the joined descendant text when a cell has no name of
// its own (Workday cells nest their text in StaticText children).
func extractGrid(byID map[accessibility.NodeID]*accessibility.Node, grid *accessibility.Node) ([]string, [][]string) {
	var headers []string
	var rows [][]string

	var rowNodes []*accessibility.Node
	var collectRows func(id accessibility.NodeID)
	collectRows = func(id accessibility.NodeID) {
		n := byID[id]
		if n == nil {
			return
		}
		if isRowRole(axString(n.Role)) {
			rowNodes = append(rowNodes, n)
			return // rows don't nest rows in a flat grid; stop descending this branch
		}
		for _, ch := range n.ChildIDs {
			collectRows(ch)
		}
	}
	for _, ch := range grid.ChildIDs {
		collectRows(ch)
	}

	for _, row := range rowNodes {
		var cells []string
		colHeaders := 0
		var walkCells func(id accessibility.NodeID)
		walkCells = func(id accessibility.NodeID) {
			n := byID[id]
			if n == nil {
				return
			}
			role := axString(n.Role)
			if isCellRole(role) {
				if role == "columnheader" {
					colHeaders++
				}
				cells = append(cells, cellText(byID, n))
				return
			}
			for _, ch := range n.ChildIDs {
				walkCells(ch)
			}
		}
		for _, ch := range row.ChildIDs {
			walkCells(ch)
		}
		if len(cells) == 0 {
			continue
		}
		// The header row is the first row that is mostly column headers — real
		// grids interleave a leading action cell (e.g. Workday's "Add Row") with
		// the column headers, so require a majority rather than all.
		if headers == nil && colHeaders*2 > len(cells) {
			headers = cells
			continue
		}
		rows = append(rows, cells)
	}
	return headers, rows
}

// cellText returns a cell's accessible name, or the joined text of its
// descendants when it has none of its own.
func cellText(byID map[accessibility.NodeID]*accessibility.Node, cell *accessibility.Node) string {
	return axSubtreeText(byID, cell)
}

// Scroll scrolls a selector into view (Into), dispatches a real mouse wheel
// (Wheel), or — by default — scrolls by (Dx, Dy) via JS scrollBy on the selector's
// scroll box (or the window when selector is empty).
func (c *CDP) Scroll(ctx context.Context, id, selector string, opts ScrollOpts) (map[string]any, error) {
	if opts.Into {
		if selector == "" {
			return nil, fmt.Errorf("scroll --to needs a selector")
		}
		err := c.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
			var nodes []*cdp.Node
			if err := chromedp.Nodes(selector, &nodes, byFor(selector, opts.Query)...).Do(actx); err != nil {
				return err
			}
			if len(nodes) == 0 {
				return fmt.Errorf("selector %q not found", selector)
			}
			return dom.ScrollIntoViewIfNeeded().WithNodeID(nodes[0].NodeID).Do(actx)
		}))
		if err != nil {
			return nil, err
		}
		return map[string]any{"scrolled": "into:" + selector}, nil
	}

	if opts.Wheel {
		err := c.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
			x, y, err := wheelOrigin(actx, selector, opts)
			if err != nil {
				return err
			}
			// Position the pointer first — a wheel event is delivered over the
			// element under the cursor.
			if err := input.DispatchMouseEvent(input.MouseMoved, x, y).Do(actx); err != nil {
				return err
			}
			return input.DispatchMouseEvent(input.MouseWheel, x, y).
				WithDeltaX(opts.Dx).WithDeltaY(opts.Dy).Do(actx)
		}))
		if err != nil {
			return nil, err
		}
		return map[string]any{"scrolled": fmt.Sprintf("wheel:%g,%g", opts.Dx, opts.Dy)}, nil
	}

	// Default: JS scrollBy — deterministic and it fires the scroll events that
	// virtualized grids render on.
	expr := fmt.Sprintf("window.scrollBy(%g,%g)", opts.Dx, opts.Dy)
	if selector != "" {
		selJSON, _ := json.Marshal(selector)
		expr = fmt.Sprintf(
			"(() => { const el = document.querySelector(%s); if (!el) throw new Error('selector not found'); el.scrollBy(%g,%g); return true; })()",
			string(selJSON), opts.Dx, opts.Dy)
	}
	if err := c.run(ctx, id, chromedp.Evaluate(expr, nil)); err != nil {
		return nil, err
	}
	return map[string]any{"scrolled": fmt.Sprintf("by:%g,%g", opts.Dx, opts.Dy)}, nil
}

// wheelPoint returns the viewport point to dispatch the wheel at: the centre of
// selector's content box, or the viewport centre when selector is empty.
func wheelPoint(ctx context.Context, selector string, q QueryOpts) (float64, float64, error) {
	if selector == "" {
		var w, h float64
		if err := chromedp.Evaluate("window.innerWidth", &w).Do(ctx); err != nil {
			return 0, 0, err
		}
		if err := chromedp.Evaluate("window.innerHeight", &h).Do(ctx); err != nil {
			return 0, 0, err
		}
		return w / 2, h / 2, nil
	}
	var nodes []*cdp.Node
	if err := chromedp.Nodes(selector, &nodes, byFor(selector, q)...).Do(ctx); err != nil {
		return 0, 0, err
	}
	if len(nodes) == 0 {
		return 0, 0, fmt.Errorf("selector %q not found", selector)
	}
	return nodeCenter(ctx, nodes[0].NodeID)
}

// wheelOrigin decides where a wheel is delivered: an explicit coordinate, or
// the element/viewport centre wheelPoint computes. Anchoring at a point is
// what lets a map or canvas zoom around the cursor rather than its middle.
func wheelOrigin(ctx context.Context, selector string, opts ScrollOpts) (float64, float64, error) {
	if opts.At != nil {
		// scroll's envelope carries no `hit`, so the probe skips the element walk.
		if err := (&viewportGate{}).check(ctx, *opts.At); err != nil {
			return 0, 0, err
		}
		return opts.At.X, opts.At.Y, nil
	}
	return wheelPoint(ctx, selector, opts.Query)
}
