package cli

// End-to-end tests for MCP mode, driven over an in-memory transport against
// the same stub browsers the CLI tests use.
//
// internal/mcp's own tests cover the protocol with a faked Runner. These cover
// what only this package can: that the command tree really is what runs, that
// the two front ends produce the same payloads (the parity test below), that a
// malformed call never reaches the browser, that the policy boundary holds, and
// that stdout carries nothing but protocol.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
	"github.com/sanketsudake/chrome-cdp-cli/internal/mcp"
	"github.com/sanketsudake/chrome-cdp-cli/internal/policy"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// mcpTimeout bounds every blocking call here. A hung test takes the suite with
// it, which is worse than a failing one.
const mcpTimeout = 20 * time.Second

var mcpTabs = []target.Info{
	{ID: "aa11", Title: "App", URL: "https://app.example.com/dash"},
	{ID: "bb22", Title: "Other", URL: "https://other.test/x"},
}

// serveMCP starts a server backed by app's command tree and returns a client
// session, torn down deterministically at the end of the test.
func serveMCP(t *testing.T, app *App, o mcp.Options) *sdk.ClientSession {
	t.Helper()
	srv, err := mcp.New(app.newMCPRunner(), o)
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	ct, st := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, st) }()

	cctx, ccancel := context.WithTimeout(context.Background(), mcpTimeout)
	defer ccancel()
	sess, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(cctx, ct, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = sess.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(mcpTimeout):
			t.Error("the MCP server did not shut down after the session closed")
		}
	})
	return sess
}

func mcpApp(t *testing.T, b chrome.Browser) *App {
	t.Helper()
	return New(b, &bytes.Buffer{}, &bytes.Buffer{})
}

func mcpCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mcpTimeout)
	t.Cleanup(cancel)
	return ctx
}

func mcpCall(t *testing.T, sess *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	out, err := sess.CallTool(mcpCtx(t), &sdk.CallToolParams{Name: "chrome_cdp_" + name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	return out
}

func mcpStructured(t *testing.T, out *sdk.CallToolResult) map[string]any {
	t.Helper()
	m, ok := out.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent is %T: %v", out.StructuredContent, out.StructuredContent)
	}
	return m
}

// VS-3: a tool call runs the real command against the real Browser seam, and
// the result is the envelope's result object.
func TestMCPCallRunsTheCommandTree(t *testing.T) {
	t.Parallel()
	b := &queryCapture{fakeBrowser: fakeBrowser{tabs: mcpTabs}}
	sess := serveMCP(t, mcpApp(t, b), mcp.Options{})
	out := mcpCall(t, sess, "click", map[string]any{
		"selector": "Save and Close", "by": "name", "role": "button", "target": "aa11",
	})
	if out.IsError {
		t.Fatalf("click failed: %v", mcpStructured(t, out))
	}
	if got := mcpStructured(t, out); got["clicked"] != "Save and Close" {
		t.Errorf("structuredContent = %v, want the click result", got)
	}
	// The addressing arguments reached QueryOpts, not just the argv.
	if b.gotQ.By != "name" || b.gotQ.Role != "button" {
		t.Errorf("QueryOpts = %+v, want by=name role=button", b.gotQ)
	}
}

// VS-4: a driver failure keeps its typed code and exit code.
func TestMCPFailurePreservesTheTypedContract(t *testing.T) {
	t.Parallel()
	b := &timingOutBrowser{fakeBrowser: fakeBrowser{tabs: mcpTabs}}
	sess := serveMCP(t, mcpApp(t, b), mcp.Options{})
	out := mcpCall(t, sess, "click", map[string]any{"selector": "Save", "by": "name", "target": "aa11"})
	if !out.IsError {
		t.Fatal("a timed-out click reported success")
	}
	got := mcpStructured(t, out)
	if got["code"] != result.CodeTargetTimeout {
		t.Errorf("code = %v, want %s", got["code"], result.CodeTargetTimeout)
	}
	if !numEquals(got["exit"], result.ExitTarget) {
		t.Errorf("exit = %v, want %d", got["exit"], result.ExitTarget)
	}
}

type timingOutBrowser struct {
	fakeBrowser
}

func (b *timingOutBrowser) Pointer(context.Context, string, string, chrome.PointerOpts) (map[string]any, error) {
	return nil, errors.New("timeout waiting for node")
}

// VS-5: a malformed call is `usage` and the browser is never contacted — the
// same contract the CLI gives, proved the same way.
func TestMCPUsageErrorsNeverReachTheBrowser(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		// Rejected by the schema's vocabulary, before any argv exists.
		{"unknown by", "click", map[string]any{"selector": "#x", "by": "xpath", "target": "aa11"}},
		// Rejected by the CLI itself, before it resolves a target.
		{"repeat out of range", "key", map[string]any{"keys": "ArrowDown", "repeat": 500, "target": "aa11"}},
		{"wait with no condition", "wait_for", map[string]any{"target": "aa11"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			sess := serveMCP(t, mcpApp(t, noCall(t)), mcp.Options{})
			out := mcpCall(t, sess, c.tool, c.args)
			if !out.IsError {
				t.Fatalf("%v was accepted", c.args)
			}
			got := mcpStructured(t, out)
			if got["code"] != result.CodeUsage {
				t.Errorf("code = %v, want usage", got["code"])
			}
			if !numEquals(got["exit"], result.ExitUsage) {
				t.Errorf("exit = %v, want 2", got["exit"])
			}
		})
	}
}

// VS-9: the policy boundary applies to a tool call exactly as it does to a
// command, and the refusal is typed rather than prose.
func TestMCPHonoursThePolicyAllowList(t *testing.T) {
	t.Parallel()
	b := refusing(t, mcpTabs...)
	app, _, _ := appWithPolicy(b, allowOnly("*.example.com"))
	sess := serveMCP(t, app, mcp.Options{})
	out := mcpCall(t, sess, "click", map[string]any{"selector": "#go", "target": "bb22"})
	if !out.IsError {
		t.Fatal("a click on a non-allowed origin was permitted")
	}
	got := mcpStructured(t, out)
	if got["code"] != result.CodePermissionDenied {
		t.Errorf("code = %v, want %s", got["code"], result.CodePermissionDenied)
	}
	if !numEquals(got["exit"], result.ExitPermission) {
		t.Errorf("exit = %v, want %d", got["exit"], result.ExitPermission)
	}
	if got["origin"] != "other.test" || got["rule"] != "allow: no match" {
		t.Errorf("the refusal must name the origin and the rule: %v", got)
	}
}

// VS-10, the connection half: a batch runs its steps over one connection.
func TestMCPBatchAcquiresTheConnectionOnce(t *testing.T) {
	t.Parallel()
	b := &connectCounter{fakeBrowser: fakeBrowser{tabs: mcpTabs}}
	app := New(nil, &bytes.Buffer{}, &bytes.Buffer{}).WithConnector(func(context.Context, ConnOpts) (chrome.Browser, error) {
		b.connects.Add(1)
		return b, nil
	})
	sess := serveMCP(t, app, mcp.Options{})
	out := mcpCall(t, sess, "batch", map[string]any{"steps": []any{
		map[string]any{"tool": "snapshot", "arguments": map[string]any{"target": "aa11"}},
		map[string]any{"tool": "read", "arguments": map[string]any{"kind": "text", "article": true, "target": "aa11"}},
		map[string]any{"tool": "click", "arguments": map[string]any{"selector": "#go", "target": "aa11"}},
	}})
	if out.IsError {
		t.Fatalf("batch failed: %v", mcpStructured(t, out))
	}
	if n := b.connects.Load(); n != 1 {
		t.Errorf("the batch connected %d times, want 1", n)
	}
	if got := mcpStructured(t, out); !numEquals(got["completed"], 3) {
		t.Errorf("completed = %v, want 3", got["completed"])
	}
}

type connectCounter struct {
	fakeBrowser
	connects atomic.Int32
}

// VS-12: cancelling a request cancels the work and leaves no goroutine behind.
func TestMCPCancellationStopsTheCallAndLeaksNothing(t *testing.T) {
	// Not parallel: it counts goroutines.
	b := &waitForeverBrowser{fakeBrowser: fakeBrowser{tabs: mcpTabs}, entered: make(chan struct{}), seen: make(chan error, 1)}
	sess := serveMCP(t, mcpApp(t, b), mcp.Options{})

	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, err := sess.CallTool(ctx, &sdk.CallToolParams{
			Name:      "chrome_cdp_wait_for",
			Arguments: map[string]any{"text": "never", "target": "aa11", "timeout": "5m"},
		})
		callDone <- err
	}()

	select {
	case <-b.entered:
	case <-time.After(mcpTimeout):
		t.Fatal("the wait never reached the driver")
	}
	cancel()

	select {
	case err := <-b.seen:
		if err == nil {
			t.Error("the driver's context was not cancelled")
		}
	case <-time.After(mcpTimeout):
		t.Fatal("cancelling the request did not cancel the driver's context")
	}
	select {
	case <-callDone:
	case <-time.After(mcpTimeout):
		t.Fatal("the cancelled call never returned")
	}

	// The handler, its AfterFunc and the command must all be gone. Poll rather
	// than sample once: teardown is asynchronous.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d, baseline %d — a cancelled call leaked", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type waitForeverBrowser struct {
	fakeBrowser
	entered chan struct{}
	once    sync.Once
	seen    chan error
}

func (b *waitForeverBrowser) Wait(ctx context.Context, _ string, _ chrome.WaitCond) (map[string]any, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	b.seen <- ctx.Err()
	return nil, ctx.Err()
}

// The parity test — the one that keeps US-2 true.
//
// Every row runs the same intent through both front ends against equivalent
// stubs and asserts the payloads are identical. It is cheap because both sides
// are stub-backed, and it is the structural guard the whole design rests on: if
// anyone reimplements a verb inside the MCP layer, or an argument stops mapping
// to the flag it mirrors, this fails.
func TestMCPParityWithCLI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		argv []string
		tool string
		args map[string]any
	}{
		{"list", []string{"list"}, "tabs", map[string]any{"action": "list"}},
		{"snap", []string{"snap", "--target", "aa11", "--role", "button"},
			"snapshot", map[string]any{"target": "aa11", "role": "button"}},
		{"text article", []string{"text", "--article", "--markdown", "--target", "aa11"},
			"read", map[string]any{"kind": "text", "article": true, "markdown": true, "target": "aa11"}},
		{"text selector", []string{"text", "#main", "--target", "aa11"},
			"read", map[string]any{"kind": "text", "selector": "#main", "target": "aa11"}},
		{"html inner", []string{"html", "#main", "--inner", "--target", "aa11"},
			"read", map[string]any{"kind": "html", "selector": "#main", "inner": true, "target": "aa11"}},
		{"grid", []string{"grid", "--target", "aa11"},
			"read", map[string]any{"kind": "grid", "target": "aa11"}},
		{"value", []string{"value", "#amount", "--target", "aa11"},
			"read", map[string]any{"kind": "value", "selector": "#amount", "target": "aa11"}},
		{"click by name", []string{"click", "Save", "--by", "name", "--role", "button", "--target", "aa11"},
			"click", map[string]any{"selector": "Save", "by": "name", "role": "button", "target": "aa11"}},
		{"type", []string{"type", "#a", "hello", "--target", "aa11"},
			"type_text", map[string]any{"selector": "#a", "text": "hello", "target": "aa11"}},
		{"fill", []string{"fill", "#a", "hello", "--target", "aa11"},
			"type_text", map[string]any{"selector": "#a", "text": "hello", "replace": true, "target": "aa11"}},
		{"key", []string{"key", "#a", "cmd+a", "--repeat", "2", "--target", "aa11"},
			"key", map[string]any{"selector": "#a", "keys": "cmd+a", "repeat": 2, "target": "aa11"}},
		{"hover", []string{"hover", "#menu", "--target", "aa11"},
			"pointer", map[string]any{"action": "hover", "selector": "#menu", "target": "aa11"}},
		{"select", []string{"select", "Time Type", "Sick", "--target", "aa11"},
			"select_option", map[string]any{"field": "Time Type", "option": "Sick", "target": "aa11"}},
		{"scroll", []string{"scroll", "--dy", "400", "--target", "aa11"},
			"scroll", map[string]any{"dy": 400, "target": "aa11"}},
		{"nav", []string{"nav", "https://app.example.com/next", "--target", "aa11"},
			"navigate", map[string]any{"url": "https://app.example.com/next", "target": "aa11"}},
		{"wait text", []string{"wait", "--text", "Saved", "--target", "aa11"},
			"wait_for", map[string]any{"text": "Saved", "target": "aa11"}},
		{"console errors", []string{"console", "--only-errors", "--limit", "5", "--target", "aa11"},
			"console", map[string]any{"only_errors": true, "limit": 5, "target": "aa11"}},
		{"net xhr", []string{"net", "--xhr", "--status", "2xx", "--target", "aa11"},
			"network", map[string]any{"xhr": true, "status": "2xx", "target": "aa11"}},
		{"eval", []string{"eval", "1+1", "--target", "aa11"},
			"evaluate", map[string]any{"expression": "1+1", "target": "aa11"}},
		{"raw", []string{"raw", "Page.getNavigationHistory", "--target", "aa11"},
			"raw_cdp", map[string]any{"method": "Page.getNavigationHistory", "target": "aa11"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cliEnv, _, cliExit := run(t, &fakeBrowser{tabs: mcpTabs}, append(c.argv, "--json")...)
			sess := serveMCP(t, mcpApp(t, &fakeBrowser{tabs: mcpTabs}), mcp.Options{Tools: mcp.SetFull})
			out := mcpCall(t, sess, c.tool, c.args)

			if (cliExit == 0) != !out.IsError {
				t.Fatalf("the front ends disagree on success: cli exit %d, mcp isError %v (%v)",
					cliExit, out.IsError, out.StructuredContent)
			}
			var want any
			if cliExit == 0 {
				want = cliEnv["result"]
			} else {
				want = cliEnv["error"]
			}
			got := out.StructuredContent
			if cliExit != 0 {
				// The MCP side adds `exit` to the error object; it is derived
				// from the CLI's own exit code, so compare the rest.
				m := map[string]any{}
				for k, v := range mcpStructured(t, out) {
					m[k] = v
				}
				if !numEquals(m["exit"], cliExit) {
					t.Errorf("exit = %v, want the CLI's %d", m["exit"], cliExit)
				}
				delete(m, "exit")
				got = m
			}
			if !sameJSON(want, got) {
				t.Errorf("payloads differ.\n cli: %s\n mcp: %s", mustJSON(want), mustJSON(got))
			}
		})
	}
}

// VS-2, the conformance half: every argument still mirrors a live CLI flag, and
// every verb a tool claims is a registered command.
//
// A flag renamed without mirroring it here would otherwise be discovered by a
// user whose client stopped working.
func TestMCPToolArgumentsMirrorCLIFlags(t *testing.T) {
	t.Parallel()
	app := New(&fakeBrowser{}, &bytes.Buffer{}, &bytes.Buffer{})
	root := app.newRoot()
	specs, err := mcp.Specs(mcp.Options{Tools: mcp.SetFull})
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if len(specs) == 0 {
		t.Fatal("no tools to check")
	}
	registered := map[string]bool{}
	for _, path := range runnableCommands(root) {
		registered[path] = true
	}
	for _, spec := range specs {
		for _, verb := range spec.Verbs {
			if !registered[verb] {
				t.Errorf("tool %s claims the verb %q, which is not a registered command", spec.Name, verb)
			}
			if _, known := policy.Classify(verb); !known {
				t.Errorf("tool %s claims the verb %q, which the policy table does not classify — --read-only cannot decide about it", spec.Name, verb)
			}
		}
		for _, a := range spec.Args {
			if a.Flag == "" {
				continue
			}
			if want := strings.ReplaceAll(a.Flag, "-", "_"); a.Name != want {
				t.Errorf("tool %s: argument %q mirrors --%s and should be named %q", spec.Name, a.Name, a.Flag, want)
			}
			if !anyVerbHasFlag(root, spec.Verbs, a.Flag) {
				t.Errorf("tool %s: argument %q mirrors --%s, which none of its verbs (%s) accepts",
					spec.Name, a.Name, a.Flag, strings.Join(spec.Verbs, ", "))
			}
		}
	}
}

func anyVerbHasFlag(root *cobra.Command, verbs []string, flag string) bool {
	for _, verb := range verbs {
		cmd, _, err := root.Find(strings.Split(verb, " "))
		if err != nil || cmd == nil {
			continue
		}
		if cmd.Flag(flag) != nil {
			return true
		}
	}
	return false
}

// VS-11 (the start-up half of US-6): MCP mode refuses to run without a policy
// allow-list, exits nonzero, and prints the configuration it needs.
func TestMCPRefusesToStartWithoutAPolicy(t *testing.T) {
	t.Parallel()
	t.Run("no policy at all", func(t *testing.T) {
		t.Parallel()
		var out, errb bytes.Buffer
		app := New(noCall(t), &out, &errb).WithInput(strings.NewReader(""))
		code := app.Execute("mcp")
		if code != result.ExitUsage {
			t.Fatalf("exit = %d, want %d", code, result.ExitUsage)
		}
		if !strings.Contains(errb.String(), "[policy]") || !strings.Contains(errb.String(), "allow =") {
			t.Errorf("the refusal must print the configuration it needs:\n%s", errb.String())
		}
		if strings.Contains(out.String(), "jsonrpc") {
			t.Errorf("a refused server still wrote protocol to stdout:\n%s", out.String())
		}
	})
	t.Run("a table with an empty allow-list", func(t *testing.T) {
		t.Parallel()
		app, _, errb := appWithPolicy(noCall(t), config.Policy{Present: true, Enabled: true, Deny: []string{"bank.example"}, Source: "/test/config.toml"})
		app.WithInput(strings.NewReader(""))
		if code := app.Execute("mcp"); code != result.ExitUsage {
			t.Fatalf("exit = %d, want %d", code, result.ExitUsage)
		}
		if !strings.Contains(errb.String(), "allow") {
			t.Errorf("stderr = %q, want it to ask for an allow-list", errb.String())
		}
	})
	t.Run("--policy-off", func(t *testing.T) {
		t.Parallel()
		app, _, _ := appWithPolicy(noCall(t), allowOnly("*.example.com"))
		app.WithInput(strings.NewReader(""))
		if code := app.Execute("mcp", "--policy-off"); code != result.ExitUsage {
			t.Fatalf("exit = %d, want %d — --policy-off must not serve", code, result.ExitUsage)
		}
	})
}

// VS-1 and VS-6, through the real command: the server speaks the protocol on
// stdout and nothing else does.
func TestMCPStdoutCarriesOnlyProtocol(t *testing.T) {
	t.Parallel()
	// One pipe each way: the client writes what the server reads as stdin, and
	// the server's stdout is teed into a buffer so every byte can be checked.
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()

	var stdout syncBuffer
	app := New(&fakeBrowser{tabs: mcpTabs}, &teeWriter{to: serverOut, copy: &stdout}, &bytes.Buffer{})
	app.WithInput(serverIn)

	served := make(chan int, 1)
	go func() { served <- app.Execute("mcp", "--verbose", "--allow", "*.example.com") }()

	sess, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).
		Connect(mcpCtx(t), &sdk.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if v := sess.InitializeResult().ProtocolVersion; v == "" {
		t.Error("no protocol version in the handshake")
	}
	tools, err := sess.ListTools(mcpCtx(t), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("no tools listed")
	}
	// Verbs whose CLI form prints human output to stdout.
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"tabs", map[string]any{"action": "list"}},
		{"read", map[string]any{"kind": "text", "article": true, "target": "aa11"}},
		{"snapshot", map[string]any{"target": "aa11"}},
	} {
		if out := mcpCall(t, sess, c.tool, c.args); out.IsError {
			t.Fatalf("%s failed: %v", c.tool, out.StructuredContent)
		}
	}

	_ = sess.Close()
	_ = clientOut.Close()
	select {
	case code := <-served:
		if code != result.ExitOK {
			t.Errorf("the server exited %d, want 0", code)
		}
	case <-time.After(mcpTimeout):
		t.Fatal("the server did not exit after the client disconnected")
	}

	// Every byte on stdout must be protocol framing.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 4 {
		t.Fatalf("stdout carried %d frames, want the handshake plus the calls:\n%s", len(lines), stdout.String())
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("stdout line %d is not a protocol frame: %v\n%s", i+1, err, line)
		}
		if frame["jsonrpc"] != "2.0" {
			t.Errorf("stdout line %d is JSON but not JSON-RPC: %s", i+1, line)
		}
	}
	if errs := app.err.(*bytes.Buffer).String(); !strings.Contains(errs, "serving") {
		t.Errorf("a verbose run wrote no diagnostics to stderr: %q", errs)
	}
}

// A write that would have corrupted the protocol is suppressed and reported,
// rather than reaching the stream.
func TestProtectStdoutSuppressesStrayWrites(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	app := New(&fakeBrowser{}, &out, &errb)
	protocol := app.protectStdout()
	if protocol != io.Writer(&out) {
		t.Fatal("protectStdout did not hand back the original stdout")
	}
	fmt.Fprintln(app.out, "hello from a library")
	if out.Len() != 0 {
		t.Errorf("a stray write reached the protocol stream: %q", out.String())
	}
	if !strings.Contains(errb.String(), "hello from a library") {
		t.Errorf("the stray write was not reported on stderr: %q", errb.String())
	}
}

// teeWriter copies everything written to the protocol stream into a buffer the
// test can inspect.
type teeWriter struct {
	to   io.Writer
	copy *syncBuffer
	mu   sync.Mutex
}

func (w *teeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.copy.Write(p)
	w.mu.Unlock()
	return w.to.Write(p)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func numEquals(v any, want int) bool {
	switch n := v.(type) {
	case float64:
		return int(n) == want
	case int:
		return n == want
	case json.Number:
		i, err := n.Int64()
		return err == nil && int(i) == want
	}
	return false
}

func sameJSON(a, b any) bool { return string(mustJSON(a)) == string(mustJSON(b)) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(err.Error())
	}
	return b
}
