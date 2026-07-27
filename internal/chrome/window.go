package chrome

// The `window` verb (RFC-0014): the real Chrome window's size, not the
// viewport emulation.
//
// `emulate viewport` tells the page it is a different size; the window on
// screen is unchanged, so a screenshot still shows the old one. Coordinate
// workflows need the opposite — the window the user actually sees, at a known
// size, so a coordinate read off a screenshot means the same thing on the next
// run.

import (
	"context"
	"errors"
	"fmt"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	cdptarget "github.com/chromedp/cdproto/target"
)

// Window reports the target's window bounds, or resizes it when opts carries
// dimensions.
//
// Bounds are a BROWSER-domain concept keyed by window, so this runs through
// onBrowser — the executor Target.*/Browser.* calls need — rather than a page
// session, and names its target explicitly the way raiseWindow does.
func (c *CDP) Window(ctx context.Context, id string, opts WindowOpts) (WindowBounds, error) {
	var out WindowBounds
	err := c.onBrowser(ctx, func(bctx context.Context) error {
		wid, bounds, err := cdpbrowser.GetWindowForTarget().WithTargetID(cdptarget.ID(id)).Do(bctx)
		if err != nil {
			return err
		}
		if opts.Width > 0 && opts.Height > 0 {
			// setWindowBounds refuses a size change on a maximized, minimized, or
			// fullscreen window, so normalize first — otherwise the command fails
			// for the single most common way a browser is left.
			if bounds != nil && bounds.WindowState != cdpbrowser.WindowStateNormal {
				if err := cdpbrowser.SetWindowBounds(wid, &cdpbrowser.Bounds{
					WindowState: cdpbrowser.WindowStateNormal,
				}).Do(bctx); err != nil {
					return err
				}
			}
			if err := cdpbrowser.SetWindowBounds(wid, &cdpbrowser.Bounds{
				Width:  opts.Width,
				Height: opts.Height,
			}).Do(bctx); err != nil {
				return err
			}
			// Read back rather than echoing the request: the window manager may
			// clamp to the screen, and reporting what was asked for instead of
			// what happened would make a clamped resize look successful.
			//
			// A failed read-back is an error, not a fallback to the bounds we
			// measured BEFORE resizing: those are stale by definition here, and
			// reporting them as the result would be the same lie in the other
			// direction. The caller can re-ask with `window info`.
			_, settled, rerr := cdpbrowser.GetWindowForTarget().WithTargetID(cdptarget.ID(id)).Do(bctx)
			if rerr != nil {
				return fmt.Errorf("window resized, but reading back the settled bounds failed: %w", rerr)
			}
			if settled == nil {
				return errors.New("window resized, but Chrome reported no settled bounds")
			}
			bounds = settled
		}
		if bounds != nil {
			out = WindowBounds{
				Left: bounds.Left, Top: bounds.Top,
				Width: bounds.Width, Height: bounds.Height,
				State: string(bounds.WindowState),
			}
		}
		return nil
	})
	return out, err
}
