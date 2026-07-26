package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrometest"
)

// netRPCBrowser records what crossed the socket, so the tests exercise the RPC
// transport rather than Chrome.
type netRPCBrowser struct {
	chrometest.StubBrowser
	values  []any
	gotOpts chrome.NetOpts
	gotCond chrome.NetCond
	gotID   string
}

func (n *netRPCBrowser) Net(_ context.Context, id string, opts chrome.NetOpts) (any, error) {
	n.gotID, n.gotOpts = id, opts
	return map[string]any{
		"requests": []any{}, "count": 0, "buffered": 11, "dropped": 4, "truncated": false, "pending": 3,
	}, nil
}

func (n *netRPCBrowser) NetStream(_ context.Context, id string, opts chrome.NetOpts, emit func(any) error) error {
	n.gotID, n.gotOpts = id, opts
	for _, v := range n.values {
		if err := emit(v); err != nil {
			return err
		}
	}
	return nil
}

func (n *netRPCBrowser) NetWait(_ context.Context, id string, cond chrome.NetCond) (map[string]any, error) {
	n.gotID, n.gotCond = id, cond
	return map[string]any{"matched": true, "request": map[string]any{"url": "https://app/api/save"}}, nil
}

// The daemon is the DEFAULT connection path, so options that do not survive the
// socket mean server-side filtering silently degrades to "everything" in normal
// use while every stub-backed test still passes.
func TestNetRPCRoundTrip(t *testing.T) {
	b := &netRPCBrowser{}
	rb := Remote(serveBrowser(t, b))

	res, err := rb.Net(context.Background(), "aa11", chrome.NetOpts{
		URL: "/api/save", Methods: []string{"POST"}, Status: ">=400", Types: []string{"xhr"},
		Failed: true, Limit: 20, Since: 30 * time.Second, Clear: true, Headers: true, Body: true,
	})
	if err != nil {
		t.Fatalf("Net: %v", err)
	}
	got := b.gotOpts
	if b.gotID != "aa11" || got.URL != "/api/save" || got.Status != ">=400" || got.Limit != 20 {
		t.Errorf("daemon received %q %+v", b.gotID, got)
	}
	if got.Since != 30*time.Second || !got.Clear || !got.Failed || !got.Headers || !got.Body {
		t.Errorf("options did not survive the socket: %+v", got)
	}
	// NoRedact must arrive as FALSE when it was not asked for: a default that
	// flipped across the RPC would leak credentials in exactly the normal path.
	if got.NoRedact {
		t.Error("NoRedact arrived set without --no-redact")
	}
	m, ok := res.(map[string]any)
	if !ok || m["buffered"].(float64) != 11 || m["pending"].(float64) != 3 {
		t.Errorf("Net result = %v", res)
	}
}

func TestNetWaitRPCRoundTrip(t *testing.T) {
	b := &netRPCBrowser{}
	rb := Remote(serveBrowser(t, b))

	res, err := rb.NetWait(context.Background(), "aa11", chrome.NetCond{
		URL: "/api/save", Methods: []string{"POST"}, Status: "2xx", Body: true,
	})
	if err != nil {
		t.Fatalf("NetWait: %v", err)
	}
	if b.gotCond.URL != "/api/save" || b.gotCond.Status != "2xx" || !b.gotCond.Body {
		t.Errorf("condition did not survive the socket: %+v", b.gotCond)
	}
	if res["matched"] != true {
		t.Errorf("NetWait result = %v", res)
	}
}

// A streaming method cannot ride the unary one-request/one-response protocol, so
// the daemon writes many responses on one connection. This is the half that
// compiles fine and fails only under the daemon if it is missed.
func TestNetStreamRPCDeliversEveryValueInOrder(t *testing.T) {
	b := &netRPCBrowser{values: []any{
		map[string]any{"requests": []any{map[string]any{"url": "one"}}, "count": 1},
		map[string]any{"requests": []any{map[string]any{"url": "two"}}, "count": 1},
		map[string]any{"requests": []any{map[string]any{"url": "three"}}, "count": 1},
	}}
	rb := Remote(serveBrowser(t, b))

	var got []string
	err := rb.NetStream(context.Background(), "aa11", chrome.NetOpts{URL: "/api"}, func(v any) error {
		reqs := v.(map[string]any)["requests"].([]any)
		got = append(got, reqs[0].(map[string]any)["url"].(string))
		return nil
	})
	if err != nil {
		t.Fatalf("NetStream: %v", err)
	}
	if len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Errorf("streamed %v, want one,two,three in order", got)
	}
	if b.gotOpts.URL != "/api" {
		t.Errorf("stream options did not cross the socket: %+v", b.gotOpts)
	}
}

// The unary dispatch must not silently answer for a streaming method — that
// would hand the caller an empty result instead of its requests.
func TestUnaryDispatchRejectsNetStream(t *testing.T) {
	t.Parallel()
	s := &server{b: chrometest.StubBrowser{}}
	_, err := s.dispatch(t.Context(), "NetStream", nil)
	if err == nil {
		t.Fatal("unary dispatch answered NetStream; it has no way to deliver the emitted values")
	}
	if err.Error() == "unknown method: NetStream" {
		t.Error("NetStream is not routed at all; add it to streamDispatch")
	}
}
