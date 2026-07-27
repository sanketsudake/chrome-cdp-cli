package chrome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --in-row scopes an accessible-name match to the row whose text contains the
// given string — so a "Delete" button repeated across many rows is disambiguated
// by row content, not by index. This is the Engage "delete the TEST row" case.
func TestInRowAddressing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Rows</title><body>
<div id="clicked">none</div>
<table><tbody>
  <tr><td>Alpha entry</td><td><button onclick="document.getElementById('clicked').textContent='alpha'">Delete</button></td></tr>
  <tr><td>Bravo entry</td><td><button onclick="document.getElementById('clicked').textContent='bravo'">Delete</button></td></tr>
  <tr><td>Charlie entry</td><td><button onclick="document.getElementById('clicked').textContent='charlie'">Delete</button></td></tr>
</tbody></table>
</body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// Click the Delete button in the Bravo row (name "Delete" is ambiguous across
	// three rows; --in-row "Bravo" scopes it).
	if _, err := clickVia(ctx, b, id, "Delete", QueryOpts{By: "name", Role: "button", InRow: "Bravo"}); err != nil {
		t.Fatalf("Click --by name Delete --in-row Bravo: %v", err)
	}
	if v := evalString(ctx, t, b, id, "document.getElementById('clicked').textContent"); v != "bravo" {
		t.Errorf("clicked = %q, want bravo (row scoping picked the wrong Delete)", v)
	}

	// A row substring that matches nothing resolves to no element (times out fast
	// under the tight ctx below), never a wrong click.
	nctx, ncancel := context.WithTimeout(ctx, 3*time.Second)
	defer ncancel()
	if _, err := clickVia(nctx, b, id, "Delete", QueryOpts{By: "name", Role: "button", InRow: "Zeta"}); err == nil {
		t.Errorf("Click --in-row Zeta (no such row) should not have matched")
	}
	if v := evalString(ctx, t, b, id, "document.getElementById('clicked').textContent"); v != "bravo" {
		t.Errorf("clicked changed to %q after a no-match --in-row; expected it to stay bravo", v)
	}

	// --in-row can't combine with a structured --by mode (ref/cell/label).
	ictx, icancel := context.WithTimeout(ctx, 3*time.Second)
	defer icancel()
	if _, err := clickVia(ictx, b, id, "Delete", QueryOpts{By: "label", InRow: "Bravo"}); err == nil {
		t.Errorf("Click --by label --in-row should error (incompatible)")
	}
}
