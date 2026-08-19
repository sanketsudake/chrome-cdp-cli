package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
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
	// filepath.Join normalizes separators for the host OS, so the expectation
	// is built the same way rather than hardcoded with forward slashes — a
	// literal "/x/cfg/chrome-cdp/config.toml" is wrong on windows, where Join
	// renders backslashes.
	want := filepath.Join("/x/cfg", "chrome-cdp", "config.toml")
	if got != want {
		t.Errorf("XDG path = %q, want %q", got, want)
	}
}

// TestUnreadableConfigIsARefusedPolicy is the fail-open that mattered most.
//
// A config file that does not PARSE while mentioning [policy] refuses to run
// (VS-15). A config file that cannot be READ at all — wrong permissions, a bad
// mount, an I/O error — used to return the error and leave Policy at its zero
// value, which short-circuits every check: the CLI printed "ignoring config" and
// ran unbounded. Same situation, opposite answer. This asserts they now match.
func TestUnreadableConfigIsARefusedPolicy(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("os.Chmod does not restrict read access on Windows, so the permission bit proves nothing")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 file, so the permission bit proves nothing")
	}
	p := writeConfig(t, "[policy]\nallow = [\"*.example.com\"]\n")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

	d, err := ResolveFrom(p, noEnv)
	if err == nil {
		t.Fatal("an unreadable config file must still surface an error")
	}
	if !d.Policy.Present || !d.Policy.Enabled {
		t.Fatalf("a policy we could not read must be Present so the CLI refuses, got %+v", d.Policy)
	}
	if d.Policy.Malformed == "" {
		t.Fatalf("a policy we could not read must be Malformed, not silently absent: %+v", d.Policy)
	}
	if !strings.Contains(d.Policy.Malformed, "could not be read") {
		t.Errorf("Malformed = %q, should say the file could not be read", d.Policy.Malformed)
	}
	if d.Policy.Source != p {
		t.Errorf("Source = %q, want the file the user has to fix", d.Policy.Source)
	}
	// The rest of the defaults still work: this is a policy refusal, not a brick.
	if d.Timeout != 30*time.Second {
		t.Errorf("built-in defaults must survive: %+v", d)
	}
}

// TestPolicyTableWithInnerWhitespaceIsDetected is L1: TOML permits `[ policy ]`,
// and a scan that only knew `[policy]` skipped the fatal-refusal path for a file
// spelled that way — a fail-open reachable by typing one space.
func TestPolicyTableWithInnerWhitespaceIsDetected(t *testing.T) {
	t.Parallel()
	spellings := []string{
		"[policy]",
		"[ policy ]",
		"[  policy]",
		"[policy ]",
		"[ policy.sub ]",
		"[policy.sub]",
	}
	for _, header := range spellings {
		t.Run(header, func(t *testing.T) {
			t.Parallel()
			// A syntax error AFTER the header, so the TOML parse fails and only
			// the text scan decides whether this is fatal.
			p := writeConfig(t, header+"\nallow = [\n")
			d, err := ResolveFrom(p, noEnv)
			if err == nil {
				t.Fatal("the file does not parse; want an error")
			}
			if !d.Policy.Present || d.Policy.Malformed == "" {
				t.Errorf("%s + a syntax error must refuse, not fail open: %+v", header, d.Policy)
			}
		})
	}
	// And the negative: a table that merely starts with "policy" is not one.
	for _, header := range []string{"[policyx]", "[ policyx ]", "# [ policy ]"} {
		t.Run("not a policy table: "+header, func(t *testing.T) {
			t.Parallel()
			p := writeConfig(t, header+"\nallow = [\n")
			d, _ := ResolveFrom(p, noEnv)
			if d.Policy.Present {
				t.Errorf("%s must not count as a [policy] table: %+v", header, d.Policy)
			}
		})
	}
}

// TestNoteWarnsWhenXDGPointsSomewhereWithNoConfig covers the other half of the
// environment problem. No CHROME_CDP_* variable can set a policy key, but
// XDG_CONFIG_HOME decides WHICH file supplies them — so pointing it at a
// directory with no config file turns the policy off, with no stderr line, no
// envelope field, and no exit code to notice it by. It cannot be an error
// (running without a config file is normal), so it is made visible.
func TestNoteWarnsWhenXDGPointsSomewhereWithNoConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env := envFrom(map[string]string{"XDG_CONFIG_HOME": dir})
	missing := pathFrom(env)

	note := noteFrom(missing, env)
	if note == "" {
		t.Fatal("XDG_CONFIG_HOME pointing at a directory with no config file must be reported")
	}
	if !strings.Contains(note, missing) || !strings.Contains(note, "policy") {
		t.Errorf("note = %q, want the path it looked at and what is not in effect", note)
	}

	// A file that IS there says nothing.
	if err := os.MkdirAll(filepath.Dir(missing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(missing, []byte("timeout = \"5s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := noteFrom(missing, env); n != "" {
		t.Errorf("note = %q, want silence when the config file exists", n)
	}
	// And an unset XDG_CONFIG_HOME says nothing either: the default ~/.config
	// path being empty is the normal case for most users, not a warning.
	if n := noteFrom(filepath.Join(t.TempDir(), "absent.toml"), noEnv); n != "" {
		t.Errorf("note = %q, want silence when XDG_CONFIG_HOME is unset", n)
	}
}

// The event-capture bounds are config keys, not flags: they size the buffers
// the connection holder (normally the daemon) retains per tab, which no
// per-command flag can change after the fact.
func TestConsoleBufferPrecedence(t *testing.T) {
	t.Parallel()

	t.Run("built-in when unset", func(t *testing.T) {
		t.Parallel()
		d, err := ResolveFrom(filepath.Join(t.TempDir(), "absent.toml"), noEnv)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if d.ConsoleBuffer != chrome.DefaultConsoleBuffer || d.ConsoleMaxEntry != chrome.DefaultConsoleMaxEntry {
			t.Errorf("defaults = %d/%d, want %d/%d",
				d.ConsoleBuffer, d.ConsoleMaxEntry, chrome.DefaultConsoleBuffer, chrome.DefaultConsoleMaxEntry)
		}
	})

	t.Run("config file overrides the built-in", func(t *testing.T) {
		t.Parallel()
		p := writeConfig(t, "console_buffer = 25\nconsole_max_entry = 512\n")
		d, err := ResolveFrom(p, noEnv)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if d.ConsoleBuffer != 25 || d.ConsoleMaxEntry != 512 {
			t.Errorf("config = %d/%d, want 25/512", d.ConsoleBuffer, d.ConsoleMaxEntry)
		}
	})

	t.Run("env overrides the config file", func(t *testing.T) {
		t.Parallel()
		p := writeConfig(t, "console_buffer = 25\nconsole_max_entry = 512\n")
		env := envFrom(map[string]string{
			"CHROME_CDP_CONSOLE_BUFFER":    "70",
			"CHROME_CDP_CONSOLE_MAX_ENTRY": "4096",
		})
		d, err := ResolveFrom(p, env)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if d.ConsoleBuffer != 70 || d.ConsoleMaxEntry != 4096 {
			t.Errorf("env = %d/%d, want 70/4096", d.ConsoleBuffer, d.ConsoleMaxEntry)
		}
	})

	// Network records get their OWN bounds, not the console's: a request record
	// is an order of magnitude larger than a console line, so one shared size
	// would either starve the request history or blow up the daemon's memory.
	t.Run("net bounds are separate from the console's", func(t *testing.T) {
		t.Parallel()
		d, err := ResolveFrom(filepath.Join(t.TempDir(), "absent.toml"), noEnv)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if d.NetBuffer != chrome.DefaultNetBuffer || d.NetMaxBody != chrome.DefaultNetMaxBody {
			t.Errorf("net defaults = %d/%d, want %d/%d",
				d.NetBuffer, d.NetMaxBody, chrome.DefaultNetBuffer, chrome.DefaultNetMaxBody)
		}

		p := writeConfig(t, "console_buffer = 25\nnet_buffer = 40\nnet_max_body = 2048\n")
		d, err = ResolveFrom(p, noEnv)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if d.NetBuffer != 40 || d.NetMaxBody != 2048 {
			t.Errorf("net config = %d/%d, want 40/2048", d.NetBuffer, d.NetMaxBody)
		}
		if d.ConsoleBuffer != 25 {
			t.Errorf("net_buffer overwrote console_buffer: %d", d.ConsoleBuffer)
		}

		env := envFrom(map[string]string{
			"CHROME_CDP_NET_BUFFER":   "90",
			"CHROME_CDP_NET_MAX_BODY": "4096",
		})
		d, err = ResolveFrom(p, env)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if d.NetBuffer != 90 || d.NetMaxBody != 4096 {
			t.Errorf("net env = %d/%d, want 90/4096", d.NetBuffer, d.NetMaxBody)
		}
		// A typo must not brick the CLI here either.
		d, err = ResolveFrom(p, envFrom(map[string]string{"CHROME_CDP_NET_MAX_BODY": "huge"}))
		if err != nil {
			t.Fatalf("a malformed bound must not be fatal: %v", err)
		}
		if d.NetMaxBody != 2048 {
			t.Errorf("NetMaxBody = %d, want the config value 2048 to survive a malformed env var", d.NetMaxBody)
		}
	})

	// Recording gets its own pair too, and one of them is a BYTE ceiling rather
	// than a count: 600 frames means something entirely different on a laptop
	// viewport and on a 4K one, so a frame count alone does not bound memory.
	t.Run("record bounds are separate and byte-aware", func(t *testing.T) {
		t.Parallel()
		d, err := ResolveFrom(filepath.Join(t.TempDir(), "absent.toml"), noEnv)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if d.RecordBuffer != chrome.DefaultRecordFrames || d.RecordMaxBytes != chrome.DefaultRecordMaxBytes {
			t.Errorf("record defaults = %d/%d, want %d/%d",
				d.RecordBuffer, d.RecordMaxBytes, chrome.DefaultRecordFrames, chrome.DefaultRecordMaxBytes)
		}

		p := writeConfig(t, "console_buffer = 25\nrecord_buffer = 12\nrecord_max_bytes = 4096\n")
		d, err = ResolveFrom(p, noEnv)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if d.RecordBuffer != 12 || d.RecordMaxBytes != 4096 {
			t.Errorf("record config = %d/%d, want 12/4096", d.RecordBuffer, d.RecordMaxBytes)
		}
		if d.ConsoleBuffer != 25 || d.NetBuffer != chrome.DefaultNetBuffer {
			t.Errorf("record_buffer disturbed another bound: console=%d net=%d", d.ConsoleBuffer, d.NetBuffer)
		}

		env := envFrom(map[string]string{
			"CHROME_CDP_RECORD_BUFFER":    "30",
			"CHROME_CDP_RECORD_MAX_BYTES": "8192",
		})
		d, err = ResolveFrom(p, env)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if d.RecordBuffer != 30 || d.RecordMaxBytes != 8192 {
			t.Errorf("record env = %d/%d, want 30/8192", d.RecordBuffer, d.RecordMaxBytes)
		}
		d, err = ResolveFrom(p, envFrom(map[string]string{"CHROME_CDP_RECORD_BUFFER": "many"}))
		if err != nil {
			t.Fatalf("a malformed bound must not be fatal: %v", err)
		}
		if d.RecordBuffer != 12 {
			t.Errorf("RecordBuffer = %d, want the config value 12 to survive a malformed env var", d.RecordBuffer)
		}
	})

	// A typo in a bound must not brick the CLI, matching how every other
	// malformed value here behaves: keep the value below it and carry on.
	t.Run("malformed env keeps the config value", func(t *testing.T) {
		t.Parallel()
		p := writeConfig(t, "console_buffer = 25\n")
		d, err := ResolveFrom(p, envFrom(map[string]string{"CHROME_CDP_CONSOLE_BUFFER": "lots"}))
		if err != nil {
			t.Fatalf("a malformed bound must not be fatal: %v", err)
		}
		if d.ConsoleBuffer != 25 {
			t.Errorf("ConsoleBuffer = %d, want the config value 25 to survive a malformed env var", d.ConsoleBuffer)
		}
	})
}

// TestConsentTimeoutPrecedence pins RFC-0013's new key through the whole
// precedence chain. It matters more than most: the value decides how long a
// daemon holds a connection open waiting for a human, so a config file that is
// silently ignored means the wedge this key exists to prevent.
func TestConsentTimeoutPrecedence(t *testing.T) {
	t.Parallel()

	// Built-in: two minutes, a human timescale for a dialog that can hide behind
	// the window.
	d, _ := ResolveFrom(filepath.Join(t.TempDir(), "absent.toml"), noEnv)
	if d.ConsentTimeout != chrome.DefaultConsentTimeout {
		t.Errorf("built-in consent_timeout = %v, want %v", d.ConsentTimeout, chrome.DefaultConsentTimeout)
	}

	p := writeConfig(t, "consent_timeout = \"45s\"\n")
	d, err := ResolveFrom(p, noEnv)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d.ConsentTimeout != 45*time.Second {
		t.Errorf("config consent_timeout = %v, want 45s", d.ConsentTimeout)
	}

	// Env beats the file.
	d, _ = ResolveFrom(p, envFrom(map[string]string{"CHROME_CDP_CONSENT_TIMEOUT": "3m"}))
	if d.ConsentTimeout != 3*time.Minute {
		t.Errorf("env consent_timeout should win, got %v", d.ConsentTimeout)
	}

	// A malformed value leaves the lower-precedence value in place rather than
	// bricking the connection, matching every other duration key here.
	d, _ = ResolveFrom(p, envFrom(map[string]string{"CHROME_CDP_CONSENT_TIMEOUT": "banana"}))
	if d.ConsentTimeout != 45*time.Second {
		t.Errorf("a malformed env value should leave the file value alone, got %v", d.ConsentTimeout)
	}
	d, _ = ResolveFrom(writeConfig(t, "consent_timeout = \"nope\"\n"), noEnv)
	if d.ConsentTimeout != chrome.DefaultConsentTimeout {
		t.Errorf("a malformed file value should leave the built-in alone, got %v", d.ConsentTimeout)
	}
}

// TestEndpointPrecedence: an explicit endpoint can come from the config file or
// CHROME_CDP_ENDPOINT, and the env wins — the same precedence every other key
// here follows.
func TestEndpointPrecedence(t *testing.T) {
	t.Parallel()

	// Built-in: unset.
	d, _ := ResolveFrom(filepath.Join(t.TempDir(), "absent.toml"), noEnv)
	if d.Endpoint != "" {
		t.Errorf("built-in endpoint = %q, want empty", d.Endpoint)
	}

	p := writeConfig(t, `endpoint = "http://127.0.0.1:9222"`+"\n")
	d, err := ResolveFrom(p, noEnv)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d.Endpoint != "http://127.0.0.1:9222" {
		t.Errorf("config endpoint = %q, want http://127.0.0.1:9222", d.Endpoint)
	}

	// Env beats the file.
	d, _ = ResolveFrom(p, envFrom(map[string]string{"CHROME_CDP_ENDPOINT": "ws://127.0.0.1:9222/devtools/browser/x"}))
	if d.Endpoint != "ws://127.0.0.1:9222/devtools/browser/x" {
		t.Errorf("env endpoint should win, got %q", d.Endpoint)
	}
}

// TestBrowserBinPrecedence: CHROME_CDP_BROWSER_BIN/config browser_bin has no
// --browser-bin flag, but it follows the same file-then-env precedence as
// every other scalar key here. Unlike Endpoint, a bad value has no scheme to
// validate, so there is no malformed-value case.
func TestBrowserBinPrecedence(t *testing.T) {
	t.Parallel()

	// Built-in: unset.
	d, _ := ResolveFrom(filepath.Join(t.TempDir(), "absent.toml"), noEnv)
	if d.BrowserBin != "" {
		t.Errorf("built-in browser_bin = %q, want empty", d.BrowserBin)
	}

	p := writeConfig(t, `browser_bin = "/usr/bin/chromium"`+"\n")
	d, err := ResolveFrom(p, noEnv)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d.BrowserBin != "/usr/bin/chromium" {
		t.Errorf("config browser_bin = %q, want /usr/bin/chromium", d.BrowserBin)
	}

	// Env beats the file.
	d, _ = ResolveFrom(p, envFrom(map[string]string{"CHROME_CDP_BROWSER_BIN": "/opt/brave/brave"}))
	if d.BrowserBin != "/opt/brave/brave" {
		t.Errorf("env browser_bin should win, got %q", d.BrowserBin)
	}
}

// TestMalformedEndpointIsDroppedNotFatal: a bad scheme in config.toml or
// CHROME_CDP_ENDPOINT must not brick the CLI. Endpoint becomes the --endpoint
// flag's DEFAULT, and the CLI validates that flag ahead of every command
// (including ones that never touch Chrome) — so an unvalidated bad value here
// would turn one stray config line into a usage failure across the whole CLI,
// contradicting "a malformed config is a warning on stderr, not a fatal
// error." It is dropped instead, the same way a malformed consent_timeout or
// CHROME_CDP_PORT is silently left at the lower-precedence value rather than
// erroring (see TestConsentTimeoutPrecedence).
func TestMalformedEndpointIsDroppedNotFatal(t *testing.T) {
	t.Parallel()

	p := writeConfig(t, `endpoint = "9222"`+"\n") // no scheme: not ws:// or http://
	d, err := ResolveFrom(p, noEnv)
	if err != nil {
		t.Fatalf("a malformed endpoint value must not fail the whole config: %v", err)
	}
	if d.Endpoint != "" {
		t.Errorf("malformed file endpoint = %q, want dropped (empty)", d.Endpoint)
	}

	// A malformed env value must not clobber a valid file value either.
	valid := writeConfig(t, `endpoint = "http://127.0.0.1:9222"`+"\n")
	d, _ = ResolveFrom(valid, envFrom(map[string]string{"CHROME_CDP_ENDPOINT": "garbage"}))
	if d.Endpoint != "http://127.0.0.1:9222" {
		t.Errorf("a malformed env endpoint should leave the file value alone, got %q", d.Endpoint)
	}

	// A malformed env value with no file underneath leaves the built-in
	// (empty) alone.
	d, _ = ResolveFrom(filepath.Join(t.TempDir(), "absent.toml"), envFrom(map[string]string{"CHROME_CDP_ENDPOINT": "garbage"}))
	if d.Endpoint != "" {
		t.Errorf("malformed env endpoint with no file = %q, want dropped (empty)", d.Endpoint)
	}
}
