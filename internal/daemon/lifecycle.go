package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// SocketPath returns the daemon socket path for an endpoint key, under the XDG
// runtime/state dir. Liveness is discovered by connecting (no PID file).
func SocketPath(key string) string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		if s := os.Getenv("XDG_STATE_HOME"); s != "" {
			base = s
		} else {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, ".local", "state")
		}
	}
	dir := filepath.Join(base, "chrome-cdp")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "daemon-"+sanitize(key)+".sock")
}

func sanitize(s string) string {
	if s == "" {
		return "default"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}

// connectErr is the JSON payload written to the .err sidecar so the daemon's
// connect failure crosses the process boundary with its stable code intact.
type connectErr struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// encodeConnectErr serializes a connect failure, preserving a *chrome.ConnectError's
// code so Ensure can reconstruct it.
func encodeConnectErr(err error) []byte {
	e := connectErr{Message: err.Error()}
	var ce *chrome.ConnectError
	if errors.As(err, &ce) {
		e.Code = ce.Code
	}
	b, _ := json.Marshal(e)
	return b
}

// decodeConnectErr reconstructs a connect failure from the sidecar. With a code
// present it returns a *chrome.ConnectError so callers recover the stable code;
// otherwise (including a legacy pre-JSON payload) it surfaces the raw message.
func decodeConnectErr(data []byte) error {
	var e connectErr
	if err := json.Unmarshal(data, &e); err != nil || e.Message == "" {
		return errors.New(strings.TrimSpace(string(data)))
	}
	if e.Code != "" {
		return &chrome.ConnectError{Code: e.Code, Message: e.Message}
	}
	return errors.New(e.Message)
}

// TryConnect returns a Client if a daemon is already listening on sockPath.
func TryConnect(sockPath string) *Client {
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		return nil
	}
	_ = conn.Close()
	return &Client{path: sockPath}
}

// Ensure connects to a running daemon, or spawns one (detached) and waits for it
// to come up. env carries the connection options (CHROME_CDP_*) for the daemon.
func Ensure(sockPath, exePath string, env []string) (*Client, error) {
	if c := TryConnect(sockPath); c != nil {
		return c, nil
	}

	// From here on, exactly one process at a time. Concurrent invocations that
	// all find no daemon would otherwise each spawn one, and each spawned daemon
	// attaches to Chrome — which raises a SEPARATE browser-modal "Allow remote
	// debugging?" prompt. Several stacked prompts is not a slower version of
	// one: the visible dialog need not be the one holding input, so the whole
	// browser looks frozen with no button that responds. The daemon exists so
	// that prompt happens once per session; nothing was making the FIRST attach
	// single-file.
	//
	// The unlinks below are the other half. Outside the lock they can delete a
	// socket a sibling daemon has just bound, orphaning a live daemon that no
	// client can ever reach.
	unlock, err := lockSpawn(sockPath)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Re-check under the lock: while we waited, the holder may have started the
	// daemon we were about to duplicate. This is what makes N callers converge
	// on one daemon and one prompt.
	if c := TryConnect(sockPath); c != nil {
		return c, nil
	}

	_ = os.Remove(sockPath)          // clear a stale socket file
	_ = os.Remove(sockPath + ".err") // and a stale error, so we only read THIS spawn's

	if err := spawnDaemon(exePath, sockPath, env); err != nil {
		return nil, err
	}

	for range 100 { // up to ~10s for the first Allow-dialog click
		time.Sleep(100 * time.Millisecond)
		if c := TryConnect(sockPath); c != nil {
			return c, nil
		}
		// The daemon writes its connect error here before exiting; decode it so
		// the specific code (e.g. not_debug_enabled) survives the process boundary.
		if data, e := os.ReadFile(sockPath + ".err"); e == nil && len(data) > 0 {
			return nil, decodeConnectErr(data)
		}
	}
	return nil, &chrome.ConnectError{Code: result.CodeDaemon, Message: "daemon did not start within 10s — Chrome may be waiting on its \"Allow remote debugging?\" prompt; it can hide behind the window, and until it is answered Chrome accepts no other input"}
}

// spawnDaemon starts the detached daemon process. It is a variable so a test can
// substitute a spawn it can count, without a real Chrome or a real binary.
var spawnDaemon = func(exePath, sockPath string, env []string) error {
	cmd := exec.Command(exePath, "__daemon", sockPath)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach into its own session
	if err := cmd.Start(); err != nil {
		return &chrome.ConnectError{Code: result.CodeDaemon, Message: "cannot start daemon: " + err.Error()}
	}
	_ = cmd.Process.Release()
	return nil
}

// lockSpawn takes an exclusive advisory lock covering the spawn-and-wait for one
// socket path, and returns the release. The lock file is never removed: unlinking
// it would let a later caller lock a different inode and defeat the exclusion.
//
// The wait is deliberately unbounded. The holder may be waiting out a consent
// prompt the user has not clicked yet, and blocking behind it is the correct
// outcome — spawning our own would add another prompt to the pile, which is the
// failure this exists to prevent.
func lockSpawn(sockPath string) (func(), error) {
	f, err := os.OpenFile(sockPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, &chrome.ConnectError{Code: result.CodeDaemon, Message: "cannot open the daemon spawn lock: " + err.Error()}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, &chrome.ConnectError{Code: result.CodeDaemon, Message: "cannot take the daemon spawn lock: " + err.Error()}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// RunDaemon connects Chrome and serves sockPath until idle or stopped. Used by
// the hidden `__daemon` invocation.
func RunDaemon(sockPath string, opts chrome.Options, idle time.Duration) error {
	b, err := chrome.Connect(context.Background(), opts)
	if err != nil {
		// Leave the reason (with its code) for Ensure to surface, then exit.
		_ = os.WriteFile(sockPath+".err", encodeConnectErr(err), 0o600)
		return err
	}
	_ = os.Remove(sockPath + ".err")
	defer b.Close()

	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	defer os.Remove(sockPath)

	Serve(ln, b, idle)
	return nil
}
