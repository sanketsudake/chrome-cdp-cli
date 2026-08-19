package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
)

// TestDialogEventRetainsLastAndClearsOnClose is RFC-0018 VS-9 (pure): the
// listener replaces (not queues) the retained dialog on a second opening,
// clears it on closed, treats a closed with nothing retained as a no-op, and
// keeps state per tab id.
func TestDialogEventRetainsLastAndClearsOnClose(t *testing.T) {
	t.Parallel()
	c := &CDP{dialogs: map[string]dialogState{}}

	c.dialogEvent("t1", &page.EventJavascriptDialogOpening{Type: page.DialogTypeConfirm, Message: "A"})
	if st, ok := c.dialog("t1"); !ok || st.Message != "A" {
		t.Fatalf("after opening A: dialog = %+v, ok=%v", st, ok)
	}

	c.dialogEvent("t1", &page.EventJavascriptDialogOpening{Type: page.DialogTypeConfirm, Message: "B"})
	if st, ok := c.dialog("t1"); !ok || st.Message != "B" {
		t.Fatalf("after opening B (should replace, not queue): dialog = %+v, ok=%v", st, ok)
	}

	c.dialogEvent("t1", &page.EventJavascriptDialogClosed{})
	if _, ok := c.dialog("t1"); ok {
		t.Fatal("dialog should be cleared after closed")
	}

	// A closed with nothing retained is a no-op, not a panic or a spurious entry.
	c.dialogEvent("t2", &page.EventJavascriptDialogClosed{})
	if _, ok := c.dialog("t2"); ok {
		t.Fatal("closed on an untouched tab should not create state")
	}

	// State is per tab id.
	c.dialogEvent("t1", &page.EventJavascriptDialogOpening{Type: page.DialogTypeAlert, Message: "t1"})
	c.dialogEvent("t3", &page.EventJavascriptDialogOpening{Type: page.DialogTypeAlert, Message: "t3"})
	st1, ok1 := c.dialog("t1")
	st3, ok3 := c.dialog("t3")
	if !ok1 || !ok3 || st1.Message == st3.Message {
		t.Fatalf("state is not per-tab: t1=%+v(%v) t3=%+v(%v)", st1, ok1, st3, ok3)
	}
}

// TestDialogStatusResultKeys pins the field table: the open map carries all
// six keys with no note when not fresh, the closed map carries only `open`,
// and `note` appears only on a fresh attach.
func TestDialogStatusResultKeys(t *testing.T) {
	t.Parallel()
	openAt := time.Date(2026, 8, 19, 10, 15, 0, 412000000, time.UTC)
	st := dialogState{
		Type: "confirm", Message: "Delete 3 items?", DefaultPrompt: "",
		FrameURL: "https://example.com/items", OpenedAt: openAt,
	}

	open := dialogStatusResult(st, true, false)
	for _, k := range []string{"open", "type", "message", "default_prompt", "frame_url", "opened_at"} {
		if _, ok := open[k]; !ok {
			t.Errorf("open result missing %q: %v", k, open)
		}
	}
	if open["open"] != true {
		t.Errorf("open = %v, want true", open["open"])
	}
	if _, ok := open["note"]; ok {
		t.Errorf("a not-fresh open result should carry no note: %v", open)
	}
	// opened_at is left as a time.Time so it gets exactly the encoding
	// console.go's `ts` field gets from encoding/json's own default — RFC 3339
	// UTC, millisecond precision, verified here by marshaling it the same way
	// the envelope eventually will.
	b, err := json.Marshal(open["opened_at"])
	if err != nil {
		t.Fatalf("marshal opened_at: %v", err)
	}
	if got := string(b); got != `"2026-08-19T10:15:00.412Z"` {
		t.Errorf("opened_at marshaled = %s, want \"2026-08-19T10:15:00.412Z\"", got)
	}

	closed := dialogStatusResult(dialogState{}, false, false)
	if len(closed) != 1 {
		t.Errorf("closed result should carry only open: %v", closed)
	}
	if closed["open"] != false {
		t.Errorf("closed open = %v, want false", closed["open"])
	}

	fresh := dialogStatusResult(dialogState{}, false, true)
	if fresh["note"] != dialogUnwatchedNote {
		t.Errorf("fresh result missing the unwatched note: %v", fresh)
	}
}

// --on-dialog auto-handles a native JS dialog opened during an action, instead
// of letting it block the renderer and wedge the CDP connection. accept makes
// confirm() return true; dismiss makes it return false; both report the dialog.
func TestOnDialog(t *testing.T) {
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

	// RFC-0018 VS-8: --on-dialog's own handling leaves nothing retained for
	// `dialog` to find afterwards — withDialog and listenDialog both see the
	// opening, and the closed event clears listenDialog's retained state too.
	res, err = b.DialogStatus(ctx, id)
	if err != nil {
		t.Fatalf("DialogStatus after --on-dialog: %v", err)
	}
	if res["open"] != false {
		t.Errorf("DialogStatus after --on-dialog handling = %v, want open:false", res)
	}
}

// evalOutcome is the result of a blocking Eval run off the test goroutine —
// necessary whenever the expression opens a native dialog, since Eval does not
// return until the dialog is handled (RFC-0018's whole premise).
type evalOutcome struct {
	value string
	err   error
}

// evalAsync runs js and delivers its string value (or error) on the returned
// channel, without blocking the caller — the goroutine's own t.Fatalf would
// not be goroutine-safe, so the caller reads the channel and asserts itself.
func evalAsync(ctx context.Context, b Browser, id, js string) <-chan evalOutcome {
	ch := make(chan evalOutcome, 1)
	go func() {
		v, err := b.Eval(ctx, id, js, EvalOpts{})
		if err != nil {
			ch <- evalOutcome{err: err}
			return
		}
		s, _ := v.(map[string]any)["value"].(string)
		ch <- evalOutcome{value: s}
	}()
	return ch
}

// waitDialogOpen polls DialogStatus until it reports a retained dialog, up to
// 5s at 50ms — the eval that opened it is still blocked in evalAsync's
// goroutine, so polling (not a fixed sleep) is what makes this reliable rather
// than merely usually-fast-enough.
func waitDialogOpen(ctx context.Context, t *testing.T, b Browser, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := b.DialogStatus(ctx, id)
		if err != nil {
			t.Fatalf("DialogStatus: %v", err)
		}
		if res["open"] == true {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("dialog never reported open within 5s (last status: %v)", res)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dialogFixtureServer serves a page with nothing on it: every test below opens
// its dialog directly through Eval rather than a click, so the fixture only
// needs to give DialogStatus's frame_url something real to report.
func dialogFixtureServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Dialog fixture</title><body>ready</body>`)
	}))
}

// TestDialogStatusAndHandleWhileBlocked is RFC-0018 VS-1, VS-2, VS-3, VS-5:
// status reports an open confirm without touching the renderer, accept
// unblocks it with the page seeing true, dismiss gives false, and text passed
// to a non-prompt dialog is not sent and is reported as ignored.
func TestDialogStatusAndHandleWhileBlocked(t *testing.T) {
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

	srv := dialogFixtureServer()
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// VS-1 / VS-2: accept unblocks and the page sees true.
	before := time.Now()
	ch := evalAsync(ctx, b, id, "String(confirm('Sure?'))")
	status := waitDialogOpen(ctx, t, b, id)
	if status["type"] != "confirm" || status["message"] != "Sure?" || status["default_prompt"] != "" {
		t.Fatalf("status while open = %v, want type=confirm message=%q default_prompt=\"\"", status, "Sure?")
	}
	if fu, _ := status["frame_url"].(string); !strings.HasPrefix(fu, srv.URL) {
		t.Errorf("frame_url = %q, want it to start with %q", fu, srv.URL)
	}
	// This is an in-process call (no RPC in between), so opened_at is still the
	// time.Time DialogStatus built it from, not yet flattened to a string.
	openedAt, ok := status["opened_at"].(time.Time)
	if !ok || openedAt.Before(before.Add(-time.Second)) || openedAt.After(time.Now().Add(time.Second)) {
		t.Errorf("opened_at = %v (a %T), want a time.Time within the test's wall-clock window", status["opened_at"], status["opened_at"])
	}

	handled, err := b.DialogHandle(ctx, id, true, "")
	if err != nil {
		t.Fatalf("DialogHandle accept: %v", err)
	}
	if handled["handled"] != true || handled["action"] != "accept" || handled["type"] != "confirm" {
		t.Errorf("DialogHandle accept result = %v", handled)
	}
	out := <-ch
	if out.err != nil {
		t.Fatalf("blocked eval: %v", out.err)
	}
	if out.value != "true" {
		t.Errorf("confirm() result = %q, want true (accept)", out.value)
	}
	after, err := b.DialogStatus(ctx, id)
	if err != nil {
		t.Fatalf("DialogStatus after accept: %v", err)
	}
	if after["open"] != false {
		t.Errorf("DialogStatus after accept = %v, want open:false", after)
	}
	if _, ok := after["note"]; ok {
		t.Errorf("an already-attached tab must carry no note: %v", after)
	}

	// VS-3: dismiss gives false.
	ch = evalAsync(ctx, b, id, "String(confirm('Again?'))")
	waitDialogOpen(ctx, t, b, id)
	dismissed, err := b.DialogHandle(ctx, id, false, "")
	if err != nil {
		t.Fatalf("DialogHandle dismiss: %v", err)
	}
	if dismissed["action"] != "dismiss" {
		t.Errorf("DialogHandle dismiss result = %v", dismissed)
	}
	out = <-ch
	if out.err != nil {
		t.Fatalf("blocked eval: %v", out.err)
	}
	if out.value != "false" {
		t.Errorf("confirm() result = %q, want false (dismiss)", out.value)
	}

	// VS-5: text on a confirm is not sent and is reported.
	ch = evalAsync(ctx, b, id, "String(confirm('Ignored?'))")
	waitDialogOpen(ctx, t, b, id)
	ignored, err := b.DialogHandle(ctx, id, true, "ignored")
	if err != nil {
		t.Fatalf("DialogHandle accept with text on a confirm: %v", err)
	}
	if ignored["text_ignored"] != true {
		t.Errorf("DialogHandle result = %v, want text_ignored:true", ignored)
	}
	if _, ok := ignored["text"]; ok {
		t.Errorf("DialogHandle result should carry no text key: %v", ignored)
	}
	out = <-ch
	if out.err != nil {
		t.Fatalf("blocked eval: %v", out.err)
	}
	if out.value != "true" {
		t.Errorf("confirm() result = %q, want true (accept, text ignored)", out.value)
	}
}

// TestDialogPromptText is RFC-0018 VS-4: the text round-trips to a prompt()
// and the default is never implied — accept with no text answers "", not the
// prompt's own default.
func TestDialogPromptText(t *testing.T) {
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

	srv := dialogFixtureServer()
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	ch := evalAsync(ctx, b, id, "String(prompt('Name?', 'dflt'))")
	status := waitDialogOpen(ctx, t, b, id)
	if status["type"] != "prompt" || status["default_prompt"] != "dflt" {
		t.Fatalf("status while open = %v, want type=prompt default_prompt=dflt", status)
	}
	res, err := b.DialogHandle(ctx, id, true, "bob")
	if err != nil {
		t.Fatalf("DialogHandle accept bob: %v", err)
	}
	if res["text"] != "bob" {
		t.Errorf("DialogHandle result = %v, want text:bob", res)
	}
	out := <-ch
	if out.err != nil {
		t.Fatalf("blocked eval: %v", out.err)
	}
	if out.value != "bob" {
		t.Errorf("prompt() result = %q, want bob", out.value)
	}

	ch = evalAsync(ctx, b, id, "String(prompt('Name?', 'dflt'))")
	waitDialogOpen(ctx, t, b, id)
	res, err = b.DialogHandle(ctx, id, true, "")
	if err != nil {
		t.Fatalf("DialogHandle accept with no text: %v", err)
	}
	if res["text"] != "" {
		t.Errorf("DialogHandle result = %v, want text:\"\" (not the default)", res)
	}
	out = <-ch
	if out.err != nil {
		t.Fatalf("blocked eval: %v", out.err)
	}
	if out.value != "" {
		t.Errorf("prompt() result = %q, want \"\" (not the default dflt)", out.value)
	}
}

// TestDialogNothingOpen is RFC-0018 VS-6: on a watched tab with nothing open,
// status is a plain {open:false} and handle refuses at once with ErrNoDialog
// rather than issuing Page.handleJavaScriptDialog and waiting out a deadline.
func TestDialogNothingOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := dialogFixtureServer()
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	status, err := b.DialogStatus(ctx, id)
	if err != nil {
		t.Fatalf("DialogStatus: %v", err)
	}
	if status["open"] != false {
		t.Errorf("DialogStatus = %v, want open:false", status)
	}
	if _, ok := status["note"]; ok {
		t.Errorf("an already-attached tab must carry no note: %v", status)
	}

	start := time.Now()
	if _, err := b.DialogHandle(ctx, id, true, ""); !IsNoDialog(err) {
		t.Fatalf("DialogHandle with nothing open: err = %v, want ErrNoDialog", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("DialogHandle took %v; it should refuse at once, not wait out a deadline", d)
	}
}

// TestDialogFreshTabCarriesNote is RFC-0018 VS-7, strengthened past the RFC's
// own test plan (which excludes this exact arrangement as "not reproduced in
// the suite") because the controller asked this task to re-confirm the RFC's
// load-bearing premise empirically rather than trust the out-of-repo probe it
// cites: that a session attaching AFTER a dialog has already opened, with no
// other Page-enabled session ever attached to that tab, gets no replay of
// javascriptDialogOpening.
//
// Open (browser-level Target.createTarget) deliberately never attaches a
// session, so the tab it returns is exactly that arrangement once the page's
// own timer has fired. Findings are recorded in the task report, not just
// asserted here: if a real Chrome ever DOES replay the event to a fresh
// attach, this test's first assertion fails loudly rather than the gap going
// unnoticed, and the retention design is unaffected either way — status
// reads retained state, never the renderer, and handle never issues the
// close command blind.
func TestDialogFreshTabCarriesNote(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The dialog opens on ITS OWN, half a second after load, entirely
		// outside anything this CDP is doing — the page needs no cooperating
		// client. By the time the test gets around to asking, no session has
		// EVER been attached to this tab.
		fmt.Fprint(w, `<!doctype html><title>Fresh</title><body>
<script>setTimeout(function(){ window.__r = confirm('late') ? 'yes' : 'no'; }, 500);</script>
</body>`)
	}))
	defer srv.Close()

	// Open is the browser-level Target.createTarget path: it creates and
	// navigates the tab WITHOUT calling c.on, so this CDP has attached to
	// nothing here yet — the exact "never touched before" tab RFC-0018 US-6
	// describes.
	opened, err := b.Open(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, _ := opened["id"].(string)
	if id == "" {
		t.Fatalf("Open result carries no id: %v", opened)
	}

	// Long enough that the page's confirm() is certainly up (500ms) before the
	// first DialogStatus below, so this is genuinely the "opened with no
	// Page-enabled session at all" case the RFC's probe describes, not a race.
	time.Sleep(1 * time.Second)

	first, err := b.DialogStatus(ctx, id)
	if err != nil {
		t.Fatalf("DialogStatus (first, fresh attach while a dialog is actually up): %v", err)
	}
	if first["open"] != false {
		t.Errorf("DialogStatus on the first attach after a pre-existing dialog = %v, want open:false "+
			"(this is the empirical check for RFC-0018's no-replay premise: a real Chrome that DOES "+
			"replay javascriptDialogOpening to a late attach would fail this assertion)", first)
	}
	if first["note"] != dialogUnwatchedNote {
		t.Errorf("first DialogStatus on a never-touched tab missing the note: %v", first)
	}

	start := time.Now()
	if _, err := b.DialogHandle(ctx, id, true, ""); !IsNoDialog(err) {
		t.Fatalf("DialogHandle on a fresh tab with a real dialog up: err = %v, want ErrNoDialog "+
			"(the design never issues the close command blind, whatever the attach itself did)", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("DialogHandle took %v; it must refuse at once from retained state, not hang trying to close a dialog it never saw", d)
	}

	second, err := b.DialogStatus(ctx, id)
	if err != nil {
		t.Fatalf("DialogStatus (second): %v", err)
	}
	if _, ok := second["note"]; ok {
		t.Errorf("second DialogStatus on a now-attached tab should carry no note: %v", second)
	}
}
