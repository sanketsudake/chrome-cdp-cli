package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// RFC-0013 US-5 is "at most one consent request", and #17 only made that true
// for the FIRST attach. Behind the spawn lock, every queued caller in turn
// cleared the previous verdict, spawned its own daemon and raised its own
// prompt: eight concurrent commands against an unanswered dialog came to about
// seventeen minutes and eight sequential prompts. VS-7 held (never two at once)
// and US-5 did not.

// writeConsentVerdict leaves the sidecar a timed-out daemon leaves behind.
func writeConsentVerdict(t *testing.T, sock string, age time.Duration) {
	t.Helper()
	path := sock + errSuffix
	payload := encodeConnectErr(&chrome.ConnectError{
		Code: result.CodeConsentPending, Message: "the daemon is still waiting on Chrome's \"Allow remote debugging?\" prompt",
	})
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write verdict: %v", err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestEnsureInheritsARecentConsentVerdict(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d.sock")
	shrinkStartupWait(t, 200*time.Millisecond)
	captureNotices(t)
	writeConsentVerdict(t, sock, 0)

	var spawns atomic.Int32
	restore := swapSpawn(func(string, string, []string) (*daemonProc, error) {
		spawns.Add(1)
		return liveProc(t), nil
	})
	defer restore()

	start := time.Now()
	_, err := Ensure(context.Background(), sock, "unused", nil, 60*time.Second)

	var ce *chrome.ConnectError
	if !errors.As(err, &ce) || ce.Code != result.CodeConsentPending {
		t.Fatalf("error = %v, want a consent_pending ConnectError inherited from the holder", err)
	}
	if got := spawns.Load(); got != 0 {
		t.Errorf("spawned %d daemons while a fresh consent verdict was on disk — each spawn attaches and raises its own prompt", got)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %v to inherit a verdict already written down", el)
	}
}

func TestEnsureIgnoresAStaleConsentVerdict(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d.sock")
	shrinkStartupWait(t, 200*time.Millisecond)
	captureNotices(t)
	// Old enough that the user has plausibly found the dialog and clicked
	// Allow: a verdict outlives its usefulness quickly, and refusing to retry
	// would strand them behind an answer they have already given.
	writeConsentVerdict(t, sock, time.Hour)

	var spawns atomic.Int32
	restore := swapSpawn(func(string, string, []string) (*daemonProc, error) {
		spawns.Add(1)
		return liveProc(t), nil
	})
	defer restore()

	if _, err := Ensure(context.Background(), sock, "unused", nil, time.Second); err == nil {
		t.Fatal("Ensure succeeded against a daemon that never bound")
	}
	if got := spawns.Load(); got != 1 {
		t.Errorf("spawned %d daemons, want 1 — a stale verdict must not become permanent", got)
	}
}

// TestEnsureLockWaitHonoursTheContext. Ensure took no ctx at all, so a caller
// queued behind a holder that was waiting out a prompt blocked for the holder's
// whole budget no matter what --timeout said.
func TestEnsureLockWaitHonoursTheContext(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d.sock")
	captureNotices(t)

	held, err := os.OpenFile(sock+lockSuffix, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock: %v", err)
	}
	defer syscall.Flock(int(held.Fd()), syscall.LOCK_UN)

	restore := swapSpawn(func(string, string, []string) (*daemonProc, error) {
		t.Error("spawned a daemon while another process held the lock")
		return liveProc(t), nil
	})
	defer restore()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Ensure(ctx, sock, "unused", nil, 60*time.Second); err == nil {
		t.Fatal("Ensure returned no error after its context expired")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("Ensure blocked on the spawn lock for %v after a 200ms deadline", el)
	}
}
