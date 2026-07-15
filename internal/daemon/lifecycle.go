package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
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
	_ = os.Remove(sockPath) // clear a stale socket file

	cmd := exec.Command(exePath, "__daemon", sockPath)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach into its own session
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	_ = cmd.Process.Release()

	for range 100 { // up to ~10s for the first Allow-dialog click
		time.Sleep(100 * time.Millisecond)
		if c := TryConnect(sockPath); c != nil {
			return c, nil
		}
		// The daemon writes its connect error here before exiting.
		if data, e := os.ReadFile(sockPath + ".err"); e == nil && len(data) > 0 {
			return nil, errors.New(strings.TrimSpace(string(data)))
		}
	}
	return nil, errors.New("daemon did not start within 10s — did you click Allow in Chrome?")
}

// RunDaemon connects Chrome and serves sockPath until idle or stopped. Used by
// the hidden `__daemon` invocation.
func RunDaemon(sockPath string, opts chrome.Options, idle time.Duration) error {
	b, err := chrome.Connect(context.Background(), opts)
	if err != nil {
		// Leave the reason for Ensure to surface, then exit.
		_ = os.WriteFile(sockPath+".err", []byte(err.Error()), 0o600)
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
