package mcp

// The server: tool selection, request handling, and the transport.
//
// Every tool call takes the same path — validate, build argv, run it through
// the CLI command tree, map the envelope — so there is one place where the
// typed contract is preserved and one place where a mistake would break it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sanketsudake/chrome-cdp-cli/internal/policy"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// Tool set names for Options.Tools.
const (
	SetDefault = "default"
	SetFull    = "full"
)

// maxDefaultTools is RFC-0004 US-4's budget, asserted by a test rather than
// left as an intention: an agent pays for every tool description in its context
// window, and a browser server that crowds out everything else is not adopted.
const maxDefaultTools = 18

// Options configure a server. The zero value exposes the default tool set with
// no pinned tab.
type Options struct {
	// ReadOnly exposes only tools that cannot modify page state. The decision
	// comes from the policy layer's verb classification (internal/policy), not
	// from a second table here: one classification table means `--read-only`
	// and a `read_only` policy origin can never disagree.
	ReadOnly bool
	// Tools is "default", "full", or a comma-separated list of tool names
	// (with or without the chrome_cdp_ prefix).
	Tools string
	// Target pins every call to one tab. With it set, a call that names a
	// different tab is refused rather than silently redirected.
	Target string
	// Version is reported to the client in the initialize handshake.
	Version string
	// Log receives human diagnostics. It must never be stdout: stdout carries
	// the protocol.
	Log io.Writer
}

// Server is the MCP front end. Build one with New and serve it with Run.
type Server struct {
	runner Runner
	opts   Options
	sdk    *sdk.Server

	// exposed is the selected tool set, by name, and actions holds the
	// discriminator values each grouped tool still accepts after --read-only
	// filtering.
	exposed map[string]*tool
	order   []*tool
	actions map[string][]string
}

// New builds a server exposing the tools o selects, backed by r.
func New(r Runner, o Options) (*Server, error) {
	sel, err := o.selection()
	if err != nil {
		return nil, err
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	s := &Server{
		runner:  r,
		opts:    o,
		exposed: make(map[string]*tool, len(sel)),
		order:   sel,
		actions: map[string][]string{},
	}
	for _, t := range sel {
		s.exposed[t.name] = t
		s.actions[t.name] = o.allowedActions(t)
	}

	s.sdk = sdk.NewServer(&sdk.Implementation{
		Name:    "chrome-cdp",
		Title:   "chrome-cdp — drive your logged-in Chrome",
		Version: o.Version,
	}, &sdk.ServerOptions{
		Instructions: instructions(o),
	})
	for _, t := range sel {
		s.sdk.AddTool(&sdk.Tool{
			Name:        t.name,
			Title:       t.title,
			Description: t.desc,
			InputSchema: t.schema(s.enumFor(t), s.hidden),
		}, s.handler(t))
	}
	// A call to a tool this server did not expose (hidden by --read-only or
	// --tools) must come back as the same typed `usage` error every other bad
	// call does, not as a protocol error an agent cannot branch on.
	s.sdk.AddReceivingMiddleware(s.hiddenToolMiddleware)
	return s, nil
}

func instructions(o Options) string {
	var b strings.Builder
	b.WriteString("Drives the user's own already-running Chrome over the DevTools Protocol, reusing their live logins and cookies — no credentials are typed and no second browser is launched. " +
		"Start with chrome_cdp_tabs(action=\"list\") to see what is open and chrome_cdp_tabs(action=\"use\") to pin one, then chrome_cdp_snapshot to read the page. " +
		"Address elements by their ARIA accessible name (by=\"name\") on dynamic applications; CSS selectors break there. " +
		"Group several steps into chrome_cdp_batch to save round trips. Actions are bounded by the user's configured policy: a refusal comes back as permission_denied and is final, not something to retry.")
	if o.ReadOnly {
		b.WriteString("\n\nThis server is running READ-ONLY: only reading verbs are exposed and nothing can modify a page.")
	}
	if o.Target != "" {
		b.WriteString("\n\nThis server is pinned to one tab; the `target` argument is not accepted.")
	}
	return b.String()
}

// selection resolves the configured tool set, applying --tools and --read-only.
func (o Options) selection() ([]*tool, error) {
	all := registry()
	set := strings.TrimSpace(o.Tools)
	var chosen []*tool
	switch set {
	case "", SetDefault:
		for _, t := range all {
			if !t.full {
				chosen = append(chosen, t)
			}
		}
	case SetFull:
		chosen = all
	default:
		byName := map[string]*tool{}
		for _, t := range all {
			byName[t.name] = t
			byName[strings.TrimPrefix(t.name, prefix)] = t
		}
		seen := map[string]bool{}
		for _, name := range strings.Split(set, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			t, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("--tools: unknown tool %q (known: %s)", name, strings.Join(toolNames(all), ", "))
			}
			if seen[t.name] {
				continue
			}
			seen[t.name] = true
			chosen = append(chosen, t)
		}
		if len(chosen) == 0 {
			return nil, fmt.Errorf("--tools: no tools selected — pass %q, %q, or a comma-separated list of tool names", SetDefault, SetFull)
		}
		// Keep registry order however the list was written, so tools/list is
		// stable and reviewable.
		sort.SliceStable(chosen, func(i, j int) bool { return indexOf(all, chosen[i]) < indexOf(all, chosen[j]) })
	}
	if !o.ReadOnly {
		return chosen, nil
	}
	var ro []*tool
	for _, t := range chosen {
		if len(o.allowedActions(t)) > 0 || !t.mutates() {
			ro = append(ro, t)
		}
	}
	return ro, nil
}

// allowedActions returns the discriminator values a grouped tool still accepts.
// It is empty for a tool without a discriminator, and under --read-only it
// drops the actions whose verb the policy layer classifies as mutating — which
// is how `tabs` keeps list/use/activate/close while losing `open`.
func (o Options) allowedActions(t *tool) []string {
	if len(t.actions) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.actions))
	for value, verb := range t.actions {
		if o.ReadOnly && mutates(verb) {
			continue
		}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// mutates reports whether any of a tool's verbs modifies page state, per the
// policy classification table.
func (t *tool) mutates() bool {
	for _, v := range t.verbs {
		if mutates(v) {
			return true
		}
	}
	return false
}

func mutates(verb string) bool {
	class, _ := policy.Classify(verb)
	return class == policy.Mutating
}

func toolNames(ts []*tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.name)
	}
	return out
}

func indexOf(all []*tool, t *tool) int {
	for i, c := range all {
		if c == t {
			return i
		}
	}
	return len(all)
}

// enumFor exposes the schema enum for one argument, narrowed to the actions
// this server still accepts.
func (s *Server) enumFor(t *tool) func(arg) []string {
	return func(a arg) []string {
		if t.disc != "" && a.name == t.disc {
			if allowed := s.actions[t.name]; len(allowed) > 0 {
				return allowed
			}
		}
		return a.enum
	}
}

// hidden drops arguments this server does not accept. A pinned server offers no
// `target`: advertising an argument whose only valid value is the one already
// chosen invites a client to send the wrong one.
func (s *Server) hidden(a arg) bool {
	return a.name == "target" && s.opts.Target != ""
}

// Run serves the protocol over r/w until ctx ends or the client disconnects.
// In production these are stdin and the process's real stdout, and nothing else
// is allowed to write to the latter.
func (s *Server) Run(ctx context.Context, r io.Reader, w io.Writer) error {
	return s.Serve(ctx, &sdk.IOTransport{Reader: io.NopCloser(r), Writer: nopWriteCloser{w}})
}

// Serve runs the server on any SDK transport. Tests use an in-memory pair
// rather than a subprocess.
func (s *Server) Serve(ctx context.Context, t sdk.Transport) error {
	return s.sdk.Run(ctx, t)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// hiddenToolMiddleware turns a call to a known-but-not-exposed chrome-cdp tool
// into the typed `usage` refusal (RFC-0004 VS-7, VS-8). Anything else — an
// unrelated tool name, another method — is left to the SDK.
func (s *Server) hiddenToolMiddleware(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		params, ok := req.GetParams().(*sdk.CallToolParamsRaw)
		if !ok || params == nil {
			return next(ctx, method, req)
		}
		if _, exposed := s.exposed[params.Name]; exposed || !strings.HasPrefix(params.Name, prefix) {
			return next(ctx, method, req)
		}
		reason := fmt.Sprintf("tool %q is not exposed by this server", params.Name)
		if known(params.Name) {
			switch {
			case s.opts.ReadOnly:
				reason = fmt.Sprintf("%s: it is running --read-only, which exposes only tools that cannot modify a page", reason)
			default:
				reason = fmt.Sprintf("%s: it was started with a --tools allow-list", reason)
			}
		}
		reason += ". Exposed tools: " + strings.Join(toolNames(s.order), ", ")
		return errorResult(result.CodeUsage, reason, nil), nil
	}
}

func known(name string) bool {
	for _, t := range registry() {
		if t.name == name {
			return true
		}
	}
	return false
}

// handler wires one tool to the shared call path.
func (s *Server) handler(t *tool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var raw map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &raw); err != nil {
				return errorResult(result.CodeUsage, fmt.Sprintf("%s: arguments must be a JSON object: %v", t.name, err), nil), nil
			}
		}
		if t.name == prefix+"batch" {
			return s.runBatch(ctx, t, raw), nil
		}
		return s.callTool(ctx, t, raw), nil
	}
}

// callTool is the single path from arguments to an MCP result: validate, build
// argv, run it through the CLI, map the envelope.
func (s *Server) callTool(ctx context.Context, t *tool, raw map[string]any) *sdk.CallToolResult {
	args, err := t.validate(raw, s.enumFor(t))
	if err != nil {
		return usageResult(err)
	}
	if err := s.applyTarget(args); err != nil {
		return usageResult(err)
	}
	c := &call{tool: t, args: args}
	verb, flags, pos, err := t.build(c)
	if err != nil {
		return usageResult(err)
	}
	// Belt to the enum's braces: even if a grouped tool's filtering missed a
	// value, a mutating verb never runs on a --read-only server.
	if s.opts.ReadOnly && mutates(verb) {
		return usageResult(usagef("%s: %q modifies the page and this server is running --read-only", t.name, verb))
	}

	// The capture verbs return their bytes through a file. With no `output`
	// asked for, that file is temporary and its path is not reported: naming a
	// directory that no longer exists would be worse than saying nothing.
	var shot *capture
	if t.image {
		shot, err = newCapture(args)
		if err != nil {
			return errorResult(result.CodeGeneric, err.Error(), nil)
		}
		defer shot.cleanup()
		flags = append(flags, shot.flags()...)
	}

	env, exit := s.runner.Run(ctx, argvFor(verb, flags, pos))
	res := mapEnvelope(env, exit)
	if shot != nil && res.OK {
		if err := shot.attach(res); err != nil {
			fmt.Fprintf(s.opts.Log, "chrome-cdp mcp: %v\n", err)
		}
	}
	return res.toolResult()
}

// argvFor assembles the command line: the verb, then every generated flag,
// then a `--` terminator, then the caller's positional values.
//
// The terminator is the security-shaped part. Tool arguments are attacker-
// controlled in the threat model this server has (the caller is an agent
// reading untrusted page content), and pflag consumes ANY word beginning with
// "-" as a flag wherever it appears — so a `selector` of "--policy-off" or
// "--allow=evil.test", spliced in ahead of the flags the way the builders used
// to, silently turned the boundary off for that call. Everything after `--` is
// a positional to pflag, whatever it looks like, which also fixes the honest
// cases: typing the text "-foo", or pressing a key spelled with a minus.
//
// --json goes in with the flags for the same reason: it must be parsed as a
// flag, so it cannot sit after the terminator.
func argvFor(verb string, flags, pos []string) []string {
	argv := append(strings.Fields(verb), flags...)
	argv = append(argv, "--json")
	if len(pos) > 0 {
		argv = append(argv, "--")
		argv = append(argv, pos...)
	}
	return argv
}

// applyTarget folds the pinned --target into a call. A pinned server that
// silently ignored a different `target` would act on a tab the client did not
// name, so it refuses instead.
func (s *Server) applyTarget(args map[string]any) error {
	if s.opts.Target == "" {
		return nil
	}
	if v, ok := args["target"].(string); ok && strings.TrimSpace(v) != "" && v != s.opts.Target {
		return usagef("this server is pinned to one tab (--target %q); remove the `target` argument", s.opts.Target)
	}
	args["target"] = s.opts.Target
	return nil
}

// runBatch runs several tool calls over the one held connection, in order,
// stopping at the first failure (RFC-0004 VS-10).
//
// The batch's own result is an error when a step failed, carrying the failing
// step's code so a caller branches on the same contract a single call gives it.
func (s *Server) runBatch(ctx context.Context, t *tool, raw map[string]any) *sdk.CallToolResult {
	args, err := t.validate(raw, s.enumFor(t))
	if err != nil {
		return usageResult(err)
	}
	steps, _ := args["steps"].([]any)
	if len(steps) == 0 {
		return usageResult(usagef("batch: `steps` must list at least one step"))
	}

	// Validate the whole plan BEFORE running any of it: a batch whose third
	// step names a tool that does not exist should not have run the first two.
	type planned struct {
		tool *tool
		args map[string]any
	}
	plan := make([]planned, 0, len(steps))
	for i, raw := range steps {
		m, ok := raw.(map[string]any)
		if !ok {
			return usageResult(usagef("batch: step %d must be an object of {tool, arguments}", i+1))
		}
		name, _ := m["tool"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return usageResult(usagef("batch: step %d has no `tool`", i+1))
		}
		if !strings.HasPrefix(name, prefix) {
			name = prefix + name
		}
		if name == t.name {
			return usageResult(usagef("batch: step %d is another batch; flatten the steps instead", i+1))
		}
		st, ok := s.exposed[name]
		if !ok {
			return usageResult(usagef("batch: step %d names %q, which this server does not expose (exposed: %s)", i+1, name, strings.Join(toolNames(s.order), ", ")))
		}
		stepArgs, _ := m["arguments"].(map[string]any)
		if stepArgs == nil {
			stepArgs = map[string]any{}
		}
		for k := range m {
			if k != "tool" && k != "arguments" {
				return usageResult(usagef("batch: step %d has an unknown field %q (want tool, arguments)", i+1, k))
			}
		}
		plan = append(plan, planned{tool: st, args: stepArgs})
	}

	results := make([]any, 0, len(plan))
	var failure map[string]any
	var content []sdk.Content
	for i, p := range plan {
		out := s.callTool(ctx, p.tool, p.args)
		step := map[string]any{"step": i + 1, "tool": p.tool.name, "ok": !out.IsError}
		if out.IsError {
			step["error"] = out.StructuredContent
		} else {
			step["result"] = out.StructuredContent
		}
		results = append(results, step)
		// An image content block belongs to the batch's content, not to the
		// structured summary — that is the only place a client can render it.
		for _, cc := range out.Content {
			if img, ok := cc.(*sdk.ImageContent); ok {
				content = append(content, img)
			}
		}
		if out.IsError {
			failure = map[string]any{"step": i + 1, "tool": p.tool.name}
			if m, ok := out.StructuredContent.(map[string]any); ok {
				failure["code"] = m["code"]
				failure["message"] = m["message"]
			}
			break
		}
	}

	payload := map[string]any{
		"steps":     len(plan),
		"completed": len(results),
		"results":   results,
		"failed":    failure, // explicit null when every step succeeded
	}
	if failure != nil {
		code, _ := failure["code"].(string)
		if code == "" {
			code = result.CodeGeneric
		}
		payload["code"] = code
		payload["exit"] = result.ExitCodeFor(code)
		payload["message"] = fmt.Sprintf("batch stopped at step %v (%v): %v", failure["step"], failure["tool"], failure["message"])
		out := &sdk.CallToolResult{
			Content:           append([]sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("%s: %v", code, payload["message"])}}, content...),
			StructuredContent: payload,
			IsError:           true,
		}
		return out
	}
	return &sdk.CallToolResult{
		Content:           append([]sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("batch ok — %d step(s)", len(plan))}}, content...),
		StructuredContent: payload,
	}
}

// capture routes a screenshot's bytes back to the client. The CLI writes an
// image to a file; MCP wants it inline, so a call that named no `output` gets a
// temporary file that is read back and removed.
type capture struct {
	path      string
	tmpDir    string
	mime      string
	reportDir bool
	data      []byte
}

var mimeFor = map[string]string{"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp"}
var extFor = map[string]string{"png": "png", "jpeg": "jpg", "webp": "webp"}

func newCapture(args map[string]any) (*capture, error) {
	format, _ := args["format"].(string)
	if format == "" {
		format = "png"
	}
	c := &capture{mime: mimeFor[format]}
	if c.mime == "" {
		// An unknown format is the CLI's error to report, with its own message;
		// capture just needs somewhere to put the bytes.
		c.mime = "application/octet-stream"
	}
	if out, _ := args["output"].(string); strings.TrimSpace(out) != "" {
		c.path, c.reportDir = out, true
		return c, nil
	}
	dir, err := os.MkdirTemp("", "chrome-cdp-mcp")
	if err != nil {
		return nil, fmt.Errorf("cannot create a temporary directory for the capture: %w", err)
	}
	ext := extFor[format]
	if ext == "" {
		ext = "bin"
	}
	c.tmpDir = dir
	c.path = filepath.Join(dir, "capture."+ext)
	return c, nil
}

func (c *capture) flags() []string { return []string{"--output", c.path} }

func (c *capture) cleanup() {
	if c.tmpDir != "" {
		_ = os.RemoveAll(c.tmpDir)
	}
}

// attach reads the captured bytes back and folds them into the outcome. A
// temporary path is stripped from the structured result: it is about to be
// deleted, and reporting a path that does not exist is worse than reporting
// none.
func (c *capture) attach(o *outcome) error {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return fmt.Errorf("capture written but unreadable at %s: %w", c.path, err)
	}
	o.image, o.mime = data, c.mime
	if m, ok := o.Result.(map[string]any); ok && !c.reportDir {
		delete(m, "path")
	}
	return nil
}
