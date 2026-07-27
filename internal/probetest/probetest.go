// Package probetest builds the stub debug endpoints that RFC-0013's tests run
// against: a port that accepts and stalls, one that completes the WebSocket
// upgrade (now or later), and one with nothing listening at all.
//
// It exists because the consent-pending state is exactly a TCP listener that
// accepts and says nothing, so every scenario in that RFC is reproducible with
// net.Listen and no browser — which matters more here than usual: reproducing
// the bug by hand wedged a real user's Chrome twice, and a regression test that
// needs a human to click a browser-modal dialog is not a test.
//
// Three packages needed the same three listeners and each grew its own, with
// different signatures and separately maintained comments. Only two of them
// counted accepted connections, which is the assertion that proves the property
// the whole RFC turns on: each connection to the debug endpoint is a consent
// request, so "how many did we open" is the question.
//
// It cannot live in chrometest: that package imports internal/chrome, and
// internal/chrome is one of the packages that needs this.
package probetest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Endpoint is a stub Chrome debug endpoint in one of the shapes the probe has
// to tell apart.
type Endpoint struct {
	ln       net.Listener
	conns    atomic.Int32
	answered atomic.Bool
}

// Stall accepts connections and never answers — Chrome holding an unanswered
// consent prompt, which is the only state that hangs rather than failing.
func Stall(t *testing.T) *Endpoint {
	t.Helper()
	e := listen(t)
	go e.accept(func(c net.Conn) {
		<-t.Context().Done() // hold it open, saying nothing
		_ = c.Close()
	})
	return e
}

// Answer accepts and completes the WebSocket upgrade with status after delay —
// the user finding the dialog behind the window and clicking Allow. Pass a
// non-101 status for a live server that is not a CDP browser (a stale port file
// whose port another process has taken).
func Answer(t *testing.T, delay time.Duration, status string) *Endpoint {
	t.Helper()
	e := listen(t)
	go e.accept(func(c net.Conn) {
		defer c.Close()
		select {
		case <-time.After(delay):
		case <-t.Context().Done():
			return
		}
		if _, err := c.Write([]byte(status + "\r\n\r\n")); err == nil {
			// The write LANDED, so the socket was still open when the answer
			// arrived. That is precisely what "the prompt was not orphaned"
			// means in the failure this all exists to prevent.
			e.answered.Store(true)
		}
		time.Sleep(50 * time.Millisecond)
	})
	return e
}

// Closed returns an endpoint with nothing listening on it: a stale port file,
// or a Chrome that has quit. It must fail in milliseconds, which is the safety
// property that makes waiting minutes on a stalling one acceptable.
func Closed(t *testing.T) *Endpoint {
	t.Helper()
	e := listen(t)
	_ = e.ln.Close()
	return e
}

func listen(t *testing.T) *Endpoint {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return &Endpoint{ln: ln}
}

func (e *Endpoint) accept(handle func(net.Conn)) {
	for {
		c, err := e.ln.Accept()
		if err != nil {
			return
		}
		e.conns.Add(1)
		go handle(c)
	}
}

// Addr is the endpoint's address, listening or not.
func (e *Endpoint) Addr() net.Addr { return e.ln.Addr() }

// HostPort is the endpoint's "127.0.0.1:port".
func (e *Endpoint) HostPort() string { return e.ln.Addr().String() }

// Port is the endpoint's port number as a string.
func (e *Endpoint) Port() string {
	_, port, _ := net.SplitHostPort(e.ln.Addr().String())
	return port
}

// WS is the browser-level ws:// URL for this endpoint.
func (e *Endpoint) WS() string {
	return fmt.Sprintf("ws://%s/devtools/browser/stub", e.HostPort())
}

// Conns is how many connections have been accepted. Every one of them is a
// consent request, so a test that asserts zero (or exactly one) is asserting
// the property the RFC exists to protect.
func (e *Endpoint) Conns() int32 { return e.conns.Load() }

// AnsweredLive reports whether an Answer endpoint's reply landed on a socket
// that was still open — i.e. whether the consent the user granted went to a
// connection somebody had kept.
func (e *Endpoint) AnsweredLive() bool { return e.answered.Load() }

// PortFile writes a DevToolsActivePort file pointing at this endpoint and
// returns its path.
func (e *Endpoint) PortFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "DevToolsActivePort")
	if err := os.WriteFile(p, []byte(e.Port()+"\n/devtools/browser/stub\n"), 0o600); err != nil {
		t.Fatalf("write port file: %v", err)
	}
	return p
}

// UsePortFile writes the port file and points CHROME_CDP_PORT_FILE at it, so a
// command discovers this endpoint the way it discovers a real Chrome.
func (e *Endpoint) UsePortFile(t *testing.T) string {
	t.Helper()
	p := e.PortFile(t)
	t.Setenv("CHROME_CDP_PORT_FILE", p)
	return p
}
