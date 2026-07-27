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
	v, err := b.Eval(ctx, id, `({
	  inputs: document.querySelectorAll("input[type=file]").length,
	  markers: document.querySelectorAll("[data-chrome-cdp-drop]").length,
	})`, EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	got, _ := v.(map[string]any)["value"].(map[string]any)
	if got["inputs"] != 0.0 || got["markers"] != 0.0 {
		t.Errorf("the page kept debris after the drop: %v", got)
	}
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
