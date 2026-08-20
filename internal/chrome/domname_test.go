package chrome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

// domNameQuery resolves elements by a DOM-computed accessible name (the fallback
// used when Chrome throttles the a11y tree on a hidden tab). It covers the common
// name sources and keeps the "skip hidden" property.
func TestDOMNameResolution(t *testing.T) {
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>DOMName</title><body>
<button aria-label="Save">S</button>
<a href="/home">Home</a>
<label for="em">Email</label><input id="em">
<button aria-label="Save" style="display:none">hidden save</button>
<span aria-label="Save">not focusable span</span>
</body>`)
	}))
	defer srv.Close()

	cdpB := b
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	resolve := func(name, role, match string) string {
		t.Helper()
		var desc string
		rerr := cdpB.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
			ids, e := domNameQuery(actx, name, role, 0, match, "")
			if e != nil {
				return e
			}
			if len(ids) == 0 {
				desc = ""
				return nil
			}
			n, e := dom.DescribeNode().WithNodeID(ids[0]).Do(actx)
			if e != nil {
				return e
			}
			desc = describeNode(n)
			return nil
		}))
		if rerr != nil {
			t.Fatalf("resolve %q/%q: %v", name, role, rerr)
		}
		return desc
	}

	if got := resolve("Save", "button", "exact"); got != "BUTTON[aria-label=Save]" {
		t.Errorf("Save/button -> %q, want the visible BUTTON (skip the display:none one and the span)", got)
	}
	if got := resolve("Home", "link", "exact"); got != "A" {
		t.Errorf("Home/link -> %q, want the <a>", got)
	}
	if got := resolve("Email", "textbox", "exact"); got != "INPUT[id=em]" {
		t.Errorf("Email/textbox (via <label for>) -> %q, want the input", got)
	}
	// No visible match -> nothing.
	if got := resolve("Nonexistent", "", "exact"); got != "" {
		t.Errorf("Nonexistent -> %q, want empty", got)
	}
}

func describeNode(n *cdp.Node) string {
	s := n.NodeName
	get := func(k string) string {
		for i := 0; i+1 < len(n.Attributes); i += 2 {
			if n.Attributes[i] == k {
				return n.Attributes[i+1]
			}
		}
		return ""
	}
	if v := get("aria-label"); v != "" {
		s += "[aria-label=" + v + "]"
	} else if v := get("id"); v != "" {
		s += "[id=" + v + "]"
	}
	return s
}
