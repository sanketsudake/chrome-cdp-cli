// Package chrome connects to Chrome over CDP (via chromedp) and exposes the
// Browser port the CLI commands drive. Keeping commands behind this interface
// lets the command boundary be tested in-process with a fake.
package chrome

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// QueryOpts controls how a selector is interpreted (By), what state to wait for
// (Wait), and whether to pierce shadow DOM / iframes (Pierce).
type QueryOpts struct {
	By     string // css (default) | id | search | jspath | css-all | name
	Wait   string // visible (default) | ready | enabled
	Pierce bool   // reach into shadow DOM / iframes (via DevTools search)

	// Accessible-name addressing (By == "name"): the selector is the ARIA
	// accessible name; Role optionally constrains the ARIA role, and Nth picks
	// the 1-based match among the visible candidates (0 = first).
	Role string
	Nth  int

	// Match controls how the accessible name is compared (By == "name"):
	// "" / "exact" (default), "contains" (case-insensitive substring), or
	// "regex". Lets a caller click "Review" without knowing its verbose full
	// accessible name.
	Match string

	// InRow scopes an accessible-name match (By == "name") to the table row
	// whose text contains this string — so `click --by name "Delete" --in-row
	// "TEST entry"` hits the Delete button in that row, not the first of many.
	// It resolves via the DOM (not the throttled a11y tree), so it also works on
	// a backgrounded tab. Requires By == "name".
	InRow string

	// OnDialog, set on an action verb (click/type/fill), auto-handles a native
	// JavaScript dialog (alert/confirm/prompt) that opens DURING the action —
	// "accept" or "dismiss" — instead of letting it wedge the CDP connection.
	// The handled dialogs are reported back in the result envelope.
	OnDialog string
}

// SelectOpts controls the `select` verb — choosing an option in a prompt /
// combobox / cascade widget (Workday-style portal popups, or a native <select>).
type SelectOpts struct {
	// Query addresses the FIELD control (usually By=="name" with a Role like
	// textbox/combobox to disambiguate a same-named column header from the input).
	Query QueryOpts

	// OptionMatch controls how each option segment is compared against rendered
	// option text: "" / "contains" (default — Workday labels are verbose),
	// "exact", or "regex".
	OptionMatch string

	// Filter is optional text typed into the prompt after it opens, to narrow a
	// long option list before the option is clicked.
	Filter string

	// Sep splits Option into cascade levels (default ">"): e.g.
	// "Project Plan Tasks > ShiftLeft: Qwiet" drills the category, then the leaf.
	Sep string
}

// KeyStroke is one resolved keyboard press: the key to dispatch plus the modifier
// bitmask held during it. It is produced by ParseKeys from a `key` verb argument
// and carries the DOM `key`/`code`/virtual-keycode tuple, because frameworks read
// all three and a page that checks `event.code` ignores a press that only sets
// `event.key`.
type KeyStroke struct {
	Key       string // DOM KeyboardEvent.key ("Escape", "a", "ArrowDown")
	Code      string // DOM KeyboardEvent.code ("Escape", "KeyA", "ArrowDown")
	KeyCode   int64  // windowsVirtualKeyCode
	Text      string // the text a printable key inserts ("" for non-printable)
	Modifiers int64  // CDP modifier bitmask (alt 1, ctrl 2, meta 4, shift 8)
}

// KeyOpts controls the `key` verb — dispatching keyboard events that are not
// literal text (named keys, modifier chords, repeats). Query addresses an
// optional element to focus first; when the selector is empty the keys go to
// whatever the page currently has focused, which is what makes `key Escape` work
// when nothing is addressable.
type KeyOpts struct {
	Repeat int           // press the sequence this many times (1..100)
	Delay  time.Duration // pause between repeats, for apps that debounce
	Query  QueryOpts
}

// PointerOpts controls the pointer verbs — click, hover, dblclick, rclick, and
// drag. One driver method backs all five, so they share the identical
// occlusion-verified centre resolution and the identical modifier handling;
// Action selects which gesture is dispatched.
type PointerOpts struct {
	Action    PointerAction
	Modifiers int64 // held during the press (alt 1, ctrl 2, meta 4, shift 8)

	// Drag targeting: either To (a drop-target selector, resolved with ToQuery)
	// or a (Dx, Dy) pixel delta from the source's centre. Exactly one is set.
	To      string
	ToQuery QueryOpts
	Dx, Dy  float64

	// Steps is the number of interpolated move events dispatched between the
	// press and the release. It is not cosmetic: drag implementations require
	// movement to register a drag at all, and a press-then-release at two points
	// is silently a click.
	Steps int
	Hold  time.Duration // pause after press before moving (long-press-to-drag UIs)

	Query QueryOpts
}

// PointerAction names a pointer verb.
type PointerAction string

// The pointer actions a single Pointer call can dispatch.
const (
	PointerClick    PointerAction = "click"
	PointerHover    PointerAction = "hover"
	PointerDblClick PointerAction = "dblclick"
	PointerRClick   PointerAction = "rclick"
	PointerDrag     PointerAction = "drag"
)

// UploadOpts controls the `upload` verb — attaching local files to a file input
// via DOM.setFileInputFiles.
//
// There is no "click the input first" option and there never will be: clicking a
// file input opens the NATIVE OS file dialog, which lives outside the page, is
// invisible to CDP, blocks the browser's main thread, and — unlike a JavaScript
// dialog, which --on-dialog handles — has no CDP method that can dismiss it.
type UploadOpts struct {
	// Append adds to the files this session already set on the input instead of
	// replacing them. It can only be honoured for files this CLI set itself:
	// setFileInputFiles replaces the FileList wholesale, and the existing
	// entries' PATHS are not readable back from the DOM (File.name is the bare
	// name by design). Appending to an input whose prior contents are unknown is
	// a usage error rather than a guess.
	Append bool

	Query QueryOpts
}

// SnapOpts filters an accessibility snapshot server-side, so a read returns just
// the relevant nodes instead of the whole tree. Alerts/focused stay page-wide.
type SnapOpts struct {
	Role   string // only nodes with this ARIA role
	Grep   string // only nodes whose accessible name matches this regex
	Region string // only nodes within the subtree of a container whose name contains this
	Dedupe bool   // collapse identical role+name (keep first) — for virtualized grids
}

// ScrollOpts controls the `scroll` verb:
//   - Into: scroll Selector into view.
//   - otherwise: scroll by (Dx, Dy) pixels — Selector's scroll box, or the window
//     when Selector is empty. This is a JS scrollBy (deterministic; it fires the
//     scroll events virtualized grids render on).
//   - Wheel: instead dispatch a real mouse-wheel of (Dx, Dy) at Selector's centre
//     (viewport centre if empty), for grids that listen for wheel specifically.
type ScrollOpts struct {
	Dx    float64
	Dy    float64
	Into  bool
	Wheel bool
	Query QueryOpts
}

// WaitCond is a condition for the `wait` verb: settle until the target URL
// contains URL, or a selector becomes Visible / is Gone. Exactly one is set;
// Query carries the selector options for Visible/Gone. A fixed --for duration is
// handled in the CLI (no browser round-trip needed).
type WaitCond struct {
	URL     string
	Visible string
	Gone    string
	Text    string // until the accessibility tree contains this text (e.g. a "Success" toast)
	Stable  bool   // until the accessibility tree stops changing (the page settled)
	Idle    bool   // until network activity settles (no in-flight requests for a window)
	Query   QueryOpts
}

// ShotMode names which capture path a screenshot took. It is reported in the
// envelope so a caller can confirm which of the four ran without inspecting the
// image.
type ShotMode string

// The capture modes a single Screenshot call can take.
const (
	ShotViewport ShotMode = "viewport"
	ShotElement  ShotMode = "element"
	ShotFullPage ShotMode = "full_page"
	ShotRegion   ShotMode = "region"
)

// Rect is a rectangle in PAGE (document) coordinates, in CSS pixels — the same
// space Page.captureScreenshot's clip uses, so it is unaffected by scrolling.
// It is reported back as the envelope's `clip`: the single most useful field for
// debugging a capture that came out wrong.
type Rect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// ShotOpts controls the `screenshot` verb. Selector, FullPage, and Region select
// the capture mode and are mutually exclusive — the CLI rejects more than one
// before connecting; the driver treats them in that precedence order.
//
// Every value here is already validated and normalized by the CLI: Format is a
// known enum, Quality is in range and only set for a lossy format, Scale is
// within 0.1–3. The driver does not re-check user input.
type ShotOpts struct {
	Selector string // capture this element's box (all QueryOpts flags apply)
	FullPage bool   // capture the whole scrollable page, beyond the fold
	Region   *Rect  // capture an explicit page-coordinate rectangle

	Format  string  // png (default) | jpeg | webp
	Quality int     // 0–100; jpeg/webp only (ignored for png, which the CLI rejects)
	Scale   float64 // output scale factor, 0.1–3 (0 means 1)
	Padding float64 // expand an element clip by this many px, clamped to the page

	Query QueryOpts
}

// PDFOpts controls the `pdf` verb. Every user-facing spelling — named paper
// sizes, `WxH` inches, one-or-four-value margins, page ranges — is parsed in the
// CLI, so the driver receives numbers only and has no grammar of its own.
//
// Lengths are inches, matching Page.printToPDF. Zero margins are meaningful (the
// CLI always resolves a value, defaulting to 0.4in), while a zero paper size
// means "leave Chrome's default".
type PDFOpts struct {
	Landscape    bool
	PaperWidth   float64 // inches; 0 = Chrome's default (letter)
	PaperHeight  float64 // inches; 0 = Chrome's default (letter)
	MarginTop    float64 // inches
	MarginRight  float64 // inches
	MarginBottom float64 // inches
	MarginLeft   float64 // inches
	Scale        float64 // 0.1–2 (0 means 1)
	Background   bool    // print background graphics
	Pages        string  // page ranges, e.g. "1-3,5" ("" = the whole document)
	Header       string  // HTML template for the print header
	Footer       string  // HTML template for the print footer
}

// TextOpts controls the `text` verb. The zero value is the long-standing
// behaviour: the visible text of Query's selector, verbatim.
//
// Article turns on Readability-style main-content extraction, which is a
// heuristic — so MinChars is the honesty threshold below which the extraction is
// reported as failed (`extracted: false`) and the FULL page text is returned
// instead of a plausible-looking fragment. Markdown preserves headings, lists,
// links, code blocks, and blockquotes; it is deliberately not a general
// HTML-to-markdown converter.
type TextOpts struct {
	Article  bool // extract the main readable content, dropping boilerplate
	Markdown bool // with Article: emit markdown structure instead of plain text
	MinChars int  // with Article: below this many extracted chars, report failure
	Query    QueryOpts
}

// EvalOpts controls the `eval` verb. Await switches Runtime.evaluate to REPL
// semantics — `awaitPromise` plus `replMode` — so a top-level `await` works and
// a statement list yields its final expression, exactly as DevTools' own console
// behaves. It is opt-in: replMode changes how bare object literals and
// `let`/`const` re-declaration are treated, so turning it on by default would
// silently alter existing scripts.
type EvalOpts struct {
	Await bool
}

// Browser is the set of Chrome operations the CLI commands need. The real
// implementation is CDP (chromedp-backed); tests use a fake.
type Browser interface {
	List(ctx context.Context) ([]target.Info, error)
	Open(ctx context.Context, url string) (map[string]any, error)
	Navigate(ctx context.Context, targetID, url string) (map[string]any, error)
	// CloseTabs closes tabs by id. It is not Close, which tears down the whole
	// connection.
	CloseTabs(ctx context.Context, targetIDs []string) (map[string]any, error)
	// Activate foregrounds a tab within its window and raises that window, which
	// is the documented remedy for the accessibility-tree throttling that makes
	// --by name/ref/cell stall on a backgrounded tab (`tab_hidden`).
	Activate(ctx context.Context, targetID string) (map[string]any, error)
	// History navigates the tab by delta entries (-1 back, +1 forward). A delta
	// with no entry in that direction is an error, not a silent no-op: a wizard
	// script that quietly failed to go back would act against the wrong page.
	History(ctx context.Context, targetID string, delta int) (map[string]any, error)
	// Reload reloads the tab, optionally bypassing the cache.
	Reload(ctx context.Context, targetID string, hard bool) (map[string]any, error)
	Eval(ctx context.Context, targetID, expr string, opts EvalOpts) (any, error)
	Snapshot(ctx context.Context, targetID string, opts SnapOpts) (any, error)
	Key(ctx context.Context, targetID, selector string, keys []KeyStroke, opts KeyOpts) (map[string]any, error)
	// Pointer dispatches every pointer gesture — click included. There is no
	// separate Click: one method means one centre resolution and one place
	// modifiers are applied, which is what lets `click --modifiers cmd`
	// multi-select without a second implementation drifting from the first.
	Pointer(ctx context.Context, targetID, selector string, opts PointerOpts) (map[string]any, error)
	Select(ctx context.Context, targetID, field, option string, opts SelectOpts) (map[string]any, error)
	Grid(ctx context.Context, targetID, selector string, q QueryOpts) (any, error)
	Scroll(ctx context.Context, targetID, selector string, opts ScrollOpts) (map[string]any, error)
	Type(ctx context.Context, targetID, selector, text string, q QueryOpts) (map[string]any, error)
	Fill(ctx context.Context, targetID, selector, value string, q QueryOpts) (map[string]any, error)
	// Upload attaches local files to a file input. The paths must already be
	// absolute, existing, readable files — CDP requires absolute paths, and the
	// CLI validates them before connecting so a bad path never costs a
	// connection. The result reports the files READ BACK from the input.
	Upload(ctx context.Context, targetID, selector string, paths []string, opts UploadOpts) (map[string]any, error)
	HTML(ctx context.Context, targetID, selector string, inner bool, q QueryOpts) (map[string]any, error)
	Text(ctx context.Context, targetID, selector string, opts TextOpts) (map[string]any, error)
	Value(ctx context.Context, targetID, selector string, q QueryOpts) (map[string]any, error)
	Values(ctx context.Context, targetID, selector string, q QueryOpts) (map[string]any, error)
	AttrGet(ctx context.Context, targetID, selector, name string, q QueryOpts) (map[string]any, error)
	AttrList(ctx context.Context, targetID, selector string, q QueryOpts) (map[string]any, error)
	AttrSet(ctx context.Context, targetID, selector, name, value string, q QueryOpts) (map[string]any, error)
	AttrRemove(ctx context.Context, targetID, selector, name string, q QueryOpts) (map[string]any, error)
	SetHeaders(ctx context.Context, targetID string, headers map[string]string) (map[string]any, error)
	EmulateViewport(ctx context.Context, targetID string, width, height int64) (map[string]any, error)
	EmulateGeo(ctx context.Context, targetID string, lat, lon float64) (map[string]any, error)
	EmulateReset(ctx context.Context, targetID string) (map[string]any, error)
	Frames(ctx context.Context, targetID string) (any, error)
	Wait(ctx context.Context, targetID string, cond WaitCond) (map[string]any, error)
	// Screenshot and PDF return the artifact bytes AND the metadata describing
	// them (dimensions, format, mode, resolved clip / page count). The metadata
	// travels with the bytes because only the driver can know it without the CLI
	// decoding the image itself.
	Screenshot(ctx context.Context, targetID string, opts ShotOpts) ([]byte, map[string]any, error)
	PDF(ctx context.Context, targetID string, opts PDFOpts) ([]byte, map[string]any, error)
	CookieList(ctx context.Context, targetID string) (any, error)
	CookieSet(ctx context.Context, targetID, name, value, domain, path string) (map[string]any, error)
	CookieDelete(ctx context.Context, targetID, name string) (map[string]any, error)
	CookieClear(ctx context.Context, targetID string) (map[string]any, error)
	Raw(ctx context.Context, targetID, method string, params json.RawMessage) (any, error)
	Close() error
}
