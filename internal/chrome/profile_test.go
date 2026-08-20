package chrome

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQueryOptions(t *testing.T) {
	// Every By/Wait branch yields exactly one selector-syntax + one node-state option.
	for _, by := range []string{"", "css", "id", "search", "jspath", "css-all"} {
		for _, wait := range []string{"", "visible", "ready", "enabled"} {
			if got := queryOptions(QueryOpts{By: by, Wait: wait}); len(got) != 2 {
				t.Errorf("queryOptions(by=%q, wait=%q) returned %d options, want 2", by, wait, len(got))
			}
		}
	}
}

func TestResolveProfileDir(t *testing.T) {
	t.Setenv("CHROME_CDP_PROFILE", "")
	t.Setenv("XDG_CACHE_HOME", "")

	// explicit flag wins
	if got := resolveProfileDir("/explicit/dir"); got != "/explicit/dir" {
		t.Errorf("explicit: got %q", got)
	}

	// env fallback
	t.Setenv("CHROME_CDP_PROFILE", "/env/profile")
	if got := resolveProfileDir(""); got != "/env/profile" {
		t.Errorf("env: got %q", got)
	}

	// default under a cache base. XDG_CACHE_HOME is set to a forward-slash
	// literal (the env var's own value, unrelated to the host OS), but
	// resolveProfileDir joins it with filepath.Join, which renders backslashes
	// on windows — so the expectation is built the same way, not compared
	// against the literal.
	t.Setenv("CHROME_CDP_PROFILE", "")
	t.Setenv("XDG_CACHE_HOME", "/tmp/cache")
	wantDefault := filepath.Join("/tmp/cache", "chrome-cdp", "profile")
	if got := resolveProfileDir(""); got != wantDefault {
		t.Errorf("default: got %q, want %q", got, wantDefault)
	}

	// default via home when XDG unset ends at the documented path
	t.Setenv("XDG_CACHE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	wantHomeDefault := filepath.Join(home, ".cache", "chrome-cdp", "profile")
	if got := resolveProfileDir(""); got != wantHomeDefault {
		t.Errorf("home default: got %q, want %q", got, wantHomeDefault)
	}
}
