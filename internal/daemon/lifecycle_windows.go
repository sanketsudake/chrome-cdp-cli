//go:build windows

package daemon

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachAttr configures the spawned daemon to detach from this console, so it
// outlives this process and doesn't share its console window (there is no
// setsid on Windows; a new, detached process group is the equivalent).
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}

// flockTry attempts a non-blocking exclusive lock on f via LockFileEx. A lock
// already held by another process reports (false, nil) — not an error — so
// the caller can treat contention as a thing to wait on rather than a
// failure.
func flockTry(f *os.File) (locked bool, err error) {
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// flockBlock takes the exclusive lock on f, waiting as long as it takes.
func flockBlock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol)
}

// flockUnlock releases the lock taken by flockTry or flockBlock.
func flockUnlock(f *os.File) {
	ol := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
