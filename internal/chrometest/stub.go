// Package chrometest provides a shared chrome.Browser test double. Both the cli
// and daemon test suites embed StubBrowser and override only the methods they
// assert on, so a new Browser method needs a default in exactly one place.
package chrometest

import (
	"context"
	"encoding/json"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// StubBrowser implements chrome.Browser with permissive defaults so tests
// override only what they assert on; a new interface method gets a default here.
type StubBrowser struct{}

func (StubBrowser) List(context.Context) ([]target.Info, error) { return nil, nil }
func (StubBrowser) Navigate(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"url": "https://example.com/", "status": 200}, nil
}
func (StubBrowser) Eval(context.Context, string, string) (any, error) {
	return map[string]any{"value": 2}, nil
}
func (StubBrowser) Snapshot(context.Context, string) (any, error) { return map[string]any{}, nil }
func (StubBrowser) Click(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"clicked": true}, nil
}
func (StubBrowser) Type(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"typed": true}, nil
}
func (StubBrowser) Select(context.Context, string, string, string, chrome.SelectOpts) (map[string]any, error) {
	return map[string]any{"selected": true}, nil
}
func (StubBrowser) Grid(context.Context, string, string, chrome.QueryOpts) (any, error) {
	return map[string]any{"headers": []any{}, "rows": []any{}, "count": 0}, nil
}
func (StubBrowser) Scroll(context.Context, string, string, chrome.ScrollOpts) (map[string]any, error) {
	return map[string]any{"scrolled": "ok"}, nil
}
func (StubBrowser) HTML(context.Context, string, string, bool, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"html": "<div></div>"}, nil
}
func (StubBrowser) Text(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"text": "hello"}, nil
}
func (StubBrowser) Value(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"value": "v"}, nil
}
func (StubBrowser) Screenshot(context.Context, string) ([]byte, error) { return []byte("PNGDATA"), nil }
func (StubBrowser) PDF(context.Context, string) ([]byte, error)        { return []byte("%PDF-"), nil }
func (StubBrowser) CookieList(context.Context, string) (any, error) {
	return map[string]any{"cookies": []any{}}, nil
}
func (StubBrowser) CookieSet(context.Context, string, string, string, string, string) (map[string]any, error) {
	return map[string]any{"set": "x"}, nil
}
func (StubBrowser) CookieDelete(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"deleted": "x"}, nil
}
func (StubBrowser) CookieClear(context.Context, string) (map[string]any, error) {
	return map[string]any{"cleared": true}, nil
}
func (StubBrowser) AttrGet(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"name": "n", "value": "v", "present": true}, nil
}
func (StubBrowser) AttrList(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"attributes": map[string]any{}}, nil
}
func (StubBrowser) AttrSet(context.Context, string, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"set": "n"}, nil
}
func (StubBrowser) AttrRemove(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"removed": "n"}, nil
}
func (StubBrowser) SetHeaders(context.Context, string, map[string]string) (map[string]any, error) {
	return map[string]any{"headers": 1}, nil
}
func (StubBrowser) EmulateViewport(context.Context, string, int64, int64) (map[string]any, error) {
	return map[string]any{"width": 100, "height": 100}, nil
}
func (StubBrowser) EmulateGeo(context.Context, string, float64, float64) (map[string]any, error) {
	return map[string]any{"lat": 1.0, "lon": 2.0}, nil
}
func (StubBrowser) EmulateReset(context.Context, string) (map[string]any, error) {
	return map[string]any{"reset": true}, nil
}
func (StubBrowser) Frames(context.Context, string) (any, error) {
	return map[string]any{"frames": []any{}}, nil
}
func (StubBrowser) Wait(context.Context, string, chrome.WaitCond) (map[string]any, error) {
	return map[string]any{"waited": "ok"}, nil
}
func (StubBrowser) Raw(context.Context, string, string, json.RawMessage) (any, error) {
	return map[string]any{}, nil
}
func (StubBrowser) Close() error { return nil }

var _ chrome.Browser = StubBrowser{}
