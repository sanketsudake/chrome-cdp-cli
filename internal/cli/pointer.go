package cli

// The pointer verbs — hover, dblclick, rclick, drag (RFC-0005). Four commands
// over one driver method: they differ only in the PointerAction they select and
// the flags they accept.

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// dragStepsMin / dragStepsMax bound --steps. One move is the minimum that still
// makes the gesture a drag; past a hundred the interpolation only costs
// round-trips, so anything outside is a usage error rather than a slow surprise.
const (
	dragStepsMin = 1
	dragStepsMax = 100
)

const modifiersUsage = "modifier keys held during the action, +-joined (ctrl+shift+alt+cmd)"

func (a *App) cmdHover() *cobra.Command {
	var mods string
	var hold time.Duration
	c := &cobra.Command{
		Use:   "hover <selector>",
		Short: "Move the pointer onto an element to reveal hover-only UI (no press)",
		Long: "Move the pointer onto an element and leave it there.\n\n" +
			"hover dispatches the move and returns — it does not wait for a tooltip or\n" +
			"flyout to render, because it cannot know one is coming. Confirm with\n" +
			"--wait-text, a following `wait --visible`, or --hold to park the pointer\n" +
			"for a fixed duration:\n\n" +
			"  chrome-cdp hover --by name \"Invoice 4102\"\n" +
			"  chrome-cdp click --by name \"Delete\" --in-row \"Invoice 4102\"",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			opts, rerr := a.pointerOpts(chrome.PointerHover, mods)
			if rerr != nil {
				a.emitErr("hover", rerr.Code, rerr.Message, nil)
				return nil
			}
			opts.Hold = hold
			a.runPointer("hover", args[0], opts)
			return nil
		},
	}
	c.Flags().StringVar(&mods, "modifiers", "", modifiersUsage)
	c.Flags().DurationVar(&hold, "hold", 0, "keep the pointer in place for this long (for slow tooltips), e.g. 500ms")
	return a.withWaitText(c)
}

func (a *App) cmdDblClick() *cobra.Command {
	var mods string
	c := &cobra.Command{
		Use:   "dblclick <selector>",
		Short: "Double-click an element (enters edit mode in most data grids)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a.runPointerVerb("dblclick", chrome.PointerDblClick, args[0], mods)
			return nil
		},
	}
	c.Flags().StringVar(&mods, "modifiers", "", modifiersUsage)
	return a.withWaitText(c)
}

func (a *App) cmdRClick() *cobra.Command {
	var mods string
	c := &cobra.Command{
		Use:   "rclick <selector>",
		Short: "Right-click an element to open its context menu",
		Long: "Right-click an element, opening its context menu.\n\n" +
			"Nothing dismisses the menu implicitly — a menu left open would otherwise\n" +
			"swallow the next command's click. Close it with `key Escape`.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a.runPointerVerb("rclick", chrome.PointerRClick, args[0], mods)
			return nil
		},
	}
	c.Flags().StringVar(&mods, "modifiers", "", modifiersUsage)
	return a.withWaitText(c)
}

func (a *App) cmdDrag() *cobra.Command {
	var mods, to, toBy string
	var dx, dy float64
	var steps int
	var hold time.Duration
	c := &cobra.Command{
		Use:   "drag <selector> (--to <selector> | --dx <px> --dy <px>)",
		Short: "Press on an element, move, and release — onto a drop target or by a pixel delta",
		Long: "Drag from an element to a drop target (--to) or by a pixel delta (--dx/--dy).\n\n" +
			"The gesture is real mouse input: press, --steps interpolated moves, release.\n" +
			"The intermediate moves are why it works — a press and a release at two\n" +
			"points with nothing between them is silently a click to every JS drag\n" +
			"implementation. Use --hold for long-press-to-drag UIs.\n\n" +
			"  chrome-cdp drag --by name \"Task A\" --to \"Done\" --to-by name\n" +
			"  chrome-cdp drag \"#volume\" --dx 80 --steps 20",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate the targeting form BEFORE connecting: a drag with no
			// destination, or two of them, is the caller's bug and must be exit 2
			// without ever touching Chrome.
			delta := cmd.Flags().Changed("dx") || cmd.Flags().Changed("dy")
			switch {
			case to != "" && delta:
				a.emitErr("drag", result.CodeUsage, "drag takes either --to <selector> or --dx/--dy, not both", nil)
				return nil
			case to == "" && !delta:
				a.emitErr("drag", result.CodeUsage, "drag needs a destination: --to <selector>, or --dx/--dy for a pixel delta", nil)
				return nil
			}
			if steps < dragStepsMin || steps > dragStepsMax {
				a.emitErr("drag", result.CodeUsage,
					fmt.Sprintf("--steps must be between %d and %d, got %d", dragStepsMin, dragStepsMax, steps), nil)
				return nil
			}
			opts, rerr := a.pointerOpts(chrome.PointerDrag, mods)
			if rerr != nil {
				a.emitErr("drag", rerr.Code, rerr.Message, nil)
				return nil
			}
			opts.To, opts.Dx, opts.Dy, opts.Steps, opts.Hold = to, dx, dy, steps, hold
			// The drop target inherits the source's addressing unless --to-by
			// overrides it, so `drag --by name A --to B` needs no second mode flag.
			opts.ToQuery = opts.Query
			if toBy != "" {
				opts.ToQuery.By = toBy
			}
			a.runPointer("drag", args[0], opts)
			return nil
		},
	}
	c.Flags().StringVar(&mods, "modifiers", "", modifiersUsage)
	c.Flags().StringVar(&to, "to", "", "drop-target selector (mutually exclusive with --dx/--dy)")
	c.Flags().StringVar(&toBy, "to-by", "", "--by mode for the drop target (defaults to --by)")
	c.Flags().Float64Var(&dx, "dx", 0, "horizontal drag distance in pixels from the source centre")
	c.Flags().Float64Var(&dy, "dy", 0, "vertical drag distance in pixels from the source centre")
	c.Flags().IntVar(&steps, "steps", 10, "interpolated move events dispatched between the press and the release")
	c.Flags().DurationVar(&hold, "hold", 0, "pause after the press before moving (long-press-to-drag UIs), e.g. 300ms")
	return a.withWaitText(c)
}

// runPointerVerb is the whole body of the pointer verbs that take nothing but a
// selector and --modifiers.
func (a *App) runPointerVerb(command string, action chrome.PointerAction, selector, mods string) {
	opts, rerr := a.pointerOpts(action, mods)
	if rerr != nil {
		a.emitErr(command, rerr.Code, rerr.Message, nil)
		return
	}
	a.runPointer(command, selector, opts)
}

// pointerOpts builds the driver options from the shared selector flags plus
// --modifiers. An unknown modifier name is a usage error raised here, before any
// connection is attempted.
func (a *App) pointerOpts(action chrome.PointerAction, mods string) (chrome.PointerOpts, *result.Err) {
	mask, err := chrome.ParseModifiers(mods)
	if err != nil {
		return chrome.PointerOpts{}, &result.Err{Code: result.CodeUsage, Message: err.Error()}
	}
	return chrome.PointerOpts{Action: action, Modifiers: mask, Query: a.queryOpts()}, nil
}

// runPointer resolves the target, dispatches the gesture, and emits the
// envelope. The `occluded` classification lives in classifyWithTabHint, which
// every action verb already routes through, so click/type/fill report a covered
// element the same way these verbs do.
func (a *App) runPointer(command, selector string, opts chrome.PointerOpts) {
	a.runResolved(command, func(ctx context.Context, b chrome.Browser, id string) (any, error) {
		return b.Pointer(ctx, id, selector, opts)
	})
}
