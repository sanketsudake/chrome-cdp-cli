package chrome

// Integration test against a MANAGED headless Chrome (Path A) — a throwaway
// browser, never the user's live Chrome. Skips when -short or when Chrome can't
// be launched (e.g. CI without a browser).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// tmpProfile returns a throwaway managed-Chrome profile dir. Unlike t.TempDir(),
// its cleanup is best-effort: managed Chrome tears its profile down
// asynchronously, so a strict RemoveAll can race the browser exit and fail with
// "directory not empty" — which must not fail an otherwise-passing test.
func tmpProfile(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "chrome-cdp-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestManagedChromeDrivesAPage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Drive a managed headless Chrome directly (Path A), independent of the
	// connection ladder — so this runs even when the dev's real Chrome is up.
	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Fixture</title><body><button id="go">Go</button></body>`)
	}))
	defer srv.Close()

	tabs, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tabs) == 0 {
		t.Fatal("List returned no page targets")
	}
	id := tabs[0].ID

	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	got, err := b.Eval(ctx, id, "document.title", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if v := got.(map[string]any)["value"]; v != "Fixture" {
		t.Errorf("document.title = %v, want Fixture", v)
	}

	// raw passthrough: Runtime.evaluate returning a value.
	rawRes, err := b.Raw(ctx, id, "Runtime.evaluate", []byte(`{"expression":"1+1","returnByValue":true}`))
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	if !strings.Contains(fmt.Sprintf("%v", rawRes), "2") {
		t.Errorf("raw Runtime.evaluate result = %v, want it to contain 2", rawRes)
	}

	// frame enumeration returns the real frame tree.
	fr, err := b.Frames(ctx, id)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	frames := fr.(map[string]any)["frames"].([]map[string]any)
	if len(frames) == 0 {
		t.Fatal("Frames returned no frames")
	}
}

// Reproduces the Workday failure shape: a hidden element with the SAME
// accessible name precedes the real, visible control. --by name must skip the
// ignored (hidden) match and act on the visible one — where --by css (first
// match + wait-visible) would stall on the hidden node.
func TestAccessibleNameAddressing(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>AX</title><body>
<button aria-label="Request Absence" style="display:none">hidden-RA</button>
<button style="position:absolute;left:-9999px">Skip to main content</button>
<button aria-label="Request Absence" onclick="window.__ra=(window.__ra||0)+1">RA</button>
<button aria-label="Dup">d1</button>
<button aria-label="Dup">d2</button>
<button aria-label="Review Approval: Awaiting Action by You">Review</button>
</body>`)
	}))
	defer srv.Close()

	tabs, err := b.List(ctx)
	if err != nil || len(tabs) == 0 {
		t.Fatalf("List: %v", err)
	}
	id := tabs[0].ID
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// --by name resolves the VISIBLE "Request Absence" (text "RA"), not the
	// hidden display:none one that shares the name.
	res, err := b.Text(ctx, id, "Request Absence", TextOpts{Query: QueryOpts{By: "name"}})
	if err != nil {
		t.Fatalf("Text --by name: %v", err)
	}
	if got := res["text"]; got != "RA" {
		t.Errorf("--by name text = %q, want RA (the visible match)", got)
	}

	// Clicking by name activates the visible control (window.__ra becomes 1).
	if _, err := clickVia(ctx, b, id, "Request Absence", QueryOpts{By: "name", Role: "button"}); err != nil {
		t.Fatalf("Click --by name --role button: %v", err)
	}
	clicked, err := b.Eval(ctx, id, "window.__ra", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if v := clicked.(map[string]any)["value"]; fmt.Sprintf("%v", v) != "1" {
		t.Errorf("window.__ra = %v, want 1 (clicked the visible RA button)", v)
	}

	// --match contains finds a control by a substring of its verbose accessible
	// name (the real-world "Review" vs "Review Approval: Awaiting Action …" case)
	// where exact match would miss.
	rv, err := b.Text(ctx, id, "Review", TextOpts{Query: QueryOpts{By: "name", Role: "button", Match: "contains"}})
	if err != nil {
		t.Errorf("Text --by name --match contains: %v", err)
	} else if rv["text"] != "Review" {
		t.Errorf("--match contains text = %q, want Review", rv["text"])
	}

	// --nth disambiguates duplicate accessible names.
	for _, tc := range []struct {
		nth  int
		want string
	}{{1, "d1"}, {2, "d2"}} {
		r, err := b.Text(ctx, id, "Dup", TextOpts{Query: QueryOpts{By: "name", Nth: tc.nth}})
		if err != nil {
			t.Fatalf("Text --by name --nth %d: %v", tc.nth, err)
		}
		if r["text"] != tc.want {
			t.Errorf("--nth %d text = %q, want %q", tc.nth, r["text"], tc.want)
		}
	}

	// Contrast: --by css "button" (first match + wait-visible) stalls on the
	// hidden-first button — the failure --by name fixes. Bounded so it's quick.
	short, scancel := context.WithTimeout(ctx, 3*time.Second)
	defer scancel()
	if _, err := b.Text(short, id, "button", TextOpts{Query: QueryOpts{By: "css"}}); err == nil {
		t.Error("expected --by css to stall on the hidden-first button, but it succeeded")
	}
}

// snap surfaces aria-live toasts, widget states, field values, and the focused
// element — so verification needs no screenshot.
func TestSnapStateAndAlerts(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>State</title><body>
<div role="status" aria-live="polite">Saved successfully</div>
<button aria-pressed="true" aria-label="Bold">B</button>
<button disabled aria-label="Submit now">Submit</button>
<input aria-label="Your name" value="Sanket" id="nm">
<script>document.getElementById('nm').focus()</script>
</body>`)
	}))
	defer srv.Close()

	tabs, err := b.List(ctx)
	if err != nil || len(tabs) == 0 {
		t.Fatalf("List: %v", err)
	}
	id := tabs[0].ID
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	got, err := b.Snapshot(ctx, id, SnapOpts{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Assert on the real JSON shape a consumer sees.
	raw, _ := json.Marshal(got)
	var s struct {
		Nodes []struct {
			Role, Name, Value string
			States            []string
		} `json:"nodes"`
		Alerts  []string                    `json:"alerts"`
		Focused struct{ Role, Name string } `json:"focused"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("snap json: %v", err)
	}

	if !contains(s.Alerts, "Saved successfully") {
		t.Errorf("alerts = %v, want it to contain the live-region toast", s.Alerts)
	}
	if s.Focused.Name != "Your name" {
		t.Errorf("focused = %+v, want the focused input", s.Focused)
	}
	stateOf := func(name string) []string {
		for _, n := range s.Nodes {
			if n.Name == name {
				return n.States
			}
		}
		return nil
	}
	if !contains(stateOf("Bold"), "pressed") {
		t.Errorf("Bold states = %v, want pressed", stateOf("Bold"))
	}
	if !contains(stateOf("Submit now"), "disabled") {
		t.Errorf("Submit states = %v, want disabled", stateOf("Submit now"))
	}
	for _, n := range s.Nodes {
		if n.Name == "Your name" && n.Value != "Sanket" {
			t.Errorf("Your name value = %q, want Sanket", n.Value)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestWaitConditions(t *testing.T) {
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

	// A page where, after 300ms, a spinner is removed, a hidden element appears,
	// and the URL hash changes — one page to exercise --gone / --visible / --url.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Wait</title><body>
<div id="spinner">loading</div>
<div id="late" style="display:none">ready</div>
<div role="status" aria-live="polite" id="st"></div>
<script>setTimeout(function(){
  document.getElementById('spinner').remove();
  document.getElementById('late').style.display='block';
  document.getElementById('st').textContent='Saved successfully';
  location.hash='done';
}, 300)</script>
</body>`)
	}))
	defer srv.Close()

	tabs, err := b.List(ctx)
	if err != nil || len(tabs) == 0 {
		t.Fatalf("List: %v", err)
	}
	id := tabs[0].ID
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	cases := []struct {
		name string
		cond WaitCond
		want string
	}{
		{"gone", WaitCond{Gone: "#spinner"}, "gone:#spinner"},
		{"visible", WaitCond{Visible: "#late"}, "visible:#late"},
		{"url", WaitCond{URL: "#done"}, "url:#done"},
		{"text", WaitCond{Text: "Saved"}, "text:Saved"},
		{"stable", WaitCond{Stable: true}, "stable"},
	}
	for _, tc := range cases {
		bctx, bcancel := context.WithTimeout(ctx, 8*time.Second)
		res, err := b.Wait(bctx, id, tc.cond)
		bcancel()
		if err != nil {
			t.Errorf("Wait %s: %v", tc.name, err)
			continue
		}
		if res["waited"] != tc.want {
			t.Errorf("Wait %s = %v, want %q", tc.name, res["waited"], tc.want)
		}
	}

	// An empty condition is a usage error, not a hang.
	if _, err := b.Wait(ctx, id, WaitCond{}); err == nil {
		t.Error("Wait with no condition should error")
	}
}
