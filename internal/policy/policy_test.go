package policy

import (
	"strings"
	"testing"
)

// mustChecker builds a Checker from an already-enabled table.
func mustChecker(t *testing.T, cfg Config) *Checker {
	t.Helper()
	cfg.Present, cfg.Enabled = true, true
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	return c
}

// TestWildcardSuffixConfusion is the first test in this file on purpose.
//
// Every one of these rows is a way a naive matcher goes wrong: strings.Contains
// on the raw URL matches all of them, strings.HasSuffix without the dot matches
// "notexample.com", and matching the raw URL rather than the parsed host matches
// "https://evil.io/?x=a.example.com". A policy that says yes to any of these is
// worse than no policy at all, because the user believes it said no.
func TestWildcardSuffixConfusion(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		url  string
		want bool
	}{
		"one subdomain label":         {"https://a.example.com/", true},
		"several subdomain labels":    {"https://a.b.example.com/", true},
		"the apex needs its own rule": {"https://example.com/", false},
		"prefix-glued host":           {"https://notexample.com/", false},
		"suffix-glued host":           {"https://example.com.evil.io/", false},
		"host in the path":            {"https://evil.io/a.example.com", false},
		"host in the query":           {"https://evil.io/?next=a.example.com", false},
		"host in userinfo":            {"https://a.example.com@evil.io/", false},
		"host in the fragment":        {"https://evil.io/#a.example.com", false},
		"leading-dot host":            {"https://.example.com/", false},
		"hyphen-glued host":           {"https://a-example.com/", false},
		"apex with a root dot":        {"https://example.com./", false},
		"subdomain with a root dot":   {"https://a.example.com./", true},
		"uppercase host":              {"https://A.EXAMPLE.COM/", true},
	}
	c := mustChecker(t, Config{Allow: []string{"*.example.com"}})
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := c.Check(tc.url, "click")
			if got.Allowed != tc.want {
				t.Errorf("Check(%q, click).Allowed = %v, want %v (rule %q)", tc.url, got.Allowed, tc.want, got.Rule)
			}
		})
	}
}

// TestPatternMatchTable is RFC-0012 VS-3: the exhaustive pattern/host table.
func TestPatternMatchTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		url     string
		want    bool
	}{
		// Subdomain wildcard.
		{"*.example.com", "https://a.example.com/", true},
		{"*.example.com", "https://a.b.example.com/", true},
		{"*.example.com", "https://example.com/", false},
		{"*.example.com", "https://notexample.com/", false},
		{"*.example.com", "https://example.com.evil.io/", false},
		{"*.example.com", "https://EXAMPLE.COM/", false},
		{"*.example.com", "https://A.EXAMPLE.COM/", true},
		// Exact host.
		{"example.com", "https://example.com/", true},
		{"example.com", "https://a.example.com/", false},
		{"example.com", "http://example.com:8080/x", true},
		{"EXAMPLE.com", "https://example.com/", true},
		// Ports.
		{"localhost:*", "http://localhost:3000/", true},
		{"localhost:*", "http://localhost/", true},
		{"localhost:3000", "http://localhost:3000/", true},
		{"localhost:3000", "http://localhost:8080/", false},
		{"localhost", "http://localhost:3000/", true},
		{"example.com:443", "https://example.com/", true},
		{"example.com:80", "https://example.com/", false},
		// Schemes.
		{"https://x.test", "https://x.test/", true},
		{"https://x.test", "http://x.test/", false},
		{"http://x.test", "http://x.test/", true},
		{"x.test", "http://x.test/", true},
		{"https://*.x.test", "https://a.x.test/", true},
		{"https://*.x.test", "http://a.x.test/", false},
		// Hostless URLs match nothing.
		{"example.com", "about:blank", false},
		{"example.com", "data:text/html,hi", false},
		{"*.example.com", "chrome://settings", false},
		// IP and IPv6 literals.
		{"127.0.0.1", "http://127.0.0.1:9222/json", true},
		{"127.0.0.1", "http://127.0.0.2:9222/json", false},
		{"[::1]:9222", "http://[::1]:9222/json", true},
		{"[::1]:9222", "http://[::1]:9333/json", false},
	}
	for _, tc := range cases {
		name := tc.pattern + " vs " + tc.url
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, err := ParsePattern(tc.pattern)
			if err != nil {
				t.Fatalf("ParsePattern(%q): %v", tc.pattern, err)
			}
			o, err := ParseOrigin(tc.url)
			if err != nil {
				if tc.want {
					t.Fatalf("ParseOrigin(%q): %v", tc.url, err)
				}
				return // a hostless URL matches nothing, which is the expectation
			}
			if got := p.Match(o); got != tc.want {
				t.Errorf("%q.Match(%q) = %v, want %v", tc.pattern, tc.url, got, tc.want)
			}
		})
	}
}

// TestAllowListPermitsAndRefuses is VS-1's pure half.
func TestAllowListPermitsAndRefuses(t *testing.T) {
	t.Parallel()
	c := mustChecker(t, Config{Allow: []string{"*.example.com"}})
	if d := c.Check("https://app.example.com/x", "click"); !d.Allowed {
		t.Errorf("allowed origin refused: %+v", d)
	}
	d := c.Check("https://other.test/x", "click")
	if d.Allowed {
		t.Fatalf("non-allowed origin permitted: %+v", d)
	}
	if d.Rule != "allow: no match" {
		t.Errorf("rule = %q, want %q", d.Rule, "allow: no match")
	}
	if !strings.Contains(d.Reason, "other.test") {
		t.Errorf("reason must name the origin, got %q", d.Reason)
	}
}

// TestDenyBeatsAllow is VS-2.
func TestDenyBeatsAllow(t *testing.T) {
	t.Parallel()
	c := mustChecker(t, Config{
		Allow: []string{"*.example.com"},
		Deny:  []string{"admin.example.com"},
	})
	if d := c.Check("https://app.example.com/", "click"); !d.Allowed {
		t.Errorf("app.example.com should be permitted: %+v", d)
	}
	d := c.Check("https://admin.example.com/users", "click")
	if d.Allowed {
		t.Fatal("deny must beat allow")
	}
	if d.Rule != "deny: admin.example.com" {
		t.Errorf("rule = %q, want the deny entry that decided it", d.Rule)
	}
	// A deny entry also wins when it is the only rule and would otherwise be
	// permitted by the "empty allow means everything" default.
	c2 := mustChecker(t, Config{Deny: []string{"*.bank.example"}})
	if c2.Check("https://anything.test/", "click").Allowed != true {
		t.Error("an unnamed origin should be permitted when only a deny-list is configured")
	}
	if c2.Check("https://login.bank.example/", "click").Allowed {
		t.Error("deny-listed origin permitted")
	}
}

// TestReadOnlyClassification is VS-4.
func TestReadOnlyClassification(t *testing.T) {
	t.Parallel()
	c := mustChecker(t, Config{ReadOnly: []string{"*.wiki.test"}})
	const u = "https://en.wiki.test/page"

	for _, verb := range []string{"snap", "text", "grid", "screenshot", "html", "value", "pdf", "cookie list", "attr get"} {
		t.Run("reading/"+verb, func(t *testing.T) {
			t.Parallel()
			if d := c.Check(u, verb); !d.Allowed {
				t.Errorf("reading verb %q refused on a read-only origin: %+v", verb, d)
			}
		})
	}
	for _, verb := range []string{"click", "fill", "type", "eval", "cookie set", "attr set", "key", "scroll", "raw"} {
		t.Run("mutating/"+verb, func(t *testing.T) {
			t.Parallel()
			d := c.Check(u, verb)
			if d.Allowed {
				t.Fatalf("mutating verb %q permitted on a read-only origin", verb)
			}
			if d.Rule != "read_only: *.wiki.test" {
				t.Errorf("rule = %q, want the read_only entry", d.Rule)
			}
		})
	}
	// read_only constrains only the origins it names.
	if d := c.Check("https://elsewhere.test/", "click"); !d.Allowed {
		t.Errorf("read_only must not restrict an unnamed origin: %+v", d)
	}
}

// TestUnclassifiedVerbIsMutating is VS-6: the fail-closed default.
func TestUnclassifiedVerbIsMutating(t *testing.T) {
	t.Parallel()
	class, known := Classify("verb-that-does-not-exist")
	if known {
		t.Fatal("a synthetic verb must not be in the table")
	}
	if class != Mutating {
		t.Errorf("unclassified verb class = %v, want mutating", class)
	}
	c := mustChecker(t, Config{ReadOnly: []string{"*.wiki.test"}})
	if d := c.Check("https://en.wiki.test/", "verb-that-does-not-exist"); d.Allowed {
		t.Error("an unclassified verb must be treated as mutating under read_only")
	}
}

// TestVerbsDenied is VS-7: denied everywhere, including on allowed origins.
func TestVerbsDenied(t *testing.T) {
	t.Parallel()
	c := mustChecker(t, Config{
		Allow:       []string{"*.example.com"},
		VerbsDenied: []string{"raw"},
	})
	d := c.Check("https://app.example.com/", "raw")
	if d.Allowed {
		t.Fatal("verbs_denied must refuse even on an allowed origin")
	}
	if d.Rule != "verbs_denied: raw" {
		t.Errorf("rule = %q", d.Rule)
	}
	if !c.Check("https://app.example.com/", "click").Allowed {
		t.Error("verbs_denied must not affect other verbs")
	}
	// It also beats an exemption-free read: a denied reading verb stays denied.
	c2 := mustChecker(t, Config{VerbsDenied: []string{"screenshot"}})
	if c2.Check("https://anything.test/", "screenshot").Allowed {
		t.Error("a denied reading verb must still be refused")
	}
}

// TestExemptVerbsAreNotOriginChecked keeps tab lifecycle and meta commands
// usable under a policy that refuses everything else.
func TestExemptVerbsAreNotOriginChecked(t *testing.T) {
	t.Parallel()
	c := mustChecker(t, Config{Allow: []string{"nothing.matches"}})
	for _, verb := range []string{"list", "use", "close", "activate", "version", "exit-codes", "daemon status", "policy init"} {
		if d := c.Check("https://other.test/", verb); !d.Allowed {
			t.Errorf("exempt verb %q refused: %+v", verb, d)
		}
	}
}

// TestInertWhenUnconfigured is VS-10/US-5 at the checker level: with no table,
// nothing is ever refused and Active() is false so callers can skip the layer.
func TestInertWhenUnconfigured(t *testing.T) {
	t.Parallel()
	c, err := New(Config{})
	if err != nil {
		t.Fatalf("empty config: %v", err)
	}
	if c.Active() {
		t.Error("a checker with no [policy] table must be inert")
	}
	for _, verb := range []string{"click", "eval", "raw", "type"} {
		if d := c.Check("https://anything.test/", verb); !d.Allowed {
			t.Errorf("inert checker refused %q: %+v", verb, d)
		}
	}
	// A nil checker behaves the same, so call sites need no nil dance.
	var nilc *Checker
	if nilc.Active() || !nilc.Check("https://x.test/", "click").Allowed {
		t.Error("a nil checker must be inert")
	}
	// enabled = false keeps a policy on file without applying it.
	off, err := New(Config{Present: true, Enabled: false, Allow: []string{"nothing.matches"}})
	if err != nil {
		t.Fatal(err)
	}
	if off.Active() || !off.Check("https://other.test/", "click").Allowed {
		t.Error("enabled = false must make the layer inert")
	}
}

// TestRequireAllowExpressesMCPPosture covers the RFC-0004 hook: the CLI default
// stays unrestricted, but the Checker can already say "an explicit allow-list is
// required" so MCP mode is a config change rather than a redesign.
func TestRequireAllowExpressesMCPPosture(t *testing.T) {
	t.Parallel()
	c := mustChecker(t, Config{RequireAllow: true})
	d := c.Check("https://anything.test/", "click")
	if d.Allowed {
		t.Fatal("RequireAllow with an empty allow-list must refuse")
	}
	if d.Rule != "allow: not configured" {
		t.Errorf("rule = %q", d.Rule)
	}
	if c.OriginAllowed("https://anything.test/") {
		t.Error("OriginAllowed must agree with Check under RequireAllow")
	}
	withList := mustChecker(t, Config{RequireAllow: true, Allow: []string{"app.test"}})
	if !withList.Check("https://app.test/", "click").Allowed {
		t.Error("RequireAllow must still permit a named origin")
	}
}

// TestMalformedPolicyIsFatal is VS-15: a pattern the CLI cannot parse must stop
// it, not warn and continue like the rest of this repo's config handling.
func TestMalformedPolicyIsFatal(t *testing.T) {
	t.Parallel()
	bad := map[string]Config{
		"interior wildcard":     {Allow: []string{"a.*.com"}},
		"prefix wildcard":       {Allow: []string{"*example.com"}},
		"bare star":             {Allow: []string{"*"}},
		"url with a path":       {Allow: []string{"https://example.com/admin"}},
		"regex":                 {Allow: []string{`^.*\.example\.com$`}},
		"empty entry":           {Allow: []string{""}},
		"whitespace entry":      {Allow: []string{"  "}},
		"empty port":            {Allow: []string{"example.com:"}},
		"non-numeric port":      {Allow: []string{"example.com:http"}},
		"port out of range":     {Allow: []string{"example.com:70000"}},
		"double dot":            {Allow: []string{"a..com"}},
		"leading hyphen label":  {Allow: []string{"-a.com"}},
		"space in host":         {Allow: []string{"a b.com"}},
		"userinfo":              {Allow: []string{"user@example.com"}},
		"bad scheme":            {Allow: []string{"ht tp://example.com"}},
		"bad deny pattern":      {Deny: []string{"*bank.example"}},
		"bad read_only pattern": {ReadOnly: []string{"*"}},
		"unknown denied verb":   {VerbsDenied: []string{"evel"}},
		"empty denied verb":     {VerbsDenied: []string{""}},
		"empty upload root":     {UploadRoots: []string{""}},
		"bad on_violation":      {OnViolation: "ask"},
	}
	for name, cfg := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg.Present, cfg.Enabled = true, true
			c, err := New(cfg)
			if err == nil {
				t.Fatalf("New(%+v) accepted a malformed policy; a policy that fails open is worse than none", cfg)
			}
			if c != nil {
				t.Error("a rejected policy must not yield a usable Checker")
			}
			if !strings.HasPrefix(err.Error(), "policy: ") {
				t.Errorf("error %q should be attributable to the policy table", err)
			}
		})
	}
}

// TestPatternsThatMustParse guards against over-strict validation rejecting
// patterns the RFC promises.
func TestPatternsThatMustParse(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"example.com", "*.example.com", "intranet.corp.local", "localhost",
		"localhost:*", "localhost:3000", "https://x.test", "http://*.x.test:8080",
		"127.0.0.1", "[::1]", "[::1]:9222", "my_host.local", "a-b.example.com",
		"*.example.com.", "  example.com  ",
	} {
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			if _, err := ParsePattern(s); err != nil {
				t.Errorf("ParsePattern(%q) rejected a documented pattern: %v", s, err)
			}
		})
	}
}

// TestOnViolationDefaultsToError keeps the safe mode the default when the key
// is omitted.
func TestOnViolationDefaultsToError(t *testing.T) {
	t.Parallel()
	c := mustChecker(t, Config{Allow: []string{"a.test"}})
	if c.OnViolation() != OnError {
		t.Errorf("OnViolation() = %q, want %q", c.OnViolation(), OnError)
	}
	p := mustChecker(t, Config{Allow: []string{"a.test"}, OnViolation: OnPrompt})
	if p.OnViolation() != OnPrompt {
		t.Errorf("OnViolation() = %q, want %q", p.OnViolation(), OnPrompt)
	}
}

// TestUploadRootsArePassedThrough: the roots are policy config, but the path
// comparison is `upload`'s, where the symlink resolution it needs can happen.
// All this package owes that caller is the configured list, unmodified.
func TestUploadRootsArePassedThrough(t *testing.T) {
	t.Parallel()
	c := mustChecker(t, Config{UploadRoots: []string{"/home/u/receipts", "/tmp/uploads"}})
	got := c.UploadRoots()
	if len(got) != 2 || got[0] != "/home/u/receipts" || got[1] != "/tmp/uploads" {
		t.Fatalf("UploadRoots() = %v", got)
	}
	got[0] = "/etc"
	if c.UploadRoots()[0] != "/home/u/receipts" {
		t.Error("UploadRoots() must return a copy")
	}
	// An inert checker has no roots, so upload stays unrestricted.
	var nilc *Checker
	if nilc.UploadRoots() != nil {
		t.Error("a nil checker must report no roots")
	}
}

// TestOriginAllowedMatchesCheck keeps the `list` redaction predicate honest: it
// must agree with Check for a reading verb, or redaction and refusal disagree.
func TestOriginAllowedMatchesCheck(t *testing.T) {
	t.Parallel()
	c := mustChecker(t, Config{Allow: []string{"*.example.com"}, Deny: []string{"admin.example.com"}})
	for _, u := range []string{
		"https://a.example.com/", "https://admin.example.com/", "https://other.test/",
		"about:blank", "https://example.com/",
	} {
		want := c.Check(u, "snap").Allowed
		if got := c.OriginAllowed(u); got != want {
			t.Errorf("OriginAllowed(%q) = %v, but Check(...) = %v", u, got, want)
		}
	}
}

// TestVerbsTableIsACopy stops a caller from mutating the classification table
// through the accessor the coverage guard uses.
func TestVerbsTableIsACopy(t *testing.T) {
	t.Parallel()
	v := Verbs()
	v["click"] = Exempt
	if class, _ := Classify("click"); class != Mutating {
		t.Fatal("Verbs() must return a copy; the classification table is the mechanism")
	}
}

// FuzzWildcardNeverOverMatches is the property a naive strings.Contains or
// bare HasSuffix implementation fails immediately: a host is permitted by
// "*.example.com" only when it genuinely ends in a "." + the suffix and has at
// least one label of its own.
func FuzzWildcardNeverOverMatches(f *testing.F) {
	for _, s := range []string{
		"a.example.com", "example.com", "notexample.com", "example.com.evil.io",
		"", ".", "..", ".example.com", "a.b.example.com", "EXAMPLE.com",
		"example.com:8080", "a.example.com.evil.io", "xn--80ak6aa92e.com",
	} {
		f.Add(s)
	}
	p, err := ParsePattern("*.example.com")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, host string) {
		o, err := ParseOrigin("https://" + host + "/")
		if err != nil {
			return
		}
		got := p.Match(o)
		want := strings.HasSuffix(o.Host, ".example.com") && len(o.Host) > len(".example.com")
		if got != want {
			t.Fatalf("Match(%q) = %v, want %v (host parsed as %q)", host, got, want, o.Host)
		}
		if got && o.Host == "example.com" {
			t.Fatalf("the apex must never match a *. wildcard")
		}
	})
}

// FuzzCheckNeverPermitsAnUnlistedHost is the same property one level up, at the
// Check boundary the CLI actually calls.
func FuzzCheckNeverPermitsAnUnlistedHost(f *testing.F) {
	for _, s := range []string{
		"https://a.example.com/", "https://evil.io/?x=a.example.com", "about:blank",
		"https://a.example.com@evil.io/", "https://example.com/", "data:text/html,x",
		"http://a.example.com:8080/", "https://a.example.com./",
	} {
		f.Add(s)
	}
	c, err := New(Config{Present: true, Enabled: true, Allow: []string{"*.example.com"}})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, rawURL string) {
		if !c.Check(rawURL, "click").Allowed {
			return
		}
		o, err := ParseOrigin(rawURL)
		if err != nil {
			t.Fatalf("Check permitted %q, whose origin does not even parse", rawURL)
		}
		if !strings.HasSuffix(o.Host, ".example.com") {
			t.Fatalf("Check permitted host %q from %q, which is not under example.com", o.Host, rawURL)
		}
	})
}
