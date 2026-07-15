package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdptarget "github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	easyjson "github.com/mailru/easyjson"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// Options controls how CDP connects.
type Options struct {
	PortFile   string // override DevToolsActivePort location (else OS candidates)
	ProfileDir string // managed-launch profile dir (else CHROME_CDP_PROFILE / default)
	Port       int    // explicit debug port to attach to / launch with (0 = auto)
	NoLaunch   bool   // don't fall back to launching a managed Chrome
	Headless   bool   // headless for the managed-launch fallback (tests use this)
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
	// An explicit --port takes precedence over the DevToolsActivePort file.
	var endpoint string
	if opts.Port != 0 {
		endpoint = fmt.Sprintf("http://127.0.0.1:%d", opts.Port)
	} else if pf := browser.FindPortFile(opts.PortFile); pf != "" {
		if ws, err := browser.WSURLFromPortFile(pf); err == nil {
			endpoint = ws
		}
	}
	probe := browser.Probe{
		PortFileWS:    endpoint,
		WSReachable:   endpoint != "" && Reachable(endpoint),
		ChromeRunning: chromeRunning(),
		NoLaunch:      opts.NoLaunch,
	}
	switch browser.DecideConnection(probe) {
	case browser.Attach:
		return attach(endpoint)
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
		return launch(opts.Headless, opts.ProfileDir, opts.Port)
	}
}

func attach(ws string) (*CDP, error) {
	alloc, allocCancel := chromedp.NewRemoteAllocator(context.Background(), ws)
	return startBase(false, alloc, allocCancel, "attach to "+ws)
}

func launch(headless bool, profileDir string, port int) (*CDP, error) {
	// A dedicated, persistent profile so managed-Chrome logins survive across
	// runs (rather than chromedp's default ephemeral temp dir).
	dir := resolveProfileDir(profileDir)
	// Best-effort: if the dir can't be created, the launch below fails clearly.
	_ = os.MkdirAll(dir, 0o700)

	execOpts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	execOpts = append(execOpts, chromedp.UserDataDir(dir))
	if port != 0 {
		execOpts = append(execOpts, chromedp.Flag("remote-debugging-port", strconv.Itoa(port)))
	}
	if !headless {
		execOpts = append(execOpts, chromedp.Flag("headless", false))
	}
	alloc, allocCancel := chromedp.NewExecAllocator(context.Background(), execOpts...)
	return startBase(true, alloc, allocCancel, "launch managed Chrome")
}

// resolveProfileDir picks the managed-launch profile: an explicit dir, else
// $CHROME_CDP_PROFILE, else <cache>/chrome-cdp/profile.
func resolveProfileDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("CHROME_CDP_PROFILE"); env != "" {
		return env
	}
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "chrome-cdp", "profile")
}

// startBase creates the base context on an allocator and initializes the browser
// connection. The init runs on base directly (not a cancellable child), so the
// browser's lifetime is not tied to a context that gets cancelled.
func startBase(managed bool, alloc context.Context, allocCancel context.CancelFunc, what string) (*CDP, error) {
	base, baseCancel := chromedp.NewContext(alloc)
	if err := chromedp.Run(base); err != nil {
		baseCancel()
		allocCancel()
		return nil, &ConnectError{Code: "connection_failed", Message: connectFailMsg(managed, what, err)}
	}
	return newCDP(managed, alloc, allocCancel, base, baseCancel), nil
}

// connectFailMsg turns a raw allocator/dial failure into an actionable message.
// The common attach case — "could not dial … deadline exceeded" — is almost
// always Chrome holding a pending "Allow remote debugging?" consent prompt (or a
// wedged DevTools endpoint), which a bare deadline error doesn't explain.
func connectFailMsg(managed bool, what string, err error) string {
	s := err.Error()
	if !managed && (strings.Contains(s, "could not dial") || strings.Contains(s, "deadline exceeded")) {
		return "cannot reach Chrome's debug endpoint — if Chrome is showing an \"Allow remote debugging?\" prompt, click Allow (it can be behind the window), then retry; if it stays unresponsive the endpoint is wedged: quit and reopen Chrome, re-enable chrome://inspect/#remote-debugging, and keep the daemon running so the consent is asked once, not per command"
	}
	return fmt.Sprintf("%s: %v", what, err)
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
		if i.Type != "page" { // page targets only (incl. chrome:// pages); skip iframes/workers
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
		Ref    string   `json:"ref,omitempty"`
		Role   string   `json:"role"`
		Name   string   `json:"name,omitempty"`
		Value  string   `json:"value,omitempty"`
		States []string `json:"states,omitempty"`
	}
	byID := make(map[accessibility.NodeID]*accessibility.Node, len(nodes))
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	out := make([]axNode, 0, len(nodes))
	var alerts []string        // aria-live / role=alert|status text — the toasts/notifications
	var focused map[string]any // the currently-focused element
	for _, n := range nodes {
		role, name := axString(n.Role), axString(n.Name)
		// A live region's text is usually in child StaticText nodes, not its own
		// name — walk the subtree so toasts ("Success! Event approved") surface.
		if role == "alert" || role == "status" || axLive(n) {
			if txt := axSubtreeText(byID, n); txt != "" {
				alerts = append(alerts, txt)
			}
		}
		if focused == nil && axHasState(n, "focused") {
			focused = map[string]any{"role": role, "name": name}
		}
		if role == "" && name == "" {
			continue
		}
		// A stable element ref (the CDP backend node id) that `--by ref` resolves
		// without re-querying by name — the same node keeps the same ref across
		// snaps for the document's lifetime.
		var ref string
		if n.BackendDOMNodeID != 0 {
			ref = fmt.Sprintf("e%d", n.BackendDOMNodeID)
		}
		out = append(out, axNode{Ref: ref, Role: role, Name: name, Value: axString(n.Value), States: axStates(n)})
	}
	res := map[string]any{"nodes": out}
	if len(alerts) > 0 {
		res["alerts"] = alerts
	}
	if focused != nil {
		res["focused"] = focused
	}
	return res, nil
}

// axStates returns the active ARIA states of a node (focused, expanded, checked,
// selected, disabled, required, pressed) — so a caller sees widget state without
// a screenshot.
func axStates(n *accessibility.Node) []string {
	var s []string
	for _, p := range n.Properties {
		switch p.Name {
		case "focused", "expanded", "checked", "selected", "disabled", "required", "pressed":
			if v := axString(p.Value); v != "" && v != "false" {
				s = append(s, string(p.Name))
			}
		}
	}
	return s
}

// axSubtreeText returns a node's accessible name, or (when it has none, as with
// a live-region container) the joined unique names of its descendants.
func axSubtreeText(byID map[accessibility.NodeID]*accessibility.Node, n *accessibility.Node) string {
	if nm := strings.TrimSpace(axString(n.Name)); nm != "" {
		return nm
	}
	var parts []string
	seen := map[string]bool{}
	var walk func(id accessibility.NodeID)
	walk = func(id accessibility.NodeID) {
		m := byID[id]
		if m == nil {
			return
		}
		if t := strings.TrimSpace(axString(m.Name)); t != "" && !seen[t] {
			seen[t] = true
			parts = append(parts, t)
		}
		for _, c := range m.ChildIDs {
			walk(c)
		}
	}
	for _, c := range n.ChildIDs {
		walk(c)
	}
	return strings.Join(parts, " ")
}

// axLive reports whether a node is an active aria-live region.
func axLive(n *accessibility.Node) bool {
	for _, p := range n.Properties {
		if p.Name == "live" {
			v := axString(p.Value)
			return v == "assertive" || v == "polite"
		}
	}
	return false
}

func axHasState(n *accessibility.Node, name string) bool {
	for _, p := range n.Properties {
		if string(p.Name) == name {
			return axString(p.Value) == "true"
		}
	}
	return false
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

// queryOptions maps QueryOpts to chromedp query options (selector syntax + the
// node state to wait for).
func waitOption(wait string) chromedp.QueryOption {
	switch wait {
	case "ready":
		return chromedp.NodeReady
	case "enabled":
		return chromedp.NodeEnabled
	default:
		return chromedp.NodeVisible
	}
}

// byOptions maps --by to the chromedp addressing option (no wait condition).
func byOptions(q QueryOpts) []chromedp.QueryOption {
	by := q.By
	if q.Pierce && (by == "" || by == "css") {
		// DevTools search matches across shadow DOM and iframes.
		by = "search"
	}
	switch by {
	case "id":
		return []chromedp.QueryOption{chromedp.ByID}
	case "search":
		return []chromedp.QueryOption{chromedp.BySearch}
	case "jspath":
		return []chromedp.QueryOption{chromedp.ByJSPath}
	case "css-all":
		return []chromedp.QueryOption{chromedp.ByQueryAll}
	default:
		return []chromedp.QueryOption{chromedp.ByQuery}
	}
}

func queryOptions(q QueryOpts) []chromedp.QueryOption {
	return append(byOptions(q), waitOption(q.Wait))
}

// byFor returns the addressing option for a selector (accessible-name / ref
// aware), without a wait condition — for verbs like wait --gone that supply
// their own.
func byFor(selector string, q QueryOpts) []chromedp.QueryOption {
	switch q.By {
	case "name":
		return []chromedp.QueryOption{chromedp.ByFunc(axNameQuery(selector, q.Role, q.Nth, q.Match))}
	case "ref":
		return []chromedp.QueryOption{chromedp.ByFunc(axRefQuery(selector))}
	}
	return byOptions(q)
}

// query builds the chromedp query options for a selector. By=="name" resolves by
// ARIA accessible name, By=="ref" by a snap-issued element ref; every other mode
// falls through to queryOptions.
func query(selector string, q QueryOpts) []chromedp.QueryOption {
	switch q.By {
	case "name":
		return []chromedp.QueryOption{
			chromedp.ByFunc(axNameQuery(selector, q.Role, q.Nth, q.Match)),
			waitOption(q.Wait),
		}
	case "ref":
		return []chromedp.QueryOption{
			chromedp.ByFunc(axRefQuery(selector)),
			waitOption(q.Wait),
		}
	}
	return queryOptions(q)
}

// axRefQuery resolves a snap-issued element ref ("e<backendNodeId>") back to a
// frontend node, so a caller acts on the exact element snap reported without
// re-resolving it by name. The backend id is stable for the document's lifetime.
func axRefQuery(ref string) func(context.Context, *cdp.Node) ([]cdp.NodeID, error) {
	return func(ctx context.Context, _ *cdp.Node) ([]cdp.NodeID, error) {
		id, err := parseRef(ref)
		if err != nil {
			return nil, err
		}
		return dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{cdp.BackendNodeID(id)}).Do(ctx)
	}
}

// parseRef parses an "e<n>" ref into its backend node id.
func parseRef(ref string) (int64, error) {
	s := strings.TrimSpace(ref)
	if !strings.HasPrefix(s, "e") {
		return 0, fmt.Errorf("bad ref %q (want e<number> from snap)", ref)
	}
	n, err := strconv.ParseInt(s[1:], 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad ref %q (want e<number> from snap)", ref)
	}
	return n, nil
}

// nameMatches compares an accessible name against the query per the match mode:
// exact (default, trimmed equality), contains (case-insensitive substring), or
// regex.
func nameMatches(actual, want, mode string) bool {
	a, w := strings.TrimSpace(actual), strings.TrimSpace(want)
	switch mode {
	case "contains":
		return strings.Contains(strings.ToLower(a), strings.ToLower(w))
	case "regex":
		re, err := regexp.Compile(want)
		return err == nil && re.MatchString(actual)
	default: // "" | "exact"
		return a == w
	}
}

// axNameQuery is a chromedp custom selector that resolves elements by their ARIA
// accessible name. It reads the full accessibility tree (the same primitive as
// `snap` — proven fast on large apps, and it crosses frames + shadow DOM),
// matches non-ignored nodes by name (and optional role) — so a hidden first
// match never stalls the wait — and returns the Nth (1-based) match, or all
// exposed matches when Nth is 0. (Accessibility.queryAXTree was tried first but
// recomputes the whole subtree per poll and times out on huge DOMs like Workday.)
func axNameQuery(name, role string, nth int, match string) func(context.Context, *cdp.Node) ([]cdp.NodeID, error) {
	return func(ctx context.Context, _ *cdp.Node) ([]cdp.NodeID, error) {
		nodes, err := accessibility.GetFullAXTree().Do(ctx)
		if err != nil {
			return nil, err
		}
		var backend []cdp.BackendNodeID
		for _, n := range nodes {
			if n.Ignored || n.BackendDOMNodeID == 0 {
				continue // ignored = not exposed to accessibility (hidden) -> skip
			}
			if !nameMatches(axString(n.Name), name, match) {
				continue
			}
			if role != "" && axString(n.Role) != role {
				continue
			}
			backend = append(backend, n.BackendDOMNodeID)
		}
		if nth > 0 {
			if nth > len(backend) {
				return nil, nil // not enough matches (yet) — let chromedp retry/timeout
			}
			backend = backend[nth-1 : nth]
		}
		if len(backend) == 0 {
			return nil, nil
		}
		return dom.PushNodesByBackendIDsToFrontend(backend).Do(ctx)
	}
}

// bringToFront is a best-effort action to make the tab active before synthetic
// input: Chrome drops clicks and keystrokes dispatched at a background/inactive
// tab, so `click`/`type`/`select` would otherwise be silent no-ops on a tab the
// user has switched away from.
func bringToFront() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		_ = page.BringToFront().Do(ctx)
		return nil
	})
}

func (c *CDP) Click(ctx context.Context, id, selector string, q QueryOpts) (map[string]any, error) {
	if err := c.run(ctx, id, bringToFront(), chromedp.Click(selector, query(selector, q)...)); err != nil {
		return nil, err
	}
	return map[string]any{"clicked": selector}, nil
}

func (c *CDP) Type(ctx context.Context, id, selector, text string, q QueryOpts) (map[string]any, error) {
	if err := c.run(ctx, id, bringToFront(), chromedp.SendKeys(selector, text, query(selector, q)...)); err != nil {
		return nil, err
	}
	return map[string]any{"typed": selector}, nil
}

func (c *CDP) HTML(ctx context.Context, id, selector string, inner bool, q QueryOpts) (map[string]any, error) {
	sel := selector
	if sel == "" {
		sel = "html" // whole document
	}
	var html string
	var action chromedp.Action
	if inner {
		action = chromedp.InnerHTML(sel, &html, query(sel, q)...)
	} else {
		action = chromedp.OuterHTML(sel, &html, query(sel, q)...)
	}
	if err := c.run(ctx, id, action); err != nil {
		return nil, err
	}
	return map[string]any{"html": html}, nil
}

func (c *CDP) Text(ctx context.Context, id, selector string, q QueryOpts) (map[string]any, error) {
	var text string
	if err := c.run(ctx, id, chromedp.Text(selector, &text, query(selector, q)...)); err != nil {
		return nil, err
	}
	return map[string]any{"text": text}, nil
}

func (c *CDP) Value(ctx context.Context, id, selector string, q QueryOpts) (map[string]any, error) {
	var val string
	if err := c.run(ctx, id, chromedp.Value(selector, &val, query(selector, q)...)); err != nil {
		return nil, err
	}
	return map[string]any{"value": val}, nil
}

func (c *CDP) Screenshot(ctx context.Context, id string) ([]byte, error) {
	var buf []byte
	if err := c.run(ctx, id, chromedp.CaptureScreenshot(&buf)); err != nil {
		return nil, err
	}
	return buf, nil
}

// PDF prints the page to PDF (no chromedp Action; raw page.PrintToPDF).
func (c *CDP) PDF(ctx context.Context, id string) ([]byte, error) {
	var buf []byte
	err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		buf, _, e = page.PrintToPDF().WithPrintBackground(true).Do(ctx)
		return e
	}))
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func (c *CDP) CookieList(ctx context.Context, id string) (any, error) {
	var cookies []*network.Cookie
	err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		cookies, e = network.GetCookies().Do(ctx)
		return e
	}))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(cookies))
	for i, ck := range cookies {
		out[i] = map[string]any{
			"name": ck.Name, "value": ck.Value, "domain": ck.Domain,
			"path": ck.Path, "secure": ck.Secure, "httpOnly": ck.HTTPOnly,
		}
	}
	return map[string]any{"cookies": out}, nil
}

func (c *CDP) CookieSet(ctx context.Context, id, name, value, domain, path string) (map[string]any, error) {
	err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		a := network.SetCookie(name, value)
		if domain != "" {
			a = a.WithDomain(domain)
		}
		if path != "" {
			a = a.WithPath(path)
		}
		return a.Do(ctx)
	}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"set": name}, nil
}

func (c *CDP) CookieDelete(ctx context.Context, id, name string) (map[string]any, error) {
	err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		return network.DeleteCookies(name).Do(ctx)
	}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"deleted": name}, nil
}

func (c *CDP) CookieClear(ctx context.Context, id string) (map[string]any, error) {
	err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		return network.ClearBrowserCookies().Do(ctx)
	}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"cleared": true}, nil
}

func (c *CDP) AttrGet(ctx context.Context, id, selector, name string, q QueryOpts) (map[string]any, error) {
	var val string
	var ok bool
	if err := c.run(ctx, id, chromedp.AttributeValue(selector, name, &val, &ok, query(selector, q)...)); err != nil {
		return nil, err
	}
	return map[string]any{"name": name, "value": val, "present": ok}, nil
}

func (c *CDP) AttrList(ctx context.Context, id, selector string, q QueryOpts) (map[string]any, error) {
	var attrs map[string]string
	if err := c.run(ctx, id, chromedp.Attributes(selector, &attrs, query(selector, q)...)); err != nil {
		return nil, err
	}
	return map[string]any{"attributes": attrs}, nil
}

func (c *CDP) AttrSet(ctx context.Context, id, selector, name, value string, q QueryOpts) (map[string]any, error) {
	if err := c.run(ctx, id, chromedp.SetAttributeValue(selector, name, value, query(selector, q)...)); err != nil {
		return nil, err
	}
	return map[string]any{"set": name}, nil
}

func (c *CDP) AttrRemove(ctx context.Context, id, selector, name string, q QueryOpts) (map[string]any, error) {
	if err := c.run(ctx, id, chromedp.RemoveAttribute(selector, name, query(selector, q)...)); err != nil {
		return nil, err
	}
	return map[string]any{"removed": name}, nil
}

func (c *CDP) SetHeaders(ctx context.Context, id string, headers map[string]string) (map[string]any, error) {
	h := network.Headers{}
	for k, v := range headers {
		h[k] = v
	}
	err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := network.Enable().Do(ctx); err != nil {
			return err
		}
		return network.SetExtraHTTPHeaders(h).Do(ctx)
	}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"headers": len(headers)}, nil
}

func (c *CDP) EmulateViewport(ctx context.Context, id string, width, height int64) (map[string]any, error) {
	if err := c.run(ctx, id, chromedp.EmulateViewport(width, height)); err != nil {
		return nil, err
	}
	return map[string]any{"width": width, "height": height}, nil
}

func (c *CDP) EmulateGeo(ctx context.Context, id string, lat, lon float64) (map[string]any, error) {
	err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		return emulation.SetGeolocationOverride().WithLatitude(lat).WithLongitude(lon).WithAccuracy(1).Do(ctx)
	}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"lat": lat, "lon": lon}, nil
}

func (c *CDP) EmulateReset(ctx context.Context, id string) (map[string]any, error) {
	err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		_ = emulation.ClearGeolocationOverride().Do(ctx)
		return emulation.ClearDeviceMetricsOverride().Do(ctx)
	}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"reset": true}, nil
}

// Frames enumerates the tab's frame tree (Page.getFrameTree).
func (c *CDP) Frames(ctx context.Context, id string) (any, error) {
	var tree *page.FrameTree
	err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		tree, e = page.GetFrameTree().Do(ctx)
		return e
	}))
	if err != nil {
		return nil, err
	}
	var frames []map[string]any
	var walk func(n *page.FrameTree)
	walk = func(n *page.FrameTree) {
		if n == nil || n.Frame == nil {
			return
		}
		frames = append(frames, map[string]any{
			"id":       string(n.Frame.ID),
			"url":      n.Frame.URL,
			"name":     n.Frame.Name,
			"parentId": string(n.Frame.ParentID),
		})
		for _, ch := range n.ChildFrames {
			walk(ch)
		}
	}
	walk(tree)
	return map[string]any{"frames": frames}, nil
}

// Wait blocks until a condition holds: the target URL contains cond.URL, or a
// selector becomes visible / is gone. The caller's context deadline bounds it.
func (c *CDP) Wait(ctx context.Context, id string, cond WaitCond) (map[string]any, error) {
	var action chromedp.Action
	var what string
	switch {
	case cond.URL != "":
		action, what = waitURL(cond.URL), "url:"+cond.URL
	case cond.Visible != "":
		action, what = chromedp.WaitVisible(cond.Visible, byFor(cond.Visible, cond.Query)...), "visible:"+cond.Visible
	case cond.Gone != "":
		action, what = chromedp.WaitNotPresent(cond.Gone, byFor(cond.Gone, cond.Query)...), "gone:"+cond.Gone
	case cond.Text != "":
		action, what = waitText(cond.Text), "text:"+cond.Text
	case cond.Stable:
		action, what = waitStable(800*time.Millisecond), "stable"
	default:
		return nil, fmt.Errorf("wait needs one of --url, --visible, --gone, --text, --stable, --for")
	}
	if err := c.run(ctx, id, action); err != nil {
		return nil, err
	}
	return map[string]any{"waited": what}, nil
}

// waitURL polls location.href until it contains substr (or the context ends).
func waitURL(substr string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			var href string
			if err := chromedp.Evaluate("location.href", &href).Do(ctx); err == nil && strings.Contains(href, substr) {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
			}
		}
	})
}

// waitText polls the accessibility tree until some node's name contains substr
// (case-insensitive) — e.g. a "Success" toast after a write, without a screenshot.
func waitText(substr string) chromedp.Action {
	want := strings.ToLower(strings.TrimSpace(substr))
	return chromedp.ActionFunc(func(ctx context.Context) error {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			if nodes, err := accessibility.GetFullAXTree().Do(ctx); err == nil {
				for _, n := range nodes {
					if strings.Contains(strings.ToLower(axString(n.Name)), want) {
						return nil
					}
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
			}
		}
	})
}

// waitStable returns once the accessibility tree has been unchanged for window —
// "the page settled" — so a caller stops guessing fixed sleeps after an action.
func waitStable(window time.Duration) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		t := time.NewTicker(150 * time.Millisecond)
		defer t.Stop()
		var lastSig string
		var lastChange time.Time
		for {
			if nodes, err := accessibility.GetFullAXTree().Do(ctx); err == nil {
				sig := axSig(nodes)
				now := time.Now()
				if lastChange.IsZero() || sig != lastSig {
					lastSig, lastChange = sig, now
				} else if now.Sub(lastChange) >= window {
					return nil
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
			}
		}
	})
}

// axSig is a cheap signature of the accessibility tree's shape (roles + names),
// used to detect when it stops changing.
func axSig(nodes []*accessibility.Node) string {
	h := fnv.New64a()
	for _, n := range nodes {
		_, _ = h.Write([]byte(axString(n.Role)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(axString(n.Name)))
		_, _ = h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 16)
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
