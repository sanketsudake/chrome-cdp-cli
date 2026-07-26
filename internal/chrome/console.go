package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	cdplog "github.com/chromedp/cdproto/log"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/sanketsudake/chrome-cdp-cli/internal/eventbuf"
)

// Capture bounds. They are defaults, not policy: `console_buffer` and
// `console_max_entry` override them (see configureCapture), because the right
// size depends on how chatty the app under automation is.
const (
	// DefaultConsoleBuffer is the per-target ring size. 1000 lines is a few
	// hundred KB at the entry cap and covers the "what just broke" window that
	// motivates the verb, without retaining a day of framework chatter.
	DefaultConsoleBuffer = 1000

	// DefaultConsoleMaxEntry caps one message's text. A single console.log of a
	// megabyte-sized object is a real thing, and it must not be able to blow up
	// either the daemon's memory or the caller's context.
	DefaultConsoleMaxEntry = 8 << 10

	// consoleTotalFactor derives the cap ACROSS targets from the per-target
	// ring: a session that touches many tabs is bounded by a constant rather
	// than growing linearly in tabs.
	consoleTotalFactor = 5

	// consoleStackFrames caps a retained stack. Deep enough to locate the throw,
	// shallow enough that a recursion blowup is not retained frame by frame.
	consoleStackFrames = 50

	// consoleFreshGrace is how long a read waits when capture was only just
	// enabled by this very call (the --no-daemon case): long enough to drain
	// what enabling the domains flushed, short enough not to be the "silent
	// block" the RFC rules out for a non-follow read.
	consoleFreshGrace = 250 * time.Millisecond

	// consoleStreamBacklog is the handoff depth between the CDP event loop and
	// a --follow reader. The subscriber must never block the event loop, so a
	// reader that cannot keep up drops rather than stalls Chrome.
	consoleStreamBacklog = 256

	// noRetainedHistoryNote is the honest answer when nothing was listening
	// before this command started. Reporting an empty list without it would let
	// a caller conclude the page was silent.
	noRetainedHistoryNote = "no retained history: nothing was listening to this tab before this command started, " +
		"so only messages emitted during it can appear. Use the daemon (drop --no-daemon) to retain history, or --follow to watch from here."
)

// consoleMessage is one retained console line or uncaught exception.
//
// Its JSON tags are public API: they are the objects under `result.messages` in
// the envelope, which both humans and the Claude skill parse.
type consoleMessage struct {
	Level string `json:"level"`
	// Source separates a console.* call from an uncaught exception and from a
	// browser-level log entry, which `level` alone cannot: an uncaught
	// TypeError and a console.error both arrive as level "error", but only one
	// of them means the page threw.
	Source string    `json:"source"`
	Text   string    `json:"text"`
	URL    string    `json:"url,omitempty"`
	Line   int64     `json:"line,omitempty"`
	Column int64     `json:"column,omitempty"`
	TS     time.Time `json:"ts"`
	Stack  []string  `json:"stack,omitempty"`
	// TextTruncated marks text cut by the per-entry cap. It is deliberately not
	// named `truncated`: the result-level `truncated` means "--limit cut the
	// message list", and conflating the two would make both unreadable.
	TextTruncated bool `json:"text_truncated,omitempty"`
}

// The `source` values a message can carry.
const (
	consoleSourceConsole   = "console"   // a console.* call
	consoleSourceException = "exception" // an uncaught exception (Runtime.exceptionThrown)
	consoleSourceLog       = "log"       // a browser-level log entry (Log.entryAdded)
)

// ConsoleOpts filters a console read. Every field is applied where the buffer
// lives — in the daemon, before the envelope is marshalled — so a chatty app
// cannot flood a caller's context (RFC-0002 US-5).
type ConsoleOpts struct {
	Grep   string        // keep only messages whose text matches this regex
	Levels []string      // keep only these levels (empty = all)
	Limit  int           // most recent N matches (0 = all)
	Since  time.Duration // only messages newer than this
	Clear  bool          // drop the buffer after reading
}

// configureCapture sizes the event buffers from the resolved config. It runs in
// Connect, BEFORE any tab is attached and therefore before any event can
// arrive, so the buffers are never resized out from under a live listener.
// Zero (an unset config key, or a direct launch in a test) means the default.
func (c *CDP) configureCapture(buffer, maxEntry int) {
	if buffer <= 0 {
		buffer = DefaultConsoleBuffer
	}
	if maxEntry <= 0 {
		maxEntry = DefaultConsoleMaxEntry
	}
	c.consoleMaxEntry = maxEntry
	c.console = eventbuf.NewSet[consoleMessage](buffer, buffer*consoleTotalFactor)
}

// consoleBuf returns the console event buffers.
//
// It takes no lock, deliberately: the field is written only by newCDP and
// configureCapture, both of which run before any tab is attached and therefore
// before any reader exists. Locking here would deadlock instead — startCapture
// is called from on(), which already holds c.mu.
func (c *CDP) consoleBuf() *eventbuf.Set[consoleMessage] { return c.console }

// attached reports whether this connection is already holding tab id open —
// which is the same question as "was anything listening to it before now".
func (c *CDP) attached(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.tabs[id]
	return ok
}

// startCapture turns on the CDP event capture for a freshly attached tab. It is
// called from on(), under c.mu, exactly once per tab.
//
// This is the hook every event-backed verb shares; RFC-0003's network capture
// starts here too.
func (c *CDP) startCapture(tctx context.Context, id string) {
	c.startConsoleCapture(tctx, id)
	c.startNetCapture(tctx, id)
}

// startConsoleCapture retains console output and uncaught exceptions for a tab.
//
// It runs at ATTACH, not at the first `console` read, and that is the whole
// design: the process holding the connection has to already be listening when
// the page throws, or `console` can only ever report what happened after
// somebody thought to look — which is exactly when it is least useful.
//
// The listener is registered on the TAB context (long-lived), the same lifetime
// as the buffer, rather than on a per-command context whose cancel would take
// the subscription with it. It runs on chromedp's event loop, so it only ever
// appends to an in-memory buffer and never issues a CDP command.
//
// Enabling the domains is best-effort HERE, because every verb attaches: a
// target that refuses Runtime/Log (a chrome:// page, say) must not have `click`
// broken by a console feature it never used. Console re-enables at read time
// and does report the failure, so the honesty is paid for where it is asked
// for.
func (c *CDP) startConsoleCapture(tctx context.Context, id string) {
	set := c.consoleBuf()
	maxEntry := c.consoleMaxEntry
	chromedp.ListenTarget(tctx, func(ev any) {
		if m, ok := consoleEvent(ev, maxEntry); ok {
			set.Add(id, m)
		}
	})
	// Bounded so a wedged target cannot hang the attach; cancelling this child
	// never closes the tab (only chromedp's own NewContext contexts do). The
	// error is swallowed HERE because every verb attaches — but Console
	// re-enables and does report it, so a tab whose capture cannot start
	// answers `cdp_error` rather than a silently empty console.
	ectx, cancel := context.WithTimeout(tctx, 5*time.Second)
	defer cancel()
	_ = chromedp.Run(ectx, consoleEnable()...)
}

// consoleEnable turns on the domains console capture listens to. Both calls are
// idempotent, so re-running them at read time costs a round trip and buys an
// honest error when a target refuses them.
func consoleEnable() []chromedp.Action {
	return []chromedp.Action{
		chromedp.ActionFunc(func(ctx context.Context) error { return cdpruntime.Enable().Do(ctx) }),
		chromedp.ActionFunc(func(ctx context.Context) error { return cdplog.Enable().Do(ctx) }),
	}
}

// Console returns the buffered console messages for a tab, filtered server-side.
func (c *CDP) Console(ctx context.Context, id string, opts ConsoleOpts) (any, error) {
	keep, err := consoleKeep(opts, time.Now())
	if err != nil {
		return nil, err
	}
	// Ask BEFORE attaching: on() is what starts capture, so afterwards every
	// tab looks like it was always being watched.
	fresh := !c.attached(id)
	if err := c.run(ctx, id, consoleEnable()...); err != nil {
		return nil, fmt.Errorf("console capture could not be enabled on this tab: %w", err)
	}
	q := eventbuf.Query[consoleMessage]{Keep: keep, Limit: opts.Limit, Clear: opts.Clear}
	if fresh {
		// Nothing was alive to receive this tab's earlier events. Report what
		// arrives now, and say so — an empty list with no note would read as
		// "the page was quiet", which is a lie the caller cannot detect.
		settle(ctx, consoleFreshGrace)
		res := consoleResult(c.consoleBuf().Query(id, q))
		res["buffered"] = 0
		res["note"] = noRetainedHistoryNote
		return res, nil
	}
	return consoleResult(c.consoleBuf().Query(id, q)), nil
}

// ConsoleStream emits one payload per message as it arrives, until ctx ends.
// Reaching the caller's deadline is the normal way a --follow window closes, so
// it is a nil return, not an error.
func (c *CDP) ConsoleStream(ctx context.Context, id string, opts ConsoleOpts, emit func(any) error) error {
	keep, err := consoleKeep(opts, time.Now())
	if err != nil {
		return err
	}
	if err := c.run(ctx, id, consoleEnable()...); err != nil {
		return fmt.Errorf("console capture could not be enabled on this tab: %w", err)
	}
	set := c.consoleBuf()
	if opts.Clear {
		set.Clear(id)
	}
	// The subscriber runs on the buffer's lock (and thus chromedp's event
	// loop): hand off without blocking, and drop rather than stall Chrome if
	// the reader cannot keep up.
	ch := make(chan consoleMessage, consoleStreamBacklog)
	stop := set.Subscribe(id, func(m consoleMessage) {
		select {
		case ch <- m:
		default:
		}
	})
	defer stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-ch:
			if keep != nil && !keep(m) {
				continue
			}
			buffered, dropped := set.Stat(id)
			payload := map[string]any{
				"messages": []any{m}, "count": 1,
				"buffered": buffered, "dropped": dropped, "truncated": false,
			}
			if err := emit(payload); err != nil {
				return err
			}
		}
	}
}

// consoleResult renders a buffer answer as the envelope's result payload.
func consoleResult(res eventbuf.Result[consoleMessage]) map[string]any {
	msgs := make([]any, 0, len(res.Entries))
	for _, m := range res.Entries {
		msgs = append(msgs, m)
	}
	return map[string]any{
		"messages":  msgs,
		"count":     res.Count,
		"buffered":  res.Buffered,
		"dropped":   res.Dropped,
		"truncated": res.Truncated,
	}
}

// settle waits up to d, or until ctx ends — whichever comes first.
func settle(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// consoleKeep compiles the filter options into one predicate, or nil when every
// message passes. now is injected so the --since cutoff is testable.
//
// The CLI validates these flags before connecting; recompiling here is defence
// in depth for the daemon RPC, whose arg decoders are deliberately lenient.
func consoleKeep(opts ConsoleOpts, now time.Time) (func(consoleMessage) bool, error) {
	var re *regexp.Regexp
	if opts.Grep != "" {
		var err error
		if re, err = regexp.Compile(opts.Grep); err != nil {
			return nil, fmt.Errorf("--grep is not a valid regex: %w", err)
		}
	}
	var levels map[string]bool
	if len(opts.Levels) > 0 {
		levels = make(map[string]bool, len(opts.Levels))
		for _, l := range opts.Levels {
			if n, ok := NormalizeConsoleLevel(l); ok {
				levels[n] = true
			}
		}
	}
	var cutoff time.Time
	if opts.Since > 0 {
		cutoff = now.Add(-opts.Since)
	}
	if re == nil && levels == nil && cutoff.IsZero() {
		return nil, nil
	}
	return func(m consoleMessage) bool {
		if levels != nil && !levels[m.Level] {
			return false
		}
		if re != nil && !re.MatchString(m.Text) {
			return false
		}
		if !cutoff.IsZero() && !m.TS.After(cutoff) {
			return false
		}
		return true
	}, nil
}

// ConsoleLevels are the level names `--level` accepts, in increasing severity.
// Exported so the CLI validates against exactly what the filter understands
// rather than a second, drifting copy of the list.
var ConsoleLevels = []string{"debug", "log", "info", "warn", "error"}

// NormalizeConsoleLevel maps a user- or Chrome-supplied level name onto the
// five documented levels, reporting whether it is one of them. Chrome's own
// vocabulary is wider than the CLI's (`warning`, `verbose`, `assert`, `trace`),
// so both ends go through here and a `--level warning` still means what the
// user obviously meant.
func NormalizeConsoleLevel(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "verbose":
		return "debug", true
	case "log":
		return "log", true
	case "info":
		return "info", true
	case "warn", "warning":
		return "warn", true
	case "error", "assert":
		return "error", true
	}
	return "", false
}

// consoleAPILevel maps a Runtime.consoleAPICalled type onto a message level.
// The call types that are not levels at all (table, group, count, …) are
// reported as "log", which is where a developer sees them in DevTools.
func consoleAPILevel(t cdpruntime.APIType) string {
	if l, ok := NormalizeConsoleLevel(string(t)); ok {
		return l
	}
	return "log"
}

// consoleEvent converts a CDP event into a retained message, reporting false
// for events this buffer does not carry.
func consoleEvent(ev any, maxEntry int) (consoleMessage, bool) {
	switch e := ev.(type) {
	case *cdpruntime.EventConsoleAPICalled:
		m := consoleMessage{
			Level:  consoleAPILevel(e.Type),
			Source: consoleSourceConsole,
			Text:   renderArgs(e.Args),
			TS:     eventTime(e.Timestamp),
			Stack:  formatStack(e.StackTrace),
		}
		if e.StackTrace != nil && len(e.StackTrace.CallFrames) > 0 {
			top := e.StackTrace.CallFrames[0]
			m.URL, m.Line, m.Column = top.URL, top.LineNumber+1, top.ColumnNumber+1
		}
		return capText(m, maxEntry), true

	case *cdpruntime.EventExceptionThrown:
		d := e.ExceptionDetails
		if d == nil {
			return consoleMessage{}, false
		}
		m := consoleMessage{
			Level:  "error",
			Source: consoleSourceException,
			Text:   exceptionText(d),
			URL:    d.URL,
			TS:     eventTime(e.Timestamp),
			Stack:  formatStack(d.StackTrace),
		}
		if m.URL == "" && d.StackTrace != nil && len(d.StackTrace.CallFrames) > 0 {
			m.URL = d.StackTrace.CallFrames[0].URL
		}
		if m.URL != "" {
			m.Line, m.Column = d.LineNumber+1, d.ColumnNumber+1
		}
		return capText(m, maxEntry), true

	case *cdplog.EventEntryAdded:
		en := e.Entry
		if en == nil {
			return consoleMessage{}, false
		}
		// Runtime is the authoritative source for anything the page itself
		// said: console-api entries duplicate consoleAPICalled, and javascript
		// entries duplicate exceptionThrown (with a worse stack). Dropping them
		// here is what keeps `buffered` an honest count of distinct messages.
		if en.Source == cdplog.SourceJavascript || string(en.Source) == "console-api" {
			return consoleMessage{}, false
		}
		level, ok := NormalizeConsoleLevel(string(en.Level))
		if !ok {
			level = "log"
		}
		m := consoleMessage{
			Level:  level,
			Source: consoleSourceLog,
			Text:   strings.TrimSpace(string(en.Source) + ": " + en.Text),
			URL:    en.URL,
			TS:     eventTime(en.Timestamp),
			Stack:  formatStack(en.StackTrace),
		}
		if m.URL != "" {
			m.Line = en.LineNumber
		}
		return capText(m, maxEntry), true
	}
	return consoleMessage{}, false
}

// capText applies the per-entry text cap and bounds the retained stack.
func capText(m consoleMessage, maxEntry int) consoleMessage {
	m.Text, m.TextTruncated = eventbuf.TruncateText(m.Text, maxEntry)
	if len(m.Stack) > consoleStackFrames {
		m.Stack = m.Stack[:consoleStackFrames]
	}
	return m
}

// eventTime converts a CDP timestamp to wall time, truncated to milliseconds
// (the precision the envelope's `ts` documents) and defaulting to now for an
// event that carries none.
func eventTime(ts *cdpruntime.Timestamp) time.Time {
	if ts == nil {
		return time.Now().UTC().Truncate(time.Millisecond)
	}
	return ts.Time().UTC().Truncate(time.Millisecond)
}

// exceptionText renders an uncaught exception's one-line summary.
//
// Chrome's Description carries the message AND its rendered stack ("TypeError:
// x is not a function\n    at render (…)"); the stack belongs in `stack`, so
// only the first line goes in `text` — otherwise every error entry repeats its
// own trace inline and a --grep over text matches frame URLs.
func exceptionText(d *cdpruntime.ExceptionDetails) string {
	if d.Exception != nil {
		if desc := d.Exception.Description; desc != "" {
			return strings.TrimSpace(strings.SplitN(desc, "\n", 2)[0])
		}
		if s := renderRemote(d.Exception); s != "" {
			return s
		}
	}
	return d.Text
}

// formatStack renders a CDP stack trace as "func (url:line:col)" frames, with
// the 1-based line/column numbers a developer sees in DevTools (CDP reports
// them 0-based).
func formatStack(st *cdpruntime.StackTrace) []string {
	if st == nil || len(st.CallFrames) == 0 {
		return nil
	}
	out := make([]string, 0, len(st.CallFrames))
	for _, f := range st.CallFrames {
		name := f.FunctionName
		if name == "" {
			name = "(anonymous)"
		}
		out = append(out, fmt.Sprintf("%s (%s:%d:%d)", name, f.URL, f.LineNumber+1, f.ColumnNumber+1))
	}
	return out
}

// renderArgs renders console.log's arguments to the single text line the
// envelope carries. Live remote-object graphs are explicitly out of scope for
// RFC-0002, so an object becomes its DevTools preview string.
func renderArgs(args []*cdpruntime.RemoteObject) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, renderRemote(a))
	}
	return strings.Join(parts, " ")
}

// renderRemote renders one CDP remote object as text.
func renderRemote(o *cdpruntime.RemoteObject) string {
	if o == nil {
		return ""
	}
	if len(o.Value) > 0 {
		var v any
		if err := json.Unmarshal([]byte(o.Value), &v); err == nil {
			if s, ok := v.(string); ok {
				return s // a logged string is its own text, not a quoted JSON string
			}
			if b, err := json.Marshal(v); err == nil {
				return string(b)
			}
		}
		return string(o.Value)
	}
	if o.UnserializableValue != "" {
		return string(o.UnserializableValue)
	}
	if o.Preview != nil {
		if s := previewString(o.Preview); s != "" {
			return s
		}
	}
	if o.Description != "" {
		return o.Description
	}
	return string(o.Type)
}

// previewString renders an abbreviated object preview as "{a: 1, b: 2}",
// prefixed by its class when Chrome names one.
func previewString(p *cdpruntime.ObjectPreview) string {
	if p == nil {
		return ""
	}
	parts := make([]string, 0, len(p.Properties))
	for _, pr := range p.Properties {
		parts = append(parts, pr.Name+": "+pr.Value)
	}
	if len(parts) == 0 {
		return p.Description
	}
	body := "{" + strings.Join(parts, ", ") + "}"
	if p.Overflow {
		body = "{" + strings.Join(parts, ", ") + ", …}"
	}
	if d := p.Description; d != "" && d != "Object" {
		return d + " " + body
	}
	return body
}
