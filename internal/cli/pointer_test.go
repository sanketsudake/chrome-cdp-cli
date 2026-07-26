package cli

// Stub-backed tests for the pointer verbs (RFC-0005 VS-9, VS-10, and the
// command-boundary plan): validation happens before any connection, each verb
// maps to the right PointerAction, and --modifiers parses to the documented
// bitmask.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// noCallPointerBrowser fails the test if anything reaches the browser. Every
// invocation that should be rejected as `usage` is run against it, which proves
// the exit-2 contract ("your call was wrong, do not retry") is decided before a
// connection is attempted rather than after a round-trip.
type noCallPointerBrowser struct {
	stubBrowser
	t *testing.T
}

func (b *noCallPointerBrowser) List(context.Context) ([]target.Info, error) {
	b.t.Fatal("browser was contacted for an invocation that should have failed validation")
	return nil, nil
}

func (b *noCallPointerBrowser) Pointer(context.Context, string, string, chrome.PointerOpts) (map[string]any, error) {
	b.t.Fatal("Pointer was dispatched for an invocation that should have failed validation")
	return nil, nil
}

// pointerCapture records what Pointer was called with.
type pointerCapture struct {
	fakeBrowser
	gotSelector string
	gotOpts     chrome.PointerOpts
}

func (p *pointerCapture) Pointer(_ context.Context, _, selector string, opts chrome.PointerOpts) (map[string]any, error) {
	p.gotSelector, p.gotOpts = selector, opts
	return map[string]any{"action": string(opts.Action), "x": 1.0, "y": 2.0}, nil
}

func newPointerCapture() *pointerCapture {
	return &pointerCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}}
}

// VS-9: drag needs exactly one destination form. Both or neither is a usage
// error that never reaches Chrome.
func TestDragTargetingIsMutuallyExclusive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		args     []string
		wantExit int
	}{
		{"to only", []string{"drag", "#a", "--to", "#b"}, 0},
		{"dx only", []string{"drag", "#a", "--dx", "100"}, 0},
		{"dy only", []string{"drag", "#a", "--dy", "40"}, 0},
		{"zero delta is still a delta", []string{"drag", "#a", "--dx", "0"}, 0},
		{"both", []string{"drag", "#a", "--to", "#b", "--dx", "100"}, 2},
		{"neither", []string{"drag", "#a"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var b chrome.Browser = newPointerCapture()
			if c.wantExit != 0 {
				b = &noCallPointerBrowser{t: t}
			}
			env, _, code := run(t, b, append(c.args, "--target", "aa11", "--json")...)
			if code != c.wantExit {
				t.Fatalf("%v exit = %d, want %d", c.args, code, c.wantExit)
			}
			if c.wantExit == 2 && env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("%v error.code = %v, want usage", c.args, env["error"])
			}
		})
	}
}

// --steps is bounded; outside the range is a usage error, not a slow surprise.
func TestDragStepsRangeIsValidatedBeforeConnecting(t *testing.T) {
	t.Parallel()
	for _, steps := range []string{"0", "-1", "101"} {
		t.Run("steps="+steps, func(t *testing.T) {
			t.Parallel()
			b := &noCallPointerBrowser{t: t}
			env, _, code := run(t, b, "drag", "#a", "--to", "#b", "--steps", steps, "--target", "aa11", "--json")
			if code != 2 {
				t.Fatalf("--steps %s exit = %d, want 2 (usage)", steps, code)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("error.code = %v, want usage", env["error"])
			}
		})
	}
}

// VS-10: an unknown modifier name is rejected before the browser is touched, on
// every verb that takes --modifiers.
func TestBadModifierNeverConnects(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"hover", "#x", "--modifiers", "command"},
		{"dblclick", "#x", "--modifiers", "command"},
		{"rclick", "#x", "--modifiers", "command"},
		{"drag", "#x", "--to", "#y", "--modifiers", "command"},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()
			b := &noCallPointerBrowser{t: t}
			env, _, code := run(t, b, append(args, "--target", "aa11", "--json")...)
			if code != 2 {
				t.Fatalf("%v exit = %d, want 2 (usage)", args, code)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("%v error.code = %v, want usage", args, env["error"])
			}
		})
	}
}

// Each verb dispatches Pointer with its own action, and the envelope's command
// name is the verb the user typed.
func TestPointerVerbsMapToActions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		verb string
		args []string
		want chrome.PointerAction
	}{
		{"hover", []string{"hover", "#x"}, chrome.PointerHover},
		{"dblclick", []string{"dblclick", "#x"}, chrome.PointerDblClick},
		{"rclick", []string{"rclick", "#x"}, chrome.PointerRClick},
		{"drag", []string{"drag", "#x", "--to", "#y"}, chrome.PointerDrag},
	}
	for _, c := range cases {
		t.Run(c.verb, func(t *testing.T) {
			t.Parallel()
			b := newPointerCapture()
			env, _, code := run(t, b, append(c.args, "--target", "aa11", "--json")...)
			if code != 0 {
				t.Fatalf("%v exit = %d, want 0", c.args, code)
			}
			if b.gotOpts.Action != c.want {
				t.Errorf("Action = %q, want %q", b.gotOpts.Action, c.want)
			}
			if b.gotSelector != "#x" {
				t.Errorf("selector = %q, want #x", b.gotSelector)
			}
			if env["command"] != c.verb {
				t.Errorf("envelope command = %v, want %q", env["command"], c.verb)
			}
			if env["ok"] != true {
				t.Errorf("ok = %v, want true", env["ok"])
			}
		})
	}
}

// --modifiers is pure arithmetic, so exhaust it: every subset of the four
// modifier keys must produce its CDP bitmask (alt 1, ctrl 2, meta 4, shift 8).
func TestModifiersParseToBitmask(t *testing.T) {
	t.Parallel()
	bits := []struct {
		mask int64
		name string
	}{{1, "alt"}, {2, "ctrl"}, {4, "meta"}, {8, "shift"}}
	for mask := int64(0); mask < 16; mask++ {
		var names []string
		for _, b := range bits {
			if mask&b.mask != 0 {
				names = append(names, b.name)
			}
		}
		spec := strings.Join(names, "+")
		t.Run(fmt.Sprintf("%d/%s", mask, spec), func(t *testing.T) {
			t.Parallel()
			b := newPointerCapture()
			_, _, code := run(t, b, "dblclick", "#x", "--modifiers", spec, "--target", "aa11", "--json")
			if code != 0 {
				t.Fatalf("--modifiers %q exit = %d, want 0", spec, code)
			}
			if b.gotOpts.Modifiers != mask {
				t.Errorf("--modifiers %q = %d, want %d", spec, b.gotOpts.Modifiers, mask)
			}
		})
	}
}

// The RFC documents `cmd` as the spelling for the meta key; it must map to the
// same bit as `meta`.
func TestCmdIsMeta(t *testing.T) {
	t.Parallel()
	b := newPointerCapture()
	if _, _, code := run(t, b, "rclick", "#x", "--modifiers", "cmd+shift", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.gotOpts.Modifiers != 4|8 {
		t.Errorf("cmd+shift = %d, want %d (meta|shift)", b.gotOpts.Modifiers, 4|8)
	}
}

// The drop target inherits the source's --by unless --to-by overrides it.
func TestDragToByDefaultsToBy(t *testing.T) {
	t.Parallel()
	t.Run("inherits", func(t *testing.T) {
		t.Parallel()
		b := newPointerCapture()
		if _, _, code := run(t, b, "drag", "Task A", "--to", "Done", "--by", "name", "--target", "aa11", "--json"); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if b.gotOpts.ToQuery.By != "name" {
			t.Errorf("ToQuery.By = %q, want name (inherited from --by)", b.gotOpts.ToQuery.By)
		}
	})
	t.Run("overrides", func(t *testing.T) {
		t.Parallel()
		b := newPointerCapture()
		if _, _, code := run(t, b, "drag", "Task A", "--to", "#done", "--by", "name", "--to-by", "css", "--target", "aa11", "--json"); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if b.gotOpts.ToQuery.By != "css" {
			t.Errorf("ToQuery.By = %q, want css (from --to-by)", b.gotOpts.ToQuery.By)
		}
		if b.gotOpts.Query.By != "name" {
			t.Errorf("Query.By = %q, want name — --to-by must not change the source's addressing", b.gotOpts.Query.By)
		}
	})
}

// --steps, --hold, --dx/--dy and the selector flags all reach the driver.
func TestDragFlagsThread(t *testing.T) {
	t.Parallel()
	b := newPointerCapture()
	_, _, code := run(t, b, "drag", "#slider", "--dx", "80", "--dy", "-10",
		"--steps", "20", "--hold", "300ms", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got := b.gotOpts
	if got.Dx != 80 || got.Dy != -10 || got.Steps != 20 || got.Hold != 300*time.Millisecond {
		t.Errorf("opts = {Dx:%v Dy:%v Steps:%d Hold:%v}, want {80 -10 20 300ms}", got.Dx, got.Dy, got.Steps, got.Hold)
	}
	if got.To != "" {
		t.Errorf("To = %q, want empty for a delta drag", got.To)
	}
}

// hover --hold parks the pointer for a stated duration (RFC open question 2).
func TestHoverHoldThreads(t *testing.T) {
	t.Parallel()
	b := newPointerCapture()
	if _, _, code := run(t, b, "hover", "#x", "--hold", "500ms", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.gotOpts.Hold != 500*time.Millisecond {
		t.Errorf("Hold = %v, want 500ms", b.gotOpts.Hold)
	}
}

// The shared selector flags reach the pointer verbs' QueryOpts, so --by name /
// --in-row / --on-dialog work here exactly as they do on click.
func TestPointerQueryFlagsThread(t *testing.T) {
	t.Parallel()
	b := newPointerCapture()
	_, _, code := run(t, b, "hover", "Delete", "--by", "name", "--role", "button",
		"--in-row", "TEST entry", "--on-dialog", "accept", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	q := b.gotOpts.Query
	if q.By != "name" || q.Role != "button" || q.InRow != "TEST entry" || q.OnDialog != "accept" {
		t.Errorf("Query = %+v, want {By:name Role:button InRow:'TEST entry' OnDialog:accept}", q)
	}
}

// --wait-text composes with a pointer verb: the action runs, then the wait, and
// the confirmation is reported in the result.
func TestPointerWaitTextComposes(t *testing.T) {
	t.Parallel()
	b := newPointerCapture()
	env, _, code := run(t, b, "dblclick", "#cell", "--wait-text", "Saved", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := env["result"].(map[string]any)["waited_text"]; got != "Saved" {
		t.Errorf("result.waited_text = %v, want Saved", got)
	}
}

// occludedBrowser fails a pointer gesture the way the driver does when an
// element resolves but is covered — including across the daemon's RPC boundary,
// where the sentinel arrives as a plain message.
type occludedBrowser struct {
	fakeBrowser
	viaRPC bool
}

func (o *occludedBrowser) Pointer(context.Context, string, string, chrome.PointerOpts) (map[string]any, error) {
	if o.viaRPC {
		return nil, fmt.Errorf("%s", chrome.ErrOccluded.Error())
	}
	return nil, fmt.Errorf("dblclick: %w", chrome.ErrOccluded)
}

// VS-11: an element that resolves but never presents an unoccluded centre is
// exit 4 with `occluded: true`, so a caller can tell "covered by an overlay"
// from "not found".
func TestOccludedTargetIsExit4WithDetail(t *testing.T) {
	t.Parallel()
	for _, viaRPC := range []bool{false, true} {
		t.Run(fmt.Sprintf("viaRPC=%v", viaRPC), func(t *testing.T) {
			t.Parallel()
			b := &occludedBrowser{
				fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}},
				viaRPC:      viaRPC,
			}
			env, _, code := run(t, b, "dblclick", "#cell", "--target", "aa11", "--json")
			if code != 4 {
				t.Fatalf("exit = %d, want 4 (target)", code)
			}
			e := env["error"].(map[string]any)
			if e["code"] != "target_timeout" {
				t.Errorf("error.code = %v, want target_timeout", e["code"])
			}
			// Err.Details is flattened into the error object by the envelope's
			// marshaller, so the detail lands as a sibling of code/message.
			if e["occluded"] != true {
				t.Errorf("expected error.occluded=true, got error=%v", e)
			}
		})
	}
}

// recordingPointerBrowser captures each Pointer call so a batch can be checked
// for order and connection reuse.
type recordingPointerBrowser struct {
	fakeBrowser
	actions []chrome.PointerAction
}

func (b *recordingPointerBrowser) Pointer(_ context.Context, _, _ string, opts chrome.PointerOpts) (map[string]any, error) {
	b.actions = append(b.actions, opts.Action)
	return map[string]any{"action": string(opts.Action)}, nil
}

// RFC-0005 VS-13: the pointer verbs are ordinary argv verbs, so they compose
// inside a session batch over one held connection.
//
// This matters more than it looks: hover's whole purpose is to leave the pointer
// somewhere so a LATER command sees what it revealed. If hover only worked as a
// standalone process, the reveal would be gone by the time anything could read
// it, and the verb would be useless for the case it exists to serve.
func TestPointerVerbsComposeInsideSession(t *testing.T) {
	t.Parallel()
	b := &recordingPointerBrowser{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}}
	in := strings.NewReader(
		`["hover","Row 1","--target","aa11"]` + "\n" +
			`["snap","--target","aa11"]` + "\n",
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
			t.Fatalf("session line is not one JSON envelope: %q (%v)", line, err)
		}
		envs = append(envs, e)
	}
	if len(envs) != 2 {
		t.Fatalf("got %d NDJSON envelopes, want 2: %v", len(envs), envs)
	}
	if envs[0]["command"] != "hover" || envs[0]["ok"] != true {
		t.Errorf("first envelope = %v, want an ok hover result", envs[0])
	}
	if envs[1]["command"] != "snap" || envs[1]["ok"] != true {
		t.Errorf("second envelope = %v, want an ok snap result", envs[1])
	}
	// The hover ran once, in order, against the batch's single connection.
	if len(b.actions) != 1 || b.actions[0] != chrome.PointerHover {
		t.Errorf("Pointer calls = %v, want exactly one hover", b.actions)
	}
}
