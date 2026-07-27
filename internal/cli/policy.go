package cli

// The domain-policy enforcement hook (RFC-0012), the --allow / --policy-off
// global flags, and `chrome-cdp policy init`.
//
// Enforcement lives HERE, at the command boundary, and not in internal/chrome.
// In the driver every future Browser method would have to remember to check,
// and the one that forgot would be a silent hole; at the boundary the check
// sits in the same place argument validation already does, and one call in
// resolveTarget covers every verb that acts on a tab. TestEveryCommandIsClassified
// is what keeps that true as verbs are added.
//
// The decisions themselves are made by internal/policy, which is pure. This
// file is only the wiring: config → Checker, refusal → envelope, and the audit
// log.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
	"github.com/sanketsudake/chrome-cdp-cli/internal/policy"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// policyConfigured reports whether anything could make the layer act. It is the
// fast path that keeps an unconfigured CLI byte-identical to the previous
// release (RFC-0012 US-5 / VS-10): with no [policy] table and no --allow, this
// file does nothing at all.
func (a *App) policyConfigured() bool {
	return a.defaults.Policy.Present || len(a.allowFlag) > 0
}

// policyConfig folds the command-line overrides onto the configured table.
//
// --allow REPLACES the configured allow-list rather than extending it (a
// one-off override that could only ever widen the boundary would be a poor
// override), and implies the layer is on, so `chrome-cdp --allow "*.x.test" …`
// works with no config file at all. deny, read_only and verbs_denied still
// apply: --allow narrows what is reachable, it never unblocks something.
func (a *App) policyConfig() policy.Config {
	p := a.defaults.Policy
	cfg := policy.Config{
		Present:     p.Present,
		Enabled:     p.Enabled,
		Allow:       p.Allow,
		Deny:        p.Deny,
		ReadOnly:    p.ReadOnly,
		VerbsDenied: p.VerbsDenied,
		UploadRoots: expandPaths(p.UploadRoots),
		AuditLog:    expandHome(p.AuditLog),
		AuditAll:    p.AuditAll,
		OnViolation: p.OnViolation,
		Source:      p.Source,
	}
	if len(a.allowFlag) > 0 {
		cfg.Present, cfg.Enabled = true, true
		cfg.Allow = a.allowFlag
		if cfg.Source == "" {
			cfg.Source = "--allow"
		}
	}
	return cfg
}

// policyVerb is the classification key for the command being run: its full
// cobra path minus the root, so `cookie set` and `cookie list` classify
// separately. It is captured per Execute in PersistentPreRun, which is also
// what makes it reset between `session` lines.
func (a *App) policyVerb() string { return a.verbPath }

// checkPolicyConfig validates the policy itself, with no origin involved, so it
// can run BEFORE the browser is contacted.
//
// Unlike every other malformed-config path in this repo, a policy that cannot
// be read refuses to run. Warning and continuing would leave the user bounded
// in their head and unbounded in fact, and a policy that fails open is worse
// than no policy (RFC-0012 VS-15). It refuses before connecting because a
// policy we could not read cannot have permitted anything, so there is nothing
// to launch Chrome or raise a consent prompt for.
//
// Exempt verbs are let through: they are never origin-checked, so no policy
// could have restricted them, and refusing `list` would trap the user inside a
// broken config with no way to look around.
func (a *App) checkPolicyConfig(verb string) *result.Err {
	if !a.policyConfigured() || a.policyOff {
		return nil
	}
	if class, _ := policy.Classify(verb); class == policy.Exempt {
		return nil
	}
	if m := a.defaults.Policy.Malformed; m != "" {
		return a.policyConfigErr(m, a.defaults.Policy.Source)
	}
	cfg := a.policyConfig()
	if _, err := policy.New(cfg); err != nil {
		return a.policyConfigErr(err.Error(), cfg.Source)
	}
	return nil
}

func (a *App) policyConfigErr(reason, source string) *result.Err {
	return &result.Err{
		Code:    result.CodeUsage,
		Message: "refusing to run with a policy that could not be read: " + reason + " — fix it, or pass --policy-off to run without a policy",
		Details: map[string]any{"config": source},
	}
}

// policyDeniesVerb reports whether the configured policy names this verb in
// verbs_denied. It reads the parsed config rather than building a Checker so it
// can be consulted before the Exempt short-circuit without paying for pattern
// compilation on every command.
func (a *App) policyDeniesVerb(verb string) bool {
	for _, v := range a.defaults.Policy.VerbsDenied {
		if v == verb {
			return true
		}
	}
	return false
}

// checkPolicy is the enforcement hook. It returns nil to proceed, or the
// envelope error to emit instead of acting.
//
// Ordering matters: --policy-off is honoured before the malformed-config
// refusal, because a bad policy that cannot be bypassed is worse than none.
func (a *App) checkPolicy(verb, rawURL string) *result.Err {
	if !a.policyConfigured() {
		return nil
	}
	// Tab and meta verbs are never origin-checked, so there is nothing here to
	// enforce, bypass, or audit for them — UNLESS verbs_denied names one. That
	// entry is about the verb, not an origin, so no class puts a verb out of its
	// reach: accepting `verbs_denied = ["recipe run"]` in the config and then
	// short-circuiting past it here would fail open on precisely the line the
	// operator went out of their way to write.
	if class, _ := policy.Classify(verb); class == policy.Exempt && !a.policyDeniesVerb(verb) {
		return nil
	}
	if a.policyOff {
		a.notePolicyOff(verb, rawURL)
		return nil
	}
	if perr := a.checkPolicyConfig(verb); perr != nil {
		return perr
	}
	cfg := a.policyConfig()
	c, err := policy.New(cfg)
	if err != nil {
		return a.policyConfigErr(err.Error(), cfg.Source)
	}
	if !c.Active() {
		return nil
	}

	d := c.Check(rawURL, verb)
	origin := originOf(rawURL)
	if d.Allowed {
		if c.AuditAll() {
			a.auditAppend(c.AuditLog(), auditRecord{Origin: origin, Verb: verb, Decision: "allowed", Rule: d.Rule})
		}
		return nil
	}

	// `prompt` asks a human and refuses when there is no human — it must never
	// block a piped, daemonised, or MCP run waiting for input nobody will
	// provide (RFC-0012 VS-13).
	if c.OnViolation() == policy.OnPrompt && a.interactive() {
		q := fmt.Sprintf("policy: %s on %s is refused by %s. Allow this once?", verb, origin, d.Rule)
		if a.askYesNo(q) {
			a.auditAppend(c.AuditLog(), auditRecord{Origin: origin, Verb: verb, Decision: "allowed_by_prompt", Rule: d.Rule})
			return nil
		}
	}

	a.auditAppend(c.AuditLog(), auditRecord{Origin: origin, Verb: verb, Decision: "refused", Rule: d.Rule})
	if !a.quiet {
		fmt.Fprintf(a.err, "policy: refused %s on %s (%s)\n", verb, origin, d.Rule)
	}
	return &result.Err{
		Code:    result.CodePermissionDenied,
		Message: d.Reason,
		Details: map[string]any{
			"origin": origin,
			"verb":   verb,
			"rule":   d.Rule,
			"config": cfg.Source,
		},
	}
}

// closeRule is the rule name a refused close reports and audits. `close` is
// checked against the allow/deny lists alone — no verb question is being asked —
// so it names that rather than borrowing Check's more specific wording.
const closeRule = "allow: origin not permitted for close"

// boundClose applies the origin allow-list to `close`, but only when an MCP
// server is driving.
//
// `close` is Exempt in the classification table and stays that way for a person
// at a shell: closing a tab touches no page content, and origin-checking it
// would produce refusals far from their cause (RFC-0012 open question 3). An
// agent is a different caller. It reads page content under a boundary the user
// wrote, and a server that enforced the allow-list for reads and not for
// destruction would be enforcing half a boundary — the reviewer's case was a
// --read-only server, advertising that it could not change anything, closing a
// tab on a denied origin.
//
// So the rule is per tab rather than per command: a named tab outside the list
// is refused, and a bulk close closes the tabs the policy permits and reports
// the rest under `refused` rather than silently doing less than it was asked.
// The CLI's own behaviour is unchanged — a.mcpLock is nil there.
func (a *App) boundClose(victims []result.TargetInfo) ([]result.TargetInfo, []any, *result.Err) {
	c := a.policyChecker()
	if a.mcpLock == nil || !c.Active() {
		return victims, nil, nil
	}
	var kept []result.TargetInfo
	var refused []any
	for _, v := range victims {
		if c.OriginAllowed(v.URL) {
			kept = append(kept, v)
			continue
		}
		origin := originOf(v.URL)
		a.auditAppend(c.AuditLog(), auditRecord{Origin: origin, Verb: "close", Decision: "refused", Rule: closeRule})
		if !a.quiet {
			fmt.Fprintf(a.err, "policy: refused close on %s (%s)\n", origin, closeRule)
		}
		refused = append(refused, map[string]any{"id": v.ID, "origin": origin})
	}
	if len(kept) > 0 {
		return kept, refused, nil
	}
	origins := make([]string, 0, len(refused))
	for _, r := range refused {
		origins = append(origins, r.(map[string]any)["origin"].(string))
	}
	return nil, nil, &result.Err{
		Code: result.CodePermissionDenied,
		Message: fmt.Sprintf("close is bounded by the policy allow-list when an MCP client is driving, and %s is not on it",
			strings.Join(origins, ", ")),
		Details: map[string]any{
			"origin": origins[0],
			"verb":   "close",
			"rule":   closeRule,
			"config": c.Source(),
		},
	}
}

// notePolicyOff records the explicit bypass. --policy-off exists so a bad
// policy is fixable, but it is never implicit: it warns on stderr and lands in
// the audit log (RFC-0012 VS-12).
func (a *App) notePolicyOff(verb, rawURL string) {
	// `nav <url>` is checked twice — destination, then current origin — and one
	// command is one bypass, so it says so once.
	if a.policyOffNoted {
		return
	}
	a.policyOffNoted = true
	if !a.quiet {
		fmt.Fprintf(a.err, "policy: --policy-off — the configured policy is NOT being enforced for %s\n", verb)
	}
	a.auditAppend(expandHome(a.defaults.Policy.AuditLog), auditRecord{
		Origin: originOf(rawURL), Verb: verb, Decision: "bypassed", Rule: "--policy-off",
	})
}

// policyChecker returns the effective checker for the ORIGIN policy's read-only
// questions — `list` and target redaction — or nil when there is nothing to ask.
// Enforcement goes through checkPolicy instead, which also audits and can
// prompt.
//
// It honours --policy-off, which is in-model for the origin layer: RFC-0012 says
// so, and the bypass is warned and audited. upload_roots does NOT come through
// here; see uploadRoots.
func (a *App) policyChecker() *policy.Checker {
	if a.policyOff {
		return nil
	}
	return a.configuredChecker()
}

// configuredChecker builds a checker from the configuration as written,
// ignoring --policy-off. Only the filesystem boundary uses it; everything about
// origins goes through policyChecker.
func (a *App) configuredChecker() *policy.Checker {
	if !a.policyConfigured() || a.defaults.Policy.Malformed != "" {
		return nil
	}
	c, err := policy.New(a.policyConfig())
	if err != nil {
		return nil
	}
	return c
}

// redactTab reduces a tab the policy does not cover to its origin (RFC-0012
// VS-17).
//
// `list` is not itself policy-checked — it has to keep working so a user can
// see where they are — but under an active policy it should not hand a caller
// the full URL and title of every other tab the browser has open. The tab stays
// listed and stays addressable by id: this narrows what a misdirected agent can
// read, without pretending the tab is not there.
func (a *App) redactTab(r tabRow) tabRow {
	c := a.policyChecker()
	if !c.Active() || c.OriginAllowed(r.URL) {
		return r
	}
	r.Title, r.URL = "", redactedURL(r.URL)
	return r
}

// redactTarget applies the same reduction to the envelope's `target` field.
//
// It is called from the EMIT boundary rather than from each verb, and that is
// the point of it. `use`, `activate`, `close` and `policy init` are Exempt —
// they never act on page content, so they are not origin-checked — but they DO
// resolve a target, and emitting its raw TargetInfo handed back the full URL
// (query string and all) and the title of a tab the policy covers, for free and
// with no side effect. Redacting at the one place every envelope passes through
// means the next Exempt verb cannot forget.
func (a *App) redactTarget(t *result.TargetInfo) *result.TargetInfo {
	if t == nil {
		return nil
	}
	c := a.policyChecker()
	if !c.Active() || c.OriginAllowed(t.URL) {
		return t
	}
	out := *t
	out.Title, out.URL = "", redactedURL(t.URL)
	return &out
}

// redactedURL reduces a URL to its origin, or to a scheme-only placeholder when
// it has no origin to show.
func redactedURL(rawURL string) string {
	if o, err := policy.ParseOrigin(rawURL); err == nil && o.Scheme != "" {
		return o.Scheme + "://" + o.String()
	}
	return originOf(rawURL)
}

// originOf renders a URL as its origin for logs, messages, and details.
//
// It never falls back to the raw string. A file:// path and a data: URL are
// exactly the tabs the policy covers least, and a refused URL's query string is
// exactly where a session token lives; either one returned unchanged would land
// in the append-only audit log, which RFC-0012 requires to record no values at
// all. policy.Label is the single implementation of that rule.
func originOf(rawURL string) string { return policy.Label(rawURL) }

// auditRecord is one NDJSON line of the audit log.
//
// The field set is the whole point: timestamp, origin, verb, decision, rule —
// and nothing else. No typed text, no cookie value, no selector, and the ORIGIN
// rather than the URL, because a URL's query string carries values too. An
// audit log that recorded what was typed would be the most sensitive file this
// tool produces, which is exactly the trade a security feature must not make.
type auditRecord struct {
	TS       string `json:"ts"`
	Origin   string `json:"origin"`
	Verb     string `json:"verb"`
	Decision string `json:"decision"`
	Rule     string `json:"rule,omitempty"`
}

// auditAppend appends one record, creating the log and its directory if needed.
//
// A log we cannot write is a warning, not a refusal: the command the user asked
// for has already been decided, and failing it here would turn a full disk into
// an outage.
func (a *App) auditAppend(path string, rec auditRecord) {
	if path == "" {
		return
	}
	rec.TS = time.Now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(a.err, "warning: cannot write the policy audit log %s: %v\n", path, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(a.err, "warning: cannot write the policy audit log %s: %v\n", path, err)
	}
}

// interactive reports whether there is a human to ask.
//
// Both ends have to be a terminal: stdin because the answer comes from there,
// and stderr because that is where the question goes. A daemonised or piped run
// fails this and is refused outright rather than blocking.
func (a *App) interactive() bool {
	if a.policyTTY != nil {
		return a.policyTTY()
	}
	return !a.noInput && isTerminal(a.in) && isTerminal(a.err)
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// askYesNo puts a question to the user; anything but an explicit yes is no.
func (a *App) askYesNo(question string) bool {
	if a.policyAsk != nil {
		return a.policyAsk(question)
	}
	fmt.Fprintf(a.err, "%s [y/N] ", question)
	line, err := bufio.NewReader(a.in).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// expandHome expands a leading ~ so config paths look like config paths.
func expandHome(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p // ~user is not supported; leave it to fail visibly
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
}

func expandPaths(ps []string) []string {
	if len(ps) == 0 {
		return nil
	}
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, expandHome(p))
	}
	return out
}

// cmdPolicy scaffolds a policy from the tab you are on.
//
// RFC-0012 open question 4, resolved yes: the gap between "I should configure
// this" and "I did" is almost entirely friction, and a one-command starting
// point closes most of it.
func (a *App) cmdPolicy() *cobra.Command {
	p := &cobra.Command{Use: "policy", Short: "Bound what the CLI may drive (see docs/cli-reference.md#policy)"}
	p.AddCommand(a.cmdPolicyInit())
	return p
}

func (a *App) cmdPolicyInit() *cobra.Command {
	var out string
	var printOnly, wildcard bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Write a starter [policy] table allow-listing the current tab's origin",
		Long: "Write a starter [policy] table to the config file, allow-listing the origin\n" +
			"of the tab you are on (or --target).\n\n" +
			"The generated policy is deliberately narrow — one origin, error on\n" +
			"violation — because a starting point you widen is safer than one you\n" +
			"forget to narrow. Use --wildcard to allow-list subdomains too, --print to\n" +
			"see the block without writing it, and -o to write somewhere else.\n\n" +
			"This bounds a cooperative caller. It is not a sandbox: anything that can\n" +
			"run chrome-cdp can also edit this file.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			path := out
			if path == "" {
				path = config.Path()
			}
			// Refuse before connecting when there is nowhere to write.
			if path == "" && !printOnly {
				a.emitErr("policy", result.CodeUsage, "cannot determine the config path; pass -o <file> or --print", nil)
				return nil
			}
			ctx, cancel := a.ctx()
			defer cancel()
			tgt, _, rerr := a.resolveTarget(ctx)
			if rerr != nil {
				a.emitErr("policy", rerr.Code, rerr.Message, rerr.Details)
				return nil
			}
			o, err := policy.ParseOrigin(tgt.URL)
			if err != nil {
				a.emitErr("policy", result.CodeUsage,
					fmt.Sprintf("the current tab (%s) has no origin to allow-list — navigate to the site you want to bound first", tgt.URL), nil)
				return nil
			}
			entry := o.String()
			if wildcard {
				entry = "*." + o.Host
			}
			block := policyBlock(entry)
			if printOnly {
				fmt.Fprint(a.out, block)
				a.emitOK("policy", tgt, map[string]any{"allow": entry, "written": false})
				return nil
			}
			if err := appendPolicyBlock(path, block); err != nil {
				a.emitErr("policy", result.CodeGeneric, err.Error(), map[string]any{"config": path})
				return nil
			}
			a.emitOK("policy", tgt, map[string]any{"allow": entry, "config": path, "written": true})
			return nil
		},
	}
	c.Flags().StringVarP(&out, "output", "o", "", "config file to write (default the resolved config path)")
	c.Flags().BoolVar(&printOnly, "print", false, "print the block instead of writing it")
	c.Flags().BoolVar(&wildcard, "wildcard", false, "allow-list *.<domain> instead of the exact host")
	return c
}

// policyBlock is the starter table `policy init` writes.
//
// verbs_denied is ON, not commented out, and that is the one non-obvious line
// here. `eval` and `raw` can navigate the tab themselves — `eval
// "location='https://elsewhere/'"` is an authenticated GET to an origin the
// allow-list would have refused — so destination checking only means anything
// alongside them being denied. A starter policy that allow-listed one origin and
// left the two verbs that walk out of it enabled would read as a boundary and
// not be one. Delete the line if you need them; that is a decision worth making
// deliberately.
func policyBlock(entry string) string {
	return fmt.Sprintf(`
# Written by `+"`chrome-cdp policy init`"+`. Bounds which origins the CLI may act on.
# This bounds a cooperative caller; it is not a sandbox.
[policy]
enabled = true
allow = [%q]
# eval and raw can navigate the tab themselves, so destination checking is only
# meaningful while they are denied. Remove this line only on purpose.
verbs_denied = ["eval", "raw"]
# deny = ["bank.example"]         # always refused, even if allow would permit it;
#                                 # in deny (unlike allow) "*.bank.example" covers
#                                 # bank.example itself too
# read_only = ["*.wikipedia.org"] # reads permitted, actions refused
# upload_roots = ["~/Documents"]  # directories files may be uploaded from
# audit_log = "~/.local/state/chrome-cdp/audit.log"
on_violation = "error"
`, entry)
}

// appendPolicyBlock adds the block to path, refusing to touch a file that
// already configures a policy — silently rewriting one would be the worst
// possible behaviour for this particular file.
func appendPolicyBlock(path, block string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}
	if strings.Contains(string(existing), "\n[policy]") || strings.HasPrefix(string(existing), "[policy]") {
		return fmt.Errorf("%s already has a [policy] table — edit it by hand rather than having a tool overwrite your policy", path)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cannot create %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(block); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	return nil
}
