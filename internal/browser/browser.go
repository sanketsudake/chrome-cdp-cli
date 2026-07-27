// Package browser holds the connection-layer logic for reaching Chrome over CDP:
// the DevToolsActivePort reader (Path B) and the connection-ladder decision.
// It is deliberately free of chromedp so it unit-tests without a live browser.
package browser

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// ParseDevToolsActivePort parses the two-line DevToolsActivePort file
// (line 1 = port, line 2 = the browser-level ws path).
func ParseDevToolsActivePort(content []byte) (port int, wsPath string, err error) {
	lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
	if len(lines) < 2 {
		return 0, "", fmt.Errorf("DevToolsActivePort: expected 2 lines, got %d", len(lines))
	}
	port, err = strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return 0, "", fmt.Errorf("DevToolsActivePort: bad port %q: %w", lines[0], err)
	}
	wsPath = strings.TrimSpace(lines[1])
	if wsPath == "" {
		return 0, "", fmt.Errorf("DevToolsActivePort: empty ws path")
	}
	return port, wsPath, nil
}

// WSURLFromPortFile reads a DevToolsActivePort file and returns the literal
// browser-level WebSocket URL to hand to chromedp.NewRemoteAllocator.
func WSURLFromPortFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	port, wsPath, err := ParseDevToolsActivePort(content)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ws://127.0.0.1:%d%s", port, wsPath), nil
}

// CandidatePortFiles returns the OS-specific locations Chrome writes
// DevToolsActivePort to (Chrome family), first match wins. Overridable by the
// caller via CHROME_CDP_PORT_FILE.
func CandidatePortFiles() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		base := home + "/Library/Application Support/Google/Chrome"
		return []string{base + "/DevToolsActivePort", base + "/Default/DevToolsActivePort"}
	case "windows":
		la := os.Getenv("LOCALAPPDATA")
		return []string{la + `\Google\Chrome\User Data\DevToolsActivePort`}
	default:
		return []string{home + "/.config/google-chrome/DevToolsActivePort"}
	}
}

// FindPortFile returns the DevToolsActivePort path to use: an explicit override,
// else $CHROME_CDP_PORT_FILE, else the first existing OS candidate ("" if none).
func FindPortFile(override string) string {
	if override != "" {
		return override
	}
	if env := os.Getenv("CHROME_CDP_PORT_FILE"); env != "" {
		return env
	}
	for _, c := range CandidatePortFiles() {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// EndpointKey identifies the debug endpoint a command targets, so the daemon
// socket and sticky state are keyed to the actual Chrome instance rather than a
// fixed port. An explicit --port wins (distinct ports get distinct keys);
// otherwise the key comes from the discovered DevToolsActivePort file, falling
// back to "default".
func EndpointKey(portFile string, port int) string {
	if port != 0 {
		return fmt.Sprintf("127.0.0.1:%d", port)
	}
	if portFile != "" {
		if ws, err := WSURLFromPortFile(portFile); err == nil {
			if hp, ok := HostPort(ws); ok {
				return hp
			}
		}
	}
	return "default"
}

// HostPort extracts the host:port authority from a ws:// URL.
func HostPort(wsURL string) (string, bool) {
	_, rest, ok := strings.Cut(wsURL, "://")
	if !ok {
		return "", false
	}
	hp, _, _ := strings.Cut(rest, "/")
	return hp, hp != ""
}

// Action is the connection-ladder outcome for a given Probe.
type Action int

const (
	Attach           Action = iota // attach to Probe.PortFileWS (Path B)
	Launch                         // launch a managed Chrome (Path A fallback)
	InstructToggle                 // Chrome is running but not debug-enabled — guide the launch flag / chrome://inspect
	InstructNoLaunch               // nothing debug-enabled and --no-launch — print the launch command
	ConsentPending                 // open port, hanging upgrade — Chrome is holding its consent prompt
)

func (a Action) String() string {
	switch a {
	case Attach:
		return "attach"
	case Launch:
		return "launch"
	case InstructToggle:
		return "instruct-toggle"
	case InstructNoLaunch:
		return "instruct-no-launch"
	case ConsentPending:
		return "consent-pending"
	default:
		return "unknown"
	}
}

// Probe captures the observable connection state the ladder decides on.
type Probe struct {
	PortFileWS    string  // ws:// from DevToolsActivePort, or "" if unavailable
	WS            WSState // what one WebSocket upgrade against PortFileWS did
	ChromeRunning bool    // is a Chrome process running (possibly without debug)?
	NoLaunch      bool    // the --no-launch flag
}

// DecideConnection walks the connection ladder:
//  1. upgrade completed                   -> Attach (Path B)
//  2. open port, hanging upgrade          -> ConsentPending (Chrome is asking the user, not failing)
//  3. no reachable endpoint, Chrome up    -> InstructToggle (use the running session; never shadow it)
//  4. no reachable endpoint, no Chrome    -> Launch (Path A) unless --no-launch
//  5. ...with --no-launch                 -> InstructNoLaunch
//
// Rungs 1 and 2 are separate only because WS is three-way. While it was a bool,
// "the port refused us" and "the port accepted and then said nothing" were the
// same observation — which is exactly why a pending consent prompt could only
// ever surface as an undifferentiated timeout.
func DecideConnection(p Probe) Action {
	if p.PortFileWS != "" {
		switch p.WS {
		case WSReady:
			return Attach
		case WSPending:
			return ConsentPending
		}
	}
	if p.ChromeRunning {
		return InstructToggle
	}
	if p.NoLaunch {
		return InstructNoLaunch
	}
	return Launch
}
