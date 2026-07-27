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
	"io"
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
//
// A unary call answers with exactly one Response. A STREAMING call (see
// streamDispatch) answers with one Response per emitted value followed by a
// terminator carrying Done, so the client knows the stream ended rather than
// guessing from a closed socket.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Done   bool            `json:"done,omitempty"`
}

// server holds the real Browser and serializes dispatch (chromedp calls are not
// meant to run fully concurrently against one connection).
type server struct {
	b        chrome.Browser
	mu       sync.Mutex
	activity chan struct{} // activity pings, to reset the idle timer
	stopCh   chan struct{}
	stopOnce sync.Once

	// pingEvery is how often a live stream reports itself to the idle timer.
	// Zero means defaultStreamPing; a test sets it so it can assert on the
	// behaviour without waiting out a production interval.
	pingEvery time.Duration
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
	// A STREAMING method runs for the client's whole --follow window, so it must
	// NOT hold the mutex that serialises unary calls: holding it made
	// `console --follow` in one terminal wedge `click` in another for the full
	// timeout, which defeats the user story the feature exists for (RFC-0002
	// US-2, "watch console output WHILE I exercise the page").
	//
	// Dropping the mutex here is safe because a stream is not the kind of thing
	// the mutex protects. It exists so multi-step chromedp action sequences on
	// one connection do not interleave; a stream issues one idempotent domain
	// enable, then only reads event buffers that hold their own locks. The
	// attach it may trigger is serialised by the CDP object's own mutex, and
	// chromedp targets are safe for concurrent use. See streamDispatch.
	if isStreamMethod(req.Method) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		// A --follow stream is the one call that can outlive the idle window,
		// and the one that has to notice its client leaving.
		go s.pingWhile(ctx)
		go watchHangup(ctx, conn, cancel)
		_, serr := s.streamDispatch(ctx, conn, req.Method, req.Args)
		finishStream(conn, serr)
		return
	}
	s.mu.Lock()
	res, err := s.dispatch(ctx, req.Method, req.Args)
	s.mu.Unlock()
	reply(conn, res, err)
}

// defaultStreamPing is how often a live stream reports itself to the idle timer.
// Well under any sane idle window, so a long --follow cannot have the listener
// closed from under it: the client would see EOF, the stream would return nil,
// and the command would exit 0 with no indication it had been cut short.
const defaultStreamPing = 30 * time.Second

// pingWhile keeps the idle timer alive for as long as a stream is running. The
// timer is otherwise reset only on Accept, which a stream does exactly once.
func (s *server) pingWhile(ctx context.Context) {
	every := s.pingEvery
	if every <= 0 {
		every = defaultStreamPing
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ping()
		}
	}
}

// watchHangup cancels a stream when its client goes away.
//
// The protocol is one request per connection, so the client never writes again:
// any read that completes means the far end closed. Without this a Ctrl-C'd
// --follow is noticed only when the daemon next WRITES to the socket — which on
// a quiet page is never, so the stream ran for the client's whole TimeoutMs
// after nobody was left to read it.
func watchHangup(ctx context.Context, conn net.Conn, cancel context.CancelFunc) {
	defer cancel()
	buf := make([]byte, 1)
	for {
		if _, err := conn.Read(buf); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// isStreamMethod reports whether a method is served by streamDispatch. It is the
// same list, kept next to it deliberately: a method that streams but is not
// named here would take the dispatch mutex for its whole window.
func isStreamMethod(method string) bool {
	switch method {
	case "ConsoleStream", "NetStream":
		return true
	}
	return false
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

// streamDispatch routes the STREAMING Browser methods — the ones that emit many
// values over the life of one call, which the unary one-request/one-response
// protocol cannot carry because the emit callback does not cross the socket.
// Each emitted value is written as its own Response on the same connection;
// finishStream writes the terminator.
//
// It runs WITHOUT the dispatch mutex (see handle) and must stay that way: it is
// the only dispatch path whose duration is the caller's choice rather than the
// action's.
//
// It reports false for anything that is not a streaming method; isStreamMethod
// is the same list, and the two must agree.
func (s *server) streamDispatch(ctx context.Context, conn net.Conn, method string, args []json.RawMessage) (bool, error) {
	enc := json.NewEncoder(conn)
	emit := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return enc.Encode(Response{Result: b})
	}
	switch method {
	case "ConsoleStream":
		return true, s.b.ConsoleStream(ctx, argStr(args, 0), argConsole(args, 1), emit)
	case "NetStream":
		return true, s.b.NetStream(ctx, argStr(args, 0), argNet(args, 1), emit)
	}
	return false, nil
}

// finishStream terminates a streaming response, carrying the error the stream
// ended with (if any).
func finishStream(conn net.Conn, err error) {
	resp := Response{Done: true}
	if err != nil {
		resp.Error = err.Error()
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
	case "Open":
		return b.Open(ctx, argStr(args, 0))
	case "Navigate":
		return b.Navigate(ctx, argStr(args, 0), argStr(args, 1))
	case "CloseTabs":
		return b.CloseTabs(ctx, argStrs(args, 0))
	case "Activate":
		return b.Activate(ctx, argStr(args, 0))
	case "History":
		return b.History(ctx, argStr(args, 0), argInt(args, 1))
	case "Reload":
		return b.Reload(ctx, argStr(args, 0), argBool(args, 1))
	case "Eval":
		return b.Eval(ctx, argStr(args, 0), argStr(args, 1), argEval(args, 2))
	case "Snapshot":
		return b.Snapshot(ctx, argStr(args, 0), argSnap(args, 1))
	case "Key":
		return b.Key(ctx, argStr(args, 0), argStr(args, 1), argKeys(args, 2), argKeyOpts(args, 3))
	case "Pointer":
		return b.Pointer(ctx, argStr(args, 0), argStr(args, 1), argPointer(args, 2))
	case "Select":
		return b.Select(ctx, argStr(args, 0), argStr(args, 1), argStr(args, 2), argSel(args, 3))
	case "Fill":
		return b.Fill(ctx, argStr(args, 0), argStr(args, 1), argStr(args, 2), argQ(args, 3))
	case "Upload":
		return b.Upload(ctx, argStr(args, 0), argStr(args, 1), argStrs(args, 2), argUpload(args, 3))
	case "Values":
		return b.Values(ctx, argStr(args, 0), argStr(args, 1), argQ(args, 2))
	case "Grid":
		return b.Grid(ctx, argStr(args, 0), argStr(args, 1), argQ(args, 2))
	case "Scroll":
		return b.Scroll(ctx, argStr(args, 0), argStr(args, 1), argScroll(args, 2))
	case "Type":
		return b.Type(ctx, argStr(args, 0), argStr(args, 1), argStr(args, 2), argQ(args, 3))
	case "HTML":
		return b.HTML(ctx, argStr(args, 0), argStr(args, 1), argBool(args, 2), argQ(args, 3))
	case "Text":
		return b.Text(ctx, argStr(args, 0), argStr(args, 1), argText(args, 2))
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
	case "Wait":
		return b.Wait(ctx, argStr(args, 0), argWait(args, 1))
	case "Console":
		return b.Console(ctx, argStr(args, 0), argConsole(args, 1))
	case "ConsoleStream":
		// Reachable over the RPC, but via streamDispatch (many responses on one
		// connection) — a unary call has nowhere to put the emitted values.
		return nil, errors.New("ConsoleStream is a streaming method: call it over the streaming RPC path")
	case "Net":
		return b.Net(ctx, argStr(args, 0), argNet(args, 1))
	case "NetWait":
		return b.NetWait(ctx, argStr(args, 0), argNetCond(args, 1))
	case "NetStream":
		// Reachable over the RPC, but via streamDispatch — a unary call has
		// nowhere to put the emitted values.
		return nil, errors.New("NetStream is a streaming method: call it over the streaming RPC path")
	case "Screenshot":
		return capture(b.Screenshot(ctx, argStr(args, 0), argShot(args, 1)))
	case "RecordStart":
		return b.RecordStart(ctx, argStr(args, 0), argRecord(args, 1))
	case "RecordStop":
		frames, meta, err := b.RecordStop(ctx, argStr(args, 0))
		if err != nil {
			return nil, err
		}
		return recordResult{Frames: frames, Meta: meta}, nil
	case "RecordRestore":
		return nil, b.RecordRestore(ctx, argStr(args, 0), argFrames(args, 1), argAnyMap(args, 2))
	case "RecordStatus":
		return b.RecordStatus(ctx, argStr(args, 0))
	case "RecordCancel":
		return b.RecordCancel(ctx, argStr(args, 0))
	case "PDF":
		return capture(b.PDF(ctx, argStr(args, 0), argPDF(args, 1)))
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

// captureResult carries a capture's two return values over the one-result RPC.
// The bytes ride as base64 (encoding/json's []byte handling) and the metadata as
// a plain object, so the forwarder can hand both back unchanged.
type captureResult struct {
	Data []byte         `json:"data,omitempty"`
	Meta map[string]any `json:"meta,omitempty"`
}

// capture adapts a (bytes, meta, error) driver return into the single value
// dispatch replies with, so both capture cases stay one line.
func capture(data []byte, meta map[string]any, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return captureResult{Data: data, Meta: meta}, nil
}

// recordResult carries RecordStop's two return values over the one-result RPC.
// The frames ride as an array of objects whose JPEG bytes encoding/json renders
// as base64 — the same treatment captureResult gives a screenshot, for the same
// reason: the forwarder has to hand both halves back unchanged.
type recordResult struct {
	Frames []chrome.Frame `json:"frames,omitempty"`
	Meta   map[string]any `json:"meta,omitempty"`
}

// arg decodes the i'th positional argument, yielding the zero value when it is
// missing or malformed. That fail-soft is safe because both ends of the protocol
// are the same binary: remoteBrowser marshals exactly the positional args each
// method expects, so a decode can only fail on a genuine internal mismatch,
// where a zero value (and the resulting Chrome-side error) is acceptable. It is
// NOT a substitute for validating user input, which happens in the CLI before
// the RPC is ever made.
func arg[T any](a []json.RawMessage, i int) T {
	var v T
	if i < len(a) {
		_ = json.Unmarshal(a[i], &v)
	}
	return v
}

func argStr(a []json.RawMessage, i int) string    { return arg[string](a, i) }
func argBool(a []json.RawMessage, i int) bool     { return arg[bool](a, i) }
func argI64(a []json.RawMessage, i int) int64     { return arg[int64](a, i) }
func argInt(a []json.RawMessage, i int) int       { return arg[int](a, i) }
func argF64(a []json.RawMessage, i int) float64   { return arg[float64](a, i) }
func argStrs(a []json.RawMessage, i int) []string { return arg[[]string](a, i) }

func argQ(a []json.RawMessage, i int) chrome.QueryOpts         { return arg[chrome.QueryOpts](a, i) }
func argText(a []json.RawMessage, i int) chrome.TextOpts       { return arg[chrome.TextOpts](a, i) }
func argEval(a []json.RawMessage, i int) chrome.EvalOpts       { return arg[chrome.EvalOpts](a, i) }
func argWait(a []json.RawMessage, i int) chrome.WaitCond       { return arg[chrome.WaitCond](a, i) }
func argSel(a []json.RawMessage, i int) chrome.SelectOpts      { return arg[chrome.SelectOpts](a, i) }
func argScroll(a []json.RawMessage, i int) chrome.ScrollOpts   { return arg[chrome.ScrollOpts](a, i) }
func argSnap(a []json.RawMessage, i int) chrome.SnapOpts       { return arg[chrome.SnapOpts](a, i) }
func argKeys(a []json.RawMessage, i int) []chrome.KeyStroke    { return arg[[]chrome.KeyStroke](a, i) }
func argKeyOpts(a []json.RawMessage, i int) chrome.KeyOpts     { return arg[chrome.KeyOpts](a, i) }
func argPointer(a []json.RawMessage, i int) chrome.PointerOpts { return arg[chrome.PointerOpts](a, i) }
func argShot(a []json.RawMessage, i int) chrome.ShotOpts       { return arg[chrome.ShotOpts](a, i) }
func argPDF(a []json.RawMessage, i int) chrome.PDFOpts         { return arg[chrome.PDFOpts](a, i) }
func argUpload(a []json.RawMessage, i int) chrome.UploadOpts   { return arg[chrome.UploadOpts](a, i) }
func argRecord(a []json.RawMessage, i int) chrome.RecordOpts   { return arg[chrome.RecordOpts](a, i) }
func argFrames(a []json.RawMessage, i int) []chrome.Frame      { return arg[[]chrome.Frame](a, i) }
func argAnyMap(a []json.RawMessage, i int) map[string]any      { return arg[map[string]any](a, i) }
func argMap(a []json.RawMessage, i int) map[string]string      { return arg[map[string]string](a, i) }
func argConsole(a []json.RawMessage, i int) chrome.ConsoleOpts { return arg[chrome.ConsoleOpts](a, i) }
func argNet(a []json.RawMessage, i int) chrome.NetOpts         { return arg[chrome.NetOpts](a, i) }
func argNetCond(a []json.RawMessage, i int) chrome.NetCond     { return arg[chrome.NetCond](a, i) }

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
	conn, err := c.dial(ctx, method, args)
	if err != nil {
		return err
	}
	defer conn.Close()
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

// dial opens a connection for one call, sends the request, and returns the
// connection to read the response(s) from. It also hands the daemon our
// remaining deadline (so a slow action fails there rather than blocking us) and
// sets a socket deadline (so a wedged daemon cannot hang us indefinitely).
func (c *Client) dial(ctx context.Context, method string, args []any) (net.Conn, error) {
	raw := make([]json.RawMessage, len(args))
	for i, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		raw[i] = b
	}

	var timeoutMs int64
	sockDeadline := time.Now().Add(30 * time.Second)
	if dl, ok := ctx.Deadline(); ok {
		if timeoutMs = time.Until(dl).Milliseconds(); timeoutMs <= 0 {
			return nil, context.DeadlineExceeded
		}
		sockDeadline = dl.Add(2 * time.Second)
	}

	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(sockDeadline)
	if err := json.NewEncoder(conn).Encode(Request{Method: method, Args: raw, TimeoutMs: timeoutMs}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// stream calls a streaming method, handing each emitted value to onValue as it
// arrives. It returns when the daemon writes the terminator, when the stream
// errors, or when the connection ends — a closed connection after a clean run
// is the end of the stream, not a failure.
func (c *Client) stream(ctx context.Context, method string, onValue func(json.RawMessage) error, args ...any) error {
	conn, err := c.dial(ctx, method, args)
	if err != nil {
		return err
	}
	defer conn.Close()

	dec := json.NewDecoder(conn)
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if resp.Error != "" {
			return errors.New(resp.Error)
		}
		if len(resp.Result) > 0 {
			if err := onValue(resp.Result); err != nil {
				return err
			}
		}
		if resp.Done {
			return nil
		}
	}
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
func (r *remoteBrowser) Open(ctx context.Context, url string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Open", &out, url)
}
func (r *remoteBrowser) Navigate(ctx context.Context, id, url string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Navigate", &out, id, url)
}
func (r *remoteBrowser) CloseTabs(ctx context.Context, ids []string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "CloseTabs", &out, ids)
}
func (r *remoteBrowser) Activate(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Activate", &out, id)
}
func (r *remoteBrowser) History(ctx context.Context, id string, delta int) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "History", &out, id, delta)
}
func (r *remoteBrowser) Reload(ctx context.Context, id string, hard bool) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Reload", &out, id, hard)
}
func (r *remoteBrowser) Eval(ctx context.Context, id, expr string, opts chrome.EvalOpts) (any, error) {
	var out any
	return out, r.c.call(ctx, "Eval", &out, id, expr, opts)
}
func (r *remoteBrowser) Snapshot(ctx context.Context, id string, opts chrome.SnapOpts) (any, error) {
	var out any
	return out, r.c.call(ctx, "Snapshot", &out, id, opts)
}
func (r *remoteBrowser) Key(ctx context.Context, id, sel string, keys []chrome.KeyStroke, opts chrome.KeyOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Key", &out, id, sel, keys, opts)
}
func (r *remoteBrowser) Pointer(ctx context.Context, id, sel string, opts chrome.PointerOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Pointer", &out, id, sel, opts)
}
func (r *remoteBrowser) Type(ctx context.Context, id, sel, text string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Type", &out, id, sel, text, q)
}
func (r *remoteBrowser) Select(ctx context.Context, id, field, option string, opts chrome.SelectOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Select", &out, id, field, option, opts)
}
func (r *remoteBrowser) Fill(ctx context.Context, id, selector, value string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Fill", &out, id, selector, value, q)
}
func (r *remoteBrowser) Upload(ctx context.Context, id, selector string, paths []string, opts chrome.UploadOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Upload", &out, id, selector, paths, opts)
}
func (r *remoteBrowser) Values(ctx context.Context, id, selector string, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Values", &out, id, selector, q)
}
func (r *remoteBrowser) Grid(ctx context.Context, id, selector string, q chrome.QueryOpts) (any, error) {
	var out any
	return out, r.c.call(ctx, "Grid", &out, id, selector, q)
}
func (r *remoteBrowser) Scroll(ctx context.Context, id, selector string, opts chrome.ScrollOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Scroll", &out, id, selector, opts)
}
func (r *remoteBrowser) HTML(ctx context.Context, id, sel string, inner bool, q chrome.QueryOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "HTML", &out, id, sel, inner, q)
}
func (r *remoteBrowser) Text(ctx context.Context, id, sel string, opts chrome.TextOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Text", &out, id, sel, opts)
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
func (r *remoteBrowser) Wait(ctx context.Context, id string, cond chrome.WaitCond) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "Wait", &out, id, cond)
}
func (r *remoteBrowser) Screenshot(ctx context.Context, id string, opts chrome.ShotOpts) ([]byte, map[string]any, error) {
	var out captureResult
	err := r.c.call(ctx, "Screenshot", &out, id, opts)
	return out.Data, out.Meta, err
}
func (r *remoteBrowser) RecordStart(ctx context.Context, id string, opts chrome.RecordOpts) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "RecordStart", &out, id, opts)
}
func (r *remoteBrowser) RecordStop(ctx context.Context, id string) ([]chrome.Frame, map[string]any, error) {
	var out recordResult
	err := r.c.call(ctx, "RecordStop", &out, id)
	return out.Frames, out.Meta, err
}
func (r *remoteBrowser) RecordRestore(ctx context.Context, id string, frames []chrome.Frame, meta map[string]any) error {
	var out any
	return r.c.call(ctx, "RecordRestore", &out, id, frames, meta)
}
func (r *remoteBrowser) RecordStatus(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "RecordStatus", &out, id)
}
func (r *remoteBrowser) RecordCancel(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "RecordCancel", &out, id)
}
func (r *remoteBrowser) Console(ctx context.Context, id string, opts chrome.ConsoleOpts) (any, error) {
	var out any
	return out, r.c.call(ctx, "Console", &out, id, opts)
}
func (r *remoteBrowser) ConsoleStream(ctx context.Context, id string, opts chrome.ConsoleOpts, emit func(any) error) error {
	return r.c.stream(ctx, "ConsoleStream", func(raw json.RawMessage) error {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		return emit(v)
	}, id, opts)
}
func (r *remoteBrowser) Net(ctx context.Context, id string, opts chrome.NetOpts) (any, error) {
	var out any
	return out, r.c.call(ctx, "Net", &out, id, opts)
}
func (r *remoteBrowser) NetStream(ctx context.Context, id string, opts chrome.NetOpts, emit func(any) error) error {
	return r.c.stream(ctx, "NetStream", func(raw json.RawMessage) error {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		return emit(v)
	}, id, opts)
}
func (r *remoteBrowser) NetWait(ctx context.Context, id string, cond chrome.NetCond) (map[string]any, error) {
	var out map[string]any
	return out, r.c.call(ctx, "NetWait", &out, id, cond)
}
func (r *remoteBrowser) PDF(ctx context.Context, id string, opts chrome.PDFOpts) ([]byte, map[string]any, error) {
	var out captureResult
	err := r.c.call(ctx, "PDF", &out, id, opts)
	return out.Data, out.Meta, err
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
