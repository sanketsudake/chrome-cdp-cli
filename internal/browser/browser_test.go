package browser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDevToolsActivePort(t *testing.T) {
	port, wsPath, err := ParseDevToolsActivePort([]byte("9222\n/devtools/browser/2d642c44\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if port != 9222 {
		t.Errorf("port = %d, want 9222", port)
	}
	if wsPath != "/devtools/browser/2d642c44" {
		t.Errorf("wsPath = %q", wsPath)
	}
}

func TestParseDevToolsActivePortErrors(t *testing.T) {
	for _, in := range []string{"", "9222", "notaport\n/devtools/browser/x"} {
		if _, _, err := ParseDevToolsActivePort([]byte(in)); err == nil {
			t.Errorf("ParseDevToolsActivePort(%q) = nil error, want error", in)
		}
	}
}

func TestWSURLFromPortFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "DevToolsActivePort")
	if err := os.WriteFile(f, []byte("9222\n/devtools/browser/abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := WSURLFromPortFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ws://127.0.0.1:9222/devtools/browser/abc"; got != want {
		t.Errorf("WSURLFromPortFile = %q, want %q", got, want)
	}
}

func TestWSURLFromPortFileMissing(t *testing.T) {
	if _, err := WSURLFromPortFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("expected error for missing port file")
	}
}

func TestDecideConnection(t *testing.T) {
	cases := []struct {
		name string
		p    Probe
		want Action
	}{
		{"reachable debug endpoint -> attach (Path B)",
			Probe{PortFileWS: "ws://127.0.0.1:9222/x", WSReachable: true}, Attach},
		{"stale port file + chrome running -> instruct toggle",
			Probe{PortFileWS: "ws://127.0.0.1:9222/x", WSReachable: false, ChromeRunning: true}, InstructToggle},
		{"no debug + chrome running -> instruct toggle (don't shadow)",
			Probe{ChromeRunning: true}, InstructToggle},
		{"no debug + no chrome -> launch managed (Path A)",
			Probe{ChromeRunning: false}, Launch},
		{"no debug + no chrome + --no-launch -> instruct launch",
			Probe{ChromeRunning: false, NoLaunch: true}, InstructNoLaunch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DecideConnection(c.p); got != c.want {
				t.Errorf("DecideConnection(%+v) = %v, want %v", c.p, got, c.want)
			}
		})
	}
}

func TestEndpointKey(t *testing.T) {
	// An explicit port yields a distinct, port-specific key (no collisions).
	if k := EndpointKey("", 9333); k != "127.0.0.1:9333" {
		t.Errorf("explicit port key = %q, want 127.0.0.1:9333", k)
	}
	if EndpointKey("", 9333) == EndpointKey("", 9444) {
		t.Error("distinct --port values must not share an endpoint key")
	}

	// With no port and no port file, the key is the stable default.
	if k := EndpointKey("", 0); k != "default" {
		t.Errorf("no port / no file key = %q, want default", k)
	}

	// With a port file and no explicit port, the key comes from the file.
	pf := filepath.Join(t.TempDir(), "DevToolsActivePort")
	if err := os.WriteFile(pf, []byte("9222\n/devtools/browser/abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k := EndpointKey(pf, 0); k != "127.0.0.1:9222" {
		t.Errorf("port-file key = %q, want 127.0.0.1:9222", k)
	}
	// An explicit port still overrides the file.
	if k := EndpointKey(pf, 9333); k != "127.0.0.1:9333" {
		t.Errorf("explicit port should override the file, got %q", k)
	}
}
