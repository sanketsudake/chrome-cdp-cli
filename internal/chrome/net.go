package chrome

import (
	"context"
	"encoding/base64"
	"fmt"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/sanketsudake/chrome-cdp-cli/internal/eventbuf"
)

// Network-capture bounds. They are defaults, not policy: `net_buffer` and
// `net_max_body` override them (see configureNetCapture).
const (
	// DefaultNetBuffer is the per-target ring size for network records. Half the
	// console's ring because a correlated request record — two header maps, a
	// URL, a request body — is an order of magnitude larger than a console line.
	DefaultNetBuffer = 500

	// DefaultNetMaxBody caps ONE body (request or response), in bytes. A JSON API
	// payload fits comfortably; a bundle or an image download must not be able to
	// blow up either the daemon's memory or the caller's context.
	DefaultNetMaxBody = 64 << 10

	// netTotalFactor derives the cap ACROSS targets from the per-target ring, so
	// a session that touches many tabs is bounded by a constant.
	netTotalFactor = 5

	// netFreshGrace is how long a read waits when capture was only just enabled
	// by this very call (the --no-daemon case). Mirrors consoleFreshGrace.
	netFreshGrace = 250 * time.Millisecond

	// netStreamBacklog is the handoff depth between the CDP event loop and a
	// --follow reader. The subscriber must never block the event loop, so a
	// reader that cannot keep up drops rather than stalls Chrome.
	netStreamBacklog = 256

	// netPartialHistoryNote is the honest answer when nothing was listening to
	// this tab before the command started. Some history does arrive — enabling
	// the domain makes Chrome describe what it still holds for the page, which
	// is a handful of cached resources rather than the session — so a short list
	// without this note would read as "the page made these requests and no
	// others", which is a lie the caller cannot detect.
	netPartialHistoryNote = "partial history: nothing was listening to this tab before this command started, so what appears is " +
		"whatever Chrome still held when capture was enabled, plus what arrived during this command. " +
		"Use the daemon (drop --no-daemon) to retain everything from the moment it attached, or --follow to watch from here."
)

// NetRedacted is the placeholder a redacted header value or URL parameter is
// replaced with. It is part of the envelope contract: a caller can tell "the
// header was absent" from "the header was present and withheld".
const NetRedacted = "<redacted>"

// netRedactHeaders are the header names whose VALUE is always withheld. This
// CLI drives the user's real, logged-in Chrome, so its buffers hold live
// session credentials by construction — redaction is the default and
// --no-redact is the explicit opt-out, never the other way round.
var netRedactHeaders = map[string]bool{
	"authorization":       true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"proxy-authorization": true,
}

// netRedactHeaderRe catches the credential-shaped header names no fixed list
// can enumerate (x-csrf-token, x-amz-security-token, app-secret, …).
var netRedactHeaderRe = regexp.MustCompile(`(?i)token|secret|password`)

// netRedactParamRe matches URL query/fragment parameter NAMES whose value is
// credential-shaped. Per RFC-0003 open question 2: a token in a query string
// leaks exactly as badly as one in a header, and OAuth implicit flows put them
// in the fragment.
var netRedactParamRe = regexp.MustCompile(`(?i)^(` + strings.Join([]string{
	`access_?token`, `refresh_?token`, `id_?token`, `auth_?token`, `bearer`,
	`api_?key`, `apikey`, `x_?api_?key`, `client_?secret`,
	`token`, `secret`, `password`, `passwd`, `pwd`, `credential`, `credentials`,
	`sig`, `signature`, `session`, `session_?id`, `sid`, `jwt`, `auth`,
	`key`, `code`,
}, "|") + `)$`)

// NetTypes are the resource types `--type` accepts, matching Chrome's own
// vocabulary lowercased. Exported so the CLI validates against exactly what the
// filter understands rather than a second, drifting copy of the list.
var NetTypes = []string{
	"document", "stylesheet", "image", "media", "font", "script", "texttrack",
	"xhr", "fetch", "prefetch", "eventsource", "websocket", "manifest",
	"signedexchange", "ping", "cspviolationreport", "preflight", "fedcm", "other",
}

// netRecord is one HTTP request, correlated from the four CDP events that
// describe it (requestWillBeSent / responseReceived / loadingFinished /
// loadingFailed) by request id.
//
// It is the BUFFER's entry type, not the envelope shape: headers and bodies are
// retained here but rendered only when --headers / --body ask for them (see
// render), so the default envelope stays small (RFC-0003 VS-6).
type netRecord struct {
	ID     string
	Method string
	URL    string
	Type   string

	// Started is when the request left the browser; StartedMs is the same
	// instant expressed as milliseconds since capture began on this tab, which
	// is what the envelope carries (a wall-clock epoch would be a 13-digit
	// number that says nothing about where the request sits in the page's life).
	Started   time.Time
	StartedMs int64
	HasStart  bool
	Ended     time.Time
	HasEnd    bool

	Status     int64
	StatusText string
	HasStatus  bool

	RequestSize  int64
	ResponseSize int64
	FromCache    bool

	// NetFailed is a network-LEVEL failure (DNS, CORS, abort): no HTTP status
	// exists. It is distinct from the derived `failed`, which also covers a
	// perfectly delivered 500.
	NetFailed bool
	Error     string

	ReqHeaders  map[string]string
	RespHeaders map[string]string

	// RequestBody arrives inline with requestWillBeSent, so retaining it costs
	// nothing extra and makes US-4 ("what did that button POST?") answerable
	// after the fact. Response bodies are NOT retained — see netFetchBodies.
	//
	// RequestBodyBinary marks a payload that is not text (a multipart image
	// upload). It is retained as empty rather than raw: json.Marshal replaces
	// invalid bytes with U+FFFD, so keeping it would put mojibake in the
	// envelope — the exact thing the response path refuses to do.
	RequestBody          string
	RequestBodyTruncated bool
	RequestBodyBinary    bool

	// Finished marks the request as complete (loadingFinished or loadingFailed
	// arrived). Everything else is `pending`, which is what lets a caller tell
	// "nothing matched" from "not finished yet" (VS-13).
	Finished bool
}

// failed reports the envelope's `failed` field: a network-level failure OR any
// non-2xx status. A request still in flight is not failed.
func (r netRecord) failed() bool {
	if r.NetFailed {
		return true
	}
	return r.HasStatus && (r.Status < 200 || r.Status > 299)
}

// durationMs is how long the request took, or -1 when it has not finished.
func (r netRecord) durationMs() int64 {
	if !r.HasStart || !r.HasEnd {
		return -1
	}
	if d := r.Ended.Sub(r.Started).Milliseconds(); d > 0 {
		return d
	}
	return 0
}

// NetOpts filters a network read. Every field is applied where the buffer lives
// — in the daemon, before the envelope is marshalled — so a chatty page cannot
// flood a caller's context.
type NetOpts struct {
	URL     string        // substring match; a "re:" prefix switches to regex
	Methods []string      // keep only these HTTP methods (empty = all)
	Status  string        // --status spec: 200 | 2xx | >=400 | !2xx (empty = all)
	Types   []string      // keep only these resource types (empty = all)
	Failed  bool          // keep only non-2xx or network-level failures
	Limit   int           // most recent N matches (0 = all)
	Since   time.Duration // only requests newer than this
	Clear   bool          // drop the buffer after reading

	// Rendering. Headers and bodies are ABSENT from the envelope unless asked
	// for; NoRedact turns off the credential redaction that is otherwise always
	// applied to headers and URLs.
	Headers  bool
	Body     bool
	NoRedact bool
}

// NetCond is the request `wait --request` / `net wait` blocks for. It is the
// filter half of NetOpts plus the rendering flags for the one record it returns
// — there is no --limit or --clear, because it answers about a single request.
type NetCond struct {
	URL     string
	Methods []string
	Status  string
	Types   []string
	Failed  bool

	Headers  bool
	Body     bool
	NoRedact bool
}

// opts renders the condition as the filter the buffer understands, so the wait
// and the read share exactly one matching implementation.
func (c NetCond) opts() NetOpts {
	return NetOpts{
		URL: c.URL, Methods: c.Methods, Status: c.Status, Types: c.Types, Failed: c.Failed,
		Headers: c.Headers, Body: c.Body, NoRedact: c.NoRedact, Limit: 1,
	}
}

// describe renders the condition for a timeout message, so a failed wait says
// what it was waiting for rather than just "timed out".
func (c NetCond) describe() string {
	parts := make([]string, 0, 4)
	if c.URL != "" {
		parts = append(parts, "url "+c.URL)
	}
	if len(c.Methods) > 0 {
		parts = append(parts, "method "+strings.Join(c.Methods, "|"))
	}
	if c.Status != "" {
		parts = append(parts, "status "+c.Status)
	}
	if c.Failed {
		parts = append(parts, "failed")
	}
	if len(parts) == 0 {
		return "any request"
	}
	return strings.Join(parts, ", ")
}

// configureNetCapture sizes the network buffers from the resolved config. It
// runs in Connect, BEFORE any tab is attached and therefore before any event can
// arrive, so the buffers are never resized out from under a live listener. Zero
// (an unset config key, or a direct launch in a test) means the default.
func (c *CDP) configureNetCapture(buffer, maxBody int) {
	if buffer <= 0 {
		buffer = DefaultNetBuffer
	}
	if maxBody <= 0 {
		maxBody = DefaultNetMaxBody
	}
	c.netMaxBody = maxBody
	c.net = eventbuf.NewSet[netRecord](buffer, buffer*netTotalFactor)
}

// netBuf returns the network event buffers. Like consoleBuf it takes no lock:
// the field is written only by newCDP and configureNetCapture, both of which run
// before any tab is attached, and startNetCapture is called from on(), which
// already holds c.mu.
func (c *CDP) netBuf() *eventbuf.Set[netRecord] { return c.net }

// listenNet retains the tab's HTTP requests, from ATTACH rather than from the
// first `net` read — the same reason console captures early: the process holding
// the connection has to already be listening when the page makes the request, or
// `net` can only ever report what happened after somebody thought to look, which
// is exactly when it is least useful.
//
// The listener runs on chromedp's event loop and only folds events into an
// in-memory record; it never issues a CDP command. Enabling the domain happens
// after the attach, in enableCapture.
func (c *CDP) listenNet(tctx context.Context, id string) {
	set := c.netBuf()
	maxBody := c.netMaxBody
	// Per-tab epoch, so `started_ms` is a small number relative to when this
	// connection started watching the tab rather than a wall-clock timestamp.
	epoch := time.Now()
	chromedp.ListenTarget(tctx, func(ev any) {
		if key, mutate, ok := netApply(ev, maxBody, epoch); ok {
			set.Upsert(id, key, mutate)
		}
	})
}

// netEnable turns on the domain network capture listens to. It is idempotent, so
// re-running it at read time costs a round trip and buys an honest error when a
// target refuses it.
//
// MaxPostDataSize is set to the body cap so request bodies actually arrive
// inline with requestWillBeSent (Chrome omits them entirely by default).
func netEnable(maxBody int) []chromedp.Action {
	p := network.Enable().WithMaxPostDataSize(int64(maxBody))
	return []chromedp.Action{
		chromedp.ActionFunc(func(ctx context.Context) error { return p.Do(ctx) }),
	}
}

// netApply folds one CDP event into the correlated record, returning the
// correlation key (the CDP request id) and the mutation to apply. It reports
// false for events this buffer does not carry.
//
// It is pure — the epoch and body cap are injected — so correlation and
// out-of-order tolerance are testable without Chrome, which is where the
// regressions hide.
func netApply(ev any, maxBody int, epoch time.Time) (string, func(netRecord, bool) netRecord, bool) {
	switch e := ev.(type) {
	case *network.EventRequestWillBeSent:
		if e.Request == nil {
			return "", nil, false
		}
		key := string(e.RequestID)
		req := e.Request
		ts := netTime(e.Timestamp)
		raw := netPostData(req)
		body, cut, binary := netRequestBody(raw, maxBody)
		typ := netType(e.Type)
		return key, func(r netRecord, _ bool) netRecord {
			r.ID = key
			r.Method = req.Method
			r.URL = req.URL + req.URLFragment
			if typ != "" {
				r.Type = typ
			}
			r.ReqHeaders = netHeaders(req.Headers)
			r.RequestBody, r.RequestBodyTruncated, r.RequestBodyBinary = body, cut, binary
			r.RequestSize = int64(len(raw))
			// A redirect reuses the request id and re-fires this event; keeping
			// the FIRST start keeps duration_ms the whole chain's cost rather
			// than only the last hop's.
			r = netStart(r, ts, epoch)
			return r
		}, true

	case *network.EventResponseReceived:
		if e.Response == nil {
			return "", nil, false
		}
		key := string(e.RequestID)
		resp := e.Response
		ts := netTime(e.Timestamp)
		typ := netType(e.Type)
		return key, func(r netRecord, _ bool) netRecord {
			if r.ID == "" {
				r.ID = key
			}
			if r.URL == "" {
				r.URL = resp.URL
			}
			if typ != "" {
				r.Type = typ
			}
			r.Status, r.StatusText, r.HasStatus = resp.Status, resp.StatusText, true
			r.RespHeaders = netHeaders(resp.Headers)
			if len(r.ReqHeaders) == 0 && len(resp.RequestHeaders) > 0 {
				r.ReqHeaders = netHeaders(resp.RequestHeaders)
			}
			r.FromCache = resp.FromDiskCache || resp.FromServiceWorker || resp.FromPrefetchCache
			if r.ResponseSize == 0 {
				r.ResponseSize = int64(resp.EncodedDataLength)
			}
			return netStart(r, ts, epoch)
		}, true

	case *network.EventLoadingFinished:
		key := string(e.RequestID)
		ts := netTime(e.Timestamp)
		size := int64(e.EncodedDataLength)
		return key, func(r netRecord, _ bool) netRecord {
			if r.ID == "" {
				r.ID = key
			}
			if size > 0 {
				r.ResponseSize = size
			}
			r = netStart(r, ts, epoch)
			r.Ended, r.HasEnd, r.Finished = ts, true, true
			return r
		}, true

	case *network.EventLoadingFailed:
		key := string(e.RequestID)
		ts := netTime(e.Timestamp)
		typ := netType(e.Type)
		text := netFailureText(e)
		return key, func(r netRecord, _ bool) netRecord {
			if r.ID == "" {
				r.ID = key
			}
			if typ != "" && r.Type == "" {
				r.Type = typ
			}
			r.NetFailed, r.Error = true, text
			r = netStart(r, ts, epoch)
			r.Ended, r.HasEnd, r.Finished = ts, true, true
			return r
		}, true
	}
	return "", nil, false
}

// netStart records the request's start the first time any event carries it, so
// whichever event arrives first establishes the timeline and the rest merge.
func netStart(r netRecord, ts time.Time, epoch time.Time) netRecord {
	if r.HasStart {
		return r
	}
	r.Started, r.HasStart = ts, true
	if ms := ts.Sub(epoch).Milliseconds(); ms > 0 {
		r.StartedMs = ms
	}
	return r
}

// netFailureText renders a network-level failure as one line. Chrome reports the
// interesting cases (CORS, a blocking extension) in fields other than errorText,
// which alone would say only "net::ERR_FAILED".
func netFailureText(e *network.EventLoadingFailed) string {
	parts := make([]string, 0, 3)
	if e.ErrorText != "" {
		parts = append(parts, e.ErrorText)
	}
	if e.BlockedReason != "" {
		parts = append(parts, "blocked: "+string(e.BlockedReason))
	}
	if e.CorsErrorStatus != nil && e.CorsErrorStatus.CorsError != "" {
		parts = append(parts, "cors: "+string(e.CorsErrorStatus.CorsError))
	}
	if len(parts) == 0 {
		if e.Canceled {
			return "canceled"
		}
		return "request failed"
	}
	return strings.Join(parts, "; ")
}

// netTime converts a CDP monotonic timestamp to wall time, truncated to the
// millisecond precision the envelope documents.
func netTime(ts *cdp.MonotonicTime) time.Time {
	if ts == nil {
		return time.Now().UTC().Truncate(time.Millisecond)
	}
	return ts.Time().UTC().Truncate(time.Millisecond)
}

// netType maps a CDP resource type onto the lowercase vocabulary --type uses.
func netType(t network.ResourceType) string {
	if t == "" {
		return ""
	}
	return strings.ToLower(string(t))
}

// netHeaders converts CDP's loosely-typed header map to strings. It does NOT
// redact: the buffer holds what the browser saw, and redaction happens at render
// time so --no-redact remains possible without re-capturing.
func netHeaders(h network.Headers) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		switch s := v.(type) {
		case string:
			out[k] = s
		default:
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

// netPostData reassembles a request body from CDP's post-data entries, which
// carry base64 when the payload is not plain text.
func netPostData(req *network.Request) string {
	if len(req.PostDataEntries) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range req.PostDataEntries {
		if e == nil || e.Bytes == "" {
			continue
		}
		if dec, err := base64.StdEncoding.DecodeString(e.Bytes); err == nil {
			b.Write(dec)
			continue
		}
		b.WriteString(e.Bytes)
	}
	return b.String()
}

// netRequestBody bounds a request body for retention, reporting a payload that
// is not text separately from one that is simply absent.
//
// The retained record feeds json.Marshal, which replaces invalid bytes with
// U+FFFD — so a multipart image upload under the cap would arrive in the
// envelope as mojibake, which is precisely what the response path refuses to
// do. Same rule, same reason, both directions.
func netRequestBody(raw string, maxBody int) (text string, truncated, binary bool) {
	if raw == "" {
		return "", false, false
	}
	if !utf8.ValidString(raw) {
		return "", false, true
	}
	text, truncated = eventbuf.TruncateText(raw, maxBody)
	return text, truncated, false
}

// NormalizeNetType maps a user-supplied --type onto the documented vocabulary,
// reporting whether it is one of them. A few obvious aliases (css, img, ws) are
// accepted because they are what people type.
func NormalizeNetType(s string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(s))
	switch n {
	case "css":
		n = "stylesheet"
	case "img":
		n = "image"
	case "doc", "html":
		n = "document"
	case "ws":
		n = "websocket"
	case "xmlhttprequest":
		n = "xhr"
	}
	for _, t := range NetTypes {
		if t == n {
			return n, true
		}
	}
	return "", false
}

// NetStatusMatcher reports whether an HTTP status satisfies a --status spec.
//
// A record with NO status — a network-level failure, or a request still in
// flight — never matches ANY spec, including a negated one. "not 2xx" would
// otherwise silently include every pending request, which is the opposite of
// what somebody asserting on an outcome means.
type NetStatusMatcher func(status int64, hasStatus bool) bool

// ParseNetStatus compiles a --status spec: an exact code (200), a class (2xx),
// a comparison (>=400, !=204), or a negation of any of those (!2xx).
//
// It is pure and runs in the CLI BEFORE anything connects, so a malformed spec
// is usage/exit 2 with Chrome never contacted.
func ParseNetStatus(spec string) (NetStatusMatcher, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return nil, fmt.Errorf("--status needs a spec: an exact code (200), a class (2xx), a comparison (>=400), or a negation (!2xx)")
	}
	// "!2xx" negates; "!=204" is the not-equal comparison, not a negation of
	// "=204" — so only a "!" that is NOT followed by "=" starts a negation.
	negate := false
	if strings.HasPrefix(s, "!") && !strings.HasPrefix(s, "!=") {
		negate, s = true, strings.TrimSpace(s[1:])
	}
	m, err := parseNetStatusAtom(s)
	if err != nil {
		return nil, err
	}
	if !negate {
		return m, nil
	}
	return func(status int64, has bool) bool { return has && !m(status, true) }, nil
}

// parseNetStatusAtom compiles the un-negated half of a --status spec.
func parseNetStatusAtom(s string) (NetStatusMatcher, error) {
	bad := fmt.Errorf("invalid --status %q: want an exact code (200), a class (2xx), a comparison (>=400), or a negation (!2xx)", s)
	// Longest operators first, so ">=" is not read as ">" followed by garbage.
	for _, op := range []string{">=", "<=", "==", "!=", ">", "<"} {
		rest, ok := strings.CutPrefix(s, op)
		if !ok {
			continue
		}
		n, err := netStatusCode(strings.TrimSpace(rest))
		if err != nil {
			return nil, bad
		}
		return func(status int64, has bool) bool {
			if !has {
				return false
			}
			switch op {
			case ">=":
				return status >= n
			case "<=":
				return status <= n
			case "==":
				return status == n
			case "!=":
				return status != n
			case ">":
				return status > n
			default:
				return status < n
			}
		}, nil
	}
	if len(s) == 3 && strings.EqualFold(s[1:], "xx") {
		d := s[0]
		if d < '1' || d > '5' {
			return nil, bad
		}
		lo := int64(d-'0') * 100
		return func(status int64, has bool) bool { return has && status >= lo && status < lo+100 }, nil
	}
	n, err := netStatusCode(s)
	if err != nil {
		return nil, bad
	}
	return func(status int64, has bool) bool { return has && status == n }, nil
}

// netStatusCode parses an exact three-digit HTTP status code. Three digits, not
// "any integer": "20" and "1234" are typos, and silently accepting them would
// produce a filter that matches nothing with no explanation.
func netStatusCode(s string) (int64, error) {
	if len(s) != 3 {
		return 0, fmt.Errorf("not a three-digit status code: %q", s)
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 100 || n > 599 {
		return 0, fmt.Errorf("not a three-digit status code: %q", s)
	}
	return int64(n), nil
}

// NetURLMatcher compiles a --url spec: a substring, or a regex behind a "re:"
// prefix. A nil matcher means "every URL".
func NetURLMatcher(spec string) (func(string) bool, error) {
	if spec == "" {
		return nil, nil
	}
	if re, ok := strings.CutPrefix(spec, "re:"); ok {
		c, err := regexp.Compile(re)
		if err != nil {
			return nil, fmt.Errorf("--url is not a valid regex after the re: prefix: %w", err)
		}
		return c.MatchString, nil
	}
	return func(u string) bool { return strings.Contains(u, spec) }, nil
}

// netKeep composes the filter options into one predicate, or nil when every
// record passes. now is injected so the --since cutoff is testable.
//
// The CLI validates these flags before connecting; recompiling here is defence
// in depth for the daemon RPC, whose arg decoders are deliberately lenient.
func netKeep(opts NetOpts, now time.Time) (func(netRecord) bool, error) {
	urlMatch, err := NetURLMatcher(opts.URL)
	if err != nil {
		return nil, err
	}
	var status NetStatusMatcher
	if opts.Status != "" {
		if status, err = ParseNetStatus(opts.Status); err != nil {
			return nil, err
		}
	}
	var methods map[string]bool
	if len(opts.Methods) > 0 {
		methods = make(map[string]bool, len(opts.Methods))
		for _, m := range opts.Methods {
			methods[strings.ToUpper(strings.TrimSpace(m))] = true
		}
	}
	var types map[string]bool
	if len(opts.Types) > 0 {
		types = make(map[string]bool, len(opts.Types))
		for _, t := range opts.Types {
			if n, ok := NormalizeNetType(t); ok {
				types[n] = true
			}
		}
	}
	var cutoff time.Time
	if opts.Since > 0 {
		cutoff = now.Add(-opts.Since)
	}
	if urlMatch == nil && status == nil && methods == nil && types == nil && !opts.Failed && cutoff.IsZero() {
		return nil, nil
	}
	return func(r netRecord) bool {
		if urlMatch != nil && !urlMatch(r.URL) {
			return false
		}
		if methods != nil && !methods[strings.ToUpper(r.Method)] {
			return false
		}
		if types != nil && !types[r.Type] {
			return false
		}
		if status != nil && !status(r.Status, r.HasStatus) {
			return false
		}
		if opts.Failed && !r.failed() {
			return false
		}
		if !cutoff.IsZero() && !r.Started.After(cutoff) {
			return false
		}
		return true
	}, nil
}

// netPendingKeep is the filter `pending` is counted against: the caller's own
// filter minus its OUTCOME terms.
//
// `pending` exists so a caller can tell "nothing matched" from "not finished
// yet", which only works if it is scoped to what the caller asked about.
// Counting every unfinished request regardless of the filter made a permanently
// open SSE stream or a long poll — which every real app has — report
// `pending >= 1` forever on `net --url /api/save`, so the signal never went
// quiet and stopped meaning anything.
//
// Status and --failed are dropped rather than applied: a request still in flight
// has no status, so keeping them would make `pending` a constant 0 for exactly
// the reads that ask about an outcome.
func netPendingKeep(opts NetOpts, now time.Time) (func(netRecord) bool, error) {
	opts.Status, opts.Failed = "", false
	return netKeep(opts, now)
}

// netBody is one fetched response body plus why it might be missing.
type netBody struct {
	Text      string
	Available bool
	Truncated bool
}

// netTextBody renders a fetched payload as an envelope body, reporting false
// for anything that is not text.
//
// The validity check is on the ORIGINAL payload, before the cap: checking the
// truncated text instead made the answer depend on SIZE. A 10 KB image was
// (correctly) reported unavailable, while the same image at 100 KB was cut to
// 64 KB — and, before the truncation fix, to "" — which passed the text check
// and was emitted as an empty-but-present body. Identical content, opposite
// answers, decided by nothing the caller can see.
func netTextBody(raw string, maxBody int) (netBody, bool) {
	if !utf8.ValidString(raw) {
		// A binary payload (an image, a font) is not text and must not be
		// smuggled into the envelope as mojibake or as a bogus empty string.
		return netBody{}, false
	}
	text, cut := eventbuf.TruncateText(raw, maxBody)
	return netBody{Text: text, Available: true, Truncated: cut}, true
}

// render turns a retained record into the envelope object.
//
// Header and body keys are ABSENT (not null) unless requested, so the default
// listing stays small; `status`, `duration_ms`, and `error` are null rather than
// absent, because "no status yet" is information the caller needs.
func (r netRecord) render(opts NetOpts, body netBody) map[string]any {
	shown := r.URL
	if !opts.NoRedact {
		shown = RedactURL(shown)
	}
	out := map[string]any{
		"id":            r.ID,
		"method":        r.Method,
		"url":           shown,
		"type":          r.Type,
		"status":        nil,
		"status_text":   r.StatusText,
		"started_ms":    r.StartedMs,
		"started_at":    nil,
		"duration_ms":   nil,
		"request_size":  r.RequestSize,
		"response_size": r.ResponseSize,
		"from_cache":    r.FromCache,
		"failed":        r.failed(),
		"error":         nil,
		"pending":       !r.Finished,
	}
	if r.HasStatus {
		out["status"] = r.Status
	}
	if r.HasStart {
		// RFC-0017: an absolute instant HAR requires and the listing's own
		// started_ms (relative to a per-tab epoch) cannot supply.
		out["started_at"] = r.Started.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	if d := r.durationMs(); d >= 0 {
		out["duration_ms"] = d
	}
	if r.Error != "" {
		out["error"] = r.Error
	}
	if opts.Headers {
		out["request_headers"] = RedactHeaders(r.ReqHeaders, opts.NoRedact)
		out["response_headers"] = RedactHeaders(r.RespHeaders, opts.NoRedact)
	}
	if opts.Body {
		out["request_body"] = nil
		if r.RequestBody != "" {
			out["request_body"] = RedactBody(r.RequestBody, opts.NoRedact)
		}
		if r.RequestBodyBinary {
			// Same contract as body_unavailable, for the other direction: the
			// body existed, and what it held is not something we can show.
			out["request_body_unavailable"] = true
		}
		out["response_body"] = nil
		if body.Available {
			out["response_body"] = RedactBody(body.Text, opts.NoRedact)
		} else {
			// A body the page has navigated away from is gone, and saying so is
			// more useful than failing the whole read (VS-14).
			out["body_unavailable"] = true
		}
		if body.Truncated || r.RequestBodyTruncated {
			out["body_truncated"] = true
		}
	}
	return out
}

// netURLHeaders are the headers whose VALUE is a URL. Their names carry no hint
// of a credential, so the name-based rules never fire on them — but a 302 to
// "…/callback?code=SECRET" leaks the authorization code just as thoroughly as an
// Authorization header would, and that redirect is how every OAuth flow ends.
// Their values go through RedactURL instead of being withheld wholesale, so a
// redirect stays diagnosable.
var netURLHeaders = map[string]bool{
	"location":         true,
	"content-location": true,
	"referer":          true,
	"referrer":         true,
}

// RedactHeaders returns a copy of h with credential-shaped values replaced.
// noRedact returns the map unchanged, which is the ONLY way a live session token
// reaches the envelope.
func RedactHeaders(h map[string]string, noRedact bool) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		switch {
		case noRedact:
		case RedactedHeaderName(k):
			v = NetRedacted
		case netURLHeaders[strings.ToLower(strings.TrimSpace(k))]:
			v = RedactURL(v)
		}
		out[k] = v
	}
	return out
}

// RedactedHeaderName reports whether a header's value must be withheld: one of
// the named credential headers, or any name containing token/secret/password.
func RedactedHeaderName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if netRedactHeaders[n] {
		return true
	}
	return netRedactHeaderRe.MatchString(n)
}

// RedactURL replaces the values of credential-shaped query and fragment
// parameters, preserving parameter order and encoding — re-encoding through
// url.Values would sort them and rewrite escapes, making the reported URL differ
// from the one the page actually requested.
func RedactURL(raw string) string {
	if raw == "" {
		return raw
	}
	main, frag, hasFrag := strings.Cut(raw, "#")
	base, query, hasQuery := strings.Cut(main, "?")
	out := base
	if hasQuery {
		out += "?" + redactParams(query)
	}
	if hasFrag {
		// OAuth implicit flows return the token in the fragment, so it gets the
		// same treatment; a plain "#section" has no "=" and is left alone.
		//
		// A hash ROUTER puts a whole URL in the fragment
		// ("#/callback?access_token=…"), so the fragment gets the same
		// path/query split the main URL does. Without it the single parameter
		// parsed as the name "/callback?access_token", which the anchored
		// pattern rejects — and the token was emitted verbatim, in exactly the
		// OAuth-implicit case fragment handling was added for.
		fpath, fquery, hasFragQuery := strings.Cut(frag, "?")
		if hasFragQuery {
			out += "#" + fpath + "?" + redactParams(fquery)
		} else {
			out += "#" + redactParams(frag)
		}
	}
	return out
}

// redactParams rewrites the values of token-shaped parameters in one
// ampersand-separated parameter list.
func redactParams(q string) string {
	if q == "" {
		return q
	}
	parts := strings.Split(q, "&")
	for i, p := range parts {
		name, _, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		if RedactedParamName(name) {
			parts[i] = name + "=" + NetRedacted
		}
	}
	return strings.Join(parts, "&")
}

// RedactedParamName reports whether a URL parameter or request/response body
// FIELD name is credential-shaped. One predicate for both, because a value is
// no less a secret for having travelled in a POST body than in a query string.
func RedactedParamName(name string) bool {
	n := strings.TrimSpace(name)
	if u, err := netUnescape(n); err == nil {
		n = u
	}
	return netRedactParamRe.MatchString(n)
}

// netRedactJSONRe matches one JSON member with a STRING value, capturing the
// member name and the separator so a replacement can keep the document's own
// spacing and escaping. Deliberately a rewrite rather than a decode/re-encode:
// a body that hit the size cap is not parseable JSON any more, and re-encoding
// would reorder members and rewrite escapes, so the reported payload would stop
// being the one the page actually sent.
var netRedactJSONRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"(\s*:\s*)"(?:[^"\\]|\\.)*"`)

// RedactBody withholds the values of credential-shaped fields in a request or
// response body, for the form-encoded and JSON shapes credentials actually
// travel in. noRedact returns the body unchanged.
//
// RFC-0003 specifies redaction for headers and URLs only, so this goes beyond
// it deliberately. The premise of the whole tool is that it drives the user's
// real, logged-in browser: `net --body` on a login POST printed `password=…` in
// clear, while the SAME credential in `?password=…` was withheld — the same
// secret, opposite answers, decided by nothing but the HTTP method. US-5 asks
// for bodies that do "not spill tokens or PII into logs by default", and this
// is what that costs.
//
// A body in any other encoding (multipart/form-data, protobuf, a bare token) is
// passed through: there is no field structure to key on, and guessing would
// either miss or mangle. `--body` remains an explicit opt-in either way.
func RedactBody(body string, noRedact bool) string {
	if noRedact || body == "" {
		return body
	}
	switch {
	case netLooksJSON(body):
		return netRedactJSONRe.ReplaceAllStringFunc(body, func(m string) string {
			g := netRedactJSONRe.FindStringSubmatch(m)
			if !RedactedParamName(g[1]) {
				return m
			}
			return `"` + g[1] + `"` + g[2] + `"` + NetRedacted + `"`
		})
	case netLooksFormEncoded(body):
		return redactParams(body)
	}
	return body
}

// netLooksJSON reports whether a body is a JSON object or array.
func netLooksJSON(body string) bool {
	t := strings.TrimLeft(body, " \t\r\n")
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
}

// netLooksFormEncoded reports whether a body is an application/x-www-form-
// urlencoded parameter list, judged on its first field: a name with no
// whitespace or JSON punctuation, followed by "=". The content-type header
// would be a stronger signal, but it is not always sent and this has to hold
// for a truncated body too.
func netLooksFormEncoded(body string) bool {
	head, _, _ := strings.Cut(body, "&")
	name, _, ok := strings.Cut(head, "=")
	if !ok || name == "" {
		return false
	}
	return !strings.ContainsAny(name, " \t\r\n\"'{}[]<>")
}

// netUnescape decodes a percent-encoded parameter name, so "api%5Fkey" is
// recognised as api_key rather than sailing past the pattern.
func netUnescape(s string) (string, error) {
	if !strings.ContainsAny(s, "%+") {
		return s, nil
	}
	return neturl.QueryUnescape(s)
}

// Net returns the buffered network records for a tab, filtered server-side.
func (c *CDP) Net(ctx context.Context, id string, opts NetOpts) (any, error) {
	keep, err := netKeep(opts, time.Now())
	if err != nil {
		return nil, err
	}
	// Ask BEFORE attaching: on() is what starts capture, so afterwards every tab
	// looks like it was always being watched.
	fresh := !c.attached(id)
	if err := c.run(ctx, id, netEnable(c.netMaxBody)...); err != nil {
		return nil, fmt.Errorf("network capture could not be enabled on this tab: %w", err)
	}
	if fresh {
		// Nothing was alive to receive this tab's earlier requests. Report what
		// arrives now, and say so.
		settle(ctx, netFreshGrace)
	}
	pendingKeep, err := netPendingKeep(opts, time.Now())
	if err != nil {
		return nil, err
	}
	// pending is counted in the SAME pass as the filter: Query visits every live
	// entry exactly once under the buffer's lock, so the count cannot drift from
	// the matches it is reported alongside.
	pending := 0
	q := eventbuf.Query[netRecord]{
		Keep: func(r netRecord) bool {
			if !r.Finished && (pendingKeep == nil || pendingKeep(r)) {
				pending++
			}
			return keep == nil || keep(r)
		},
		Limit: opts.Limit,
		Clear: opts.Clear,
	}
	res := c.netBuf().Query(id, q)
	out := c.netResult(ctx, id, res, pending, opts)
	if fresh {
		// `buffered` is a real count of what is held, not a claim about
		// completeness; the note is what says the history is Chrome's window
		// rather than ours.
		out["note"] = netPartialHistoryNote
	}
	return out, nil
}

// netResult renders a buffer answer as the envelope's result payload, fetching
// response bodies only when --body asked for them.
func (c *CDP) netResult(ctx context.Context, id string, res eventbuf.Result[netRecord], pending int, opts NetOpts) map[string]any {
	bodies := map[string]netBody{}
	if opts.Body {
		bodies = c.netFetchBodies(ctx, id, res.Entries)
	}
	reqs := make([]any, 0, len(res.Entries))
	for _, r := range res.Entries {
		reqs = append(reqs, r.render(opts, bodies[r.ID]))
	}
	return map[string]any{
		"requests":  reqs,
		"count":     res.Count,
		"buffered":  res.Buffered,
		"dropped":   res.Dropped,
		"truncated": res.Truncated,
		"pending":   pending,
	}
}

// netFetchBodies pulls response bodies with Network.getResponseBody at READ
// time, for exactly the records being reported.
//
// Buffering every body would multiply the daemon's memory by orders of magnitude
// and retain payloads the user never asked to see. The cost is that a body may
// already be gone (the page navigated, or Chrome evicted it), which is reported
// as an unavailable body rather than as a failed read — a partial answer beats
// no answer (VS-14).
func (c *CDP) netFetchBodies(ctx context.Context, id string, recs []netRecord) map[string]netBody {
	out := make(map[string]netBody, len(recs))
	actions := make([]chromedp.Action, 0, len(recs))
	for _, r := range recs {
		if !r.Finished || !r.HasStatus || r.NetFailed {
			continue // nothing was delivered; there is no body to ask for
		}
		reqID := network.RequestID(r.ID)
		key := r.ID
		actions = append(actions, chromedp.ActionFunc(func(actx context.Context) error {
			raw, err := network.GetResponseBody(reqID).Do(actx)
			if err != nil {
				return nil // one gone body must not abort the whole read
			}
			if b, ok := netTextBody(string(raw), c.netMaxBody); ok {
				out[key] = b
			}
			return nil
		}))
	}
	if len(actions) == 0 {
		return out
	}
	// A failure here (the tab went away mid-read) leaves the bodies unavailable,
	// which render already reports honestly.
	_ = c.run(ctx, id, actions...)
	return out
}

// NetStream emits one payload per COMPLETED request as it finishes, until ctx
// ends. Reaching the caller's deadline is the normal way a --follow window
// closes, so it is a nil return, not an error.
func (c *CDP) NetStream(ctx context.Context, id string, opts NetOpts, emit func(any) error) error {
	keep, err := netKeep(opts, time.Now())
	if err != nil {
		return err
	}
	if err := c.run(ctx, id, netEnable(c.netMaxBody)...); err != nil {
		return fmt.Errorf("network capture could not be enabled on this tab: %w", err)
	}
	set := c.netBuf()
	if opts.Clear {
		set.Clear(id)
	}
	// The subscriber runs on the buffer's lock (and thus chromedp's event loop):
	// hand off without blocking, and drop rather than stall Chrome if the reader
	// cannot keep up.
	ch := make(chan netRecord, netStreamBacklog)
	stop := set.Subscribe(id, func(r netRecord) {
		// Every event on a request notifies, so a record would otherwise be
		// emitted three or four times as it is built up. A follow reports
		// OUTCOMES, so only the completed shape is worth a line.
		if !r.Finished {
			return
		}
		select {
		case ch <- r:
		default:
		}
	})
	defer stop()

	seen := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-ch:
			// An out-of-order responseReceived after loadingFinished mutates the
			// record again; the request has still only completed once.
			if seen[r.ID] {
				continue
			}
			if keep != nil && !keep(r) {
				continue
			}
			seen[r.ID] = true
			buffered, dropped := set.Stat(id)
			bodies := map[string]netBody{}
			if opts.Body {
				bodies = c.netFetchBodies(ctx, id, []netRecord{r})
			}
			payload := map[string]any{
				"requests": []any{r.render(opts, bodies[r.ID])}, "count": 1,
				"buffered": buffered, "dropped": dropped, "truncated": false,
			}
			if err := emit(payload); err != nil {
				return err
			}
		}
	}
}

// NetWait blocks until a request matching cond completes.
//
// It subscribes BEFORE scanning the buffer, then answers from the already-
// buffered records first. Both halves matter: scanning first would lose a
// request that completed between the scan and the subscription, and not scanning
// at all would time out on a request that completed between the action that
// triggered it and this call — the race a naive implementation loses (VS-10).
func (c *CDP) NetWait(ctx context.Context, id string, cond NetCond) (map[string]any, error) {
	opts := cond.opts()
	keep, err := netKeep(opts, time.Now())
	if err != nil {
		return nil, err
	}
	if err := c.run(ctx, id, netEnable(c.netMaxBody)...); err != nil {
		return nil, fmt.Errorf("network capture could not be enabled on this tab: %w", err)
	}
	set := c.netBuf()
	ch := make(chan netRecord, netStreamBacklog)
	stop := set.Subscribe(id, func(r netRecord) {
		if !r.Finished {
			return
		}
		select {
		case ch <- r:
		default:
		}
	})
	defer stop()

	hit := set.Query(id, eventbuf.Query[netRecord]{
		Keep:  func(r netRecord) bool { return r.Finished && (keep == nil || keep(r)) },
		Limit: 1,
	})
	if hit.Count > 0 {
		return c.netWaitResult(ctx, id, hit.Entries[0], opts), nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("no request matching %s completed before the timeout: %w", cond.describe(), ctx.Err())
		case r := <-ch:
			if keep != nil && !keep(r) {
				continue
			}
			return c.netWaitResult(ctx, id, r, opts), nil
		}
	}
}

// netWaitResult renders the one matched request under `request`, in the same
// shape a `net` listing uses.
func (c *CDP) netWaitResult(ctx context.Context, id string, r netRecord, opts NetOpts) map[string]any {
	bodies := map[string]netBody{}
	if opts.Body {
		bodies = c.netFetchBodies(ctx, id, []netRecord{r})
	}
	return map[string]any{"matched": true, "request": r.render(opts, bodies[r.ID])}
}
