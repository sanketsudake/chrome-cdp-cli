package chrome

// Capture — the screenshot and pdf drivers (RFC-0008).
//
// Both return their artifact bytes AND a metadata map, because the useful facts
// about a capture (its real pixel dimensions, which mode produced it, the clip
// that was actually used) are known here and nowhere else. Everything a user can
// spell — formats, quality, scale, paper sizes, margins, page ranges — is
// validated and reduced to numbers in the CLI before it arrives here, so this
// file has no grammar of its own.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // registered so imageDims can read a JPEG header
	_ "image/png"  // registered so imageDims can read a PNG header
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// ErrZeroArea reports that an element resolved but occupies no space —
// display:none, a collapsed container, or a zero-size image. There is nothing to
// capture, and a clip of zero width is a CDP error rather than an empty image,
// so this is raised before the capture. The CLI reports it as `zero_area: true`.
var ErrZeroArea = errors.New("element resolved but has a zero-area box (display:none, or collapsed)")

// IsZeroArea reports whether err is the zero-area condition. Like IsOccluded it
// falls back to a message match, because an error raised inside the daemon
// reaches the CLI as a plain string with its Go type gone — and the daemon is
// the default connection path.
func IsZeroArea(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrZeroArea) {
		return true
	}
	return strings.Contains(err.Error(), ErrZeroArea.Error())
}

// Screenshot captures the tab and reports what it captured.
//
// Four modes, in precedence order: an element's box (Selector), the whole
// scrollable page (FullPage), an explicit rectangle (Region), or — the default —
// the viewport. The CLI rejects more than one before connecting.
//
// The returned metadata carries `mode` and the resolved `clip` because a capture
// that came out wrong is otherwise undebuggable: the image alone cannot tell you
// whether the wrong rectangle was chosen or the right one was rendered badly.
func (c *CDP) Screenshot(ctx context.Context, id string, opts ShotOpts) ([]byte, map[string]any, error) {
	scale := opts.Scale
	if scale <= 0 {
		scale = 1
	}
	format := opts.Format
	if format == "" {
		format = "png"
	}

	mode := ShotViewport
	var clip Rect
	var buf []byte

	err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		metrics, err := layoutRects(ctx)
		if err != nil {
			return err
		}
		// Whether to send a clip at all. The default viewport capture sends none,
		// so the no-flag call stays exactly the call it has always been.
		clipped := true
		switch {
		case opts.Selector != "":
			mode = ShotElement
			if clip, err = elementPageRect(ctx, opts); err != nil {
				return err
			}
			clip = padRect(clip, opts.Padding, metrics.page)
		case opts.FullPage:
			mode = ShotFullPage
			clip = metrics.page
		case opts.Region != nil:
			mode = ShotRegion
			clip = *opts.Region
		default:
			clip = metrics.viewport
			clipped = scale != 1 // a scaled capture can only be expressed through a clip
		}

		p := page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormat(format)).
			WithFromSurface(true)
		if format != "png" {
			p = p.WithQuality(int64(opts.Quality))
		}
		if clipped {
			// captureBeyondViewport is what makes an element taller than the
			// window, or a full page, come back whole — and it removes any
			// dependence on where the page happens to be scrolled.
			p = p.WithClip(&page.Viewport{
				X: clip.X, Y: clip.Y, Width: clip.Width, Height: clip.Height, Scale: scale,
			}).WithCaptureBeyondViewport(true)
		}
		buf, err = p.Do(ctx)
		return err
	}))
	if err != nil {
		return nil, nil, err
	}

	w, h := imageDims(buf, clip, scale)
	return buf, map[string]any{
		"format": format,
		"scale":  scale,
		"mode":   string(mode),
		"clip":   clip,
		"width":  w,
		"height": h,
	}, nil
}

// PDF prints the page to PDF (no chromedp Action; raw page.PrintToPDF).
func (c *CDP) PDF(ctx context.Context, id string, opts PDFOpts) ([]byte, map[string]any, error) {
	var buf []byte
	err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		// Margins are always sent (they have no omitempty), so `--margin 0` means
		// zero rather than Chrome's 0.4in default. A zero paper size is left off,
		// which is how "keep Chrome's default paper" is expressed.
		p := page.PrintToPDF().
			WithLandscape(opts.Landscape).
			WithPrintBackground(opts.Background).
			WithMarginTop(opts.MarginTop).
			WithMarginRight(opts.MarginRight).
			WithMarginBottom(opts.MarginBottom).
			WithMarginLeft(opts.MarginLeft)
		if opts.Scale > 0 {
			p = p.WithScale(opts.Scale)
		}
		if opts.PaperWidth > 0 && opts.PaperHeight > 0 {
			p = p.WithPaperWidth(opts.PaperWidth).WithPaperHeight(opts.PaperHeight)
		}
		if opts.Pages != "" {
			p = p.WithPageRanges(opts.Pages)
		}
		if opts.Header != "" || opts.Footer != "" {
			// Chrome renders its own date/title template for an EMPTY half, which
			// is not what "I asked for a footer only" means; a blank span is.
			p = p.WithDisplayHeaderFooter(true).
				WithHeaderTemplate(orBlankTemplate(opts.Header)).
				WithFooterTemplate(orBlankTemplate(opts.Footer))
		}
		var e error
		buf, _, e = p.Do(ctx)
		return e
	}))
	if err != nil {
		return nil, nil, err
	}
	return buf, map[string]any{"pages": PDFPageCount(buf)}, nil
}

// orBlankTemplate substitutes an empty header/footer template with a blank span,
// because Chrome treats "" as "use my default".
func orBlankTemplate(tpl string) string {
	if tpl == "" {
		return "<span></span>"
	}
	return tpl
}

// layoutRects is the page geometry a capture needs: the full scrollable content
// box and the current visual viewport, both in CSS pixels and both in PAGE
// coordinates — the space Page.captureScreenshot's clip uses.
type layoutRectSet struct {
	page     Rect
	viewport Rect
}

func layoutRects(ctx context.Context) (layoutRectSet, error) {
	_, _, _, _, visual, content, err := page.GetLayoutMetrics().Do(ctx)
	if err != nil {
		return layoutRectSet{}, err
	}
	var out layoutRectSet
	if content != nil {
		out.page = Rect{X: content.X, Y: content.Y, Width: content.Width, Height: content.Height}
	}
	if visual != nil {
		out.viewport = Rect{X: visual.PageX, Y: visual.PageY, Width: visual.ClientWidth, Height: visual.ClientHeight}
	}
	return out, nil
}

// elementPageRect resolves the selector and returns the element's settled box in
// page coordinates.
//
// It resolves through resolveNodeReady (a PRESENT-not-visible wait, so it works
// on a hidden tab) and computes geometry in JS, for the same reason every
// pointer verb does: a DOM.getBoxModel read is wrong on a backgrounded tab.
func elementPageRect(ctx context.Context, opts ShotOpts) (Rect, error) {
	nid, err := resolveNodeReady(ctx, opts.Selector, opts.Query)
	if err != nil {
		return Rect{}, err
	}
	obj, err := dom.ResolveNode().WithNodeID(nid).Do(ctx)
	if err != nil {
		return Rect{}, err
	}
	if obj == nil || obj.ObjectID == "" {
		return Rect{}, fmt.Errorf("node has no remote object")
	}
	return settledPageRect(ctx, obj.ObjectID)
}

// settledPageRect scrolls the element into view and then RE-READS its box until
// two consecutive reads agree.
//
// The re-read is the whole point. Scrolling is what makes an offscreen element
// capturable, and it is also what moves things: scroll handlers reposition,
// sticky headers reflow, content above expands as it lazy-loads. A clip computed
// before the scroll describes where the element used to be, and the capture then
// comes back at exactly the right size showing the wrong part of the page —
// a bug that looks like a rendering problem rather than an arithmetic one. It has
// its own regression test for that reason.
func settledPageRect(ctx context.Context, objID cdpruntime.RemoteObjectID) (Rect, error) {
	if _, err := callOnObject(ctx, objID, scrollIntoViewJS); err != nil {
		return Rect{}, err
	}
	t := time.NewTicker(60 * time.Millisecond)
	defer t.Stop()
	var prev Rect
	have := false
	for {
		r, err := readPageRect(ctx, objID)
		if err != nil {
			return Rect{}, err
		}
		if have && sameRect(prev, r) {
			if r.Width < 1 || r.Height < 1 {
				return Rect{}, ErrZeroArea
			}
			return r, nil
		}
		prev, have = r, true
		select {
		case <-ctx.Done():
			return Rect{}, fmt.Errorf("element box never settled: %w", ctx.Err())
		case <-t.C:
		}
	}
}

// readPageRect reads one element box in page coordinates.
func readPageRect(ctx context.Context, objID cdpruntime.RemoteObjectID) (Rect, error) {
	res, err := callOnObject(ctx, objID, pageRectJS)
	if err != nil {
		return Rect{}, err
	}
	var r Rect
	if err := json.Unmarshal(res, &r); err != nil {
		return Rect{}, err
	}
	return r, nil
}

// scrollIntoViewJS brings the element into view so it is rendered (and, for a
// lazy page, so its content loads) before its box is read.
const scrollIntoViewJS = `function() {
  try { this.scrollIntoView({block:"center", inline:"nearest"}); } catch (e) {}
  return true;
}`

// pageRectJS returns the element's box in PAGE coordinates — the viewport rect
// plus the scroll offset — which is the space captureScreenshot clips in, and is
// therefore stable no matter where the page ends up scrolled.
const pageRectJS = `function() {
  const r = this.getBoundingClientRect();
  return { x: r.left + window.scrollX, y: r.top + window.scrollY, width: r.width, height: r.height };
}`

// sameRect reports whether two boxes agree to within half a pixel — subpixel
// layout jitter is not movement.
func sameRect(a, b Rect) bool {
	return math.Abs(a.X-b.X) < 0.5 && math.Abs(a.Y-b.Y) < 0.5 &&
		math.Abs(a.Width-b.Width) < 0.5 && math.Abs(a.Height-b.Height) < 0.5
}

// padRect expands a clip by pad on every side and clamps it to the page, so
// padding an element at the top-left corner cannot produce a negative origin
// (which Chrome renders as blank margin) and padding at the bottom cannot run
// off the end of the document.
func padRect(r Rect, pad float64, bounds Rect) Rect {
	if pad > 0 {
		r = Rect{X: r.X - pad, Y: r.Y - pad, Width: r.Width + 2*pad, Height: r.Height + 2*pad}
	}
	return clampRect(r, bounds)
}

// clampRect intersects r with the page bounds, leaving r alone when the bounds
// are unknown.
func clampRect(r, bounds Rect) Rect {
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return r
	}
	left := math.Max(r.X, bounds.X)
	top := math.Max(r.Y, bounds.Y)
	right := math.Min(r.X+r.Width, bounds.X+bounds.Width)
	bottom := math.Min(r.Y+r.Height, bounds.Y+bounds.Height)
	if right <= left || bottom <= top {
		return r // degenerate intersection: report what was asked for
	}
	return Rect{X: left, Y: top, Width: right - left, Height: bottom - top}
}

// imageDims reports the artifact's real pixel dimensions by decoding its header,
// rather than trusting the requested clip — the two differ whenever the device
// scale factor is not 1. WebP is not decodable by the standard library, so it
// falls back to the computed size.
func imageDims(buf []byte, clip Rect, scale float64) (int, int) {
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(buf)); err == nil {
		return cfg.Width, cfg.Height
	}
	return int(math.Round(clip.Width * scale)), int(math.Round(clip.Height * scale))
}

// pdfPagesRe and pdfPageObjRe read a PDF's page count without a PDF library: the
// page tree's /Count, else a count of /Type /Page objects. Crude, but the
// alternative is a dependency for one integer.
var (
	pdfPagesRe   = regexp.MustCompile(`/Type\s*/Pages\b[^>]*?/Count\s+(\d+)|/Count\s+(\d+)[^>]*?/Type\s*/Pages\b`)
	pdfPageObjRe = regexp.MustCompile(`/Type\s*/Page[^s]`)
)

// PDFPageCount returns the number of pages in a PDF document, or 0 when the
// structure is not readable (an object-stream-compressed page tree, say) — the
// envelope reports what it can rather than failing a capture that succeeded.
func PDFPageCount(b []byte) int {
	for _, m := range pdfPagesRe.FindAllSubmatch(b, -1) {
		for _, g := range m[1:] {
			if n, err := strconv.Atoi(string(g)); err == nil && n > 0 {
				return n
			}
		}
	}
	return len(pdfPageObjRe.FindAll(b, -1))
}
