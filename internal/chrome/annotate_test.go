package chrome

// Pure tests for the RFC-0016 actionable predicate — no browser. axV/axProp
// come from find_test.go, which builds nodes the same way.

import (
	"testing"

	"github.com/chromedp/cdproto/accessibility"
)

func TestAnnotateActionable(t *testing.T) {
	t.Parallel()
	focusable := []*accessibility.Property{axProp("focusable", "true")}

	cases := map[string]struct {
		node *accessibility.Node
		want bool
	}{
		"button in annotateRoles":                {&accessibility.Node{Role: axV("button"), Name: axV("Save"), BackendDOMNodeID: 1}, true},
		"link in annotateRoles":                  {&accessibility.Node{Role: axV("link"), Name: axV("Home"), BackendDOMNodeID: 2}, true},
		"textbox in annotateRoles":               {&accessibility.Node{Role: axV("textbox"), Name: axV("Search"), BackendDOMNodeID: 3}, true},
		"searchbox in annotateRoles":             {&accessibility.Node{Role: axV("searchbox"), Name: axV(""), BackendDOMNodeID: 4}, true},
		"combobox in annotateRoles":              {&accessibility.Node{Role: axV("combobox"), Name: axV("Country"), BackendDOMNodeID: 5}, true},
		"checkbox in annotateRoles":              {&accessibility.Node{Role: axV("checkbox"), Name: axV("Agree"), BackendDOMNodeID: 6}, true},
		"radio in annotateRoles":                 {&accessibility.Node{Role: axV("radio"), Name: axV("Yes"), BackendDOMNodeID: 7}, true},
		"switch in annotateRoles":                {&accessibility.Node{Role: axV("switch"), Name: axV("Dark mode"), BackendDOMNodeID: 8}, true},
		"slider in annotateRoles":                {&accessibility.Node{Role: axV("slider"), Name: axV("Volume"), BackendDOMNodeID: 9}, true},
		"spinbutton in annotateRoles":            {&accessibility.Node{Role: axV("spinbutton"), Name: axV("Qty"), BackendDOMNodeID: 10}, true},
		"tab in annotateRoles":                   {&accessibility.Node{Role: axV("tab"), Name: axV("Overview"), BackendDOMNodeID: 11}, true},
		"menuitem in annotateRoles":              {&accessibility.Node{Role: axV("menuitem"), Name: axV("Open"), BackendDOMNodeID: 12}, true},
		"menuitemcheckbox in roles":              {&accessibility.Node{Role: axV("menuitemcheckbox"), Name: axV("Show"), BackendDOMNodeID: 13}, true},
		"menuitemradio in roles":                 {&accessibility.Node{Role: axV("menuitemradio"), Name: axV("A"), BackendDOMNodeID: 14}, true},
		"option in annotateRoles":                {&accessibility.Node{Role: axV("option"), Name: axV("Red"), BackendDOMNodeID: 15}, true},
		"treeitem in annotateRoles":              {&accessibility.Node{Role: axV("treeitem"), Name: axV("Documents"), BackendDOMNodeID: 16}, true},
		"focusable generic is structural, drops": {&accessibility.Node{Role: axV("generic"), Name: axV(""), BackendDOMNodeID: 17, Properties: focusable}, false},
		"focusable gridcell qualifies":           {&accessibility.Node{Role: axV("gridcell"), Name: axV("3"), BackendDOMNodeID: 18, Properties: focusable}, true},
		"non-focusable gridcell drops":           {&accessibility.Node{Role: axV("gridcell"), Name: axV("3"), BackendDOMNodeID: 19}, false},
		"focusable cell qualifies like gridcell": {&accessibility.Node{Role: axV("cell"), Name: axV("3"), BackendDOMNodeID: 20, Properties: focusable}, true},
		"non-focusable cell drops":               {&accessibility.Node{Role: axV("cell"), Name: axV("3"), BackendDOMNodeID: 26}, false},
		"ignored button drops":                   {&accessibility.Node{Role: axV("button"), Name: axV("Hidden"), BackendDOMNodeID: 21, Ignored: true}, false},
		"no backend id drops":                    {&accessibility.Node{Role: axV("button"), Name: axV("Ghost")}, false},
		"non-focusable heading drops":            {&accessibility.Node{Role: axV("heading"), Name: axV("Title"), BackendDOMNodeID: 22}, false},
		"focusable RootWebArea drops":            {&accessibility.Node{Role: axV("RootWebArea"), Name: axV("Page"), BackendDOMNodeID: 23, Properties: focusable}, false},
		"focusable region drops":                 {&accessibility.Node{Role: axV("region"), Name: axV("Sidebar"), BackendDOMNodeID: 24, Properties: focusable}, false},
		"disabled button still counts":           {&accessibility.Node{Role: axV("button"), Name: axV("Save"), BackendDOMNodeID: 25, Properties: []*accessibility.Property{axProp("disabled", "true")}}, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := annotateActionable(c.node); got != c.want {
				t.Errorf("annotateActionable(%+v) = %v, want %v", c.node, got, c.want)
			}
		})
	}
}

// TestAnnotateDegradeReason pins the branching Screenshot's annotatePass uses
// to pick a degrade reason. It exists as a pure test because live-triggering
// each of the three outcomes (a genuine tree-read error, an actually throttled
// tab, a clean empty pass) is not reliably reproducible across Chrome builds —
// the same reason find's DOM-fallback ranking is tested this way instead of
// through a live backgrounded tab.
func TestAnnotateDegradeReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		ok, hidden bool
		wantReason string
	}{
		{"hidden tab, tree read never completed", false, true, "tab_hidden"},
		{"hidden tab, tree read completed empty", true, true, "tab_hidden"},
		{"visible tab, tree read failed or timed out", false, false, "tree_unavailable"},
		{"visible tab, clean pass with nothing actionable", true, false, "no_actionable_nodes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := annotateDegradeReason(c.ok, c.hidden); got != c.wantReason {
				t.Errorf("annotateDegradeReason(ok=%v, hidden=%v) = %q, want %q", c.ok, c.hidden, got, c.wantReason)
			}
		})
	}
}

// annotateCandidates preserves document (tree) order and applies the same
// axFilterNodes selection snap/find share — an ignored node never reaches the
// predicate even though it would fail it anyway, which is the point of
// reusing the shared filter rather than hand-rolling one.
func TestAnnotateCandidatesOrderAndFilter(t *testing.T) {
	t.Parallel()
	nodes := []*accessibility.Node{
		{NodeID: "1", Role: axV("RootWebArea"), Name: axV("Fixture")},
		{NodeID: "2", Role: axV("button"), Name: axV("First"), BackendDOMNodeID: 101},
		{NodeID: "3", Role: axV("heading"), Name: axV("Section"), BackendDOMNodeID: 102},
		{NodeID: "4", Role: axV("button"), Name: axV("Hidden"), BackendDOMNodeID: 103, Ignored: true},
		{NodeID: "5", Role: axV("link"), Name: axV("Second"), BackendDOMNodeID: 104},
	}
	got := annotateCandidates(nodes)
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2 (First button, Second link): %+v", len(got), got)
	}
	if axString(got[0].Name) != "First" || axString(got[1].Name) != "Second" {
		t.Errorf("candidates = %q, %q, want First then Second in document order", axString(got[0].Name), axString(got[1].Name))
	}
}
