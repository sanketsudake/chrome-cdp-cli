// Package policy bounds what chrome-cdp may drive: which origins it will act
// on, which verbs are permitted there, and which local paths may be uploaded.
//
// The package is deliberately pure — no I/O, no context, no browser — so the
// whole decision table can be exercised exhaustively in tests. Enforcement
// lives at the command boundary in internal/cli, which consults a Checker
// before calling into chrome.Browser; putting it in the driver would mean every
// future Browser method had to remember to check.
//
// Two properties are load-bearing, and any change must preserve both:
//
//   - Matching is on the PARSED URL's host, never on a substring of the raw
//     URL. "*.example.com" matches a.example.com and a.b.example.com but NOT
//     example.com (which needs its own entry), notexample.com, or
//     example.com.evil.io. Those suffix-confusion cases are exactly where a
//     naive implementation is wrong, and they are the first tests in the suite.
//
//   - Every ambiguity fails CLOSED. deny beats allow; an unclassified verb is
//     mutating; an origin that cannot be parsed is refused whenever the checker
//     is active AT ALL — a deny-list can no more prove a URL is not the denied
//     origin than an allow-list can prove it is — and a pattern that does not
//     parse is a fatal config error rather than a rule that silently matches
//     nothing. A policy that fails open is worse than no policy.
package policy

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Class is what a command does to the page, which decides how it is checked.
//
// Mutating is the zero value on purpose: a verb missing from the table is
// treated as mutating, so forgetting to classify a new verb over-restricts
// rather than silently opening a hole (RFC-0012 VS-6).
type Class int

const (
	// Mutating verbs act on the page: checked against allow/deny, read_only,
	// and verbs_denied.
	Mutating Class = iota
	// Reading verbs only observe the page: checked against allow/deny and
	// verbs_denied, but permitted on a read_only origin.
	Reading
	// Exempt commands never touch page content — tab lifecycle, batch, and
	// meta verbs. They are not origin-checked at all.
	Exempt
)

func (c Class) String() string {
	switch c {
	case Reading:
		return "reading"
	case Exempt:
		return "exempt"
	default:
		return "mutating"
	}
}

// verbClass classifies every registered command, keyed by its full command path
// minus the root ("click", "cookie set", "attr get").
//
// This table IS the read-only mechanism, so it has to be exhaustive by
// construction rather than by good intentions: TestEveryCommandIsClassified in
// internal/cli walks the cobra tree and fails when a runnable command is absent
// from it. Add the verb here in the same commit that registers it.
var verbClass = map[string]Class{
	// Tab lifecycle: inert with respect to page content, so not origin-checked
	// (RFC-0012 open question 3 — blocking `use` would produce confusing errors
	// far from the cause, and the check at action time is sufficient).
	"list":     Exempt,
	"use":      Exempt,
	"close":    Exempt,
	"activate": Exempt,

	// Meta and batch. `session` re-enters the command tree per NDJSON line, and
	// each of those lines is checked on its own.
	// recipe run is Exempt for the same reason session is: it touches no tab
	// itself, it re-enters the command tree per step, and EACH of those steps is
	// classified and checked on its own. Classifying the wrapper as Mutating
	// would refuse a recipe made entirely of reads on a read_only origin, while
	// classifying it as Reading would be a lie — so the wrapper abstains and the
	// steps decide. list/show/new never reach a browser at all.
	"recipe list":   Exempt,
	"recipe show":   Exempt,
	"recipe new":    Exempt,
	"recipe run":    Exempt,
	"session":       Exempt,
	"doctor":        Exempt,
	"daemon start":  Exempt,
	"daemon stop":   Exempt,
	"daemon status": Exempt,
	"exit-codes":    Exempt,
	"version":       Exempt,
	// `policy init` writes the policy; it must stay reachable even when the
	// configured policy would refuse everything, or a bad policy traps the user.
	"policy init": Exempt,

	// Reading.
	"snap":  Reading,
	"html":  Reading,
	"text":  Reading,
	"value": Reading,
	"grid":  Reading,
	// The observability verbs read what the page already logged or requested.
	// They are Reading, not Exempt: a console line or a request URL is page
	// content, and a read_only origin still gets to withhold it.
	"console":     Reading,
	"net":         Reading,
	"net wait":    Reading,
	"screenshot":  Reading,
	"pdf":         Reading,
	"frame list":  Reading,
	"wait":        Reading,
	"attr get":    Reading,
	"attr list":   Reading,
	"cookie list": Reading,

	// Mutating.
	"open":             Mutating, // checked against the DESTINATION origin
	"nav":              Mutating, // ditto, when given a url
	"eval":             Mutating,
	"raw":              Mutating,
	"click":            Mutating,
	"hover":            Mutating,
	"dblclick":         Mutating,
	"rclick":           Mutating,
	"drag":             Mutating,
	"key":              Mutating,
	"type":             Mutating,
	"fill":             Mutating,
	"select":           Mutating,
	"scroll":           Mutating,
	"attr set":         Mutating,
	"attr rm":          Mutating,
	"cookie set":       Mutating,
	"cookie rm":        Mutating,
	"cookie clear":     Mutating,
	"headers set":      Mutating,
	"emulate viewport": Mutating,
	"emulate geo":      Mutating,
	"emulate reset":    Mutating,
	"upload":           Mutating, // RFC-0006; classified ahead of the verb landing
}

// Classify returns a verb's class and whether it was declared in the table.
// An undeclared verb is reported as Mutating — the fail-closed default.
func Classify(verb string) (Class, bool) {
	c, ok := verbClass[verb]
	if !ok {
		return Mutating, false
	}
	return c, true
}

// Verbs returns a copy of the classification table, for the coverage guard in
// internal/cli.
func Verbs() map[string]Class {
	out := make(map[string]Class, len(verbClass))
	for k, v := range verbClass {
		out[k] = v
	}
	return out
}

// Config is the parsed [policy] table. It is plain data so internal/config can
// fill it from TOML and the CLI can overlay --allow / --policy-off before
// handing it to New.
type Config struct {
	// Present reports that a [policy] table existed at all. With no table the
	// whole layer is inert and nothing about the CLI changes (US-5).
	Present bool
	// Enabled is the master switch. A present table defaults to enabled; set
	// enabled = false to keep a policy on file without applying it.
	Enabled bool

	Allow       []string
	Deny        []string
	ReadOnly    []string
	VerbsDenied []string
	UploadRoots []string

	AuditLog string
	AuditAll bool
	// OnViolation is "error" (default) or "prompt".
	OnViolation string

	// RequireAllow refuses everything when Allow is empty, instead of treating
	// an empty allow-list as "all except deny". The CLI leaves this false —
	// adding a feature must not change existing behaviour — and MCP mode
	// (RFC-0004) will set it, which is the whole point of expressing it here.
	RequireAllow bool

	// Source is the config file the table came from, echoed in refusals so a
	// user knows which file to edit.
	Source string
}

// Violation modes.
const (
	OnError  = "error"
	OnPrompt = "prompt"
)

// Checker answers policy questions about a parsed configuration. The zero value
// is an inert checker that permits everything; use New to build a real one.
type Checker struct {
	enabled     bool
	allow       []Pattern
	deny        []Pattern
	readOnly    []Pattern
	verbsDenied map[string]bool
	uploadRoots []string

	auditLog     string
	auditAll     bool
	onViolation  string
	requireAllow bool
	source       string
}

// New parses cfg into a Checker.
//
// A parse failure is FATAL and the caller must refuse to run — unlike the rest
// of this repo's config handling, which warns and continues. A malformed
// pattern that was skipped with a warning would be a rule the user believes is
// in force and is not, which is the failure mode this whole feature exists to
// prevent (RFC-0012 VS-15).
func New(cfg Config) (*Checker, error) {
	c := &Checker{
		enabled:      cfg.Present && cfg.Enabled,
		auditLog:     cfg.AuditLog,
		auditAll:     cfg.AuditAll,
		onViolation:  cfg.OnViolation,
		requireAllow: cfg.RequireAllow,
		source:       cfg.Source,
		verbsDenied:  map[string]bool{},
	}
	if c.onViolation == "" {
		c.onViolation = OnError
	}
	if c.onViolation != OnError && c.onViolation != OnPrompt {
		return nil, fmt.Errorf("policy: on_violation = %q is not one of %q, %q", cfg.OnViolation, OnError, OnPrompt)
	}

	var err error
	if c.allow, err = parsePatterns("allow", cfg.Allow); err != nil {
		return nil, err
	}
	if c.deny, err = parsePatterns("deny", cfg.Deny); err != nil {
		return nil, err
	}
	if c.readOnly, err = parsePatterns("read_only", cfg.ReadOnly); err != nil {
		return nil, err
	}

	for _, v := range cfg.VerbsDenied {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, fmt.Errorf("policy: verbs_denied contains an empty entry")
		}
		// A typo here ("evel") would be a rule that never fires — a silent
		// fail-open — so an unknown verb is a config error, not a warning.
		if _, known := Classify(v); !known {
			return nil, fmt.Errorf("policy: verbs_denied names an unknown verb %q (known verbs: %s)", v, strings.Join(KnownVerbs(), ", "))
		}
		c.verbsDenied[v] = true
	}

	for _, r := range cfg.UploadRoots {
		r = strings.TrimSpace(r)
		if r == "" {
			return nil, fmt.Errorf("policy: upload_roots contains an empty entry")
		}
		c.uploadRoots = append(c.uploadRoots, r)
	}
	return c, nil
}

// KnownVerbs returns every classified verb, sorted — used in error messages.
func KnownVerbs() []string {
	out := make([]string, 0, len(verbClass))
	for v := range verbClass {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Active reports whether the checker will refuse anything at all. A caller uses
// it to keep behaviour byte-identical when no policy is configured (US-5), and
// to decide whether `list` should redact non-allowed tabs (VS-17).
func (c *Checker) Active() bool { return c != nil && c.enabled }

// OnViolation reports the configured violation mode ("error" or "prompt").
func (c *Checker) OnViolation() string {
	if c == nil || c.onViolation == "" {
		return OnError
	}
	return c.onViolation
}

// AuditLog returns the configured audit-log path ("" for none).
func (c *Checker) AuditLog() string {
	if c == nil {
		return ""
	}
	return c.auditLog
}

// AuditAll reports whether permitted actions are logged too, not just refusals.
func (c *Checker) AuditAll() bool { return c != nil && c.auditAll }

// Source returns the config file the policy came from ("" when unknown).
func (c *Checker) Source() string {
	if c == nil {
		return ""
	}
	return c.source
}

// Decision is the outcome of a check. Rule names the entry that decided it, in
// the form the user wrote it ("deny: admin.corp.local"), so a refusal points at
// the line to edit rather than at a guess.
type Decision struct {
	Allowed bool
	Rule    string
	Reason  string
}

func allowed(rule string) Decision { return Decision{Allowed: true, Rule: rule} }

// Check decides whether verb may run against the tab (or destination) at
// rawURL.
//
// The order is the fail-closed order and must not be rearranged:
// verbs_denied, then deny, then allow, then read_only.
func (c *Checker) Check(rawURL, verb string) Decision {
	if !c.Active() {
		return allowed("")
	}
	class, _ := Classify(verb)
	if class == Exempt {
		return allowed("exempt: " + verb)
	}

	if c.verbsDenied[verb] {
		return Decision{
			Rule:   "verbs_denied: " + verb,
			Reason: fmt.Sprintf("verb %s is denied everywhere by policy", verb),
		}
	}

	// An origin we cannot pin down (about:blank, a data: URL, a chrome:// page,
	// a nesting scheme whose inner URL is itself hostless) or a command with no
	// origin at all (a browser-level CDP call) is refused whenever the checker is
	// active, and NOT only under an allow-list.
	//
	// "Matches nothing" is the safe outcome under an allow-list and the unsafe
	// one under a deny-list, so a checker that returned it either way would hand
	// every deny-list user a bypass: view-source:https://bank.example/x names the
	// denied origin in plain sight and parses to no host at all. A deny-list can
	// no more prove a URL is not the denied origin than an allow-list can prove
	// it is one, and a browser-level call in particular reaches every tab at once.
	o, err := ParseOrigin(rawURL)
	if err != nil {
		reason := fmt.Sprintf("cannot determine an origin for %s, and a policy cannot decide about an origin it cannot identify", Label(rawURL))
		if strings.TrimSpace(rawURL) == "" {
			reason = fmt.Sprintf("%s acts at the browser level rather than on one origin, which no policy rule can name", verb)
		}
		return Decision{Rule: "origin: unresolvable", Reason: reason}
	}

	if p, ok := matchAny(c.deny, o); ok {
		return Decision{
			Rule:   "deny: " + p.Raw,
			Reason: fmt.Sprintf("origin %s is denied by policy", o),
		}
	}

	switch {
	case len(c.allow) > 0:
		if _, ok := matchAny(c.allow, o); !ok {
			return Decision{
				Rule:   "allow: no match",
				Reason: fmt.Sprintf("origin %s is not permitted by policy", o),
			}
		}
	case c.requireAllow:
		return Decision{
			Rule:   "allow: not configured",
			Reason: "no policy allow-list is configured, and this mode requires an explicit one",
		}
	}

	if class == Mutating {
		if p, ok := matchAny(c.readOnly, o); ok {
			return Decision{
				Rule:   "read_only: " + p.Raw,
				Reason: fmt.Sprintf("origin %s is read-only by policy; %s would modify it", o, verb),
			}
		}
	}
	return allowed("")
}

// OriginAllowed reports whether an origin passes the allow/deny rules alone,
// ignoring the verb. `list` uses it to redact tabs the policy does not cover.
//
// It agrees with Check on the fail-closed reading: a URL with no identifiable
// origin is not allowed under an active policy, whatever the shape of the rules.
// A file:// or data: tab is exactly the tab the policy covers least, so it is
// the last one whose full URL and title should be handed back.
func (c *Checker) OriginAllowed(rawURL string) bool {
	if !c.Active() {
		return true
	}
	o, err := ParseOrigin(rawURL)
	if err != nil {
		return false
	}
	if _, denied := matchAny(c.deny, o); denied {
		return false
	}
	if len(c.allow) > 0 {
		_, ok := matchAny(c.allow, o)
		return ok
	}
	return !c.requireAllow
}

// UploadRoots returns the configured upload roots, as written (RFC-0006).
//
// This package deliberately stops here and does NOT decide whether a path is
// under a root. The only honest root comparison resolves symlinks on both
// sides — on macOS /tmp and /var are themselves symlinks, so a configured
// "/tmp/receipts" and the real path of a file inside it are different strings —
// and resolving symlinks is I/O, which does not belong in a pure decision
// package. `upload` does that comparison where the I/O already is. Two
// implementations of one security rule would be worse than none.
func (c *Checker) UploadRoots() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.uploadRoots...)
}

// Pattern is one parsed origin pattern: an exact host, or a "*." subdomain
// wildcard, each with an optional scheme and an optional port.
//
// Deliberately not a regex. A policy language that is hard to read is a policy
// that is wrong without anyone noticing.
type Pattern struct {
	Raw      string // as written, for the rule string in a refusal
	Scheme   string // "" matches any scheme
	Host     string // for a wildcard, the suffix WITHOUT the leading "*."
	Port     string // "" or "*" matches any port; otherwise canonical digits
	Wildcard bool

	// MatchApex makes a "*.host" wildcard cover the apex `host` as well.
	//
	// It is set for `deny` entries and only for them, because the same exclusion
	// is right in one list and a trap in the other. In `allow`, excluding the
	// apex is the strict reading a boundary needs: `*.example.com` must not
	// quietly widen to example.com itself. In `deny` it is a hole — a user who
	// writes deny = ["*.bank.example"] means "not my bank", and reading that as
	// "every subdomain of my bank, but the bank itself is fine" protects them
	// everywhere except the one host they were thinking of. Over-blocking is the
	// safe direction in a deny-list, so the wildcard is apex-inclusive there.
	MatchApex bool
}

// parsePatterns parses one configured list. `deny` gets apex-inclusive
// wildcards (see Pattern.MatchApex); allow and read_only keep the strict
// reading, where "*.host" is a statement about subdomains only.
func parsePatterns(field string, raw []string) ([]Pattern, error) {
	out := make([]Pattern, 0, len(raw))
	for _, s := range raw {
		p, err := ParsePattern(s)
		if err != nil {
			return nil, fmt.Errorf("policy: %s: %w", field, err)
		}
		p.MatchApex = field == "deny"
		out = append(out, p)
	}
	return out, nil
}

// ParsePattern parses one pattern of the form [scheme://]host[:port], where
// host is either an exact host or "*." followed by a suffix.
//
// It is strict on purpose: anything it cannot represent exactly is an error, so
// a user never ends up with a rule that matches something other than what they
// wrote. In particular a bare "*", an interior "*", a path, and a userinfo or
// query fragment are all rejected rather than being interpreted generously.
func ParsePattern(s string) (Pattern, error) {
	p := Pattern{Raw: strings.TrimSpace(s)}
	rest := p.Raw
	if rest == "" {
		return Pattern{}, fmt.Errorf("empty pattern")
	}
	if i := strings.Index(rest, "://"); i >= 0 {
		scheme := strings.ToLower(rest[:i])
		if !validScheme(scheme) {
			return Pattern{}, fmt.Errorf("invalid pattern %q: %q is not a valid scheme", s, rest[:i])
		}
		p.Scheme = scheme
		rest = rest[i+3:]
	}
	if rest == "" {
		return Pattern{}, fmt.Errorf("invalid pattern %q: no host", s)
	}
	// Everything after the scheme must be host[:port] — a path, query, or
	// userinfo means the user wrote a URL, and guessing which part they meant
	// is how a policy ends up matching something other than what it says.
	if strings.ContainsAny(rest, " \t/?#@\\") {
		return Pattern{}, fmt.Errorf("invalid pattern %q: patterns match origins, not URLs — write host, *.host, or scheme://host:port", s)
	}

	// Bracketed IPv6 literal, optionally followed by :port.
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return Pattern{}, fmt.Errorf("invalid pattern %q: unclosed [ in IPv6 literal", s)
		}
		host := rest[1:end]
		after := rest[end+1:]
		if host == "" {
			return Pattern{}, fmt.Errorf("invalid pattern %q: empty IPv6 literal", s)
		}
		port, err := splitPort(s, after)
		if err != nil {
			return Pattern{}, err
		}
		p.Host, p.Port = strings.ToLower(host), port
		return p, nil
	}

	host := rest
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		port, err := splitPort(s, rest[i:])
		if err != nil {
			return Pattern{}, err
		}
		host, p.Port = rest[:i], port
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	switch {
	case host == "":
		return Pattern{}, fmt.Errorf("invalid pattern %q: no host", s)
	case host == "*":
		return Pattern{}, fmt.Errorf(`invalid pattern %q: a bare "*" is not a pattern — omit the allow list to permit every origin, or name the origins you mean`, s)
	case strings.HasPrefix(host, "*."):
		p.Wildcard = true
		p.Host = host[2:]
		if p.Host == "" || strings.Contains(p.Host, "*") {
			return Pattern{}, fmt.Errorf(`invalid pattern %q: "*." must be followed by a plain host suffix`, s)
		}
	case strings.Contains(host, "*"):
		// "*example.com" and "a.*.com" look like they work and do not; a
		// wildcard is only ever a whole leading label.
		return Pattern{}, fmt.Errorf(`invalid pattern %q: "*" is only allowed as a leading "*." label (e.g. *.example.com)`, s)
	default:
		p.Host = host
	}
	if err := validHost(s, p.Host); err != nil {
		return Pattern{}, err
	}
	return p, nil
}

// splitPort parses a ":port" suffix (including the colon), allowing ":*".
func splitPort(pattern, colonPort string) (string, error) {
	if colonPort == "" {
		return "", nil
	}
	if !strings.HasPrefix(colonPort, ":") {
		return "", fmt.Errorf("invalid pattern %q: trailing %q after the host", pattern, colonPort)
	}
	port := colonPort[1:]
	if port == "*" {
		return port, nil
	}
	if port == "" {
		return "", fmt.Errorf("invalid pattern %q: empty port — write host, host:* or host:8080", pattern)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid pattern %q: %q is not a port number or *", pattern, port)
	}
	// Canonical digits, not the text as written: ports are compared as strings,
	// and a pattern of "host:443" against a URL of "host:0443" would otherwise
	// simply not match (a free bypass of a deny entry). normalizePort does the
	// same on the URL side, so both ends of the comparison are canonical.
	return strconv.Itoa(n), nil
}

// normalizePort strips a port's leading zeros so "0443" and "443" compare
// equal. Anything that is not a plain number is returned untouched, since it
// cannot match a parsed pattern port anyway.
func normalizePort(port string) string {
	if port == "" || port == "*" {
		return port
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 {
		return port
	}
	return strconv.Itoa(n)
}

func validScheme(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9' && i > 0:
		case (ch == '+' || ch == '-' || ch == '.') && i > 0:
		default:
			return false
		}
	}
	return true
}

// validHost rejects anything that is not a plain dotted host or IP literal, so
// a typo becomes a config error rather than a rule that matches nothing.
func validHost(pattern, host string) error {
	if host == "" {
		return fmt.Errorf("invalid pattern %q: no host", pattern)
	}
	if strings.Contains(host, ":") { // an IPv6 literal, already unbracketed
		return nil
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return fmt.Errorf("invalid pattern %q: empty label in host %q", pattern, host)
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("invalid pattern %q: host label %q may not start or end with '-'", pattern, label)
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			ok := ch == '-' || ch == '_' ||
				(ch >= 'a' && ch <= 'z') ||
				(ch >= '0' && ch <= '9')
			if !ok {
				return fmt.Errorf("invalid pattern %q: host %q contains an unsupported character %q", pattern, host, string(ch))
			}
		}
	}
	return nil
}

// Match reports whether o satisfies the pattern.
//
// The wildcard case is the one that matters: HasSuffix alone would let
// "notexample.com" match "*.example.com", so the dot is part of the compared
// suffix and the matched host must be strictly longer than "." + suffix, which
// is what excludes the bare "example.com" and a hostile ".example.com".
func (p Pattern) Match(o Origin) bool {
	if p.Scheme != "" && p.Scheme != o.Scheme {
		return false
	}
	if p.Port != "" && p.Port != "*" && p.Port != o.effectivePort() {
		return false
	}
	if !p.Wildcard {
		return o.Host == p.Host
	}
	if p.MatchApex && o.Host == p.Host {
		return true
	}
	suffix := "." + p.Host
	return len(o.Host) > len(suffix) && strings.HasSuffix(o.Host, suffix)
}

func matchAny(ps []Pattern, o Origin) (Pattern, bool) {
	for _, p := range ps {
		if p.Match(o) {
			return p, true
		}
	}
	return Pattern{}, false
}

// Origin is the scheme/host/port a decision is made about — the parsed form of
// a tab's URL. Nothing in this package ever looks at the raw URL string.
type Origin struct {
	Scheme string // lowercased; "" when the URL carried none
	Host   string // lowercased, with the FQDN root dot stripped
	Port   string // canonical digits ("0443" → "443"); "" when the URL omitted it
}

// defaultPorts lets a pattern that names a port still match a URL that relies
// on the scheme's default, so "x.test:443" matches "https://x.test/".
var defaultPorts = map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443", "ftp": "21"}

func (o Origin) effectivePort() string {
	if o.Port != "" {
		return o.Port
	}
	return defaultPorts[o.Scheme]
}

// String renders the origin the way a refusal message names it: the host, plus
// the port when the URL carried one explicitly.
func (o Origin) String() string {
	if o.Port != "" {
		return o.Host + ":" + o.Port
	}
	return o.Host
}

// ParseOrigin extracts the origin from a URL, as CHROME would resolve it.
//
// The "as Chrome would resolve it" is the whole job. A policy decides about the
// origin the browser will actually act on, so any disagreement between Chrome's
// parse and this one is a bypass, in whichever direction it happens to fall.
// Two disagreements are known and handled here, by canonicalize:
//
//   - Nesting schemes. view-source:, blob:, and filesystem: carry a whole URL
//     in their opaque part, so net/url reports no host for them while Chrome
//     serves the inner origin's authenticated content. They are unwrapped, so
//     the INNER origin is what gets decided about — which cuts both ways:
//     view-source: of an allowed origin stays allowed.
//
//   - Backslashes in the authority. Chrome normalises `\` to `/` there, so
//     https://bank.example\@evil.io/ is bank.example with the path /@evil.io/,
//     while net/url rejects it outright as invalid userinfo.
//
// It still fails for anything with no host at all — about:blank, data:, file://,
// javascript:, chrome:// — and the caller treats that failure as a refusal
// whenever a policy is active, because a rule cannot decide about an origin it
// cannot identify.
func ParseOrigin(rawURL string) (Origin, error) {
	s := canonicalize(rawURL)
	if s == "" {
		return Origin{}, fmt.Errorf("empty url")
	}
	u, err := url.Parse(s)
	if err != nil {
		return Origin{}, fmt.Errorf("unparseable url: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return Origin{}, fmt.Errorf("url has no host")
	}
	// "a.example.com." and "a.example.com" are the same name; without stripping
	// the root dot a trailing dot would slip past both allow and deny.
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return Origin{}, fmt.Errorf("url has no host")
	}
	return Origin{Scheme: strings.ToLower(u.Scheme), Host: host, Port: normalizePort(u.Port())}, nil
}

// nestingSchemes carry another URL in their opaque part and serve that inner
// origin's content. Anything added here must be a scheme where the inner
// origin is the one whose data is reachable.
var nestingSchemes = map[string]bool{
	"view-source": true,
	"blob":        true,
	"filesystem":  true,
}

// maxUnwrap bounds the nesting loop. view-source:view-source:… is legal to
// write and pointless to follow forever.
const maxUnwrap = 8

// canonicalize rewrites a URL into the form Chrome resolves: nesting schemes
// unwrapped to their inner URL, and `\` normalised to `/` in the authority.
//
// The two run in one loop because either can hide the other — a
// view-source: of a backslash URL, or the reverse.
func canonicalize(rawURL string) string {
	s := strings.TrimSpace(rawURL)
	for range maxUnwrap {
		s = normalizeSlashes(s)
		i := strings.Index(s, ":")
		if i <= 0 || !nestingSchemes[strings.ToLower(s[:i])] {
			return s
		}
		s = strings.TrimSpace(s[i+1:])
	}
	return s
}

// normalizeSlashes converts `\` to `/` after the scheme of a URL that has an
// authority, matching Chrome's URL parser.
//
// Replacing every backslash in the remainder — not just the ones in the
// authority — is deliberate and host-equivalent: the authority ends at the
// first slash of either kind, so the ones past it only ever touch the path,
// which no policy rule looks at.
func normalizeSlashes(s string) string {
	i := strings.Index(s, ":")
	if i <= 0 || !validScheme(strings.ToLower(s[:i])) {
		return s
	}
	rest := s[i+1:]
	if len(rest) < 2 || !isSlash(rest[0]) || !isSlash(rest[1]) {
		return s
	}
	return s[:i+1] + strings.ReplaceAll(rest, `\`, "/")
}

func isSlash(b byte) bool { return b == '/' || b == '\\' }

// Label renders a URL the way a log record, a refusal message, and a redacted
// tab row all need it: its origin when it has one, and otherwise the scheme
// plus a placeholder.
//
// It never returns the raw URL. That is the point of it: a URL's query string
// carries values — session tokens, ids, search terms — and RFC-0012 forbids the
// audit log from recording values at all. A fallback that returned the string
// unchanged would put exactly the URLs a policy refused, in full, into the
// append-only file the feature exists to make safe.
func Label(rawURL string) string {
	if strings.TrimSpace(rawURL) == "" {
		return "(browser-level)"
	}
	if o, err := ParseOrigin(rawURL); err == nil {
		return o.String()
	}
	if s := schemeOf(rawURL); s != "" {
		return s + ":(unparseable)"
	}
	return "(unparseable)"
}

// schemeOf returns the lowercased scheme of a URL, or "" when it has none.
func schemeOf(rawURL string) string {
	s := strings.TrimSpace(rawURL)
	i := strings.Index(s, ":")
	if i <= 0 {
		return ""
	}
	scheme := strings.ToLower(s[:i])
	if !validScheme(scheme) {
		return ""
	}
	return scheme
}
