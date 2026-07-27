package browser

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/url"
	"strings"
	"time"
)

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

// Upgrade is one probe's outcome plus, when the endpoint accepted, the socket it
// used.
//
// The socket is kept rather than closed on purpose. Chrome asks for consent per
// fresh attach, so a probe that connects, learns the answer and hangs up has
// spent the user's click on a connection nobody kept; the follow-on attach would
// be a fresh one again. Holding it open until the real attach has been
// established means the click the user just made is still doing work when the
// attach lands. Close it as soon as the attach returns — see chrome.Connect.
type Upgrade struct {
	State WSState
	conn  net.Conn
}

// Close releases the probe socket (safe on a nil/refused Upgrade).
func (u *Upgrade) Close() {
	if u == nil || u.conn == nil {
		return
	}
	_ = u.conn.Close()
	u.conn = nil
}

// AwaitUpgrade dials wsURL and performs exactly ONE WebSocket handshake against
// it, classifying the result.
//
// It is deliberately a single connection: every connection to the debug endpoint
// is a consent request, and stacking those is what wedges a browser. The timings
// have three distinct jobs:
//
//   - dialTimeout bounds the TCP connect. Nothing listening is a fast, ordinary
//     failure and must stay one — this is the safety property that makes the long
//     wait below acceptable.
//   - pendingAfter is how much silence counts as "Chrome is asking the user".
//     Reaching it calls onPending (once) so the caller can say so while it waits,
//     rather than after.
//   - wait is the total budget. The same upgrade stays open across it, so an
//     answer that arrives late still lands on a live connection instead of an
//     orphaned one.
func AwaitUpgrade(wsURL string, dialTimeout, pendingAfter, wait time.Duration, onPending func()) *Upgrade {
	hostport, ok := HostPort(wsURL)
	if !ok {
		return &Upgrade{State: WSRefused}
	}
	conn, err := net.DialTimeout("tcp", hostport, dialTimeout)
	if err != nil {
		return &Upgrade{State: WSRefused}
	}
	if err := writeUpgradeRequest(conn, wsURL, hostport, dialTimeout); err != nil {
		_ = conn.Close()
		return &Upgrade{State: WSRefused}
	}

	// The read runs in a goroutine because there is nothing else to bound it:
	// a pending endpoint never writes and never closes. Closing conn is what
	// unblocks it, which the caller (or the timeout path below) always does.
	answered := make(chan bool, 1)
	go func() {
		line, err := bufio.NewReader(conn).ReadString('\n')
		answered <- err == nil && isSwitchingProtocols(line)
	}()

	if pendingAfter > wait {
		pendingAfter = wait
	}
	first := time.NewTimer(pendingAfter)
	defer first.Stop()
	select {
	case ok := <-answered:
		return settle(conn, ok)
	case <-first.C:
	}

	// Silence past pendingAfter on an OPEN port: the consent signature. Report it
	// now — a user who has not seen the dialog needs telling while it is still on
	// screen — and keep this same upgrade open for the rest of the budget.
	if onPending != nil {
		onPending()
	}
	rest := wait - pendingAfter
	if rest <= 0 {
		_ = conn.Close()
		return &Upgrade{State: WSPending}
	}
	second := time.NewTimer(rest)
	defer second.Stop()
	select {
	case ok := <-answered:
		return settle(conn, ok)
	case <-second.C:
		_ = conn.Close()
		return &Upgrade{State: WSPending}
	}
}

// settle turns a completed handshake into an Upgrade, keeping the socket only
// when it is worth keeping.
func settle(conn net.Conn, ok bool) *Upgrade {
	if !ok {
		_ = conn.Close()
		return &Upgrade{State: WSRefused}
	}
	return &Upgrade{State: WSReady, conn: conn}
}

// ProbeWS classifies an endpoint for a caller that wants the answer and not the
// socket — `doctor`, which must report what it verified and hold nothing.
func ProbeWS(wsURL string, dialTimeout, wait time.Duration) WSState {
	u := AwaitUpgrade(wsURL, dialTimeout, wait, wait, nil)
	defer u.Close()
	return u.State
}

// writeUpgradeRequest sends a minimal RFC 6455 handshake. The response is what
// classifies the endpoint; nothing is ever sent over the resulting connection,
// so no CDP session is started and no target is created.
func writeUpgradeRequest(conn net.Conn, wsURL, hostport string, timeout time.Duration) error {
	path := "/"
	if u, err := url.Parse(wsURL); err == nil && u.Path != "" {
		path = u.RequestURI()
	}
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + hostport + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(nonce[:]) + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	_, err := conn.Write([]byte(req))
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

// isSwitchingProtocols reports whether an HTTP status line accepted the upgrade.
func isSwitchingProtocols(line string) bool {
	f := strings.Fields(strings.TrimSpace(line))
	return len(f) >= 2 && strings.HasPrefix(f[0], "HTTP/") && f[1] == "101"
}
