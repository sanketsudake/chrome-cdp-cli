package chrome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --on-dialog auto-handles a native JS dialog opened during an action, instead
// of letting it block the renderer and wedge the CDP connection. accept makes
// confirm() return true; dismiss makes it return false; both report the dialog.
func TestOnDialog(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>Dialog</title><body>
<div id="out">none</div>
<button id="c" onclick="window.__r = confirm('Are you sure?') ? 'yes' : 'no'; document.getElementById('out').textContent = window.__r">Confirm</button>
<button id="a" onclick="alert('hi'); document.getElementById('out').textContent='alerted'">Alert</button>
</body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// accept -> confirm() returns true. The whole call must complete (not hang on
	// the modal) within the action timeout.
	actx, acancel := context.WithTimeout(ctx, 12*time.Second)
	defer acancel()
	res, err := clickVia(actx, b, id, "#c", QueryOpts{By: "css", OnDialog: "accept"})
	if err != nil {
		t.Fatalf("Click #c --on-dialog accept: %v", err)
	}
	if v := evalString(ctx, t, b, id, "window.__r"); v != "yes" {
		t.Errorf("confirm result = %q, want yes (accept)", v)
	}
	if _, ok := res["dialogs"]; !ok {
		t.Errorf("result missing dialogs report: %v", res)
	}

	// dismiss -> confirm() returns false.
	dctx, dcancel := context.WithTimeout(ctx, 12*time.Second)
	defer dcancel()
	if _, err := clickVia(dctx, b, id, "#c", QueryOpts{By: "css", OnDialog: "dismiss"}); err != nil {
		t.Fatalf("Click #c --on-dialog dismiss: %v", err)
	}
	if v := evalString(ctx, t, b, id, "window.__r"); v != "no" {
		t.Errorf("confirm result = %q, want no (dismiss)", v)
	}

	// alert() has no return but must not wedge; the click completes and the
	// post-alert statement runs.
	lctx, lcancel := context.WithTimeout(ctx, 12*time.Second)
	defer lcancel()
	if _, err := clickVia(lctx, b, id, "#a", QueryOpts{By: "css", OnDialog: "accept"}); err != nil {
		t.Fatalf("Click #a (alert) --on-dialog accept: %v", err)
	}
	if v := evalString(ctx, t, b, id, "document.getElementById('out').textContent"); v != "alerted" {
		t.Errorf("out = %q, want alerted (alert should have been auto-handled)", v)
	}
}
