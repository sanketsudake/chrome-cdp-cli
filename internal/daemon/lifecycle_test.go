package daemon

import (
	"errors"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
)

func TestConnectErrSidecarRoundTrip(t *testing.T) {
	t.Parallel()
	// A ConnectError's stable code survives the encode -> .err -> decode trip,
	// so a daemon-path connect failure keeps its specific code.
	orig := &chrome.ConnectError{Code: "not_debug_enabled", Message: "chrome up but not debuggable"}
	got := decodeConnectErr(encodeConnectErr(orig))
	var ce *chrome.ConnectError
	if !errors.As(got, &ce) {
		t.Fatalf("decoded error is not *ConnectError: %v", got)
	}
	if ce.Code != "not_debug_enabled" || ce.Message != orig.Message {
		t.Errorf("round-trip lost fidelity: %+v", ce)
	}

	// A plain (codeless) error decodes to a message-only error, not a coded one.
	plain := decodeConnectErr(encodeConnectErr(errors.New("boom")))
	if errors.As(plain, &ce) {
		t.Errorf("plain error should not decode to a coded ConnectError: %v", plain)
	}
	if plain.Error() != "boom" {
		t.Errorf("plain message = %q, want boom", plain.Error())
	}

	// A legacy, pre-JSON plain-text sidecar still surfaces its text.
	if legacy := decodeConnectErr([]byte("legacy text\n")); legacy.Error() != "legacy text" {
		t.Errorf("legacy sidecar = %q, want 'legacy text'", legacy.Error())
	}
}
