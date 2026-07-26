package chrome

// Tests for the `upload` verb (RFC-0006).
//
// The pure half (the `accept` grammar, the element description) runs anywhere.
// The rest drives a managed headless Chrome against a fixture page that records
// its `change` events into window.__log, then reads them back with Eval — the
// only way to prove the page actually saw the files, rather than that a CDP
// call returned nil. Skipped under -short, and never parallel: they share a
// browser.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// The `accept` attribute is advisory in HTML, so this only ever decides whether
// the envelope carries a warning — but it has to understand all three forms the
// attribute takes, or the warning fires on correct uploads and gets ignored.
func TestAcceptMismatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		accept string
		paths  []string
		want   bool
	}{
		{"no accept covers everything", "", []string{"/a/x.txt"}, false},
		{"extension matches", ".pdf", []string{"/a/x.pdf"}, false},
		{"extension is case-insensitive", ".PDF", []string{"/a/x.pdf"}, false},
		{"extension does not match", ".pdf", []string{"/a/x.txt"}, true},
		{"one of several extensions", ".pdf,.png,.csv", []string{"/a/x.png"}, false},
		{"mime type matches", "application/pdf", []string{"/a/x.pdf"}, false},
		{"mime wildcard matches", "image/*", []string{"/a/x.png"}, false},
		{"mime wildcard does not match", "image/*", []string{"/a/x.txt"}, true},
		{"any-file wildcard", "*/*", []string{"/a/x.txt"}, false},
		{"one bad file among good ones", ".txt", []string{"/a/x.txt", "/a/y.pdf"}, true},
		{"extension-less file with a strict accept", ".txt", []string{"/a/README"}, true},
		{"whitespace in the list is tolerated", " .pdf , .txt ", []string{"/a/x.txt"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := acceptMismatch(c.accept, c.paths); got != c.want {
				t.Errorf("acceptMismatch(%q, %v) = %v, want %v", c.accept, c.paths, got, c.want)
			}
		})
	}
}

// The wrong-element error has to name what was actually found, because that
// name is the whole remedy: the caller fixes the selector instead of retrying.
func TestFileInputStateDescribe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state fileInputState
		want  string
	}{
		{fileInputState{Tag: "input", Type: "text"}, "input[type=text]"},
		{fileInputState{Tag: "input", Type: "file"}, "input[type=file]"},
		{fileInputState{Tag: "div"}, "div"},
		{fileInputState{}, "a non-element node"},
	}
	for _, c := range cases {
		if got := c.state.describe(); got != c.want {
			t.Errorf("describe(%+v) = %q, want %q", c.state, got, c.want)
		}
	}
	if (fileInputState{Tag: "input", Type: "file"}).isFileInput() != true {
		t.Error("an <input type=file> must be recognised as a file input")
	}
	if (fileInputState{Tag: "input", Type: "text"}).isFileInput() {
		t.Error("an <input type=text> must not be recognised as a file input")
	}
}

// IsUploadUsage has to keep working after the error has crossed the daemon's
// RPC boundary, where it is rebuilt from its message and has lost its Go type —
// otherwise "wrong element" degrades to exit 4 under the DEFAULT connection
// path and an agent retries a doomed action.
func TestIsUploadUsageSurvivesTheRPCBoundary(t *testing.T) {
	t.Parallel()
	for _, sentinel := range []error{ErrNotFileInput, ErrNotMultiple, ErrAppendUnknown} {
		wrapped := errors.New(sentinel.Error() + ": selector \"#x\"")
		if !IsUploadUsage(wrapped) {
			t.Errorf("IsUploadUsage(%q) = false, want true (message-matched across the RPC)", wrapped)
		}
	}
	if IsUploadUsage(nil) {
		t.Error("IsUploadUsage(nil) = true, want false")
	}
	if IsUploadUsage(errors.New("context deadline exceeded")) {
		t.Error("a timeout must not be classified as a usage error")
	}
}

const uploadFixture = `<!doctype html><title>Upload</title>
<style> #hidden { display: none } </style>
<body>
<input type="file" id="f">
<input type="file" id="multi" multiple>
<input type="file" id="hidden">
<input type="file" id="acc" accept=".pdf">
<input type="file" id="clearing">
<input type="text" id="t" value="not a file input">
<div id="zone">Drop zone</div>
<script>
window.__log = [];
for (const id of ["f", "multi", "hidden", "acc", "clearing"]) {
  const el = document.getElementById(id);
  el.addEventListener("change", e => {
    window.__log.push({type: "change", id: id, names: [...e.target.files].map(f => f.name)});
    // The uploader that empties the input in its own change handler: the
    // envelope must report what the DOM holds afterwards, not what was sent.
    if (id === "clearing") e.target.value = "";
  });
}
</script>
</body>`

// uploadFile writes a fixture file into the test's temp dir and returns its path.
func uploadFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return p
}

// uploadNames reads the file names the page currently holds on an input.
func uploadNames(ctx context.Context, t *testing.T, b *CDP, id, sel string) []string {
	t.Helper()
	v := pointerEval(ctx, t, b, id,
		`[...document.querySelector("`+sel+`").files].map(f => f.name)`)
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, _ := it.(string)
		out = append(out, s)
	}
	return out
}

func uploadResultNames(t *testing.T, res map[string]any) []string {
	t.Helper()
	files, ok := res["files"].([]any)
	if !ok {
		t.Fatalf("result.files = %v, want a list", res["files"])
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		m, ok := f.(map[string]any)
		if !ok {
			t.Fatalf("result.files entry = %v, want an object", f)
		}
		out = append(out, m["name"].(string))
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The whole verb against a real renderer: VS-1 (a file lands and the page sees
// the change), VS-2 (several files, in order), VS-3 (too many for a single
// input, and the input is left untouched), VS-4 (a display:none input works),
// VS-5 (wrong element type names the element), VS-11 (the envelope reports the
// post-call DOM, not the arguments), VS-12 (an accept mismatch is a warning).
func TestUploadDrivesRealFileInputs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	dir := t.TempDir()
	a := uploadFile(t, dir, "a.txt", "alpha")
	bb := uploadFile(t, dir, "b.txt", "bravo")
	cc := uploadFile(t, dir, "c.txt", "charlie")

	srv := pointerFixture(t, uploadFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	ready := QueryOpts{Wait: "ready"} // the CLI's per-verb default

	// VS-1: one file lands, the envelope reports it read back from the input,
	// and the page's own change handler recorded it.
	pointerClearLog(ctx, t, b, id)
	res, err := b.Upload(ctx, id, "#f", []string{a}, UploadOpts{Query: ready})
	if err != nil {
		t.Fatalf("upload one file: %v", err)
	}
	if got := uploadResultNames(t, res); !equalStrings(got, []string{"a.txt"}) {
		t.Errorf("result files = %v, want [a.txt]", got)
	}
	if res["count"] != 1 || res["change_fired"] != true || res["multiple"] != false {
		t.Errorf("result = %v, want count 1, change_fired true, multiple false", res)
	}
	if _, ok := res["accept_mismatch"]; ok {
		t.Errorf("result = %v, want no accept_mismatch on an input with no accept", res)
	}
	log := pointerLog(ctx, t, b, id)
	if len(log) != 1 || log[0]["id"] != "f" {
		t.Fatalf("page change log = %v, want exactly one change on #f", log)
	}
	if names, _ := log[0]["names"].([]any); len(names) != 1 || names[0] != "a.txt" {
		t.Errorf("the change event carried %v, want [a.txt]", log[0]["names"])
	}

	// VS-2: several files reach a `multiple` input, in argument order.
	pointerClearLog(ctx, t, b, id)
	res, err = b.Upload(ctx, id, "#multi", []string{a, bb, cc}, UploadOpts{Query: ready})
	if err != nil {
		t.Fatalf("upload three files: %v", err)
	}
	want := []string{"a.txt", "b.txt", "c.txt"}
	if got := uploadResultNames(t, res); !equalStrings(got, want) {
		t.Errorf("result files = %v, want %v", got, want)
	}
	if res["count"] != 3 || res["multiple"] != true {
		t.Errorf("result = %v, want count 3 and multiple true", res)
	}
	if got := uploadNames(ctx, t, b, id, "#multi"); !equalStrings(got, want) {
		t.Errorf("page files = %v, want %v", got, want)
	}

	// VS-3: two files at a non-`multiple` input is the caller's bug, and the
	// input must be left exactly as it was — a partial upload would be worse
	// than the refusal.
	pointerClearLog(ctx, t, b, id)
	before := uploadNames(ctx, t, b, id, "#f")
	_, err = b.Upload(ctx, id, "#f", []string{a, bb}, UploadOpts{Query: ready})
	if err == nil {
		t.Fatal("two files at a non-multiple input succeeded, want a refusal")
	}
	if !IsUploadUsage(err) {
		t.Errorf("err = %v, want one IsUploadUsage recognises (usage, not a timeout)", err)
	}
	if got := uploadNames(ctx, t, b, id, "#f"); !equalStrings(got, before) {
		t.Errorf("the refused upload changed the input: %v -> %v", before, got)
	}
	if log := pointerLog(ctx, t, b, id); len(log) != 0 {
		t.Errorf("the refused upload fired %v, want no change event", log)
	}

	// VS-4: the real input behind a styled drop zone is display:none. That is
	// the common case, which is why --wait defaults to `ready` for this verb.
	if _, err := b.Upload(ctx, id, "#hidden", []string{a}, UploadOpts{Query: ready}); err != nil {
		t.Fatalf("upload to a display:none input: %v", err)
	}
	// ...and the reason the default had to change: waiting for visibility fails.
	visCtx, visCancel := context.WithTimeout(ctx, 3*time.Second)
	defer visCancel()
	if _, err := b.Upload(visCtx, id, "#hidden", []string{a}, UploadOpts{Query: QueryOpts{Wait: "visible"}}); err == nil {
		t.Error("--wait visible on a display:none input succeeded, want a timeout (the reason `ready` is this verb's default)")
	}

	// VS-5: a resolved element of the wrong type names itself, and is a usage
	// failure rather than a timeout — retrying will never fix a text input.
	_, err = b.Upload(ctx, id, "#t", []string{a}, UploadOpts{Query: ready})
	if err == nil {
		t.Fatal("upload onto a text input succeeded, want a refusal")
	}
	if !IsUploadUsage(err) {
		t.Errorf("err = %v, want one IsUploadUsage recognises", err)
	}
	if !strings.Contains(err.Error(), "input[type=text]") {
		t.Errorf("err = %v, want it to name input[type=text]", err)
	}
	_, err = b.Upload(ctx, id, "#zone", []string{a}, UploadOpts{Query: ready})
	if err == nil || !strings.Contains(err.Error(), "div") {
		t.Errorf("upload onto a drop-zone div: err = %v, want one naming div", err)
	}

	// VS-11: the envelope is evidence, not an echo. This input's change handler
	// clears itself, so the honest report is zero files even though one was sent.
	pointerClearLog(ctx, t, b, id)
	res, err = b.Upload(ctx, id, "#clearing", []string{a}, UploadOpts{Query: ready})
	if err != nil {
		t.Fatalf("upload to the self-clearing input: %v", err)
	}
	if res["count"] != 0 || len(uploadResultNames(t, res)) != 0 {
		t.Errorf("result = %v, want count 0 read back from the cleared input", res)
	}
	if res["change_fired"] != true {
		t.Errorf("change_fired = %v, want true (the handler that cleared it ran)", res["change_fired"])
	}

	// VS-12: an accept mismatch is surfaced and NOT fatal — accept is advisory
	// in HTML, and apps set it loosely.
	res, err = b.Upload(ctx, id, "#acc", []string{a}, UploadOpts{Query: ready})
	if err != nil {
		t.Fatalf("upload a .txt to an accept=\".pdf\" input: %v", err)
	}
	if res["accept"] != ".pdf" || res["accept_mismatch"] != true || res["count"] != 1 {
		t.Errorf("result = %v, want accept .pdf, accept_mismatch true, count 1", res)
	}
}

// --append can only be honoured for files this session set itself: CDP replaces
// the FileList wholesale and the DOM will not hand back the existing entries'
// paths. Both halves of that bargain are tested — the append that works, and
// the refusal that is the honest answer when the prior contents are unknown.
func TestUploadAppendTracksOnlyWhatThisSessionSet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	dir := t.TempDir()
	a := uploadFile(t, dir, "a.txt", "alpha")
	bb := uploadFile(t, dir, "b.txt", "bravo")

	srv := pointerFixture(t, uploadFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	ready := QueryOpts{Wait: "ready"}

	// Appending to an input this session never set is refused, not guessed.
	_, err = b.Upload(ctx, id, "#multi", []string{a}, UploadOpts{Append: true, Query: ready})
	if err == nil {
		t.Fatal("append onto an untouched input succeeded, want a refusal")
	}
	if !IsUploadUsage(err) {
		t.Errorf("err = %v, want a usage-class refusal", err)
	}

	// After this session sets the files, an append is honest and works.
	if _, err := b.Upload(ctx, id, "#multi", []string{a}, UploadOpts{Query: ready}); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	res, err := b.Upload(ctx, id, "#multi", []string{bb}, UploadOpts{Append: true, Query: ready})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	want := []string{"a.txt", "b.txt"}
	if got := uploadResultNames(t, res); !equalStrings(got, want) {
		t.Errorf("appended files = %v, want %v", got, want)
	}
	if got := uploadNames(ctx, t, b, id, "#multi"); !equalStrings(got, want) {
		t.Errorf("page files after append = %v, want %v", got, want)
	}

	// A reload resets the input and mints a new node, so the tracked state no
	// longer applies — and the verb says so rather than re-sending stale paths.
	if _, err := b.Reload(ctx, id, false); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, err := b.Upload(ctx, id, "#multi", []string{bb}, UploadOpts{Append: true, Query: ready}); err == nil {
		t.Error("append after a reload succeeded, want a refusal: the page's files are no longer ours")
	}
}

// VS-14: no native dialog is ever opened, and the connection stays responsive.
//
// A native OS file dialog is by construction unobservable from CDP — that is
// precisely why the verb must never click a file input. So this asserts the two
// things that ARE observable, and which a blocked main thread would break: no
// dialog event reaches the client, and the very next commands still answer
// quickly.
func TestUploadOpensNoDialogAndStaysResponsive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := pointerFixture(t, uploadFixture)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	tctx, err := b.on(id)
	if err != nil {
		t.Fatalf("attach to the tab: %v", err)
	}
	var dialogs atomic.Int32
	chromedp.ListenTarget(tctx, func(ev any) {
		if _, ok := ev.(*page.EventJavascriptDialogOpening); ok {
			dialogs.Add(1)
		}
	})

	a := uploadFile(t, t.TempDir(), "a.txt", "alpha")
	if _, err := b.Upload(ctx, id, "#f", []string{a}, UploadOpts{Query: QueryOpts{Wait: "ready"}}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if n := dialogs.Load(); n != 0 {
		t.Errorf("%d dialog(s) opened during the upload, want 0", n)
	}

	// A blocked main thread would show up here as a timeout, not a slow test.
	nextCtx, nextCancel := context.WithTimeout(ctx, 5*time.Second)
	defer nextCancel()
	if got := pointerEval(nextCtx, t, b, id, "document.title"); got != "Upload" {
		t.Errorf("the next command after an upload returned %v, want the page title", got)
	}
	if _, err := b.Text(nextCtx, id, "#zone", TextOpts{Query: QueryOpts{Wait: "ready"}}); err != nil {
		t.Errorf("the connection was not responsive after the upload: %v", err)
	}
}
