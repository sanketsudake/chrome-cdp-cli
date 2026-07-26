package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// oneTab is the tab list every reading test resolves --target against.
func oneTab() []target.Info {
	return []target.Info{{ID: "aa11", Title: "X", URL: "https://example.com/"}}
}

// evalSpy records what the CLI actually asked the browser to evaluate, so a test
// can pin the wire call and not just the envelope.
type evalSpy struct {
	fakeBrowser
	expr  string
	await bool
	value any
}

func (e *evalSpy) Eval(_ context.Context, _, expr string, opts chrome.EvalOpts) (any, error) {
	e.await = opts.Await
	e.expr = expr
	return map[string]any{"value": e.value}, nil
}

var _ chrome.Browser = (*evalSpy)(nil)

// VS-13 — the regression guard, written before the Eval signature change.
//
// Without --await, `eval` must behave exactly as it always has: the expression
// reaches the browser verbatim, the envelope carries {"value": …} and nothing
// else, and no `awaited` marker appears. Everything RFC-0010 adds is opt-in, so
// this test must pass unchanged before and after the feature lands.
func TestEvalWithoutAwaitIsUnchanged(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		expr  string
		value any
	}{
		"arithmetic":      {"1+1", 2.0},
		"document title":  {"document.title", "Fixture"},
		"object literal":  {"({a: 1})", map[string]any{"a": 1.0}},
		"iife workaround": {"(() => 7)()", 7.0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := &evalSpy{value: tc.value}
			b.tabs = oneTab()
			env, _, code := run(t, b, "eval", tc.expr, "--target", "aa11", "--json")
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if b.expr != tc.expr {
				t.Errorf("browser saw expression %q, want the argument verbatim (%q)", b.expr, tc.expr)
			}
			res, ok := env["result"].(map[string]any)
			if !ok {
				t.Fatalf("result = %#v, want an object", env["result"])
			}
			if got, want := fmt.Sprintf("%v", res["value"]), fmt.Sprintf("%v", tc.value); got != want {
				t.Errorf("result.value = %s, want %s", got, want)
			}
			if _, present := res["awaited"]; present {
				t.Errorf("result carries `awaited` on the default (non---await) path: %#v", res)
			}
			if len(res) != 1 {
				t.Errorf("result = %#v, want exactly the pre-existing {value} shape", res)
			}
		})
	}
}

// VS-7 — flag validation, and the contract that it happens BEFORE connecting.
//
// noCall fails the test if the CLI reaches for Chrome at all, so this asserts
// the ordering and not merely the exit code: agents rely on exit 2 meaning
// "your call was wrong, don't retry", and a connection attempt means a consent
// prompt the user should never have seen.
func TestTextFlagValidationHappensBeforeConnecting(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"--markdown without --article":  {"text", "--markdown"},
		"--min-chars without --article": {"text", "--min-chars", "100"},
		"--article with a selector":     {"text", "--article", "main"},
		"--article with both":           {"text", "--article", "--markdown", "main"},
		"negative --min-chars":          {"text", "--article", "--min-chars", "-1"},
		"no selector and no --article":  {"text"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, _, code := run(t, noCall(t), append(args, "--target", "aa11", "--json")...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage)", code)
			}
			e, ok := env["error"].(map[string]any)
			if !ok {
				t.Fatalf("envelope has no error object: %#v", env)
			}
			if e["code"] != "usage" {
				t.Errorf("error.code = %v, want usage", e["code"])
			}
			if msg, _ := e["message"].(string); msg == "" {
				t.Error("usage error carries no message")
			}
		})
	}
}

// textSpy records the TextOpts the CLI built, so flag plumbing is pinned at the
// seam rather than inferred from the envelope.
type textSpy struct {
	fakeBrowser
	sel  string
	opts chrome.TextOpts
	res  map[string]any
}

func (s *textSpy) Text(_ context.Context, _, sel string, opts chrome.TextOpts) (map[string]any, error) {
	s.sel, s.opts = sel, opts
	if s.res != nil {
		return s.res, nil
	}
	return map[string]any{"text": "body"}, nil
}

var _ chrome.Browser = (*textSpy)(nil)

func TestTextFlagsReachTheBrowser(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args []string
		want chrome.TextOpts
		sel  string
	}{
		"plain selector keeps the pre-existing behaviour": {
			args: []string{"text", "h1"},
			want: chrome.TextOpts{MinChars: chrome.DefaultArticleMinChars, Query: chrome.QueryOpts{By: "css", Wait: "visible"}},
			sel:  "h1",
		},
		"article takes no selector": {
			args: []string{"text", "--article"},
			want: chrome.TextOpts{Article: true, MinChars: chrome.DefaultArticleMinChars, Query: chrome.QueryOpts{By: "css", Wait: "visible"}},
		},
		"markdown and an explicit threshold": {
			args: []string{"text", "--article", "--markdown", "--min-chars", "40"},
			want: chrome.TextOpts{Article: true, Markdown: true, MinChars: 40, Query: chrome.QueryOpts{By: "css", Wait: "visible"}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := &textSpy{}
			b.tabs = oneTab()
			_, _, code := run(t, b, append(tc.args, "--target", "aa11", "--json")...)
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if b.sel != tc.sel {
				t.Errorf("selector = %q, want %q", b.sel, tc.sel)
			}
			if b.opts != tc.want {
				t.Errorf("TextOpts = %+v, want %+v", b.opts, tc.want)
			}
		})
	}
}

// A low-confidence extraction is exit 0 with an honest flag, not an error —
// the envelope contract RFC-0010 US-5 turns on.
func TestArticleLowConfidenceIsNotAnError(t *testing.T) {
	t.Parallel()
	b := &textSpy{res: map[string]any{
		"text": "the whole page", "extracted": false, "chars": 14, "total_chars": 14,
		"ratio": 1.0, "reason": "extraction kept only 3 characters", "format": "text",
	}}
	b.tabs = oneTab()
	env, _, code := run(t, b, "text", "--article", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — a low-confidence extraction is not a failure", code)
	}
	if env["ok"] != true {
		t.Fatalf("ok = %v, want true", env["ok"])
	}
	res := env["result"].(map[string]any)
	if res["extracted"] != false {
		t.Errorf("result.extracted = %v, want false", res["extracted"])
	}
	if res["reason"] == "" || res["reason"] == nil {
		t.Error("result carries no reason for the failed extraction")
	}
}

// --await is opt-in and must reach the browser as an option, not be inferred.
func TestEvalAwaitFlagReachesTheBrowser(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args      []string
		wantAwait bool
	}{
		"default":      {[]string{"eval", "1+1"}, false},
		"with --await": {[]string{"eval", "--await", "await window.p"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := &evalSpy{value: 1.0}
			b.tabs = oneTab()
			_, _, code := run(t, b, append(tc.args, "--target", "aa11", "--json")...)
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if b.await != tc.wantAwait {
				t.Errorf("EvalOpts.Await = %v, want %v", b.await, tc.wantAwait)
			}
		})
	}
}

// rejectBrowser fails an eval the way a rejected promise does.
type rejectBrowser struct {
	fakeBrowser
	err error
}

func (r *rejectBrowser) Eval(context.Context, string, string, chrome.EvalOpts) (any, error) {
	return nil, r.err
}

var _ chrome.Browser = (*rejectBrowser)(nil)

// VS-10 at the envelope boundary: a rejection is a cdp_error (exit 5) carrying
// the message and stack — never a successful envelope with an odd-looking value.
func TestEvalRejectionIsAnError(t *testing.T) {
	t.Parallel()
	b := &rejectBrowser{err: &chrome.JSError{
		Message: "Error: boom",
		Stack:   "at <anonymous> (about:blank:1:7)",
	}}
	b.tabs = oneTab()
	env, _, code := run(t, b, "eval", "--await", `await Promise.reject(new Error("boom"))`, "--target", "aa11", "--json")
	if code != 5 {
		t.Fatalf("exit = %d, want 5 (cdp_error)", code)
	}
	if env["ok"] != false {
		t.Fatalf("ok = %v, want false", env["ok"])
	}
	if _, present := env["result"]; present {
		t.Errorf("a rejected promise produced a result field: %#v", env["result"])
	}
	e := env["error"].(map[string]any)
	if e["code"] != "cdp_error" {
		t.Errorf("error.code = %v, want cdp_error", e["code"])
	}
	if msg, _ := e["message"].(string); !strings.Contains(msg, "boom") {
		t.Errorf("error.message = %q, want it to contain the rejection message", msg)
	}
	if stack, _ := e["stack"].(string); stack == "" {
		t.Error("error details carry no stack")
	}
	if e["awaited"] != true {
		t.Errorf("error details do not record awaited:true: %#v", e)
	}
}

// VS-12 at the envelope boundary: a never-settling promise times out as
// target_timeout / exit 4, the same as any other command that ran out of time.
func TestEvalAwaitTimeoutIsExitFour(t *testing.T) {
	t.Parallel()
	b := &rejectBrowser{err: context.DeadlineExceeded}
	b.tabs = oneTab()
	env, _, code := run(t, b, "eval", "--await", "await new Promise(() => {})", "--target", "aa11", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (target/timeout)", code)
	}
	if env["error"].(map[string]any)["code"] != "target_timeout" {
		t.Errorf("error.code = %v, want target_timeout", env["error"])
	}
}
