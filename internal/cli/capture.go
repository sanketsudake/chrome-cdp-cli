package cli

// The capture verbs — screenshot and pdf (RFC-0008).
//
// Every user-facing spelling is parsed HERE, into numbers, before anything
// connects to Chrome: region rectangles, paper sizes, margin specs, page ranges.
// Each is a pure function with a table-driven test, and each rejection is
// `usage` / exit 2 with the browser untouched — the contract agents rely on to
// know a call was wrong rather than unlucky.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// Bounds on the numeric options. They are contract, not taste: --scale 0 is a
// zero-pixel image and --scale 50 is a texture-limit failure several seconds
// later, and both are better refused up front.
const (
	shotScaleMin float64 = 0.1
	shotScaleMax float64 = 3
	pdfScaleMin  float64 = 0.1
	pdfScaleMax  float64 = 2
)

// shotExt maps an image format to the extension the default filename gets.
// jpeg's file extension is .jpg, which is what every viewer and uploader
// expects.
var shotExt = map[string]string{"png": "png", "jpeg": "jpg", "webp": "webp"}

func (a *App) cmdScreenshot() *cobra.Command {
	var out, selector, region, format string
	var fullPage bool
	var quality int
	var scale, padding float64
	c := &cobra.Command{
		Use:   "screenshot",
		Short: "Capture the tab: the viewport, an element, a region, or the full page",
		Long: "Capture the target tab as an image.\n\n" +
			"With no flags it captures the viewport as a PNG, as it always has.\n" +
			"--selector, --full-page and --region select the other three modes and are\n" +
			"mutually exclusive; the envelope reports which one ran (`mode`) and the\n" +
			"rectangle it resolved (`clip`), so a capture that came out wrong is\n" +
			"debuggable without opening the image.\n\n" +
			"  chrome-cdp screenshot --selector \"#invoice-table\" --padding 8 -o invoice.png\n" +
			"  chrome-cdp screenshot --full-page -o report.png\n" +
			"  chrome-cdp screenshot --format jpeg --quality 60 --scale 0.5 -o small.jpg\n" +
			"  chrome-cdp screenshot --selector \"Summary card\" --by name --role region\n\n" +
			"Full-page capture does not force lazy-loaded content to load: images below\n" +
			"the fold that appear on scroll may come out blank. Scroll through the page\n" +
			"first (`scroll --dy …`, `wait --idle`) when that matters.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts, rerr := a.shotOpts(cmd, selector, region, format, fullPage, quality, scale, padding)
			if rerr != nil {
				a.emitErr("screenshot", rerr.Code, rerr.Message, rerr.Details)
				return nil
			}
			ctx, cancel := a.ctx()
			defer cancel()
			tgt, b, rerr := a.resolveTarget(ctx)
			if rerr != nil {
				a.emitErr("screenshot", rerr.Code, rerr.Message, rerr.Details)
				return nil
			}
			data, meta, err := b.Screenshot(ctx, tgt.ID, opts)
			if err != nil {
				code, msg, details := classifyCaptureErr(err)
				a.emitErr("screenshot", code, msg, details)
				return nil
			}
			a.emitArtifact("screenshot", tgt, data, out, shotExt[opts.Format], meta)
			return nil
		},
	}
	f := c.Flags()
	f.StringVarP(&out, "output", "o", "", "output path, or - for stdout (default ./screenshot-<timestamp>.<ext>)")
	f.StringVar(&selector, "selector", "", "capture this element's box (honours --by/--role/--nth/--match/--in-row/--pierce)")
	f.BoolVar(&fullPage, "full-page", false, "capture the whole scrollable page, beyond the fold")
	f.StringVar(&region, "region", "", "capture an explicit page-coordinate rectangle: x,y,w,h")
	f.StringVar(&format, "format", "png", "image format: png|jpeg|webp")
	f.IntVar(&quality, "quality", 80, "compression quality 0-100 (jpeg/webp only; an error with png)")
	f.Float64Var(&scale, "scale", 1, fmt.Sprintf("output scale factor, %g-%g", shotScaleMin, shotScaleMax))
	f.Float64Var(&padding, "padding", 0, "expand an element capture by this many pixels (clamped to the page)")
	return c
}

// shotOpts validates the screenshot flags and reduces them to driver options.
// It runs before resolveTarget, so every rejection below happens without a
// connection — the invariant nocall_test.go enforces.
func (a *App) shotOpts(cmd *cobra.Command, selector, region, format string, fullPage bool, quality int, scale, padding float64) (chrome.ShotOpts, *result.Err) {
	modes := 0
	for _, on := range []bool{selector != "", fullPage, region != ""} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return chrome.ShotOpts{}, usageErr("--selector, --full-page and --region select different capture modes; pass at most one")
	}

	format = strings.ToLower(strings.TrimSpace(format))
	if _, ok := shotExt[format]; !ok {
		return chrome.ShotOpts{}, usageErr("unknown --format %q: want png, jpeg or webp", format)
	}
	if cmd.Flags().Changed("quality") {
		if format == "png" {
			// Silently ignoring it would teach the caller that a smaller PNG is
			// one flag away, which it is not — png is lossless.
			return chrome.ShotOpts{}, usageErr("--quality applies to jpeg and webp; png is lossless — drop --quality or pass --format jpeg")
		}
		if quality < 0 || quality > 100 {
			return chrome.ShotOpts{}, usageErr("--quality must be between 0 and 100, got %d", quality)
		}
	}
	if scale < shotScaleMin || scale > shotScaleMax {
		return chrome.ShotOpts{}, usageErr("--scale must be between %g and %g, got %g", shotScaleMin, shotScaleMax, scale)
	}
	if padding < 0 {
		return chrome.ShotOpts{}, usageErr("--padding must not be negative, got %g", padding)
	}

	opts := chrome.ShotOpts{
		Selector: selector,
		FullPage: fullPage,
		Format:   format,
		Quality:  quality,
		Scale:    scale,
		Padding:  padding,
		Query:    a.queryOpts(),
	}
	if region != "" {
		r, err := parseRegion(region)
		if err != nil {
			return chrome.ShotOpts{}, usageErr("%s", err.Error())
		}
		opts.Region = r
	}
	return opts, nil
}

func (a *App) cmdPDF() *cobra.Command {
	var out, paper, margin, pages, header, footer string
	var landscape, background bool
	var scale float64
	c := &cobra.Command{
		Use:   "pdf",
		Short: "Print the target tab to PDF (cwd, or -o)",
		Long: "Print the target tab to PDF with the page setup Chrome's printer exposes.\n\n" +
			"  chrome-cdp pdf --landscape --paper a4 --background -o report.pdf\n" +
			"  chrome-cdp pdf --pages 1-3,7 --margin 0.5in,1in,0.5in,1in\n\n" +
			"Background graphics are off by default, matching a browser's own print\n" +
			"dialog; pass --background for a PDF that looks like the screen.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			opts, rerr := pdfOpts(paper, margin, pages, header, footer, landscape, background, scale)
			if rerr != nil {
				a.emitErr("pdf", rerr.Code, rerr.Message, rerr.Details)
				return nil
			}
			ctx, cancel := a.ctx()
			defer cancel()
			tgt, b, rerr := a.resolveTarget(ctx)
			if rerr != nil {
				a.emitErr("pdf", rerr.Code, rerr.Message, rerr.Details)
				return nil
			}
			data, meta, err := b.PDF(ctx, tgt.ID, opts)
			if err != nil {
				code, msg, details := classifyCaptureErr(err)
				a.emitErr("pdf", code, msg, details)
				return nil
			}
			a.emitArtifact("pdf", tgt, data, out, "pdf", meta)
			return nil
		},
	}
	f := c.Flags()
	f.StringVarP(&out, "output", "o", "", "output path, or - for stdout (default ./pdf-<timestamp>.pdf)")
	f.BoolVar(&landscape, "landscape", false, "landscape orientation")
	f.StringVar(&paper, "paper", "letter", "paper size: letter|legal|tabloid|a0..a6, or WxH in inches (e.g. 8.5x11)")
	f.StringVar(&margin, "margin", "0.4in", "margins: one value, or top,right,bottom,left (units: in|cm|mm|px|pt, default in)")
	f.Float64Var(&scale, "scale", 1, fmt.Sprintf("render scale, %g-%g", pdfScaleMin, pdfScaleMax))
	f.BoolVar(&background, "background", false, "print background graphics")
	f.StringVar(&pages, "pages", "", "page ranges to print, e.g. 1-3,5 (default: all)")
	f.StringVar(&header, "header", "", "HTML template for the page header (classes: date,title,url,pageNumber,totalPages)")
	f.StringVar(&footer, "footer", "", "HTML template for the page footer")
	return c
}

// pdfOpts validates the pdf flags and reduces every spelling to numbers, so the
// driver never sees a paper name or a margin unit.
func pdfOpts(paper, margin, pages, header, footer string, landscape, background bool, scale float64) (chrome.PDFOpts, *result.Err) {
	w, h, err := parsePaper(paper)
	if err != nil {
		return chrome.PDFOpts{}, usageErr("%s", err.Error())
	}
	m, err := parseMargins(margin)
	if err != nil {
		return chrome.PDFOpts{}, usageErr("%s", err.Error())
	}
	if err := validatePageRanges(pages); err != nil {
		return chrome.PDFOpts{}, usageErr("%s", err.Error())
	}
	if scale < pdfScaleMin || scale > pdfScaleMax {
		return chrome.PDFOpts{}, usageErr("--scale must be between %g and %g, got %g", pdfScaleMin, pdfScaleMax, scale)
	}
	return chrome.PDFOpts{
		Landscape:    landscape,
		PaperWidth:   w,
		PaperHeight:  h,
		MarginTop:    m[0],
		MarginRight:  m[1],
		MarginBottom: m[2],
		MarginLeft:   m[3],
		Scale:        scale,
		Background:   background,
		Pages:        strings.TrimSpace(pages),
		Header:       header,
		Footer:       footer,
	}, nil
}

// usageErr builds the exit-2 result error every parse failure above returns.
func usageErr(format string, args ...any) *result.Err {
	return &result.Err{Code: result.CodeUsage, Message: fmt.Sprintf(format, args...)}
}

// classifyCaptureErr maps a capture failure onto the error contract. A zero-area
// element is reported as `zero_area` rather than a bare timeout: the selector
// was right and the element is present, it simply has nothing to show, and
// "fix your selector" is the wrong advice for `display:none`.
func classifyCaptureErr(err error) (string, string, map[string]any) {
	if chrome.IsZeroArea(err) {
		return result.CodeTargetTimeout, err.Error(), map[string]any{"zero_area": true}
	}
	return classifyActionErr(err), err.Error(), nil
}

// parseRegion parses `x,y,w,h` in page coordinates. Every rejection here is a
// caller's typo, so the message names the expected shape.
func parseRegion(s string) (*chrome.Rect, error) {
	v, ok := parseFloats(s, 4)
	if !ok {
		return nil, fmt.Errorf("--region %q must be x,y,w,h — four numbers in page pixels", s)
	}
	if v[0] < 0 || v[1] < 0 {
		return nil, fmt.Errorf("--region %q: x and y must not be negative", s)
	}
	if v[2] <= 0 || v[3] <= 0 {
		return nil, fmt.Errorf("--region %q: width and height must be positive", s)
	}
	return &chrome.Rect{X: v[0], Y: v[1], Width: v[2], Height: v[3]}, nil
}

// paperSizes are the named page sizes, in inches, matching the print dialog's
// own table. Lookup is case-insensitive.
var paperSizes = map[string][2]float64{
	"letter":  {8.5, 11},
	"legal":   {8.5, 14},
	"tabloid": {11, 17},
	"ledger":  {17, 11},
	"a0":      {33.11, 46.81},
	"a1":      {23.39, 33.11},
	"a2":      {16.54, 23.39},
	"a3":      {11.69, 16.54},
	"a4":      {8.27, 11.69},
	"a5":      {5.83, 8.27},
	"a6":      {4.13, 5.83},
}

// parsePaper resolves a paper name, or a `WxH` pair of inches, to dimensions.
func parsePaper(s string) (width, height float64, err error) {
	name := strings.ToLower(strings.TrimSpace(s))
	if name == "" {
		return 0, 0, nil // leave Chrome's default
	}
	if d, ok := paperSizes[name]; ok {
		return d[0], d[1], nil
	}
	if w, h, ok := strings.Cut(name, "x"); ok {
		wf, werr := strconv.ParseFloat(strings.TrimSpace(w), 64)
		hf, herr := strconv.ParseFloat(strings.TrimSpace(h), 64)
		if werr == nil && herr == nil && wf > 0 && hf > 0 && !math.IsInf(wf, 0) && !math.IsInf(hf, 0) {
			return wf, hf, nil
		}
	}
	return 0, 0, fmt.Errorf("unknown --paper %q: want a name (letter, legal, tabloid, a0-a6) or WxH in inches (e.g. 8.5x11)", s)
}

// parseMargins parses the CSS-like margin spec into top/right/bottom/left
// inches: one value applies to all four, four apply in that order.
func parseMargins(s string) ([4]float64, error) {
	var out [4]float64
	parts := strings.Split(strings.TrimSpace(s), ",")
	switch len(parts) {
	case 1, 4:
	default:
		return out, fmt.Errorf("--margin %q must be one value or four (top,right,bottom,left), got %d", s, len(parts))
	}
	vals := make([]float64, 0, 4)
	for _, p := range parts {
		v, err := parseLengthInches(p)
		if err != nil {
			return out, fmt.Errorf("--margin %q: %w", s, err)
		}
		vals = append(vals, v)
	}
	if len(vals) == 1 {
		return [4]float64{vals[0], vals[0], vals[0], vals[0]}, nil
	}
	return [4]float64{vals[0], vals[1], vals[2], vals[3]}, nil
}

// inchesPer converts a supported length unit to inches. A bare number is inches,
// which is what Page.printToPDF speaks.
var inchesPer = map[string]float64{"": 1, "in": 1, "cm": 1 / 2.54, "mm": 1 / 25.4, "px": 1 / 96.0, "pt": 1 / 72.0}

// parseLengthInches parses "0.4", "0.4in", "10mm", "1cm", "48px" into inches.
func parseLengthInches(s string) (float64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("empty length")
	}
	unit := ""
	for _, u := range []string{"in", "cm", "mm", "px", "pt"} {
		if strings.HasSuffix(t, u) {
			unit, t = u, strings.TrimSpace(strings.TrimSuffix(t, u))
			break
		}
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%q is not a length", strings.TrimSpace(s))
	}
	if v < 0 {
		return 0, fmt.Errorf("%q must not be negative", strings.TrimSpace(s))
	}
	return v * inchesPer[unit], nil
}

// validatePageRanges checks the `1-3,5` grammar Chrome accepts, so a typo is
// exit 2 here rather than a protocol error after the render.
func validatePageRanges(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("--pages %q has an empty range", s)
		}
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := parsePageNumber(lo)
		if err != nil {
			return fmt.Errorf("--pages %q: %w", s, err)
		}
		if !isRange {
			continue
		}
		last, err := parsePageNumber(hi)
		if err != nil {
			return fmt.Errorf("--pages %q: %w", s, err)
		}
		if last < first {
			return fmt.Errorf("--pages %q: range %q ends before it starts", s, part)
		}
	}
	return nil
}

func parsePageNumber(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not a page number (1-based)", strings.TrimSpace(s))
	}
	return n, nil
}

// emitArtifact writes binary output: raw to stdout for "-o -", else to `out`
// (or a default ./<command>-<ts>.<ext> with a collision counter), then emits.
// The driver's metadata (dimensions, format, mode, clip / page count) is merged
// into the result alongside path and bytes.
func (a *App) emitArtifact(command string, tgt *result.TargetInfo, data []byte, out, ext string, meta map[string]any) {
	if out == "-" {
		_, _ = a.out.Write(data)
		if !a.quiet {
			fmt.Fprintf(a.err, "wrote %d bytes to stdout\n", len(data))
		}
		return
	}
	path := out
	if path == "" {
		// The default name gets a collision counter; an explicit -o path is
		// honored as-is (overwrite), as the user named it.
		path = uniquePath(fmt.Sprintf("./%s-%s.%s", command, time.Now().Format("20060102-150405"), ext))
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		a.emitErr(command, result.CodeGeneric, err.Error(), nil)
		return
	}
	res := map[string]any{"path": path, "bytes": len(data)}
	for k, v := range meta {
		res[k] = v
	}
	a.emitOK(command, tgt, res)
}

// uniquePath returns path if free, else inserts -1, -2, … before the extension.
func uniquePath(path string) string {
	if _, err := os.Stat(path); err != nil {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(cand); err != nil {
			return cand
		}
	}
}
