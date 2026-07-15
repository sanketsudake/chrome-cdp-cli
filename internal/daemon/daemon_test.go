package daemon

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrometest"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// fakeBrowser exercises the RPC transport; it embeds the shared stub and
// overrides only the methods the round-trip tests assert on.
type fakeBrowser struct{ chrometest.StubBrowser }

func (fakeBrowser) List(context.Context) ([]target.Info, error) {
	return []target.Info{{ID: "aa11", Title: "A", URL: "u"}}, nil
}
func (fakeBrowser) Eval(context.Context, string, string) (any, error) {
	return map[string]any{"value": 42}, nil
}
func (fakeBrowser) Screenshot(context.Context, string) ([]byte, error) {
	return []byte("PNG"), nil
}

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

// slowBrowser's Eval blocks until its context is cancelled — standing in for a
// wedged/slow CDP action.
type slowBrowser struct{ chrometest.StubBrowser }

func (slowBrowser) Eval(ctx context.Context, _, _ string) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestDaemonHonorsClientTimeout(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go Serve(ln, slowBrowser{}, time.Minute)
	t.Cleanup(func() { _ = ln.Close() })
	rb := Remote(&Client{path: sock})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = rb.Eval(ctx, "aa11", "1+1")
	if err == nil {
		t.Fatal("expected a deadline error — the daemon ignored the client timeout or the client hung")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("call took %v; the client blocked past the deadline instead of failing fast", d)
	}
}

// gateBrowser signals when its Eval has entered (holding the dispatch mutex) and
// then blocks, so a concurrent __stop can be tested against a busy daemon.
type gateBrowser struct {
	chrometest.StubBrowser
	entered chan struct{}
	release chan struct{}
}

func (g *gateBrowser) Eval(context.Context, string, string) (any, error) {
	close(g.entered)
	<-g.release
	return nil, nil
}

func TestStopRespondsWhileBusy(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	g := &gateBrowser{entered: make(chan struct{}), release: make(chan struct{})}
	go Serve(ln, g, time.Minute)
	t.Cleanup(func() { close(g.release); _ = ln.Close() })

	// Occupy the dispatch mutex with a long-running Eval.
	go func() { _, _ = Remote(&Client{path: sock}).Eval(context.Background(), "x", "y") }()
	<-g.entered // Eval now holds the mutex

	start := time.Now()
	if err := (&Client{path: sock}).Stop(); err != nil {
		t.Fatalf("Stop while busy: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Stop took %v; it blocked behind the busy action instead of responding", d)
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
