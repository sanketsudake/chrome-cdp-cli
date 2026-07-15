package daemon

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// fakeBrowser is a minimal chrome.Browser for exercising the RPC transport.
type fakeBrowser struct{}

func (fakeBrowser) List(context.Context) ([]target.Info, error) {
	return []target.Info{{ID: "aa11", Title: "A", URL: "u"}}, nil
}
func (fakeBrowser) Navigate(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"url": "x"}, nil
}
func (fakeBrowser) Eval(context.Context, string, string) (any, error) {
	return map[string]any{"value": 42}, nil
}
func (fakeBrowser) Snapshot(context.Context, string) (any, error) { return map[string]any{}, nil }
func (fakeBrowser) Click(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) Type(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) HTML(context.Context, string, string, bool, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) Text(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) Value(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) AttrGet(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) AttrList(context.Context, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) AttrSet(context.Context, string, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) AttrRemove(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) SetHeaders(context.Context, string, map[string]string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) EmulateViewport(context.Context, string, int64, int64) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) EmulateGeo(context.Context, string, float64, float64) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) EmulateReset(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) Frames(context.Context, string) (any, error) { return map[string]any{}, nil }
func (fakeBrowser) Screenshot(context.Context, string) ([]byte, error) {
	return []byte("PNG"), nil
}
func (fakeBrowser) PDF(context.Context, string) ([]byte, error)     { return []byte("PDF"), nil }
func (fakeBrowser) CookieList(context.Context, string) (any, error) { return map[string]any{}, nil }
func (fakeBrowser) CookieClear(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) CookieSet(context.Context, string, string, string, string, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) CookieDelete(context.Context, string, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeBrowser) Raw(context.Context, string, string, json.RawMessage) (any, error) {
	return map[string]any{"ok": true}, nil
}
func (fakeBrowser) Close() error { return nil }

func serveTemp(t *testing.T) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go Serve(ln, fakeBrowser{}, time.Minute)
	t.Cleanup(func() { _ = ln.Close() })
	return &Client{path: sock}
}

func TestRPCRoundTrip(t *testing.T) {
	rb := Remote(serveTemp(t))

	tabs, err := rb.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tabs) != 1 || tabs[0].ID != "aa11" {
		t.Errorf("List = %v", tabs)
	}

	v, err := rb.Eval(context.Background(), "aa11", "1+1")
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok || m["value"].(float64) != 42 {
		t.Errorf("Eval = %v", v)
	}

	// []byte results survive the round-trip (base64 in JSON).
	png, err := rb.Screenshot(context.Background(), "aa11")
	if err != nil || string(png) != "PNG" {
		t.Errorf("Screenshot = %q, %v", png, err)
	}
}

func TestEnsureConnectsToExisting(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go Serve(ln, fakeBrowser{}, time.Minute)
	t.Cleanup(func() { _ = ln.Close() })

	// A daemon is already listening, so Ensure connects without spawning (the
	// exe path is never used).
	c, err := Ensure(sock, "/nonexistent-exe", nil)
	if err != nil {
		t.Fatalf("Ensure should connect to the running daemon: %v", err)
	}
	if err := c.Status(); err != nil {
		t.Errorf("Status via Ensure client: %v", err)
	}
}

func TestStatusAndStop(t *testing.T) {
	c := serveTemp(t)
	if err := c.Status(); err != nil {
		t.Fatalf("Status on live daemon: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if c.Status() == nil {
		t.Error("expected Status to fail after Stop closed the listener")
	}
}
