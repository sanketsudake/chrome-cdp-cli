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

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

// Window reports the target's window bounds, or resizes it when opts carries
// dimensions.
func (c *CDP) Window(ctx context.Context, id string, opts WindowOpts) (WindowBounds, error) {
	var out WindowBounds
	err := c.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
		// Bounds are a BROWSER-domain concept keyed by window, so the target has
		// to be mapped to its window first.
		winID, bounds, err := browser.GetWindowForTarget().Do(actx)
		if err != nil {
			return err
		}
		if opts.Width > 0 && opts.Height > 0 {
			// setWindowBounds refuses a size change on a maximized, minimized, or
			// fullscreen window, so normalize first — otherwise the command fails
			// for the single most common way a browser is left.
			if bounds != nil && bounds.WindowState != browser.WindowStateNormal {
				if err := browser.SetWindowBounds(winID, &browser.Bounds{
					WindowState: browser.WindowStateNormal,
				}).Do(actx); err != nil {
					return err
				}
			}
			if err := browser.SetWindowBounds(winID, &browser.Bounds{
				Width:  opts.Width,
				Height: opts.Height,
			}).Do(actx); err != nil {
				return err
			}
		}
		// Read back rather than echoing the request: the window manager may clamp
		// to the screen, and reporting what was asked for instead of what
		// happened would make a clamped resize look successful.
		_, settled, err := browser.GetWindowForTarget().Do(actx)
		if err != nil || settled == nil {
			settled = bounds
		}
		if settled != nil {
			out = WindowBounds{
				Left: settled.Left, Top: settled.Top,
				Width: settled.Width, Height: settled.Height,
				State: string(settled.WindowState),
			}
		}
		return nil
	}))
	return out, err
}
