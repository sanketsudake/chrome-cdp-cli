package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEnsureSpawnsOneDaemonUnderConcurrency: N callers, one daemon, one consent
// prompt. Why that matters, and why the unlinks have to be inside the lock too,
// is documented on Ensure.
func TestEnsureSpawnsOneDaemonUnderConcurrency(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d.sock")
	// Seven of the eight callers lose the lock race and say so; capture that
	// rather than printing it eight times.
	captureNotices(t)

	var spawns atomic.Int32
	restore := swapSpawn(func(_, sockPath string, _ []string) (*daemonProc, error) {
		spawns.Add(1)
		// Behave like the real daemon: bind the socket, a moment later, so the
		// window between spawning and being connectable is real rather than
		// instantaneous. Every caller must still converge on this one listener.
		go func() {
			time.Sleep(150 * time.Millisecond)
			ln, err := net.Listen("unix", sockPath)
			if err != nil {
				t.Errorf("fake daemon could not bind %s: %v", sockPath, err)
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
		return liveProc(t), nil
	})
	defer restore()

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	clients := make([]*Client, callers)
	for i := range callers {
		wg.Go(func() {
			clients[i], errs[i] = Ensure(context.Background(), sock, "unused", nil, 2*time.Second)
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: Ensure failed: %v", i, err)
		} else if clients[i] == nil {
			t.Errorf("caller %d: got a nil client and no error", i)
		}
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("%d daemons were spawned for %d concurrent callers, want exactly 1 — "+
			"each spawn attaches to Chrome and raises its own consent prompt", got, callers)
	}
}

// TestEnsureReusesARunningDaemon pins the fast path: an already-listening socket
// is connected to without taking the lock or spawning anything.
func TestEnsureReusesARunningDaemon(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	var spawns atomic.Int32
	restore := swapSpawn(func(string, string, []string) (*daemonProc, error) {
		spawns.Add(1)
		return liveProc(t), nil
	})
	defer restore()

	if _, err := Ensure(context.Background(), sock, "unused", nil, 2*time.Second); err != nil {
		t.Fatalf("Ensure against a live daemon: %v", err)
	}
	if got := spawns.Load(); got != 0 {
		t.Errorf("spawned %d daemons while one was already running, want 0", got)
	}
}

// shortTempDir returns a temp dir with a SHORT path. A Unix socket address is
// capped near 104 bytes on darwin, and t.TempDir() embeds the test's name — long
// enough here to fail the bind with a bare "invalid argument".
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cdpd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func swapSpawn(fn func(exePath, sockPath string, env []string) (*daemonProc, error)) func() {
	prev := spawnDaemon
	spawnDaemon = fn
	return func() { spawnDaemon = prev }
}

// liveProc is a daemon handle whose process is still running: Ensure's liveness
// check must find nothing wrong, so the sidecars and the deadline decide.
func liveProc(t *testing.T) *daemonProc {
	t.Helper()
	p := &daemonProc{exited: make(chan struct{})}
	t.Cleanup(func() { close(p.exited) })
	return p
}
