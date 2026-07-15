// Package chrome connects to Chrome over CDP (via chromedp) and exposes the
// Browser port the CLI commands drive. Keeping commands behind this interface
// lets the command boundary be tested in-process with a fake.
package chrome

import (
	"context"
	"encoding/json"

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
}

// WaitCond is a condition for the `wait` verb: settle until the target URL
// contains URL, or a selector becomes Visible / is Gone. Exactly one is set;
// Query carries the selector options for Visible/Gone. A fixed --for duration is
// handled in the CLI (no browser round-trip needed).
type WaitCond struct {
	URL     string
	Visible string
	Gone    string
	Query   QueryOpts
}

// Browser is the set of Chrome operations the CLI commands need. The real
// implementation is CDP (chromedp-backed); tests use a fake.
type Browser interface {
	List(ctx context.Context) ([]target.Info, error)
	Navigate(ctx context.Context, targetID, url string) (map[string]any, error)
	Eval(ctx context.Context, targetID, expr string) (any, error)
	Snapshot(ctx context.Context, targetID string) (any, error)
	Click(ctx context.Context, targetID, selector string, q QueryOpts) (map[string]any, error)
	Type(ctx context.Context, targetID, selector, text string, q QueryOpts) (map[string]any, error)
	HTML(ctx context.Context, targetID, selector string, inner bool, q QueryOpts) (map[string]any, error)
	Text(ctx context.Context, targetID, selector string, q QueryOpts) (map[string]any, error)
	Value(ctx context.Context, targetID, selector string, q QueryOpts) (map[string]any, error)
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
	Screenshot(ctx context.Context, targetID string) ([]byte, error)
	PDF(ctx context.Context, targetID string) ([]byte, error)
	CookieList(ctx context.Context, targetID string) (any, error)
	CookieSet(ctx context.Context, targetID, name, value, domain, path string) (map[string]any, error)
	CookieDelete(ctx context.Context, targetID, name string) (map[string]any, error)
	CookieClear(ctx context.Context, targetID string) (map[string]any, error)
	Raw(ctx context.Context, targetID, method string, params json.RawMessage) (any, error)
	Close() error
}
