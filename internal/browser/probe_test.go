package browser

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The consent-pending state is reproducible without a browser: it is a TCP
// listener that accepts and then stalls. These helpers build the three endpoint
// shapes the probe has to tell apart. The manual reproduction of this bug wedged
// a real browser twice, so it must never be the regression test.

// stallListener accepts connections and never answers — Chrome holding a consent
// prompt. It counts accepted connections, so a test can prove nothing connected.
func stallListener(t *testing.T) (wsURL string, conns *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n atomic.Int32
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			held = append(held, c) // hold it open, saying nothing
		}
	}()
	return wsFor(ln), &n
}

// answerListener accepts and completes the WebSocket upgrade after delay — the
// user finding the dialog and clicking Allow. It records whether the connection
// was still open when the answer was written: that is what "no orphaned prompt"
// means in the failure this exists to prevent.
func answerListener(t *testing.T, delay time.Duration, status string) (wsURL string, answeredLive *atomic.Bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var live atomic.Bool
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				time.Sleep(delay)
				if _, err := c.Write([]byte(status + "\r\n\r\n")); err == nil {
					live.Store(true)
				}
				time.Sleep(50 * time.Millisecond)
			}(c)
		}
	}()
	return wsFor(ln), &live
}

// closedWS returns a ws:// URL for a port with nothing listening.
func closedWS(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := wsFor(ln)
	_ = ln.Close()
	return url
}

func wsFor(ln net.Listener) string {
	return fmt.Sprintf("ws://%s/devtools/browser/stub", ln.Addr().String())
}

// wsRoot is the ws:// root of an http:// endpoint — where the browser-level
// endpoint lives when /json/version cannot say.
func wsRoot(httpURL string) string {
	return "ws://" + strings.TrimPrefix(httpURL, "http://") + "/"
}

// TestAwaitUpgradeRefusedIsFast is the safety property behind the long consent
// wait: only an OPEN port earns it. A dead endpoint must fail in milliseconds,
// never after the consent timeout.
func TestAwaitUpgradeRefusedIsFast(t *testing.T) {
	t.Parallel()
	start := time.Now()
	u := AwaitUpgrade(closedWS(t), 2*time.Second, time.Second, 30*time.Second, nil)
	defer u.Close()
	if u.State != WSRefused {
		t.Errorf("closed port classified %v, want refused", u.State)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("a refused endpoint took %v — it must fail fast, not wait out the consent budget", el)
	}
}

// TestAwaitUpgradePendingIsBoundedAndAnnounced covers the consent signature:
// silence on an open port is reported while it is happening, and the wait ends.
func TestAwaitUpgradePendingIsBoundedAndAnnounced(t *testing.T) {
	t.Parallel()
	ws, conns := stallListener(t)
	var pendingAt time.Duration
	start := time.Now()
	u := AwaitUpgrade(ws, time.Second, 100*time.Millisecond, 600*time.Millisecond, func() {
		pendingAt = time.Since(start)
	})
	defer u.Close()
	elapsed := time.Since(start)

	if u.State != WSPending {
		t.Fatalf("a stalling endpoint classified %v, want pending", u.State)
	}
	if pendingAt == 0 {
		t.Error("onPending never fired — the user is told only after the wait, which is the bug")
	}
	if pendingAt > 400*time.Millisecond {
		t.Errorf("onPending fired after %v, want ~100ms (it must announce during the wait)", pendingAt)
	}
	if elapsed < 500*time.Millisecond {
		t.Errorf("gave up after %v, want the full ~600ms budget", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("the wait is unbounded (%v)", elapsed)
	}
	if got := conns.Load(); got != 1 {
		t.Errorf("probe opened %d connections, want exactly 1 — each one is a consent request", got)
	}
}

// TestAwaitUpgradeLateAnswerStillSucceeds is the orphaned-prompt regression: an
// answer that arrives long after the old ~10s dial timeout must still land on a
// live connection.
func TestAwaitUpgradeLateAnswerStillSucceeds(t *testing.T) {
	t.Parallel()
	ws, answeredLive := answerListener(t, 300*time.Millisecond, "HTTP/1.1 101 Switching Protocols")
	var announced bool
	u := AwaitUpgrade(ws, time.Second, 50*time.Millisecond, 5*time.Second, func() { announced = true })
	defer u.Close()

	if u.State != WSReady {
		t.Fatalf("a late-but-completed upgrade classified %v, want ready", u.State)
	}
	if !announced {
		t.Error("the pending state was never announced even though the answer took 6x the threshold")
	}
	if !answeredLive.Load() {
		t.Error("the endpoint answered into a closed socket — the prompt was orphaned")
	}
	if u.conn == nil {
		t.Error("a ready upgrade must keep its socket, so the granted consent is still held when the attach lands")
	}
}

// floodListener accepts and then streams bytes that never contain a newline —
// a hostile or broken local process on the debug port. Anything that can bind
// 127.0.0.1:9222 can be this.
func floodListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				chunk := bytes.Repeat([]byte("A"), 32<<10)
				for {
					if _, err := c.Write(chunk); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return wsFor(ln)
}

// TestAwaitUpgradeBoundsTheResponse: the status line is one line of HTTP, and
// the reader must be bounded like one.
//
// bufio.Reader.ReadString('\n') accumulates without limit and had no read
// deadline, so the only ceiling was the caller's total wait — 120s in the
// daemon. A listener streaming newline-free bytes drove 18 GB of heap in six
// seconds; the daemon's full budget reaches hundreds of gigabytes. Nothing here
// needs more than a status line, so nothing here should read more than one.
func TestAwaitUpgradeBoundsTheResponse(t *testing.T) {
	t.Parallel()
	start := time.Now()
	u := AwaitUpgrade(floodListener(t), time.Second, 30*time.Second, 30*time.Second, nil)
	defer u.Close()
	if u.State != WSRefused {
		t.Errorf("an endpoint that answers with garbage classified %v, want refused", u.State)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("the read ran for %v — a bounded read ends at the limit, not at the consent budget", el)
	}
}

// TestProbeWSClassifiesAllThree is doctor's view: three endpoints, three answers,
// and the ready one established by a completed upgrade rather than a port file.
func TestProbeWSClassifiesAllThree(t *testing.T) {
	t.Parallel()
	stalling, _ := stallListener(t)
	ready, _ := answerListener(t, 0, "HTTP/1.1 101 Switching Protocols")
	// An endpoint that ANSWERS with something other than 101 is a live server
	// that is not a CDP browser (a stale port file reused by another process).
	wrong, _ := answerListener(t, 0, "HTTP/1.1 404 Not Found")

	for _, c := range []struct {
		name string
		ws   string
		want WSState
	}{
		{"nothing listening", closedWS(t), WSRefused},
		{"accepts and stalls", stalling, WSPending},
		{"completes the upgrade", ready, WSReady},
		{"answers 404", wrong, WSRefused},
		{"not a ws url", "::::", WSRefused},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ProbeWS(c.ws, time.Second, 400*time.Millisecond); got != c.want {
				t.Errorf("ProbeWS = %v, want %v", got, c.want)
			}
		})
	}
}

// TestResolveWSURL covers the endpoint shapes the two connection paths produce.
// An explicit --port names an http:// endpoint, and without this resolution the
// upgrade probe would handshake against "/" and classify a perfectly healthy
// Chrome as refused.
func TestResolveWSURL(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"Browser":"Chrome/1","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/abc"}`)
	}))
	// t.Cleanup, not defer: the parallel subtests below run after this function
	// returns, so a deferred Close would shut the server before they use it.
	t.Cleanup(srv.Close)

	// A 404 on /json/version is exactly what the chrome://inspect path returns,
	// consent or no consent. It locates nothing, so it resolves to nothing — and
	// it is never treated as a consent signal.
	notFound := httptest.NewServer(http.HandlerFunc(http.NotFound))
	t.Cleanup(notFound.Close)

	for _, c := range []struct {
		name     string
		endpoint string
		want     string
		wantOK   bool
	}{
		{"a ws url passes through", "ws://127.0.0.1:9222/devtools/browser/x", "ws://127.0.0.1:9222/devtools/browser/x", true},
		{"an http endpoint resolves via /json/version", srv.URL, "ws://127.0.0.1:9222/devtools/browser/abc", true},
		// The chrome://inspect toggle path 404s /json/version whether or not
		// consent has been granted, so a 404 must NOT end the resolution: it
		// leaves the browser endpoint at the root of the same host:port, and
		// probing that is the only way the pending state is visible on the one
		// path that actually prompts.
		{"a 404 falls back to the ws root", notFound.URL, wsRoot(notFound.URL), true},
		{"nothing listening still falls back", "http://127.0.0.1:1", "ws://127.0.0.1:1/", true},
		{"empty", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ResolveWSURL(c.endpoint, 2*time.Second)
			if ok != c.wantOK || got != c.want {
				t.Errorf("ResolveWSURL(%q) = %q,%v; want %q,%v", c.endpoint, got, ok, c.want, c.wantOK)
			}
		})
	}
}
