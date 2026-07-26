package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// noEnv is a getenv that reports every variable as unset.
func noEnv(string) string { return "" }

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveBuiltinWhenNoFileNoEnv(t *testing.T) {
	t.Parallel()
	d, err := ResolveFrom(filepath.Join(t.TempDir(), "absent.toml"), noEnv)
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if d.Timeout != 30*time.Second || d.By != "css" || d.Wait != "visible" {
		t.Errorf("built-in defaults not applied: %+v", d)
	}
	if d.Port != 0 || d.ProfileDir != "" || d.NoLaunch || d.NoDaemon || d.JSON || d.NoColor {
		t.Errorf("zero-valued defaults expected: %+v", d)
	}
}

func TestConfigFileOverridesBuiltin(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, `
# persistent defaults
timeout = "5s"
by = "search"
wait = "ready"
port = 9333
profile_dir = "/tmp/prof"
no_launch = true
no_daemon = true
json = true
no_color = true
`)
	d, err := ResolveFrom(p, noEnv)
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if d.Timeout != 5*time.Second || d.By != "search" || d.Wait != "ready" {
		t.Errorf("string/duration overrides: %+v", d)
	}
	if d.Port != 9333 || d.ProfileDir != "/tmp/prof" {
		t.Errorf("port/profile overrides: %+v", d)
	}
	if !d.NoLaunch || !d.NoDaemon || !d.JSON || !d.NoColor {
		t.Errorf("bool overrides: %+v", d)
	}
}

func TestConfigTarget(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, "target = \"url:github\"\n")
	d, err := ResolveFrom(p, noEnv)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d.Target != "url:github" {
		t.Errorf("config target = %q, want url:github", d.Target)
	}
	// CHROME_CDP_TARGET overrides the file.
	d, _ = ResolveFrom(p, envFrom(map[string]string{"CHROME_CDP_TARGET": "@2"}))
	if d.Target != "@2" {
		t.Errorf("env target should win, got %q", d.Target)
	}
}

func TestEnvOverridesConfig(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, `
timeout = "5s"
by = "search"
port = 9333
json = true
`)
	env := envFrom(map[string]string{
		"CHROME_CDP_TIMEOUT": "12s",
		"CHROME_CDP_BY":      "id",
		"CHROME_CDP_PORT":    "9444",
		"CHROME_CDP_JSON":    "false",
		"CHROME_CDP_PROFILE": "/env/prof",
	})
	d, err := ResolveFrom(p, env)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d.Timeout != 12*time.Second {
		t.Errorf("env timeout should win: %v", d.Timeout)
	}
	if d.By != "id" {
		t.Errorf("env by should win: %q", d.By)
	}
	if d.Port != 9444 {
		t.Errorf("env port should win: %d", d.Port)
	}
	if d.JSON {
		t.Errorf("env json=false should win over config json=true")
	}
	if d.ProfileDir != "/env/prof" {
		t.Errorf("env profile should apply: %q", d.ProfileDir)
	}
	// wait had no config or env value -> built-in.
	if d.Wait != "visible" {
		t.Errorf("untouched field should stay built-in: %q", d.Wait)
	}
}

func TestMalformedConfigSurfacesErrorButStillUsable(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, "timeout = \"5s\"\nthis is not valid toml =\n")
	env := envFrom(map[string]string{"CHROME_CDP_BY": "id"})
	d, err := ResolveFrom(p, env)
	if err == nil {
		t.Fatal("malformed config should surface an error")
	}
	// Built-ins + env still apply so the CLI can run.
	if d.Timeout != 30*time.Second {
		t.Errorf("built-in timeout expected after parse failure: %v", d.Timeout)
	}
	if d.By != "id" {
		t.Errorf("env should still apply after parse failure: %q", d.By)
	}
}

// TestNoPolicyTableIsInert is US-5 at the config layer: an existing config with
// no [policy] table must not acquire one by accident.
func TestNoPolicyTableIsInert(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, "timeout = \"5s\"\n")
	d, err := ResolveFrom(p, noEnv)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d.Policy.Present {
		t.Errorf("policy present with no [policy] table: %+v", d.Policy)
	}
	if d.Policy.Malformed != "" {
		t.Errorf("policy malformed with no table: %q", d.Policy.Malformed)
	}
}

func TestPolicyTableIsRead(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, `
timeout = "5s"

[policy]
allow = ["*.workday.com", "intranet.corp.local"]
deny = ["*.bank.example"]
read_only = ["*.wikipedia.org"]
verbs_denied = ["raw"]
upload_roots = ["~/Documents/receipts"]
audit_log = "/tmp/audit.log"
audit_all = true
on_violation = "prompt"
`)
	d, err := ResolveFrom(p, noEnv)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	pol := d.Policy
	if !pol.Present || !pol.Enabled {
		t.Fatalf("a present [policy] table defaults to enabled: %+v", pol)
	}
	if len(pol.Allow) != 2 || pol.Allow[0] != "*.workday.com" {
		t.Errorf("allow = %v", pol.Allow)
	}
	if len(pol.Deny) != 1 || len(pol.ReadOnly) != 1 || len(pol.VerbsDenied) != 1 || len(pol.UploadRoots) != 1 {
		t.Errorf("list keys not read: %+v", pol)
	}
	if pol.AuditLog != "/tmp/audit.log" || !pol.AuditAll || pol.OnViolation != "prompt" {
		t.Errorf("scalar keys not read: %+v", pol)
	}
	if pol.Source != p {
		t.Errorf("Source = %q, want the config path %q", pol.Source, p)
	}
	// The rest of the file still applies.
	if d.Timeout != 5*time.Second {
		t.Errorf("timeout = %v", d.Timeout)
	}
}

func TestPolicyEnabledFalseIsHonored(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, "[policy]\nenabled = false\nallow = [\"a.test\"]\n")
	d, _ := ResolveFrom(p, noEnv)
	if !d.Policy.Present || d.Policy.Enabled {
		t.Errorf("enabled = false must be honored: %+v", d.Policy)
	}
}

// TestPolicyIsNotEnvOverridable keeps a safety boundary out of reach of an
// inherited environment.
func TestPolicyIsNotEnvOverridable(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, "[policy]\nallow = [\"a.test\"]\n")
	env := envFrom(map[string]string{
		"CHROME_CDP_POLICY":         "off",
		"CHROME_CDP_POLICY_ENABLED": "false",
		"CHROME_CDP_POLICY_ALLOW":   "*",
	})
	d, _ := ResolveFrom(p, env)
	if !d.Policy.Enabled || len(d.Policy.Allow) != 1 || d.Policy.Allow[0] != "a.test" {
		t.Errorf("env must not be able to widen or disable a policy: %+v", d.Policy)
	}
}

// TestMalformedPolicyIsRecordedNotSwallowed is the config half of VS-15.
//
// The repo's rule for a bad config is "warn and continue on the built-ins".
// Applied to a policy that means running wide open with the user believing they
// are bounded, so both a file that does not parse while mentioning [policy] and
// an unrecognised key inside the table are recorded for the CLI to refuse on.
func TestMalformedPolicyIsRecordedNotSwallowed(t *testing.T) {
	t.Parallel()
	t.Run("unknown key in the table", func(t *testing.T) {
		t.Parallel()
		p := writeConfig(t, "[policy]\nallowed = [\"*.example.com\"]\n")
		d, err := ResolveFrom(p, noEnv)
		if err != nil {
			t.Fatalf("the file parses as TOML: %v", err)
		}
		if d.Policy.Malformed == "" {
			t.Fatal("a typo'd policy key must not be silently ignored")
		}
		if !strings.Contains(d.Policy.Malformed, "allowed") {
			t.Errorf("Malformed = %q, should name the offending key", d.Policy.Malformed)
		}
	})
	t.Run("file does not parse but mentions policy", func(t *testing.T) {
		t.Parallel()
		p := writeConfig(t, "timeout = \"5s\"\n[policy]\nallow = [\n")
		d, err := ResolveFrom(p, noEnv)
		if err == nil {
			t.Fatal("a broken file should still surface a parse error")
		}
		if !d.Policy.Present || d.Policy.Malformed == "" {
			t.Fatalf("a policy we cannot read must be refused, not skipped: %+v", d.Policy)
		}
	})
	t.Run("file does not parse and has no policy", func(t *testing.T) {
		t.Parallel()
		p := writeConfig(t, "timeout = \"5s\"\nthis is not valid toml =\n")
		d, err := ResolveFrom(p, noEnv)
		if err == nil {
			t.Fatal("want a parse error")
		}
		// Unchanged behaviour for everyone who never configured a policy.
		if d.Policy.Present || d.Policy.Malformed != "" {
			t.Errorf("a broken config with no policy must stay a warning: %+v", d.Policy)
		}
	})
	t.Run("commented-out policy table", func(t *testing.T) {
		t.Parallel()
		p := writeConfig(t, "# [policy]\nthis is not valid toml =\n")
		d, _ := ResolveFrom(p, noEnv)
		if d.Policy.Present {
			t.Error("a commented-out [policy] header must not count as a policy")
		}
	})
}

func TestPathHonorsXDG(t *testing.T) {
	t.Parallel()
	got := pathFrom(envFrom(map[string]string{"XDG_CONFIG_HOME": "/x/cfg"}))
	if got != "/x/cfg/chrome-cdp/config.toml" {
		t.Errorf("XDG path = %q", got)
	}
}
