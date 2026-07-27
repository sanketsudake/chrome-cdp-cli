package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrometest"
)

// streamBrowser serves a canned console read and a canned stream, so the tests
// exercise the RPC transport rather than Chrome.
type streamBrowser struct {
	chrometest.StubBrowser
	values  []any
	err     error
	gotOpts chrome.ConsoleOpts
	gotID   string
}

func (s *streamBrowser) Console(_ context.Context, id string, opts chrome.ConsoleOpts) (any, error) {
	s.gotID, s.gotOpts = id, opts
	return map[string]any{"messages": []any{}, "count": 0, "buffered": 7, "dropped": 2}, nil
}

func (s *streamBrowser) ConsoleStream(_ context.Context, id string, opts chrome.ConsoleOpts, emit func(any) error) error {
	s.gotID, s.gotOpts = id, opts
	for _, v := range s.values {
		if err := emit(v); err != nil {
			return err
		}
	}
	return s.err
}

func serveBrowser(t *testing.T, b chrome.Browser) *Client {
	t.Helper()
	// A short dir, not t.TempDir(): a Unix socket path is capped around 100
	// bytes, and t.TempDir() embeds the (long) test name.
	dir, err := os.MkdirTemp("", "cdpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go Serve(ln, b, time.Minute)
	t.Cleanup(func() { _ = ln.Close() })
	return &Client{path: sock}
}

func TestConsoleRPCRoundTrip(t *testing.T) {
	b := &streamBrowser{}
	rb := Remote(serveBrowser(t, b))

	res, err := rb.Console(context.Background(), "aa11", chrome.ConsoleOpts{
		Grep: `\[App\]`, Levels: []string{"error"}, Limit: 20, Since: 30 * time.Second, Clear: true,
	})
	if err != nil {
		t.Fatalf("Console: %v", err)
	}
	// The options must survive the socket intact, or server-side filtering
	// silently degrades to "everything" under the default connection path.
	if b.gotID != "aa11" || b.gotOpts.Grep != `\[App\]` || b.gotOpts.Limit != 20 || !b.gotOpts.Clear {
		t.Errorf("daemon received %q %+v", b.gotID, b.gotOpts)
	}
	if b.gotOpts.Since != 30*time.Second {
		t.Errorf("Since = %v, want 30s across the RPC", b.gotOpts.Since)
	}
	m, ok := res.(map[string]any)
	if !ok || m["buffered"].(float64) != 7 || m["dropped"].(float64) != 2 {
		t.Errorf("Console result = %v", res)
	}
}

// A streaming method cannot ride the unary one-request/one-response protocol,
// so the daemon serves many responses on one connection. This is the half that
// compiles fine and fails only under the daemon if it is missed.
func TestConsoleStreamRPCDeliversEveryValueInOrder(t *testing.T) {
	b := &streamBrowser{values: []any{
		map[string]any{"messages": []any{map[string]any{"text": "one"}}, "count": 1},
		map[string]any{"messages": []any{map[string]any{"text": "two"}}, "count": 1},
		map[string]any{"messages": []any{map[string]any{"text": "three"}}, "count": 1},
	}}
	rb := Remote(serveBrowser(t, b))

	var got []string
	err := rb.ConsoleStream(context.Background(), "aa11", chrome.ConsoleOpts{Grep: "x"}, func(v any) error {
		msgs := v.(map[string]any)["messages"].([]any)
		got = append(got, msgs[0].(map[string]any)["text"].(string))
		return nil
	})
	if err != nil {
		t.Fatalf("ConsoleStream: %v", err)
	}
	if len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Errorf("streamed %v, want one,two,three in order", got)
	}
	if b.gotOpts.Grep != "x" {
		t.Errorf("stream options did not cross the socket: %+v", b.gotOpts)
	}
}

func TestConsoleStreamRPCPropagatesAFailure(t *testing.T) {
	b := &streamBrowser{
		values: []any{map[string]any{"count": 1}},
		err:    errors.New("capture could not be enabled"),
	}
	rb := Remote(serveBrowser(t, b))

	n := 0
	err := rb.ConsoleStream(context.Background(), "aa11", chrome.ConsoleOpts{}, func(any) error { n++; return nil })
	if err == nil || err.Error() != "capture could not be enabled" {
		t.Fatalf("err = %v, want the stream's failure to reach the client", err)
	}
	// The values emitted before the failure still arrive: a stream that breaks
	// halfway has still told the caller something true.
	if n != 1 {
		t.Errorf("emitted %d values before the failure, want 1", n)
	}
}

// blockingStreamBrowser holds a stream open until its context ends, and reports
// when the stream started and stopped — the shape of a real `--follow` on a page
// that says nothing.
type blockingStreamBrowser struct {
	chrometest.StubBrowser
	started chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func (b *blockingStreamBrowser) Console(context.Context, string, chrome.ConsoleOpts) (any, error) {
	return map[string]any{"messages": []any{}, "count": 0}, nil
}

func (b *blockingStreamBrowser) ConsoleStream(ctx context.Context, _ string, _ chrome.ConsoleOpts, _ func(any) error) error {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	close(b.stopped)
	return nil
}

// THE regression test for the streaming mutex.
//
// A --follow stream runs for as long as the caller asked; holding the dispatch
// mutex for that whole window made `console --follow` in one terminal wedge
// `click` in another for the full --timeout. That defeats the literal user story
// the feature exists for (RFC-0002 US-2, "watch console output WHILE I exercise
// the page"), and it is invisible until two terminals are open at once.
func TestAStreamDoesNotBlockUnaryCalls(t *testing.T) {
	b := &blockingStreamBrowser{started: make(chan struct{}), stopped: make(chan struct{})}
	c := serveBrowser(t, b)
	rb := Remote(c)

	streamCtx, cancelStream := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStream()
	done := make(chan error, 1)
	go func() {
		done <- rb.ConsoleStream(streamCtx, "aa11", chrome.ConsoleOpts{}, func(any) error { return nil })
	}()

	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never reached the browser")
	}

	// With the stream in flight, an ordinary read must still be answered
	// promptly — it is a different command in a different terminal.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, err := rb.Console(ctx, "aa11", chrome.ConsoleOpts{}); err != nil {
		t.Fatalf("a unary Console during a live stream failed: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("a unary call waited %v for a live --follow to finish; "+
			"streaming must not hold the mutex that serialises unary dispatch", took)
	}
	cancelStream()
	<-done
}

// A Ctrl-C'd --follow must be noticed when the client goes away, not when the
// daemon next writes — which on a quiet page is never, so the stream held on for
// the client's whole TimeoutMs after nobody was left to read it.
func TestAStreamEndsWhenTheClientHangsUp(t *testing.T) {
	b := &blockingStreamBrowser{started: make(chan struct{}), stopped: make(chan struct{})}
	c := serveBrowser(t, b)

	// Dial by hand: the point is a client that disappears mid-stream, which the
	// Remote wrapper has no way to express.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := c.dial(ctx, "ConsoleStream", []any{"aa11", chrome.ConsoleOpts{}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never reached the browser")
	}
	_ = conn.Close()

	select {
	case <-b.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its client: a Ctrl-C'd --follow on a quiet page " +
			"would hold the daemon for the client's whole timeout")
	}
}

// A stream must keep the idle timer alive. The timer is otherwise reset only on
// Accept, which a stream does exactly once — so a --follow longer than the idle
// window had the listener closed under it, and the client saw EOF, exit 0, and
// no indication it had been cut short.
func TestALiveStreamHoldsOffTheIdleTimeout(t *testing.T) {
	b := &blockingStreamBrowser{started: make(chan struct{}), stopped: make(chan struct{})}
	dir, err := os.MkdirTemp("", "cdpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	s := &server{b: b, activity: make(chan struct{}, 1), stopCh: make(chan struct{}), pingEvery: 50 * time.Millisecond}
	pings := make(chan struct{}, 8)
	// Stand in for the idle goroutine, so the test asserts on the ping rather
	// than on a real 30-minute window.
	go func() {
		for {
			select {
			case <-s.activity:
				select {
				case pings <- struct{}{}:
				default:
				}
			case <-s.stopCh:
				return
			}
		}
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.ping()
			go s.handle(conn)
		}
	}()
	t.Cleanup(s.stop)

	c := &Client{path: sock}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := c.dial(ctx, "ConsoleStream", []any{"aa11", chrome.ConsoleOpts{}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never reached the browser")
	}
	<-pings // the Accept ping

	// The stream itself must go on reporting activity.
	select {
	case <-pings:
	case <-time.After(5 * time.Second):
		t.Fatal("a live stream never pinged the idle timer; a --follow longer than the idle " +
			"window would have its listener closed mid-stream")
	}
}

// The unary dispatch must not silently answer for a streaming method — that
// would hand the caller an empty result instead of its messages.
func TestUnaryDispatchRejectsAStreamingMethod(t *testing.T) {
	t.Parallel()
	s := &server{b: chrometest.StubBrowser{}}
	_, err := s.dispatch(t.Context(), "ConsoleStream", nil)
	if err == nil {
		t.Fatal("unary dispatch answered ConsoleStream; it has no way to deliver the emitted values")
	}
	if err.Error() == "unknown method: ConsoleStream" {
		t.Error("ConsoleStream is not routed at all; add it to streamDispatch")
	}
}
