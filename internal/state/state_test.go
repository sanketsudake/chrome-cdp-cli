package state

import "testing"

// TestValidateSession pins the grammar a --session / CHROME_CDP_SESSION /
// TOML `session` value must satisfy: ^[A-Za-z0-9._-]{1,64}$, with an empty
// string meaning "no session" (valid, the same way ValidateEndpoint treats
// an empty --endpoint as "unset").
func TestValidateSession(t *testing.T) {
	t.Parallel()
	valid := []string{"", "a", "task-1", "agent_2.env", "A", "0", "x-y_z.9"}
	for _, name := range valid {
		if err := ValidateSession(name); err != nil {
			t.Errorf("ValidateSession(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{"a b", "a/b", "a:b", "a!b", "spa ce", "trailing/", "a\tb", "emoji-🙂"}
	for _, name := range invalid {
		if err := ValidateSession(name); err == nil {
			t.Errorf("ValidateSession(%q) = nil, want an error", name)
		}
	}
	// Length bound: 64 is fine, 65 is not.
	sixtyFour := ""
	for i := 0; i < 64; i++ {
		sixtyFour += "a"
	}
	if err := ValidateSession(sixtyFour); err != nil {
		t.Errorf("ValidateSession(64 chars) = %v, want nil", err)
	}
	if err := ValidateSession(sixtyFour + "a"); err == nil {
		t.Error("ValidateSession(65 chars) = nil, want an error")
	}
}

// TestTwoSessionsIndependentCurrentTarget is the core of --session: two Store
// values built from the SAME endpoint key plus a different session suffix
// (the shape cmd/chrome-cdp/main.go's stateFor constructs: EndpointKey(...) +
// "/" + session) must not share a sticky current tab.
//
// sanitize maps every disallowed rune (including the "/" separator) to '-',
// but it does so rune-for-rune — it never drops or merges runes — so for a
// FIXED base key, sanitize(base+"/"+s1) and sanitize(base+"/"+s2) share the
// same prefix up to and including the mapped separator and then diverge
// exactly where s1 and s2 diverge (a valid session name only contains
// characters sanitize already passes through unchanged, since the session
// regex is a subset of sanitize's allowed set). Two different sessions on the
// same endpoint therefore always produce different keys.
func TestTwoSessionsIndependentCurrentTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const base = "127.0.0.1:9222"

	a, err := New(base + "/" + "task-a")
	if err != nil {
		t.Fatalf("New(session a): %v", err)
	}
	b, err := New(base + "/" + "task-b")
	if err != nil {
		t.Fatalf("New(session b): %v", err)
	}

	if err := a.SetCurrentTarget("aa11"); err != nil {
		t.Fatalf("a.SetCurrentTarget: %v", err)
	}
	if err := b.SetCurrentTarget("bb22"); err != nil {
		t.Fatalf("b.SetCurrentTarget: %v", err)
	}

	if got := a.CurrentTarget(); got != "aa11" {
		t.Errorf("session a CurrentTarget = %q, want aa11 (unaffected by session b)", got)
	}
	if got := b.CurrentTarget(); got != "bb22" {
		t.Errorf("session b CurrentTarget = %q, want bb22 (unaffected by session a)", got)
	}

	// Reopening each Store (a fresh process, the normal CLI shape) must see
	// the same independent values.
	aAgain, err := New(base + "/" + "task-a")
	if err != nil {
		t.Fatalf("New (reopen a): %v", err)
	}
	if got := aAgain.CurrentTarget(); got != "aa11" {
		t.Errorf("reopened session a CurrentTarget = %q, want aa11", got)
	}
}

// TestSessionNeverCollidesWithNoSession: a sessioned key and the bare
// endpoint key it was built from must not share a sticky target, because
// sanitize's rune-for-rune mapping never shortens its input — appending a
// non-empty "/"+session always yields a longer (and therefore different)
// sanitized string than the bare key alone.
func TestSessionNeverCollidesWithNoSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const base = "127.0.0.1:9222"

	bare, err := New(base)
	if err != nil {
		t.Fatalf("New(bare): %v", err)
	}
	sessioned, err := New(base + "/" + "task-a")
	if err != nil {
		t.Fatalf("New(sessioned): %v", err)
	}

	if err := bare.SetCurrentTarget("aa11"); err != nil {
		t.Fatalf("bare.SetCurrentTarget: %v", err)
	}
	if got := sessioned.CurrentTarget(); got != "" {
		t.Errorf("a fresh session's CurrentTarget = %q, want empty — it must not inherit the no-session sticky target", got)
	}

	if err := sessioned.SetCurrentTarget("bb22"); err != nil {
		t.Fatalf("sessioned.SetCurrentTarget: %v", err)
	}
	if got := bare.CurrentTarget(); got != "aa11" {
		t.Errorf("no-session CurrentTarget = %q, want aa11 — a session write must not leak back", got)
	}
}

// TestCurrentTargetDoesNotValidateLiveness documents (rather than builds) the
// target_not_found behaviour described in docs/cli-reference.md: the Store
// has no notion of whether the tab it names is still open. It persists and
// reads back whatever spec was written, and it is target.Resolve (in
// internal/target, given a live tab list) that turns a stale id into
// target_not_found — never a silent fallback to some other tab.
func TestCurrentTargetDoesNotValidateLiveness(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s, err := New("127.0.0.1:9222")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.SetCurrentTarget("closed-tab-id"); err != nil {
		t.Fatalf("SetCurrentTarget: %v", err)
	}
	// Nothing about the Store can know this id no longer names an open tab —
	// it is a plain persisted string, echoed back unconditionally.
	if got := s.CurrentTarget(); got != "closed-tab-id" {
		t.Errorf("CurrentTarget = %q, want the stored id echoed back regardless of liveness", got)
	}
}
