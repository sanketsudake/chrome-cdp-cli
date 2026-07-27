package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// RFC-0013. Ensure has to know about the consent prompt even though the daemon
// is the process doing the waiting: the daemon is detached, so a client that
// gave up on its own ten-second clock would report a failure that had not
// happened and leave a live daemon behind holding a connection nobody uses.

// shrinkStartupWait shortens both the plain startup budget and the grace on top
// of the consent budget, so these run in milliseconds.
func shrinkStartupWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := startupWait
	startupWait = d
	t.Cleanup(func() { startupWait = prev })
}

// captureNotices redirects the advisories Ensure prints while it waits. The
// mutex is not decoration: lockSpawn's contention notice comes from whichever
// goroutine is blocked on the lock, not from the caller's.
func captureNotices(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	prev := notice
	notice = func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, msg)
	}
	t.Cleanup(func() { notice = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// bindAfter makes a fake daemon that binds sockPath after delay, so the socket
// becomes connectable at a time the test controls.
func bindAfter(t *testing.T, delay time.Duration) func(sockPath string) {
	t.Helper()
	return func(sockPath string) {
		go func() {
			time.Sleep(delay)
			ln, err := net.Listen("unix", sockPath)
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = ln.Close() })
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
	}
}

// TestEnsureWaitsOutTheConsentPrompt is VS-1 from the client side: a daemon that
// says it is holding a consent prompt is waited for, not declared dead.
func TestEnsureWaitsOutTheConsentPrompt(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d.sock")
	shrinkStartupWait(t, 300*time.Millisecond)
	notices := captureNotices(t)

	bind := bindAfter(t, time.Second) // ~3x the plain startup budget
	restore := swapSpawn(func(_, sockPath string, _ []string) (*daemonProc, error) {
		// The real daemon publishes this the moment chrome.Connect classifies the
		// upgrade as pending — while the dialog is still on screen.
		if err := os.WriteFile(sockPath+pendingSuffix, []byte("waiting\n"), 0o600); err != nil {
			t.Errorf("write pending sidecar: %v", err)
		}
		bind(sockPath)
		return liveProc(t), nil
	})
	defer restore()

	start := time.Now()
	c, err := Ensure(sock, "unused", nil, 3*time.Second)
	if err != nil {
		t.Fatalf("Ensure gave up on a daemon that was waiting for consent: %v", err)
	}
	if c == nil {
		t.Fatal("nil client and no error")
	}
	if el := time.Since(start); el < 900*time.Millisecond {
		t.Errorf("connected after %v, before the daemon was up — the test is not exercising the wait", el)
	}
	said := strings.Join(notices(), "\n")
	if said == "" {
		t.Error("nothing was said while waiting; a user staring at a frozen browser has to be told it is a dialog")
	} else if !strings.Contains(said, "Allow remote debugging") || !strings.Contains(said, "no other input") {
		t.Errorf("the wait notice must name the prompt and say Chrome accepts no other input:\n%s", said)
	}
}

// TestEnsureBoundsTheConsentWait is VS-4 from the client side: patient, not
// infinite.
func TestEnsureBoundsTheConsentWait(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d.sock")
	shrinkStartupWait(t, 200*time.Millisecond)
	captureNotices(t)

	restore := swapSpawn(func(_, sockPath string, _ []string) (*daemonProc, error) {
		return liveProc(t), os.WriteFile(sockPath+pendingSuffix, []byte("waiting\n"), 0o600)
	})
	defer restore()

	start := time.Now()
	_, err := Ensure(sock, "unused", nil, 500*time.Millisecond)
	elapsed := time.Since(start)

	var ce *chrome.ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *ConnectError, so no stable code reaches the envelope", err)
	}
	if ce.Code != result.CodeConsentPending {
		t.Errorf("error.code = %q, want %q", ce.Code, result.CodeConsentPending)
	}
	if elapsed < 500*time.Millisecond {
		t.Errorf("gave up after %v, inside the consent budget", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("waited %v — the consent wait must be bounded", elapsed)
	}
}

// TestEnsureFailsFastWithoutAPendingPrompt keeps the long wait from leaking into
// the ordinary broken-daemon case: no pending marker, no extension.
func TestEnsureFailsFastWithoutAPendingPrompt(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d.sock")
	shrinkStartupWait(t, 300*time.Millisecond)

	restore := swapSpawn(func(string, string, []string) (*daemonProc, error) { return liveProc(t), nil }) // never binds
	defer restore()

	start := time.Now()
	_, err := Ensure(sock, "unused", nil, 60*time.Second)
	elapsed := time.Since(start)

	var ce *chrome.ConnectError
	if !errors.As(err, &ce) || ce.Code != result.CodeDaemon {
		t.Errorf("error = %v, want a daemon_error", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("a daemon that never came up took %v — only a PENDING one earns the consent budget", elapsed)
	}
}

// TestRunDaemonPublishesPendingWhileWaiting is VS-1 end to end within the daemon
// process: the pending state is published DURING the wait (so the CLI can say so
// while the dialog is up), and the connect failure that eventually crosses the
// process boundary keeps its consent_pending code.
func TestRunDaemonPublishesPendingWhileWaiting(t *testing.T) {
	// A listener that accepts and stalls is exactly the consent-pending state.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
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

	dir := shortTempDir(t)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	pf := filepath.Join(dir, "DevToolsActivePort")
	if err := os.WriteFile(pf, []byte(port+"\n/devtools/browser/stub\n"), 0o600); err != nil {
		t.Fatalf("write port file: %v", err)
	}
	sock := filepath.Join(dir, "d.sock")

	done := make(chan error, 1)
	go func() {
		done <- RunDaemon(sock, chrome.Options{
			PortFile: pf, NoLaunch: true, ConsentTimeout: 3 * time.Second,
		}, time.Minute)
	}()

	sawPending := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(sock + pendingSuffix); err == nil {
			sawPending = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawPending {
		t.Error("the daemon never published the pending marker while it waited — the CLI has no way to know it is a dialog and not a hang")
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("RunDaemon never returned; the consent wait is unbounded")
	}

	data, err := os.ReadFile(sock + errSuffix)
	if err != nil {
		t.Fatalf("the daemon left no error sidecar: %v", err)
	}
	var ce *chrome.ConnectError
	if !errors.As(decodeConnectErr(data), &ce) || ce.Code != result.CodeConsentPending {
		t.Errorf("the sidecar decodes to %v, want a consent_pending ConnectError", decodeConnectErr(data))
	}
	if _, err := os.Stat(sock + pendingSuffix); err == nil {
		t.Error("the pending marker outlived the wait; a later run would read a stale prompt")
	}
}
