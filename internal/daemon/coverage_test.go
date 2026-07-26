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

	iface := reflect.TypeOf((*chrome.Browser)(nil)).Elem()
	// A permissive stub, so a routed method runs harmlessly; the arg decoders
	// yield zero values for the nil arg list.
	s := &server{b: chrometest.StubBrowser{}}

	for i := 0; i < iface.NumMethod(); i++ {
		name := iface.Method(i).Name
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
