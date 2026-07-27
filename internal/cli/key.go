package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// maxKeyRepeat caps --repeat. The ceiling exists because a repeat is the one
// argument that turns a typo into damage: `--repeat 100000` on a Backspace is a
// long-running command the caller cannot interrupt cleanly, and no legitimate
// use needs more than a hundred presses in one call.
const maxKeyRepeat = 100

func (a *App) cmdKey() *cobra.Command {
	var repeat int
	var delay time.Duration
	c := &cobra.Command{
		Use:   "key [selector] <keyspec>",
		Short: "Press named keys and modifier chords (Escape, cmd+a, \"End shift+Home Backspace\")",
		Long: "Press keys that are not literal text: named keys, modifier chords, and sequences.\n\n" +
			"With a selector the element is focused first; with no selector the keys go to\n" +
			"whatever the page currently has focused — which is how `key Escape` closes a\n" +
			"modal that has nothing addressable to aim at. The keyspec is always the last\n" +
			"argument.\n\n" +
			"  chrome-cdp key Escape\n" +
			"  chrome-cdp key \"#search\" cmd+a\n" +
			"  chrome-cdp key \"End shift+Home Backspace\"\n" +
			"  chrome-cdp key ArrowDown --repeat 5 --delay 100ms\n\n" +
			"A key is a known name (Escape, Enter, Tab, Space, Home, PageDown, ArrowUp,\n" +
			"F1…F24, matched case-insensitively) or a single printable character. Modifiers\n" +
			"are ctrl, shift, alt, and cmd (aliases meta, super); cmd is Meta on every\n" +
			"platform. Use `type` to send literal text — an unknown multi-character key\n" +
			"name is an error, never typed out letter by letter.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			// The keyspec is always last, so the optional selector is arg 0 only
			// when there are two arguments.
			selector, spec := "", args[0]
			if len(args) == 2 {
				selector, spec = args[0], args[1]
			}

			// Everything below runs BEFORE resolveTarget, so a bad keyspec or an
			// out-of-range --repeat is exit 2 without ever contacting Chrome.
			if repeat < 1 || repeat > maxKeyRepeat {
				a.emitErr("key", result.CodeUsage, fmt.Sprintf("--repeat must be between 1 and %d, got %d", maxKeyRepeat, repeat), nil)
				return nil
			}
			keys, err := chrome.ParseKeys(spec)
			if err != nil {
				a.emitErr("key", result.CodeUsage, err.Error(), nil)
				return nil
			}

			opts := chrome.KeyOpts{Repeat: repeat, Delay: delay, Query: a.queryOpts()}
			a.runResolved("key", func(ctx context.Context, b chrome.Browser, id string) (any, error) {
				return b.Key(ctx, id, selector, keys, opts)
			})
			return nil
		},
	}
	c.Flags().IntVar(&repeat, "repeat", 1, fmt.Sprintf("press the sequence this many times (1..%d)", maxKeyRepeat))
	c.Flags().DurationVar(&delay, "delay", 0, "pause between repeats (for apps that debounce keyboard input)")
	return a.withWaitText(c)
}
