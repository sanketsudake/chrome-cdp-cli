package cli

// The drop form's CLI contract (RFC-0014): it takes no selector, every
// positional is a path, and the upload_roots allow-list applies exactly as it
// does to a file input — a drop is still a file leaving the machine.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

type dropCapture struct {
	fakeBrowser
	selector string
	paths    []string
	opts     chrome.UploadOpts
}

func (d *dropCapture) Upload(_ context.Context, _, selector string, paths []string, opts chrome.UploadOpts) (map[string]any, error) {
	d.selector, d.paths, d.opts = selector, paths, opts
	return map[string]any{"mode": "drop", "count": len(paths), "drop_handled": true}, nil
}

func dropFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.4"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUploadDropCLIForm(t *testing.T) {
	t.Parallel()
	path := dropFile(t)

	t.Run("every positional is a path, none is a selector", func(t *testing.T) {
		t.Parallel()
		b := &dropCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "t1", Title: "T", URL: "u"}}}}
		env, _, code := run(t, b, "upload", "--drop", "#zone", path, "--target", "t1", "--json")
		if code != 0 {
			t.Fatalf("exit = %d (env %v)", code, env)
		}
		if b.opts.Drop != "#zone" {
			t.Errorf("Drop = %q", b.opts.Drop)
		}
		if b.selector != "" {
			t.Errorf("a selector reached the driver in the drop form: %q", b.selector)
		}
		if len(b.paths) != 1 {
			t.Errorf("paths = %v, want just the file", b.paths)
		}
	})

	t.Run("drop-at parses to a point", func(t *testing.T) {
		t.Parallel()
		b := &dropCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "t1", Title: "T", URL: "u"}}}}
		if _, _, code := run(t, b, "upload", "--drop-at", "400,300", path, "--target", "t1", "--json"); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if b.opts.DropAt == nil || b.opts.DropAt.X != 400 {
			t.Errorf("DropAt = %v", b.opts.DropAt)
		}
	})

	t.Run("malformed combinations never connect", func(t *testing.T) {
		t.Parallel()
		cases := map[string][]string{
			"drop and drop-at":  {"upload", "--drop", "#z", "--drop-at", "1,2", path},
			"drop with no path": {"upload", "--drop", "#z"},
			"bad drop-at":       {"upload", "--drop-at", "4;3", path},
			"drop with append":  {"upload", "--drop", "#z", "--append", path},
		}
		for name, args := range cases {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				env, _, code := run(t, noCall(t), append(args, "--json")...)
				if code != 2 {
					t.Fatalf("exit = %d, want 2 (env %v)", code, env)
				}
			})
		}
	})
}

// A drop is still a file leaving the machine, so upload_roots bounds it the
// same way it bounds a file input — checked before Chrome is contacted.
func TestUploadDropHonoursUploadRoots(t *testing.T) {
	t.Parallel()
	outside := dropFile(t)
	pol := config.Policy{Present: true, Enabled: true, UploadRoots: []string{t.TempDir()}, Source: "test"}
	b := &dropCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "T", URL: "https://example.com/"}}}}

	env, _, code := runPolicy(t, b, pol, "upload", "--drop", "#zone", outside, "--target", "aa11", "--json")
	if code != result.ExitPermission {
		t.Fatalf("exit = %d, want %d — a drop outside upload_roots must be refused", code, result.ExitPermission)
	}
	if got := env["error"].(map[string]any)["code"]; got != "permission_denied" {
		t.Errorf("error.code = %v", got)
	}
	if b.paths != nil {
		t.Error("the driver was reached despite the refusal")
	}
}

// The target-form rules as one table, now that they live in one pure function.
func TestUploadTargetForm(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args            []string
		drop, dropAt    string
		appendFiles     bool
		wantSelector    string
		wantPaths       []string
		wantPoint       bool
		wantMsgFragment string
	}{
		"input form": {
			args: []string{"#in", "a.pdf", "b.pdf"}, wantSelector: "#in", wantPaths: []string{"a.pdf", "b.pdf"},
		},
		"drop form takes every positional as a path": {
			args: []string{"a.pdf", "b.pdf"}, drop: "#zone", wantPaths: []string{"a.pdf", "b.pdf"},
		},
		"drop-at parses a point": {
			args: []string{"a.pdf"}, dropAt: "400,300", wantPaths: []string{"a.pdf"}, wantPoint: true,
		},
		"both drop forms":     {args: []string{"a.pdf"}, drop: "#z", dropAt: "1,2", wantMsgFragment: "not both"},
		"drop with append":    {args: []string{"a.pdf"}, drop: "#z", appendFiles: true, wantMsgFragment: "--append applies"},
		"drop with no path":   {drop: "#z", wantMsgFragment: "no paths given"},
		"input form no args":  {wantMsgFragment: "needs a selector"},
		"input form only sel": {args: []string{"#in"}, wantMsgFragment: "no paths given"},
		"bad drop-at":         {args: []string{"a.pdf"}, dropAt: "4;3", wantMsgFragment: "--drop-at"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sel, paths, at, msg := uploadTargetForm(tc.args, tc.drop, tc.dropAt, tc.appendFiles)
			if tc.wantMsgFragment != "" {
				if !strings.Contains(msg, tc.wantMsgFragment) {
					t.Fatalf("msg = %q, want it to mention %q", msg, tc.wantMsgFragment)
				}
				return
			}
			if msg != "" {
				t.Fatalf("unexpected usage error: %q", msg)
			}
			if sel != tc.wantSelector {
				t.Errorf("selector = %q, want %q", sel, tc.wantSelector)
			}
			if len(paths) != len(tc.wantPaths) {
				t.Errorf("paths = %v, want %v", paths, tc.wantPaths)
			}
			if (at != nil) != tc.wantPoint {
				t.Errorf("point = %v, wantPoint = %v", at, tc.wantPoint)
			}
		})
	}
}
