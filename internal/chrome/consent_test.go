package chrome

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/probetest"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// RFC-0013. The consent-pending state is a TCP listener that accepts and then
// stalls, so every scenario here runs against net.Listen and no browser at all.
// That is not a convenience: reproducing this by hand wedged a real user's
// Chrome twice, and a regression test that needs a human to click a modal is not
// a test.

// pinChromeRunning fixes the pgrep answer: whether the machine running the test
// happens to have Chrome open must not decide which rung of the ladder we land on.
func pinChromeRunning(t *testing.T, running bool) {
	t.Helper()
	prev := chromeRunning
	chromeRunning = func() bool { return running }
	t.Cleanup(func() { chromeRunning = prev })
}

func connectErrCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("Connect succeeded against a stub listener, want an error")
	}
	var ce *ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *ConnectError, so its code never reaches the envelope", err)
	}
	return ce.Code
}

// TestConnectConsentPendingWaitsAndReports is VS-1 and VS-4.
//
// The old behaviour: the dial timed out in ~10s, the daemon wrote its error and
// exited, and the modal it had raised was left on screen with nothing behind it.
// Clicking Allow then granted consent to a connection that no longer existed.
func TestConnectConsentPendingWaitsAndReports(t *testing.T) {
	ep := probetest.Stall(t)
	pinChromeRunning(t, true) // even so: a hanging upgrade is not "enable the toggle"

	var pendingAt time.Duration
	start := time.Now()
	_, err := Connect(context.Background(), Options{
		PortFile:            ep.PortFile(t),
		NoLaunch:            true,
		ConsentTimeout:      2 * time.Second,
		ConsentPendingAfter: 200 * time.Millisecond,
		OnConsentPending:    func() { pendingAt = time.Since(start) },
	})
	elapsed := time.Since(start)

	if got := connectErrCode(t, err); got != result.CodeConsentPending {
		t.Errorf("error.code = %q, want %q — a hanging upgrade must not surface as a generic failure", got, result.CodeConsentPending)
	}
	// VS-1: it stayed alive well past the dial timeout that used to abandon the
	// prompt, rather than giving up at ~10s.
	if elapsed < 1800*time.Millisecond {
		t.Errorf("gave up after %v, want the full ~2s consent budget", elapsed)
	}
	// VS-4: and the wait is bounded — a long wait is not an unbounded one.
	if elapsed > 20*time.Second {
		t.Errorf("waited %v; the consent wait must be bounded by consent_timeout", elapsed)
	}
	if pendingAt == 0 {
		t.Error("OnConsentPending never fired — the daemon can only tell the user while the dialog is up if it knows during the wait")
	} else if pendingAt > elapsed/2 {
		t.Errorf("OnConsentPending fired after %v of a %v wait — it must announce while the dialog is up, not on the way out", pendingAt, elapsed)
	}

	// VS-4: the message has to carry the one authored explanation of the prompt
	// AND the recovery, because the symptom the user is looking at is a browser
	// that appears to have crashed. Asserting on the const rather than on a list
	// of substrings is the point: five hand-written copies of this paragraph had
	// already drifted, and a substring list cannot tell the difference.
	msg := err.Error()
	if !strings.Contains(msg, browser.ConsentPromptAdvice) {
		t.Errorf("the consent-timeout message does not carry browser.ConsentPromptAdvice:\n%s", msg)
	}
	if !strings.Contains(msg, "--remote-debugging-port=9222") {
		t.Errorf("the consent-timeout message does not name the recovery:\n%s", msg)
	}
}

// TestConnectRefusedEndpointFailsFast is VS-2, the safety property that makes a
// two-minute wait acceptable at all: only an OPEN port earns it.
func TestConnectRefusedEndpointFailsFast(t *testing.T) {
	pf := probetest.Closed(t).PortFile(t) // nothing is listening there
	pinChromeRunning(t, false)

	start := time.Now()
	_, cerr := Connect(context.Background(), Options{
		PortFile:       pf,
		NoLaunch:       true, // never launch a real browser from a test
		ConsentTimeout: 60 * time.Second,
	})
	elapsed := time.Since(start)

	if got := connectErrCode(t, cerr); got != result.CodeConnection {
		t.Errorf("error.code = %q, want %q", got, result.CodeConnection)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("a closed port took %v to fail — it must fail fast, not wait out consent_timeout", elapsed)
	}
}

// TestConnectLateConsentIsNotAbandoned is VS-3: consent answered long after the
// old ~10s dial timeout still finds the connection there.
//
// It stops at the classification rather than asserting a working CDP session,
// because the stub is a socket and not a browser — a full success would need a
// fake Chrome speaking the protocol, and the defect this pins is entirely about
// whether we were still connected when the answer arrived.
func TestConnectLateConsentIsNotAbandoned(t *testing.T) {
	ep := probetest.Answer(t, 700*time.Millisecond, "HTTP/1.1 101 Switching Protocols")
	pinChromeRunning(t, true)

	start := time.Now()
	_, err := Connect(context.Background(), Options{
		PortFile:       ep.PortFile(t),
		NoLaunch:       true,
		ConsentTimeout: 10 * time.Second,
	})
	elapsed := time.Since(start)

	if code := connectErrCode(t, err); code == result.CodeConsentPending {
		t.Errorf("a completed upgrade was still reported as %q — a late Allow must be accepted, not timed out", code)
	}
	if !ep.AnsweredLive() {
		t.Error("the endpoint answered into a closed socket: the consent prompt was orphaned")
	}
	if elapsed < 600*time.Millisecond {
		t.Errorf("returned after %v, before the endpoint answered at 700ms — it gave up on the prompt", elapsed)
	}
}

// TestConnectNoEndpointLeadsWithTheLaunchFlag pins US-3: the route that never
// prompts is recommended before the toggle that prompts every time.
func TestConnectNoEndpointLeadsWithTheLaunchFlag(t *testing.T) {
	pinChromeRunning(t, true)
	_, err := Connect(context.Background(), Options{
		PortFile: filepath.Join(t.TempDir(), "no-such-port-file"),
		NoLaunch: true,
	})
	if got := connectErrCode(t, err); got != result.CodeNotDebug {
		t.Fatalf("error.code = %q, want %q", got, result.CodeNotDebug)
	}
	msg := err.Error()
	flagAt, toggleAt := strings.Index(msg, "--remote-debugging-port"), strings.Index(msg, "chrome://inspect")
	if flagAt < 0 || toggleAt < 0 {
		t.Fatalf("both routes should be offered:\n%s", msg)
	}
	if flagAt > toggleAt {
		t.Errorf("the message recommends the chrome://inspect toggle before the launch flag, which routes every new user through the consent prompt:\n%s", msg)
	}
}

// TestConnectExplicitPortStillProbes guards the path RFC-0013 now tells everyone
// to use. An explicit --port names an HTTP endpoint, not a WebSocket one; if the
// probe handshakes against that URL directly it never sees a 101 and reports a
// healthy Chrome as unreachable. Resolving through /json/version first is what
// keeps the recommended route working.
func TestConnectExplicitPortStillProbes(t *testing.T) {
	stalled := probetest.Stall(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"webSocketDebuggerUrl":%q}`, stalled.WS())
	}))
	defer srv.Close()

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	pinChromeRunning(t, false)

	_, err := Connect(context.Background(), Options{
		Port: p, NoLaunch: true, ConsentTimeout: 700 * time.Millisecond,
		ConsentPendingAfter: 100 * time.Millisecond,
	})
	if got := connectErrCode(t, err); got != result.CodeConsentPending {
		t.Errorf("error.code = %q, want %q — the --port endpoint was not probed as a WebSocket", got, result.CodeConsentPending)
	}
}

// TestConnectExplicitPortDetectsConsentWithoutJSONVersion is the same path with
// the JSON API withheld, which is what the toggle actually does.
//
// RFC-0013's fourth observation: on the chrome://inspect path /json/version
// 404s regardless of consent state. So `--port 9222` against a toggle-path
// Chrome holding an unanswered prompt used to resolve to nothing, classify as
// refused, and fall to InstructToggle — telling the user "Chrome is running but
// not debug-enabled" about a Chrome that IS debug-enabled and is at that moment
// showing them the dialog. It sent them to re-enable a setting already on.
func TestConnectExplicitPortDetectsConsentWithoutJSONVersion(t *testing.T) {
	// One listener that both 404s every HTTP request and stalls the upgrade:
	// exactly the toggle path with consent pending.
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
				buf := make([]byte, 1024)
				n, _ := c.Read(buf)
				if strings.Contains(string(buf[:n]), "/json/version") {
					_, _ = c.Write([]byte("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n"))
					_ = c.Close()
					return
				}
				<-t.Context().Done() // the upgrade: accepted, and then silence
				_ = c.Close()
			}(c)
		}
	}()

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	pinChromeRunning(t, true) // and yet: a hanging upgrade is not "enable the toggle"

	_, cerr := Connect(context.Background(), Options{
		Port: p, NoLaunch: true, ConsentTimeout: 700 * time.Millisecond,
		ConsentPendingAfter: 100 * time.Millisecond,
	})
	if got := connectErrCode(t, cerr); got != result.CodeConsentPending {
		t.Errorf("error.code = %q, want %q — with /json/version 404ing, the pending prompt is invisible and the user is told to re-enable a setting that is already on:\n%v",
			got, result.CodeConsentPending, cerr)
	}
}
