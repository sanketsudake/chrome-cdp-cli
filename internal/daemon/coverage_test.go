package daemon

import (
	"reflect"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrometest"
)

// notDispatched lists Browser methods that deliberately have no daemon dispatch
// case, with the reason. Everything else must be reachable over the RPC.
var notDispatched = map[string]string{
	"Close": "tears down the local connection; the daemon owns its own lifetime",
}

// TestDispatchCoversBrowser fails when a chrome.Browser method has no case in
// the daemon's dispatch switch.
//
// This is the guard for the failure mode that no other test can see: the daemon
// is the DEFAULT connection path, but every stub-backed test injects a Browser
// directly. A new method implemented on *CDP but never wired into remoteBrowser
// + dispatch compiles cleanly, passes the whole suite, and then fails for every
// real user the moment they run the command without --no-daemon.
func TestDispatchCoversBrowser(t *testing.T) {
	t.Parallel()

	// A permissive stub, so a routed method runs harmlessly; the arg decoders
	// yield zero values for the nil arg list.
	s := &server{b: chrometest.StubBrowser{}}

	for m := range reflect.TypeFor[chrome.Browser]().Methods() {
		name := m.Name
		if reason, ok := notDispatched[name]; ok {
			t.Logf("skipping %s: %s", name, reason)
			continue
		}
		if _, err := s.dispatch(t.Context(), name, nil); err != nil && err.Error() == "unknown method: "+name {
			t.Errorf("chrome.Browser method %q has no daemon dispatch case.\n"+
				"Add a remoteBrowser forwarder AND a dispatch case in internal/daemon/daemon.go, "+
				"or record it in notDispatched with a reason.", name)
		}
	}
}

// TestIsStreamMethodCoversEveryStreamingBrowserMethod fails when a streaming
// Browser method is missing from isStreamMethod.
//
// The consequence of missing one is invisible in every stub-backed test and in
// any single-terminal use: the method would take the dispatch mutex for the
// whole life of the caller's --follow window, so every other command against the
// daemon would block behind it. A streaming method is the one whose last
// parameter is the emit callback, which is exactly why it cannot ride the unary
// one-request/one-response protocol.
func TestIsStreamMethodCoversEveryStreamingBrowserMethod(t *testing.T) {
	t.Parallel()
	emit := reflect.TypeOf(func(any) error { return nil })
	for m := range reflect.TypeFor[chrome.Browser]().Methods() {
		last := m.Type.NumIn() - 1
		streams := last >= 0 && m.Type.In(last) == emit
		if got := isStreamMethod(m.Name); got != streams {
			t.Errorf("isStreamMethod(%q) = %v, want %v — it must name exactly the methods "+
				"streamDispatch serves, or a stream holds the dispatch mutex for its whole window", m.Name, got, streams)
		}
	}
}
