package chrome

import (
	"strings"
	"testing"
)

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

	// default under a cache base
	t.Setenv("CHROME_CDP_PROFILE", "")
	t.Setenv("XDG_CACHE_HOME", "/tmp/cache")
	if got := resolveProfileDir(""); got != "/tmp/cache/chrome-cdp/profile" {
		t.Errorf("default: got %q, want /tmp/cache/chrome-cdp/profile", got)
	}

	// default via home when XDG unset ends at the documented path
	t.Setenv("XDG_CACHE_HOME", "")
	if got := resolveProfileDir(""); !strings.HasSuffix(got, "/.cache/chrome-cdp/profile") {
		t.Errorf("home default: got %q", got)
	}
}
