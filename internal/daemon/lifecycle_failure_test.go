package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrometest"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// deadProc is a daemon handle whose process has already gone.
func deadProc() *daemonProc {
	p := &daemonProc{exited: make(chan struct{})}
	close(p.exited)
	return p
}

// TestEnsureNoticesADeadDaemon. Once the pending marker appears, the only exits
// from the wait were a bindable socket, an .err sidecar, or the deadline — so a
// daemon that published "I am waiting on the prompt" and was then SIGKILLed (or
// panicked inside chrome.Connect, which RunDaemon does not recover) left the
// caller sitting for the whole consent budget plus the startup grace, ~130s,
// waiting for a process that no longer existed.
//
// A child that is gone with no .err behind it is an immediate failure, and it
// is not a consent failure: saying "still waiting on the prompt" about a dead
// daemon sends the user hunting for a dialog that nothing is holding.
func TestEnsureNoticesADeadDaemon(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d.sock")
	shrinkStartupWait(t, 200*time.Millisecond)
	captureNotices(t)

	restore := swapSpawn(func(_, sockPath string, _ []string) (*daemonProc, error) {
		// Publish the marker, then die without writing an error — a SIGKILL, or
		// a panic in the connect.
		if err := os.WriteFile(sockPath+pendingSuffix, []byte("waiting\n"), 0o600); err != nil {
			t.Errorf("write pending sidecar: %v", err)
		}
		return deadProc(), nil
	})
	defer restore()

	start := time.Now()
	_, err := Ensure(sock, "unused", nil, 60*time.Second)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("waited %v for a daemon that had already exited — the consent budget is for a live daemon holding a prompt", elapsed)
	}
	var ce *chrome.ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *ConnectError", err)
	}
	if ce.Code != result.CodeDaemon {
		t.Errorf("error.code = %q, want %q — a dead daemon is a daemon failure, not a pending prompt", ce.Code, result.CodeDaemon)
	}
}

// TestLockSpawnSaysItIsWaiting. The lock wait is unbounded on purpose (the
// holder may be waiting out a prompt nobody has clicked, and spawning our own
// would add to the pile), but it was also silent: a second invocation during a
// pending prompt printed nothing for 130 seconds. US-2 says tell the user what
// is happening, and this is the path that most needs it.
func TestLockSpawnSaysItIsWaiting(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d.sock")
	notices := captureNotices(t)

	// Hold the lock the way another chrome-cdp process would. flock is per
	// open-file-description, so a second open in this process still blocks.
	held, err := os.OpenFile(sock+lockSuffix, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock: %v", err)
	}

	got := make(chan func(), 1)
	go func() {
		unlock, err := lockSpawn(sock)
		if err != nil {
			t.Errorf("lockSpawn: %v", err)
			return
		}
		got <- unlock
	}()

	var said string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if got := notices(); len(got) > 0 {
			said = got[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if said == "" {
		t.Fatal("lockSpawn blocked in silence; a user running a second command during a pending prompt sees nothing at all")
	}
	if !strings.Contains(said, "waiting") {
		t.Errorf("the contention notice does not say what it is waiting for:\n%s", said)
	}

	_ = syscall.Flock(int(held.Fd()), syscall.LOCK_UN)
	select {
	case unlock := <-got:
		unlock()
	case <-time.After(3 * time.Second):
		t.Fatal("lockSpawn never acquired the lock after it was released")
	}
}

// TestRunDaemonReportsAPostConnectFailure. Everything after chrome.Connect
// wrote no .err sidecar, so a daemon that connected and then failed to bind its
// socket exited with nothing but a stderr nobody reads — and Ensure, seeing the
// pending marker and no error, reported the CONSENT PROMPT for what was a bind
// failure. The RFC's own darwin sun_path note makes that reachable.
func TestRunDaemonReportsAPostConnectFailure(t *testing.T) {
	prev := connectBrowser
	connectBrowser = func(context.Context, chrome.Options) (chrome.Browser, error) {
		return chrometest.StubBrowser{}, nil
	}
	t.Cleanup(func() { connectBrowser = prev })

	// A socket path already occupied by a non-empty directory: the connect
	// succeeds, the unlink and then the bind cannot.
	sock := filepath.Join(shortTempDir(t), "d.sock")
	if err := os.MkdirAll(filepath.Join(sock, "occupied"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err := RunDaemon(sock, chrome.Options{}, time.Minute)
	if err == nil {
		t.Fatal("RunDaemon returned nil for a socket it could not bind")
	}

	data, rerr := os.ReadFile(sock + errSuffix)
	if rerr != nil {
		t.Fatalf("the daemon left no error sidecar for a bind failure, so Ensure can only report the consent prompt: %v", rerr)
	}
	var ce *chrome.ConnectError
	decoded := decodeConnectErr(data)
	if !errors.As(decoded, &ce) || ce.Code != result.CodeDaemon {
		t.Errorf("the sidecar decodes to %v, want a daemon_error ConnectError", decoded)
	}
	if strings.Contains(decoded.Error(), "Allow remote debugging") {
		t.Errorf("a bind failure is reported as a consent prompt:\n%s", decoded)
	}
}
