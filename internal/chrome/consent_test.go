package chrome

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// RFC-0013. The consent-pending state is a TCP listener that accepts and then
// stalls, so every scenario here runs against net.Listen and no browser at all.
// That is not a convenience: reproducing this by hand wedged a real user's
// Chrome twice, and a regression test that needs a human to click a modal is not
// a test.

// stallListener accepts and never answers — Chrome holding the consent prompt.
func stallListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
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
			held = append(held, c)
		}
	}()
	return ln
}

// lateAnswerListener stalls for delay and then completes the upgrade — the user
// finding the dialog behind the window and clicking Allow. answeredLive records
// that the answer landed on a still-open socket, which is precisely what "the
// prompt was not orphaned" means.
func lateAnswerListener(t *testing.T, delay time.Duration) (net.Listener, *atomic.Bool) {
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
				if _, err := c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n\r\n")); err == nil {
					live.Store(true)
				}
				time.Sleep(100 * time.Millisecond)
			}(c)
		}
	}()
	return ln, &live
}

// portFileFor writes a DevToolsActivePort file pointing at addr.
func portFileFor(t *testing.T, addr net.Addr) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	p := filepath.Join(t.TempDir(), "DevToolsActivePort")
	if err := os.WriteFile(p, []byte(fmt.Sprintf("%s\n/devtools/browser/stub\n", port)), 0o600); err != nil {
		t.Fatalf("write port file: %v", err)
	}
	return p
}

// shrinkPendingThreshold shortens the silence that counts as consent-pending, so
// a test can assert the announce-during-the-wait property in milliseconds.
func shrinkPendingThreshold(t *testing.T, d time.Duration) {
	t.Helper()
	prev := consentPendingAfter
	consentPendingAfter = d
	t.Cleanup(func() { consentPendingAfter = prev })
}

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
	ln := stallListener(t)
	pinChromeRunning(t, true) // even so: a hanging upgrade is not "enable the toggle"
	shrinkPendingThreshold(t, 200*time.Millisecond)

	var pendingAt time.Duration
	start := time.Now()
	_, err := Connect(context.Background(), Options{
		PortFile:         portFileFor(t, ln.Addr()),
		NoLaunch:         true,
		ConsentTimeout:   2 * time.Second,
		OnConsentPending: func() { pendingAt = time.Since(start) },
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

	// VS-4: the message has to name the prompt AND the recovery, because the
	// symptom the user is looking at is a browser that appears to have crashed.
	msg := err.Error()
	for _, want := range []string{"Allow remote debugging", "modal", "BEHIND", "no other input", "--remote-debugging-port=9222"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the consent-timeout message does not mention %q:\n%s", want, msg)
		}
	}
}

// TestConnectRefusedEndpointFailsFast is VS-2, the safety property that makes a
// two-minute wait acceptable at all: only an OPEN port earns it.
func TestConnectRefusedEndpointFailsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	pf := portFileFor(t, ln.Addr())
	_ = ln.Close() // nothing is listening there now
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
	ln, answeredLive := lateAnswerListener(t, 700*time.Millisecond)
	pinChromeRunning(t, true)

	start := time.Now()
	_, err := Connect(context.Background(), Options{
		PortFile:       portFileFor(t, ln.Addr()),
		NoLaunch:       true,
		ConsentTimeout: 10 * time.Second,
	})
	elapsed := time.Since(start)

	if code := connectErrCode(t, err); code == result.CodeConsentPending {
		t.Errorf("a completed upgrade was still reported as %q — a late Allow must be accepted, not timed out", code)
	}
	if !answeredLive.Load() {
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
