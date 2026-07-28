package cli

import "testing"

// A daemon running a different build from the CLI is reported as stale.
//
// The daemon is started on first use and held for the session, so it outlives
// the binary that spawned it. After an upgrade — or a rebuild while working on
// this tool — the OLD process is still serving every command while
// `chrome-cdp version` reports the new one, and because commands forward over
// its socket, a fix that has landed can appear to do nothing. That cost real
// debugging time once; this is the cheap signal that prevents it.
func TestDaemonVersionSkew(t *testing.T) {
	t.Parallel()
	t.Run("mismatch is flagged", func(t *testing.T) {
		t.Parallel()
		got := withDaemonVersionSkew(map[string]any{"running": true, "version": "0.1.0"})
		if got["stale"] != true {
			t.Errorf("stale = %v, want true", got["stale"])
		}
		if got["cli_version"] != Version {
			t.Errorf("cli_version = %v, want %q", got["cli_version"], Version)
		}
		if s, _ := got["status"].(string); s == "" {
			t.Error("skew carries no explanation")
		}
	})
	t.Run("same build is not flagged", func(t *testing.T) {
		t.Parallel()
		got := withDaemonVersionSkew(map[string]any{"running": true, "version": Version})
		if _, ok := got["stale"]; ok {
			t.Error("a matching build must not be reported as stale")
		}
	})
	// A daemon predating this field sends no version. "Unknown" is not "stale",
	// and claiming skew there would be the same unverified assertion this exists
	// to avoid.
	t.Run("absent version is not flagged", func(t *testing.T) {
		t.Parallel()
		got := withDaemonVersionSkew(map[string]any{"running": true})
		if _, ok := got["stale"]; ok {
			t.Error("an unknown daemon version must not be reported as stale")
		}
	})
	t.Run("nil payload survives", func(t *testing.T) {
		t.Parallel()
		if got := withDaemonVersionSkew(nil); got != nil {
			t.Errorf("nil payload became %#v", got)
		}
	})
}
