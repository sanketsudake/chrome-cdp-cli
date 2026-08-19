package browser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDevToolsActivePort(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	for _, in := range []string{"", "9222", "notaport\n/devtools/browser/x"} {
		if _, _, err := ParseDevToolsActivePort([]byte(in)); err == nil {
			t.Errorf("ParseDevToolsActivePort(%q) = nil error, want error", in)
		}
	}
}

func TestWSURLFromPortFile(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	if _, err := WSURLFromPortFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("expected error for missing port file")
	}
}

func TestDecideConnection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    Probe
		want Action
	}{
		{"completed upgrade -> attach (Path B)",
			Probe{Endpoint: "ws://127.0.0.1:9222/x", WS: WSReady}, Attach},
		{"open port, hanging upgrade -> consent pending (NOT a timeout, NOT the toggle)",
			Probe{Endpoint: "ws://127.0.0.1:9222/x", WS: WSPending}, ConsentPending},
		{"open port, hanging upgrade, chrome running -> still consent pending",
			Probe{Endpoint: "ws://127.0.0.1:9222/x", WS: WSPending, ChromeRunning: true}, ConsentPending},
		{"open port, hanging upgrade, --no-launch -> still consent pending (nothing to launch, it is asking)",
			Probe{Endpoint: "ws://127.0.0.1:9222/x", WS: WSPending, NoLaunch: true}, ConsentPending},
		// There is no "pending with no endpoint" case: an upgrade is only ever
		// attempted against an endpoint, so WS being anything but WSRefused
		// already says one was found.
		{"stale port file (refused) + chrome running -> instruct toggle",
			Probe{Endpoint: "ws://127.0.0.1:9222/x", WS: WSRefused, ChromeRunning: true}, InstructToggle},
		{"no debug + chrome running -> instruct toggle (don't shadow)",
			Probe{ChromeRunning: true}, InstructToggle},
		{"no debug + no chrome -> launch managed (Path A)",
			Probe{ChromeRunning: false}, Launch},
		{"no debug + no chrome + --no-launch -> instruct launch",
			Probe{ChromeRunning: false, NoLaunch: true}, InstructNoLaunch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := DecideConnection(c.p); got != c.want {
				t.Errorf("DecideConnection(%+v) = %v, want %v", c.p, got, c.want)
			}
		})
	}
}

func TestEndpointKey(t *testing.T) {
	t.Parallel()
	// An explicit port yields a distinct, port-specific key (no collisions).
	if k := EndpointKey("", "", 9333); k != "127.0.0.1:9333" {
		t.Errorf("explicit port key = %q, want 127.0.0.1:9333", k)
	}
	if EndpointKey("", "", 9333) == EndpointKey("", "", 9444) {
		t.Error("distinct --port values must not share an endpoint key")
	}

	// With no port and no port file, the key is the stable default.
	if k := EndpointKey("", "", 0); k != "default" {
		t.Errorf("no port / no file key = %q, want default", k)
	}

	// With a port file and no explicit port, the key comes from the file.
	pf := filepath.Join(t.TempDir(), "DevToolsActivePort")
	if err := os.WriteFile(pf, []byte("9222\n/devtools/browser/abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k := EndpointKey("", pf, 0); k != "127.0.0.1:9222" {
		t.Errorf("port-file key = %q, want 127.0.0.1:9222", k)
	}
	// An explicit port still overrides the file.
	if k := EndpointKey("", pf, 9333); k != "127.0.0.1:9333" {
		t.Errorf("explicit port should override the file, got %q", k)
	}
}

func TestFindEndpointExplicitWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		explicit string
		port     int
		wantURL  string
		wantErr  bool
	}{
		{"ws verbatim", "ws://127.0.0.1:9222/devtools/browser/abc", 9333, "ws://127.0.0.1:9222/devtools/browser/abc", false},
		{"http kept for /json/version lookup", "http://10.0.0.5:9222", 0, "http://10.0.0.5:9222", false},
		{"garbage is an error, not a fallback", "9222", 0, "", true},
		{"empty explicit falls through to port", "", 9333, "http://127.0.0.1:9333", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep := FindEndpoint(tc.explicit, "", tc.port)
			if (ep.Err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", ep.Err, tc.wantErr)
			}
			if ep.URL != tc.wantURL {
				t.Fatalf("URL = %q, want %q", ep.URL, tc.wantURL)
			}
		})
	}
}

func TestEndpointKeyExplicit(t *testing.T) {
	t.Parallel()
	if got := EndpointKey("ws://10.0.0.5:9222/devtools/browser/x", "", 0); got != "10.0.0.5:9222" {
		t.Fatalf("got %q", got)
	}
	if got := EndpointKey("", "", 9333); got != "127.0.0.1:9333" {
		t.Fatalf("got %q", got)
	}
}
