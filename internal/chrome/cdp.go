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
	cdpruntime "github.com/chromedp/cdproto/runtime"
	cdptarget "github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	easyjson "github.com/mailru/easyjson"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/eventbuf"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// Options controls how CDP connects.
type Options struct {
	PortFile   string // override DevToolsActivePort location (else OS candidates)
	ProfileDir string // managed-launch profile dir (else CHROME_CDP_PROFILE / default)
	Port       int    // explicit debug port to attach to / launch with (0 = auto)
	NoLaunch   bool   // don't fall back to launching a managed Chrome
	Headless   bool   // headless for the managed-launch fallback (tests use this)

	// Event-capture bounds (config keys console_buffer / console_max_entry).
	// Zero means the built-in default; see configureCapture.
	ConsoleBuffer   int // retained console messages per target
	ConsoleMaxEntry int // per-message text cap, in bytes

	// Network-capture bounds (config keys net_buffer / net_max_body). Separate
	// from the console's because a correlated request record is much larger than
	// a console line. Zero means the built-in default.
	NetBuffer  int // retained network records per target
	NetMaxBody int // per-body cap, in bytes
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

	// Retained CDP events, per target. The buffers live HERE — on the object
	// that holds the connection, which in normal use is owned by the daemon —
	// because a per-command process cannot retain events it was not running to
	// receive. Capture starts at attach (see startCapture in console.go), not
	// at the first read. Sized once by configureCapture, before any attach.
	console         *eventbuf.Set[consoleMessage]
	consoleMaxEntry int

	// Network records get their OWN buffers and bounds: they are larger per
	// entry and evicted on a different rhythm than console lines, so sharing one
	// ring would let a chatty console throw away the request history (and vice
	// versa). See net.go.
	net        *eventbuf.Set[netRecord]
	netMaxBody int
}

// tabConn is a cached per-tab context and its cancel func.
type tabConn struct {
	ctx  context.Context
	stop context.CancelFunc
}

func newCDP(managed bool, alloc context.Context, allocCancel context.CancelFunc, base context.Context, baseCancel context.CancelFunc) *CDP {
	c := &CDP{
		managed: managed, alloc: alloc, allocCancel: allocCancel, base: base, baseCancel: baseCancel,
		tabs: map[string]tabConn{},
	}
	// Capture is on from the start, at the built-in bounds; Connect resizes it
	// from the config before the first attach.
	c.configureCapture(0, 0)
	c.configureNetCapture(0, 0)
	return c
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
	var c *CDP
	var err error
	switch browser.DecideConnection(probe) {
	case browser.Attach:
		c, err = attach(endpoint)
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
		c, err = launch(opts.Headless, opts.ProfileDir, opts.Port)
	}
	if err != nil {
		return nil, err
	}
	// Size the event buffers before any tab is attached, i.e. before capture
	// can receive anything.
	c.configureCapture(opts.ConsoleBuffer, opts.ConsoleMaxEntry)
	c.configureNetCapture(opts.NetBuffer, opts.NetMaxBody)
	return c, nil
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
	// Event capture starts at ATTACH, not at the first `console`/`net` read:
	// the process holding the connection has to already be listening when the
	// page logs, or an observability verb can only report what happened after
	// somebody thought to look. See startCapture in console.go.
	c.startCapture(tctx, id)
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

// Open creates a new tab at url (browser-level Target.createTarget, which
// navigates it) and returns the new tab's id — replacing a raw
// Target.createTarget for the common "start from a fresh tab on X" case.
func (c *CDP) Open(ctx context.Context, url string) (map[string]any, error) {
	var tid cdptarget.ID
	rc, cancel := deadline(ctx, c.base)
	defer cancel()
	err := chromedp.Run(rc, chromedp.ActionFunc(func(actx context.Context) error {
		cc := chromedp.FromContext(actx)
		bctx := cdp.WithExecutor(actx, cc.Browser)
		var e error
		tid, e = cdptarget.CreateTarget(url).Do(bctx)
		return e
	}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": tid.String(), "url": url}, nil
}

func (c *CDP) Navigate(ctx context.Context, id, url string) (map[string]any, error) {
	var loc string
	if err := c.run(ctx, id, chromedp.Navigate(url), chromedp.Location(&loc)); err != nil {
		return nil, err
	}
	return map[string]any{"url": loc}, nil
}

// Eval and Text live in read.go — the page-reading verbs (RFC-0010).

func (c *CDP) Snapshot(ctx context.Context, id string, opts SnapOpts) (any, error) {
	var re *regexp.Regexp
	if opts.Grep != "" {
		var rerr error
		if re, rerr = regexp.Compile(opts.Grep); rerr != nil {
			return nil, fmt.Errorf("--grep is not a valid regex: %w", rerr)
		}
	}
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
	// --region scopes the node list to the subtree of the first container whose
	// name contains the given text (alerts/focused stay page-wide).
	var inRegion map[accessibility.NodeID]bool
	if opts.Region != "" {
		if rn := findRegion(nodes, opts.Region); rn != nil {
			inRegion = map[accessibility.NodeID]bool{}
			markSubtree(byID, rn, inRegion)
		} else {
			inRegion = map[accessibility.NodeID]bool{} // region not found -> nothing
		}
	}
	out := make([]axNode, 0, len(nodes))
	var alerts []string        // aria-live / role=alert|status text — the toasts/notifications
	var focused map[string]any // the currently-focused element
	seen := map[string]bool{}
	for _, n := range nodes {
		role, name := axString(n.Role), axString(n.Name)
		// A live region's text is usually in child StaticText nodes, not its own
		// name — walk the subtree so toasts ("Success! Event approved") surface.
		// Computed over the FULL tree, before any --role/--grep/--region filter.
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
		if opts.Role != "" && role != opts.Role {
			continue
		}
		if re != nil && !re.MatchString(name) {
			continue
		}
		if inRegion != nil && !inRegion[n.NodeID] {
			continue
		}
		if opts.Dedupe {
			key := role + "\x00" + name
			if seen[key] {
				continue
			}
			seen[key] = true
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

// findRegion returns the first exposed node whose accessible name contains sub
// (case-insensitive) — the container that --region scopes to.
func findRegion(nodes []*accessibility.Node, sub string) *accessibility.Node {
	want := strings.ToLower(strings.TrimSpace(sub))
	for _, n := range nodes {
		if n.Ignored {
			continue
		}
		if strings.Contains(strings.ToLower(axString(n.Name)), want) {
			return n
		}
	}
	return nil
}

// markSubtree records a node and all its descendants in set.
func markSubtree(byID map[accessibility.NodeID]*accessibility.Node, n *accessibility.Node, set map[accessibility.NodeID]bool) {
	if n == nil || set[n.NodeID] {
		return
	}
	set[n.NodeID] = true
	for _, c := range n.ChildIDs {
		markSubtree(byID, byID[c], set)
	}
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
	if q.InRow != "" {
		return []chromedp.QueryOption{chromedp.ByFunc(nameRowQuery(selector, q.Role, q.Nth, q.Match, q.InRow, q.By))}
	}
	switch q.By {
	case "name":
		return []chromedp.QueryOption{chromedp.ByFunc(axNameQuery(selector, q.Role, q.Nth, q.Match))}
	case "ref":
		return []chromedp.QueryOption{chromedp.ByFunc(axRefQuery(selector))}
	case "cell":
		return []chromedp.QueryOption{chromedp.ByFunc(cellQuery(selector))}
	case "label":
		return []chromedp.QueryOption{chromedp.ByFunc(labelQuery(selector, q.Match))}
	}
	return byOptions(q)
}

// query builds the chromedp query options for a selector. By=="name" resolves by
// ARIA accessible name, By=="ref" by a snap-issued element ref; every other mode
// falls through to queryOptions.
func query(selector string, q QueryOpts) []chromedp.QueryOption {
	if q.InRow != "" {
		return []chromedp.QueryOption{
			chromedp.ByFunc(nameRowQuery(selector, q.Role, q.Nth, q.Match, q.InRow, q.By)),
			waitOption(q.Wait),
		}
	}
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
	case "cell":
		return []chromedp.QueryOption{
			chromedp.ByFunc(cellQuery(selector)),
			waitOption(q.Wait),
		}
	case "label":
		return []chromedp.QueryOption{
			chromedp.ByFunc(labelQuery(selector, q.Match)),
			waitOption(q.Wait),
		}
	}
	return queryOptions(q)
}

// splitCell parses a cell selector into an optional row header and a column
// header. "Mon, 7/13" targets the column in a single-row grid; "Regular|Mon,
// 7/13" (row|col) disambiguates the row in a multi-row grid.
func splitCell(sel string) (row, col string) {
	parts := strings.Split(sel, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) == 1 {
		return "", parts[0]
	}
	return parts[0], parts[len(parts)-1]
}

// cellQuery resolves the editable input in a grid cell addressed by its column
// header (and optional row header) — killing the manual "map inputs to day
// columns by x-coordinate" dance for timesheet/calendar grids. It computes the
// column from the header's centre-x in JS (works on a hidden tab) and returns the
// matching input via DOM.requestNode.
func cellQuery(cellSel string) func(context.Context, *cdp.Node) ([]cdp.NodeID, error) {
	return func(ctx context.Context, _ *cdp.Node) ([]cdp.NodeID, error) {
		row, col := splitCell(cellSel)
		colJSON, _ := json.Marshal(col)
		rowJSON, _ := json.Marshal(row)
		expr := fmt.Sprintf(cellLocatorJS, string(colJSON), string(rowJSON))
		res, exc, err := cdpruntime.Evaluate(expr).Do(ctx)
		if err != nil {
			return nil, err
		}
		if exc != nil {
			return nil, fmt.Errorf("cell locator: %s", exc.Text)
		}
		if res == nil || res.ObjectID == "" {
			return nil, nil // not found (yet) — let chromedp retry/timeout
		}
		nid, err := dom.RequestNode(res.ObjectID).Do(ctx)
		if err != nil || nid == 0 {
			return nil, err
		}
		return []cdp.NodeID{nid}, nil
	}
}

// cellLocatorJS returns the editable input in the column whose header matches
// (case-insensitive substring) — restricted to the data row whose text matches
// the row header when one is given. Args: %[1]s column-header JSON, %[2]s
// row-header JSON (empty = any row).
const cellLocatorJS = `(() => {
  const col = %[1]s, row = %[2]s;
  const norm = s => (s || "").replace(/\s+/g, " ").trim();
  const has = (a, b) => norm(a).toLowerCase().includes(norm(b).toLowerCase());
  const vis = el => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const headers = [...document.querySelectorAll("[role=columnheader],th,[data-automation-id=columnHeader]")].filter(vis);
  const hdr = headers.find(h => has(h.textContent, col));
  if (!hdr) return null;
  const hr = hdr.getBoundingClientRect();
  const colX = hr.left + hr.width / 2, tol = Math.max(hr.width / 2, 30);
  const fields = "input,textarea,select,[contenteditable=true],[role=textbox],[role=spinbutton]";
  let cands = [...document.querySelectorAll(fields)].filter(el => {
    if (!vis(el)) return false;
    const r = el.getBoundingClientRect();
    return Math.abs((r.left + r.width / 2) - colX) <= tol;
  });
  if (row) {
    cands = cands.filter(el => {
      const tr = el.closest("[role=row],tr");
      return tr && has(tr.textContent, row);
    });
  }
  return cands[0] || null;
})()`

// labelQuery resolves a FORM CONTROL by its visible label text — for forms whose
// labels are visible to a human but not wired to the control (no `aria-label`, no
// `<label for>`), which would otherwise force an eval to find a CSS selector. It
// matches a `<label for>`/wrapping label, else a label-ish element (span/div/dt/
// legend/th) whose text matches, then returns the control after it in its
// container. match defaults to contains (labels are often verbose).
func labelQuery(label, match string) func(context.Context, *cdp.Node) ([]cdp.NodeID, error) {
	return func(ctx context.Context, _ *cdp.Node) ([]cdp.NodeID, error) {
		mode := match
		if mode == "" {
			mode = "contains"
		}
		labelJSON, _ := json.Marshal(label)
		modeJSON, _ := json.Marshal(mode)
		expr := fmt.Sprintf(labelLocatorJS, string(labelJSON), string(modeJSON))
		res, exc, err := cdpruntime.Evaluate(expr).Do(ctx)
		if err != nil {
			return nil, err
		}
		if exc != nil {
			return nil, fmt.Errorf("label locator: %s", exc.Text)
		}
		if res == nil || res.ObjectID == "" {
			return nil, nil
		}
		nid, err := dom.RequestNode(res.ObjectID).Do(ctx)
		if err != nil || nid == 0 {
			return nil, err
		}
		return []cdp.NodeID{nid}, nil
	}
}

// labelLocatorJS finds the form control described by a visible label. Args: %[1]s
// label JSON, %[2]s match-mode JSON (exact|contains|regex).
const labelLocatorJS = `(() => {
  const want = %[1]s, mode = %[2]s;
  const norm = s => (s || "").replace(/\s+/g, " ").trim();
  const cmp = (a, b) => {
    a = norm(a);
    if (mode === "exact") return a.toLowerCase() === b.toLowerCase();
    if (mode === "regex") { try { return new RegExp(b).test(a); } catch (e) { return false; } }
    return a.toLowerCase().includes(b.toLowerCase());
  };
  const CTL = "input:not([type=hidden]),select,textarea,[contenteditable=true],[role=textbox],[role=combobox],[role=spinbutton],[role=listbox]";
  const vis = el => {
    const r = el.getBoundingClientRect();
    if (r.width < 1 || r.height < 1) return false;
    const cs = getComputedStyle(el);
    return cs.visibility !== "hidden" && cs.display !== "none";
  };
  // Given a matching label element, return the control it labels: a control it
  // wraps, else the first visible control in the nearest ancestor that has one,
  // preferring one that follows the label in document order.
  const controlFor = lab => {
    let node = lab;
    for (let i = 0; i < 4 && node; i++) {
      const ctls = [...node.querySelectorAll(CTL)].filter(vis);
      if (ctls.length) {
        const after = ctls.find(c => lab.compareDocumentPosition(c) & Node.DOCUMENT_POSITION_FOLLOWING);
        return after || ctls[0];
      }
      node = node.parentElement;
    }
    return null;
  };
  // 1. A real <label> (for= or wrapping) — the authoritative case.
  for (const lab of document.querySelectorAll("label")) {
    if (!cmp(lab.textContent, want)) continue;
    const f = lab.getAttribute("for");
    const ctl = f ? document.getElementById(f) : lab.querySelector(CTL);
    if (ctl && vis(ctl)) return ctl;
  }
  // 2. A label-ish element whose text matches -> the control near it. Prefer the
  // tightest (shortest-text) match so "Notes" beats a paragraph containing it.
  const labelish = [...document.querySelectorAll("label,span,div,dt,legend,th,p,strong,b")]
    .filter(e => e.childElementCount <= 2 && cmp(e.textContent, want));
  labelish.sort((a, b) => norm(a.textContent).length - norm(b.textContent).length);
  for (const lab of labelish) {
    const c = controlFor(lab);
    if (c) return c;
  }
  return null;
})()`

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
		if err == nil {
			backend := axMatchBackends(nodes, name, role, nth, match)
			if len(backend) > 0 {
				return dom.PushNodesByBackendIDsToFrontend(backend).Do(ctx)
			}
		}
		// The a11y tree yielded no match (or errored). Chrome throttles the tree on
		// a tab it can't foreground, so on a hidden tab fall back to a DOM-based
		// accessible-name match (querySelector isn't throttled). On a visible tab
		// the a11y tree is authoritative, so we don't second-guess it.
		if tabHidden(ctx) {
			return domNameQuery(ctx, name, role, nth, match, "")
		}
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
}

// axMatchBackends returns the backend ids of the exposed (non-ignored) a11y nodes
// matching name (and optional role), the Nth (1-based) or all when nth is 0.
func axMatchBackends(nodes []*accessibility.Node, name, role string, nth int, match string) []cdp.BackendNodeID {
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
			return nil
		}
		return backend[nth-1 : nth]
	}
	return backend
}

// tabHidden reports whether document.visibilityState is "hidden" — the tab isn't
// the active tab in a focused window, so Chrome throttles its accessibility tree.
func tabHidden(ctx context.Context) bool {
	var vis string
	if err := chromedp.Evaluate("document.visibilityState", &vis).Do(ctx); err != nil {
		return false
	}
	return vis == "hidden"
}

// domNameQuery resolves an element by its accessible name computed in JS (not the
// throttled a11y tree) — aria-label / labelledby / associated label / role text /
// title / placeholder — restricted to visible, non-aria-hidden elements so it
// keeps the "skip hidden nodes" property. Returns the Nth (1-based, or first)
// visible match, via DOM.requestNode.
func domNameQuery(ctx context.Context, name, role string, nth int, match, row string) ([]cdp.NodeID, error) {
	nameJSON, _ := json.Marshal(name)
	roleJSON, _ := json.Marshal(role)
	modeJSON, _ := json.Marshal(match)
	rowJSON, _ := json.Marshal(row)
	expr := fmt.Sprintf(domNameLocatorJS, string(nameJSON), string(roleJSON), string(modeJSON), nth, string(rowJSON))
	res, exc, err := cdpruntime.Evaluate(expr).Do(ctx)
	if err != nil {
		return nil, err
	}
	if exc != nil {
		return nil, fmt.Errorf("dom name locator: %s", exc.Text)
	}
	if res == nil || res.ObjectID == "" {
		return nil, nil
	}
	nid, err := dom.RequestNode(res.ObjectID).Do(ctx)
	if err != nil || nid == 0 {
		return nil, err
	}
	return []cdp.NodeID{nid}, nil
}

// domNameLocatorJS computes a simplified ARIA accessible name + role in JS and
// returns the Nth (1-based, or first) visible matching element. Args: %[1]s name
// JSON, %[2]s role JSON (empty = any), %[3]s match-mode JSON, %[4]d nth, %[5]s
// row JSON (empty = any row; else keep only elements whose closest [role=row]/tr
// ancestor's text contains it — case-insensitive).
const domNameLocatorJS = `(() => {
  const want = %[1]s, role = %[2]s, mode = %[3]s, nth = %[4]d, row = %[5]s;
  const norm = s => (s || "").replace(/\s+/g, " ").trim();
  const inRow = el => {
    if (!row) return true;
    const tr = el.closest("[role=row],tr");
    return tr && norm(tr.textContent).toLowerCase().includes(norm(row).toLowerCase());
  };
  const cmp = (a, b) => {
    a = norm(a);
    if (mode === "contains") return a.toLowerCase().includes(b.toLowerCase());
    if (mode === "regex") { try { return new RegExp(b).test(a); } catch (e) { return false; } }
    return a === b;
  };
  const visible = el => {
    if (el.getAttribute("aria-hidden") === "true") return false;
    const cs = getComputedStyle(el);
    if (cs.visibility === "hidden" || cs.display === "none") return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const roleOf = el => {
    const ex = el.getAttribute("role"); if (ex) return ex;
    const tag = el.tagName.toLowerCase();
    if (tag === "button") return "button";
    if (tag === "a" && el.hasAttribute("href")) return "link";
    if (tag === "select") return "combobox";
    if (tag === "textarea") return "textbox";
    if (/^h[1-6]$/.test(tag)) return "heading";
    if (tag === "input") {
      const ty = (el.getAttribute("type") || "text").toLowerCase();
      if (["button", "submit", "reset"].includes(ty)) return "button";
      if (ty === "checkbox") return "checkbox";
      if (ty === "radio") return "radio";
      return "textbox";
    }
    return "";
  };
  const textRoles = ["button", "link", "heading", "option", "menuitem", "menuitemradio", "menuitemcheckbox", "tab", "treeitem", "cell", "columnheader", "rowheader"];
  const accName = el => {
    const al = el.getAttribute("aria-label"); if (al) return al;
    const lb = el.getAttribute("aria-labelledby");
    if (lb) {
      const t = lb.split(/\s+/).map(id => { const e = document.getElementById(id); return e ? e.textContent : ""; }).join(" ");
      if (norm(t)) return t;
    }
    if (el.id) { try { const lab = document.querySelector('label[for="' + CSS.escape(el.id) + '"]'); if (lab && norm(lab.textContent)) return lab.textContent; } catch (e) {} }
    const wrap = el.closest("label"); if (wrap && norm(wrap.textContent)) return wrap.textContent;
    if (textRoles.includes(roleOf(el)) && norm(el.textContent)) return el.textContent;
    const ph = el.getAttribute("placeholder"); if (ph) return ph;
    const ti = el.getAttribute("title"); if (ti) return ti;
    const alt = el.getAttribute("alt"); if (alt) return alt;
    if (el.tagName === "INPUT" && ["button", "submit", "reset"].includes((el.getAttribute("type") || "").toLowerCase())) {
      const v = el.getAttribute("value"); if (v) return v;
    }
    return "";
  };
  const out = [];
  for (const el of document.querySelectorAll("*")) {
    if (!visible(el)) continue;
    if (role && roleOf(el) !== role) continue;
    if (!cmp(accName(el), want)) continue;
    if (!inRow(el)) continue;
    out.push(el);
  }
  if (!out.length) return null;
  return out[nth > 0 ? nth - 1 : 0] || null;
})()`

// nameRowQuery resolves an element by accessible name (and optional role/nth/
// match) scoped to the table row whose text contains `row`. Row membership is a
// DOM notion (closest [role=row]/tr), so this resolves via the DOM accessible-
// name locator rather than the a11y tree — which also means it isn't throttled
// on a backgrounded tab. Requires By == "name" (validated in query/byFor).
func nameRowQuery(name, role string, nth int, match, row, by string) func(context.Context, *cdp.Node) ([]cdp.NodeID, error) {
	return func(ctx context.Context, _ *cdp.Node) ([]cdp.NodeID, error) {
		switch by {
		case "ref", "cell", "label":
			return nil, fmt.Errorf("--in-row addresses the control by accessible name; it can't combine with --by %s", by)
		}
		return domNameQuery(ctx, name, role, nth, match, row)
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

// dialogSink records native JS dialogs auto-handled during an action, so the
// verb can report them in its result envelope.
type dialogSink struct {
	mu   sync.Mutex
	seen []map[string]any
}

func (d *dialogSink) add(ev *page.EventJavascriptDialogOpening, action string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, map[string]any{"type": string(ev.Type), "message": ev.Message, "handled": action})
}

func (d *dialogSink) list() []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seen
}

func (d *dialogSink) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// withDialog wraps an action so a native JavaScript dialog (alert/confirm/prompt)
// that opens DURING it is auto-accepted (policy "accept", the default) or
// dismissed ("dismiss") — instead of blocking the renderer and wedging the CDP
// connection (the failure every skill warns about). A confirm() triggered by a
// click blocks synchronously inside the click's event dispatch, so the listener
// is live while action.Do runs; a short grace after catches a just-late dialog.
func withDialog(policy string, sink *dialogSink, action chromedp.Action) chromedp.Action {
	accept := policy != "dismiss"
	return chromedp.ActionFunc(func(ctx context.Context) error {
		// Page must be enabled for javascriptDialogOpening to fire (idempotent).
		_ = page.Enable().Do(ctx)
		chromedp.ListenTarget(ctx, func(ev interface{}) {
			d, ok := ev.(*page.EventJavascriptDialogOpening)
			if !ok {
				return
			}
			sink.add(d, policy)
			// Handle from a goroutine: the callback runs on the event-loop and
			// must not itself issue a blocking CDP command.
			go func() { _ = page.HandleJavaScriptDialog(accept).Do(ctx) }()
		})
		err := action.Do(ctx)
		// Grace for a dialog that opens right as the action returns (e.g. a
		// navigation-triggered beforeunload) — only pay it if none fired yet.
		if sink.count() == 0 {
			select {
			case <-ctx.Done():
			case <-time.After(250 * time.Millisecond):
			}
		}
		return err
	})
}

// withOptionalDialog wraps action with dialog handling when q.OnDialog is set,
// and returns the sink (nil when off) so the verb can attach handled dialogs to
// its result.
func withOptionalDialog(q QueryOpts, action chromedp.Action) (chromedp.Action, *dialogSink) {
	if q.OnDialog == "" {
		return action, nil
	}
	sink := &dialogSink{}
	return withDialog(q.OnDialog, sink, action), sink
}

// withDialogResult folds any handled dialogs into the verb's result map.
func withDialogResult(res map[string]any, sink *dialogSink) map[string]any {
	if sink != nil {
		if d := sink.list(); len(d) > 0 {
			res["dialogs"] = d
		}
	}
	return res
}

// Type coordinate-clicks the selector to focus it (robust on a background tab),
// then sends the text as real keystrokes to the focused element.
func (c *CDP) Type(ctx context.Context, id, selector, text string, q QueryOpts) (map[string]any, error) {
	core := chromedp.ActionFunc(func(actx context.Context) error {
		nid, err := resolveNodeReady(actx, selector, q)
		if err != nil {
			return err
		}
		if err := coordClickNode(actx, nid); err != nil {
			return err
		}
		return chromedp.KeyEvent(text).Do(actx)
	})
	action, sink := withOptionalDialog(q, core)
	if err := c.run(ctx, id, bringToFront(), action); err != nil {
		return nil, err
	}
	return withDialogResult(map[string]any{"typed": selector}, sink), nil
}

// Fill sets a field to value, replacing (not appending to) any existing content:
// it triple-clicks the field to select all its text, then types value as real
// keystrokes over the selection — the reliable way to set a pre-filled cell (e.g.
// a timesheet "0" hour cell) to a new value in one call.
func (c *CDP) Fill(ctx context.Context, id, selector, value string, q QueryOpts) (map[string]any, error) {
	core := chromedp.ActionFunc(func(actx context.Context) error {
		nid, err := resolveNodeReady(actx, selector, q)
		if err != nil {
			return err
		}
		// Triple-click selects all text in the field; typing then replaces it.
		if err := coordClickNodeN(actx, nid, 3); err != nil {
			return err
		}
		return chromedp.KeyEvent(value).Do(actx)
	})
	action, sink := withOptionalDialog(q, core)
	if err := c.run(ctx, id, bringToFront(), action); err != nil {
		return nil, err
	}
	return withDialogResult(map[string]any{"filled": selector, "value": value}, sink), nil
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

func (c *CDP) Value(ctx context.Context, id, selector string, q QueryOpts) (map[string]any, error) {
	var val string
	if err := c.run(ctx, id, chromedp.Value(selector, &val, query(selector, q)...)); err != nil {
		return nil, err
	}
	return map[string]any{"value": val}, nil
}

// Values reads the value (or text) of EVERY element matching a CSS selector, in
// document order — one round trip instead of an eval per field (e.g. reading a
// whole row of timesheet hour cells or a set of selected pills). Uses
// querySelectorAll, so it works even on a background/hidden tab.
func (c *CDP) Values(ctx context.Context, id, selector string, q QueryOpts) (map[string]any, error) {
	selJSON, _ := json.Marshal(selector)
	expr := fmt.Sprintf(`(() => [...document.querySelectorAll(%s)].map(e => ("value" in e && typeof e.value === "string") ? e.value : (e.textContent || "").trim()))()`, string(selJSON))
	var vals []string
	if err := c.run(ctx, id, chromedp.Evaluate(expr, &vals)); err != nil {
		return nil, err
	}
	return map[string]any{"values": vals, "count": len(vals)}, nil
}

// Screenshot and PDF live in capture.go.

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
	case cond.Idle:
		action, what = waitIdle(500*time.Millisecond), "idle"
	default:
		return nil, fmt.Errorf("wait needs one of --url, --visible, --gone, --text, --stable, --idle, --for")
	}
	// Also read the URL the tab settled at, so the envelope's target reflects
	// where a --url / redirect wait actually landed, not the pre-wait URL.
	var loc string
	if err := c.run(ctx, id, action, chromedp.Location(&loc)); err != nil {
		return nil, err
	}
	return map[string]any{"waited": what, "url": loc}, nil
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

// waitIdle returns once network activity has settled. It considers the page
// settled when EITHER no requests are in flight for `window` (the clean path,
// for pages whose requests all complete), OR requests remain open but the
// connection has gone silent for `idleStall` (the stalled path). The stalled
// path is what makes --idle usable on SPAs (Outlook, Workday) that hold a
// websocket / long-poll / EventSource stream open indefinitely: such a request
// fires requestWillBeSent but never loadingFinished, so inflight never returns
// to zero and a strict "inflight == 0" wait would hang until --timeout. Progress
// events (response/data), not just start/finish, keep the clock live, so an
// in-progress download is never mistaken for a silent held-open stream.
func waitIdle(window time.Duration) chromedp.Action {
	// A still-open request is treated as idle after this much network silence.
	// Longer than `window` so a normally-completing load always settles via the
	// clean path first; short enough that a held-open stream doesn't wait out
	// the whole --timeout.
	const idleStall = 2 * time.Second
	return chromedp.ActionFunc(func(ctx context.Context) error {
		if err := network.Enable().Do(ctx); err != nil {
			return err
		}
		var mu sync.Mutex
		inflight := 0
		// lastActivity is the last time any request started, progressed, or
		// ended — set only for network events (ListenTarget also delivers page/
		// DOM events, which must not count as network activity).
		lastActivity := time.Now()
		chromedp.ListenTarget(ctx, func(ev interface{}) {
			mu.Lock()
			defer mu.Unlock()
			switch ev.(type) {
			case *network.EventRequestWillBeSent:
				inflight++
				lastActivity = time.Now()
			case *network.EventLoadingFinished, *network.EventLoadingFailed:
				if inflight > 0 {
					inflight--
				}
				lastActivity = time.Now()
			case *network.EventResponseReceived, *network.EventDataReceived:
				// bytes moving on an open request — keep it counted as active
				lastActivity = time.Now()
			}
		})
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			mu.Lock()
			threshold := window
			if inflight > 0 {
				threshold = idleStall // still-open requests need the longer stall window
			}
			idle := time.Since(lastActivity) >= threshold
			mu.Unlock()
			if idle {
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
