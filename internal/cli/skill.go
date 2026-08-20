package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
	"github.com/sanketsudake/chrome-cdp-cli/skills"
)

// cmdSkill serves the embedded agent skill so it always matches the installed
// binary, instead of drifting from a vendored copy. It never touches Chrome:
// the content comes entirely from the skills package's embed.FS.
func (a *App) cmdSkill() *cobra.Command {
	var full bool
	c := &cobra.Command{
		Use:   "skill",
		Short: "Print an agent skill doc embedded in this binary",
		Long: "Print an agent skill embedded in this binary, so the doc an agent reads\n" +
			"always matches the CLI it is driving: drive-chrome-cdp plus the scenario\n" +
			"skills built on it.\n" +
			"With no flag, prints the drive-chrome-cdp core loop; --full adds every\n" +
			"reference.\n" +
			"`skill list` names every skill and reference; `skill get <name>` prints\n" +
			"one (a reference, a skill, or `<skill>/<reference>`).",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			var content []byte
			var err error
			if full {
				content, err = skills.Full()
			} else {
				content, err = skills.Core()
			}
			if err != nil {
				// A read failure here is the embedded FS misbehaving, not a
				// bad invocation — this command takes no args to get wrong.
				a.emitErr("skill", result.CodeGeneric, err.Error(), nil)
				return nil
			}
			if a.jsonOut {
				refs, err := skills.References()
				if err != nil {
					a.emitErr("skill", result.CodeGeneric, err.Error(), nil)
					return nil
				}
				a.emitOK("skill", nil, map[string]any{
					"name":       "drive-chrome-cdp",
					"references": refs,
					"content":    string(content),
				})
				return nil
			}
			fmt.Fprint(a.out, string(content))
			a.exitCode = result.ExitOK
			return nil
		},
	}
	c.Flags().BoolVar(&full, "full", false, "include every reference, not just the core loop")
	c.AddCommand(a.cmdSkillList(), a.cmdSkillGet())
	return c
}

// cmdSkillList lists the embedded skills and, for drive-chrome-cdp, its
// reference names. In human mode: skills first (drive-chrome-cdp pinned to
// the top, since every other skill here builds on it — Skills() would
// otherwise sort check-logged-in ahead of it), a blank line, then the
// references.
func (a *App) cmdSkillList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the embedded skill and reference names",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			names, err := skills.Skills()
			if err != nil {
				a.emitErr("skill", result.CodeGeneric, err.Error(), nil)
				return nil
			}
			refs, err := skills.References()
			if err != nil {
				a.emitErr("skill", result.CodeGeneric, err.Error(), nil)
				return nil
			}
			if a.jsonOut {
				a.emitOK("skill", nil, map[string]any{
					"name":       "drive-chrome-cdp",
					"skills":     names,
					"references": refs,
				})
				return nil
			}
			// drive-chrome-cdp leads the human listing — it is the skill
			// every other one here builds on — even though it does not
			// necessarily sort first among the rest.
			fmt.Fprintln(a.out, "drive-chrome-cdp")
			for _, n := range names {
				if n == "drive-chrome-cdp" {
					continue
				}
				fmt.Fprintln(a.out, n)
			}
			fmt.Fprintln(a.out)
			for _, r := range refs {
				fmt.Fprintln(a.out, r)
			}
			a.exitCode = result.ExitOK
			return nil
		},
	}
}

// cmdSkillGet resolves <name> to a document in this order: a drive-chrome-cdp
// reference (e.g. "core"), else a skill name (e.g. "check-logged-in"), else
// the explicit "<skill>/<reference>" form (e.g. "drive-chrome-cdp/core").
// Anything else is a usage error before touching Chrome.
func (a *App) cmdSkillGet() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Print one skill or drive-chrome-cdp reference",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name, ref, content, err := resolveSkillGet(args[0])
			if err != nil {
				a.emitErr("skill", result.CodeUsage, err.Error(), nil)
				return nil
			}
			if a.jsonOut {
				a.emitOK("skill", nil, map[string]any{"name": name, "reference": ref, "content": string(content)})
				return nil
			}
			fmt.Fprint(a.out, string(content))
			a.exitCode = result.ExitOK
			return nil
		},
	}
}

// resolveSkillGet implements the resolution order documented on
// cmdSkillGet, returning the skill name, the reference name (empty when
// arg names a whole skill), and the resolved content.
func resolveSkillGet(arg string) (name, ref string, content []byte, err error) {
	if b, e := skills.Reference(arg); e == nil {
		return "drive-chrome-cdp", arg, b, nil
	}
	if b, e := skills.Skill(arg); e == nil {
		return arg, "", b, nil
	}
	if skillPart, refPart, ok := strings.Cut(arg, "/"); ok && skillPart == "drive-chrome-cdp" {
		if b, e := skills.Reference(refPart); e == nil {
			return "drive-chrome-cdp", refPart, b, nil
		}
	}
	return "", "", nil, fmt.Errorf("unknown skill or reference %q", arg)
}
