package cli

// The pointer verbs — click, hover, dblclick, rclick, drag (RFC-0005). Five
// commands over one driver method: they differ only in the PointerAction they
// select and the flags they accept.
//
// `click` belongs here rather than in commands.go because it is the same
// gesture: routing it through Pointer is what gives it --modifiers (RFC-0005
// US-6, the multi-select) without a second click implementation to keep in step.

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

const atUsage = "act at this viewport coordinate \"x,y\" instead of resolving an element (for canvas/WebGL surfaces, or a screenshot-driven agent)"

// coordinateForm resolves the positional-selector vs --at choice for a pointer
// verb, rejecting every combination that mixes the two.
//
// --at bypasses element resolution entirely, so pairing it with a selector or
// any addressing flag is a contradiction rather than a refinement: there would
// be no way to honour both. Caught here, before connecting, because exit 2
// means "your call was wrong, do not retry".
func (a *App) coordinateForm(cmd *cobra.Command, args []string, at string) (selector string, pt *chrome.Point, msg string) {
	if at == "" {
		if len(args) == 0 {
			return "", nil, "needs a selector, or --at x,y to act at a viewport coordinate"
		}
		return args[0], nil, ""
	}
	if len(args) > 0 {
		return "", nil, "--at acts at a coordinate and takes no selector; drop one or the other"
	}
	// Every flag that only steers ELEMENT resolution, including the two the RFC's
	// own list omitted: --wait waits for a selector's state and --pierce reaches
	// into shadow DOM to find one, so both are silent no-ops once there is no
	// selector to resolve.
	for _, f := range []string{"by", "role", "nth", "match", "in-row", "wait", "pierce"} {
		if cmd.Flags().Changed(f) {
			return "", nil, "--at bypasses element addressing, so it cannot combine with --" + f
		}
	}
	p, err := parsePoint(at)
	if err != nil {
		return "", nil, err.Error()
	}
	return "", &p, ""
}

// pointerCmd builds a pointer verb that takes a selector or --at and nothing
// else. click, dblclick, tripleclick, and rclick differ only in their name,
// their PointerAction, and their help text, so they are built rather than
// written out — five hand-copied bodies is how dblclick came to advertise --at
// while quietly ignoring it.
//
// hover and drag stay hand-written: they have real per-verb options (--hold, a
// choice of destination forms), which is where generalizing should stop.
func (a *App) pointerCmd(name string, action chrome.PointerAction, short, long string) *cobra.Command {
	var mods, at string
	c := &cobra.Command{
		Use:   name + " <selector>",
		Short: short,
		Long:  long,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sel, pt, msg := a.coordinateForm(cmd, args, at)
			if msg != "" {
				a.emitErr(name, result.CodeUsage, name+" "+msg, nil)
				return nil
			}
			a.runPointerVerb(name, action, sel, mods, func(o *chrome.PointerOpts) { o.At = pt })
			return nil
		},
	}
	c.Flags().StringVar(&mods, "modifiers", "", modifiersUsage)
	c.Flags().StringVar(&at, "at", "", atUsage)
	return a.withWaitText(c)
}

func (a *App) cmdClick() *cobra.Command {
	return a.pointerCmd("click", chrome.PointerClick,
		"Click an element, or --at a viewport coordinate (auto-waits)",
		"Click an element at its live, occlusion-verified centre, or at a raw\n"+
			"viewport coordinate with --at.\n\n"+
			"--modifiers holds keys during the press, which is how a grid or file list\n"+
			"is multi-selected:\n\n"+
			"  chrome-cdp click --by name \"Row 2\" --modifiers cmd\n"+
			"  chrome-cdp click --at 512,340")
}

func (a *App) cmdTripleClick() *cobra.Command {
	return a.pointerCmd("tripleclick", chrome.PointerTripleClick,
		"Triple-click an element to select its text block",
		"Triple-click an element, selecting the whole text block under the pointer.\n\n"+
			"This is the standard prelude to copying or overwriting a paragraph:\n\n"+
			"  chrome-cdp tripleclick \"p.abstract\" && chrome-cdp key cmd+c\n\n"+
			"`fill` performs the same gesture internally before typing; this exposes it\n"+
			"for when you want only the selection.")
}

func (a *App) cmdDblClick() *cobra.Command {
	return a.pointerCmd("dblclick", chrome.PointerDblClick,
		"Double-click an element (enters edit mode in most data grids)",
		"Double-click an element, or --at a viewport coordinate.\n\n"+
			"This dispatches one `dblclick` event with `detail: 2`, the way a human\n"+
			"double-click does — which is what grid cells that edit on double-click\n"+
			"listen for.")
}

func (a *App) cmdRClick() *cobra.Command {
	return a.pointerCmd("rclick", chrome.PointerRClick,
		"Right-click an element to open its context menu",
		"Right-click an element, or --at a viewport coordinate, opening the context\n"+
			"menu.\n\n"+
			"Nothing dismisses the menu implicitly — a menu left open would otherwise\n"+
			"swallow the next command's click. Close it with `key Escape`.")
}

func (a *App) cmdHover() *cobra.Command {
	var mods, at string
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
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sel, pt, msg := a.coordinateForm(cmd, args, at)
			if msg != "" {
				a.emitErr("hover", result.CodeUsage, "hover "+msg, nil)
				return nil
			}
			a.runPointerVerb("hover", chrome.PointerHover, sel, mods, func(o *chrome.PointerOpts) {
				o.Hold, o.At = hold, pt
			})
			return nil
		},
	}
	c.Flags().StringVar(&mods, "modifiers", "", modifiersUsage)
	c.Flags().StringVar(&at, "at", "", atUsage)
	c.Flags().DurationVar(&hold, "hold", 0, "keep the pointer in place for this long (for slow tooltips), e.g. 500ms")
	return a.withWaitText(c)
}

func (a *App) cmdDrag() *cobra.Command {
	var mods, to, toBy, at, toAt string
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
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate the targeting form BEFORE connecting: a drag with no
			// destination, or two of them, is the caller's bug and must be exit 2
			// without ever touching Chrome.
			sel, from, msg := a.coordinateForm(cmd, args, at)
			if msg != "" {
				a.emitErr("drag", result.CodeUsage, "drag "+msg, nil)
				return nil
			}
			delta := cmd.Flags().Changed("dx") || cmd.Flags().Changed("dy")
			dests := 0
			for _, set := range []bool{to != "", toAt != "", delta} {
				if set {
					dests++
				}
			}
			switch {
			case dests > 1:
				a.emitErr("drag", result.CodeUsage, "drag takes exactly one destination: --to <selector>, --to-at x,y, or --dx/--dy", nil)
				return nil
			case dests == 0:
				a.emitErr("drag", result.CodeUsage, "drag needs a destination: --to <selector>, --to-at x,y, or --dx/--dy for a pixel delta", nil)
				return nil
			}
			var dest *chrome.Point
			if toAt != "" {
				p, err := parsePoint(toAt)
				if err != nil {
					a.emitErr("drag", result.CodeUsage, "--to-at "+err.Error(), nil)
					return nil
				}
				dest = &p
			}
			if steps < dragStepsMin || steps > dragStepsMax {
				a.emitErr("drag", result.CodeUsage,
					fmt.Sprintf("--steps must be between %d and %d, got %d", dragStepsMin, dragStepsMax, steps), nil)
				return nil
			}
			a.runPointerVerb("drag", chrome.PointerDrag, sel, mods, func(o *chrome.PointerOpts) {
				o.To, o.Dx, o.Dy, o.Steps, o.Hold = to, dx, dy, steps, hold
				o.At, o.ToAt = from, dest
				o.ToQuery = toQueryFrom(o.Query, toBy)
			})
			return nil
		},
	}
	c.Flags().StringVar(&mods, "modifiers", "", modifiersUsage)
	c.Flags().StringVar(&at, "at", "", atUsage)
	c.Flags().StringVar(&toAt, "to-at", "", "release at this viewport coordinate \"x,y\" (mutually exclusive with --to and --dx/--dy)")
	c.Flags().StringVar(&to, "to", "", "drop-target selector (mutually exclusive with --to-at and --dx/--dy)")
	c.Flags().StringVar(&toBy, "to-by", "", "--by mode for the drop target (defaults to --by)")
	c.Flags().Float64Var(&dx, "dx", 0, "horizontal drag distance in pixels from the source centre")
	c.Flags().Float64Var(&dy, "dy", 0, "vertical drag distance in pixels from the source centre")
	c.Flags().IntVar(&steps, "steps", 10, "interpolated move events dispatched between the press and the release")
	c.Flags().DurationVar(&hold, "hold", 0, "pause after the press before moving (long-press-to-drag UIs), e.g. 300ms")
	return a.withWaitText(c)
}

// toQueryFrom builds the drop target's addressing from the source's, so
// `drag --by name A --to B` needs no second mode flag.
//
// It inherits only how to READ a selector — the --by mode, the --wait state, and
// --pierce — and deliberately drops every flag that NARROWS a match: --role,
// --nth, --match, --in-row. Those describe which of several candidates the
// SOURCE is, and mean nothing about the destination. --in-row is the one that
// bites: it scopes a name match to one table row (and byFor short-circuits on
// it), so a leaked --in-row makes any drop target outside that row unresolvable
// — `drag --by name "Task A" --in-row "Backlog" --to "Done"` would fail
// target_timeout on a correct command, blaming the selector for a scope the
// caller never asked to apply there. A refiner reaches the drop target only when
// a --to-* counterpart exists to ask for it; today that is --to-by alone.
func toQueryFrom(src chrome.QueryOpts, toBy string) chrome.QueryOpts {
	to := chrome.QueryOpts{By: src.By, Wait: src.Wait, Pierce: src.Pierce}
	if toBy != "" {
		to.By = toBy
	}
	return to
}

// runPointerVerb is the whole body of every pointer verb: build the driver
// options from the shared selector flags plus --modifiers, let the verb add the
// options only it has (set may be nil), then dispatch and emit.
//
// An unknown modifier name is a usage error raised here, before any connection is
// attempted.
func (a *App) runPointerVerb(command string, action chrome.PointerAction, selector, mods string, set func(*chrome.PointerOpts)) {
	mask, err := chrome.ParseModifiers(mods)
	if err != nil {
		a.emitErr(command, result.CodeUsage, err.Error(), nil)
		return
	}
	opts := chrome.PointerOpts{Action: action, Modifiers: mask, Query: a.queryOpts()}
	if set != nil {
		set(&opts)
	}
	a.runResolved(command, func(ctx context.Context, b chrome.Browser, id string) (any, error) {
		return b.Pointer(ctx, id, selector, opts)
	})
}
