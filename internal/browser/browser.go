// Package browser holds the connection-layer logic for reaching Chrome over CDP:
// the DevToolsActivePort reader (Path B), the endpoint resolution, the
// vocabulary a probe answers in (WSState), and the connection-ladder decision.
//
// It is deliberately free of chromedp AND of any I/O against Chrome itself, so
// it unit-tests without a live browser. The socket work that classifies an
// endpoint used to live here and no longer does: see chrome.AwaitUpgrade.
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

// Endpoint is where a command should try to reach Chrome, and how that was
// decided. It exists because two callers have to agree on it: chrome.Connect,
// which attaches, and `doctor`, which diagnoses. doctor used to read the port
// file directly and ignore --port entirely, so `doctor --port 9333` probed a
// different browser than the one the flag named and reported IT healthy.
type Endpoint struct {
	// URL is the ws:// (port-file path) or http:// (explicit --port) endpoint,
	// or "" when none was found.
	URL string
	// PortFile is the DevToolsActivePort file URL came from, "" when an
	// explicit --port was used (there is no file in that case).
	PortFile string
	// Err is set when a port file was found and could not be read or parsed —
	// distinct from "no endpoint", because the remedy is different.
	Err error
}

// FindEndpoint resolves the debug endpoint from an explicit port, else the
// DevToolsActivePort file. An explicit --port wins: it names a specific Chrome,
// and a port file naming a different one is not a fallback for it.
func FindEndpoint(portFileOverride string, port int) Endpoint {
	if port != 0 {
		return Endpoint{URL: fmt.Sprintf("http://127.0.0.1:%d", port)}
	}
	pf := FindPortFile(portFileOverride)
	if pf == "" {
		return Endpoint{}
	}
	ws, err := WSURLFromPortFile(pf)
	if err != nil {
		return Endpoint{PortFile: pf, Err: err}
	}
	return Endpoint{URL: ws, PortFile: pf}
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

// EnableAdvice is the single authored answer to "how do I make Chrome
// debuggable?", and it leads with the launch flag ON PURPOSE.
//
// --remote-debugging-port skips the consent dialog entirely. The
// chrome://inspect toggle raises a browser-modal prompt on every fresh attach,
// and every message in this tool used to recommend it first — which routed each
// new user straight through the failure RFC-0013 exists to remove. The order of
// these two clauses is the fix.
const EnableAdvice = "relaunch Chrome with --remote-debugging-port=9222 " +
	"(on macOS: open -a \"Google Chrome\" --args --remote-debugging-port=9222), which never prompts; " +
	"or enable chrome://inspect/#remote-debugging, which raises a consent prompt on every fresh attach"

// ConsentPromptAdvice is the single authored explanation of Chrome's consent
// dialog, for every place that has to describe it: the connect timeout, the
// generic dial failure, the daemon's wait notice, the client's give-up message,
// and doctor's consent_pending state.
//
// Every clause is here because a user cannot deduce it. That the dialog is
// modal to the BROWSER is why the frozen window is a symptom and not a crash;
// that it can sit behind the window is why they have not seen it; that nothing
// else in Chrome responds until it is answered is why the tool looks like the
// thing that broke. Five hand-written copies had already drifted — one said
// "behind" where the others shouted it, one said "blocks all other input" —
// and two test files asserted on a substring list that one of those copies
// would have failed.
const ConsentPromptAdvice = "Chrome is holding its \"Allow remote debugging?\" consent prompt. " +
	"The prompt is browser-modal and can sit BEHIND the Chrome window, and Chrome accepts no other input until it is answered, " +
	"so a browser that looks frozen or crashed is usually this dialog. Find it and click Allow."

// WSState is what one WebSocket upgrade against Chrome's browser-level debug
// endpoint actually did. It is three-way, and that is the whole point.
//
// While consent for a fresh attach is pending, Chrome does not refuse the
// connection: it accepts the TCP connect, then holds the upgrade open and says
// nothing until the user answers a browser-modal dialog. There is no error to
// classify — only silence. A boolean "reachable" collapses that silence into the
// same value as a refused port, so the tool cannot tell "nothing is listening"
// (a real failure, and fast) from "Chrome is waiting for a human" (not a failure
// at all, and slow by nature). Splitting them is what lets a refused endpoint
// keep failing in milliseconds while a pending one is waited out for minutes.
//
// Note that Chrome's HTTP JSON API is NOT a substitute signal: on the
// chrome://inspect toggle path GET /json/version answers 404 whether or not
// consent has been granted. Only the upgrade distinguishes the states.
type WSState int

const (
	// WSRefused: nothing accepted the connection, or something answered the
	// upgrade with anything other than 101 (a stale port file, a different
	// server on the port). A real failure.
	WSRefused WSState = iota
	// WSPending: the port accepted and the upgrade never completed. This is the
	// consent signature.
	WSPending
	// WSReady: the upgrade completed — the endpoint is live and consented.
	WSReady
)

func (s WSState) String() string {
	switch s {
	case WSPending:
		return "pending"
	case WSReady:
		return "ready"
	default:
		return "refused"
	}
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
