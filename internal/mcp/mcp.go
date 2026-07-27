// Package mcp serves chrome-cdp's verbs to Model Context Protocol clients over
// stdio (RFC-0004).
//
// It is a FRONT END, not a fork. A tool call is validated, turned into exactly
// the argv a user would type, and run through the same cobra command tree
// against the same chrome.Browser; the envelope that comes back is mapped onto
// the MCP result with its `code` and `exit` intact. Nothing about a verb is
// implemented twice, which is what keeps the two front ends from drifting —
// TestMCPParityWithCLI in internal/cli is the guard.
//
// Two invariants are load-bearing:
//
//   - stdout is protocol, stderr is diagnostics. Every byte this package writes
//     to stdout is a JSON-RPC frame. The command tree's human output is
//     redirected away from stdout for the life of the server (see
//     internal/cli/mcp.go), because one stray byte corrupts the stream and
//     surfaces as an unexplained client failure far from its cause.
//
//   - Errors keep the typed contract. A failure is `isError` with structured
//     content carrying `code` and `exit`, never prose, so an agent branches on
//     target_timeout vs permission_denied vs usage exactly as a shell script
//     does — including the recoverable signals (`tab_hidden`, `occluded`).
package mcp

import (
	"context"
	"fmt"
	"strings"
)

// Runner executes one CLI argv and hands back the JSON envelope it printed and
// the exit code it would have produced. It is the seam between this package and
// internal/cli; internal/cli implements it over a shared, connected App, and
// tests implement it with canned envelopes.
//
// The envelope bytes are exactly what `--json` writes to stdout, so a Runner
// that reimplemented any of it would be a fork by another name.
type Runner interface {
	Run(ctx context.Context, argv []string) (envelope []byte, exit int)
}

// tool is one registered MCP tool: the schema an agent sees, the CLI verbs it
// can reach, and the function that turns arguments into argv.
type tool struct {
	name string
	// title is the human-facing display name; description is what an agent
	// actually reads to decide whether and how to call it, so it carries the
	// addressing guidance rather than a restatement of the name.
	title string
	desc  string
	args  []arg

	// verbs lists every CLI verb path this tool can dispatch. It drives
	// --read-only (via the policy classification table, not a second table of
	// our own) and the flag-mirror test.
	verbs []string
	// disc names the discriminator argument ("action", "kind") when one value
	// selects the verb, and actions maps its values to those verbs. Together
	// they let --read-only narrow a grouped tool's enum instead of dropping the
	// whole tool.
	disc    string
	actions map[string]string

	// full marks a tool that only appears under `--tools full`.
	full bool
	// image marks a tool whose result carries an image content block.
	image bool

	// build returns the verb the call selects, the flag words it generated, and
	// the POSITIONAL values the caller supplied — the last two kept apart so
	// the server can emit the positionals after a `--` terminator.
	//
	// That separation is load-bearing, not tidiness. pflag parses flags and
	// positionals interspersed, so a positional whose value begins with "-" is
	// consumed as a FLAG wherever it sits in argv — and every positional here
	// is caller-controlled. A selector of "--policy-off" spliced ahead of the
	// generated flags turned the policy off for that call; the terminator is
	// what makes it data. It also makes the legitimate cases work: `type` with
	// the text "-foo", `key` with a keyspec, an `eval` expression starting with
	// a minus.
	//
	// It is called only after the arguments have been validated against args.
	build func(c *call) (verb string, flags []string, pos []string, err error)
}

// arg is one tool argument. flag names the CLI flag it mirrors, which is what
// the schema-conformance test checks against the live cobra tree: a renamed
// flag that is not mirrored here fails in CI rather than in a user's client.
//
// An arg with no flag is either positional (pos) or synthetic — a choice the
// tool makes for the caller, like type_text's `replace` picking `fill` over
// `type`.
type arg struct {
	name     string
	typ      string // "string" | "boolean" | "integer" | "number" | "array" | "object"
	desc     string
	flag     string
	pos      bool
	required bool
	enum     []string
	items    string // element type for typ == "array"
}

// usageError is a caller mistake: it maps to the envelope's `usage` code and
// exit 2, and it is produced BEFORE anything reaches the browser.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &usageError{msg: fmt.Sprintf(format, a...)} }

// call is one validated invocation: the tool plus its arguments, with the
// accessors the builders use.
type call struct {
	tool *tool
	args map[string]any
}

func (c *call) has(name string) bool {
	v, ok := c.args[name]
	return ok && v != nil
}

func (c *call) str(name string) string {
	s, _ := c.args[name].(string)
	return strings.TrimSpace(s)
}

func (c *call) bool(name string) bool {
	b, _ := c.args[name].(bool)
	return b
}

// flags renders every present argument that mirrors a CLI flag, skipping the
// names the builder consumed itself. Doing it generically here is what keeps
// each tool's builder to a few lines and keeps the flag spelling in exactly one
// place.
func (c *call) flags(skip ...string) []string {
	skipped := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipped[s] = true
	}
	var out []string
	for _, a := range c.tool.args {
		if a.flag == "" || skipped[a.name] || !c.has(a.name) {
			continue
		}
		out = append(out, flagArgs(a, c.args[a.name])...)
	}
	return out
}

// flagArgs renders one argument as CLI flag words: a bool is the bare flag when
// true and nothing when false, an array repeats the flag, everything else is
// flag + value.
func flagArgs(a arg, v any) []string {
	switch a.typ {
	case "boolean":
		if b, _ := v.(bool); b {
			return []string{"--" + a.flag}
		}
		return nil
	case "array":
		items, _ := v.([]any)
		out := make([]string, 0, len(items)*2)
		for _, it := range items {
			out = append(out, "--"+a.flag, scalarString(it))
		}
		return out
	default:
		s := scalarString(v)
		if s == "" {
			return nil
		}
		return []string{"--" + a.flag, s}
	}
}

// scalarString renders a JSON scalar the way a shell user would have typed it.
// Numbers arrive from encoding/json as float64, so an integer flag must not
// come out as "5" spelled "5.000000".
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", t), "0"), ".")
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
