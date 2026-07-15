// Package daemon runs a background process that holds one CDP connection and
// serves Browser calls over a Unix socket. Keeping a single long-lived
// connection means Chrome's "Allow debugging?" prompt fires once per session
// (not once per command) and each command is a fast socket round-trip.
//
// Protocol: one JSON request/response per connection (NDJSON), where a request
// names a Browser method and carries its positional args as JSON.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// Request is a Browser method call. Args are the positional arguments (after
// ctx) as JSON. TimeoutMs carries the client's remaining deadline so the daemon
// bounds the action instead of running (and blocking the client) forever.
type Request struct {
	Method    string            `json:"method"`
	Args      []json.RawMessage `json:"args,omitempty"`
	TimeoutMs int64             `json:"timeout_ms,omitempty"`
}

// Response is the method result (or an error string).
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// server holds the real Browser and serializes dispatch (chromedp calls are not
// meant to run fully concurrently against one connection).
type server struct {
	b        chrome.Browser
	mu       sync.Mutex
	activity chan struct{} // activity pings, to reset the idle timer
	stopCh   chan struct{}
	stopOnce sync.Once
}

func (s *server) stop() { s.stopOnce.Do(func() { close(s.stopCh) }) }

// Serve accepts connections on ln and dispatches each to b until an idle period
// of `idle` elapses or a stop request arrives.
func Serve(ln net.Listener, b chrome.Browser, idle time.Duration) {
	s := &server{b: b, activity: make(chan struct{}, 1), stopCh: make(chan struct{})}

	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-s.activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
			case <-timer.C:
				_ = ln.Close()
				return
			case <-s.stopCh:
				_ = ln.Close()
				return
			}
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed (idle/stop)
		}
		s.ping()
		go s.handle(conn)
	}
}

func (s *server) ping() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

func (s *server) handle(conn net.Conn) {
	defer conn.Close()
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}

	// __stop must respond even while a slow action holds the mutex, so it never
	// waits on dispatch.
	if req.Method == "__stop" {
		s.stop()
		reply(conn, map[string]any{"stopped": true}, nil)
		return
	}

	// Bound the action by the client's remaining deadline, so a slow/hung action
	// returns a clean error (and frees the mutex) instead of blocking forever.
	ctx := context.Background()
	if req.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	s.mu.Lock()
	res, err := s.dispatch(ctx, req.Method, req.Args)
	s.mu.Unlock()
	reply(conn, res, err)
}

// reply writes a Response for a dispatch result to conn.
func reply(conn net.Conn, res any, err error) {
	resp := Response{}
	if err != nil {
		resp.Error = err.Error()
	} else if res != nil {
		if b, mErr := json.Marshal(res); mErr == nil {
			resp.Result = b
		} else {
			resp.Error = mErr.Error()
		}
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

// dispatch routes a method name + JSON args to the Browser.
func (s *server) dispatch(ctx context.Context, method string, args []json.RawMessage) (any, error) {
	b := s.b
	switch method {
	case "__status":
		// Best-effort: report the tabs the daemon can currently see. A List
		// failure just omits them rather than failing the status call.
		info := map[string]any{"connected": true}
		if tabs, err := b.List(ctx); err == nil {
			info["targets"] = tabs
		}
		return info, nil
	case "List":
		return b.List(ctx)
	case "Navigate":
		return b.Navigate(ctx, argStr(args, 0), argStr(args, 1))
	case "Eval":
		return b.Eval(ctx, argStr(args, 0), argStr(args, 1))
	case "Snapshot":
		return b.Snapshot(ctx, argStr(args, 0))
	case "Click":
		return b.Click(ctx, argStr(args, 0), argStr(args, 1), argQ(args, 2))
	case "Type":
		return b.Type(ctx, argStr(args, 0), argStr(args, 1), argStr(args, 2), argQ(args, 3))
	case "HTML":
		return b.HTML(ctx, argStr(args, 0), argStr(args, 1), argBool(args, 2), argQ(args, 3))
	case "Text":
		return b.Text(ctx, argStr(args, 0), argStr(args, 1), argQ(args, 2))
	case "Value":
		return b.Value(ctx, argStr(args, 0), argStr(args, 1), argQ(args, 2))
	case "AttrGet":
		return b.AttrGet(ctx, argStr(args, 0), argStr(args, 1), argStr(args, 2), argQ(args, 3))
	case "AttrList":
		return b.AttrList(ctx, argStr(args, 0), argStr(args, 1), argQ(args, 2))
	case "AttrSet":
		return b.AttrSet(ctx, argStr(args, 0), argStr(args, 1), argStr(args, 2), argStr(args, 3), argQ(args, 4))
	case "AttrRemove":
		return b.AttrRemove(ctx, argStr(args, 0), argStr(args, 1), argStr(args, 2), argQ(args, 3))
	case "SetHeaders":
		return b.SetHeaders(ctx, argStr(args, 0), argMap(args, 1))
	case "EmulateViewport":
		return b.EmulateViewport(ctx, argStr(args, 0), argI64(args, 1), argI64(args, 2))
	case "EmulateGeo":
		return b.EmulateGeo(ctx, argStr(args, 0), argF64(args, 1), argF64(args, 2))
	case "EmulateReset":
		return b.EmulateReset(ctx, argStr(args, 0))
	case "Frames":
		return b.Frames(ctx, argStr(args, 0))
	case "Screenshot":
		return b.Screenshot(ctx, argStr(args, 0))
	case "PDF":
		return b.PDF(ctx, argStr(args, 0))
	case "CookieList":
		return b.CookieList(ctx, argStr(args, 0))
	case "CookieSet":
		return b.CookieSet(ctx, argStr(args, 0), argStr(args, 1), argStr(args, 2), argStr(args, 3), argStr(args, 4))
	case "CookieDelete":
		return b.CookieDelete(ctx, argStr(args, 0), argStr(args, 1))
	case "CookieClear":
		return b.CookieClear(ctx, argStr(args, 0))
	case "Raw":
		return b.Raw(ctx, argStr(args, 0), argStr(args, 1), argRaw(args, 2))
	default:
		return nil, errors.New("unknown method: " + method)
	}
}

// The arg decoders return the zero value for a missing or malformed argument.
// This is safe because both ends of the protocol are the same binary: the
// remoteBrowser marshals exactly the positional args each method expects, so a
// decode can only fail on a genuine internal mismatch, where a zero value (and
// the resulting Chrome-side error) is an acceptable fail-soft.
func argStr(a []json.RawMessage, i int) string {
	var v string
	if i < len(a) {
		_ = json.Unmarshal(a[i], &v)
	}
	return v
}
func argBool(a []json.RawMessage, i int) bool {
	var v bool
	if i < len(a) {
		_ = json.Unmarshal(a[i], &v)
	}
	return v
}
func argI64(a []json.RawMessage, i int) int64 {
	var v int64
	if i < len(a) {
		_ = json.Unmarshal(a[i], &v)
	}
	return v
}
func argF64(a []json.RawMessage, i int) float64 {
	var v float64
	if i < len(a) {
		_ = json.Unmarshal(a[i], &v)
	}
	return v
}
func argQ(a []json.RawMessage, i int) chrome.QueryOpts {
	var v chrome.QueryOpts
	if i < len(a) {
		_ = json.Unmarshal(a[i], &v)
	}
	return v
}
func argMap(a []json.RawMessage, i int) map[string]string {
	var v map[string]string
	if i < len(a) {
		_ = json.Unmarshal(a[i], &v)
	}
	return v
}
func argRaw(a []json.RawMessage, i int) json.RawMessage {
	if i < len(a) {
		return a[i]
	}
	return nil
}

// Client talks to a running daemon over its Unix socket (one connection per call).
type Client struct {
	path string
}

func (c *Client) call(ctx context.Context, method string, out any, args ...any) error {
	raw := make([]json.RawMessage, len(args))
	for i, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return err
		}
		raw[i] = b
	}

	// Hand the daemon our remaining deadline so a slow action fails cleanly
	// there rather than blocking us; a socket deadline (the action deadline plus
	// grace for its response, or a default cap) keeps a wedged daemon from
	// hanging us indefinitely.
	var timeoutMs int64
	sockDeadline := time.Now().Add(30 * time.Second)
	if dl, ok := ctx.Deadline(); ok {
		if timeoutMs = time.Until(dl).Milliseconds(); timeoutMs <= 0 {
			return context.DeadlineExceeded
		}
		sockDeadline = dl.Add(2 * time.Second)
	}

	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(sockDeadline)
	if err := json.NewEncoder(conn).Encode(Request{Method: method, Args: raw, TimeoutMs: timeoutMs}); err != nil {
		return err
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if out != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

// Status pings the daemon; nil error means it's alive.
func (c *Client) Status() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.call(ctx, "__status", nil)
}

// StatusInfo returns the daemon's status payload: {connected, targets}.
func (c *Client) StatusInfo() (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out map[string]any
	return out, c.call(ctx, "__status", &out)
}

// Stop asks the daemon to shut down.
func (c *Client) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.call(ctx, "__stop", nil)
}

// remoteBrowser implements chrome.Browser by RPC to the daemon.
type remoteBrowser struct{ c *Client }

// Remote returns a chrome.Browser backed by the given daemon Client.
func Remote(c *Client) chrome.Browser { return &remoteBrowser{c: c} }

func (r *remoteBrowser) List(ctx context.Context) ([]target.Info, error) {
	var out []target.Info
	return out, r.c.call(ctx, "List", &out)
}
func (r *remoteBrowser) Navigate(ctx context.Context, id, url string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Navigate", &out, id, url)
}
func (r *remoteBrowser) Eval(ctx context.Context, id, expr string) (any, error) {
	var out any
	return out, r.c.call(ctx, "Eval", &out, id, expr)
}
func (r *remoteBrowser) Snapshot(ctx context.Context, id string) (any, error) {
	var out any
	return out, r.c.call(ctx, "Snapshot", &out, id)
}
func (r *remoteBrowser) Click(ctx context.Context, id, sel string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Click", &out, id, sel, q)
}
func (r *remoteBrowser) Type(ctx context.Context, id, sel, text string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Type", &out, id, sel, text, q)
}
func (r *remoteBrowser) HTML(ctx context.Context, id, sel string, inner bool, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "HTML", &out, id, sel, inner, q)
}
func (r *remoteBrowser) Text(ctx context.Context, id, sel string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Text", &out, id, sel, q)
}
func (r *remoteBrowser) Value(ctx context.Context, id, sel string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Value", &out, id, sel, q)
}
func (r *remoteBrowser) AttrGet(ctx context.Context, id, sel, name string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "AttrGet", &out, id, sel, name, q)
}
func (r *remoteBrowser) AttrList(ctx context.Context, id, sel string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "AttrList", &out, id, sel, q)
}
func (r *remoteBrowser) AttrSet(ctx context.Context, id, sel, name, value string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "AttrSet", &out, id, sel, name, value, q)
}
func (r *remoteBrowser) AttrRemove(ctx context.Context, id, sel, name string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "AttrRemove", &out, id, sel, name, q)
}
func (r *remoteBrowser) SetHeaders(ctx context.Context, id string, h map[string]string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "SetHeaders", &out, id, h)
}
func (r *remoteBrowser) EmulateViewport(ctx context.Context, id string, w, h int64) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "EmulateViewport", &out, id, w, h)
}
func (r *remoteBrowser) EmulateGeo(ctx context.Context, id string, lat, lon float64) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "EmulateGeo", &out, id, lat, lon)
}
func (r *remoteBrowser) EmulateReset(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "EmulateReset", &out, id)
}
func (r *remoteBrowser) Frames(ctx context.Context, id string) (any, error) {
	var out any
	return out, r.c.call(ctx, "Frames", &out, id)
}
func (r *remoteBrowser) Screenshot(ctx context.Context, id string) ([]byte, error) {
	var out []byte
	return out, r.c.call(ctx, "Screenshot", &out, id)
}
func (r *remoteBrowser) PDF(ctx context.Context, id string) ([]byte, error) {
	var out []byte
	return out, r.c.call(ctx, "PDF", &out, id)
}
func (r *remoteBrowser) CookieList(ctx context.Context, id string) (any, error) {
	var out any
	return out, r.c.call(ctx, "CookieList", &out, id)
}
func (r *remoteBrowser) CookieSet(ctx context.Context, id, name, value, domain, path string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "CookieSet", &out, id, name, value, domain, path)
}
func (r *remoteBrowser) CookieDelete(ctx context.Context, id, name string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "CookieDelete", &out, id, name)
}
func (r *remoteBrowser) CookieClear(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "CookieClear", &out, id)
}
func (r *remoteBrowser) Raw(ctx context.Context, id, method string, params json.RawMessage) (any, error) {
	var out any
	return out, r.c.call(ctx, "Raw", &out, id, method, params)
}

// Close is a no-op: the shared daemon outlives a single command.
func (r *remoteBrowser) Close() error { return nil }

var _ chrome.Browser = (*remoteBrowser)(nil)
