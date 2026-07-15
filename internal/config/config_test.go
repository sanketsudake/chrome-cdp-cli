package config

import (
	"os"
	"path/filepath"
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

func TestEnvOverridesConfig(t *testing.T) {
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

func TestPathHonorsXDG(t *testing.T) {
	got := pathFrom(envFrom(map[string]string{"XDG_CONFIG_HOME": "/x/cfg"}))
	if got != "/x/cfg/chrome-cdp/config.toml" {
		t.Errorf("XDG path = %q", got)
	}
}
