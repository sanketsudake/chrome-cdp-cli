package cli

// The `window` verb (RFC-0014): the real Chrome window's size, as opposed to
// `emulate viewport`, which only changes what the page believes.
//
// The distinction matters for coordinate work: a coordinate read off a
// screenshot is only reusable if the window is the same size next run, and
// only `window size` moves the window a screenshot actually captures.

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// windowDimMax bounds a requested dimension. Past this the request is a typo
// rather than a window — the OS would clamp it anyway, and reporting the clamp
// as success is worse than refusing the input.
const windowDimMax = 20000

func (a *App) cmdWindow() *cobra.Command {
	c := &cobra.Command{
		Use:   "window",
		Short: "Report or set the real Chrome window's size",
		Long: "Report or set the size of the REAL Chrome window.\n\n" +
			"This is not `emulate viewport`. That tells the page it is a different\n" +
			"size while the window on screen stays put, so a screenshot still shows the\n" +
			"old one; `window size` moves the window you can see. Use `window size` to\n" +
			"make screenshots and coordinates reproducible, `emulate viewport` to test\n" +
			"responsive breakpoints.\n\n" +
			"The reported bounds are read back after the change, because the window\n" +
			"manager may clamp a request to the screen.",
	}
	c.AddCommand(a.cmdWindowInfo(), a.cmdWindowSize())
	return c
}

func (a *App) cmdWindowInfo() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Report the window's position, size, and state",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			a.runResolved("window", func(ctx context.Context, b chrome.Browser, id string) (any, error) {
				return b.Window(ctx, id, chrome.WindowOpts{})
			})
			return nil
		},
	}
}

func (a *App) cmdWindowSize() *cobra.Command {
	return &cobra.Command{
		Use:   "size <width> <height>",
		Short: "Resize the real Chrome window (CSS pixels)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			w, msg := parseWindowDim("width", args[0])
			if msg == "" {
				var m2 string
				_, m2 = parseWindowDim("height", args[1])
				msg = m2
			}
			if msg != "" {
				a.emitErr("window", result.CodeUsage, msg, nil)
				return nil
			}
			h, _ := parseWindowDim("height", args[1])
			a.runResolved("window", func(ctx context.Context, b chrome.Browser, id string) (any, error) {
				return b.Window(ctx, id, chrome.WindowOpts{Width: w, Height: h})
			})
			return nil
		},
	}
}

// parseWindowDim parses one dimension, rejecting anything that is not a
// positive, plausible pixel count — before connecting.
func parseWindowDim(name, s string) (int64, string) {
	var v int64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0, fmt.Sprintf("%s must be a whole number of pixels (got %q)", name, s)
	}
	if v < 1 || v > windowDimMax {
		return 0, fmt.Sprintf("%s must be between 1 and %d pixels (got %d)", name, windowDimMax, v)
	}
	return v, ""
}
