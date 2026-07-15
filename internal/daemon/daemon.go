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
// ctx) as JSON.
type Request struct {
	Method string            `json:"method"`
	Args   []json.RawMessage `json:"args,omitempty"`
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
}

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
	s.mu.Lock()
	res, err := s.dispatch(context.Background(), req.Method, req.Args)
	s.mu.Unlock()

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
		return map[string]any{"connected": true}, nil
	case "__stop":
		close(s.stopCh)
		return map[string]any{"stopped": true}, nil
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

func (c *Client) call(method string, out any, args ...any) error {
	raw := make([]json.RawMessage, len(args))
	for i, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return err
		}
		raw[i] = b
	}
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(Request{Method: method, Args: raw}); err != nil {
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
func (c *Client) Status() error { return c.call("__status", nil) }

// Stop asks the daemon to shut down.
func (c *Client) Stop() error { return c.call("__stop", nil) }

// remoteBrowser implements chrome.Browser by RPC to the daemon.
type remoteBrowser struct{ c *Client }

// Remote returns a chrome.Browser backed by the daemon at sockPath.
func Remote(c *Client) chrome.Browser { return &remoteBrowser{c: c} }

func (r *remoteBrowser) List(context.Context) ([]target.Info, error) {
	var out []target.Info
	return out, r.c.call("List", &out)
}
func (r *remoteBrowser) Navigate(_ context.Context, id, url string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("Navigate", &out, id, url)
}
func (r *remoteBrowser) Eval(_ context.Context, id, expr string) (any, error) {
	var out any
	return out, r.c.call("Eval", &out, id, expr)
}
func (r *remoteBrowser) Snapshot(_ context.Context, id string) (any, error) {
	var out any
	return out, r.c.call("Snapshot", &out, id)
}
func (r *remoteBrowser) Click(_ context.Context, id, sel string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("Click", &out, id, sel, q)
}
func (r *remoteBrowser) Type(_ context.Context, id, sel, text string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("Type", &out, id, sel, text, q)
}
func (r *remoteBrowser) HTML(_ context.Context, id, sel string, inner bool, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("HTML", &out, id, sel, inner, q)
}
func (r *remoteBrowser) Text(_ context.Context, id, sel string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("Text", &out, id, sel, q)
}
func (r *remoteBrowser) Value(_ context.Context, id, sel string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("Value", &out, id, sel, q)
}
func (r *remoteBrowser) AttrGet(_ context.Context, id, sel, name string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("AttrGet", &out, id, sel, name, q)
}
func (r *remoteBrowser) AttrList(_ context.Context, id, sel string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("AttrList", &out, id, sel, q)
}
func (r *remoteBrowser) AttrSet(_ context.Context, id, sel, name, value string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("AttrSet", &out, id, sel, name, value, q)
}
func (r *remoteBrowser) AttrRemove(_ context.Context, id, sel, name string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("AttrRemove", &out, id, sel, name, q)
}
func (r *remoteBrowser) SetHeaders(_ context.Context, id string, h map[string]string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("SetHeaders", &out, id, h)
}
func (r *remoteBrowser) EmulateViewport(_ context.Context, id string, w, h int64) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("EmulateViewport", &out, id, w, h)
}
func (r *remoteBrowser) EmulateGeo(_ context.Context, id string, lat, lon float64) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("EmulateGeo", &out, id, lat, lon)
}
func (r *remoteBrowser) EmulateReset(_ context.Context, id string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("EmulateReset", &out, id)
}
func (r *remoteBrowser) Frames(_ context.Context, id string) (any, error) {
	var out any
	return out, r.c.call("Frames", &out, id)
}
func (r *remoteBrowser) Screenshot(_ context.Context, id string) ([]byte, error) {
	var out []byte
	return out, r.c.call("Screenshot", &out, id)
}
func (r *remoteBrowser) PDF(_ context.Context, id string) ([]byte, error) {
	var out []byte
	return out, r.c.call("PDF", &out, id)
}
func (r *remoteBrowser) CookieList(_ context.Context, id string) (any, error) {
	var out any
	return out, r.c.call("CookieList", &out, id)
}
func (r *remoteBrowser) CookieSet(_ context.Context, id, name, value, domain, path string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("CookieSet", &out, id, name, value, domain, path)
}
func (r *remoteBrowser) CookieDelete(_ context.Context, id, name string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("CookieDelete", &out, id, name)
}
func (r *remoteBrowser) CookieClear(_ context.Context, id string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call("CookieClear", &out, id)
}
func (r *remoteBrowser) Raw(_ context.Context, id, method string, params json.RawMessage) (any, error) {
	var out any
	return out, r.c.call("Raw", &out, id, method, params)
}

// Close is a no-op: the shared daemon outlives a single command.
func (r *remoteBrowser) Close() error { return nil }

var _ chrome.Browser = (*remoteBrowser)(nil)
