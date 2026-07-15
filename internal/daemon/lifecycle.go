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
	_ = os.Remove(sockPath)          // clear a stale socket file
	_ = os.Remove(sockPath + ".err") // and a stale error, so we only read THIS spawn's

	cmd := exec.Command(exePath, "__daemon", sockPath)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach into its own session
	if err := cmd.Start(); err != nil {
		return nil, &chrome.ConnectError{Code: result.CodeDaemon, Message: "cannot start daemon: " + err.Error()}
	}
	_ = cmd.Process.Release()

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
	return nil, &chrome.ConnectError{Code: result.CodeDaemon, Message: "daemon did not start within 10s — did you click Allow in Chrome?"}
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
