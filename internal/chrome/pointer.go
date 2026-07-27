package chrome

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// defaultDragSteps is the number of interpolated move events a drag dispatches
// when the caller didn't choose one. The CLI validates and passes an explicit
// value; this covers a direct/RPC caller that left Steps zero.
const defaultDragSteps = 10

// dragMoveInterval spaces the interpolated drag moves apart. Drag
// implementations that debounce or run their bookkeeping in requestAnimationFrame
// need the moves to land in separate frames; a burst dispatched in one tick reads
// as a teleport and is commonly ignored.
const dragMoveInterval = 8 * time.Millisecond

// leftButtonMask is the CDP `buttons` bitfield for "left button currently held"
// (Left=1). It has to be set on the moves BETWEEN a press and a release, or the
// page sees a hover, not a drag.
const leftButtonMask int64 = 1

// errIs reports whether err is target, including after the daemon RPC has
// flattened it to a plain message and lost its Go type. Every sentinel the CLI
// classifies on is matched through here, so the message fallback exists once.
func errIs(err, target error) bool {
	return err != nil && (errors.Is(err, target) || strings.Contains(err.Error(), target.Error()))
}

// IsOccluded reports whether err is ErrOccluded.
func IsOccluded(err error) bool { return errIs(err, ErrOccluded) }

// Pointer dispatches one pointer gesture — click, hover, double-click,
// right-click, or drag — at an element's live, occlusion-verified centre.
//
// All five verbs share one centre resolution (settledNodePoint) rather than
// recomputing geometry: a second box-model read would drift on overlapped
// elements and is simply wrong on a hidden tab. Modifiers are set on every
// dispatched event, so the page's handlers see event.metaKey / shiftKey and a
// modified click multi-selects the way a human's does.
func (c *CDP) Pointer(ctx context.Context, id, selector string, opts PointerOpts) (map[string]any, error) {
	var out map[string]any
	// Where the gesture landed, kept for the recorder. This resolution is the
	// only place a centre point exists, which is precisely why RFC-0011's
	// annotation needs no geometry of its own.
	var markX, markY float64
	core := chromedp.ActionFunc(func(actx context.Context) error {
		x, y, hit, err := pointerOrigin(actx, selector, opts)
		if err != nil {
			return err
		}
		markX, markY = x, y
		mods := input.Modifier(opts.Modifiers)

		switch opts.Action {
		case PointerClick:
			if err := pointerClickSeq(actx, x, y, input.Left, 1, mods); err != nil {
				return err
			}
			// click's payload predates the pointer verbs and is public API — the
			// Agent Skill and the human formatter both read `clicked`. Routing the
			// verb through this method must not change what it emits, so it keeps
			// its own shape instead of adopting the x/y/name one.
			out = map[string]any{"clicked": pointerLabel(selector, opts)}
			if opts.At != nil {
				// The coordinate form has no selector to echo, so it reports the
				// point and what sat under it — the evidence a caller needs to
				// confirm the click went where the screenshot said.
				out["x"], out["y"], out["hit"] = x, y, hit
			}
			return nil
		case PointerHover:
			// Dispatch the move and return. Whether the app rendered a tooltip is
			// the caller's problem (--wait-text, or `wait --visible`); the verb
			// must not silently sleep waiting for one. --hold only keeps the
			// pointer parked for a stated duration, which is an explicit request.
			if err := pointerMove(actx, x, y, mods, 0); err != nil {
				return err
			}
			if err := pointerHold(actx, opts.Hold); err != nil {
				return err
			}
		case PointerDblClick:
			if err := pointerClickSeq(actx, x, y, input.Left, 2, mods); err != nil {
				return err
			}
		case PointerTripleClick:
			// Three escalating press/release pairs — clickCount 1, then 2, then
			// 3 — which is what a human triple-click puts on the wire and what
			// Blink selects a paragraph on.
			if err := pointerClickSeq(actx, x, y, input.Left, 3, mods); err != nil {
				return err
			}
		case PointerRClick:
			if err := pointerClickSeq(actx, x, y, input.Right, 1, mods); err != nil {
				return err
			}
		case PointerDrag:
			tx, ty, err := dragDestination(actx, x, y, opts)
			if err != nil {
				return err
			}
			steps := opts.Steps
			if steps <= 0 {
				steps = defaultDragSteps
			}
			if err := pointerDrag(actx, x, y, tx, ty, steps, opts.Hold, mods); err != nil {
				return err
			}
			out = dragResult(pointerLabel(selector, opts), x, y, tx, ty, steps, opts)
			return nil
		default:
			return fmt.Errorf("unknown pointer action %q", opts.Action)
		}
		out = map[string]any{
			"action":    string(opts.Action),
			"x":         x,
			"y":         y,
			"name":      pointerLabel(selector, opts),
			"modifiers": modifierNames(opts.Modifiers),
		}
		if opts.At != nil {
			out["hit"] = hit
		}
		return nil
	})
	action, sink := withOptionalDialog(opts.Query, core)
	if err := c.run(ctx, id, bringToFront(), action); err != nil {
		return nil, err
	}
	// Recorded only after the gesture succeeded: a marker for a click that never
	// landed would be worse than no annotation at all. A no-op when the tab is
	// not being recorded.
	c.noteRecordMark(id, string(opts.Action), markX, markY)
	return withDialogResult(out, sink), nil
}

// dragDestination resolves where a drag ends: the centre of the --to element
// (addressed with ToQuery, so --to-by works), or the (Dx, Dy) offset from the
// source centre. The CLI guarantees exactly one form is set.
func dragDestination(ctx context.Context, x, y float64, opts PointerOpts) (float64, float64, error) {
	if opts.ToAt != nil {
		if err := checkInViewport(ctx, *opts.ToAt); err != nil {
			return 0, 0, err
		}
		return opts.ToAt.X, opts.ToAt.Y, nil
	}
	if opts.To == "" {
		return x + opts.Dx, y + opts.Dy, nil
	}
	nid, err := resolveNodeReady(ctx, opts.To, opts.ToQuery)
	if err != nil {
		return 0, 0, err
	}
	return settledNodePoint(ctx, nid)
}

// dragResult builds the drag envelope payload: both endpoints, the step count
// actually used, and the modifiers held.
func dragResult(selector string, fx, fy, tx, ty float64, steps int, opts PointerOpts) map[string]any {
	to := map[string]any{"x": tx, "y": ty}
	if opts.To != "" {
		to["name"] = opts.To
	}
	if opts.ToAt != nil {
		to["at"] = true
	}
	return map[string]any{
		"action":    string(opts.Action),
		"from":      map[string]any{"x": fx, "y": fy, "name": selector},
		"to":        to,
		"steps":     steps,
		"modifiers": modifierNames(opts.Modifiers),
	}
}

// pointerMove dispatches a bare pointer move. buttons is the mask of buttons
// held during the move (0 for a hover, leftButtonMask mid-drag).
func pointerMove(ctx context.Context, x, y float64, mods input.Modifier, buttons int64) error {
	ev := input.DispatchMouseEvent(input.MouseMoved, x, y).WithModifiers(mods)
	if buttons != 0 {
		ev = ev.WithButton(input.Left).WithButtons(buttons)
	}
	return ev.Do(ctx)
}

// pointerClickSeq moves to (x,y) and presses/releases button with click counts
// escalating up to count.
//
// The escalation is what a real double-click puts on the wire: Blink fires
// `dblclick` off the release whose clickCount is 2, and a page that counts
// clicks itself sees the same 1-then-2 progression a human produces. Dispatching
// two independent count-1 clicks instead would fire no dblclick at all.
func pointerClickSeq(ctx context.Context, x, y float64, btn input.MouseButton, count int64, mods input.Modifier) error {
	if err := pointerMove(ctx, x, y, mods, 0); err != nil {
		return err
	}
	for cc := int64(1); cc <= count; cc++ {
		press := input.DispatchMouseEvent(input.MousePressed, x, y).
			WithButton(btn).WithClickCount(cc).WithModifiers(mods)
		if err := press.Do(ctx); err != nil {
			return err
		}
		release := input.DispatchMouseEvent(input.MouseReleased, x, y).
			WithButton(btn).WithClickCount(cc).WithModifiers(mods)
		if err := release.Do(ctx); err != nil {
			return err
		}
	}
	return nil
}

// pointerDrag presses at the source, walks the pointer to the target through
// steps interpolated moves, and releases there.
//
// The interpolated moves are the load-bearing part: a press at one point and a
// release at another, with nothing in between, is silently a click to every JS
// drag implementation. hold pauses after the press for long-press-to-drag UIs.
func pointerDrag(ctx context.Context, fx, fy, tx, ty float64, steps int, hold time.Duration, mods input.Modifier) error {
	if err := pointerMove(ctx, fx, fy, mods, 0); err != nil {
		return err
	}
	press := input.DispatchMouseEvent(input.MousePressed, fx, fy).
		WithButton(input.Left).WithClickCount(1).WithModifiers(mods)
	if err := press.Do(ctx); err != nil {
		return err
	}
	if err := pointerHold(ctx, hold); err != nil {
		return err
	}
	for i := 1; i <= steps; i++ {
		f := float64(i) / float64(steps+1)
		if err := pointerMove(ctx, fx+(tx-fx)*f, fy+(ty-fy)*f, mods, leftButtonMask); err != nil {
			return err
		}
		if err := pointerHold(ctx, dragMoveInterval); err != nil {
			return err
		}
	}
	// Land exactly on the target before releasing, so the drop lands at the
	// resolved centre and not at the last interpolated fraction of it.
	if err := pointerMove(ctx, tx, ty, mods, leftButtonMask); err != nil {
		return err
	}
	release := input.DispatchMouseEvent(input.MouseReleased, tx, ty).
		WithButton(input.Left).WithClickCount(1).WithModifiers(mods)
	return release.Do(ctx)
}

// pointerHold pauses for d, giving up early if the command's context expires.
func pointerHold(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pointerOrigin decides where a gesture starts: an explicit coordinate, or an
// element's settled, occlusion-verified centre.
//
// The two paths differ in more than resolution. An element centre is checked
// for occlusion because the caller named a THING and expects to hit it; a
// coordinate is checked only for being inside the viewport, because the
// caller named a PLACE — second-guessing that would make every canvas app
// unreachable, since elementFromPoint there always answers "the canvas".
func pointerOrigin(ctx context.Context, selector string, opts PointerOpts) (x, y float64, hit map[string]any, err error) {
	if opts.At != nil {
		if err := checkInViewport(ctx, *opts.At); err != nil {
			return 0, 0, nil, err
		}
		return opts.At.X, opts.At.Y, hitAt(ctx, *opts.At), nil
	}
	nid, err := resolveNodeReady(ctx, selector, opts.Query)
	if err != nil {
		return 0, 0, nil, err
	}
	x, y, err = settledNodePoint(ctx, nid)
	return x, y, nil, err
}

// pointerLabel is what the envelope calls the gesture's target: the selector,
// or the coordinate when there was no selector.
func pointerLabel(selector string, opts PointerOpts) string {
	if opts.At != nil {
		return fmt.Sprintf("%g,%g", opts.At.X, opts.At.Y)
	}
	return selector
}
