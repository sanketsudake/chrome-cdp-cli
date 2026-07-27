package chrome

// Drop-zone upload (RFC-0014). The fixtures mirror the four shapes a real drop
// handler reads, because "it worked on my div" is not evidence that it works
// with the libraries people actually use.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dropFixture(t *testing.T) (*CDP, context.Context, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Drop</title><body style="margin:0">
<div id="zone" aria-label="Upload files" style="position:fixed;left:0;top:0;width:300px;height:200px">drop here</div>
<div id="inert" style="position:fixed;left:320px;top:0;width:200px;height:100px">no handler</div>
<script>
window.__got = null;
const z = document.getElementById("zone");
z.addEventListener("dragenter", e => e.preventDefault());
z.addEventListener("dragover", e => e.preventDefault());
z.addEventListener("drop", e => {
  e.preventDefault();
  const it = e.dataTransfer.items[0];
  window.__got = {
    files: [...e.dataTransfer.files].map(f => ({name: f.name, size: f.size, type: f.type})),
    items: [...e.dataTransfer.items].map(i => ({kind: i.kind, type: i.type})),
    types: [...e.dataTransfer.types],
    getAsFile: it && it.kind === "file" ? it.getAsFile().name : null,
  };
});
</script></body>`)
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.4 drop zone payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	return b, ctx, id, path
}

func dropReceived(t *testing.T, b *CDP, ctx context.Context, id string) map[string]any {
	t.Helper()
	v, err := b.Eval(ctx, id, "window.__got", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	got, _ := v.(map[string]any)["value"].(map[string]any)
	return got
}

// VS-9: the drop delivers real files, in every shape a handler might read.
func TestUploadDropDeliversRealFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id, path := dropFixture(t)

	res, err := b.Upload(ctx, id, "", []string{path}, UploadOpts{Drop: "#zone"})
	if err != nil {
		t.Fatalf("upload --drop: %v", err)
	}
	if res["mode"] != "drop" || res["count"] != 1 {
		t.Errorf("result = %v", res)
	}
	if res["drop_handled"] != true {
		t.Errorf("a handled drop was reported unhandled: %v", res)
	}

	got := dropReceived(t, b, ctx, id)
	if got == nil {
		t.Fatal("the drop handler never fired")
	}
	files, _ := got["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("dataTransfer.files = %v, want one file", got["files"])
	}
	f := files[0].(map[string]any)
	if f["name"] != "report.pdf" {
		t.Errorf("file name = %v", f["name"])
	}
	// The size proves the bytes are real, not a stub File.
	if size, _ := f["size"].(float64); int(size) != len("%PDF-1.4 drop zone payload") {
		t.Errorf("file size = %v, want the real byte count", f["size"])
	}
	// The three other shapes a drop handler may read.
	items, _ := got["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["kind"] != "file" {
		t.Errorf("dataTransfer.items = %v, want one file item", got["items"])
	}
	types, _ := got["types"].([]any)
	var sawFiles bool
	for _, ty := range types {
		if ty == "Files" {
			sawFiles = true
		}
	}
	if !sawFiles {
		t.Errorf("dataTransfer.types = %v, want it to include Files (the guard most libraries gate on)", types)
	}
	if got["getAsFile"] != "report.pdf" {
		t.Errorf("items[0].getAsFile() = %v", got["getAsFile"])
	}
}

// The temporary input and the marker attribute must not survive the call: this
// is the user's real page, and debris in it would outlive the automation.
func TestUploadDropLeavesNoTrace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id, path := dropFixture(t)

	if _, err := b.Upload(ctx, id, "", []string{path}, UploadOpts{Drop: "#zone"}); err != nil {
		t.Fatalf("upload --drop: %v", err)
	}
	assertPageClean(t, b, ctx, id)
}

// A drop nothing consumed is reported, not silently called success.
func TestUploadDropReportsUnhandled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id, path := dropFixture(t)

	res, err := b.Upload(ctx, id, "", []string{path}, UploadOpts{Drop: "#inert"})
	if err != nil {
		t.Fatalf("upload --drop on an inert target errored instead of reporting: %v", err)
	}
	if res["drop_handled"] != false {
		t.Errorf("an unhandled drop was reported as handled: %v", res)
	}
	if note, _ := res["note"].(string); !strings.Contains(note, "did not consume") {
		t.Errorf("no explanatory note on an unhandled drop: %v", res)
	}
}

// The coordinate form drops at a point, for a zone with no stable selector.
func TestUploadDropAtCoordinate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id, path := dropFixture(t)

	res, err := b.Upload(ctx, id, "", []string{path}, UploadOpts{DropAt: &Point{X: 150, Y: 100}})
	if err != nil {
		t.Fatalf("upload --drop-at: %v", err)
	}
	if res["drop_handled"] != true {
		t.Errorf("the coordinate drop missed the zone: %v", res)
	}
	if got := dropReceived(t, b, ctx, id); got == nil {
		t.Error("the drop handler never fired for the coordinate form")
	}
	raw, _ := json.Marshal(res["dropped_on"])
	if !strings.Contains(string(raw), "Upload files") {
		t.Errorf("dropped_on = %s, want the zone it landed on", raw)
	}
}

// An out-of-viewport drop coordinate is refused like any other.
func TestUploadDropAtRejectsOutOfViewport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id, path := dropFixture(t)

	_, err := b.Upload(ctx, id, "", []string{path}, UploadOpts{DropAt: &Point{X: 99999, Y: 10}})
	if err == nil {
		t.Fatal("a drop coordinate outside the viewport was accepted")
	}
	if !IsCoordinateOOB(err) {
		t.Errorf("error %v is not classified as out-of-bounds", err)
	}
}

// Cleanup is guaranteed on every exit, not just the happy one.
//
// The page-side `finally` this replaced could only run when the JS ran at all —
// so a failure before or during dispatch left an injected file input, holding
// the user's real files, in the page it was driving. The cleanup is now a Go
// defer, and this pins the failing path.
func TestUploadDropCleansUpAfterFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id, path := dropFixture(t)

	// A selector that never resolves auto-waits, so bound it: the point is the
	// exit path, not the wait.
	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := b.Upload(short, id, "", []string{path}, UploadOpts{Drop: "#nonexistent"}); err == nil {
		t.Fatal("a drop onto a selector that resolves to nothing should fail")
	}
	assertPageClean(t, b, ctx, id)

	// And a target that resolves but whose drop nothing handles still cleans up.
	if _, err := b.Upload(ctx, id, "", []string{path}, UploadOpts{Drop: "#inert"}); err != nil {
		t.Fatalf("unhandled drop: %v", err)
	}
	assertPageClean(t, b, ctx, id)
}

// The page is never mutated, so there is nothing a failed run could leave
// behind to misdirect a later drop — no injected element, no marker attribute.
// The old design wrote a marker onto the caller's own element; this pins that
// it is gone for good.
func TestUploadDropNeverMutatesThePage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id, path := dropFixture(t)

	before := domFingerprint(t, b, ctx, id)
	res, err := b.Upload(ctx, id, "", []string{path}, UploadOpts{Drop: "#zone"})
	if err != nil {
		t.Fatalf("upload --drop: %v", err)
	}
	if res["drop_handled"] != true {
		t.Fatalf("the drop was not handled: %v", res)
	}
	if after := domFingerprint(t, b, ctx, id); after != before {
		t.Errorf("the page changed:\n before %s\n after  %s", before, after)
	}
}

// A page-global `change` listener must never see the user's files.
//
// An ATTACHED input would fire `change` on setFileInputFiles, and `change`
// bubbles — so any script on the page (analytics, a compromised dependency, an
// XSS payload) would receive real file data that a native drag never exposes
// outside the drop target's own dispatch path. The input is detached precisely
// so those events reach no one.
func TestUploadDropDoesNotLeakToPageListeners(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id, path := dropFixture(t)

	// The eavesdropper: exactly the generic hook a compromised script installs.
	if _, err := b.Eval(ctx, id, `(() => {
	  window.__stolen = [];
	  for (const ev of ["change", "input"]) {
	    document.addEventListener(ev, e => {
	      if (e.target && e.target.files) {
	        window.__stolen.push([...e.target.files].map(f => f.name));
	      }
	    }, true);
	  }
	  return true;
	})()`, EvalOpts{}); err != nil {
		t.Fatalf("Eval: %v", err)
	}

	if _, err := b.Upload(ctx, id, "", []string{path}, UploadOpts{Drop: "#zone"}); err != nil {
		t.Fatalf("upload --drop: %v", err)
	}

	// The intended target got the files...
	if got := dropReceived(t, b, ctx, id); got == nil {
		t.Fatal("the drop handler never fired")
	}
	// ...and nothing else did.
	v, err := b.Eval(ctx, id, "window.__stolen", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	stolen, _ := v.(map[string]any)["value"].([]any)
	if len(stolen) != 0 {
		t.Errorf("a page-global listener saw the user's files: %v", stolen)
	}
}

// domFingerprint captures enough of the page to notice anything this verb
// might have left behind or altered.
func domFingerprint(t *testing.T, b *CDP, ctx context.Context, id string) string {
	t.Helper()
	v, err := b.Eval(ctx, id, `(() => {
	  const attrs = [];
	  for (const el of document.querySelectorAll("*")) {
	    attrs.push(el.tagName + "#" + (el.id || "") + "[" + [...el.attributes].map(a => a.name).sort().join(",") + "]");
	  }
	  return attrs.join("|");
	})()`, EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	s, _ := v.(map[string]any)["value"].(string)
	return s
}

func assertPageClean(t *testing.T, b *CDP, ctx context.Context, id string) {
	t.Helper()
	v, err := b.Eval(ctx, id, `({
	  inputs: document.querySelectorAll("input[type=file]").length,
	  markers: document.querySelectorAll("[data-chrome-cdp-drop]").length,
	})`, EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	got, _ := v.(map[string]any)["value"].(map[string]any)
	if got["inputs"] != 0.0 || got["markers"] != 0.0 {
		t.Errorf("the page kept debris: %v", got)
	}
}

// Several files in one drop, and a target addressed the way the drop form's
// design exists for: an accessible name, which has no CSS spelling.
func TestUploadDropMultipleFilesByName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id, _ := dropFixture(t)

	dir := t.TempDir()
	var paths []string
	for _, n := range []string{"a.pdf", "b.png", "c.csv"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("payload-"+n), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	// --by name addresses the zone by its accessible name ("Upload files").
	res, err := b.Upload(ctx, id, "", paths, UploadOpts{
		Drop:  "Upload files",
		Query: QueryOpts{By: "name"},
	})
	if err != nil {
		t.Fatalf("upload --drop --by name: %v", err)
	}
	if res["count"] != 3 {
		t.Errorf("count = %v, want 3", res["count"])
	}
	if res["drop_handled"] != true {
		t.Errorf("the named target did not receive the drop: %v", res)
	}

	got := dropReceived(t, b, ctx, id)
	files, _ := got["files"].([]any)
	if len(files) != 3 {
		t.Fatalf("the handler saw %v files, want 3", len(files))
	}
	names := map[string]bool{}
	for _, f := range files {
		names[f.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"a.pdf", "b.png", "c.csv"} {
		if !names[want] {
			t.Errorf("%s did not arrive: %v", want, names)
		}
	}
}
