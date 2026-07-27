package chrome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cdplog "github.com/chromedp/cdproto/log"
	cdpruntime "github.com/chromedp/cdproto/runtime"
)

// logEntryEvent builds a Log.entryAdded event for the source-filtering tests.
func logEntryEvent(source, level, text string) *cdplog.EventEntryAdded {
	return &cdplog.EventEntryAdded{Entry: &cdplog.Entry{
		Source: cdplog.Source(source), Level: cdplog.Level(level), Text: text,
	}}
}

// ---------------------------------------------------------------------------
// Pure unit tests — no Chrome, so they run under -short.
// ---------------------------------------------------------------------------

func TestNormalizeConsoleLevel(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   string
		want string
		ok   bool
	}{
		"log":                        {"log", "log", true},
		"info":                       {"info", "info", true},
		"error":                      {"error", "error", true},
		"debug":                      {"debug", "debug", true},
		"warn":                       {"warn", "warn", true},
		"chrome says warning":        {"warning", "warn", true},
		"chrome says verbose":        {"verbose", "debug", true},
		"console.assert is an error": {"assert", "error", true},
		"case and space tolerant":    {"  ERROR ", "error", true},
		"unknown":                    {"critical", "", false},
		"empty":                      {"", "", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := NormalizeConsoleLevel(c.in)
			if got != c.want || ok != c.ok {
				t.Errorf("NormalizeConsoleLevel(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}

// Every Runtime console call type must land on a level, including the ones that
// are not levels at all — an unmapped type must not silently vanish.
func TestConsoleAPILevelAlwaysResolves(t *testing.T) {
	t.Parallel()
	cases := map[cdpruntime.APIType]string{
		cdpruntime.APITypeLog:        "log",
		cdpruntime.APITypeInfo:       "info",
		cdpruntime.APITypeWarning:    "warn",
		cdpruntime.APITypeError:      "error",
		cdpruntime.APITypeDebug:      "debug",
		cdpruntime.APITypeAssert:     "error",
		cdpruntime.APITypeTable:      "log",
		cdpruntime.APITypeStartGroup: "log",
		cdpruntime.APITypeTrace:      "log",
	}
	for in, want := range cases {
		if got := consoleAPILevel(in); got != want {
			t.Errorf("consoleAPILevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderArgs(t *testing.T) {
	t.Parallel()
	str := func(s string) *cdpruntime.RemoteObject {
		return &cdpruntime.RemoteObject{Type: "string", Value: []byte(`"` + s + `"`)}
	}
	num := &cdpruntime.RemoteObject{Type: "number", Value: []byte(`42`)}
	obj := &cdpruntime.RemoteObject{
		Type: "object", Description: "Object",
		Preview: &cdpruntime.ObjectPreview{Description: "Object", Properties: []*cdpruntime.PropertyPreview{
			{Name: "a", Value: "1"}, {Name: "b", Value: "two"},
		}},
	}
	fn := &cdpruntime.RemoteObject{Type: "function", Description: "function go() {}"}

	// A logged string is its own text, not a quoted JSON string: --grep is
	// applied to this, and a stray pair of quotes would break every pattern.
	if got := renderArgs([]*cdpruntime.RemoteObject{str("hello"), num}); got != "hello 42" {
		t.Errorf("renderArgs(string, number) = %q, want %q", got, "hello 42")
	}
	if got := renderArgs([]*cdpruntime.RemoteObject{obj}); got != "{a: 1, b: two}" {
		t.Errorf("renderArgs(object) = %q", got)
	}
	if got := renderArgs([]*cdpruntime.RemoteObject{fn}); got != "function go() {}" {
		t.Errorf("renderArgs(function) = %q", got)
	}
	if got := renderArgs(nil); got != "" {
		t.Errorf("renderArgs(nil) = %q, want empty", got)
	}
}

func TestFormatStackIsOneBased(t *testing.T) {
	t.Parallel()
	st := &cdpruntime.StackTrace{CallFrames: []*cdpruntime.CallFrame{
		{FunctionName: "render", URL: "https://app/bundle.js", LineNumber: 4209, ColumnNumber: 16},
		{FunctionName: "", URL: "https://app/bundle.js", LineNumber: 880, ColumnNumber: 2},
	}}
	got := formatStack(st)
	want := []string{
		"render (https://app/bundle.js:4210:17)",
		"(anonymous) (https://app/bundle.js:881:3)",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("formatStack = %v, want %v (CDP is 0-based; DevTools shows 1-based)", got, want)
	}
	if formatStack(nil) != nil {
		t.Error("formatStack(nil) must be nil, so the field is omitted rather than an empty list")
	}
}

// The description Chrome sends carries the rendered stack inline; text is the
// summary line only, or a --grep over text starts matching frame URLs.
func TestExceptionTextIsTheSummaryLineOnly(t *testing.T) {
	t.Parallel()
	d := &cdpruntime.ExceptionDetails{
		Text: "Uncaught",
		Exception: &cdpruntime.RemoteObject{
			Type:        "object",
			Description: "TypeError: x.map is not a function\n    at render (bundle.js:4210:17)\n    at commit (bundle.js:881:3)",
		},
	}
	if got := exceptionText(d); got != "TypeError: x.map is not a function" {
		t.Errorf("exceptionText = %q", got)
	}
	// With no exception object at all, the details' own text is the fallback.
	if got := exceptionText(&cdpruntime.ExceptionDetails{Text: "Uncaught (in promise)"}); got != "Uncaught (in promise)" {
		t.Errorf("exceptionText fallback = %q", got)
	}
}

func TestConsoleKeepComposesFilters(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 26, 11, 0, 0, 0, time.UTC)
	msgs := []consoleMessage{
		{Level: "log", Text: "[Noise] tick", TS: now.Add(-time.Second)},
		{Level: "error", Text: "[App] boom", TS: now.Add(-time.Second)},
		{Level: "error", Text: "[App] ancient", TS: now.Add(-time.Hour)},
	}
	keep, err := consoleKeep(ConsoleOpts{Grep: `\[App\]`, Levels: []string{"error"}, Since: time.Minute}, now)
	if err != nil {
		t.Fatalf("consoleKeep: %v", err)
	}
	var got []string
	for _, m := range msgs {
		if keep(m) {
			got = append(got, m.Text)
		}
	}
	if len(got) != 1 || got[0] != "[App] boom" {
		t.Errorf("kept %v, want only the recent [App] error", got)
	}

	// No filters at all is a nil predicate, so the buffer skips the call.
	nilKeep, err := consoleKeep(ConsoleOpts{}, now)
	if err != nil {
		t.Errorf("consoleKeep(zero) errored: %v", err)
	}
	if nilKeep != nil {
		t.Error("consoleKeep with no filters must return a nil predicate, so the buffer skips the per-entry call")
	}

	if _, err := consoleKeep(ConsoleOpts{Grep: "("}, now); err == nil {
		t.Error("an invalid --grep must be an error even at the daemon boundary")
	}
}

func TestCapTextBoundsTextAndStack(t *testing.T) {
	t.Parallel()
	stack := make([]string, consoleStackFrames+20)
	for i := range stack {
		stack[i] = fmt.Sprintf("f%d", i)
	}
	m := capText(consoleMessage{Text: strings.Repeat("x", 100), Stack: stack}, 10)
	if len(m.Text) != 10 || !m.TextTruncated {
		t.Errorf("text = %d bytes truncated=%v, want 10/true", len(m.Text), m.TextTruncated)
	}
	if len(m.Stack) != consoleStackFrames {
		t.Errorf("stack = %d frames, want it capped at %d", len(m.Stack), consoleStackFrames)
	}
}

// A javascript-source Log entry is no longer dropped by SOURCE. That drop threw
// away Log.enable's replay of "the entries collected so far", which is a record
// of what a page did before the connection attached — so a tab that had already
// thrown could answer `console --only-errors` with an empty list and exit 0,
// which reads as "the page is clean".
func TestConsoleEventKeepsEveryLogSource(t *testing.T) {
	t.Parallel()
	m, ok := consoleEvent(logEntryEvent("javascript", "error", "Uncaught TypeError: boom"), DefaultConsoleMaxEntry)
	if !ok {
		t.Fatal("a javascript log entry was dropped by source; the pre-attach backlog arrives this way")
	}
	if m.Level != "error" || m.Source != consoleSourceLog || !strings.Contains(m.Text, "TypeError") {
		t.Errorf("javascript log entry = %+v", m)
	}
	// A browser-level source (a failed subresource, a deprecation) is real
	// added value and must survive too.
	m, ok = consoleEvent(logEntryEvent("network", "error", "Failed to load resource: 404"), DefaultConsoleMaxEntry)
	if !ok {
		t.Fatal("a network log entry was dropped")
	}
	if m.Level != "error" || m.Source != consoleSourceLog || !strings.Contains(m.Text, "404") {
		t.Errorf("network log entry = %+v", m)
	}
}

// Duplicates are suppressed by IDENTITY instead: one uncaught exception that
// Chrome describes twice is one message, but the same error thrown again later
// is two.
func TestConsoleDedupSuppressesOneExceptionReportedTwice(t *testing.T) {
	t.Parallel()
	const url = "https://app.example/bundle.js"
	thrown := &cdpruntime.EventExceptionThrown{
		ExceptionDetails: &cdpruntime.ExceptionDetails{
			URL: url, LineNumber: 41, ColumnNumber: 7,
			Exception: &cdpruntime.RemoteObject{Description: "TypeError: x.map is not a function\n    at render"},
		},
	}
	entry := &cdplog.EventEntryAdded{Entry: &cdplog.Entry{
		Source: cdplog.SourceJavascript, Level: "error",
		Text: "Uncaught TypeError: x.map is not a function", URL: url, LineNumber: 41,
	}}
	other := &cdplog.EventEntryAdded{Entry: &cdplog.Entry{
		Source: cdplog.SourceJavascript, Level: "error",
		Text: "Uncaught ReferenceError: nope is not defined", URL: url,
	}}

	// Order-independent: whichever of the two arrives first is the report kept.
	for _, order := range [][]any{{thrown, entry}, {entry, thrown}} {
		d := &consoleDedup{}
		at := time.Now()
		kept := 0
		for _, ev := range order {
			m, _ := consoleEvent(ev, DefaultConsoleMaxEntry)
			m.TS = at
			ident, collidable := consoleDedupIdent(ev, m)
			if !collidable {
				t.Fatalf("%T is not treated as a collidable report", ev)
			}
			if d.first(ident, m.TS) {
				kept++
			}
		}
		if kept != 1 {
			t.Errorf("%d of the 2 reports of one exception were kept, want 1", kept)
		}
	}

	// A DIFFERENT error is never suppressed.
	d := &consoleDedup{}
	at := time.Now()
	for _, ev := range []any{thrown, other} {
		m, _ := consoleEvent(ev, DefaultConsoleMaxEntry)
		m.TS = at
		ident, _ := consoleDedupIdent(ev, m)
		if !d.first(ident, m.TS) {
			t.Errorf("a distinct error was suppressed as a duplicate: %T", ev)
		}
	}

	// And the SAME error thrown again later is a real repeat, not a duplicate:
	// an app that throws on every poll is reporting something.
	d = &consoleDedup{}
	m, _ := consoleEvent(thrown, DefaultConsoleMaxEntry)
	ident, _ := consoleDedupIdent(thrown, m)
	if !d.first(ident, at) {
		t.Fatal("the first report was suppressed")
	}
	if !d.first(ident, at.Add(consoleDedupWindow+time.Second)) {
		t.Error("a repeat outside the dedupe window was suppressed; a recurring error must still be visible")
	}
}

// A console.* call is NEVER deduped: a page that logs the same line twice on
// purpose has to see both, and only an uncaught exception can arrive twice.
func TestConsoleDedupIgnoresWhatCannotCollide(t *testing.T) {
	t.Parallel()
	ev := &cdpruntime.EventConsoleAPICalled{
		Type: cdpruntime.APITypeLog,
		Args: []*cdpruntime.RemoteObject{{Type: "string", Value: []byte(`"tick"`)}},
	}
	m, _ := consoleEvent(ev, DefaultConsoleMaxEntry)
	if _, collidable := consoleDedupIdent(ev, m); collidable {
		t.Error("a console.* call was treated as dedupable; repeated logs are real")
	}
	netEntry := logEntryEvent("network", "error", "Failed to load resource: 404")
	nm, _ := consoleEvent(netEntry, DefaultConsoleMaxEntry)
	if _, collidable := consoleDedupIdent(netEntry, nm); collidable {
		t.Error("a network log entry was treated as dedupable; Runtime never reports it")
	}
}

func TestConsoleEventMapsAnException(t *testing.T) {
	t.Parallel()
	ev := &cdpruntime.EventExceptionThrown{
		ExceptionDetails: &cdpruntime.ExceptionDetails{
			Text: "Uncaught", LineNumber: 9, ColumnNumber: 4, URL: "https://app/x.js",
			Exception:  &cdpruntime.RemoteObject{Description: "TypeError: boom\n    at f (x.js:10:5)"},
			StackTrace: &cdpruntime.StackTrace{CallFrames: []*cdpruntime.CallFrame{{FunctionName: "f", URL: "https://app/x.js", LineNumber: 9, ColumnNumber: 4}}},
		},
	}
	m, ok := consoleEvent(ev, DefaultConsoleMaxEntry)
	if !ok {
		t.Fatal("exceptionThrown was not retained")
	}
	if m.Level != "error" || m.Source != consoleSourceException {
		t.Errorf("level/source = %q/%q, want error/exception", m.Level, m.Source)
	}
	if m.Text != "TypeError: boom" {
		t.Errorf("text = %q", m.Text)
	}
	if len(m.Stack) == 0 {
		t.Error("stack is empty — it is the field users need most and the one a marshalling mistake drops")
	}
	if m.Line != 10 || m.Column != 5 {
		t.Errorf("line:col = %d:%d, want the 1-based 10:5", m.Line, m.Column)
	}
}

// ---------------------------------------------------------------------------
// Live Chrome — skipped under -short, not parallel (they share a browser).
// ---------------------------------------------------------------------------

// consoleFixtures serves the pages the live tests drive. Each page logs on load
// so a read that happens afterwards is testing retention, not timing.
func consoleFixtures(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	page := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<!doctype html><title>Console fixture</title><body>%s</body>`, body)
		}
	}
	// No favicon request, so the browser's own 404 does not add log noise the
	// exact-count assertions would trip over.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/log", page(`<script>console.log("hello from the page"); console.warn("careful"); console.error("bad")</script>`))
	mux.HandleFunc("/throw", page(`<script>
	  function inner() { return null.nope; }
	  function outer() { return inner(); }
	  outer();
	</script>`))
	mux.HandleFunc("/click", page(`<button id="go" onclick="console.log('clicked once')">Go</button>
	  <script>console.log("noise from load")</script>`))
	mux.HandleFunc("/one", page(`<script>console.log("i am tab one")</script>`))
	mux.HandleFunc("/two", page(`<script>console.log("i am tab two")</script>`))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// preAttachThrow serves a page that throws an uncaught TypeError and THEN
// signals the returned channel, so a test can attach strictly after the throw
// rather than guessing at a sleep. The throw is in its own script tag: an
// uncaught error ends that script, not the next one.
func preAttachThrow(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	thrown := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/thrown", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case thrown <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/late", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><title>Console fixture</title><body>
<script>
  function inner() { return null.nope; }
  function outer() { return inner(); }
  outer();
</script>
<script>fetch("/thrown");</script>
</body>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, thrown
}

func liveChrome(t *testing.T) *CDP {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// consoleMsgs unpacks a Console result into the retained messages.
func consoleMsgs(t *testing.T, res any) []consoleMessage {
	t.Helper()
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("console result is %T, want a map", res)
	}
	raw, _ := m["messages"].([]any)
	out := make([]consoleMessage, 0, len(raw))
	for _, r := range raw {
		cm, ok := r.(consoleMessage)
		if !ok {
			t.Fatalf("message is %T, want consoleMessage", r)
		}
		out = append(out, cm)
	}
	return out
}

// awaitConsole polls Console until want is satisfied, so a test asserts on what
// the page said rather than on how fast the event loop delivered it. It returns
// the last read, satisfied or not, so the failure message shows what arrived.
func awaitConsole(ctx context.Context, t *testing.T, b *CDP, id string, opts ConsoleOpts, want func([]consoleMessage) bool) []consoleMessage {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last []consoleMessage
	for {
		res, err := b.Console(ctx, id, opts)
		if err != nil {
			t.Fatalf("Console: %v", err)
		}
		last = consoleMsgs(t, res)
		if want(last) || time.Now().After(deadline) {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func hasText(msgs []consoleMessage, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Text, sub) {
			return true
		}
	}
	return false
}

// VS-1: a message logged before anyone asked for it is still there, because
// capture starts when the connection attaches to the tab.
func TestConsoleCapturesALogEmittedBeforeTheRead(t *testing.T) {
	b := liveChrome(t)
	srv := consoleFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := firstTab(ctx, t, b)
	// Attaching is what starts capture — this stands in for the earlier command
	// (a click, a nav) that a real session would have run before reading.
	if _, err := b.Navigate(ctx, id, "about:blank"); err != nil {
		t.Fatalf("attach nav: %v", err)
	}
	if _, err := b.Navigate(ctx, id, srv.URL+"/log"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	msgs := awaitConsole(ctx, t, b, id, ConsoleOpts{Limit: 100}, func(m []consoleMessage) bool {
		return hasText(m, "hello from the page")
	})
	var got *consoleMessage
	for i := range msgs {
		if strings.Contains(msgs[i].Text, "hello from the page") {
			got = &msgs[i]
		}
	}
	if got == nil {
		t.Fatalf("the load-time console.log was not retained; got %+v", msgs)
	}
	if got.Level != "log" {
		t.Errorf("level = %q, want log", got.Level)
	}
	if got.Source != consoleSourceConsole {
		t.Errorf("source = %q, want console", got.Source)
	}
	if got.TS.IsZero() {
		t.Error("ts is zero; the envelope documents a timestamp per message")
	}
}

// RFC-0002 US-1, the case the verb exists for: the daemon attaches to a tab that
// has ALREADY loaded and thrown.
//
// The capture listeners have to be registered before the attach, because
// chromedp's own attach sequence issues Runtime.enable and Log.enable — and
// those enables are what flush what the page did before we arrived. Registering
// afterwards dropped the whole backlog, so `console --only-errors` returned an
// empty list with exit 0 and no note: the reader concludes the page is clean.
func TestConsoleCapturesAnErrorThrownBeforeTheAttach(t *testing.T) {
	b := liveChrome(t)
	srv, thrown := preAttachThrow(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Open creates the tab WITHOUT attaching, so the page throws with nothing
	// listening — which is exactly a daemon arriving at an existing tab.
	res, err := b.Open(ctx, srv.URL+"/late")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, _ := res["id"].(string)
	if id == "" {
		t.Fatalf("Open returned no target id: %v", res)
	}
	select {
	case <-thrown:
	case <-time.After(30 * time.Second):
		t.Fatal("the fixture never reported that it had thrown")
	}
	if b.attached(id) {
		t.Fatal("the tab was already attached; this test has to read a backlog, not a live event")
	}

	opts := ConsoleOpts{Levels: []string{"error"}, Limit: 100} // --only-errors
	msgs := awaitConsole(ctx, t, b, id, opts, func(m []consoleMessage) bool {
		return hasText(m, "TypeError")
	})
	if !hasText(msgs, "TypeError") {
		t.Fatalf("the error the page threw before we attached was not reported; --only-errors returned %+v.\n"+
			"An empty list here reads as \"the page is clean\", which is the opposite of the truth", msgs)
	}
}

// VS-2: an uncaught exception arrives at error level WITH its stack — the field
// users need most and the one a marshalling mistake silently drops.
func TestConsoleCapturesAnUncaughtExceptionWithAStack(t *testing.T) {
	b := liveChrome(t)
	srv := consoleFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, "about:blank"); err != nil {
		t.Fatalf("attach nav: %v", err)
	}
	if _, err := b.Navigate(ctx, id, srv.URL+"/throw"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	opts := ConsoleOpts{Levels: []string{"error"}, Limit: 100} // --only-errors
	msgs := awaitConsole(ctx, t, b, id, opts, func(m []consoleMessage) bool {
		return hasText(m, "TypeError")
	})
	var exc *consoleMessage
	for i := range msgs {
		if msgs[i].Source == consoleSourceException {
			exc = &msgs[i]
		}
	}
	if exc == nil {
		t.Fatalf("no uncaught exception was retained; --only-errors returned %+v", msgs)
	}
	if !strings.Contains(exc.Text, "TypeError") {
		t.Errorf("text = %q, want the exception type name", exc.Text)
	}
	if len(exc.Stack) == 0 {
		t.Fatal("stack is empty — an exception without its stack cannot tell you where the page broke")
	}
	if !strings.Contains(strings.Join(exc.Stack, "\n"), "inner") {
		t.Errorf("stack = %v, want the throwing frame", exc.Stack)
	}
	for _, m := range msgs {
		if m.Level != "error" {
			t.Errorf("--only-errors returned a %q message: %+v", m.Level, m)
		}
	}
}

// VS-5: clear, act, read — the second read shows only what the action produced.
func TestConsoleClearScopesTheReadToOneAction(t *testing.T) {
	b := liveChrome(t)
	srv := consoleFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, "about:blank"); err != nil {
		t.Fatalf("attach nav: %v", err)
	}
	if _, err := b.Navigate(ctx, id, srv.URL+"/click"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	awaitConsole(ctx, t, b, id, ConsoleOpts{Limit: 100}, func(m []consoleMessage) bool {
		return hasText(m, "noise from load")
	})

	if _, err := b.Console(ctx, id, ConsoleOpts{Clear: true}); err != nil {
		t.Fatalf("Console --clear: %v", err)
	}
	if _, err := b.Pointer(ctx, id, "#go", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}

	msgs := awaitConsole(ctx, t, b, id, ConsoleOpts{Limit: 100}, func(m []consoleMessage) bool {
		return hasText(m, "clicked once")
	})
	// Count only what the PAGE said: a browser-level log entry (a network or
	// deprecation notice) is legitimate noise that the clear cannot suppress
	// and that this scenario is not about.
	var fromPage []consoleMessage
	for _, m := range msgs {
		if m.Source == consoleSourceConsole {
			fromPage = append(fromPage, m)
		}
	}
	if len(fromPage) != 1 || fromPage[0].Text != "clicked once" {
		t.Errorf("after clear+click the console shows %+v, want exactly the click's message", fromPage)
	}
}

// VS-10, as amended by the backlog fix: with nothing alive when the page logged,
// the history is whatever Chrome replays at enable time — real, but Chrome's
// window rather than ours. The envelope SAYS so rather than passing it off as a
// full session record, and `buffered` reports what is actually held.
func TestConsoleWithoutRetainedHistorySaysSo(t *testing.T) {
	b := liveChrome(t)
	srv := consoleFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Open creates and navigates the tab at the BROWSER level, so nothing is
	// attached to it — exactly the --no-daemon situation, where the process
	// doing the reading was not running when the page logged.
	opened, err := b.Open(ctx, srv.URL+"/log")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, _ := opened["id"].(string)
	if id == "" {
		t.Fatalf("Open returned no id: %v", opened)
	}
	t.Cleanup(func() { _, _ = b.CloseTabs(context.Background(), []string{id}) })
	// Let the page finish logging BEFORE the first attach, so the test is about
	// retention and not about winning a race with the load event.
	time.Sleep(2 * time.Second)

	res, err := b.Console(ctx, id, ConsoleOpts{Limit: 100})
	if err != nil {
		t.Fatalf("Console: %v", err)
	}
	m := res.(map[string]any)
	note, _ := m["note"].(string)
	if note == "" {
		t.Error("no note: a message list with no explanation reads as a full session record, which is a lie the caller cannot detect")
	}
	// `buffered` must match what is really held — reporting 0 alongside a
	// non-empty list would make both numbers useless.
	if got, want := m["buffered"], len(consoleMsgs(t, res)); got.(int) < want {
		t.Errorf("buffered = %v but %d messages were returned; the count must describe what is held", got, want)
	}
}

// VS-11: buffers are per target; one tab's messages never leak into another's.
func TestConsoleIsIsolatedPerTarget(t *testing.T) {
	b := liveChrome(t)
	srv := consoleFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	one := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, one, srv.URL+"/one"); err != nil {
		t.Fatalf("Navigate tab one: %v", err)
	}
	opened, err := b.Open(ctx, "about:blank")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	two, _ := opened["id"].(string)
	t.Cleanup(func() { _, _ = b.CloseTabs(context.Background(), []string{two}) })
	if _, err := b.Navigate(ctx, two, srv.URL+"/two"); err != nil {
		t.Fatalf("Navigate tab two: %v", err)
	}

	msgsOne := awaitConsole(ctx, t, b, one, ConsoleOpts{Limit: 100}, func(m []consoleMessage) bool {
		return hasText(m, "i am tab one")
	})
	if hasText(msgsOne, "i am tab two") {
		t.Errorf("tab one's console contains tab two's message: %+v", msgsOne)
	}
	msgsTwo := awaitConsole(ctx, t, b, two, ConsoleOpts{Limit: 100}, func(m []consoleMessage) bool {
		return hasText(m, "i am tab two")
	})
	if hasText(msgsTwo, "i am tab one") {
		t.Errorf("tab two's console contains tab one's message: %+v", msgsTwo)
	}
}

// --follow delivers messages emitted after the stream started, and returns
// cleanly (not as an error) when the window closes.
func TestConsoleStreamDeliversNewMessages(t *testing.T) {
	b := liveChrome(t)
	srv := consoleFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL+"/click"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	got := make(chan string, 8)
	streamCtx, streamCancel := context.WithTimeout(ctx, 45*time.Second)
	defer streamCancel()
	done := make(chan error, 1)
	go func() {
		done <- b.ConsoleStream(streamCtx, id, ConsoleOpts{Grep: "clicked once"}, func(v any) error {
			m := v.(map[string]any)["messages"].([]any)[0].(consoleMessage)
			select {
			case got <- m.Text:
			default:
			}
			return nil
		})
	}()

	// Click until the stream delivers, rather than sleeping for a fixed interval
	// and hoping the subscription landed inside it: on a loaded machine that is a
	// race, and an extra click only repeats the one message this stream is
	// filtered to.
	var text string
	for deadline := time.Now().Add(20 * time.Second); text == "" && time.Now().Before(deadline); {
		if _, err := b.Pointer(ctx, id, "#go", PointerOpts{Action: PointerClick}); err != nil {
			t.Fatalf("Click: %v", err)
		}
		select {
		case text = <-got:
		case <-time.After(time.Second):
		}
	}
	if text == "" {
		t.Fatal("the stream delivered nothing for a message logged while it was running")
	}
	if text != "clicked once" {
		t.Errorf("streamed %q, want %q", text, "clicked once")
	}

	streamCancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ConsoleStream = %v; the window closing is how a follow ends, not a failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ConsoleStream did not return after its context was cancelled")
	}
}
