package mcp

// Request-scoped cancellation.
//
// The command tree roots its contexts at context.Background() on purpose (see
// the comment on chrome.CDP: the allocator, base and tab contexts must outlive
// any one command, or a deferred cancel would close the user's real tab), so a
// per-command deadline is the only thing it derives. That leaves the MCP front
// end with a problem the CLI does not have: an MCP client can CANCEL an
// in-flight request, and a `wait_for` that kept blocking after the client gave
// up would hold the connection and leak a goroutine (RFC-0004 VS-12).
//
// The seam we own is the Browser we hand the command tree, so cancellation is
// applied there: Bind returns a chrome.Browser whose every call runs under a
// context that is cancelled when the request is. Nothing about the command tree
// changes, and the driver sees an ordinary cancelled context — which is exactly
// what it would see from a timeout.
//
// TestBoundBrowserCancelsEveryMethod walks the interface by reflection and
// fails when a method is missing here, so a Browser method added by a later RFC
// cannot quietly opt out of cancellation.

import (
	"context"
	"encoding/json"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// Bind returns b with every call scoped to req: when req is cancelled, the
// context the driver sees is cancelled too. Close is deliberately NOT bound —
// it tears down the shared connection, which outlives any one request.
func Bind(b chrome.Browser, req context.Context) chrome.Browser {
	if b == nil || req == nil {
		return b
	}
	return boundBrowser{Browser: b, req: req}
}

type boundBrowser struct {
	chrome.Browser
	req context.Context
}

// bind derives the per-call context. An already-cancelled request cancels
// synchronously rather than through AfterFunc, so a caller never observes a
// live context for a request that is already gone.
func (b boundBrowser) bind(ctx context.Context) (context.Context, context.CancelFunc) {
	c, cancel := context.WithCancel(ctx)
	if b.req.Err() != nil {
		cancel()
		return c, cancel
	}
	stop := context.AfterFunc(b.req, cancel)
	return c, func() {
		stop()
		cancel()
	}
}

// Ensure the wrapper still satisfies the interface it wraps.
var _ chrome.Browser = boundBrowser{}

func (b boundBrowser) List(ctx context.Context) ([]target.Info, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.List(ctx)
}

func (b boundBrowser) Open(ctx context.Context, url string) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Open(ctx, url)
}

func (b boundBrowser) Navigate(ctx context.Context, targetID string, url string) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Navigate(ctx, targetID, url)
}

func (b boundBrowser) CloseTabs(ctx context.Context, targetIDs []string) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.CloseTabs(ctx, targetIDs)
}

func (b boundBrowser) Activate(ctx context.Context, targetID string) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Activate(ctx, targetID)
}

func (b boundBrowser) History(ctx context.Context, targetID string, delta int) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.History(ctx, targetID, delta)
}

func (b boundBrowser) Reload(ctx context.Context, targetID string, hard bool) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Reload(ctx, targetID, hard)
}

func (b boundBrowser) Eval(ctx context.Context, targetID string, expr string, opts chrome.EvalOpts) (any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Eval(ctx, targetID, expr, opts)
}

func (b boundBrowser) Snapshot(ctx context.Context, targetID string, opts chrome.SnapOpts) (any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Snapshot(ctx, targetID, opts)
}

func (b boundBrowser) Find(ctx context.Context, targetID string, query string, opts chrome.FindOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Find(ctx, targetID, query, opts)
}

func (b boundBrowser) Key(ctx context.Context, targetID string, selector string, keys []chrome.KeyStroke, opts chrome.KeyOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Key(ctx, targetID, selector, keys, opts)
}

func (b boundBrowser) Pointer(ctx context.Context, targetID string, selector string, opts chrome.PointerOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Pointer(ctx, targetID, selector, opts)
}

func (b boundBrowser) Select(ctx context.Context, targetID string, field string, option string, opts chrome.SelectOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Select(ctx, targetID, field, option, opts)
}

func (b boundBrowser) Grid(ctx context.Context, targetID string, selector string, q chrome.QueryOpts) (any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Grid(ctx, targetID, selector, q)
}

func (b boundBrowser) Scroll(ctx context.Context, targetID string, selector string, opts chrome.ScrollOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Scroll(ctx, targetID, selector, opts)
}

func (b boundBrowser) Type(ctx context.Context, targetID string, selector string, text string, q chrome.QueryOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Type(ctx, targetID, selector, text, q)
}

func (b boundBrowser) Fill(ctx context.Context, targetID string, selector string, value string, q chrome.QueryOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Fill(ctx, targetID, selector, value, q)
}

func (b boundBrowser) Upload(ctx context.Context, targetID string, selector string, paths []string, opts chrome.UploadOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Upload(ctx, targetID, selector, paths, opts)
}

func (b boundBrowser) HTML(ctx context.Context, targetID string, selector string, inner bool, q chrome.QueryOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.HTML(ctx, targetID, selector, inner, q)
}

func (b boundBrowser) Text(ctx context.Context, targetID string, selector string, opts chrome.TextOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Text(ctx, targetID, selector, opts)
}

func (b boundBrowser) Value(ctx context.Context, targetID string, selector string, q chrome.QueryOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Value(ctx, targetID, selector, q)
}

func (b boundBrowser) Values(ctx context.Context, targetID string, selector string, q chrome.QueryOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Values(ctx, targetID, selector, q)
}

func (b boundBrowser) AttrGet(ctx context.Context, targetID string, selector string, name string, q chrome.QueryOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.AttrGet(ctx, targetID, selector, name, q)
}

func (b boundBrowser) AttrList(ctx context.Context, targetID string, selector string, q chrome.QueryOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.AttrList(ctx, targetID, selector, q)
}

func (b boundBrowser) AttrSet(ctx context.Context, targetID string, selector string, name string, value string, q chrome.QueryOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.AttrSet(ctx, targetID, selector, name, value, q)
}

func (b boundBrowser) AttrRemove(ctx context.Context, targetID string, selector string, name string, q chrome.QueryOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.AttrRemove(ctx, targetID, selector, name, q)
}

func (b boundBrowser) SetHeaders(ctx context.Context, targetID string, headers map[string]string) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.SetHeaders(ctx, targetID, headers)
}

func (b boundBrowser) EmulateViewport(ctx context.Context, targetID string, width int64, height int64) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.EmulateViewport(ctx, targetID, width, height)
}

func (b boundBrowser) EmulateGeo(ctx context.Context, targetID string, lat float64, lon float64) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.EmulateGeo(ctx, targetID, lat, lon)
}

func (b boundBrowser) EmulateReset(ctx context.Context, targetID string) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.EmulateReset(ctx, targetID)
}

func (b boundBrowser) Frames(ctx context.Context, targetID string) (any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Frames(ctx, targetID)
}

func (b boundBrowser) Wait(ctx context.Context, targetID string, cond chrome.WaitCond) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Wait(ctx, targetID, cond)
}

func (b boundBrowser) Screenshot(ctx context.Context, targetID string, opts chrome.ShotOpts) ([]byte, map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Screenshot(ctx, targetID, opts)
}

func (b boundBrowser) PDF(ctx context.Context, targetID string, opts chrome.PDFOpts) ([]byte, map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.PDF(ctx, targetID, opts)
}

func (b boundBrowser) Console(ctx context.Context, targetID string, opts chrome.ConsoleOpts) (any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Console(ctx, targetID, opts)
}

func (b boundBrowser) ConsoleStream(ctx context.Context, targetID string, opts chrome.ConsoleOpts, emit func(any) error) error {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.ConsoleStream(ctx, targetID, opts, emit)
}

func (b boundBrowser) Net(ctx context.Context, targetID string, opts chrome.NetOpts) (any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Net(ctx, targetID, opts)
}

func (b boundBrowser) NetStream(ctx context.Context, targetID string, opts chrome.NetOpts, emit func(any) error) error {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.NetStream(ctx, targetID, opts, emit)
}

func (b boundBrowser) NetWait(ctx context.Context, targetID string, cond chrome.NetCond) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.NetWait(ctx, targetID, cond)
}

func (b boundBrowser) CookieList(ctx context.Context, targetID string) (any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.CookieList(ctx, targetID)
}

func (b boundBrowser) CookieSet(ctx context.Context, targetID string, name string, value string, domain string, path string) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.CookieSet(ctx, targetID, name, value, domain, path)
}

func (b boundBrowser) CookieDelete(ctx context.Context, targetID string, name string) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.CookieDelete(ctx, targetID, name)
}

func (b boundBrowser) CookieClear(ctx context.Context, targetID string) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.CookieClear(ctx, targetID)
}

func (b boundBrowser) Raw(ctx context.Context, targetID string, method string, params json.RawMessage) (any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.Raw(ctx, targetID, method, params)
}

func (b boundBrowser) RecordStart(ctx context.Context, targetID string, opts chrome.RecordOpts) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.RecordStart(ctx, targetID, opts)
}

func (b boundBrowser) RecordStop(ctx context.Context, targetID string) ([]chrome.Frame, map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.RecordStop(ctx, targetID)
}

func (b boundBrowser) RecordStatus(ctx context.Context, targetID string) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.RecordStatus(ctx, targetID)
}

func (b boundBrowser) RecordCancel(ctx context.Context, targetID string) (map[string]any, error) {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.RecordCancel(ctx, targetID)
}

func (b boundBrowser) RecordRestore(ctx context.Context, targetID string, frames []chrome.Frame, meta map[string]any) error {
	ctx, cancel := b.bind(ctx)
	defer cancel()
	return b.Browser.RecordRestore(ctx, targetID, frames, meta)
}
