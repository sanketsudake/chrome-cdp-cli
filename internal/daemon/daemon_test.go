package daemon

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrometest"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// fakeBrowser exercises the RPC transport; it embeds the shared stub and
// overrides only the methods the round-trip tests assert on.
type fakeBrowser struct{ chrometest.StubBrowser }

func (fakeBrowser) List(context.Context) ([]target.Info, error) {
	return []target.Info{{ID: "aa11", Title: "A", URL: "u"}}, nil
}
func (fakeBrowser) Eval(context.Context, string, string, chrome.EvalOpts) (any, error) {
	return map[string]any{"value": 42}, nil
}
func (fakeBrowser) Screenshot(_ context.Context, _ string, opts chrome.ShotOpts) ([]byte, map[string]any, error) {
	return []byte("PNG"), map[string]any{"mode": string(chrome.ShotElement), "selector": opts.Selector}, nil
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

	v, err := rb.Eval(context.Background(), "aa11", "1+1", chrome.EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok || m["value"].(float64) != 42 {
		t.Errorf("Eval = %v", v)
	}

	// A capture's bytes AND its metadata survive the round-trip (the bytes as
	// base64), and the options reach the far side — the daemon is the default
	// connection path, so a dropped field here is invisible to every stub test
	// and broken for every real user.
	png, meta, err := rb.Screenshot(context.Background(), "aa11", chrome.ShotOpts{Selector: "#box"})
	if err != nil || string(png) != "PNG" {
		t.Errorf("Screenshot = %q, %v", png, err)
	}
	if meta["mode"] != "element" || meta["selector"] != "#box" {
		t.Errorf("Screenshot meta = %v, want the element mode and the forwarded selector", meta)
	}
}

// TestRecordRPCRoundTrip is the daemon half of RFC-0011.
//
// A recording lives in the daemon, so every byte of it and every field of its
// accounting crosses this socket. The frames ride as base64 inside an array of
// objects, which is exactly the kind of thing that marshals fine and arrives
// empty — and no stub-backed CLI test would ever notice, because they all
// inject a Browser directly.
func TestRecordRPCRoundTrip(t *testing.T) {
	rb := Remote(serveTemp(t))
	ctx := t.Context()

	if _, err := rb.RecordStart(ctx, "aa11", chrome.RecordOpts{FPS: 8, Scale: 0.25, Annotate: true}); err != nil {
		t.Fatalf("RecordStart: %v", err)
	}
	st, err := rb.RecordStatus(ctx, "aa11")
	if err != nil {
		t.Fatalf("RecordStatus: %v", err)
	}
	if _, ok := st["recording"]; !ok {
		t.Errorf("RecordStatus = %v, want a recording field", st)
	}

	frames, meta, err := rb.RecordStop(ctx, "aa11")
	if err != nil {
		t.Fatalf("RecordStop: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames over the RPC, want the stub's 2", len(frames))
	}
	for i, f := range frames {
		if len(f.Data) == 0 {
			t.Errorf("frame %d arrived with no bytes — the base64 round-trip is broken", i)
		}
		if f.TS.IsZero() {
			t.Errorf("frame %d arrived with no timestamp; the frame delays depend on it", i)
		}
	}
	if meta["frames"] != float64(2) {
		t.Errorf("meta = %v, want the accounting to survive the RPC", meta)
	}
	if _, err := rb.RecordCancel(ctx, "aa11"); err != nil {
		t.Errorf("RecordCancel: %v", err)
	}
}

// TestRecordOptsCrossTheRPC guards the arg decoder for a new option struct: a
// missing field here would silently record at the wrong scale for every real
// user, since the daemon is the default connection path.
func TestRecordOptsCrossTheRPC(t *testing.T) {
	got := make(chan chrome.RecordOpts, 1)
	sock := filepath.Join(t.TempDir(), "r.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go Serve(ln, recordOptsBrowser{seen: got}, time.Minute)
	t.Cleanup(func() { _ = ln.Close() })

	want := chrome.RecordOpts{FPS: 12, Scale: 0.25, Quality: 70, MaxFrames: 33, MaxDuration: 90 * time.Second, Annotate: true}
	if _, err := Remote(&Client{path: sock}).RecordStart(t.Context(), "aa11", want); err != nil {
		t.Fatalf("RecordStart: %v", err)
	}
	if seen := <-got; seen != want {
		t.Errorf("the daemon saw %+v, want %+v", seen, want)
	}
}

type recordOptsBrowser struct {
	chrometest.StubBrowser
	seen chan chrome.RecordOpts
}

func (b recordOptsBrowser) RecordStart(_ context.Context, _ string, opts chrome.RecordOpts) (map[string]any, error) {
	b.seen <- opts
	return map[string]any{"recording": true}, nil
}

// slowBrowser's Eval blocks until its context is cancelled — standing in for a
// wedged/slow CDP action.
type slowBrowser struct{ chrometest.StubBrowser }

func (slowBrowser) Eval(ctx context.Context, _, _ string, _ chrome.EvalOpts) (any, error) {
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
	_, err = rb.Eval(ctx, "aa11", "1+1", chrome.EvalOpts{})
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

func (g *gateBrowser) Eval(context.Context, string, string, chrome.EvalOpts) (any, error) {
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
	go func() { _, _ = Remote(&Client{path: sock}).Eval(context.Background(), "x", "y", chrome.EvalOpts{}) }()
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
	c, err := Ensure(sock, "/nonexistent-exe", nil, time.Minute)
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
