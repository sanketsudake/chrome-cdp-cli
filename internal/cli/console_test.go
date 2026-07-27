package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// consoleBrowser serves a canned console read and records the options the CLI
// built, so the tests assert on the contract (envelope + exit code + what
// reached the browser) rather than on rendered text.
type consoleBrowser struct {
	fakeBrowser
	gotOpts  chrome.ConsoleOpts
	messages []any
	stream   []any // payloads ConsoleStream emits, in order
	streamed bool
}

func (c *consoleBrowser) Console(_ context.Context, _ string, opts chrome.ConsoleOpts) (any, error) {
	c.gotOpts = opts
	return map[string]any{
		"messages": c.messages, "count": len(c.messages),
		"buffered": 52, "dropped": 3, "truncated": false,
	}, nil
}

func (c *consoleBrowser) ConsoleStream(_ context.Context, _ string, opts chrome.ConsoleOpts, emit func(any) error) error {
	c.gotOpts, c.streamed = opts, true
	for _, p := range c.stream {
		if err := emit(p); err != nil {
			return err
		}
	}
	return nil
}

func consoleTestBrowser(msgs ...any) *consoleBrowser {
	return &consoleBrowser{
		fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}},
		messages:    msgs,
	}
}

func msg(level, text string) map[string]any {
	return map[string]any{"level": level, "text": text, "source": "console"}
}

func TestConsoleEnvelopeShape(t *testing.T) {
	t.Parallel()
	b := consoleTestBrowser(msg("error", "TypeError: x.map is not a function"))
	env, _, code := run(t, b, "console", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if env["ok"] != true || env["command"] != "console" {
		t.Fatalf("envelope = %v", env)
	}
	res := env["result"].(map[string]any)
	for _, k := range []string{"messages", "count", "buffered", "dropped", "truncated"} {
		if _, has := res[k]; !has {
			t.Errorf("result is missing %q — it is part of the documented envelope", k)
		}
	}
	if res["count"].(float64) != 1 || res["buffered"].(float64) != 52 {
		t.Errorf("result = %v, want count 1 of 52 buffered", res)
	}
	// dropped is what tells a caller it read too late; it must survive to the
	// envelope rather than being summarised away.
	if res["dropped"].(float64) != 3 {
		t.Errorf("dropped = %v, want 3", res["dropped"])
	}
}

// Filtering is server-side: the flags must arrive at the browser as options,
// not be applied to an already-marshalled result.
func TestConsoleFiltersAreSentToTheBrowser(t *testing.T) {
	t.Parallel()
	b := consoleTestBrowser()
	_, _, code := run(t, b,
		"console", "--target", "aa11", "--json",
		"--grep", `\[App\]`, "--level", "warn", "--level", "error",
		"--limit", "20", "--since", "30s", "--clear")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got := b.gotOpts
	if got.Grep != `\[App\]` {
		t.Errorf("Grep = %q", got.Grep)
	}
	if strings.Join(got.Levels, ",") != "warn,error" {
		t.Errorf("Levels = %v, want [warn error]", got.Levels)
	}
	if got.Limit != 20 || got.Since.String() != "30s" || !got.Clear {
		t.Errorf("opts = %+v, want limit 20 / since 30s / clear", got)
	}
}

func TestConsoleOnlyErrorsIsLevelError(t *testing.T) {
	t.Parallel()
	b := consoleTestBrowser()
	if _, _, code := run(t, b, "console", "--target", "aa11", "--json", "--only-errors"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Join(b.gotOpts.Levels, ",") != "error" {
		t.Errorf("Levels = %v, want [error] — uncaught exceptions are reported at error level", b.gotOpts.Levels)
	}
}

// VS-8 and its siblings: every malformed invocation is usage/exit 2 with the
// browser never contacted. noCall proves the second half — asserting only on
// the exit code would also pass for a command that connected first.
func TestConsoleValidationNeverConnects(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"invalid regex":             {"console", "--grep", "("},
		"unknown level":             {"console", "--level", "critical"},
		"bad since":                 {"console", "--since", "banana"},
		"negative limit":            {"console", "--limit", "-2"},
		"follow with fail-on-match": {"console", "--follow", "--fail-on-match"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, _, code := run(t, noCall(t), append(args, "--target", "aa11", "--json")...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage)", code)
			}
			if env == nil {
				return // a cobra parse failure renders to stderr; exit 2 is the contract
			}
			if got := env["error"].(map[string]any)["code"]; got != "usage" {
				t.Errorf("error.code = %v, want usage", got)
			}
		})
	}
}

// A streaming line inside `session` would emit many objects for one command,
// breaking the one-envelope-per-line contract the batch mode promises.
func TestConsoleFollowInsideSessionIsUsage(t *testing.T) {
	t.Parallel()
	b := consoleTestBrowser()
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithInput(strings.NewReader(`["console","--follow","--target","aa11"]` + "\n"))
	if code := app.Execute("session"); code != 0 {
		t.Fatalf("session exit = %d, want 0 (per-line failures ride in the envelope)", code)
	}
	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &env); err != nil {
		t.Fatalf("session line is not one JSON envelope: %v\n%s", err, out.String())
	}
	if env["ok"] != false || env["error"].(map[string]any)["code"] != "usage" {
		t.Errorf("envelope = %v, want a usage error", env)
	}
	if b.streamed {
		t.Error("ConsoleStream ran inside session; --follow must be rejected before it starts")
	}
}

// VS-9: the assertion failing must not suppress the findings — exit 1 AND the
// messages, or a CI log shows that something broke without saying what.
func TestConsoleFailOnMatchExitsOneWithTheEvidence(t *testing.T) {
	t.Parallel()
	b := consoleTestBrowser(msg("error", "TypeError: x.map is not a function"))
	env, _, code := run(t, b, "console", "--target", "aa11", "--json", "--only-errors", "--fail-on-match")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if env["ok"] != false {
		t.Errorf("ok = %v, want false", env["ok"])
	}
	if got := env["error"].(map[string]any)["code"]; got != "assertion_failed" {
		t.Errorf("error.code = %v, want assertion_failed", got)
	}
	res, ok := env["result"].(map[string]any)
	if !ok {
		t.Fatalf("the failing envelope dropped its result: %v", env)
	}
	msgs := res["messages"].([]any)
	if len(msgs) != 1 || !strings.Contains(msgs[0].(map[string]any)["text"].(string), "TypeError") {
		t.Errorf("result.messages = %v, want the matching error", msgs)
	}
}

func TestConsoleFailOnMatchIsSilentWhenNothingMatched(t *testing.T) {
	t.Parallel()
	b := consoleTestBrowser() // no messages
	env, _, code := run(t, b, "console", "--target", "aa11", "--json", "--only-errors", "--fail-on-match")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — a clean console is the passing case", code)
	}
	if env["ok"] != true {
		t.Errorf("envelope = %v, want ok", env)
	}
}

// VS-7: --follow writes one NDJSON object per message, in order, each line
// independently parseable, and exits 0 when the window closes.
func TestConsoleFollowStreamsNDJSON(t *testing.T) {
	t.Parallel()
	b := consoleTestBrowser()
	b.stream = []any{
		map[string]any{"messages": []any{msg("log", "first")}, "count": 1, "buffered": 1, "dropped": 0},
		map[string]any{"messages": []any{msg("warn", "second")}, "count": 1, "buffered": 2, "dropped": 0},
		map[string]any{"messages": []any{msg("error", "third")}, "count": 1, "buffered": 3, "dropped": 0},
	}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	if code := app.Execute("console", "--target", "aa11", "--follow"); code != 0 {
		t.Fatalf("exit = %d, want 0 (the follow window closing is not a failure): %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d NDJSON lines, want 3:\n%s", len(lines), out.String())
	}
	want := []string{"first", "second", "third"}
	for i, line := range lines {
		var env map[string]any
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("line %d is not independently parseable: %v\n%s", i, err, line)
		}
		if env["command"] != "console" || env["ok"] != true {
			t.Errorf("line %d envelope = %v", i, env)
		}
		got := env["result"].(map[string]any)["messages"].([]any)[0].(map[string]any)["text"]
		if got != want[i] {
			t.Errorf("line %d text = %v, want %q — order must be preserved", i, got, want[i])
		}
	}
}

// --follow streams NDJSON envelopes whether or not --json was passed, the same
// way `session` does, so a caller parses one shape in both modes.
func TestConsoleFollowIsNDJSONWithoutJSONFlag(t *testing.T) {
	t.Parallel()
	b := consoleTestBrowser()
	b.stream = []any{map[string]any{"messages": []any{msg("log", "only")}, "count": 1}}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	if code := app.Execute("console", "--target", "aa11", "--follow"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Errorf("follow output is not JSON: %q", out.String())
	}
}

func TestConsoleWithoutTargetIsTargetError(t *testing.T) {
	t.Parallel()
	b := &consoleBrowser{fakeBrowser: fakeBrowser{tabs: []target.Info{
		{ID: "aa11", Title: "A", URL: "u"}, {ID: "bb22", Title: "B", URL: "v"},
	}}}
	env, _, code := run(t, b, "console", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (target)", code)
	}
	if env["error"].(map[string]any)["code"] != "no_current_target" {
		t.Errorf("error.code = %v", env["error"])
	}
}
