package cli

// Stub-backed tests for the `upload` verb (RFC-0006).
//
// The theme is that everything about a path is decided BEFORE Chrome exists:
// each invalid invocation runs against a browser that fails the test if it is
// touched at all, which is what makes exit 2 / exit 7 mean "your call was
// wrong" rather than "we asked Chrome and it said no".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// uploadCapture records what Upload was called with and answers like a real
// file input would.
type uploadCapture struct {
	fakeBrowser
	gotSelector string
	gotPaths    []string
	gotOpts     chrome.UploadOpts
	err         error
}

func (u *uploadCapture) Upload(_ context.Context, _, selector string, paths []string, opts chrome.UploadOpts) (map[string]any, error) {
	u.gotSelector, u.gotPaths, u.gotOpts = selector, paths, opts
	if u.err != nil {
		return nil, u.err
	}
	files := make([]any, 0, len(paths))
	for _, p := range paths {
		files = append(files, map[string]any{"name": filepath.Base(p), "size": 5, "type": "text/plain"})
	}
	return map[string]any{
		"files": files, "count": len(files),
		"multiple": true, "accept": "", "change_fired": true,
	}, nil
}

// The tab carries a real URL rather than the "u" placeholder the older fixtures
// use: an active [policy] table now refuses a verb on a tab whose origin it
// cannot identify, so a placeholder URL would make these tests exercise that
// rule instead of upload_roots.
func newUploadCapture() *uploadCapture {
	return &uploadCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "https://app.example.com/form"}}}}
}

// noCallUpload is noCall plus a guard on the verb's own driver method, so a
// rejected invocation cannot even reach a browser that was handed in directly.
type noCallUpload struct {
	noCallBrowser
}

func (b *noCallUpload) Upload(context.Context, string, string, []string, chrome.UploadOpts) (map[string]any, error) {
	b.t.Fatal("Upload was dispatched for an invocation that should have failed validation first")
	return nil, nil
}

func noCallUploadBrowser(t *testing.T) *noCallUpload {
	t.Helper()
	return &noCallUpload{noCallBrowser: noCallBrowser{t: t}}
}

func writeUploadFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// VS-6, VS-7, plus the empty-argument cases: every bad path is usage / exit 2,
// names the path, and never reaches the browser.
func TestUploadBadPathsAreUsageAndNeverConnect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := writeUploadFile(t, dir, "a.txt")
	missing := filepath.Join(dir, "nope.txt")

	cases := []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{"no arguments at all", []string{"upload"}, "selector"},
		{"selector but no path", []string{"upload", "#f"}, "no paths given"},
		{"missing file", []string{"upload", "#f", missing}, "nope.txt"},
		{"a directory is not a file", []string{"upload", "#f", dir}, "is a directory"},
		{"one bad path among good ones", []string{"upload", "#f", good, missing}, "nope.txt"},
		{"empty path", []string{"upload", "#f", "  "}, "empty file path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env, _, code := run(t, noCallUploadBrowser(t), append(c.args, "--target", "aa11", "--json")...)
			if code != result.ExitUsage {
				t.Fatalf("exit = %d, want %d (usage); envelope %v", code, result.ExitUsage, env)
			}
			e := env["error"].(map[string]any)
			if e["code"] != result.CodeUsage {
				t.Errorf("error.code = %v, want usage", e["code"])
			}
			if !strings.Contains(e["message"].(string), c.wantMsg) {
				t.Errorf("message = %q, want it to mention %q", e["message"], c.wantMsg)
			}
			if env["command"] != "upload" {
				t.Errorf("command = %v, want upload", env["command"])
			}
		})
	}
}

// An unreadable file fails here rather than inside Chrome, where the error
// would surface far from the argument that caused it.
func TestUploadUnreadableFileIsUsage(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("os.Chmod does not restrict read access on Windows, so the permission bit proves nothing")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: the permission bits would not be enforced")
	}
	p := writeUploadFile(t, t.TempDir(), "secret.txt")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

	env, _, code := run(t, noCallUploadBrowser(t), "upload", "#f", p, "--target", "aa11", "--json")
	if code != result.ExitUsage {
		t.Fatalf("exit = %d, want 2 (usage); envelope %v", code, env)
	}
	if got := env["error"].(map[string]any)["message"].(string); !strings.Contains(got, "cannot read") {
		t.Errorf("message = %q, want it to say the file cannot be read", got)
	}
}

// VS-8: a relative path reaches the driver as the absolute path CDP requires,
// still pointing at the same file.
func TestUploadResolvesRelativePathsToAbsolute(t *testing.T) {
	dir := t.TempDir()
	p := writeUploadFile(t, dir, "a.txt")
	t.Chdir(dir)

	b := newUploadCapture()
	env, _, code := run(t, b, "upload", "#f", "a.txt", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; envelope %v", code, env)
	}
	if len(b.gotPaths) != 1 || !filepath.IsAbs(b.gotPaths[0]) {
		t.Fatalf("driver received %v, want one absolute path", b.gotPaths)
	}
	sent, err := os.Stat(b.gotPaths[0])
	if err != nil {
		t.Fatalf("the resolved path does not exist: %v", err)
	}
	orig, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sent, orig) {
		t.Errorf("resolved %s, which is not the file the caller meant (%s)", b.gotPaths[0], p)
	}
	if files := env["result"].(map[string]any)["files"].([]any); len(files) != 1 {
		t.Errorf("result.files = %v, want the one uploaded file", files)
	}
}

// VS-8, the other half: `~` is expanded here, because the shell only does it
// for an unquoted argument and nothing does it for a configured value.
func TestUploadExpandsTilde(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	writeUploadFile(t, home, "a.txt")
	got, rerr := resolveUploadPaths([]string{"~/a.txt"}, nil, home)
	if rerr != nil {
		t.Fatalf("~/a.txt was refused: %v", rerr.Message)
	}
	if len(got) != 1 || got[0] != filepath.Join(home, "a.txt") {
		t.Errorf("resolved %v, want %s", got, filepath.Join(home, "a.txt"))
	}
	// A `~user` form is NOT a tilde expansion this expands; it must fail as a
	// missing path rather than being silently rewritten to something else.
	if _, rerr := resolveUploadPaths([]string{"~someone/a.txt"}, nil, home); rerr == nil {
		t.Error("~someone/a.txt was accepted, want a usage refusal")
	}
}

// uploadRootsFixture builds a temp tree for the allow-list tests:
//
//	<tmp>/allowed/ok.txt          inside the root
//	<tmp>/allowed/sub/deep.txt    inside a subdirectory of the root
//	<tmp>/secret.txt              outside it
//	<tmp>/allowed/escape.txt   -> <tmp>/secret.txt        (symlink out)
//	<tmp>/allowed/inside.txt   -> <tmp>/allowed/ok.txt    (symlink in)
//	<tmp>/allowed/outdir       -> <tmp>/elsewhere         (directory symlink out)
//	<tmp>/allowed-evil/loot.txt   a sibling sharing the root's prefix
func uploadRootsFixture(t *testing.T) (root string, paths map[string]string) {
	t.Helper()
	tmp := t.TempDir()
	root = filepath.Join(tmp, "allowed")
	for _, d := range []string{root, filepath.Join(root, "sub"), filepath.Join(tmp, "elsewhere"), filepath.Join(tmp, "allowed-evil")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths = map[string]string{
		"inside":   writeUploadFile(t, root, "ok.txt"),
		"deep":     writeUploadFile(t, filepath.Join(root, "sub"), "deep.txt"),
		"secret":   writeUploadFile(t, tmp, "secret.txt"),
		"elsewhrd": writeUploadFile(t, filepath.Join(tmp, "elsewhere"), "far.txt"),
		"sibling":  writeUploadFile(t, filepath.Join(tmp, "allowed-evil"), "loot.txt"),
	}
	if err := os.Symlink(paths["secret"], filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(paths["inside"], filepath.Join(root, "inside.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "elsewhere"), filepath.Join(root, "outdir")); err != nil {
		t.Fatal(err)
	}
	paths["tmp"] = tmp
	paths["escapeLink"] = filepath.Join(root, "escape.txt")
	paths["insideLink"] = filepath.Join(root, "inside.txt")
	paths["outdirLink"] = filepath.Join(root, "outdir", "far.txt")
	paths["traversal"] = filepath.Join(root, "..", "secret.txt")
	paths["traversalDeep"] = filepath.Join(root, "sub", "..", "..", "secret.txt")
	return root, paths
}

// VS-9 and VS-10, the security-relevant table: upload_roots has to hold up
// against the paths an attacker (or a confused agent) would actually try, not
// just against the happy path. Each denial is exit 7 / permission_denied.
func TestUploadRootsResistTraversalAndSymlinks(t *testing.T) {
	t.Parallel()
	root, p := uploadRootsFixture(t)
	roots := []string{root}

	cases := []struct {
		name  string
		path  string
		roots []string
		allow bool
	}{
		{"a file directly in the root", p["inside"], roots, true},
		{"a file in a subdirectory of the root", p["deep"], roots, true},
		{"a symlink inside the root pointing inside it", p["insideLink"], roots, true},
		{"the root spelled with a trailing separator", p["inside"], []string{root + string(os.PathSeparator)}, true},
		{"the root spelled with an interior dot segment", p["inside"], []string{filepath.Join(root, "sub", "..")}, true},
		{"no roots configured means unrestricted", p["secret"], nil, true},

		{"a file outside the root", p["secret"], roots, false},
		{"traversal out of the root", p["traversal"], roots, false},
		{"traversal out via a subdirectory", p["traversalDeep"], roots, false},
		{"a symlink inside the root escaping it", p["escapeLink"], roots, false},
		{"a file reached through a directory symlink out of the root", p["outdirLink"], roots, false},
		{"a sibling directory sharing the root's name prefix", p["sibling"], roots, false},
		{"a root that does not exist grants nothing", p["secret"], []string{filepath.Join(p["tmp"], "absent")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, rerr := resolveUploadPaths([]string{c.path}, c.roots, "")
			if c.allow {
				if rerr != nil {
					t.Fatalf("%s was refused (%s: %s), want it allowed", c.path, rerr.Code, rerr.Message)
				}
				if len(got) != 1 || !filepath.IsAbs(got[0]) {
					t.Errorf("resolved %v, want one absolute path", got)
				}
				return
			}
			if rerr == nil {
				t.Fatalf("%s was allowed, want permission_denied", c.path)
			}
			if rerr.Code != result.CodePermissionDenied {
				t.Errorf("error.code = %q, want %q", rerr.Code, result.CodePermissionDenied)
			}
			if result.ExitCodeFor(rerr.Code) != result.ExitPermission {
				t.Errorf("%s maps to exit %d, want %d", rerr.Code, result.ExitCodeFor(rerr.Code), result.ExitPermission)
			}
		})
	}
}

// A case-variant spelling of the root must never widen it. On a
// case-insensitive filesystem the file is reachable and the comparison refuses
// it; on a case-sensitive one it does not exist at all. Either way the answer
// is "no" — the point is that it is never "yes".
func TestUploadRootsRejectCaseVariantSpelling(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		// Windows filesystems are case-insensitive, so ALLOWED/ok.txt genuinely
		// IS inside root allowed — accepting it is correct there, not a bug this
		// test can pin. Case-sensitivity is exercised on darwin/linux instead.
		t.Skip("case-insensitive filesystem: a case-variant path is genuinely inside the root on windows")
	}
	root, p := uploadRootsFixture(t)
	variant := filepath.Join(filepath.Dir(root), "ALLOWED", filepath.Base(p["inside"]))
	if _, rerr := resolveUploadPaths([]string{variant}, []string{root}, ""); rerr == nil {
		t.Errorf("%s was accepted against root %s, want a refusal", variant, root)
	}
}

// runWithRoots runs the CLI with `upload_roots` configured, the way a [policy]
// table in the user's config file would.
func runWithRoots(t *testing.T, b chrome.Browser, roots []string, args ...string) (map[string]any, int) {
	t.Helper()
	d := config.Builtin()
	d.Policy = config.Policy{Present: true, Enabled: true, UploadRoots: roots, Source: "test"}
	var out, errb bytes.Buffer
	code := New(b, &out, &errb).WithDefaults(d).Execute(args...)
	var env map[string]any
	if s := strings.TrimSpace(out.String()); strings.HasPrefix(s, "{") {
		if err := json.Unmarshal([]byte(s), &env); err != nil {
			t.Fatalf("stdout is not one JSON value: %v\n%s", err, s)
		}
	}
	return env, code
}

// VS-9 end to end: the refusal is exit 7 with the typed code, and the browser
// is never contacted — the file is never even opened for a path we will not send.
func TestUploadOutsideRootsIsExitSevenAndNeverConnects(t *testing.T) {
	t.Parallel()
	root, p := uploadRootsFixture(t)

	env, code := runWithRoots(t, noCallUploadBrowser(t), []string{root},
		"upload", "#f", p["secret"], "--target", "aa11", "--json")
	if code != result.ExitPermission {
		t.Fatalf("exit = %d, want %d (permission); envelope %v", code, result.ExitPermission, env)
	}
	e := env["error"].(map[string]any)
	if e["code"] != result.CodePermissionDenied {
		t.Fatalf("error.code = %v, want permission_denied", e["code"])
	}
	if !strings.Contains(e["message"].(string), "upload_roots") {
		t.Errorf("message = %q, want it to name upload_roots", e["message"])
	}
	if _, ok := e["path"]; !ok {
		t.Errorf("error details = %v, want the refused path", e)
	}
}

// With the roots configured, a file inside them still uploads: the allow-list
// restricts, it does not disable the verb.
func TestUploadInsideRootsSucceeds(t *testing.T) {
	t.Parallel()
	root, p := uploadRootsFixture(t)

	b := newUploadCapture()
	env, code := runWithRoots(t, b, []string{root}, "upload", "#f", p["inside"], "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; envelope %v", code, env)
	}
	if len(b.gotPaths) != 1 {
		t.Errorf("driver received %v, want the one allowed path", b.gotPaths)
	}
}

// The result envelope is the public contract: the fields RFC-0006 specifies are
// passed through from the driver untouched, alongside the acted-on target.
func TestUploadEnvelopeShape(t *testing.T) {
	t.Parallel()
	p := writeUploadFile(t, t.TempDir(), "a.txt")
	b := newUploadCapture()
	env, _, code := run(t, b, "upload", "#f", p, "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; envelope %v", code, env)
	}
	if env["ok"] != true || env["command"] != "upload" {
		t.Fatalf("envelope = %v", env)
	}
	if env["target"].(map[string]any)["id"] != "aa11" {
		t.Errorf("target = %v, want the resolved tab", env["target"])
	}
	res := env["result"].(map[string]any)
	for _, k := range []string{"files", "count", "multiple", "accept", "change_fired"} {
		if _, ok := res[k]; !ok {
			t.Errorf("result is missing %q: %v", k, res)
		}
	}
	files := res["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["name"] != "a.txt" {
		t.Errorf("result.files = %v, want the uploaded file read back", files)
	}
}

// VS-3 and VS-5 at the command boundary: the driver failures that mean "fix
// your call" become exit 2, while a genuine timing failure stays exit 4. This
// mapping is the whole reason the verb classifies its own errors — an agent
// that cannot tell them apart retries a doomed action until it times out.
func TestUploadDriverErrorsMapToTheRightExitCode(t *testing.T) {
	t.Parallel()
	p := writeUploadFile(t, t.TempDir(), "a.txt")
	cases := []struct {
		name     string
		err      error
		wantCode string
		wantExit int
	}{
		{"wrong element type", fmt.Errorf("%w: selector %q resolved to input[type=text]", chrome.ErrNotFileInput, "#t"),
			result.CodeUsage, result.ExitUsage},
		{"too many files for the input", fmt.Errorf("%w: 2 files were given", chrome.ErrNotMultiple),
			result.CodeUsage, result.ExitUsage},
		{"append with unknown prior contents", fmt.Errorf("%w (selector %q)", chrome.ErrAppendUnknown, "#f"),
			result.CodeUsage, result.ExitUsage},
		{"selector never resolved", fmt.Errorf("context deadline exceeded"),
			result.CodeTargetTimeout, result.ExitTarget},
		{"the CDP call was rejected", fmt.Errorf("DOM.setFileInputFiles: rejected"),
			result.CodeCDP, result.ExitCDP},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			b := newUploadCapture()
			b.err = c.err
			env, _, code := run(t, b, "upload", "#f", p, "--target", "aa11", "--json")
			if code != c.wantExit {
				t.Fatalf("exit = %d, want %d; envelope %v", code, c.wantExit, env)
			}
			if got := env["error"].(map[string]any)["code"]; got != c.wantCode {
				t.Errorf("error.code = %v, want %v", got, c.wantCode)
			}
		})
	}
}

// The per-verb default: `ready` for upload, because the correct target is
// usually hidden behind a styled drop zone (US-2) — and only for upload, so the
// change cannot leak into the verbs whose visibility wait is load-bearing.
func TestUploadWaitDefaultsToReadyForThisVerbOnly(t *testing.T) {
	t.Parallel()
	p := writeUploadFile(t, t.TempDir(), "a.txt")

	b := newUploadCapture()
	if _, _, code := run(t, b, "upload", "#f", p, "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.gotOpts.Query.Wait != "ready" {
		t.Errorf("default --wait threaded as %q, want ready", b.gotOpts.Query.Wait)
	}

	// An explicit --wait still wins.
	b = newUploadCapture()
	if _, _, code := run(t, b, "upload", "#f", p, "--wait", "visible", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.gotOpts.Query.Wait != "visible" {
		t.Errorf("--wait visible threaded as %q, want visible", b.gotOpts.Query.Wait)
	}

	// Another verb keeps the global default.
	q := &queryCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}}
	if _, _, code := run(t, q, "click", "#x", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("click exit = %d, want 0", code)
	}
	if q.gotQ.Wait != "visible" {
		t.Errorf("click's --wait = %q, want the global default visible", q.gotQ.Wait)
	}
}

// --append threads to the driver, which is the only place that can decide
// whether it can be honoured.
func TestUploadAppendFlagThreads(t *testing.T) {
	t.Parallel()
	p := writeUploadFile(t, t.TempDir(), "a.txt")
	b := newUploadCapture()
	if _, _, code := run(t, b, "upload", "#f", p, "--append", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !b.gotOpts.Append {
		t.Error("--append did not thread to UploadOpts.Append")
	}
}

// The shared act-and-confirm flag composes, exactly as it does on click/fill.
func TestUploadComposesWithWaitText(t *testing.T) {
	t.Parallel()
	p := writeUploadFile(t, t.TempDir(), "a.txt")
	b := newUploadCapture()
	env, _, code := run(t, b, "upload", "#f", p, "--wait-text", "Uploaded", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; envelope %v", code, env)
	}
	if got := env["result"].(map[string]any)["waited_text"]; got != "Uploaded" {
		t.Errorf("result.waited_text = %v, want Uploaded", got)
	}
}

// VS-13: upload is an ordinary argv verb, so a session line drives it over the
// one held connection and emits one envelope per line, in order.
func TestUploadInsideSession(t *testing.T) {
	t.Parallel()
	p := writeUploadFile(t, t.TempDir(), "a.txt")
	b := newUploadCapture()
	in := strings.NewReader(
		`["upload","#f",` + strconv.Quote(p) + `,"--target","aa11"]` + "\n" +
			`["click","#submit","--target","aa11"]` + "\n")

	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithInput(in)
	if code := app.Execute("session"); code != 0 {
		t.Fatalf("session exit = %d, want 0\n%s", code, errb.String())
	}

	var commands []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("session line is not JSON: %q (%v)", line, err)
		}
		if e["ok"] != true {
			t.Errorf("session line %q is not ok: %v", line, e["error"])
		}
		commands = append(commands, e["command"].(string))
	}
	if len(commands) != 2 || commands[0] != "upload" || commands[1] != "click" {
		t.Errorf("session emitted %v, want [upload click] in order", commands)
	}
	if len(b.gotPaths) != 1 || b.gotPaths[0] != p {
		t.Errorf("the session's upload sent %v, want [%s]", b.gotPaths, p)
	}
}

// The allow-list comes from the config file's [policy] table and nowhere else.
// It is not a flag and not an environment variable, because either could be set
// by the very agent the roots exist to bound — and an unconfigured or disabled
// policy leaves the verb unrestricted, exactly as before the feature existed.
func TestUploadRootsComeFromThePolicyTableOnly(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		policy config.Policy
		want   int
	}{
		"no policy table at all":  {config.Policy{}, 0},
		"table present but off":   {config.Policy{Present: true, UploadRoots: []string{"/nowhere"}}, 0},
		"table present and armed": {config.Policy{Present: true, Enabled: true, UploadRoots: []string{"/nowhere"}}, 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := config.Builtin()
			d.Policy = c.policy
			a := New(&fakeBrowser{}, io.Discard, io.Discard).WithDefaults(d)
			if got := a.uploadRoots(); len(got) != c.want {
				t.Errorf("uploadRoots() = %v, want %d root(s)", got, c.want)
			}
		})
	}
}

// TestUploadRootsSurviveAPolicyOff is M8.
//
// --policy-off is in the model for the ORIGIN allow-list: RFC-0012 says a bad
// policy that cannot be bypassed is worse than none, and the bypass is warned
// and audited. It is NOT in the model for upload_roots. RFC-0006 US-7's threat
// model is specifically an agent that controls the argv, and --policy-off is
// argv — a filesystem boundary any caller could switch off with a flag would
// bound nobody. Before the fix, `upload '#f' ~/.ssh/id_rsa --policy-off` exited
// 0 against a configured roots list.
func TestUploadRootsSurviveAPolicyOff(t *testing.T) {
	t.Parallel()
	root, p := uploadRootsFixture(t)

	t.Run("outside the roots is still refused", func(t *testing.T) {
		t.Parallel()
		env, code := runWithRoots(t, noCallUploadBrowser(t), []string{root},
			"upload", "#f", p["secret"], "--policy-off", "--target", "aa11", "--json")
		if code != result.ExitPermission {
			t.Fatalf("exit = %d, want %d — --policy-off must not lift a filesystem boundary; envelope %v",
				code, result.ExitPermission, env)
		}
		if e := env["error"].(map[string]any); e["code"] != result.CodePermissionDenied {
			t.Errorf("error.code = %v, want permission_denied", e["code"])
		}
	})

	t.Run("inside the roots still works", func(t *testing.T) {
		t.Parallel()
		b := newUploadCapture()
		_, code := runWithRoots(t, b, []string{root},
			"upload", "#f", p["inside"], "--policy-off", "--target", "aa11", "--json")
		if code != 0 {
			t.Fatalf("exit = %d, want 0 — the roots restrict, they do not disable the verb", code)
		}
	})

	t.Run("--policy-off still lifts the origin allow-list", func(t *testing.T) {
		t.Parallel()
		// The two boundaries are separable, and this is the half that stays
		// bypassable: a bad allow-list must remain fixable.
		d := config.Builtin()
		d.Policy = config.Policy{Present: true, Enabled: true,
			Allow: []string{"nothing.matches"}, UploadRoots: []string{root}, Source: "test"}
		b := newUploadCapture()
		var out, errb bytes.Buffer
		code := New(b, &out, &errb).WithDefaults(d).
			Execute("upload", "#f", p["inside"], "--policy-off", "--target", "aa11", "--json")
		if code != 0 {
			t.Fatalf("exit = %d, want 0 — the origin allow-list is still bypassable; %s", code, out.String())
		}
	})
}
