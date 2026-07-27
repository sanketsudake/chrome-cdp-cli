package chrome

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/probetest"
)

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
	u := AwaitUpgrade(probetest.Closed(t).WS(), UpgradeTimings{PendingAfter: time.Second, Total: 30 * time.Second}, nil)
	defer u.Close()
	if u.State != browser.WSRefused {
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
	ep := probetest.Stall(t)
	var pendingAt time.Duration
	start := time.Now()
	u := AwaitUpgrade(ep.WS(), UpgradeTimings{PendingAfter: 100 * time.Millisecond, Total: 600 * time.Millisecond}, func() {
		pendingAt = time.Since(start)
	})
	defer u.Close()
	elapsed := time.Since(start)

	if u.State != browser.WSPending {
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
	if got := ep.Conns(); got != 1 {
		t.Errorf("probe opened %d connections, want exactly 1 — each one is a consent request", got)
	}
}

// TestAwaitUpgradeLateAnswerStillSucceeds is the orphaned-prompt regression: an
// answer that arrives long after the old ~10s dial timeout must still land on a
// live connection.
func TestAwaitUpgradeLateAnswerStillSucceeds(t *testing.T) {
	t.Parallel()
	ep := probetest.Answer(t, 300*time.Millisecond, "HTTP/1.1 101 Switching Protocols")
	var announced bool
	u := AwaitUpgrade(ep.WS(), UpgradeTimings{PendingAfter: 50 * time.Millisecond, Total: 5 * time.Second}, func() { announced = true })
	defer u.Close()

	if u.State != browser.WSReady {
		t.Fatalf("a late-but-completed upgrade classified %v, want ready", u.State)
	}
	if !announced {
		t.Error("the pending state was never announced even though the answer took 6x the threshold")
	}
	if !ep.AnsweredLive() {
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
	return fmt.Sprintf("ws://%s/devtools/browser/stub", ln.Addr().String())
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
	u := AwaitUpgrade(floodListener(t), UpgradeTimings{PendingAfter: 30 * time.Second, Total: 30 * time.Second}, nil)
	defer u.Close()
	if u.State != browser.WSRefused {
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
	stalling := probetest.Stall(t)
	ready := probetest.Answer(t, 0, "HTTP/1.1 101 Switching Protocols")
	// An endpoint that ANSWERS with something other than 101 is a live server
	// that is not a CDP browser (a stale port file reused by another process).
	wrong := probetest.Answer(t, 0, "HTTP/1.1 404 Not Found")

	for _, c := range []struct {
		name string
		ws   string
		want browser.WSState
	}{
		{"nothing listening", probetest.Closed(t).WS(), browser.WSRefused},
		{"accepts and stalls", stalling.WS(), browser.WSPending},
		{"completes the upgrade", ready.WS(), browser.WSReady},
		{"answers 404", wrong.WS(), browser.WSRefused},
		{"not a ws url", "::::", browser.WSRefused},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ProbeWS(c.ws, 400*time.Millisecond); got != c.want {
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
			got, ok := ResolveWSURL(c.endpoint)
			if ok != c.wantOK || got != c.want {
				t.Errorf("ResolveWSURL(%q) = %q,%v; want %q,%v", c.endpoint, got, ok, c.want, c.wantOK)
			}
		})
	}
}

// TestAwaitUpgradeAnswerDuringOnPendingIsNotDiscarded.
//
// With PendingAfter >= Total — which is every doctor probe, since ProbeWS
// passes the same value for both — the old code computed a remainder of <= 0
// and returned browser.WSPending WITHOUT ever selecting on the answer channel again.
// Anything delivered while onPending was running was therefore thrown away and
// its socket closed, and onPending is not instantaneous: the daemon's writes a
// file. So an endpoint that had completed the handshake was reported as
// holding a consent prompt.
func TestAwaitUpgradeAnswerDuringOnPendingIsNotDiscarded(t *testing.T) {
	t.Parallel()
	const budget = 50 * time.Millisecond
	// The answer lands after the budget is up but WHILE onPending is still
	// running, so it is sitting in the channel when the wait ends.
	ep := probetest.Answer(t, budget+10*time.Millisecond, "HTTP/1.1 101 Switching Protocols")

	u := AwaitUpgrade(ep.WS(), UpgradeTimings{PendingAfter: budget, Total: budget}, func() {
		time.Sleep(30 * time.Millisecond)
	})
	defer u.Close()

	if u.State != browser.WSReady {
		t.Errorf("a completed handshake classified %v: the answer arrived while onPending ran and was discarded unread", u.State)
	}
}
