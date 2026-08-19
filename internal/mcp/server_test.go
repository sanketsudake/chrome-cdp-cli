package mcp

// Protocol-level tests, driven over an in-memory transport rather than a
// subprocess: initialize, tools/list, tools/call.
//
// The Runner is faked here, so these cover the protocol and the mapping. The
// end-to-end tests that prove the CLI command tree really is what runs — and
// that the two front ends agree — live in internal/cli/mcp_test.go, where the
// stub browser and the cobra tree both are.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// testTimeout bounds every blocking call in this file. A test that hangs is
// worse than one that fails: it takes the whole suite with it.
const testTimeout = 20 * time.Second

// fakeRunner answers with canned envelopes and records the argv it was handed.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	reply func(argv []string) ([]byte, int)
}

func (r *fakeRunner) Run(_ context.Context, argv []string) ([]byte, int) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), argv...))
	reply := r.reply
	r.mu.Unlock()
	if reply == nil {
		return okEnvelope(argv[0], map[string]any{"ok": true}), 0
	}
	return reply(argv)
}

func (r *fakeRunner) argv(i int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.calls) {
		return nil
	}
	return r.calls[i]
}

func (r *fakeRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func okEnvelope(command string, res map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"ok": true, "command": command, "result": res, "elapsed_ms": 1})
	return b
}

func errEnvelope(command, code, msg string, details map[string]any) []byte {
	e := map[string]any{"code": code, "message": msg}
	for k, v := range details {
		e[k] = v
	}
	b, _ := json.Marshal(map[string]any{"ok": false, "command": command, "error": e, "elapsed_ms": 1})
	return b
}

// connect starts a server on an in-memory transport pair and returns a client
// session, torn down deterministically when the test ends.
func connect(t *testing.T, r Runner, o Options) *sdk.ClientSession {
	t.Helper()
	s, err := New(r, o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ct, st := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, st) }()

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cctx, ccancel := context.WithTimeout(context.Background(), testTimeout)
	defer ccancel()
	sess, err := client.Connect(cctx, ct, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = sess.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Error("the server did not shut down after the session closed")
		}
	})
	return sess
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

// VS-1: the handshake reports a protocol version and capabilities.
func TestInitializeHandshake(t *testing.T) {
	t.Parallel()
	sess := connect(t, &fakeRunner{}, Options{Version: "test"})
	init := sess.InitializeResult()
	if init.ProtocolVersion == "" {
		t.Error("no protocol version in the initialize result")
	}
	if init.Capabilities == nil || init.Capabilities.Tools == nil {
		t.Errorf("no tools capability: %+v", init.Capabilities)
	}
	if init.ServerInfo == nil || init.ServerInfo.Name != "chrome-cdp" {
		t.Errorf("server info = %+v", init.ServerInfo)
	}
	if !strings.Contains(init.Instructions, "by=\"name\"") {
		t.Errorf("the instructions do not mention accessible-name addressing:\n%s", init.Instructions)
	}
}

// VS-2: the tool list is bounded, every tool is documented, and every schema is
// a valid JSON Schema.
func TestToolListIsBoundedAndSchemaValid(t *testing.T) {
	t.Parallel()
	sess := connect(t, &fakeRunner{}, Options{})
	res, err := sess.ListTools(ctxT(t), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("no tools listed")
	}
	if len(res.Tools) > maxDefaultTools {
		t.Errorf("the default set lists %d tools, over the budget of %d (RFC-0004 US-4)", len(res.Tools), maxDefaultTools)
	}
	for _, tl := range res.Tools {
		if !strings.HasPrefix(tl.Name, prefix) {
			t.Errorf("tool %q is not namespaced with %q", tl.Name, prefix)
		}
		if len(tl.Description) < 40 {
			t.Errorf("tool %q has a thin description (%d chars); descriptions are the product", tl.Name, len(tl.Description))
		}
		raw, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatalf("tool %q: cannot marshal the input schema: %v", tl.Name, err)
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("tool %q: the input schema is not a JSON Schema: %v", tl.Name, err)
		}
		if _, err := s.Resolve(nil); err != nil {
			t.Errorf("tool %q: the input schema does not resolve: %v", tl.Name, err)
		}
		if s.Type != "object" {
			t.Errorf("tool %q: schema type = %q, want object", tl.Name, s.Type)
		}
		for name, p := range s.Properties {
			if p.Description == "" {
				t.Errorf("tool %q: argument %q has no description", tl.Name, name)
			}
		}
	}
	// raw_cdp is powerful and unconstrained; it stays behind --tools full
	// (RFC-0004 open question 1).
	for _, tl := range res.Tools {
		if tl.Name == prefix+"raw_cdp" {
			t.Error("raw_cdp must not be in the default tool set")
		}
	}
}

func TestFullSetAddsRawCDP(t *testing.T) {
	t.Parallel()
	sess := connect(t, &fakeRunner{}, Options{Tools: SetFull})
	res, err := sess.ListTools(ctxT(t), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if !hasTool(res.Tools, prefix+"raw_cdp") {
		t.Error("--tools full does not expose raw_cdp")
	}
}

func hasTool(tools []*sdk.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// VS-3: a success maps to the envelope's result object, unchanged.
func TestSuccessMapsToEnvelopeResult(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{reply: func([]string) ([]byte, int) {
		return okEnvelope("click", map[string]any{"clicked": true, "waited_text": "Saved"}), 0
	}}
	sess := connect(t, r, Options{})
	out := callTool(t, sess, prefix+"click", map[string]any{"selector": "Save", "by": "name"})
	if out.IsError {
		t.Fatalf("isError = true: %+v", out.StructuredContent)
	}
	want := map[string]any{"clicked": true, "waited_text": "Saved"}
	if got := structured(t, out); !equalJSON(got, want) {
		t.Errorf("structuredContent = %v, want %v", got, want)
	}
	if len(out.Content) == 0 {
		t.Error("no text summary in the content blocks")
	}
	// The addressing options must reach the CLI as the flags they mirror, and
	// the selector as a positional behind the `--` terminator.
	if argv := r.argv(0); argv[0] != "click" || !containsSeq(argv, "--by", "name") || !containsSeq(argv, "--", "Save") {
		t.Errorf("argv = %v, want a click on Save with --by name", argv)
	}
}

// VS-4: a failure keeps the typed contract — code, exit, and the recoverable
// details an agent branches on.
func TestFailurePreservesCodeAndExit(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{reply: func([]string) ([]byte, int) {
		return errEnvelope("click", "target_timeout", `no element matching name "Save" after 30s`, map[string]any{"tab_hidden": true}), 4
	}}
	sess := connect(t, r, Options{})
	out := callTool(t, sess, prefix+"click", map[string]any{"selector": "Save", "by": "name"})
	if !out.IsError {
		t.Fatal("isError = false for a failing command")
	}
	got := structured(t, out)
	if got["code"] != "target_timeout" {
		t.Errorf("code = %v, want target_timeout", got["code"])
	}
	if fmt.Sprint(got["exit"]) != "4" {
		t.Errorf("exit = %v, want 4", got["exit"])
	}
	if got["tab_hidden"] != true {
		t.Errorf("the recoverable detail was dropped: %v", got)
	}
	if text := textOf(out); !strings.Contains(text, "target_timeout") || !strings.Contains(text, "tab_hidden") {
		t.Errorf("summary = %q, want the code and the hint", text)
	}
}

// VS-5 (protocol half): a bad enum value is `usage`, and nothing runs.
func TestUnknownEnumValueIsUsageAndRunsNothing(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{})
	out := callTool(t, sess, prefix+"click", map[string]any{"selector": "#x", "by": "xpath"})
	if !out.IsError {
		t.Fatal("an unknown `by` value was accepted")
	}
	got := structured(t, out)
	if got["code"] != "usage" || fmt.Sprint(got["exit"]) != "2" {
		t.Errorf("error = %v, want usage/2", got)
	}
	if r.count() != 0 {
		t.Errorf("the command ran anyway: %v", r.calls)
	}
}

func TestArgumentValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"unknown argument", prefix + "click", map[string]any{"selector": "#x", "selectr": "#y"}, "unknown argument"},
		{"missing required", prefix + "read", map[string]any{}, "is required"},
		// click's target is selector-or-at, so its "missing" message names both
		// rather than the generic required-arg wording.
		{"click without a target", prefix + "click", map[string]any{}, "needs `selector`, or `at`"},
		{"click with both targets", prefix + "click", map[string]any{"selector": "#x", "at": "1,2"}, "not both"},
		{"wrong type", prefix + "click", map[string]any{"selector": 3}, "must be a string"},
		{"non-integer", prefix + "click", map[string]any{"selector": "#x", "nth": 1.5}, "must be a integer"},
		{"wrong kind argument", prefix + "read", map[string]any{"kind": "text", "inner": true}, "applies to kind"},
		{"open without url", prefix + "tabs", map[string]any{"action": "open"}, "needs `url`"},
		{"unknown action", prefix + "tabs", map[string]any{"action": "explode"}, "is not one of"},
		{"drag option on hover", prefix + "pointer", map[string]any{"action": "hover", "selector": "#x", "steps": 3}, "applies to action"},
		{"value without selector", prefix + "read", map[string]any{"kind": "value"}, "needs `selector`"},
		{"text on dialog_status", prefix + "tabs", map[string]any{"action": "dialog_status", "text": "x"}, "applies to"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{}
			sess := connect(t, r, Options{})
			out := callTool(t, sess, c.tool, c.args)
			if !out.IsError {
				t.Fatalf("%v was accepted", c.args)
			}
			got := structured(t, out)
			if got["code"] != "usage" {
				t.Errorf("code = %v, want usage", got["code"])
			}
			if msg, _ := got["message"].(string); !strings.Contains(msg, c.want) {
				t.Errorf("message = %q, want it to mention %q", msg, c.want)
			}
			if r.count() != 0 {
				t.Errorf("a malformed call reached the browser: %v", r.calls)
			}
		})
	}
}

// VS-7: --read-only hides every mutating tool, and one invoked by name comes
// back as a typed usage error rather than a protocol error.
func TestReadOnlyHidesMutatingTools(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{ReadOnly: true})
	res, err := sess.ListTools(ctxT(t), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, hidden := range []string{"click", "type_text", "key", "pointer", "select_option", "upload", "evaluate", "navigate", "scroll"} {
		if hasTool(res.Tools, prefix+hidden) {
			t.Errorf("--read-only still lists %s", hidden)
		}
	}
	for _, shown := range []string{"tabs", "snapshot", "find", "read", "wait_for", "screenshot", "console", "network", "batch"} {
		if !hasTool(res.Tools, prefix+shown) {
			t.Errorf("--read-only dropped the reading tool %s", shown)
		}
	}
	// A grouped tool keeps the actions that do not mutate and loses the one
	// that does, rather than disappearing whole.
	for _, tl := range res.Tools {
		if tl.Name != prefix+"tabs" {
			continue
		}
		enum := actionEnum(t, tl)
		if contains(enum, "open") {
			t.Errorf("--read-only still offers tabs action=open: %v", enum)
		}
		for _, want := range []string{"list", "use", "activate"} {
			if !contains(enum, want) {
				t.Errorf("--read-only dropped tabs action=%s: %v", want, enum)
			}
		}
		// RFC-0018 VS-15: dialog_status observes, so --read-only keeps it;
		// dialog_accept/dialog_dismiss change what the page's script sees
		// next, so they go the way action=open does.
		if !contains(enum, "dialog_status") {
			t.Errorf("--read-only dropped tabs action=dialog_status: %v", enum)
		}
		for _, mutating := range []string{"dialog_accept", "dialog_dismiss"} {
			if contains(enum, mutating) {
				t.Errorf("--read-only still offers tabs action=%s: %v", mutating, enum)
			}
		}
	}

	out := callTool(t, sess, prefix+"click", map[string]any{"selector": "#x"})
	if !out.IsError {
		t.Fatal("a hidden tool was invocable")
	}
	if got := structured(t, out); got["code"] != "usage" {
		t.Errorf("hidden tool error = %v, want usage", got)
	}
	if r.count() != 0 {
		t.Errorf("a hidden tool reached the browser: %v", r.calls)
	}
}

// A --read-only server does not offer `close`.
//
// `close` is Exempt in the policy table rather than Mutating — it touches no
// page content — so --read-only kept it in the `tabs` enum, and a server whose
// instructions say "only reading verbs are exposed and nothing can modify a
// page" would close the user's tabs on request.
func TestReadOnlyWithholdsClose(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{ReadOnly: true})
	res, err := sess.ListTools(ctxT(t), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tl := range res.Tools {
		if tl.Name != prefix+"tabs" {
			continue
		}
		enum := actionEnum(t, tl)
		if contains(enum, "close") {
			t.Errorf("--read-only still offers tabs action=close: %v", enum)
		}
		// The reading half of the tool survives; the tool does not disappear.
		for _, want := range []string{"list", "use", "activate"} {
			if !contains(enum, want) {
				t.Errorf("--read-only dropped tabs action=%s: %v", want, enum)
			}
		}
	}
	out := callTool(t, sess, prefix+"tabs", map[string]any{"action": "close", "target": "bb22"})
	if !out.IsError {
		t.Fatal("a --read-only server closed a tab")
	}
	if got := structured(t, out); got["code"] != "usage" {
		t.Errorf("error = %v, want usage", got)
	}
	if r.count() != 0 {
		t.Errorf("the close reached the CLI anyway: %v", r.calls)
	}
}

func actionEnum(t *testing.T, tl *sdk.Tool) []string {
	t.Helper()
	raw, err := json.Marshal(tl.InputSchema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var s struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	return s.Properties["action"].Enum
}

// VS-8: --tools lists exactly what was asked for and nothing else is invocable.
func TestToolsAllowList(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{Tools: "snapshot,read"})
	res, err := sess.ListTools(ctxT(t), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(res.Tools) != 2 || !hasTool(res.Tools, prefix+"snapshot") || !hasTool(res.Tools, prefix+"read") {
		t.Fatalf("listed %d tools: %v", len(res.Tools), toolNamesOf(res.Tools))
	}
	out := callTool(t, sess, prefix+"click", map[string]any{"selector": "#x"})
	if !out.IsError {
		t.Fatal("a tool outside the allow-list was invocable")
	}
	if got := structured(t, out); got["code"] != "usage" {
		t.Errorf("error = %v, want usage", got)
	}
	if r.count() != 0 {
		t.Error("an excluded tool reached the browser")
	}
}

func TestUnknownToolSetIsRefusedAtStartup(t *testing.T) {
	t.Parallel()
	if _, err := New(&fakeRunner{}, Options{Tools: "snapshot,teleport"}); err == nil {
		t.Fatal("an unknown tool name in --tools was accepted")
	}
}

func toolNamesOf(tools []*sdk.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// VS-10: batch runs its steps in order, over one connection, and a mid-batch
// failure stops the rest with the failing index reported.
func TestBatchRunsInOrderAndStopsOnFailure(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{reply: func(argv []string) ([]byte, int) {
		if argv[0] == "fill" {
			return errEnvelope("fill", "target_timeout", "no field named Amount", nil), 4
		}
		return okEnvelope(argv[0], map[string]any{"ok": true}), 0
	}}
	sess := connect(t, r, Options{})
	out := callTool(t, sess, prefix+"batch", map[string]any{"steps": []any{
		map[string]any{"tool": "chrome_cdp_snapshot", "arguments": map[string]any{}},
		map[string]any{"tool": "type_text", "arguments": map[string]any{"selector": "Amount", "text": "10", "replace": true}},
		map[string]any{"tool": "chrome_cdp_click", "arguments": map[string]any{"selector": "Save", "by": "name"}},
	}})
	if !out.IsError {
		t.Fatal("a batch with a failing step reported success")
	}
	got := structured(t, out)
	if got["code"] != "target_timeout" {
		t.Errorf("batch code = %v, want the failing step's code", got["code"])
	}
	failed, _ := got["failed"].(map[string]any)
	if fmt.Sprint(failed["step"]) != "2" {
		t.Errorf("failed step = %v, want 2", failed["step"])
	}
	results, _ := got["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (the third step must not run)", len(results))
	}
	if r.count() != 2 {
		t.Errorf("ran %d commands, want 2: %v", r.count(), r.calls)
	}
	if argv := r.argv(0); argv[0] != "snap" {
		t.Errorf("first step ran %v, want snap", argv)
	}
	if argv := r.argv(1); argv[0] != "fill" {
		t.Errorf("second step ran %v, want fill (replace: true)", argv)
	}
}

func TestBatchSuccessReportsEveryStep(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{})
	out := callTool(t, sess, prefix+"batch", map[string]any{"steps": []any{
		map[string]any{"tool": "snapshot"},
		map[string]any{"tool": "read", "arguments": map[string]any{"kind": "text"}},
	}})
	if out.IsError {
		t.Fatalf("batch failed: %v", structured(t, out))
	}
	got := structured(t, out)
	if fmt.Sprint(got["completed"]) != "2" {
		t.Errorf("completed = %v, want 2", got["completed"])
	}
	if got["failed"] != nil {
		t.Errorf("failed = %v, want null", got["failed"])
	}
}

func TestBatchRejectsNesting(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{})
	out := callTool(t, sess, prefix+"batch", map[string]any{"steps": []any{
		map[string]any{"tool": "batch", "arguments": map[string]any{}},
	}})
	if !out.IsError {
		t.Fatal("a nested batch was accepted")
	}
	if r.count() != 0 {
		t.Error("a rejected batch still ran steps")
	}
}

// A batch is validated whole before any of it runs: a step naming a tool that
// does not exist must not leave the first two steps already applied.
func TestBatchValidatesBeforeRunningAnything(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{})
	out := callTool(t, sess, prefix+"batch", map[string]any{"steps": []any{
		map[string]any{"tool": "snapshot"},
		map[string]any{"tool": "teleport"},
	}})
	if !out.IsError {
		t.Fatal("a batch naming an unknown tool was accepted")
	}
	if r.count() != 0 {
		t.Errorf("the batch ran %d step(s) before failing validation", r.count())
	}
}

// The pinned-tab mode refuses a call that names a different tab rather than
// silently redirecting it.
func TestPinnedTargetIsInjectedAndConflictsRefused(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{Target: "url:workday"})
	if out := callTool(t, sess, prefix+"snapshot", map[string]any{}); out.IsError {
		t.Fatalf("pinned call failed: %v", structured(t, out))
	}
	if argv := r.argv(0); !containsSeq(argv, "--target", "url:workday") {
		t.Errorf("argv = %v, want the pinned --target", argv)
	}
	out := callTool(t, sess, prefix+"snapshot", map[string]any{"target": "@2"})
	if !out.IsError {
		t.Fatal("a conflicting target was accepted on a pinned server")
	}
	// A pinned server does not advertise an argument whose only valid value is
	// the one already chosen.
	tools, err := sess.ListTools(ctxT(t), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tl := range tools.Tools {
		raw, _ := json.Marshal(tl.InputSchema)
		var s struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("decode schema for %s: %v", tl.Name, err)
		}
		if _, ok := s.Properties["target"]; ok {
			t.Errorf("pinned server still advertises `target` on %s", tl.Name)
		}
	}
}

// splitAtDash returns the verb and the positional values a built argv carries,
// which is the whole shape the CLI sees: `<verb> <flags…> -- <positionals…>`.
func splitAtDash(argv []string) (verb string, pos []string) {
	if len(argv) == 0 {
		return "", nil
	}
	for i, w := range argv {
		if w == "--" {
			return argv[0], argv[i+1:]
		}
	}
	return argv[0], nil
}

func TestArgvMirrorsTheCLISpelling(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tool string
		args map[string]any
		want []string
	}{
		{"type appends", prefix + "type_text", map[string]any{"selector": "#a", "text": "hi"}, []string{"type", "#a", "hi"}},
		{"fill replaces", prefix + "type_text", map[string]any{"selector": "#a", "text": "hi", "replace": true}, []string{"fill", "#a", "hi"}},
		{"key spec is last", prefix + "key", map[string]any{"keys": "cmd+a", "selector": "#s"}, []string{"key", "#s", "cmd+a"}},
		{"key without selector", prefix + "key", map[string]any{"keys": "Escape"}, []string{"key", "Escape"}},
		{"read grid", prefix + "read", map[string]any{"kind": "grid", "selector": "table"}, []string{"grid", "table"}},
		{"tabs use", prefix + "tabs", map[string]any{"action": "use", "target": "@2"}, []string{"use", "@2"}},
		{"tabs open", prefix + "tabs", map[string]any{"action": "open", "url": "https://x.test/"}, []string{"open", "https://x.test/"}},
		{"select cascade", prefix + "select_option", map[string]any{"field": "Time Type", "option": "A > B"}, []string{"select", "Time Type", "A > B"}},
		{"upload paths", prefix + "upload", map[string]any{"selector": "#f", "paths": []any{"/a.pdf", "/b.pdf"}}, []string{"upload", "#f", "/a.pdf", "/b.pdf"}},
		{"pointer drag", prefix + "pointer", map[string]any{"action": "drag", "selector": "#s", "to": "#d"}, []string{"drag", "#s"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{}
			sess := connect(t, r, Options{Tools: SetFull})
			if out := callTool(t, sess, c.tool, c.args); out.IsError {
				t.Fatalf("call failed: %v", structured(t, out))
			}
			argv := r.argv(0)
			verb, pos := splitAtDash(argv)
			got := append([]string{verb}, pos...)
			if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
				t.Fatalf("argv = %v, want the verb %q and the positionals %v after `--`", argv, c.want[0], c.want[1:])
			}
		})
	}
}

// TestTabsToolBuildsDialogVerbs is RFC-0018 VS-15 (build half): dialog_status,
// dialog_accept (with its text answering a prompt) and dialog_dismiss fold
// into the tabs tool's two-word verb spelling, which splitAtDash's
// single-word check cannot see — so this asserts on the full argv instead.
func TestTabsToolBuildsDialogVerbs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args map[string]any
		// seqs are contiguous runs argv must contain, checked independently —
		// the verb and the positional are not adjacent once --json is spliced
		// between them by argvFor.
		seqs [][]string
	}{
		{"dialog_status", map[string]any{"action": "dialog_status"}, [][]string{{"dialog", "status"}}},
		{"dialog_accept with text", map[string]any{"action": "dialog_accept", "text": "bob"},
			[][]string{{"dialog", "accept"}, {"--", "bob"}}},
		{"dialog_accept without text", map[string]any{"action": "dialog_accept"}, [][]string{{"dialog", "accept"}}},
		{"dialog_dismiss", map[string]any{"action": "dialog_dismiss"}, [][]string{{"dialog", "dismiss"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{}
			sess := connect(t, r, Options{Tools: SetFull})
			if out := callTool(t, sess, prefix+"tabs", c.args); out.IsError {
				t.Fatalf("call failed: %v", structured(t, out))
			}
			argv := r.argv(0)
			if argv[0] != "dialog" {
				t.Errorf("argv = %v, want it to start with \"dialog\"", argv)
			}
			for _, seq := range c.seqs {
				if !containsSeq(argv, seq...) {
					t.Errorf("argv = %v, want it to contain %v", argv, seq)
				}
			}
		})
	}
}

// Numbers arrive from JSON as float64; an integer flag must not reach the CLI
// spelled "5.000000".
func TestNumericArgumentsAreSpelledAsTyped(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{})
	if out := callTool(t, sess, prefix+"scroll", map[string]any{"dy": 400, "dx": 0.5}); out.IsError {
		t.Fatalf("call failed: %v", structured(t, out))
	}
	argv := r.argv(0)
	if !containsSeq(argv, "--dy", "400") {
		t.Errorf("argv = %v, want --dy 400", argv)
	}
	if !containsSeq(argv, "--dx", "0.5") {
		t.Errorf("argv = %v, want --dx 0.5", argv)
	}
}

// A command that printed nothing parseable is still an error with an exit code,
// not a silent success.
func TestUnparseableEnvelopeIsAnError(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{reply: func([]string) ([]byte, int) { return []byte("not json"), 1 }}
	sess := connect(t, r, Options{})
	out := callTool(t, sess, prefix+"snapshot", map[string]any{})
	if !out.IsError {
		t.Fatal("unparseable output was reported as success")
	}
	if got := structured(t, out); got["code"] != "generic" {
		t.Errorf("code = %v, want generic", got["code"])
	}
}

func TestSpecsDescribeTheExposedSet(t *testing.T) {
	t.Parallel()
	specs, err := Specs(Options{})
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if len(specs) == 0 || len(specs) > maxDefaultTools {
		t.Fatalf("Specs returned %d tools", len(specs))
	}
	for _, s := range specs {
		if len(s.Verbs) == 0 {
			t.Errorf("tool %s declares no verbs; --read-only cannot classify it", s.Name)
		}
	}
}

// callTool invokes one tool and returns its result. A protocol-level error
// fails the test: this server reports tool failures in the result, never as a
// protocol error, so an agent can see and correct them.
func callTool(t *testing.T, sess *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	out, err := sess.CallTool(ctxT(t), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	return out
}

func structured(t *testing.T, out *sdk.CallToolResult) map[string]any {
	t.Helper()
	m, ok := out.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent is %T, want a JSON object: %v", out.StructuredContent, out.StructuredContent)
	}
	return m
}

func textOf(out *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range out.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func containsSeq(argv []string, want ...string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		ok := true
		for j, w := range want {
			if argv[i+j] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// `--tools raw_cdp` names the escape hatch explicitly, and the named-list
// branch honours that — so the description has to say so.
//
// It used to claim "exposed only under --tools full" while the named branch
// built its map from the whole registry without consulting `full`, which made
// the sentence false. Naming a tool is an explicit act by the user at a shell,
// not something an agent can reach, so the behaviour is right and the sentence
// was wrong. What actually bounds it now is that MCP mode denies the `raw` verb
// unless --allow-eval is passed.
func TestNamedToolListCanIncludeRawCDP(t *testing.T) {
	t.Parallel()
	specs, err := Specs(Options{Tools: "raw_cdp"})
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != prefix+"raw_cdp" {
		t.Fatalf("--tools raw_cdp selected %v", toolSpecNames(specs))
	}
	var desc string
	for _, tl := range registry() {
		if tl.name == prefix+"raw_cdp" {
			desc = tl.desc
		}
	}
	if strings.Contains(desc, "exposed only under --tools full") {
		t.Error("the description still claims --tools full is the only way in, which the named list disproves")
	}
	for _, want := range []string{"--tools full", "--tools`", "--allow-eval"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the description does not mention %q:\n%s", want, desc)
		}
	}
}

// The effective policy's denied verbs take a tool off the list entirely: it
// could only answer permission_denied, and an agent pays for the description.
func TestDeniedVerbsAreNotExposed(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{Tools: SetFull, DenyVerbs: []string{"eval", "raw"}})
	res, err := sess.ListTools(ctxT(t), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, gone := range []string{"evaluate", "raw_cdp"} {
		if hasTool(res.Tools, prefix+gone) {
			t.Errorf("%s is listed although its verb is denied", gone)
		}
	}
	if !hasTool(res.Tools, prefix+"snapshot") {
		t.Error("denying eval dropped an unrelated tool")
	}
	out := callTool(t, sess, prefix+"evaluate", map[string]any{"expression": "1+1"})
	if !out.IsError {
		t.Fatal("a denied tool was invocable")
	}
	got := structured(t, out)
	if got["code"] != "usage" {
		t.Errorf("code = %v, want usage", got["code"])
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "policy") {
		t.Errorf("message = %q, want it to name the policy rather than --tools", msg)
	}
	if r.count() != 0 {
		t.Errorf("a denied tool reached the CLI: %v", r.calls)
	}
}

func toolSpecNames(specs []ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}
