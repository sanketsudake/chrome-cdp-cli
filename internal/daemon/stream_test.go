package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
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
