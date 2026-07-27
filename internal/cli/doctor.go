package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// doctor's own probe timings. They are much shorter than the connect path's
// consent budget on purpose: doctor answers a question, it does not wait out a
// dialog. Five seconds of silence from a loopback endpoint is already conclusive.
const doctorDialTimeout = 2 * time.Second

// doctorProbeWait is a var only so a test can shrink the clock.
var doctorProbeWait = 5 * time.Second

// The three states doctor distinguishes, reported as `state` in the envelope so
// a caller branches on a value rather than on prose.
const (
	stateNoEndpoint     = "no_endpoint"
	stateConsentPending = "consent_pending"
	stateReady          = "ready"
	stateUnverified     = "unverified" // --no-probe: an endpoint exists and nothing was checked
)

// cmdDoctor answers "can I connect?" by actually connecting.
//
// It used to read the DevToolsActivePort file, find one, and report "debug
// endpoint reachable — Path B attach ready" with a ws:// URL, having never
// spoken to Chrome. During the RFC-0013 reproduction it said ready while every
// connection was hanging on an unanswered consent prompt, which sent the
// investigation everywhere except the dialog on screen. A diagnostic that
// reports readiness it did not verify is worse than no diagnostic.
//
// The awkwardness is that verifying costs a connection, and a connection is a
// consent request — so doctor prefers evidence that costs nothing: a live daemon
// is already holding an established CDP connection, which is a STRONGER proof
// than any probe, and asking it touches Chrome not at all.
func (a *App) cmdDoctor() *cobra.Command {
	var noProbe bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check the Chrome connection by probing it, and explain how to fix it",
		Long: "Report one of three states: no_endpoint, consent_pending, or ready.\n\n" +
			"When the background daemon is running, doctor answers through it and opens no\n" +
			"new connection. Otherwise it attempts a WebSocket upgrade against the debug\n" +
			"endpoint — which is itself a connection request, so Chrome may raise its\n" +
			"consent prompt. Pass --no-probe to report only what the port file says.",
		RunE: func(*cobra.Command, []string) error {
			a.runDoctor(noProbe)
			return nil
		},
	}
	c.Flags().BoolVar(&noProbe, "no-probe", false, "don't open a connection; report only what the DevToolsActivePort file says (unverified)")
	return c
}

func (a *App) runDoctor(noProbe bool) {
	// VS-6. A running daemon has already been through the whole ladder and holds
	// the connection; re-probing here would raise a second consent request for an
	// answer we can get for free.
	if via, ok := a.doctorViaDaemon(); ok {
		a.emitOK("doctor", nil, via)
		return
	}

	// --port names a SPECIFIC Chrome, and every other verb resolves it before
	// the port file. doctor read the port file directly and never looked at the
	// flag, so `doctor --port 9333` diagnosed whichever browser the file
	// happened to name and reported that one healthy.
	ep := browser.FindEndpoint("", a.port)
	if ep.Err != nil {
		a.emitErr("doctor", result.CodeConnection,
			"the DevToolsActivePort file is unreadable ("+ep.Err.Error()+") — "+browser.EnableAdvice,
			map[string]any{"state": stateNoEndpoint, "port_file": ep.PortFile})
		return
	}
	if ep.URL == "" {
		a.emitErr("doctor", result.CodeConnection,
			"no debug endpoint found (no DevToolsActivePort file) — "+browser.EnableAdvice,
			map[string]any{"state": stateNoEndpoint})
		return
	}
	base := map[string]any{"endpoint": ep.URL, "via": "probe", "probed": true}
	if ep.PortFile != "" {
		base["port_file"] = ep.PortFile
	}
	if noProbe {
		a.emitOK("doctor", nil, map[string]any{
			"endpoint": ep.URL, "port_file": ep.PortFile, "via": "port-file", "probed": false, "state": stateUnverified,
			"status": "an endpoint was found, but --no-probe means nothing was verified — a stale port file looks exactly like this",
		})
		return
	}

	// Open question 3: doctor may probe without a daemon, but it says so first.
	// The user asked for a diagnosis, not for a connection, and on the
	// chrome://inspect path a connection is what raises the modal prompt.
	if !a.quiet {
		fmt.Fprintln(a.err, "chrome-cdp doctor: no daemon is running, so this opens one connection to Chrome to verify the endpoint; on the chrome://inspect path that can raise Chrome's consent prompt (use --no-probe to skip)")
	}
	// An explicit --port names an HTTP endpoint; the browser-level WebSocket
	// path has to be resolved before anything can be upgraded against it.
	ws, ok := browser.ResolveWSURL(ep.URL, doctorDialTimeout)
	if !ok {
		a.emitErr("doctor", result.CodeConnection,
			"nothing usable answered at "+ep.URL+" (stale port file, or another process on that port) — "+browser.EnableAdvice,
			base)
		return
	}
	base["ws"] = ws
	switch browser.ProbeWS(ws, doctorDialTimeout, doctorProbeWait) {
	case browser.WSReady:
		base["state"] = stateReady
		base["status"] = "debug endpoint ready — the WebSocket upgrade completed, so an attach will connect"
		a.emitOK("doctor", nil, base)
	case browser.WSPending:
		base["state"] = stateConsentPending
		a.emitErr("doctor", result.CodeConsentPending,
			"the debug endpoint accepted the connection and then went silent — Chrome is holding its \"Allow remote debugging?\" prompt. "+
				"It is browser-modal, can sit BEHIND the Chrome window, and Chrome accepts no other input until it is answered, "+
				"so a browser that looks frozen is usually this dialog and not a crash. Find it and click Allow. "+
				"To stop being asked at all, "+browser.EnableAdvice+".",
			base)
	default:
		base["state"] = stateNoEndpoint
		a.emitErr("doctor", result.CodeConnection,
			"a port file exists but nothing usable answered at "+ws+" (stale file, or another process on that port) — "+browser.EnableAdvice,
			base)
	}
}

// doctorViaDaemon returns the daemon-backed answer when a daemon for this
// endpoint holds a connection it has just PROVED.
//
// The daemon binds its socket only after chrome.Connect succeeded, so liveness
// once looked like evidence — but the socket outlives the connection. Quit
// Chrome and the daemon keeps its listener for the rest of its idle window with
// a dead chromedp connection behind it, so `running: true` is exactly the same
// unverified claim as the DevToolsActivePort file this RFC removed one level
// down. `connected` is the daemon's answer to a round trip it just made to
// Chrome (see the __status dispatch), and nothing short of that earns `ready`:
// anything else falls through to the probe, which asks Chrome itself.
//
// What crosses into the envelope is an ALLOWLIST, not the daemon's map. That
// map used to be copied wholesale, and it carried every open tab's title and
// URL — into `doctor --json`, which the Agent Skill runs as step 1 of every
// session, before any tab has been chosen. The count answers the question a
// diagnostic is asking; the URLs only answer a question nobody asked.
func (a *App) doctorViaDaemon() (map[string]any, bool) {
	if a.noDaemon || a.daemonStatus == nil {
		return nil, false
	}
	st, err := a.daemonStatus(a.connOpts())
	if err != nil {
		return nil, false
	}
	if running, _ := st["running"].(bool); !running {
		return nil, false
	}
	if connected, _ := st["connected"].(bool); !connected {
		return nil, false
	}
	res := map[string]any{
		"state": stateReady, "via": "daemon", "probed": false, "running": true, "connected": true,
		"status": "debug endpoint ready — the running daemon answered a live CDP round trip (no new connection was opened, so no consent prompt was raised)",
	}
	for _, k := range []string{"endpoint", "socket", "target_count"} {
		if v, ok := st[k]; ok {
			res[k] = v
		}
	}
	return res, true
}
