package mcp

// Argument injection: a tool argument must never be parsed as a CLI flag.
//
// The bug this file pins down was structural rather than local. The builders
// spliced caller-controlled POSITIONAL values into argv ahead of the flags they
// generated, and pflag parses flags and positionals interspersed — so any
// positional whose value began with "-" was consumed as a flag. `--policy-off`
// and `--allow` are root persistent flags, which made
//
//	{"name":"chrome_cdp_read","arguments":{"kind":"text","target":"bb22","selector":"--policy-off"}}
//
// a full read of a tab the allow-list excludes, with no warning and no audit
// record. It worked inside `batch` and on a `--read-only` server, because
// neither of those bounds origins.
//
// The tests here walk the registry rather than a hand-written list, so a tool
// added later that reintroduces the shape fails here instead of in a client.

import (
	"strings"
	"testing"
)

// sentinel is a real root persistent flag, so a test that passes only because
// the value was harmless would be lying.
const sentinel = "--policy-off"

// TestNoArgumentCanReachFlagPosition is the structural guard.
//
// Every caller-controlled value must reach the CLI either as a POSITIONAL
// (emitted after the `--` terminator, where pflag cannot read it as anything
// else) or as the VALUE of a flag word (`--by --policy-off`, which pflag
// consumes unconditionally). A bare word in flag position is the hole.
func TestNoArgumentCanReachFlagPosition(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, tl := range registry() {
		for _, action := range discValues(tl) {
			for _, victim := range injectableArgs(tl) {
				args := fillArgs(tl, action, victim, sentinel)
				verb, flags, pos, err := tl.build(&call{tool: tl, args: args})
				if err != nil {
					// An argument that does not belong to this action, or a
					// combination the builder refuses outright. Nothing is
					// built, so there is nothing to escape.
					continue
				}
				checked++
				argv := argvFor(verb, flags, pos)
				if why := sentinelIsContained(tl, argv); why != "" {
					t.Errorf("%s (%s=%q, %q): %s\nargv = %v", tl.name, tl.disc, action, victim, why, argv)
				}
			}
		}
	}
	// A guard that silently exercised nothing would pass forever.
	if checked < 40 {
		t.Errorf("only %d argument placements were exercised; the table stopped covering the surface", checked)
	}
}

// sentinelIsContained reports why an argv lets the sentinel reach flag
// position, or "" when every occurrence of it is safely contained.
func sentinelIsContained(tl *tool, argv []string) string {
	valueTaking := map[string]bool{}
	for _, a := range tl.args {
		if a.flag != "" && a.typ != "boolean" {
			valueTaking["--"+a.flag] = true
		}
	}
	dash := len(argv)
	for i, w := range argv {
		if w == "--" {
			dash = i
			break
		}
	}
	for i, w := range argv {
		if w != sentinel || i > dash {
			continue
		}
		if i > 0 && valueTaking[argv[i-1]] {
			continue // the value of a flag word: pflag consumes it as data
		}
		return "the value reached flag position, where pflag parses it as a flag"
	}
	return ""
}

// TestPositionalsFollowTheTerminator states the assembly contract directly:
// flags first, then `--`, then the caller's values, with --json parsed as the
// flag it is.
func TestPositionalsFollowTheTerminator(t *testing.T) {
	t.Parallel()
	argv := argvFor("click", []string{"--by", "name"}, []string{"--policy-off"})
	want := []string{"click", "--by", "name", "--json", "--", "--policy-off"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	// A call with no positionals emits no terminator: a bare `--` would be
	// noise in every command that takes none.
	if argv := argvFor("snap", []string{"--role", "button"}, nil); contains(argv, "--") {
		t.Errorf("argv = %v, want no terminator when there are no positionals", argv)
	}
}

// TestValuesBeginningWithADashAreTypeable is the other half of the terminator:
// it makes the honest cases work, rather than only refusing the dishonest ones.
func TestValuesBeginningWithADashAreTypeable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tool string
		args map[string]any
		want []string
	}{
		{"type text starting with a dash", prefix + "type_text",
			map[string]any{"selector": "#a", "text": "-foo"}, []string{"type", "#a", "-foo"}},
		{"an eval expression starting with a dash", prefix + "evaluate",
			map[string]any{"expression": "-1"}, []string{"eval", "-1"}},
		{"a selector that looks like a flag", prefix + "click",
			map[string]any{"selector": "--policy-off"}, []string{"click", "--policy-off"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{}
			sess := connect(t, r, Options{Tools: SetFull})
			if out := callTool(t, sess, c.tool, c.args); out.IsError {
				t.Fatalf("call failed: %v", structured(t, out))
			}
			verb, pos := splitAtDash(r.argv(0))
			got := append([]string{verb}, pos...)
			if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
				t.Errorf("argv = %v, want the verb %q and the positionals %v after `--`", r.argv(0), c.want[0], c.want[1:])
			}
		})
	}
}

// discValues lists the discriminator values to exercise a tool with — every
// value for a grouped tool, and one no-op for the rest.
func discValues(tl *tool) []string {
	if tl.disc == "" {
		return []string{""}
	}
	out := make([]string, 0, len(tl.actions))
	for v := range tl.actions {
		out = append(out, v)
	}
	return out
}

// injectableArgs lists the arguments a caller can put an arbitrary string in.
// An enum argument cannot carry one — validate() refuses anything off the list
// before a builder ever sees it.
func injectableArgs(tl *tool) []string {
	var out []string
	for _, a := range tl.args {
		if len(a.enum) > 0 || a.name == tl.disc {
			continue
		}
		if a.typ == "string" || (a.typ == "array" && a.items == "string") {
			out = append(out, a.name)
		}
	}
	return out
}

// fillArgs builds a call whose `victim` argument carries evil, with every
// required argument present so the builder gets far enough to emit an argv.
func fillArgs(tl *tool, action, victim, evil string) map[string]any {
	args := map[string]any{}
	for _, a := range tl.args {
		switch {
		case a.name == tl.disc && tl.disc != "":
			args[a.name] = action
		case a.name == victim:
			if a.typ == "array" {
				args[a.name] = []any{evil}
			} else {
				args[a.name] = evil
			}
		case !a.required:
			continue
		case len(a.enum) > 0:
			args[a.name] = a.enum[0]
		case a.typ == "array":
			args[a.name] = []any{"placeholder"}
		case a.typ == "object":
			args[a.name] = map[string]any{}
		default:
			args[a.name] = "placeholder"
		}
	}
	// `url` and `target` are flags on `tabs` and positionals to its builder,
	// so an action that needs one has to have it.
	if tl.disc == "action" && tl.name == prefix+"tabs" {
		switch action {
		case "open":
			if _, ok := args["url"]; !ok {
				args["url"] = "https://placeholder.test/"
			}
		case "use":
			if _, ok := args["target"]; !ok {
				args["target"] = "placeholder"
			}
		}
	}
	return args
}
