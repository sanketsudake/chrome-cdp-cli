package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// storageBrowser is a stub-backed double that records every StorageX call's
// arguments, the way dialogBrowser and recordBrowser record theirs.
type storageBrowser struct {
	fakeBrowser

	listErr error
	listRes map[string]any
	getRes  map[string]any

	lastScope string
	lastKey   string
	lastValue string
	lastOpts  chrome.StorageListOpts
	calls     []string
}

func newStorageBrowser(t *testing.T) *storageBrowser {
	t.Helper()
	return &storageBrowser{
		fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "https://example.com/"}}},
	}
}

func (b *storageBrowser) StorageList(_ context.Context, _ string, scope string, opts chrome.StorageListOpts) (map[string]any, error) {
	b.calls = append(b.calls, "StorageList")
	b.lastScope, b.lastOpts = scope, opts
	if b.listErr != nil {
		return nil, b.listErr
	}
	if b.listRes != nil {
		return b.listRes, nil
	}
	return map[string]any{
		"scope": scope, "origin": "https://example.com",
		"items": []map[string]any{
			{"key": "access_token", "value": chrome.NetRedacted},
			{"key": "theme", "value": "dark"},
		},
		"count": 2, "truncated": false,
	}, nil
}

func (b *storageBrowser) StorageGet(_ context.Context, _ string, scope, key string) (map[string]any, error) {
	b.calls = append(b.calls, "StorageGet")
	b.lastScope, b.lastKey = scope, key
	if b.getRes != nil {
		return b.getRes, nil
	}
	return map[string]any{"scope": scope, "origin": "https://example.com", "key": key, "value": "dark", "present": true}, nil
}

func (b *storageBrowser) StorageSet(_ context.Context, _ string, scope, key, value string) (map[string]any, error) {
	b.calls = append(b.calls, "StorageSet")
	b.lastScope, b.lastKey, b.lastValue = scope, key, value
	return map[string]any{"scope": scope, "origin": "https://example.com", "key": key, "set": true}, nil
}

func (b *storageBrowser) StorageRemove(_ context.Context, _ string, scope, key string) (map[string]any, error) {
	b.calls = append(b.calls, "StorageRemove")
	b.lastScope, b.lastKey = scope, key
	return map[string]any{"scope": scope, "origin": "https://example.com", "key": key, "removed": true}, nil
}

func (b *storageBrowser) StorageClear(_ context.Context, _ string, scope string) (map[string]any, error) {
	b.calls = append(b.calls, "StorageClear")
	b.lastScope = scope
	return map[string]any{"scope": scope, "origin": "https://example.com", "cleared": true}, nil
}

var _ chrome.Browser = (*storageBrowser)(nil)

// TestStorageListAndGetEnvelope is RFC-0019 VS-10: command is "storage" for
// both, the result maps cross unchanged, and the stub sees the right scope.
func TestStorageListAndGetEnvelope(t *testing.T) {
	t.Parallel()
	b := newStorageBrowser(t)

	env, _, code := run(t, b, "storage", "local", "list", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("list exit = %d, want 0: %v", code, env)
	}
	if env["command"] != "storage" || env["ok"] != true {
		t.Errorf("list envelope = %v", env)
	}
	res := env["result"].(map[string]any)
	if res["count"] != float64(2) {
		t.Errorf("list result = %v", res)
	}
	if b.lastScope != "local" {
		t.Errorf("StorageList saw scope %q, want local", b.lastScope)
	}

	b.getRes = map[string]any{"scope": "session", "origin": "https://example.com", "key": "nope", "value": "", "present": false}
	env2, _, code2 := run(t, b, "storage", "session", "get", "nope", "--target", "aa11", "--json")
	if code2 != 0 {
		t.Fatalf("get exit = %d, want 0: %v", code2, env2)
	}
	if env2["command"] != "storage" {
		t.Errorf("get command = %v, want storage", env2["command"])
	}
	res2 := env2["result"].(map[string]any)
	if res2["present"] != false {
		t.Errorf("get result = %v, want present:false", res2)
	}
	if b.lastScope != "session" {
		t.Errorf("StorageGet saw scope %q, want session", b.lastScope)
	}
}

// TestStorageSetRmClearEnvelope covers the three remaining leaves' envelopes.
func TestStorageSetRmClearEnvelope(t *testing.T) {
	t.Parallel()
	b := newStorageBrowser(t)

	env, _, code := run(t, b, "storage", "local", "set", "onboarding_done", "1", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("set exit = %d, want 0: %v", code, env)
	}
	if got := env["result"].(map[string]any)["set"]; got != true {
		t.Errorf("set result = %v", env["result"])
	}

	env2, _, code2 := run(t, b, "storage", "session", "rm", "draft", "--target", "aa11", "--json")
	if code2 != 0 {
		t.Fatalf("rm exit = %d, want 0: %v", code2, env2)
	}
	if got := env2["result"].(map[string]any)["removed"]; got != true {
		t.Errorf("rm result = %v", env2["result"])
	}

	env3, _, code3 := run(t, b, "storage", "local", "clear", "--target", "aa11", "--json")
	if code3 != 0 {
		t.Fatalf("clear exit = %d, want 0: %v", code3, env3)
	}
	if got := env3["result"].(map[string]any)["cleared"]; got != true {
		t.Errorf("clear result = %v", env3["result"])
	}
}

// TestStorageFlagsAndPositionalsReachTheBrowser is VS-11: --no-redact and
// --max-value reach StorageListOpts, positionals reach Set/Remove, and the
// default MaxValue is DefaultStorageMaxValue when no flag is given.
func TestStorageFlagsAndPositionalsReachTheBrowser(t *testing.T) {
	t.Parallel()
	b := newStorageBrowser(t)

	if _, _, code := run(t, b, "storage", "local", "list", "--no-redact", "--max-value", "0", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("list exit = %d, want 0", code)
	}
	if want := (chrome.StorageListOpts{NoRedact: true, MaxValue: 0}); b.lastOpts != want {
		t.Errorf("StorageListOpts = %+v, want %+v", b.lastOpts, want)
	}

	if _, _, code := run(t, b, "storage", "local", "list", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("list (defaults) exit = %d, want 0", code)
	}
	if b.lastOpts.MaxValue != chrome.DefaultStorageMaxValue || b.lastOpts.NoRedact {
		t.Errorf("default StorageListOpts = %+v, want MaxValue %d, NoRedact false", b.lastOpts, chrome.DefaultStorageMaxValue)
	}

	if _, _, code := run(t, b, "storage", "session", "set", "k", "v", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("set exit = %d, want 0", code)
	}
	if b.lastScope != "session" || b.lastKey != "k" || b.lastValue != "v" {
		t.Errorf("StorageSet saw (%q, %q, %q), want (session, k, v)", b.lastScope, b.lastKey, b.lastValue)
	}
}

// TestStorageValidationNeverConnects is VS-12: every malformed invocation is
// usage/exit 2 without the browser ever being contacted.
func TestStorageValidationNeverConnects(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"storage"},
		{"storage", "lcoal", "list"},
		{"storage", "local", "get"},
		{"storage", "local", "set", "k"},
		{"storage", "local", "set", "k", "v", "w"},
		{"storage", "local", "rm"},
		{"storage", "local", "list", "extra"},
		{"storage", "local", "clear", "x"},
		{"storage", "local", "list", "--max-value", "-1"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			full := append(append([]string{}, args...), "--target", "aa11", "--json")
			env, _, code := run(t, noCall(t), full...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2: %v", code, env)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("code = %v, want usage", env["error"])
			}
		})
	}
}

// TestStorageOpaqueOriginIsTargetNotFound is VS-13: an opaque-origin failure
// from the driver classifies as target_not_found/exit 4 with opaque_origin:true.
func TestStorageOpaqueOriginIsTargetNotFound(t *testing.T) {
	t.Parallel()
	b := newStorageBrowser(t)
	b.listErr = fmt.Errorf("%w: data:text/html,<title>d</title>", chrome.ErrOpaqueOrigin)

	env, _, code := run(t, b, "storage", "local", "list", "--target", "aa11", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4: %v", code, env)
	}
	e := env["error"].(map[string]any)
	if e["code"] != "target_not_found" {
		t.Errorf("code = %v, want target_not_found", e["code"])
	}
	if e["opaque_origin"] != true {
		t.Errorf("opaque_origin = %v, want true", e["opaque_origin"])
	}
}

// TestStorageInsideSession is VS-14: two storage lines inside `session`
// produce two envelopes, one per line.
func TestStorageInsideSession(t *testing.T) {
	t.Parallel()
	b := newStorageBrowser(t)
	in := strings.NewReader(
		`["storage","local","set","k","v","--target","aa11"]` + "\n" +
			`["storage","local","get","k","--target","aa11"]` + "\n",
	)
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithInput(in)
	if code := app.Execute("session"); code != 0 {
		t.Fatalf("session exit = %d, want 0", code)
	}
	var envs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("output line is not JSON: %q (%v)", line, err)
		}
		envs = append(envs, e)
	}
	if len(envs) != 2 {
		t.Fatalf("got %d envelopes, want 2: %v", len(envs), envs)
	}
	for i, e := range envs {
		if e["command"] != "storage" || e["ok"] != true {
			t.Errorf("envelope %d = %v, want ok storage", i, e)
		}
	}
}
