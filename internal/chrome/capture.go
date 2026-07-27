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
			// Re-read the page bounds AFTER the element settled, for the same
			// reason the box itself is re-read: scrolling into view is what runs
			// the page's scroll handlers, and those can grow or shrink the
			// document. Clamping the clip to the pre-scroll page box would crop a
			// capture against bounds that no longer exist.
			if metrics, err = layoutRects(ctx); err != nil {
				return err
			}
			clip = padRect(clip, opts.Padding, metrics.page)
		case opts.FullPage:
			// No scroll, so no re-read: the content box comes from the layout
			// metrics read a moment ago and nothing between there and the capture
			// touches the page. Same for --region, which is verbatim arithmetic on
			// what the caller asked for.
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
// the page has PROCESSED that scroll and two consecutive reads agree.
//
// The re-read is the whole point. Scrolling is what makes an offscreen element
// capturable, and it is also what moves things: scroll handlers reposition,
// sticky headers reflow, content above expands as it lazy-loads. A clip computed
// before the scroll describes where the element used to be, and the capture then
// comes back at exactly the right size showing the wrong part of the page —
// a bug that looks like a rendering problem rather than an arithmetic one. It has
// its own regression test for that reason.
//
// "Two consecutive reads agree" is NOT on its own enough, which is the subtle
// half. A scroll event is not dispatched by the scroll: it is queued and fired
// when the page next runs its rendering steps. Until then every read — however
// many, however far apart — returns the same pre-scroll box, and a settle loop
// that only compares reads accepts that stale answer the moment the page is slow
// enough to miss its first frame. So the loop also waits for the scroll to have
// been OBSERVED by a listener installed before the scroll, and drives the page's
// rendering steps itself (pokeFrame) instead of waiting for a frame that a
// hidden or throttled tab will never produce. No sleep and no interval tuning:
// the loop advances the page rather than betting on when the page advances.
func settledPageRect(ctx context.Context, objID cdpruntime.RemoteObjectID) (Rect, error) {
	w, err := scrollIntoViewWatched(ctx, objID)
	if err != nil {
		return Rect{}, err
	}
	defer w.release(ctx)

	t := time.NewTicker(60 * time.Millisecond)
	defer t.Stop()
	var prev Rect
	have := false
	for {
		// Every iteration, not just while waiting for the scroll event: on a tab
		// that produces no frames of its own, this is the only thing that makes a
		// deferred relayout (a handler that moves the element from a rAF, an
		// image that lays out when it decodes) ever happen — and therefore the
		// only thing that makes "the reads agree" mean the page has stopped
		// moving rather than that it never started.
		pokeFrame(ctx)

		r, processed, err := w.read(ctx)
		if err != nil {
			return Rect{}, err
		}
		if processed {
			if have && sameRect(prev, r) {
				if r.Width < 1 || r.Height < 1 {
					return Rect{}, ErrZeroArea
				}
				return r, nil
			}
			prev, have = r, true
		}
		select {
		case <-ctx.Done():
			return Rect{}, fmt.Errorf("element box never settled after scrolling it into view: %w", ctx.Err())
		case <-t.C:
		}
	}
}

// scrollWatch is a handle on the page-side object scrollWatchJS leaves behind:
// the element, the capture-phase scroll listener installed before the scroll,
// and the flag that listener sets. Keeping it page-side (rather than in a global)
// means the watch cannot collide with a concurrent capture or leave a name on
// the user's window.
type scrollWatch struct {
	obj cdpruntime.RemoteObjectID
}

// scrollWatchJS installs the watch and scrolls, in that order — the ordering is
// the load-bearing part, since a listener added after the scroll can never see
// it.
//
// It also reports whether the scroll actually moved anything (`moved`), by
// comparing the scroll offsets of every ancestor plus the window across the
// call. An element already in view scrolls nothing and so will never produce a
// scroll event; waiting for one would hang until the caller's deadline.
const scrollWatchJS = `function() {
  const el = this;
  // Every scroll container that scrollIntoView could move, crossing shadow
  // boundaries via the host.
  const ancestors = [];
  for (let n = el; n; ) {
    n = n.parentElement || (n.getRootNode && n.getRootNode().host) || null;
    if (n) ancestors.push(n);
  }
  const snap = () => ancestors.map(n => n.scrollLeft + ":" + n.scrollTop).join("|") +
                     "#" + window.scrollX + ":" + window.scrollY;

  const state = {
    el: el,
    scrolled: false,
    moved: false,
    onScroll: null,
    read: function () {
      const r = this.el.getBoundingClientRect();
      // PAGE coordinates — the viewport rect plus the scroll offset — which is
      // the space captureScreenshot clips in, and is therefore stable no matter
      // where the page ends up scrolled.
      return { scrolled: this.scrolled, moved: this.moved,
               x: r.left + window.scrollX, y: r.top + window.scrollY,
               width: r.width, height: r.height };
    },
    stop: function () { removeEventListener("scroll", this.onScroll, true); return true; }
  };
  state.onScroll = function () { state.scrolled = true; };
  // Capture phase on window: it sees scroll events on nested containers too,
  // which do not bubble.
  addEventListener("scroll", state.onScroll, true);

  const before = snap();
  // "instant" so the scroll completes within this call and moved is the truth
  // rather than the first frame of a CSS scroll-behavior:smooth animation.
  try { el.scrollIntoView({block:"center", inline:"nearest", behavior:"instant"}); }
  catch (e) { try { el.scrollIntoView(); } catch (e2) {} }
  state.moved = snap() !== before;
  return state;
}`

// scrollIntoViewWatched installs the watch, scrolls the element into view, and
// returns the handle to poll. The state object is returned BY HANDLE, not by
// value, because the flag it carries is read repeatedly afterwards.
func scrollIntoViewWatched(ctx context.Context, objID cdpruntime.RemoteObjectID) (*scrollWatch, error) {
	res, exc, err := cdpruntime.CallFunctionOn(scrollWatchJS).WithObjectID(objID).Do(ctx)
	if err != nil {
		return nil, err
	}
	if exc != nil {
		return nil, fmt.Errorf("scrolling into view: %s", exc.Text)
	}
	if res == nil || res.ObjectID == "" {
		return nil, fmt.Errorf("scroll watch returned no object")
	}
	return &scrollWatch{obj: res.ObjectID}, nil
}

// read returns the element's current page box and whether the scroll has been
// processed — either a scroll event has been dispatched, or the scrollIntoView
// moved nothing and none is coming.
func (w *scrollWatch) read(ctx context.Context) (Rect, bool, error) {
	res, err := callOnObject(ctx, w.obj, `function(){ return this.read(); }`)
	if err != nil {
		return Rect{}, false, err
	}
	var v struct {
		Rect
		Scrolled bool `json:"scrolled"`
		Moved    bool `json:"moved"`
	}
	if err := json.Unmarshal(res, &v); err != nil {
		return Rect{}, false, err
	}
	return v.Rect, v.Scrolled || !v.Moved, nil
}

// release removes the listener and drops the page-side object, so a capture
// leaves no scroll listener behind on the user's live page. Errors are ignored:
// the caller's context is usually what ended the wait, and a failed cleanup must
// not mask the capture's own outcome.
func (w *scrollWatch) release(ctx context.Context) {
	_, _ = callOnObject(ctx, w.obj, `function(){ return this.stop(); }`)
	_ = cdpruntime.ReleaseObject(w.obj).Do(ctx)
}

// pokeFrame makes the page run one rendering update by capturing a throwaway 1×1
// image.
//
// Dispatching queued scroll events is part of that update, and a tab that is not
// the frontmost one produces no updates on its own — measured: a hidden tab sat
// on its pre-scroll box for the full two seconds it was watched, then processed
// the scroll on the first poke. Waiting for a frame would therefore mean element
// capture silently returning stale geometry on any background tab, which is most
// of them for a tool that drives the user's real browser.
//
// Errors are deliberately dropped: this only accelerates something the page may
// also do by itself, so a capture that cannot be taken right now (a surface not
// yet ready) is a reason to poll again, not to fail.
func pokeFrame(ctx context.Context) {
	_, _ = page.CaptureScreenshot().
		WithFormat(page.CaptureScreenshotFormatPng).
		WithFromSurface(true).
		WithClip(&page.Viewport{Width: 1, Height: 1, Scale: 1}).
		Do(ctx)
}

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
