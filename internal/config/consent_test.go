package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
)

// The consent budget crosses four layers — flag, config file, env, and the
// daemon's own environment — and every one of them used to be free to read a
// zero or an absurd value differently. chrome.Connect mapped <= 0 to the
// default, daemon.Ensure did not, and main forwarded the env var only when
// > 0: with consent_timeout = "0s" the daemon waited 120s while its client gave
// up at 10s and reported "still waiting ... after 0s", which is the
// orphaned-prompt failure the parameter exists to prevent, restored through the
// zero value.
//
// Resolution is where flag/env/config meet, so it is where the value is made
// sane, once.

func TestResolveClampsTheConsentTimeout(t *testing.T) {
	for _, c := range []struct {
		name string
		file string
		env  string
		want time.Duration
	}{
		{"unset", "", "", chrome.DefaultConsentTimeout},
		{"zero means the default, not no wait", `consent_timeout = "0s"`, "", chrome.DefaultConsentTimeout},
		{"negative means the default", `consent_timeout = "-5s"`, "", chrome.DefaultConsentTimeout},
		{"a sane value survives", `consent_timeout = "45s"`, "", 45 * time.Second},
		{"an inherited year does not hold the spawn lock for a year", "", "8760h", chrome.MaxConsentTimeout},
		{"a sub-second value is raised to the floor", "", "10ms", chrome.MinConsentTimeout},
		{"env zero means the default too", "", "0s", chrome.DefaultConsentTimeout},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if c.file != "" {
				if err := os.WriteFile(path, []byte(c.file+"\n"), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			}
			getenv := func(k string) string {
				if k == "CHROME_CDP_CONSENT_TIMEOUT" {
					return c.env
				}
				return ""
			}
			d, err := ResolveFrom(path, getenv)
			if err != nil {
				t.Fatalf("ResolveFrom: %v", err)
			}
			if d.ConsentTimeout != c.want {
				t.Errorf("ConsentTimeout = %v, want %v", d.ConsentTimeout, c.want)
			}
		})
	}
}

// TestFromEnvClampsTheConsentTimeout: the daemon subprocess resolves its
// options from the environment alone, so it needs the same clamp — otherwise
// the two processes that must agree on this number are the two that disagree.
func TestFromEnvClampsTheConsentTimeout(t *testing.T) {
	t.Setenv("CHROME_CDP_CONSENT_TIMEOUT", "0s")
	if got := FromEnv().ConsentTimeout; got != chrome.DefaultConsentTimeout {
		t.Errorf("ConsentTimeout = %v, want %v", got, chrome.DefaultConsentTimeout)
	}
}
