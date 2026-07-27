package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// netBrowser serves a canned network read and records the options the CLI built,
// so the tests assert on the contract (envelope + exit code + what reached the
// browser) rather than on rendered text.
type netBrowser struct {
	fakeBrowser
	gotOpts  chrome.NetOpts
	gotCond  chrome.NetCond
	requests []any
	stream   []any // payloads NetStream emits, in order
	streamed bool
	waited   bool
	waitErr  error
}

func (n *netBrowser) Net(_ context.Context, _ string, opts chrome.NetOpts) (any, error) {
	n.gotOpts = opts
	return map[string]any{
		"requests": n.requests, "count": len(n.requests),
		"buffered": 214, "dropped": 3, "truncated": false, "pending": 2,
	}, nil
}

func (n *netBrowser) NetStream(_ context.Context, _ string, opts chrome.NetOpts, emit func(any) error) error {
	n.gotOpts, n.streamed = opts, true
	for _, p := range n.stream {
		if err := emit(p); err != nil {
			return err
		}
	}
	return nil
}

func (n *netBrowser) NetWait(_ context.Context, _ string, cond chrome.NetCond) (map[string]any, error) {
	n.gotCond, n.waited = cond, true
	if n.waitErr != nil {
		return nil, n.waitErr
	}
	return map[string]any{"matched": true, "request": req("POST", "https://app/api/save", 200)}, nil
}

func netTestBrowser(reqs ...any) *netBrowser {
	return &netBrowser{
		fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}},
		requests:    reqs,
	}
}

func req(method, url string, status int) map[string]any {
	return map[string]any{"method": method, "url": url, "status": status, "type": "xhr", "failed": status >= 300}
}

func TestNetEnvelopeShape(t *testing.T) {
	t.Parallel()
	b := netTestBrowser(req("POST", "https://app/api/timesheet", 200))
	env, _, code := run(t, b, "net", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if env["ok"] != true || env["command"] != "net" {
		t.Fatalf("envelope = %v", env)
	}
	res := env["result"].(map[string]any)
	for _, k := range []string{"requests", "count", "buffered", "dropped", "truncated", "pending"} {
		if _, has := res[k]; !has {
			t.Errorf("result is missing %q — it is part of the documented envelope", k)
		}
	}
	if res["count"].(float64) != 1 || res["buffered"].(float64) != 214 {
		t.Errorf("result = %v, want count 1 of 214 buffered", res)
	}
	// dropped tells a caller it read too late; pending tells it "not finished
	// yet" as distinct from "nothing matched". Both must survive to the envelope.
	if res["dropped"].(float64) != 3 || res["pending"].(float64) != 2 {
		t.Errorf("dropped/pending = %v/%v, want 3/2", res["dropped"], res["pending"])
	}
}

// Filtering is server-side: the flags must arrive at the browser as options, not
// be applied to an already-marshalled result.
func TestNetFiltersAreSentToTheBrowser(t *testing.T) {
	t.Parallel()
	b := netTestBrowser()
	_, _, code := run(t, b,
		"net", "--target", "aa11", "--json",
		"--url", "/api/save", "--method", "post", "--method", "put",
		"--status", ">=400", "--type", "xhr", "--limit", "20", "--since", "30s",
		"--headers", "--body", "--clear")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got := b.gotOpts
	if got.URL != "/api/save" {
		t.Errorf("URL = %q", got.URL)
	}
	// Methods are upper-cased so `--method post` matches what the wire carries.
	if strings.Join(got.Methods, ",") != "POST,PUT" {
		t.Errorf("Methods = %v, want [POST PUT]", got.Methods)
	}
	if got.Status != ">=400" || strings.Join(got.Types, ",") != "xhr" {
		t.Errorf("Status/Types = %q/%v", got.Status, got.Types)
	}
	if got.Limit != 20 || got.Since.String() != "30s" || !got.Clear {
		t.Errorf("opts = %+v, want limit 20 / since 30s / clear", got)
	}
	if !got.Headers || !got.Body {
		t.Errorf("--headers/--body did not reach the browser: %+v", got)
	}
	// Redaction is on by default: NoRedact must be false unless asked for.
	if got.NoRedact {
		t.Error("NoRedact is set without --no-redact; redaction must be the default")
	}
}

func TestNetNoRedactIsOptIn(t *testing.T) {
	t.Parallel()
	b := netTestBrowser()
	if _, _, code := run(t, b, "net", "--target", "aa11", "--json", "--headers", "--no-redact"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !b.gotOpts.NoRedact {
		t.Error("--no-redact did not reach the browser")
	}
}

func TestNetXHRIsShorthandForXHRAndFetch(t *testing.T) {
	t.Parallel()
	b := netTestBrowser()
	if _, _, code := run(t, b, "net", "--target", "aa11", "--json", "--xhr"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Join(b.gotOpts.Types, ",") != "xhr,fetch" {
		t.Errorf("Types = %v, want [xhr fetch] — modern apps use fetch, older ones XHR", b.gotOpts.Types)
	}
}

// Aliases exist because they are what people type; they must reach the browser
// as the canonical vocabulary the filter understands.
func TestNetTypeAliasesNormalize(t *testing.T) {
	t.Parallel()
	b := netTestBrowser()
	if _, _, code := run(t, b, "net", "--target", "aa11", "--json", "--type", "css", "--type", "IMG"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Join(b.gotOpts.Types, ",") != "stylesheet,image" {
		t.Errorf("Types = %v, want [stylesheet image]", b.gotOpts.Types)
	}
}

// VS-4 and its siblings: every malformed invocation is usage/exit 2 with the
// browser never contacted. noCall proves the second half — asserting only on the
// exit code would also pass for a command that connected first and validated
// afterwards, which means a consent prompt the user should never have seen.
func TestNetValidationNeverConnects(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"bad status class":          {"net", "--status", "20x"},
		"nonsense status":           {"net", "--status", "abc"},
		"empty status":              {"net", "--status", ""},
		"unknown type":              {"net", "--type", "widget"},
		"invalid url regex":         {"net", "--url", "re:("},
		"bad since":                 {"net", "--since", "banana"},
		"negative limit":            {"net", "--limit", "-2"},
		"follow with fail-on-match": {"net", "--follow", "--fail-on-match"},
		"wait with no matcher":      {"net", "wait"},
		"wait bad status":           {"net", "wait", "--url", "/api", "--status", "2x"},
		"wait --request bad status": {"wait", "--request", "/api", "--status", "nope"},
		"wait qualifier no request": {"wait", "--status", "2xx"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, _, code := run(t, noCall(t), append(args, "--target", "aa11", "--json")...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage)", code)
			}
			if env == nil {
				return // a cobra parse failure renders to stderr; exit 2 is the contract
			}
			if got := env["error"].(map[string]any)["code"]; got != "usage" {
				t.Errorf("error.code = %v, want usage", got)
			}
		})
	}
}

// An unset --status must NOT be a usage error — only an explicitly empty one is.
// Getting this backwards would make a bare `net` unusable.
func TestNetWithoutStatusIsNotAUsageError(t *testing.T) {
	t.Parallel()
	b := netTestBrowser()
	if _, _, code := run(t, b, "net", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.gotOpts.Status != "" {
		t.Errorf("Status = %q, want empty (no filter)", b.gotOpts.Status)
	}
}

// A streaming line inside `session` would emit many objects for one command,
// breaking the one-envelope-per-line contract the batch mode promises.
func TestNetFollowInsideSessionIsUsage(t *testing.T) {
	t.Parallel()
	b := netTestBrowser()
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithInput(strings.NewReader(`["net","--follow","--target","aa11"]` + "\n"))
	if code := app.Execute("session"); code != 0 {
		t.Fatalf("session exit = %d, want 0 (per-line failures ride in the envelope)", code)
	}
	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &env); err != nil {
		t.Fatalf("session line is not one JSON envelope: %v\n%s", err, out.String())
	}
	if env["ok"] != false || env["error"].(map[string]any)["code"] != "usage" {
		t.Errorf("envelope = %v, want a usage error", env)
	}
	if b.streamed {
		t.Error("NetStream ran inside session; --follow must be rejected before it starts")
	}
}

// --fail-on-match exits 1 AND still reports the requests: a CI log has to show
// WHAT failed, not just that something did.
func TestNetFailOnMatchExitsOneWithTheEvidence(t *testing.T) {
	t.Parallel()
	b := netTestBrowser(req("GET", "https://app/api/me", 401))
	env, _, code := run(t, b, "net", "--target", "aa11", "--json", "--failed", "--fail-on-match")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if env["ok"] != false {
		t.Errorf("ok = %v, want false", env["ok"])
	}
	// The shared assertion code, not a new one: `nonzero means the assertion
	// tripped` has to keep working in a `set -e` script.
	if got := env["error"].(map[string]any)["code"]; got != "assertion_failed" {
		t.Errorf("error.code = %v, want assertion_failed", got)
	}
	res, ok := env["result"].(map[string]any)
	if !ok {
		t.Fatalf("the failing envelope dropped its result: %v", env)
	}
	reqs := res["requests"].([]any)
	if len(reqs) != 1 || reqs[0].(map[string]any)["status"].(float64) != 401 {
		t.Errorf("result.requests = %v, want the matching 401", reqs)
	}
}

func TestNetFailOnMatchIsSilentWhenNothingMatched(t *testing.T) {
	t.Parallel()
	b := netTestBrowser() // no requests
	env, _, code := run(t, b, "net", "--target", "aa11", "--json", "--failed", "--fail-on-match")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — no failed requests is the passing case", code)
	}
	if env["ok"] != true {
		t.Errorf("envelope = %v, want ok", env)
	}
}

// --follow writes one NDJSON object per completed request, in order, each line
// independently parseable, and exits 0 when the window closes.
func TestNetFollowStreamsNDJSON(t *testing.T) {
	t.Parallel()
	b := netTestBrowser()
	b.stream = []any{
		map[string]any{"requests": []any{req("GET", "https://app/a", 200)}, "count": 1, "buffered": 1, "dropped": 0},
		map[string]any{"requests": []any{req("POST", "https://app/b", 500)}, "count": 1, "buffered": 2, "dropped": 0},
		map[string]any{"requests": []any{req("GET", "https://app/c", 200)}, "count": 1, "buffered": 3, "dropped": 0},
	}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	if code := app.Execute("net", "--target", "aa11", "--follow"); code != 0 {
		t.Fatalf("exit = %d, want 0 (the follow window closing is not a failure): %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d NDJSON lines, want 3:\n%s", len(lines), out.String())
	}
	want := []string{"https://app/a", "https://app/b", "https://app/c"}
	for i, line := range lines {
		var env map[string]any
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("line %d is not independently parseable: %v\n%s", i, err, line)
		}
		if env["command"] != "net" || env["ok"] != true {
			t.Errorf("line %d envelope = %v", i, env)
		}
		got := env["result"].(map[string]any)["requests"].([]any)[0].(map[string]any)["url"]
		if got != want[i] {
			t.Errorf("line %d url = %v, want %q — order must be preserved", i, got, want[i])
		}
	}
}

// --follow streams NDJSON envelopes whether or not --json was passed, the same
// way `session` does, so a caller parses one shape in both modes.
func TestNetFollowIsNDJSONWithoutJSONFlag(t *testing.T) {
	t.Parallel()
	b := netTestBrowser()
	b.stream = []any{map[string]any{"requests": []any{req("GET", "https://app/only", 200)}, "count": 1}}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	if code := app.Execute("net", "--target", "aa11", "--follow"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Errorf("follow output is not JSON: %q", out.String())
	}
}

func TestNetWithoutTargetIsTargetError(t *testing.T) {
	t.Parallel()
	b := &netBrowser{fakeBrowser: fakeBrowser{tabs: []target.Info{
		{ID: "aa11", Title: "A", URL: "u"}, {ID: "bb22", Title: "B", URL: "v"},
	}}}
	env, _, code := run(t, b, "net", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (target)", code)
	}
	if env["error"].(map[string]any)["code"] != "no_current_target" {
		t.Errorf("error.code = %v", env["error"])
	}
}

// ---------------------------------------------------------------------------
// Waiting for a request: `wait --request` (primary) and `net wait` (alias)
// ---------------------------------------------------------------------------

// Both spellings must build the SAME condition, or the alias becomes a second,
// subtly different verb.
func TestWaitRequestAndNetWaitBuildTheSameCondition(t *testing.T) {
	t.Parallel()
	forms := map[string][]string{
		"wait --request": {"wait", "--request", "/api/save", "--method", "POST", "--status", "2xx"},
		"net wait":       {"net", "wait", "--url", "/api/save", "--method", "POST", "--status", "2xx"},
	}
	for name, args := range forms {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := netTestBrowser()
			env, _, code := run(t, b, append(args, "--target", "aa11", "--json")...)
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if !b.waited {
				t.Fatal("NetWait was never called")
			}
			got := b.gotCond
			if got.URL != "/api/save" || strings.Join(got.Methods, ",") != "POST" || got.Status != "2xx" {
				t.Errorf("cond = %+v", got)
			}
			res := env["result"].(map[string]any)
			if res["matched"] != true {
				t.Errorf("result = %v, want matched", res)
			}
			// The matched request rides in `request`, in the same shape a listing
			// uses, so one parser handles both.
			if _, has := res["request"]; !has {
				t.Error("result is missing `request`, the matched record")
			}
		})
	}
}

// A wait that finds nothing is exit 4 / target_timeout — the same code a `wait
// --visible` timeout uses, so an agent's retry logic does not need a special
// case for the network.
func TestNetWaitTimeoutIsTargetTimeout(t *testing.T) {
	t.Parallel()
	b := netTestBrowser()
	b.waitErr = context.DeadlineExceeded
	env, _, code := run(t, b, "wait", "--request", "/never", "--target", "aa11", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (target/timeout)", code)
	}
	if got := env["error"].(map[string]any)["code"]; got != "target_timeout" {
		t.Errorf("error.code = %v, want target_timeout", got)
	}
}

// --request routes to NetWait; the page-level conditions must still route to
// Wait, or adding the request condition would have broken every existing wait.
func TestWaitWithoutRequestStillUsesThePageConditions(t *testing.T) {
	t.Parallel()
	b := netTestBrowser()
	if _, _, code := run(t, b, "wait", "--text", "Saved", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.waited {
		t.Error("wait --text called NetWait; only --request blocks on a request")
	}
}

func TestNetWaitFailedNeedsNoURL(t *testing.T) {
	t.Parallel()
	b := netTestBrowser()
	if _, _, code := run(t, b, "wait", "--request", "", "--failed", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("exit = %d, want 0 — --failed is a complete condition on its own", code)
	}
	if !b.gotCond.Failed {
		t.Errorf("cond = %+v, want Failed", b.gotCond)
	}
}

// ---------------------------------------------------------------------------
// Fuzzing the --status grammar
// ---------------------------------------------------------------------------

// FuzzNetStatusSpec drives the parser at its boundary. The properties it asserts
// are the ones that matter for a filter used in assertions:
//
//   - it never panics, whatever a user (or an agent) types;
//   - a parsed matcher is deterministic;
//   - a record with NO status never matches, so a negated spec cannot silently
//     turn every in-flight request into an assertion failure;
//   - negation is the exact complement, so `!2xx` and `2xx` partition the space
//     rather than overlapping or leaving a gap.
func FuzzNetStatusSpec(f *testing.F) {
	for _, seed := range []string{
		"200", "2xx", "4xx", ">=400", "<400", "!=204", "!2xx", "!404", "  2xx  ",
		"20x", "abc", "", "!", "!!", ">=", ">=abc", "6xx", "0xx", "1234", "20",
		"\x00", "２００", ">=4 0 0", "==200", "<=599",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		m, err := chrome.ParseNetStatus(spec)
		if err != nil {
			if m != nil {
				t.Fatalf("ParseNetStatus(%q) returned both a matcher and an error", spec)
			}
			return
		}
		if m == nil {
			t.Fatalf("ParseNetStatus(%q) returned no matcher and no error", spec)
		}
		// A statusless record (a transport failure, or one still in flight) must
		// never satisfy a status assertion.
		if m(0, false) {
			t.Fatalf("ParseNetStatus(%q) matched a record with no status", spec)
		}
		for status := int64(100); status < 600; status++ {
			got := m(status, true)
			if m(status, true) != got {
				t.Fatalf("ParseNetStatus(%q) is not deterministic at %d", spec, status)
			}
		}
		// Negation is an exact complement over the statuses that exist. Skip the
		// two spellings where prefixing "!" would parse as a different operator:
		// an already-negated spec, and one starting with "=" (where "!"+"==200"
		// reads as the not-equal comparison "!=" and is correctly rejected).
		trimmed := strings.TrimSpace(spec)
		if strings.HasPrefix(trimmed, "!") || strings.HasPrefix(trimmed, "=") {
			return
		}
		neg, err := chrome.ParseNetStatus("!" + trimmed)
		if err != nil {
			t.Fatalf("%q parses but !%q does not: %v", spec, spec, err)
		}
		for status := int64(100); status < 600; status++ {
			if neg(status, true) == m(status, true) {
				t.Fatalf("!%q is not the complement of %q at status %d", spec, spec, status)
			}
		}
	})
}
