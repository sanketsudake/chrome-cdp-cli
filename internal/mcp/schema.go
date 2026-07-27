package mcp

// Schema generation and argument validation.
//
// Validation runs HERE, before any argv is built, and a rejection is the
// envelope's `usage` code with exit 2 and no browser contacted — the same
// contract the CLI gives a malformed invocation (RFC-0004 VS-5). It is written
// against the declared argument table rather than per tool, so a new tool
// cannot forget it.

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// schema renders the tool's arguments as a JSON Schema object.
//
// enumFor lets --read-only narrow a grouped tool's discriminator (dropping
// `open` from `tabs`, say) without a second copy of the tool table.
func (t *tool) schema(enumFor func(a arg) []string) *jsonschema.Schema {
	props := make(map[string]*jsonschema.Schema, len(t.args))
	var required []string
	for _, a := range t.args {
		s := &jsonschema.Schema{Type: a.typ, Description: a.desc}
		if a.typ == "array" {
			s.Items = &jsonschema.Schema{Type: a.items}
		}
		for _, e := range enumFor(a) {
			s.Enum = append(s.Enum, e)
		}
		props[a.name] = s
		if a.required {
			required = append(required, a.name)
		}
	}
	sort.Strings(required)
	return &jsonschema.Schema{
		Type:       "object",
		Properties: props,
		Required:   required,
		// An unknown argument is a caller mistake worth reporting rather than
		// ignoring: silently dropping it would run a different command from the
		// one the client believes it asked for.
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
}

// validate checks arguments against the declared table and returns them
// normalized. Every failure is a usage error, produced before the browser is
// contacted.
func (t *tool) validate(raw map[string]any, allowed func(a arg) []string) (map[string]any, error) {
	byName := make(map[string]arg, len(t.args))
	for _, a := range t.args {
		byName[a.name] = a
	}
	out := make(map[string]any, len(raw))
	for name, v := range raw {
		a, ok := byName[name]
		if !ok {
			return nil, usagef("%s: unknown argument %q (known: %s)", t.name, name, strings.Join(t.argNames(), ", "))
		}
		if v == nil {
			continue // an explicit null means "not given"
		}
		nv, err := checkType(t.name, a, v)
		if err != nil {
			return nil, err
		}
		if enum := allowed(a); len(enum) > 0 {
			s, _ := nv.(string)
			if !contains(enum, s) {
				return nil, usagef("%s: `%s` = %q is not one of %s", t.name, a.name, s, strings.Join(enum, ", "))
			}
		}
		out[name] = nv
	}
	for _, a := range t.args {
		if a.required {
			if v, ok := out[a.name]; !ok || isEmpty(v) {
				return nil, usagef("%s: `%s` is required", t.name, a.name)
			}
		}
	}
	return out, nil
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case nil:
		return true
	}
	return false
}

// checkType enforces the declared JSON type. It is strict on purpose: a number
// where a string belongs usually means the client built the call from the wrong
// half of the schema, and guessing would run something the caller did not ask
// for.
func checkType(toolName string, a arg, v any) (any, error) {
	switch a.typ {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, typeErr(toolName, a, v)
		}
		return s, nil
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, typeErr(toolName, a, v)
		}
		return b, nil
	case "number":
		f, ok := toFloat(v)
		if !ok {
			return nil, typeErr(toolName, a, v)
		}
		return f, nil
	case "integer":
		f, ok := toFloat(v)
		if !ok || f != math.Trunc(f) {
			return nil, typeErr(toolName, a, v)
		}
		return f, nil
	case "array":
		items, ok := v.([]any)
		if !ok {
			return nil, typeErr(toolName, a, v)
		}
		for _, it := range items {
			if a.items == "string" {
				if _, ok := it.(string); !ok {
					return nil, usagef("%s: `%s` must be an array of strings", toolName, a.name)
				}
			}
		}
		return items, nil
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return nil, typeErr(toolName, a, v)
		}
		return m, nil
	}
	return v, nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func typeErr(toolName string, a arg, v any) error {
	return usagef("%s: `%s` must be a %s, got %s", toolName, a.name, a.typ, jsonKind(v))
}

func jsonKind(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

func (t *tool) argNames() []string {
	out := make([]string, 0, len(t.args))
	for _, a := range t.args {
		out = append(out, a.name)
	}
	sort.Strings(out)
	return out
}

// stringList reads an array-of-strings argument.
func (c *call) stringList(name string) ([]string, error) {
	items, _ := c.args[name].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, usagef("`%s` must be an array of strings", name)
		}
		out = append(out, s)
	}
	return out, nil
}

func jsonBytes(v any) ([]byte, error) { return json.Marshal(v) }

// ToolSpec is a read-only view of one registered tool, for callers that need to
// reason about the surface without invoking it — the docs, and the conformance
// test in internal/cli that checks every argument still mirrors a live CLI flag.
type ToolSpec struct {
	Name  string
	Title string
	Verbs []string
	Args  []ArgSpec
}

// ArgSpec is one argument of a ToolSpec. Flag is the CLI flag it mirrors, empty
// for a positional or synthetic argument.
type ArgSpec struct {
	Name     string
	Type     string
	Flag     string
	Enum     []string
	Required bool
	Pos      bool
}

// Specs returns the tools a server built with o would expose, in listing order.
func Specs(o Options) ([]ToolSpec, error) {
	sel, err := o.selection()
	if err != nil {
		return nil, err
	}
	out := make([]ToolSpec, 0, len(sel))
	for _, t := range sel {
		spec := ToolSpec{Name: t.name, Title: t.title, Verbs: append([]string(nil), t.verbs...)}
		for _, a := range t.args {
			spec.Args = append(spec.Args, ArgSpec{
				Name: a.name, Type: a.typ, Flag: a.flag,
				Enum: append([]string(nil), a.enum...), Required: a.required, Pos: a.pos,
			})
		}
		out = append(out, spec)
	}
	return out, nil
}
