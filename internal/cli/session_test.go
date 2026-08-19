package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// session runs each stdin line as a JSON argv over one held connection and emits
// one JSON envelope per line (NDJSON).
func TestSessionNDJSON(t *testing.T) {
	t.Parallel()
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	in := strings.NewReader(
		`["list"]` + "\n" +
			`# a comment line is skipped` + "\n" +
			"\n" + // blank line skipped
			`["snap","--target","aa11"]` + "\n" +
			`not-json` + "\n",
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
	// list ok, snap ok, and the malformed line as a usage error = 3 envelopes.
	if len(envs) != 3 {
		t.Fatalf("got %d NDJSON envelopes, want 3: %v", len(envs), envs)
	}
	if envs[0]["command"] != "list" || envs[0]["ok"] != true {
		t.Errorf("envelope 0 = %v, want ok list", envs[0])
	}
	if envs[1]["command"] != "snap" || envs[1]["ok"] != true {
		t.Errorf("envelope 1 = %v, want ok snap", envs[1])
	}
	if envs[2]["ok"] != false {
		t.Errorf("envelope 2 = %v, want an error for the malformed line", envs[2])
	}
}

// TestSessionCmdFreezesSession mirrors TestRecipeRunFreezesEndpoint /
// TestMCPRunnerFreezesEndpoint: --session is a connection-shaped flag exactly
// like --port and --endpoint, and `session`'s stdin loop has to freeze it
// into a.defaults the same way. Each line is a fresh a.Execute(argv) that
// rebuilds the command tree via newRoot, which re-registers --session with
// a.defaults.Session as its default; an argv line with no --session of its
// own (every real line) then resets a.session to that default. Without the
// freeze, `chrome-cdp --session a session` with two `use` lines would keep
// session "a" only on the very first line and silently fall back to the
// unset default on every line after it.
func TestSessionCmdFreezesSession(t *testing.T) {
	t.Parallel()
	var gotSessions []string
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	in := strings.NewReader(`["use","aa11"]` + "\n" + `["use","aa11"]` + "\n")
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithInput(in)
	app.WithStickyTarget(
		func(ConnOpts) string { return "" },
		func(o ConnOpts, id string) error { gotSessions = append(gotSessions, o.Session); return nil },
	)

	if code := app.Execute("--session", "a", "session"); code != 0 {
		t.Fatalf("session exit = %d\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
	}
	if len(gotSessions) != 2 {
		t.Fatalf("stickySet called %d times, want 2 (one per `use` line)", len(gotSessions))
	}
	for i, got := range gotSessions {
		if got != "a" {
			t.Errorf("line %d: ConnOpts.Session = %q, want the frozen --session \"a\"", i+1, got)
		}
	}
}

// TestSessionCmdFreezesPort: --port has to survive the same per-line
// re-registration as --session, or `chrome-cdp --port 9333 session` would key
// only its first line's sticky-target write under the explicit port and every
// line after it under the auto-discovered default.
func TestSessionCmdFreezesPort(t *testing.T) {
	t.Parallel()
	var gotPorts []int
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	in := strings.NewReader(`["use","aa11"]` + "\n" + `["use","aa11"]` + "\n")
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithInput(in)
	app.WithStickyTarget(
		func(ConnOpts) string { return "" },
		func(o ConnOpts, id string) error { gotPorts = append(gotPorts, o.Port); return nil },
	)

	if code := app.Execute("--port", "9333", "session"); code != 0 {
		t.Fatalf("session exit = %d\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
	}
	if len(gotPorts) != 2 {
		t.Fatalf("stickySet called %d times, want 2 (one per `use` line)", len(gotPorts))
	}
	for i, got := range gotPorts {
		if got != 9333 {
			t.Errorf("line %d: ConnOpts.Port = %d, want the frozen --port 9333", i+1, got)
		}
	}
}

// TestSessionIsolatesStickyTarget: `use` under one --session must not move
// another session's current tab. The seam is two in-memory stores keyed by
// ConnOpts.Session, the same shape cmd/chrome-cdp/main.go's stateFor gives
// WithStickyTarget in production (there it is two files under XDG_STATE_HOME
// instead of two map entries).
func TestSessionIsolatesStickyTarget(t *testing.T) {
	t.Parallel()
	tabs := []target.Info{{ID: "aa11", Title: "A", URL: "u"}, {ID: "bb22", Title: "B", URL: "v"}}
	b := &fakeBrowser{tabs: tabs}
	store := map[string]string{}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	app.WithStickyTarget(
		func(o ConnOpts) string { return store[o.Session] },
		func(o ConnOpts, id string) error { store[o.Session] = id; return nil },
	)

	if code := app.Execute("--session", "a", "use", "aa11", "--json"); code != 0 {
		t.Fatalf("use under --session a: exit = %d, stderr: %s", code, errb.String())
	}
	if store["a"] != "aa11" {
		t.Fatalf("store[a] = %q, want aa11", store["a"])
	}

	out.Reset()
	errb.Reset()
	// --session b never called `use`, so it must have no sticky target of its
	// own — session a's write must not have leaked into it.
	if code := app.Execute("--session", "b", "eval", "1+1", "--json"); code == 0 {
		t.Fatalf("eval under --session b unexpectedly succeeded: %s", out.String())
	}
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, out.String())
	}
	if env["error"].(map[string]any)["code"] != "no_current_target" {
		t.Errorf("error.code = %v, want no_current_target (session b has no sticky tab)", env["error"])
	}
	if store["b"] != "" {
		t.Errorf("store[b] = %q, want empty — session a's use must not touch it", store["b"])
	}
}

// TestSessionEnvelopeKeysReportTheActiveSession: an agent driving several
// sessions on one Chrome has to tell, from the envelope alone, which
// sticky-target namespace a command ran under — list carries it as
// current_session, use carries it as session, and both are "" with no
// --session at all.
func TestSessionEnvelopeKeysReportTheActiveSession(t *testing.T) {
	t.Parallel()
	tabs := []target.Info{{ID: "aa11", Title: "A", URL: "u"}}
	b := &fakeBrowser{tabs: tabs}
	store := map[string]string{}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	app.WithStickyTarget(
		func(o ConnOpts) string { return store[o.Session] },
		func(o ConnOpts, id string) error { store[o.Session] = id; return nil },
	)

	if code := app.Execute("--session", "a", "list", "--json"); code != 0 {
		t.Fatalf("list under --session a: exit = %d, stderr: %s", code, errb.String())
	}
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, out.String())
	}
	if got := env["result"].(map[string]any)["current_session"]; got != "a" {
		t.Errorf("list result.current_session = %v, want \"a\"", got)
	}

	out.Reset()
	errb.Reset()
	if code := app.Execute("--session", "a", "use", "aa11", "--json"); code != 0 {
		t.Fatalf("use under --session a: exit = %d, stderr: %s", code, errb.String())
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, out.String())
	}
	if got := env["result"].(map[string]any)["session"]; got != "a" {
		t.Errorf("use result.session = %v, want \"a\"", got)
	}

	out.Reset()
	errb.Reset()
	if code := app.Execute("list", "--json"); code != 0 {
		t.Fatalf("list with no --session: exit = %d, stderr: %s", code, errb.String())
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, out.String())
	}
	if got := env["result"].(map[string]any)["current_session"]; got != "" {
		t.Errorf("list result.current_session with no --session = %v, want \"\"", got)
	}

	out.Reset()
	errb.Reset()
	if code := app.Execute("use", "aa11", "--json"); code != 0 {
		t.Fatalf("use with no --session: exit = %d, stderr: %s", code, errb.String())
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, out.String())
	}
	if got := env["result"].(map[string]any)["session"]; got != "" {
		t.Errorf("use result.session with no --session = %v, want \"\"", got)
	}
}

// TestSessionValidatedBeforeConnecting: a malformed --session (a space, which
// ^[A-Za-z0-9._-]{1,64}$ rejects) is usage/exit 2 with Chrome never
// contacted — the same "validate before connecting" guarantee --endpoint
// gets, proven with a browser stub whose methods t.Fatal if reached.
func TestSessionValidatedBeforeConnecting(t *testing.T) {
	t.Parallel()
	env, _, code := run(t, noCall(t), "--session", "a b/c", "list", "--json")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
	if env["error"].(map[string]any)["code"] != result.CodeUsage {
		t.Errorf("error.code = %v, want %s", env["error"], result.CodeUsage)
	}
}
