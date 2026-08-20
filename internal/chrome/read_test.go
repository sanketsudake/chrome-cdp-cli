package chrome

// Live-Chrome tests for the reading verbs (RFC-0010): `text --article` and
// `eval --await`. They drive a MANAGED headless Chrome against httptest
// fixtures, never the user's real browser, and skip when -short or when no
// Chrome binary can be launched.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// evalValue runs an expression through the default (non-await) Eval path and
// renders the returned value for comparison. Every reading test funnels through
// it, so the Eval call site exists once per file.
func evalValue(ctx context.Context, t *testing.T, b Browser, id, expr string) (string, error) {
	t.Helper()
	res, err := b.Eval(ctx, id, expr, EvalOpts{})
	if err != nil {
		return "", err
	}
	m, ok := res.(map[string]any)
	if !ok {
		return "", fmt.Errorf("Eval returned %T, want map[string]any", res)
	}
	return fmt.Sprintf("%v", m["value"]), nil
}

// VS-13 — regression guard, written BEFORE the Eval signature change.
//
// Plain `eval` (no --await) evaluates in the page's main world and returns
// {"value": …}. This pins the pre-RFC-0010 contract, including the thing that
// must keep FAILING without --await: a top-level `await`. If that starts
// succeeding, the default semantics changed — which RFC-0010 Open Question 1
// explicitly defers to a major version.
func TestEvalDefaultSemanticsUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Guard</title><body><p id="p">hi</p></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	for name, tc := range map[string]struct{ expr, want string }{
		"arithmetic":      {"1+1", "2"},
		"document title":  {"document.title", "Guard"},
		"dom read":        {"document.getElementById('p').textContent", "hi"},
		"member access":   {"({a:1}).a", "1"},
		"iife workaround": {"(() => { const t = document.title; return t; })()", "Guard"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := evalValue(ctx, t, b, id, tc.expr)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.expr, err)
			}
			if got != tc.want {
				t.Errorf("Eval(%q) = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}

	// Top-level await — the ergonomic gap RFC-0010 exists to close — must still
	// be a failure on the DEFAULT path. That is what makes --await opt-in rather
	// than a silent behaviour change for existing scripts.
	t.Run("top-level await still fails without --await", func(t *testing.T) {
		short, scancel := context.WithTimeout(ctx, 5*time.Second)
		defer scancel()
		if _, err := evalValue(short, t, b, id, `await Promise.resolve(1)`); err == nil {
			t.Error("Eval of a top-level await succeeded without --await; default semantics changed")
		}
	})

	// A bare promise is NOT awaited on the default path: the caller gets the
	// serialized promise object, not its resolved value.
	t.Run("bare promise is not awaited", func(t *testing.T) {
		got, err := evalValue(ctx, t, b, id, `Promise.resolve(42)`)
		if err != nil {
			t.Fatalf("Eval of a bare promise: %v", err)
		}
		if got == "42" {
			t.Error("Eval resolved a promise without --await; default semantics changed")
		}
	})
}

// ---------------------------------------------------------------- fixtures
//
// Four shapes, because they fail in different ways: a realistic article page, a
// page that is nothing but navigation, a single-page-app shell that has not
// rendered, and a page with several plausible article candidates. The last is
// where extraction most often picks wrong, so it stays in the suite to keep any
// future tuning honest.

const articlePage = `<!doctype html>
<html><head>
<title>Quarterly report — Example Corp Newsroom</title>
<meta property="og:title" content="Quarterly report">
<meta name="author" content="A. Author">
</head><body>
<div id="cookie-banner">We use cookies and similar technologies to improve your experience. Accept all cookies to continue browsing this website today.</div>
<nav class="site-nav"><a href="/">Home</a> <a href="/about">About us</a> <a href="/blog">Blog</a> <a href="/contact">Contact sales</a></nav>
<header class="masthead"><span>Example Corp Newsroom, published continuously since the year two thousand and four</span></header>
<main>
  <article class="post-content">
    <h1>Quarterly report</h1>
    <p>Revenue for the quarter reached one point two billion dollars, an increase of eleven percent over the same period last year, driven largely by renewals in the enterprise segment.</p>
    <h2>Regional breakdown</h2>
    <ul>
      <li>North America grew nine percent, with the public sector contributing most of the gain.</li>
      <li>Europe grew fourteen percent, its strongest quarter since the region was reorganised.</li>
    </ul>
    <p>Operating margin held steady at twenty-two percent, and the company reaffirmed its guidance for the full year. See the <a href="https://example.com/deep">full filing</a> for the detailed statements and the reconciliation tables.</p>
    <blockquote>We are pleased with the durability of the enterprise business, and we expect that durability to continue.</blockquote>
    <pre><code>revenue = 1200
margin  = 0.22</code></pre>
  </article>
</main>
<aside class="sidebar"><h3>Related</h3><ul><li><a href="/a">Another story entirely, about something else</a></li></ul></aside>
<footer class="site-footer">Copyright 2026 Example Corp. All rights reserved. Read our privacy policy and our terms of service.</footer>
</body></html>`

const navOnlyPage = `<!doctype html>
<html><head><title>Menu</title></head><body>
<nav class="site-nav"><ul>
<li><a href="/one">Products and services</a></li>
<li><a href="/two">Solutions for enterprise</a></li>
<li><a href="/three">Pricing and licensing</a></li>
<li><a href="/four">Documentation and guides</a></li>
</ul></nav>
<footer class="site-footer">Copyright 2026. All rights reserved.</footer>
</body></html>`

const spaShellPage = `<!doctype html>
<html><head><title>Dashboard</title></head><body>
<nav class="app-nav"><a href="#/home">Home</a><a href="#/reports">Reports</a></nav>
<div id="root"></div>
<script>window.__booted = true;</script>
</body></html>`

const shortArticlePage = `<!doctype html>
<html><head><title>Short</title></head><body>
<nav class="site-nav"><a href="/">Home</a><a href="/about">About</a></nav>
<main><article class="post"><h1>Notice</h1>
<p>The service will be unavailable on Sunday between two and four in the morning for maintenance.</p>
</article></main>
</body></html>`

// multiCandidatePage has three blocks that all look like content to a naive
// scorer: two short teasers and the actual story.
const multiCandidatePage = `<!doctype html>
<html><head><title>Index</title></head><body>
<main>
  <div class="teasers">
    <div class="entry"><h2>Teaser one</h2><p>A brief note about the first unrelated topic, running to a couple of lines.</p></div>
    <div class="entry"><h2>Teaser two</h2><p>A brief note about the second unrelated topic, also running to a couple of lines.</p></div>
  </div>
  <div class="entry post">
    <h1>The real story</h1>
    <p>The committee met for six hours on Tuesday, and by the end of the afternoon it had agreed on a schedule that nobody in the room particularly liked but everybody could live with.</p>
    <p>That compromise, which the chair described as durable, sets the deadline in March, moves the review to June, and leaves the funding question open until the autumn session.</p>
    <p>Delegates who had argued for an earlier deadline said afterwards that they would not reopen the question, and that the schedule as agreed was workable for their departments.</p>
  </div>
</main>
</body></html>`

// mutationPage records every DOM mutation after load into window.__mutations —
// the fixture VS-6 needs.
const mutationPage = `<!doctype html>
<html><head><title>Observed</title>
<script>
window.__mutations = [];
addEventListener("load", function () {
  new MutationObserver(function (records) {
    for (var i = 0; i < records.length; i++) window.__mutations.push(records[i].type);
  }).observe(document.documentElement, {
    childList: true, subtree: true, attributes: true, characterData: true
  });
});
</script>
</head><body>
<nav class="site-nav"><a href="/">Home</a></nav>
<main><article class="post-content"><h1>Observed article</h1>
<p>This page watches itself for mutations so a test can prove that reading it changes nothing at all about the live document.</p>
<p>Extraction scores a detached clone, which is why the observer stays silent while the reading verb runs against this page.</p>
<p>If it ever fires, some reading verb has started writing to the document that other commands in the same session are driving.</p>
</article></main>
</body></html>`

// fixtureServer serves each named fixture at /<name>.
func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	pages := map[string]string{
		"/article": articlePage,
		"/nav":     navOnlyPage,
		"/spa":     spaShellPage,
		"/short":   shortArticlePage,
		"/multi":   multiCandidatePage,
		"/mutate":  mutationPage,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// article navigates to a fixture and returns the `text --article` result.
func article(ctx context.Context, t *testing.T, b Browser, id, url string, opts TextOpts) map[string]any {
	t.Helper()
	if _, err := b.Navigate(ctx, id, url); err != nil {
		t.Fatalf("Navigate(%s): %v", url, err)
	}
	opts.Article = true
	res, err := b.Text(ctx, id, "", opts)
	if err != nil {
		t.Fatalf("Text --article on %s: %v", url, err)
	}
	return res
}

func str(t *testing.T, m map[string]any, k string) string {
	t.Helper()
	s, ok := m[k].(string)
	if !ok {
		t.Fatalf("result[%q] = %#v, want a string", k, m[k])
	}
	return s
}

func num(t *testing.T, m map[string]any, k string) float64 {
	t.Helper()
	switch v := m[k].(type) {
	case int:
		return float64(v)
	case float64:
		return v
	default:
		t.Fatalf("result[%q] = %#v, want a number", k, m[k])
		return 0
	}
}

// TestArticleExtraction covers VS-1 through VS-6: what extraction keeps, what it
// reports, when it admits failure, and — the one that matters most — that it
// never touches the live DOM.
func TestArticleExtraction(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := fixtureServer(t)
	id := firstTab(ctx, t, b)

	// VS-1 — the article survives; the furniture does not.
	t.Run("drops boilerplate", func(t *testing.T) {
		res := article(ctx, t, b, id, srv.URL+"/article", TextOpts{})
		if res["extracted"] != true {
			t.Fatalf("extracted = %v, want true (result: %#v)", res["extracted"], res)
		}
		text := str(t, res, "text")
		for _, want := range []string{
			"one point two billion dollars",
			"Operating margin held steady",
			"North America grew nine percent",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("extracted text is missing article content %q:\n%s", want, text)
			}
		}
		for _, unwanted := range []string{
			"We use cookies",     // cookie banner
			"Contact sales",      // nav
			"published continuo", // header
			"Another story",      // aside
			"All rights reserved",
		} {
			if strings.Contains(text, unwanted) {
				t.Errorf("extracted text still contains boilerplate %q:\n%s", unwanted, text)
			}
		}
		if got := str(t, res, "title"); got != "Quarterly report" {
			t.Errorf("title = %q, want %q", got, "Quarterly report")
		}
		if got := str(t, res, "byline"); got != "A. Author" {
			t.Errorf("byline = %q, want %q", got, "A. Author")
		}
		if ex := str(t, res, "excerpt"); ex == "" || !strings.Contains(text, strings.TrimSuffix(ex, "…")) {
			t.Errorf("excerpt %q is not an opening of the extracted text", ex)
		}
		if res["format"] != "text" {
			t.Errorf("format = %v, want text", res["format"])
		}
	})

	// VS-2 — the counts describe what was actually returned.
	t.Run("reports chars, total_chars and ratio", func(t *testing.T) {
		res := article(ctx, t, b, id, srv.URL+"/article", TextOpts{})
		text := str(t, res, "text")
		chars, total, ratio := num(t, res, "chars"), num(t, res, "total_chars"), num(t, res, "ratio")
		if want := float64(utf8.RuneCountInString(text)); chars != want {
			t.Errorf("chars = %v, want %v (the length of the returned text)", chars, want)
		}
		if total <= chars {
			t.Errorf("total_chars = %v, want more than chars = %v", total, chars)
		}
		if want := chars / total; math.Abs(ratio-want) > 0.001 {
			t.Errorf("ratio = %v, want ~%v (chars ÷ total_chars)", ratio, want)
		}
		// US-2: the whole point is that --article is materially smaller.
		if ratio > 0.9 {
			t.Errorf("ratio = %v — extraction kept almost the whole page, so it dropped nothing", ratio)
		}
	})

	// VS-3 — a page with no article says so instead of inventing one.
	t.Run("low confidence is flagged, not faked", func(t *testing.T) {
		for name, path := range map[string]string{
			"navigation only": "/nav",
			"unrendered SPA":  "/spa",
		} {
			t.Run(name, func(t *testing.T) {
				res := article(ctx, t, b, id, srv.URL+path, TextOpts{})
				if res["extracted"] != false {
					t.Errorf("extracted = %v, want false for %s (result: %#v)", res["extracted"], name, res)
				}
				if str(t, res, "reason") == "" {
					t.Error("extracted:false carries no reason")
				}
				// The caller still gets usable text — that is why this is not an error.
				if str(t, res, "text") == "" {
					t.Error("extracted:false returned no text at all; it must fall back to the full page text")
				}
			})
		}
	})

	// VS-4 — --min-chars is the threshold, and it is the only thing that moves.
	t.Run("min-chars threshold", func(t *testing.T) {
		for _, tc := range []struct {
			min  int
			want bool
		}{{250, false}, {50, true}} {
			res := article(ctx, t, b, id, srv.URL+"/short", TextOpts{MinChars: tc.min})
			if res["extracted"] != tc.want {
				t.Errorf("--min-chars %d: extracted = %v, want %v (article_chars=%v)",
					tc.min, res["extracted"], tc.want, res["article_chars"])
			}
		}
	})

	// VS-5 — structure survives --markdown.
	t.Run("markdown preserves structure", func(t *testing.T) {
		res := article(ctx, t, b, id, srv.URL+"/article", TextOpts{Markdown: true})
		md := str(t, res, "text")
		if res["format"] != "markdown" {
			t.Errorf("format = %v, want markdown", res["format"])
		}
		for _, want := range []string{
			"# Quarterly report",
			"## Regional breakdown",
			"- North America grew nine percent",
			"[full filing](https://example.com/deep)",
			"> We are pleased",
			"```",
		} {
			if !strings.Contains(md, want) {
				t.Errorf("markdown is missing %q:\n%s", want, md)
			}
		}
	})

	// VS-6 — THE regression test that matters most in this RFC.
	t.Run("extraction does not mutate the page", func(t *testing.T) {
		if _, err := b.Navigate(ctx, id, srv.URL+"/mutate"); err != nil {
			t.Fatalf("Navigate: %v", err)
		}
		// Snapshot window's own properties first, so the check is a diff rather
		// than a guess at what a leak would be called.
		globals := `JSON.stringify(Object.getOwnPropertyNames(window))`
		before, err := evalValue(ctx, t, b, id, globals)
		if err != nil {
			t.Fatalf("snapshotting globals: %v", err)
		}

		res, err := b.Text(ctx, id, "", TextOpts{Article: true})
		if err != nil {
			t.Fatalf("Text --article: %v", err)
		}
		if res["extracted"] != true {
			t.Fatalf("fixture did not extract, so the mutation check proves nothing: %#v", res)
		}

		got, err := evalValue(ctx, t, b, id, "window.__mutations.length")
		if err != nil {
			t.Fatalf("reading window.__mutations: %v", err)
		}
		if got != "0" {
			list, _ := evalValue(ctx, t, b, id, "JSON.stringify(window.__mutations)")
			t.Errorf("extraction caused %s DOM mutations (%s); it must score a clone, never the live document", got, list)
		}

		// The extractor runs in an isolated world, so it also leaves no globals.
		after, err := evalValue(ctx, t, b, id, globals)
		if err != nil {
			t.Fatalf("snapshotting globals: %v", err)
		}
		if after != before {
			t.Errorf("extraction changed window's own properties.\nbefore: %s\nafter:  %s", before, after)
		}
	})

	// A page with several plausible candidates is where a scorer picks wrong.
	t.Run("picks the story over the teasers", func(t *testing.T) {
		res := article(ctx, t, b, id, srv.URL+"/multi", TextOpts{})
		text := str(t, res, "text")
		if !strings.Contains(text, "The committee met for six hours") {
			t.Errorf("extraction missed the main story:\n%s", text)
		}
		if strings.Contains(text, "first unrelated topic") || strings.Contains(text, "second unrelated topic") {
			t.Errorf("extraction pulled in the teaser blocks instead of the story:\n%s", text)
		}
	})
}

// TestEvalAwait covers VS-8 through VS-12 and VS-14: REPL semantics, rejections
// as errors, timeouts that leave the connection usable, and a fetch from the
// page's own authenticated context.
func TestEvalAwait(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	// VS-14's server: /api/me answers only when the page's session cookie rides
	// along, which is the whole point of evaluating in the user's live session.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/me":
			c, err := r.Cookie("session")
			if err != nil || c.Value != "s3cret" {
				http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"user":"ada","admin":true}`)
		default:
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "s3cret", Path: "/"})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!doctype html><title>Session</title><body>
<script>window.p = new Promise(r => setTimeout(() => r(42), 50));</script>
</body>`)
		}
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	awaited := func(t *testing.T, expr string) map[string]any {
		t.Helper()
		res, err := b.Eval(ctx, id, expr, EvalOpts{Await: true})
		if err != nil {
			t.Fatalf("Eval --await %q: %v", expr, err)
		}
		m, ok := res.(map[string]any)
		if !ok {
			t.Fatalf("Eval returned %T, want map[string]any", res)
		}
		if m["awaited"] != true {
			t.Errorf("result does not record awaited:true — a caller cannot tell which path ran: %#v", m)
		}
		return m
	}

	// VS-8 — a top-level await resolves.
	t.Run("top-level await resolves", func(t *testing.T) {
		m := awaited(t, "await window.p")
		if fmt.Sprintf("%v", m["value"]) != "42" {
			t.Errorf("value = %#v, want 42", m["value"])
		}
	})

	// VS-9 — statements, then a value, with no IIFE.
	t.Run("statements then a value", func(t *testing.T) {
		m := awaited(t, "const a = 1; const b = 2; a + b")
		if fmt.Sprintf("%v", m["value"]) != "3" {
			t.Errorf("value = %#v, want 3", m["value"])
		}
	})

	// VS-10 — the single most important correctness point in the RFC.
	t.Run("rejection is an error, never a value", func(t *testing.T) {
		res, err := b.Eval(ctx, id, `await Promise.reject(new Error("boom"))`, EvalOpts{Await: true})
		if err == nil {
			t.Fatalf("a rejected promise returned a successful result %#v; it must be an error", res)
		}
		if res != nil {
			t.Errorf("a rejected promise returned a non-nil result alongside the error: %#v", res)
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("error = %q, want it to carry the rejection message 'boom'", err)
		}
		js, ok := JSException(err)
		if !ok {
			t.Fatalf("error is %T, want a *JSError so the CLI can report the stack", err)
		}
		if js.Stack == "" {
			t.Error("JSError carries no stack; RFC-0010 puts the rejection stack in error.details")
		}
	})

	// A rejection of a plain value has no Error description — report the value,
	// not a bare "Uncaught".
	t.Run("rejection of a non-Error value", func(t *testing.T) {
		_, err := b.Eval(ctx, id, `await Promise.reject("nope")`, EvalOpts{Await: true})
		if err == nil {
			t.Fatal("Promise.reject(\"nope\") succeeded, want an error")
		}
		if !strings.Contains(err.Error(), "nope") {
			t.Errorf("error = %q, want it to carry the rejected value", err)
		}
	})

	// VS-11 — a syntax error is reported in both modes.
	t.Run("syntax errors in both modes", func(t *testing.T) {
		for name, opts := range map[string]EvalOpts{
			"default": {},
			"await":   {Await: true},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := b.Eval(ctx, id, `const = ;`, opts)
				if err == nil {
					t.Fatal("a malformed expression succeeded, want an error")
				}
				if !strings.Contains(err.Error(), "SyntaxError") {
					t.Errorf("error = %q, want it to name the SyntaxError", err)
				}
			})
		}
	})

	// VS-12 — a never-settling promise is bounded, and the connection survives.
	t.Run("timeout does not wedge the connection", func(t *testing.T) {
		short, scancel := context.WithTimeout(ctx, 2*time.Second)
		defer scancel()
		start := time.Now()
		if _, err := b.Eval(short, id, `await new Promise(() => {})`, EvalOpts{Await: true}); err == nil {
			t.Fatal("a never-settling promise returned successfully, want a timeout")
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("the awaited evaluation ran for %v, well past its 2s deadline", elapsed)
		}
		// The connection must still be usable — this is what "does not wedge" means.
		got, err := evalValue(ctx, t, b, id, "1+1")
		if err != nil {
			t.Fatalf("a follow-up eval on the same connection failed: %v", err)
		}
		if got != "2" {
			t.Errorf("follow-up eval = %q, want 2", got)
		}
		m := awaited(t, "await Promise.resolve(7)")
		if fmt.Sprintf("%v", m["value"]) != "7" {
			t.Errorf("follow-up awaited eval = %#v, want 7", m["value"])
		}
	})

	// VS-14 — fetch from the page's authenticated context: the use case that
	// motivates the feature.
	t.Run("fetch from the page's authenticated context", func(t *testing.T) {
		m := awaited(t, `await fetch("/api/me").then(r => r.json())`)
		obj, ok := m["value"].(map[string]any)
		if !ok {
			t.Fatalf("value = %#v, want the parsed JSON object", m["value"])
		}
		if obj["user"] != "ada" || obj["admin"] != true {
			t.Errorf("value = %#v, want {user: ada, admin: true}", obj)
		}
	})
}

// ------------------------------------------------------- pure reporting tests
//
// The scoring heuristic can only be exercised through a real renderer, but what
// it REPORTS is what callers act on — so the threshold, the ratio, and the
// extracted/failed decision are pure functions with their own table, and they
// run under -short.

func TestArticleResultReporting(t *testing.T) {
	t.Parallel()
	full := strings.Repeat("x", 1000)
	long := strings.Repeat("y", 400)
	short := strings.Repeat("z", 40)

	cases := map[string]struct {
		ex            extraction
		minChars      int
		markdown      bool
		wantExtracted bool
		wantText      string
		wantChars     int
		wantRatio     float64
		wantFormat    string
	}{
		"confident extraction": {
			ex: extraction{Article: long, Full: full, Title: "T", Byline: "B"}, minChars: 250,
			wantExtracted: true, wantText: long, wantChars: 400, wantRatio: 0.4, wantFormat: "text",
		},
		"below the threshold falls back to full text": {
			ex: extraction{Article: short, Full: full}, minChars: 250,
			wantExtracted: false, wantText: full, wantChars: 1000, wantRatio: 1, wantFormat: "text",
		},
		"a lower threshold accepts the same extraction": {
			ex: extraction{Article: short, Full: full}, minChars: 10,
			wantExtracted: true, wantText: short, wantChars: 40, wantRatio: 0.04, wantFormat: "text",
		},
		"markdown is reported as the format": {
			ex: extraction{Article: long, Full: full}, minChars: 250, markdown: true,
			wantExtracted: true, wantText: long, wantChars: 400, wantRatio: 0.4, wantFormat: "markdown",
		},
		"nothing extracted at all": {
			ex: extraction{Article: "", Full: full}, minChars: 250,
			wantExtracted: false, wantText: full, wantChars: 1000, wantRatio: 1, wantFormat: "text",
		},
		"empty page does not divide by zero": {
			ex: extraction{Article: "", Full: ""}, minChars: 250,
			wantExtracted: false, wantText: "", wantChars: 0, wantRatio: 0, wantFormat: "text",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := articleResult(tc.ex, tc.minChars, tc.markdown)
			if got["extracted"] != tc.wantExtracted {
				t.Errorf("extracted = %v, want %v", got["extracted"], tc.wantExtracted)
			}
			if got["text"] != tc.wantText {
				t.Errorf("text length = %d, want %d", utf8.RuneCountInString(got["text"].(string)), utf8.RuneCountInString(tc.wantText))
			}
			if got["chars"] != tc.wantChars {
				t.Errorf("chars = %v, want %v", got["chars"], tc.wantChars)
			}
			if got["ratio"] != tc.wantRatio {
				t.Errorf("ratio = %v, want %v", got["ratio"], tc.wantRatio)
			}
			if got["format"] != tc.wantFormat {
				t.Errorf("format = %v, want %v", got["format"], tc.wantFormat)
			}
			// A failed extraction must say why, and say how little it found —
			// that honesty is the whole point of not making this an error.
			if !tc.wantExtracted {
				if got["reason"] == nil || got["reason"] == "" {
					t.Error("extracted:false carries no reason")
				}
				if got["article_chars"] == nil {
					t.Error("extracted:false does not report how much it did extract")
				}
			} else if _, ok := got["reason"]; ok {
				t.Errorf("a successful extraction carries a failure reason: %v", got["reason"])
			}
		})
	}
}

func TestKeptRatio(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		chars, total int
		want         float64
	}{
		"typical article": {4821, 24193, 0.199},
		"kept everything": {100, 100, 1},
		"kept nothing":    {0, 100, 0},
		"empty page":      {0, 0, 0},
		"negative total":  {10, -1, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := keptRatio(tc.chars, tc.total); got != tc.want {
				t.Errorf("keptRatio(%d, %d) = %v, want %v", tc.chars, tc.total, got, tc.want)
			}
		})
	}
}

func TestExcerpt(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		text string
		max  int
		want string
	}{
		"short text is returned whole":  {"Just a sentence.", 200, "Just a sentence."},
		"stops at the first paragraph":  {"First para.\n\nSecond para.", 200, "First para."},
		"single newlines become spaces": {"One\nTwo", 200, "One Two"},
		"truncates on a word boundary":  {"alpha bravo charlie delta", 12, "alpha bravo…"},
		"empty":                         {"", 200, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := excerpt(tc.text, tc.max); got != tc.want {
				t.Errorf("excerpt(%q, %d) = %q, want %q", tc.text, tc.max, got, tc.want)
			}
		})
	}
}
