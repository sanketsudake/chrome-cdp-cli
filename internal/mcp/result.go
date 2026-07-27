package mcp

// Envelope → MCP result.
//
// The mapping is deliberately thin. A success carries the envelope's `result`
// object through UNCHANGED as structured content (RFC-0004 VS-3), and a failure
// carries the error object with its `code` and `exit` (VS-4) rather than being
// flattened into prose. That is the whole point: an agent branches on
// target_timeout vs permission_denied vs usage exactly as a shell script does,
// and reads the recoverable signals — tab_hidden, occluded, zero_area — from
// the same details the CLI reports.
//
// The text content block is a one-line human summary. It is a convenience for a
// model reading the transcript, never the contract.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// outcome is one command's envelope, decoded.
type outcome struct {
	OK      bool
	Command string
	Result  any
	Err     map[string]any
	Exit    int
	Target  map[string]any

	image []byte
	mime  string
}

// envelopeJSON mirrors result.Envelope for decoding. Numbers stay json.Number
// so a payload's values are handed to the client exactly as the CLI printed
// them — a request id or a large integer must not acquire an exponent on the
// way through.
type envelopeJSON struct {
	OK      bool            `json:"ok"`
	Command string          `json:"command"`
	Target  map[string]any  `json:"target"`
	Result  json.RawMessage `json:"result"`
	Error   map[string]any  `json:"error"`
}

// mapEnvelope decodes what the CLI printed. exit is the process exit code the
// same invocation would have produced, and it is what travels in `exit`.
func mapEnvelope(env []byte, exit int) *outcome {
	trimmed := strings.TrimSpace(string(env))
	if trimmed == "" {
		return &outcome{
			Err: errObject(result.CodeGeneric, "the command produced no result envelope", nil),
			// A command that printed nothing still failed with its own exit
			// code; reporting exit 0 alongside an error would be a contradiction.
			Exit: nonZero(exit),
		}
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var e envelopeJSON
	if err := dec.Decode(&e); err != nil {
		return &outcome{
			Err:  errObject(result.CodeGeneric, fmt.Sprintf("the command's output was not a result envelope: %v", err), map[string]any{"output": truncate(trimmed, 400)}),
			Exit: nonZero(exit),
		}
	}
	o := &outcome{OK: e.OK, Command: e.Command, Target: e.Target, Exit: exit}
	if e.OK {
		o.Result = decodeResult(e.Result)
		return o
	}
	o.Err = e.Error
	if o.Err == nil {
		o.Err = errObject(result.CodeGeneric, "the command failed without reporting an error", nil)
	}
	// `exit` completes the pair an agent branches on. It comes from the process
	// exit code the CLI would have returned, falling back to the code table when
	// a caller ran us without one.
	if o.Exit == 0 {
		code, _ := o.Err["code"].(string)
		o.Exit = result.ExitCodeFor(code)
	}
	o.Err["exit"] = o.Exit
	return o
}

func nonZero(exit int) int {
	if exit == 0 {
		return result.ExitGeneric
	}
	return exit
}

// decodeResult keeps the payload as it was, except that structured content must
// be a JSON object: a payload that is not one is wrapped rather than dropped.
func decodeResult(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return map[string]any{"value": string(raw)}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{"value": v}
}

func errObject(code, msg string, details map[string]any) map[string]any {
	m := make(map[string]any, len(details)+3)
	for k, v := range details {
		m[k] = v
	}
	m["code"] = code
	m["message"] = msg
	m["exit"] = result.ExitCodeFor(code)
	return m
}

// toolResult renders the outcome as the MCP result.
func (o *outcome) toolResult() *sdk.CallToolResult {
	res := &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: o.summary()}}}
	if len(o.image) > 0 {
		res.Content = append(res.Content, &sdk.ImageContent{Data: o.image, MIMEType: o.mime})
	}
	if o.OK {
		res.StructuredContent = o.Result
		return res
	}
	res.IsError = true
	res.StructuredContent = o.Err
	return res
}

// usageResult is a caller mistake caught before the browser was contacted.
func usageResult(err error) *sdk.CallToolResult {
	return errorResult(result.CodeUsage, err.Error(), nil)
}

func errorResult(code, msg string, details map[string]any) *sdk.CallToolResult {
	o := &outcome{Err: errObject(code, msg, details), Exit: result.ExitCodeFor(code)}
	return o.toolResult()
}

// summary is the one-line human rendering in the text content block.
func (o *outcome) summary() string {
	if !o.OK {
		code, _ := o.Err["code"].(string)
		msg, _ := o.Err["message"].(string)
		var hints []string
		for _, k := range []string{"tab_hidden", "occluded", "zero_area", "no_history"} {
			if b, ok := o.Err[k].(bool); ok && b {
				hints = append(hints, k)
			}
		}
		line := fmt.Sprintf("%s: %s", code, msg)
		if len(hints) > 0 {
			line += " [" + strings.Join(hints, ", ") + "]"
		}
		return line
	}
	line := o.Command
	if line == "" {
		line = "ok"
	}
	if d := describe(o.Result); d != "" {
		line += " — " + d
	}
	if u, _ := o.Target["url"].(string); u != "" {
		line += " (" + u + ")"
	}
	return line
}

// describe picks the most informative few fields of a result for the summary
// line, in a fixed order so the rendering is stable.
func describe(res any) string {
	m, ok := res.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	for _, k := range []string{"url", "clicked", "typed", "filled", "selected", "value", "text", "waited", "path", "count"} {
		if v, ok := m[k]; ok {
			return fmt.Sprintf("%s: %s", k, truncate(fmt.Sprintf("%v", v), 120))
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 6 {
		keys = keys[:6]
	}
	return strings.Join(keys, ", ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
