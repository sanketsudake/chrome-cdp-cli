package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shot.png")

	// free path returns unchanged
	if got := uniquePath(p); got != p {
		t.Fatalf("free path: got %q, want %q", got, p)
	}

	// on collision, a counter is inserted before the extension
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := uniquePath(p)
	if want := filepath.Join(dir, "shot-1.png"); got != want {
		t.Errorf("first collision: got %q, want %q", got, want)
	}

	if err := os.WriteFile(got, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got2, want := uniquePath(p), filepath.Join(dir, "shot-2.png"); got2 != want {
		t.Errorf("second collision: got %q, want %q", got2, want)
	}
}

func TestScreenshotToFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.png")
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	env, _, code := run(t, b, "screenshot", "-o", p, "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if env["result"].(map[string]any)["path"] != p {
		t.Errorf("result.path = %v, want %q", env["result"], p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("file not written: %v", err)
	}
}

func TestScreenshotToStdout(t *testing.T) {
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	if code := app.Execute("screenshot", "-o", "-", "--target", "aa11"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out.String() != "PNGDATA" {
		t.Errorf("stdout = %q, want the raw PNG bytes", out.String())
	}
}
