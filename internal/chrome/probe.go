// Probe classification of Chrome's debug endpoint: the one WebSocket handshake
// that tells "nothing is listening" apart from "Chrome is asking the user".
//
// It lives here, next to the chromedp connection it feeds, rather than in
// internal/browser — whose own doc says it is deliberately free of chromedp so
// it unit-tests without a live browser. That claim held for a port-file parser
// and a decision table; it did not survive a TCP dialer, an HTTP client, a
// hand-rolled RFC 6455 handshake, a reader goroutine, and a live net.Conn
// handed across the package boundary for this package to close. The tests are
// net.Listen-based and need no browser either way.

package chrome

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
)

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
	State browser.WSState
	conn  net.Conn
}

// Close releases the probe socket. Safe on a refused or pending Upgrade, which
// have no socket to release — AwaitUpgrade never returns nil, so that is the
// only case there is.
func (u *Upgrade) Close() {
	if u.conn == nil {
		return
	}
	_ = u.conn.Close()
	u.conn = nil
}

// ResolveWSURL returns the browser-level ws:// URL to probe and attach to.
//
// A ws:// endpoint (the DevToolsActivePort path) is already one. An explicit
// --port names an http:// endpoint instead, and the browser-level WebSocket path
// is normally discoverable through Chrome's HTTP JSON API — the same resolution
// chromedp's remote allocator performs, done here first so the probe and the
// attach agree on what they are talking to.
//
// This is NOT the consent check, and the difference matters: /json/version
// answers the same whether or not consent is pending (on the chrome://inspect
// path it 404s either way), so it can locate an endpoint and can never classify
// one. Only the upgrade does that.
//
// Which is exactly why a failed lookup falls back to the ws:// ROOT of the same
// host:port rather than giving up. On the toggle path the JSON API is simply
// absent, so "no answer from /json/version" carried no information at all — and
// treating it as "no endpoint" made the pending state undetectable on the one
// path that actually prompts: `--port 9222` against a toggle-path Chrome
// holding an unanswered dialog reported not_debug_enabled, sending the user to
// re-enable a setting that was already on. Probing the root instead costs
// nothing in the granted case (an endpoint that will not upgrade there is
// classified refused, which is where it already was) and makes the hang — the
// one unambiguous consent signature — visible.
func ResolveWSURL(endpoint string) (string, bool) {
	switch {
	case strings.HasPrefix(endpoint, "ws://"), strings.HasPrefix(endpoint, "wss://"):
		return endpoint, true
	case strings.HasPrefix(endpoint, "http://"), strings.HasPrefix(endpoint, "https://"):
	default:
		return "", false
	}
	hostport, ok := browser.HostPort(endpoint)
	if !ok {
		return "", false
	}
	if ws, ok := wsFromJSONVersion(endpoint, hostport); ok {
		return ws, true
	}
	return "ws://" + hostport + "/", true
}

// wsFromJSONVersion asks Chrome's HTTP JSON API where the browser-level
// WebSocket is. It reports false for every way that can fail to answer, all of
// which mean the same thing here: ask the socket instead.
//
// The answer is only accepted if it points back at the endpoint we asked. This
// is a question about ONE loopback port, and both the redirect chain and the
// returned URL used to be taken on trust: http.Client follows up to ten
// redirects, and the webSocketDebuggerUrl was returned verbatim, so a request
// about 127.0.0.1 was verified to come back with ws://10.1.2.3:4444/pwned —
// and that URL is what the probe dials and what chromedp attaches to.
func wsFromJSONVersion(endpoint, hostport string) (string, bool) {
	client := &http.Client{
		Timeout: dialTimeout,
		// A redirect is not an answer to "where is YOUR WebSocket".
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(strings.TrimSuffix(endpoint, "/") + "/json/version")
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil || v.WS == "" {
		return "", false
	}
	u, err := url.Parse(v.WS)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.User != nil || !sameEndpoint(u.Host, hostport) {
		return "", false
	}
	return v.WS, true
}

// sameEndpoint reports whether a returned authority names the endpoint we
// asked. The port must match exactly; the host must match too, except that two
// spellings of loopback are accepted for each other because Chrome answers with
// whichever one it was asked on and the caller may have used the other.
func sameEndpoint(got, want string) bool {
	if got == want {
		return true
	}
	gh, gp, err := net.SplitHostPort(got)
	if err != nil {
		return false
	}
	wh, wp, err := net.SplitHostPort(want)
	if err != nil || gp != wp {
		return false
	}
	return isLoopback(gh) && isLoopback(wh)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// maxHandshakeResponse caps what the probe will read looking for the
// handshake's status line and headers. Those are a few hundred bytes; 8 KiB is
// generous and still nothing at all to hold.
const maxHandshakeResponse = 8 << 10

// UpgradeTimings bounds one probe. It is a struct rather than two positional
// durations because the two are easy to swap and the consequences are not
// symmetric: a pending threshold longer than the budget announces nothing, and
// a budget shorter than the threshold abandons the prompt it just raised.
type UpgradeTimings struct {
	// PendingAfter is how much silence counts as "Chrome is asking the user".
	// Reaching it calls onPending (once) so the caller can say so WHILE it
	// waits, rather than afterwards, which would be a post-mortem.
	PendingAfter time.Duration
	// Total is the whole budget. The same upgrade stays open across it, so an
	// answer that arrives late still lands on a live connection instead of an
	// orphaned one.
	Total time.Duration
}

// dialTimeout bounds the TCP connect. Nothing listening is a fast, ordinary
// failure and must stay one — that is the safety property that makes the long
// consent wait acceptable at all. It is a constant, not a parameter: this is
// loopback, both call sites had independently declared the same two seconds,
// and a caller has no information that would make a different value right.
const dialTimeout = 2 * time.Second

// AwaitUpgrade dials wsURL and performs exactly ONE WebSocket handshake against
// it, classifying the result.
//
// It is deliberately a single connection: every connection to the debug endpoint
// is a consent request, and stacking those is what wedges a browser.
func AwaitUpgrade(wsURL string, t UpgradeTimings, onPending func()) *Upgrade {
	hostport, ok := browser.HostPort(wsURL)
	if !ok {
		return &Upgrade{State: browser.WSRefused}
	}
	conn, dialErr := net.DialTimeout("tcp", hostport, dialTimeout)
	if dialErr != nil {
		return &Upgrade{State: browser.WSRefused}
	}
	key, err := writeUpgradeRequest(conn, wsURL, hostport)
	if err != nil {
		_ = conn.Close()
		return &Upgrade{State: browser.WSRefused}
	}
	wait := t.Total

	// The read runs in a goroutine because there is nothing else to bound its
	// COMPLETION: a pending endpoint never writes and never closes. Closing
	// conn is what unblocks it, which the caller (or the timeout path below)
	// always does.
	//
	// Its SIZE and its lifetime are bounded here, and both bounds are load-
	// bearing. Anything that can bind the loopback debug port can answer, and
	// what is expected back is one line of HTTP: without the LimitReader,
	// ReadString('\n') on a newline-free stream accumulated at gigabytes per
	// second for the caller's whole budget — two minutes in the daemon. The
	// read deadline is the matching bound in time, so the goroutine cannot
	// outlive the wait even if nobody closes the socket.
	//
	// The deadline sits a little PAST the budget rather than on it. It is a
	// backstop against a goroutine that outlives everything, not a second copy
	// of the budget — the timers below own that — and putting it exactly on the
	// budget makes an answer that lands as the wait ends unreadable, so a
	// completed handshake still in flight would be reported as a pending
	// prompt.
	_ = conn.SetReadDeadline(time.Now().Add(wait + dialTimeout))
	answered := make(chan bool, 1)
	go func() {
		ok, err := readHandshakeResponse(conn, key)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			// Hitting the deadline is the endpoint's SILENCE, not its answer:
			// reporting it as a failed handshake would classify a Chrome that
			// is still holding the consent prompt as refused. The caller's own
			// timers below say what silence means.
			return
		}
		answered <- ok
	}()

	// Two independent timers on one loop. They were once two sequential selects
	// with a "rest" computation between them, which had a case the loop simply
	// does not have: when PendingAfter was >= Total the remainder came out <= 0
	// and the function returned browser.WSPending having never looked at the answer
	// channel again, so a completed handshake sitting in the buffer was thrown
	// away and its socket closed — doctor reporting consent_pending for a ready
	// endpoint.
	pending := time.NewTimer(min(t.PendingAfter, wait))
	defer pending.Stop()
	total := time.NewTimer(wait)
	defer total.Stop()
	for {
		select {
		case ok := <-answered:
			return upgraded(conn, ok)
		case <-pending.C:
			// Silence past PendingAfter on an OPEN port: the consent
			// signature. Say so now — a user who has not seen the dialog needs
			// telling while it is still on screen — and keep this same upgrade
			// open for the rest of the budget.
			if onPending != nil {
				onPending()
			}
		case <-total.C:
			// The budget and the answer can come ready in the same instant,
			// and select picks between ready cases at random. Ask once more
			// before discarding a handshake that did complete.
			select {
			case ok := <-answered:
				return upgraded(conn, ok)
			default:
			}
			_ = conn.Close()
			return &Upgrade{State: browser.WSPending}
		}
	}
}

// upgraded turns a completed handshake into an Upgrade, keeping the socket only
// when it is worth keeping. NOT called settle: everything else in this package
// that says "settle" means "wait until it stops moving" (settle, settledPageRect,
// settledNodePoint), and this one means "close it or keep it".
func upgraded(conn net.Conn, ok bool) *Upgrade {
	if !ok {
		_ = conn.Close()
		return &Upgrade{State: browser.WSRefused}
	}
	// The probe's read deadline bounded the handshake; a socket that is being
	// KEPT must not carry it into the attach that follows.
	_ = conn.SetReadDeadline(time.Time{})
	return &Upgrade{State: browser.WSReady, conn: conn}
}

// ProbeWS classifies an endpoint for a caller that wants the answer and not the
// socket — `doctor`, which must report what it verified and hold nothing.
//
// It therefore does the thing Upgrade's doc comment warns about: it connects,
// learns the answer, and hangs up, spending the user's click on a connection
// nobody kept. That is the right trade HERE and only here. doctor is a
// diagnostic with nothing to hand a live socket to, and holding one open past
// the command that made it would be worse. What it must not do is pretend
// otherwise, so doctor's ready verdict says the connection was closed and the
// next command may prompt again — see runDoctor.
func ProbeWS(wsURL string, wait time.Duration) browser.WSState {
	u := AwaitUpgrade(wsURL, UpgradeTimings{PendingAfter: wait, Total: wait}, nil)
	defer u.Close()
	return u.State
}

// The handshake is written and verified by hand rather than with a WebSocket
// library, and that IS the right call here even though it looks like the wrong
// one. What this code has to observe is "accepted, then silent for two
// minutes", and a dialer's API cannot express that: it returns a connection or
// an error, and the state we care about is neither. Owning the socket is the
// only way to hold a pending upgrade open across the consent wait, which is the
// whole point. (It also keeps a WebSocket library out of the direct
// dependencies for one handshake.)

// writeUpgradeRequest sends a minimal RFC 6455 handshake and returns the
// Sec-WebSocket-Key it used, which the response has to be checked against.
// Nothing is ever sent over the resulting connection, so no CDP session is
// started and no target is created.
func writeUpgradeRequest(conn net.Conn, wsURL, hostport string) (key string, err error) {
	path := "/"
	if u, perr := url.Parse(wsURL); perr == nil && u.Path != "" {
		path = u.RequestURI()
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	key = base64.StdEncoding.EncodeToString(nonce[:])
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + hostport + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	_ = conn.SetWriteDeadline(time.Now().Add(dialTimeout))
	_, err = conn.Write([]byte(req))
	_ = conn.SetWriteDeadline(time.Time{})
	return key, err
}

// wsGUID is RFC 6455's fixed accept-key salt.
const wsGUID = "258EAFA5-E914-47DA-95CA-5AB0DC85B11A"

// acceptFor is the Sec-WebSocket-Accept a correct server must return for key.
func acceptFor(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// readHandshakeResponse reads the status line and headers and reports whether
// this is really a WebSocket server completing OUR handshake.
//
// The key was generated with crypto/rand and then never checked, so the whole
// test was "did the status line contain 101" — which anything replying
// "HTTP/9.9 101 whatever" passes. Chrome's debug port is a loopback port any
// local process can bind, and being told "ready" by something that is not
// Chrome is how a probe ends up handing chromedp a socket that will never speak
// CDP. Verifying the accept key is what makes the 101 mean this server saw this
// request.
func readHandshakeResponse(conn net.Conn, key string) (bool, error) {
	r := bufio.NewReader(io.LimitReader(conn, maxHandshakeResponse))
	line, err := r.ReadString('\n')
	if err != nil || !isSwitchingProtocols(line) {
		return false, err
	}
	var upgraded, accepted bool
	for {
		h, err := r.ReadString('\n')
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(h) == "" { // end of headers
			break
		}
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "upgrade":
			upgraded = strings.EqualFold(strings.TrimSpace(value), "websocket")
		case "sec-websocket-accept":
			accepted = strings.TrimSpace(value) == acceptFor(key)
		}
	}
	return upgraded && accepted, nil
}

// isSwitchingProtocols reports whether an HTTP status line accepted the upgrade.
func isSwitchingProtocols(line string) bool {
	f := strings.Fields(strings.TrimSpace(line))
	return len(f) >= 2 && strings.HasPrefix(f[0], "HTTP/") && f[1] == "101"
}
