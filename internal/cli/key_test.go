package cli

import (
	"context"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// keyCapture records what the `key` verb handed the Browser, so a test can assert
// on the parsed strokes and options rather than only on the envelope the stub
// echoes back.
type keyCapture struct {
	fakeBrowser
	gotSelector string
	gotKeys     []chrome.KeyStroke
	gotOpts     chrome.KeyOpts
}

func (k *keyCapture) Key(ctx context.Context, id, selector string, keys []chrome.KeyStroke, opts chrome.KeyOpts) (map[string]any, error) {
	k.gotSelector, k.gotKeys, k.gotOpts = selector, keys, opts
	return k.fakeBrowser.Key(ctx, id, selector, keys, opts)
}

func keyBrowser() *keyCapture {
	return &keyCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "https://example.com/"}}}}
}

func TestKeyEnvelope(t *testing.T) {
	t.Parallel()
	b := keyBrowser()
	env, _, code := run(t, b, "key", "Escape", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if env["ok"] != true || env["command"] != "key" {
		t.Fatalf("envelope = %v, want ok=true command=key", env)
	}
	res := env["result"].(map[string]any)
	keys := res["keys"].([]any)
	if len(keys) != 1 || keys[0] != "Escape" {
		t.Errorf("result.keys = %v, want [Escape]", res["keys"])
	}
	if res["repeat"] != float64(1) {
		t.Errorf("result.repeat = %v, want 1", res["repeat"])
	}
}

// The keyspec is always the last argument, so one argument is a bare keyspec and
// two are selector + keyspec.
func TestKeyArgumentOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		args     []string
		wantSel  string
		wantKey  string
		wantMods int64
	}{
		{"keyspec only", []string{"key", "Escape"}, "", "Escape", 0},
		{"selector then keyspec", []string{"key", "#search", "cmd+a"}, "#search", "a", 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			b := keyBrowser()
			args := append(append([]string{}, c.args...), "--target", "aa11", "--json")
			if _, _, code := run(t, b, args...); code != 0 {
				t.Fatalf("%v exit = %d, want 0", args, code)
			}
			if b.gotSelector != c.wantSel {
				t.Errorf("selector = %q, want %q", b.gotSelector, c.wantSel)
			}
			if len(b.gotKeys) != 1 {
				t.Fatalf("keys = %+v, want exactly one stroke", b.gotKeys)
			}
			if got := b.gotKeys[0]; got.Key != c.wantKey || got.Modifiers != c.wantMods {
				t.Errorf("stroke = %+v, want key %q modifiers %d", got, c.wantKey, c.wantMods)
			}
		})
	}
}

// The parsed sequence, not the raw string, is what reaches the browser.
func TestKeySequenceReachesBrowser(t *testing.T) {
	t.Parallel()
	b := keyBrowser()
	if _, _, code := run(t, b, "key", "End shift+Home Backspace", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	want := []string{"End", "Home", "Backspace"}
	if len(b.gotKeys) != len(want) {
		t.Fatalf("keys = %+v, want %d strokes", b.gotKeys, len(want))
	}
	for i, w := range want {
		if b.gotKeys[i].Key != w {
			t.Errorf("stroke %d = %q, want %q", i, b.gotKeys[i].Key, w)
		}
	}
	if b.gotKeys[1].Modifiers != 8 {
		t.Errorf("shift+Home modifiers = %d, want 8 (shift)", b.gotKeys[1].Modifiers)
	}
}

func TestKeyRepeatAndDelayThread(t *testing.T) {
	t.Parallel()
	b := keyBrowser()
	env, _, code := run(t, b, "key", "ArrowDown", "--repeat", "5", "--delay", "50ms", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.gotOpts.Repeat != 5 {
		t.Errorf("KeyOpts.Repeat = %d, want 5", b.gotOpts.Repeat)
	}
	if b.gotOpts.Delay != 50*time.Millisecond {
		t.Errorf("KeyOpts.Delay = %v, want 50ms", b.gotOpts.Delay)
	}
	if got := env["result"].(map[string]any)["repeat"]; got != float64(5) {
		t.Errorf("result.repeat = %v, want 5", got)
	}
}

// --repeat outside 1..100 is a usage error decided before the browser is
// touched, so a mistyped repeat never launches Chrome or presses anything.
func TestKeyRepeatOutOfRangeIsUsageAndNeverConnects(t *testing.T) {
	t.Parallel()
	for _, repeat := range []string{"0", "-1", "500"} {
		t.Run(repeat, func(t *testing.T) {
			t.Parallel()
			env, _, code := run(t, noCall(t), "key", "ArrowDown", "--repeat", repeat, "--target", "aa11", "--json")
			if code != 2 {
				t.Fatalf("--repeat %s exit = %d, want 2 (usage)", repeat, code)
			}
			if got := env["error"].(map[string]any)["code"]; got != "usage" {
				t.Errorf("error.code = %v, want usage", got)
			}
		})
	}
}

// A keyspec that does not parse is a usage error decided before connecting. The
// noCall browser is the assertion that matters: exit 2 alone would also pass for
// a command that connected first and validated afterwards.
func TestKeyBadKeyspecIsUsageAndNeverConnects(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"misspelled key name":   "Ecsape",
		"bare word is not text": "hello",
		"unknown modifier":      "hyper+a",
		"two keys in one chord": "cmd+a+b",
		"trailing plus":         "ctrl+",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, _, code := run(t, noCall(t), "key", spec, "--target", "aa11", "--json")
			if code != 2 {
				t.Fatalf("key %q exit = %d, want 2 (usage)", spec, code)
			}
			if got := env["error"].(map[string]any)["code"]; got != "usage" {
				t.Errorf("error.code = %v, want usage", got)
			}
		})
	}
}

// No arguments at all is a cobra arity failure, and must still be usage/exit 2
// rather than a generic error.
func TestKeyNoArgsIsUsage(t *testing.T) {
	t.Parallel()
	if _, _, code := run(t, noCall(t), "key", "--target", "aa11", "--json"); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}

// The selector flows through the shared QueryOpts, so `key` gets --by/--role for
// free rather than inventing its own addressing.
func TestKeyThreadsQueryOpts(t *testing.T) {
	t.Parallel()
	b := keyBrowser()
	if _, _, code := run(t, b, "key", "Search", "Escape", "--by", "name", "--role", "textbox", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.gotOpts.Query.By != "name" || b.gotOpts.Query.Role != "textbox" {
		t.Errorf("KeyOpts.Query = %+v, want By=name Role=textbox", b.gotOpts.Query)
	}
}
