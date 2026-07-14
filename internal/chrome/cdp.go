package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	cdptarget "github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	easyjson "github.com/mailru/easyjson"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// Options controls how CDP connects.
type Options struct {
	PortFile string // override DevToolsActivePort location (else OS candidates)
	NoLaunch bool   // don't fall back to launching a managed Chrome
	Headless bool   // headless for the managed-launch fallback (tests use this)
}

// ConnectError carries a stable error.code (matching the result contract) so the
// CLI can distinguish "not debug-enabled" from a generic connection failure.
type ConnectError struct {
	Code    string
	Message string
}

func (e *ConnectError) Error() string { return e.Message }

// CDP is the chromedp-backed Browser.
//
// The allocator/base/tab contexts are rooted at context.Background(), NOT at a
// per-command context — otherwise a command's deferred cancel would tear the
// whole tree down and (in attach mode) CloseTarget the user's real tab. Tabs are
// attached once and reused; per-command timeouts are applied as short-lived
// child contexts around each action (see run). The shared daemon is the eventual
// home for this attach-and-hold model.
type CDP struct {
	managed     bool
	alloc       context.Context
	allocCancel context.CancelFunc
	base        context.Context
	baseCancel  context.CancelFunc

	mu   sync.Mutex
	tabs map[string]tabConn
}

// tabConn is a cached per-tab context and its cancel func.
type tabConn struct {
	ctx  context.Context
	stop context.CancelFunc
}

func newCDP(managed bool, alloc context.Context, allocCancel context.CancelFunc, base context.Context, baseCancel context.CancelFunc) *CDP {
	return &CDP{
		managed: managed, alloc: alloc, allocCancel: allocCancel, base: base, baseCancel: baseCancel,
		tabs: map[string]tabConn{},
	}
}

// Connect walks the connection ladder (mirroring browser.DecideConnection):
//   - a reachable DevToolsActivePort endpoint -> attach (Path B)
//   - a running but non-debug Chrome          -> ConnectError{not_debug_enabled}
//     (never shadow the user's session with a second browser)
//   - nothing running, --no-launch            -> ConnectError{connection_failed}
//   - nothing running                         -> launch a managed Chrome (Path A)
//
// The ctx is intentionally not used to root the browser: the allocator lives at
// context.Background() so a command's deferred cancel can never tear down (and,
// in attach mode, CloseTarget) the user's real tabs. The decision (attach /
// instruct / launch) is delegated to the tested browser.DecideConnection so
// there is exactly one authored copy of the ladder.
func Connect(_ context.Context, opts Options) (*CDP, error) {
	var portFileWS string
	if pf := browser.FindPortFile(opts.PortFile); pf != "" {
		if ws, err := browser.WSURLFromPortFile(pf); err == nil {
			portFileWS = ws
		}
	}
	probe := browser.Probe{
		PortFileWS:    portFileWS,
		WSReachable:   portFileWS != "" && Reachable(portFileWS),
		ChromeRunning: chromeRunning(),
		NoLaunch:      opts.NoLaunch,
	}
	switch browser.DecideConnection(probe) {
	case browser.Attach:
		return attach(portFileWS)
	case browser.InstructToggle:
		return nil, &ConnectError{
			Code:    "not_debug_enabled",
			Message: "Chrome is running but not debug-enabled — open chrome://inspect/#remote-debugging and toggle it on (chrome-cdp will not open a second browser over your session)",
		}
	case browser.InstructNoLaunch:
		return nil, &ConnectError{
			Code:    "connection_failed",
			Message: "no debug-enabled Chrome found and --no-launch is set — enable chrome://inspect/#remote-debugging or drop --no-launch",
		}
	default: // Launch
		return launch(opts.Headless)
	}
}

func attach(ws string) (*CDP, error) {
	alloc, allocCancel := chromedp.NewRemoteAllocator(context.Background(), ws)
	return startBase(false, alloc, allocCancel, "attach to "+ws)
}

func launch(headless bool) (*CDP, error) {
	execOpts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	if !headless {
		execOpts = append(execOpts, chromedp.Flag("headless", false))
	}
	alloc, allocCancel := chromedp.NewExecAllocator(context.Background(), execOpts...)
	return startBase(true, alloc, allocCancel, "launch managed Chrome")
}

// startBase creates the base context on an allocator and initializes the browser
// connection. The init runs on base directly (not a cancellable child), so the
// browser's lifetime is not tied to a context that gets cancelled.
func startBase(managed bool, alloc context.Context, allocCancel context.CancelFunc, what string) (*CDP, error) {
	base, baseCancel := chromedp.NewContext(alloc)
	if err := chromedp.Run(base); err != nil {
		baseCancel()
		allocCancel()
		return nil, &ConnectError{Code: "connection_failed", Message: fmt.Sprintf("%s: %v", what, err)}
	}
	return newCDP(managed, alloc, allocCancel, base, baseCancel), nil
}

// chromeRunning best-effort detects an already-running Chrome (so we instruct
// the toggle instead of shadowing the user's session with a managed browser).
func chromeRunning() bool {
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "Google Chrome"
	case "linux":
		name = "chrome"
	default:
		return false
	}
	return exec.Command("pgrep", "-x", name).Run() == nil
}

// deadline returns a child of tab carrying src's deadline (if any). Cancelling
// it never closes the tab — only chromedp's own NewContext contexts do that.
func deadline(src, tab context.Context) (context.Context, context.CancelFunc) {
	if dl, ok := src.Deadline(); ok {
		return context.WithDeadline(tab, dl)
	}
	return context.WithCancel(tab)
}

// Close tears down the connection. Managed Chrome is fully closed. An attached
// real Chrome is left alone — cancelling any context would CloseTarget a real
// tab, so we let the WebSocket drop with the process instead.
func (c *CDP) Close() error {
	if !c.managed {
		return nil
	}
	c.mu.Lock()
	for _, t := range c.tabs {
		t.stop()
	}
	c.mu.Unlock()
	c.baseCancel()
	c.allocCancel()
	return nil
}

func (c *CDP) List(_ context.Context) ([]target.Info, error) {
	infos, err := chromedp.Targets(c.base)
	if err != nil {
		return nil, err
	}
	var out []target.Info
	for _, i := range infos {
		if i.Type != "page" || strings.HasPrefix(i.URL, "chrome://") {
			continue
		}
		out = append(out, target.Info{ID: i.TargetID.String(), Title: i.Title, URL: i.URL})
	}
	return out, nil
}

// on returns a context bound to a target, attached once and cached, parented on
// base (which holds the established Browser). The attach runs on the tab context
// itself (long-lived) so the session's event loop isn't tied to a per-action
// deadline. The context is cancelled only in managed mode (see Close), so the
// user's real tabs are never closed in attach mode.
func (c *CDP) on(id string) (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tabs[id]; ok {
		return t.ctx, nil
	}
	tctx, cancel := chromedp.NewContext(c.base, chromedp.WithTargetID(cdptarget.ID(id)))
	if err := chromedp.Run(tctx); err != nil { // attach once, tied to tctx
		cancel()
		return nil, err
	}
	c.tabs[id] = tabConn{ctx: tctx, stop: cancel}
	return tctx, nil
}

// run executes actions against a target under the caller's deadline. The tab is
// attached once (in on); the deadline child bounds the action without detaching.
func (c *CDP) run(ctx context.Context, id string, actions ...chromedp.Action) error {
	tctx, err := c.on(id)
	if err != nil {
		return err
	}
	rc, cancel := deadline(ctx, tctx)
	defer cancel()
	return chromedp.Run(rc, actions...)
}

func (c *CDP) Navigate(ctx context.Context, id, url string) (map[string]any, error) {
	var loc string
	if err := c.run(ctx, id, chromedp.Navigate(url), chromedp.Location(&loc)); err != nil {
		return nil, err
	}
	return map[string]any{"url": loc}, nil
}

func (c *CDP) Eval(ctx context.Context, id, expr string) (any, error) {
	var res json.RawMessage
	if err := c.run(ctx, id, chromedp.Evaluate(expr, &res)); err != nil {
		return nil, err
	}
	var v any
	_ = json.Unmarshal(res, &v)
	return map[string]any{"value": v}, nil
}

func (c *CDP) Snapshot(ctx context.Context, id string) (any, error) {
	var nodes []*accessibility.Node
	err := c.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
		var e error
		nodes, e = accessibility.GetFullAXTree().Do(actx)
		return e
	}))
	if err != nil {
		return nil, err
	}
	type axNode struct {
		Role string `json:"role"`
		Name string `json:"name,omitempty"`
	}
	out := make([]axNode, 0, len(nodes))
	for _, n := range nodes {
		role, name := axString(n.Role), axString(n.Name)
		if role == "" && name == "" {
			continue
		}
		out = append(out, axNode{Role: role, Name: name})
	}
	return map[string]any{"nodes": out}, nil
}

// axString decodes an accessibility Value (a raw JSON value) to a string.
func axString(v *accessibility.Value) string {
	if v == nil || len(v.Value) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(v.Value), &s); err == nil {
		return s
	}
	return string(v.Value)
}

func (c *CDP) Click(ctx context.Context, id, selector string) (map[string]any, error) {
	if err := c.run(ctx, id, chromedp.Click(selector, chromedp.NodeVisible)); err != nil {
		return nil, err
	}
	return map[string]any{"clicked": selector}, nil
}

func (c *CDP) Type(ctx context.Context, id, selector, text string) (map[string]any, error) {
	if err := c.run(ctx, id, chromedp.SendKeys(selector, text, chromedp.NodeVisible)); err != nil {
		return nil, err
	}
	return map[string]any{"typed": selector}, nil
}

func (c *CDP) Screenshot(ctx context.Context, id, outPath string) (map[string]any, error) {
	var buf []byte
	if err := c.run(ctx, id, chromedp.CaptureScreenshot(&buf)); err != nil {
		return nil, err
	}
	if err := os.WriteFile(outPath, buf, 0o644); err != nil {
		return nil, err
	}
	return map[string]any{"path": outPath, "bytes": len(buf)}, nil
}

// Raw sends any CDP method by string via the executor — full coverage, no
// per-method registry. An empty id targets the browser-level executor, so
// Browser.* / Target.* methods are reachable.
func (c *CDP) Raw(ctx context.Context, id, method string, params json.RawMessage) (any, error) {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	var res easyjson.RawMessage
	exec := func(actx context.Context) error {
		return cdp.Execute(actx, method, easyjson.RawMessage(params), &res)
	}
	var err error
	if id == "" {
		rc, cancel := deadline(ctx, c.base)
		defer cancel()
		err = chromedp.Run(rc, chromedp.ActionFunc(func(actx context.Context) error {
			cc := chromedp.FromContext(actx)
			return exec(cdp.WithExecutor(actx, cc.Browser))
		}))
	} else {
		err = c.run(ctx, id, chromedp.ActionFunc(exec))
	}
	if err != nil {
		return nil, err
	}
	var v any
	_ = json.Unmarshal(res, &v)
	return v, nil
}

// Reachable reports whether the loopback debug port is actually listening
// (used by `doctor` so a stale port file isn't reported as ready).
func Reachable(wsURL string) bool {
	hostport, ok := browser.HostPort(wsURL)
	if !ok {
		return false
	}
	conn, err := net.DialTimeout("tcp", hostport, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
