package cli

// Stub-backed tests for the tab-lifecycle verbs (RFC-0007 VS-5, VS-6, VS-7,
// VS-12). Everything here runs under -short: no Chrome is involved, and the
// validation cases prove none is contacted.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/state"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// closeRecorder records every CloseTabs call, so a test can assert that a
// refused close closed NOTHING rather than merely exiting 4.
type closeRecorder struct {
	fakeBrowser
	calls [][]string
}

func (r *closeRecorder) CloseTabs(_ context.Context, ids []string) (map[string]any, error) {
	r.calls = append(r.calls, append([]string(nil), ids...))
	closed := make([]any, 0, len(ids))
	for _, id := range ids {
		closed = append(closed, map[string]any{"id": id, "url": "u", "title": "t"})
	}
	return map[string]any{"closed": closed, "count": len(closed)}, nil
}

func twoTabs() []target.Info {
	return []target.Info{
		{ID: "aa11", Title: "Staging report", URL: "https://staging.internal/report"},
		{ID: "bb22", Title: "Staging admin", URL: "https://staging.internal/admin"},
		{ID: "cc33", Title: "GitHub", URL: "https://github.com/"},
	}
}

// VS-5: a filter matching several tabs without --all is ambiguous_target, and —
// the half that matters — nothing is closed.
func TestCloseAmbiguousClosesNothing(t *testing.T) {
	t.Parallel()
	b := &closeRecorder{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
	env, _, code := run(t, b, "close", "--url", "staging.internal", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (target)", code)
	}
	if got := env["error"].(map[string]any)["code"]; got != "ambiguous_target" {
		t.Errorf("error.code = %v, want ambiguous_target", got)
	}
	if len(b.calls) != 0 {
		t.Errorf("CloseTabs was called %v — an ambiguous close must close nothing", b.calls)
	}
}

// --all is what makes a multi-match close legal; the envelope then carries the
// closed tabs instead of a single `target`.
func TestCloseAllClosesEveryMatchAndOmitsTarget(t *testing.T) {
	t.Parallel()
	b := &closeRecorder{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
	env, _, code := run(t, b, "close", "--url", "staging.internal", "--all", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(b.calls) != 1 || strings.Join(b.calls[0], ",") != "aa11,bb22" {
		t.Fatalf("CloseTabs calls = %v, want one call with [aa11 bb22]", b.calls)
	}
	res := env["result"].(map[string]any)
	if res["count"] != float64(2) {
		t.Errorf("result.count = %v, want 2", res["count"])
	}
	if _, ok := env["target"]; ok {
		t.Errorf("a bulk close must omit the single-tab target object, got %v", env["target"])
	}
}

// A single match needs no --all, and reports the tab it closed.
func TestCloseSingleMatchReportsTarget(t *testing.T) {
	t.Parallel()
	b := &closeRecorder{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
	env, _, code := run(t, b, "close", "--title", "GitHub", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if env["target"].(map[string]any)["id"] != "cc33" {
		t.Errorf("target = %v, want cc33", env["target"])
	}
}

// VS-6: a filter that matches nothing is target_not_found.
func TestCloseNoMatchIsTargetNotFound(t *testing.T) {
	t.Parallel()
	b := &closeRecorder{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
	env, _, code := run(t, b, "close", "--url", "nothing-matches", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (target)", code)
	}
	if got := env["error"].(map[string]any)["code"]; got != "target_not_found" {
		t.Errorf("error.code = %v, want target_not_found", got)
	}
	if len(b.calls) != 0 {
		t.Errorf("CloseTabs was called %v for a filter that matched nothing", b.calls)
	}
}

// Naming a tab positionally and by filter are two different ways to say which
// tab; giving both is rejected before the browser is contacted.
func TestCloseTargetPlusFilterIsUsage(t *testing.T) {
	t.Parallel()
	env, _, code := run(t, noCall(t), "close", "aa11", "--url", "staging", "--json")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)", code)
	}
	if got := env["error"].(map[string]any)["code"]; got != "usage" {
		t.Errorf("error.code = %v, want usage", got)
	}
}

// VS-7: closing the sticky tab clears it and says so, and the NEXT command then
// fails with no_current_target rather than against a dead tab id.
func TestCloseStickyTargetClearsItAndNextCommandHasNone(t *testing.T) {
	t.Parallel()
	b := &closeRecorder{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
	sticky := "aa11"
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithStickyTarget(
		func(ConnOpts) string { return sticky },
		func(_ ConnOpts, spec string) error { sticky = spec; return nil },
	)

	if code := app.Execute("close", "--json"); code != 0 {
		t.Fatalf("close exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	env := decodeEnvelope(t, out.String())
	if got := env["result"].(map[string]any)["sticky_cleared"]; got != true {
		t.Errorf("result.sticky_cleared = %v, want true", got)
	}
	if sticky != "" {
		t.Errorf("persisted sticky target = %q, want it cleared", sticky)
	}

	out.Reset()
	code := app.Execute("eval", "1+1", "--json")
	if code != 4 {
		t.Fatalf("follow-up exit = %d, want 4 (target)", code)
	}
	env = decodeEnvelope(t, out.String())
	if got := env["error"].(map[string]any)["code"]; got != "no_current_target" {
		t.Errorf("follow-up error.code = %v, want no_current_target", got)
	}
}

// Closing some OTHER tab must leave the sticky target alone (US-7's first half).
func TestCloseOtherTabLeavesStickyAlone(t *testing.T) {
	t.Parallel()
	b := &closeRecorder{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
	sticky := "aa11"
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithStickyTarget(
		func(ConnOpts) string { return sticky },
		func(_ ConnOpts, spec string) error { sticky = spec; return nil },
	)
	if code := app.Execute("close", "cc33", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	env := decodeEnvelope(t, out.String())
	if got := env["result"].(map[string]any)["sticky_cleared"]; got != false {
		t.Errorf("result.sticky_cleared = %v, want false", got)
	}
	if sticky != "aa11" {
		t.Errorf("sticky target = %q, want it untouched", sticky)
	}
}

// partialCloseBrowser refuses one id and closes the rest — what CloseTabs
// reports when Target.closeTarget is rejected for a tab (one with an attached
// debugger, or wedged in beforeunload). It is a SUCCESS: a close that half
// worked is not an error, and the refused tab is listed under `failed`.
type partialCloseBrowser struct {
	fakeBrowser
	refuse string
}

func (p *partialCloseBrowser) CloseTabs(_ context.Context, ids []string) (map[string]any, error) {
	closed := make([]any, 0, len(ids))
	var failed []any
	for _, id := range ids {
		if id == p.refuse {
			failed = append(failed, map[string]any{"id": id, "error": "Target.closeTarget refused"})
			continue
		}
		closed = append(closed, map[string]any{"id": id, "url": "u", "title": "t"})
	}
	res := map[string]any{"closed": closed, "count": len(closed)}
	if len(failed) > 0 {
		res["failed"] = failed
	}
	return res, nil
}

// stickyApp wires an App to an in-memory sticky target and returns the app plus
// a pointer to the stored value, so a test can assert what survived.
func stickyApp(b chrome.Browser, out, errb *bytes.Buffer, sticky *string) *App {
	return New(b, out, errb).WithStickyTarget(
		func(ConnOpts) string { return *sticky },
		func(_ ConnOpts, spec string) error { *sticky = spec; return nil },
	)
}

// The sticky tab's close was REFUSED, so the tab is still open and still listed.
// Clearing the sticky pointer anyway strands every later command on
// no_current_target while the tab it named is alive — the exact inversion of
// RFC-0007 US-7, which exists to avoid stranding the user.
func TestCloseKeepsStickyWhenItsTabRefusedToClose(t *testing.T) {
	t.Parallel()
	b := &partialCloseBrowser{fakeBrowser: fakeBrowser{tabs: twoTabs()}, refuse: "aa11"}
	sticky := "aa11"
	var out, errb bytes.Buffer
	app := stickyApp(b, &out, &errb, &sticky)

	if code := app.Execute("close", "--url", "staging.internal", "--all", "--json"); code != 0 {
		t.Fatalf("close exit = %d, want 0 — a partial close still succeeds (stderr: %s)", code, errb.String())
	}
	env := decodeEnvelope(t, out.String())
	res := env["result"].(map[string]any)
	if res["count"] != float64(1) {
		t.Fatalf("result.count = %v, want 1 (only bb22 closed): %v", res["count"], res)
	}
	if _, ok := res["failed"]; !ok {
		t.Fatalf("result.failed missing — the fixture must report the refused tab: %v", res)
	}
	if got := res["sticky_cleared"]; got != false {
		t.Errorf("result.sticky_cleared = %v, want false: %s is still open", got, sticky)
	}
	if sticky != "aa11" {
		t.Errorf("sticky target = %q, want aa11 — its tab never closed", sticky)
	}

	// And the proof that it is still usable: the next command resolves it.
	out.Reset()
	if code := app.Execute("eval", "1+1", "--json"); code != 0 {
		t.Errorf("follow-up exit = %d, want 0 — the sticky tab is still open", code)
	}
}

// The mirror case: when the sticky tab is among the ones that DID close, it is
// cleared even though a different tab failed.
func TestCloseClearsStickyWhenItsTabActuallyClosed(t *testing.T) {
	t.Parallel()
	b := &partialCloseBrowser{fakeBrowser: fakeBrowser{tabs: twoTabs()}, refuse: "bb22"}
	sticky := "aa11"
	var out, errb bytes.Buffer
	app := stickyApp(b, &out, &errb, &sticky)

	if code := app.Execute("close", "--url", "staging.internal", "--all", "--json"); code != 0 {
		t.Fatalf("close exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	env := decodeEnvelope(t, out.String())
	if got := env["result"].(map[string]any)["sticky_cleared"]; got != true {
		t.Errorf("result.sticky_cleared = %v, want true — aa11 did close", got)
	}
	if sticky != "" {
		t.Errorf("sticky target = %q, want it cleared", sticky)
	}
}

// With no sticky setter wired at all there is nothing to clear, and the envelope
// must say so rather than claim a clear that never happened.
func TestCloseWithoutStickyStoreReportsNotCleared(t *testing.T) {
	t.Parallel()
	b := &closeRecorder{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
	env, _, code := run(t, b, "close", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := env["result"].(map[string]any)["sticky_cleared"]; got != false {
		t.Errorf("result.sticky_cleared = %v, want false with no sticky store", got)
	}
}

// The sticky store persists, so the clear that `close` performs must survive a
// fresh Store over the same state dir — not just live in memory.
func TestStickyStoreClearPersists(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const key = "127.0.0.1:9222"

	s, err := state.New(key)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	if err := s.SetCurrentTarget("aa11"); err != nil {
		t.Fatalf("SetCurrentTarget: %v", err)
	}
	if got := s.CurrentTarget(); got != "aa11" {
		t.Fatalf("CurrentTarget = %q, want aa11", got)
	}

	// This is exactly what `close` does when it destroys the sticky tab.
	if err := s.SetCurrentTarget(""); err != nil {
		t.Fatalf("clear: %v", err)
	}

	fresh, err := state.New(key)
	if err != nil {
		t.Fatalf("state.New (reopen): %v", err)
	}
	if got := fresh.CurrentTarget(); got != "" {
		t.Errorf("CurrentTarget after clear = %q, want empty so the next command reports no_current_target", got)
	}

	// A different endpoint keeps its own target — the clear must not be global.
	other, err := state.New("127.0.0.1:9333")
	if err != nil {
		t.Fatalf("state.New (other endpoint): %v", err)
	}
	if err := other.SetCurrentTarget("zz99"); err != nil {
		t.Fatalf("other SetCurrentTarget: %v", err)
	}
	if got := fresh.CurrentTarget(); got != "" {
		t.Errorf("endpoint keys leaked: CurrentTarget = %q, want empty", got)
	}
}

func TestActivateReportsHonestFlags(t *testing.T) {
	t.Parallel()
	b := &activateStub{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
	env, _, code := run(t, b, "activate", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	res := env["result"].(map[string]any)
	for k, want := range map[string]any{"activated": true, "was_active": true, "window_focused": false} {
		if res[k] != want {
			t.Errorf("result.%s = %v, want %v (the verb must report what happened, not what was attempted)", k, res[k], want)
		}
	}
	if env["target"].(map[string]any)["id"] != "aa11" {
		t.Errorf("target = %v, want aa11", env["target"])
	}
}

// activateStub reports a window the OS refused to raise — the case a retry loop
// must be able to distinguish.
type activateStub struct {
	fakeBrowser
}

func (activateStub) Activate(context.Context, string) (map[string]any, error) {
	return map[string]any{"activated": true, "was_active": true, "window_focused": false}, nil
}

// VS-12: the nav modes are mutually exclusive, and an illegal combination is
// exit 2 with Chrome never contacted.
func TestNavFlagValidationTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want int // 0 = accepted, 2 = usage
	}{
		{"url alone", []string{"nav", "https://example.com/"}, 0},
		{"back alone", []string{"nav", "--back"}, 0},
		{"forward alone", []string{"nav", "--forward"}, 0},
		{"reload alone", []string{"nav", "--reload"}, 0},
		{"hard reload", []string{"nav", "--reload", "--hard"}, 0},
		{"back with a url", []string{"nav", "--back", "https://example.com/"}, 2},
		{"reload with a url", []string{"nav", "--reload", "https://example.com/"}, 2},
		{"back and forward", []string{"nav", "--back", "--forward"}, 2},
		{"back and reload", []string{"nav", "--back", "--reload"}, 2},
		{"hard without reload", []string{"nav", "--hard", "--back"}, 2},
		{"hard alone", []string{"nav", "--hard"}, 2},
		{"no mode at all", []string{"nav"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// The rejected cases get a browser that fails the test on any call, so
			// "validated before connecting" is proven rather than asserted.
			var b chrome.Browser = noCall(t)
			if c.want == 0 {
				b = &fakeBrowser{tabs: twoTabs()}
			}
			env, _, code := run(t, b, append(c.args, "--target", "aa11", "--json")...)
			if code != c.want {
				t.Fatalf("%v exit = %d, want %d", c.args, code, c.want)
			}
			if c.want == 2 && env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("%v error.code = %v, want usage", c.args, env["error"])
			}
		})
	}
}

// A history move with nothing in that direction is a typed error, not a silent
// no-op: a wizard script that quietly failed to go back would act on the wrong
// page. The daemon flattens errors to strings, so the CLI must still recognise
// it — this stub returns the bare message, exactly as the RPC would.
func TestNavNoHistoryIsTypedTargetError(t *testing.T) {
	t.Parallel()
	b := &noHistoryBrowser{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
	env, _, code := run(t, b, "nav", "--back", "--target", "aa11", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (target)", code)
	}
	e := env["error"].(map[string]any)
	if e["code"] != "target_not_found" {
		t.Errorf("error.code = %v, want target_not_found", e["code"])
	}
	if e["no_history"] != true {
		t.Errorf("error.no_history = %v, want true", e["no_history"])
	}
}

type noHistoryBrowser struct {
	fakeBrowser
}

func (noHistoryBrowser) History(context.Context, string, int) (map[string]any, error) {
	// Rebuilt from the message, the way a daemon round-trip delivers it.
	return nil, errors.New(chrome.ErrNoHistory.Error() + ": delta -1 from entry 1 of 1")
}

// nav's history and reload modes report the settled URL in target.url, the same
// way a redirecting navigation does.
func TestNavHistoryAndReloadReportSettledURL(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"nav", "--back"},
		{"nav", "--forward"},
		{"nav", "--reload"},
		{"nav", "--reload", "--hard"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			t.Parallel()
			b := &settledBrowser{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
			env, _, code := run(t, b, append(args, "--target", "aa11", "--json")...)
			if code != 0 {
				t.Fatalf("%v exit = %d, want 0", args, code)
			}
			if got := env["target"].(map[string]any)["url"]; got != "https://settled.test/" {
				t.Errorf("%v target.url = %v, want the settled URL", args, got)
			}
		})
	}
}

type settledBrowser struct {
	fakeBrowser
}

func (settledBrowser) History(context.Context, string, int) (map[string]any, error) {
	return map[string]any{"url": "https://settled.test/", "status": 200}, nil
}

func (settledBrowser) Reload(context.Context, string, bool) (map[string]any, error) {
	return map[string]any{"url": "https://settled.test/", "status": 200}, nil
}

// --hard reaches the browser as ignoreCache, and a soft reload does not.
func TestNavHardThreadsIgnoreCache(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"nav", "--reload"}, false},
		{[]string{"nav", "--reload", "--hard"}, true},
	} {
		b := &reloadCapture{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
		if _, _, code := run(t, b, append(c.args, "--target", "aa11", "--json")...); code != 0 {
			t.Fatalf("%v exit = %d, want 0", c.args, code)
		}
		if b.hard != c.want {
			t.Errorf("%v: Reload hard = %v, want %v", c.args, b.hard, c.want)
		}
	}
}

type reloadCapture struct {
	fakeBrowser
	hard bool
}

func (r *reloadCapture) Reload(_ context.Context, _ string, hard bool) (map[string]any, error) {
	r.hard = hard
	return map[string]any{"url": "https://settled.test/"}, nil
}

// --back and --forward must reach the browser as the deltas the interface takes,
// so that a later --back N is a flag change and not a signature change.
func TestNavHistoryDeltas(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		flag string
		want int
	}{{"--back", -1}, {"--forward", 1}} {
		b := &historyCapture{fakeBrowser: fakeBrowser{tabs: twoTabs()}}
		if _, _, code := run(t, b, "nav", c.flag, "--target", "aa11", "--json"); code != 0 {
			t.Fatalf("nav %s exit = %d, want 0", c.flag, code)
		}
		if b.delta != c.want {
			t.Errorf("nav %s delta = %d, want %d", c.flag, b.delta, c.want)
		}
	}
}

type historyCapture struct {
	fakeBrowser
	delta int
}

func (h *historyCapture) History(_ context.Context, _ string, delta int) (map[string]any, error) {
	h.delta = delta
	return map[string]any{"url": "https://settled.test/"}, nil
}

func decodeEnvelope(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var env map[string]any
	s := strings.TrimSpace(stdout)
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		t.Fatalf("stdout is not one JSON value: %v\n%s", err, s)
	}
	return env
}
