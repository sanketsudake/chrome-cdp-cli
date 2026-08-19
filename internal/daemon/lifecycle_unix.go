//go:build !windows

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// detachAttr configures the spawned daemon to detach into its own session, so
// it outlives this process and is reparented rather than killed with it.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// flockTry attempts a non-blocking exclusive lock on f. A lock already held by
// another process reports (false, nil) — not an error — so the caller can
// treat contention as a thing to wait on rather than a failure.
func flockTry(f *os.File) (locked bool, err error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// flockBlock takes the exclusive lock on f, waiting as long as it takes.
func flockBlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// flockUnlock releases the lock taken by flockTry or flockBlock.
func flockUnlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
