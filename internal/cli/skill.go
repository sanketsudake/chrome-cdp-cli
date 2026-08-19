package cli

import (
	"fmt"

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
		Short: "Print the agent skill doc that matches this binary",
		Long: "Print the drive-chrome-cdp agent skill embedded in this binary, so the\n" +
			"doc an agent reads always matches the CLI it is driving.\n" +
			"With no flag, prints the core loop; --full adds every reference.\n" +
			"`skill list` names the references and `skill get <name>` prints one.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			content, err := skills.Core()
			if full {
				content, err = skills.Full()
			}
			if err != nil {
				a.emitErr("skill", result.CodeUsage, err.Error(), nil)
				return nil
			}
			if a.jsonOut {
				a.emitOK("skill", nil, map[string]any{
					"name":       "drive-chrome-cdp",
					"references": skills.References(),
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

func (a *App) cmdSkillList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the skill's reference names",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			refs := skills.References()
			if a.jsonOut {
				a.emitOK("skill", nil, map[string]any{"name": "drive-chrome-cdp", "references": refs})
				return nil
			}
			for _, r := range refs {
				fmt.Fprintln(a.out, r)
			}
			a.exitCode = result.ExitOK
			return nil
		},
	}
}

func (a *App) cmdSkillGet() *cobra.Command {
	return &cobra.Command{
		Use:   "get <reference>",
		Short: "Print one reference file",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			content, err := skills.Reference(args[0])
			if err != nil {
				a.emitErr("skill", result.CodeUsage, err.Error(), nil)
				return nil
			}
			if a.jsonOut {
				a.emitOK("skill", nil, map[string]any{"name": args[0], "content": string(content)})
				return nil
			}
			fmt.Fprint(a.out, string(content))
			a.exitCode = result.ExitOK
			return nil
		},
	}
}
