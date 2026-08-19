// Package chrometest provides a shared chrome.Browser test double. Both the cli
// and daemon test suites embed StubBrowser and override only the methods they
// assert on, so a new Browser method needs a default in exactly one place.
package chrometest

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// StubBrowser implements chrome.Browser with permissive defaults so tests
// override only what they assert on; a new interface method gets a default here.
type StubBrowser struct{}

func (StubBrowser) List(context.Context) ([]target.Info, error) { return nil, nil }
func (StubBrowser) Open(_ context.Context, url string) (map[string]any, error) {
	return map[string]any{"id": "newtab", "url": url}, nil
}
func (StubBrowser) Navigate(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"url": "https://example.com/", "status": 200}, nil
}
func (StubBrowser) Eval(context.Context, string, string, chrome.EvalOpts) (any, error) {
	return map[string]any{"value": 2}, nil
}
func (StubBrowser) Snapshot(context.Context, string, chrome.SnapOpts) (any, error) {
	return map[string]any{}, nil
}
func (StubBrowser) Find(_ context.Context, _, query string, _ chrome.FindOpts) (map[string]any, error) {
	return map[string]any{"query": query, "matches": []any{}, "count": 0, "truncated": false}, nil
}
func (StubBrowser) CloseTabs(_ context.Context, ids []string) (map[string]any, error) {
	return map[string]any{"closed": []any{}, "count": len(ids)}, nil
}
func (StubBrowser) Activate(context.Context, string) (map[string]any, error) {
	return map[string]any{"activated": true, "was_active": false}, nil
}
func (StubBrowser) History(context.Context, string, int) (map[string]any, error) {
	// No `status`: a history move has no HTTP response of its own, so the real
	// driver reports none either.
	return map[string]any{"url": "https://example.com/"}, nil
}
func (StubBrowser) Reload(context.Context, string, bool) (map[string]any, error) {
	return map[string]any{"url": "https://example.com/", "status": 200}, nil
}
func (StubBrowser) Key(_ context.Context, _, _ string, keys []chrome.KeyStroke, opts chrome.KeyOpts) (map[string]any, error) {
	return map[string]any{"keys": chrome.KeyNames(keys), "repeat": max(opts.Repeat, 1)}, nil
}
func (StubBrowser) Pointer(_ context.Context, _ string, _ string, opts chrome.PointerOpts) (map[string]any, error) {
	return map[string]any{"action": string(opts.Action), "x": 0.0, "y": 0.0}, nil
}
func (StubBrowser) Type(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"typed": true}, nil
}
func (StubBrowser) Select(context.Context, string, string, string, chrome.SelectOpts) (map[string]any, error) {
	return map[string]any{"selected": true}, nil
}
func (StubBrowser) Fill(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"filled": true}, nil
}
func (StubBrowser) Upload(context.Context, string, string, []string, chrome.UploadOpts) (map[string]any, error) {
	return map[string]any{"files": []any{}, "count": 0, "change_fired": true}, nil
}
func (StubBrowser) Values(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"values": []any{}, "count": 0}, nil
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
func (StubBrowser) Text(context.Context, string, string, chrome.TextOpts) (map[string]any, error) {
	return map[string]any{"text": "hello"}, nil
}
func (StubBrowser) Value(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{"value": "v"}, nil
}
func (StubBrowser) Screenshot(context.Context, string, chrome.ShotOpts) ([]byte, map[string]any, error) {
	return []byte("PNGDATA"), nil, nil
}
func (StubBrowser) PDF(context.Context, string, chrome.PDFOpts) ([]byte, map[string]any, error) {
	return []byte("%PDF-"), nil, nil
}
func (StubBrowser) RecordStart(context.Context, string, chrome.RecordOpts) (map[string]any, error) {
	return map[string]any{"action": "start", "recording": true, "fps": 4.0, "scale": 0.5}, nil
}

// RecordStop hands back two REAL frames, small but decodable.
//
// A stub that returned empty bytes would make every CLI-level export test pass
// through a code path that never encodes anything — and encoding is most of
// what `record stop` does. Two frames also make the result an animation rather
// than a still, which is what the command promises.
func (StubBrowser) RecordStop(context.Context, string) ([]chrome.Frame, map[string]any, error) {
	now := time.Now()
	return []chrome.Frame{
			{Data: stubFrame, TS: now, Width: 4, Height: 4},
			{Data: stubFrame, TS: now.Add(250 * time.Millisecond), Width: 4, Height: 4},
		}, map[string]any{
			"action": "stop", "frames": 2, "dropped_frames": 0,
			"truncated": false, "elapsed_ms": int64(250), "fps": 4.0, "scale": 0.5,
		}, nil
}

// RecordRestore accepts a re-seated recording and forgets it: a stub has nowhere
// to hold one, and every test that cares about the retry overrides this.
func (StubBrowser) RecordRestore(context.Context, string, []chrome.Frame, map[string]any) error {
	return nil
}

func (StubBrowser) RecordStatus(context.Context, string) (map[string]any, error) {
	return map[string]any{"action": "status", "recording": false, "frames": 0}, nil
}

func (StubBrowser) RecordCancel(context.Context, string) (map[string]any, error) {
	return map[string]any{"action": "cancel", "recording": false, "discarded": 0}, nil
}

// stubFrame is a 4x4 PNG, built once so the frames the stub hands back decode
// like the real thing.
var stubFrame = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			img.Set(x, y, color.RGBA{R: 0x20, G: 0x80, B: 0xC0, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic("chrometest: cannot build the stub frame: " + err.Error())
	}
	return buf.Bytes()
}()

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
func (StubBrowser) StorageList(_ context.Context, _ string, scope string, _ chrome.StorageListOpts) (map[string]any, error) {
	return map[string]any{"scope": scope, "origin": "https://stub.test", "items": []map[string]any{}, "count": 0, "truncated": false}, nil
}
func (StubBrowser) StorageGet(_ context.Context, _ string, scope, key string) (map[string]any, error) {
	return map[string]any{"scope": scope, "origin": "https://stub.test", "key": key, "value": "stub", "present": true}, nil
}
func (StubBrowser) StorageSet(_ context.Context, _ string, scope, key, _ string) (map[string]any, error) {
	return map[string]any{"scope": scope, "origin": "https://stub.test", "key": key, "set": true}, nil
}
func (StubBrowser) StorageRemove(_ context.Context, _ string, scope, key string) (map[string]any, error) {
	return map[string]any{"scope": scope, "origin": "https://stub.test", "key": key, "removed": true}, nil
}
func (StubBrowser) StorageClear(_ context.Context, _ string, scope string) (map[string]any, error) {
	return map[string]any{"scope": scope, "origin": "https://stub.test", "cleared": true}, nil
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
func (StubBrowser) Window(_ context.Context, _ string, opts chrome.WindowOpts) (chrome.WindowBounds, error) {
	b := chrome.WindowBounds{Left: 0, Top: 0, Width: 1280, Height: 800, State: "normal"}
	if opts.Width > 0 && opts.Height > 0 {
		b.Width, b.Height = opts.Width, opts.Height
	}
	return b, nil
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
func (StubBrowser) Console(context.Context, string, chrome.ConsoleOpts) (any, error) {
	return map[string]any{"messages": []any{}, "count": 0, "buffered": 0, "dropped": 0, "truncated": false}, nil
}
func (StubBrowser) ConsoleStream(context.Context, string, chrome.ConsoleOpts, func(any) error) error {
	return nil
}
func (StubBrowser) Net(context.Context, string, chrome.NetOpts) (any, error) {
	return map[string]any{"requests": []any{}, "count": 0, "buffered": 0, "dropped": 0, "truncated": false, "pending": 0}, nil
}
func (StubBrowser) NetStream(context.Context, string, chrome.NetOpts, func(any) error) error {
	return nil
}
func (StubBrowser) NetWait(context.Context, string, chrome.NetCond) (map[string]any, error) {
	return map[string]any{"matched": false}, nil
}

// DialogStatus and DialogHandle default to a permissive success shape, like
// every other StubBrowser method: a stub whose default is a FAILURE shape
// would make every test that does not care about dialogs look like the "none"
// case, and that case is an error (ErrNoDialog) a test overrides to produce
// (RFC-0018).
func (StubBrowser) DialogStatus(context.Context, string) (map[string]any, error) {
	return map[string]any{"open": false}, nil
}
func (StubBrowser) DialogHandle(_ context.Context, _ string, accept bool, _ string) (map[string]any, error) {
	action := "dismiss"
	if accept {
		action = "accept"
	}
	return map[string]any{"handled": true, "action": action, "type": "confirm", "message": "stub"}, nil
}
func (StubBrowser) Raw(context.Context, string, string, json.RawMessage) (any, error) {
	return map[string]any{}, nil
}
func (StubBrowser) Close() error { return nil }

var _ chrome.Browser = StubBrowser{}
