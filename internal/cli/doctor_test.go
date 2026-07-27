package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// RFC-0013 VS-5 / VS-6. `doctor` used to read the DevToolsActivePort file and
// report "Path B attach ready" with a ws:// URL, having never connected. During
// the reproduction it said ready while every connection was hanging on an
// unanswered consent prompt. These tests pin the three states it must now
// distinguish, and the one case where it must NOT connect at all.

// stubEndpoint starts a listener in one of the shapes doctor has to tell apart
// and points CHROME_CDP_PORT_FILE at it. It returns the accepted-connection
// count, which is how "doctor opened no connection" is proved.
func stubEndpoint(t *testing.T, answer string) *atomic.Int32 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var conns atomic.Int32
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			if answer == "" { // accept and stall: consent pending
				held = append(held, c)
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte(answer + "\r\n\r\n"))
				time.Sleep(50 * time.Millisecond)
			}(c)
		}
	}()

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	pf := filepath.Join(t.TempDir(), "DevToolsActivePort")
	if err := os.WriteFile(pf, []byte(fmt.Sprintf("%s\n/devtools/browser/stub\n", port)), 0o600); err != nil {
		t.Fatalf("write port file: %v", err)
	}
	t.Setenv("CHROME_CDP_PORT_FILE", pf)
	if answer == "closed" {
		_ = ln.Close() // nothing listening: no endpoint
	} else {
		t.Cleanup(func() { _ = ln.Close() })
	}
	return &conns
}

// runDoctorApp runs `doctor --json`, optionally with a daemon-status seam wired.
func runDoctorApp(t *testing.T, status func(ConnOpts) (map[string]any, error), args ...string) (env map[string]any, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	app := New(nil, &out, &errb)
	if status != nil {
		app.WithDaemonCtl(nil, nil, status)
	}
	code = app.Execute(append([]string{"doctor", "--json"}, args...)...)
	if s := strings.TrimSpace(out.String()); strings.HasPrefix(s, "{") {
		if err := json.Unmarshal([]byte(s), &env); err != nil {
			t.Fatalf("stdout is not one JSON value: %v\n%s", err, s)
		}
	}
	return env, errb.String(), code
}

func doctorState(t *testing.T, env map[string]any) string {
	t.Helper()
	if res, ok := env["result"].(map[string]any); ok {
		s, _ := res["state"].(string)
		return s
	}
	if e, ok := env["error"].(map[string]any); ok {
		s, _ := e["state"].(string)
		return s
	}
	t.Fatalf("envelope has neither result nor error: %v", env)
	return ""
}

func doctorErrCode(env map[string]any) string {
	e, _ := env["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// TestDoctorDistinguishesAllThreeStates is VS-5. The ready case is established
// by a COMPLETED upgrade, not by the presence of a port file — that distinction
// is the whole defect.
func TestDoctorDistinguishesAllThreeStates(t *testing.T) {
	prev := doctorProbeWait
	doctorProbeWait = 400 * time.Millisecond
	t.Cleanup(func() { doctorProbeWait = prev })

	for _, c := range []struct {
		name      string
		answer    string
		wantState string
		wantCode  string
		wantOK    bool
	}{
		{"nothing listening", "closed", browser.WSRefused.String(), result.CodeConnection, false},
		{"accepts and stalls", "", browser.WSPending.String(), result.CodeConsentPending, false},
		{"completes the upgrade", "HTTP/1.1 101 Switching Protocols", browser.WSReady.String(), "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubEndpoint(t, c.answer)
			env, stderr, code := runDoctorApp(t, nil)
			if env["ok"] != c.wantOK {
				t.Fatalf("ok = %v, want %v (envelope %v)", env["ok"], c.wantOK, env)
			}
			if got := doctorState(t, env); got != c.wantState {
				t.Errorf("state = %q, want %q", got, c.wantState)
			}
			if !c.wantOK {
				if got := doctorErrCode(env); got != c.wantCode {
					t.Errorf("error.code = %q, want %q", got, c.wantCode)
				}
				if code != result.ExitConnection {
					t.Errorf("exit = %d, want %d", code, result.ExitConnection)
				}
			}
			// Open question 3: probing is itself a connection request, so a
			// diagnostic that was not asked to connect says so before it does.
			if !strings.Contains(stderr, "opens one connection") {
				t.Errorf("doctor probed without warning that it would:\n%s", stderr)
			}
		})
	}
}

// TestDoctorConsentPendingNamesTheDialog: the state is only useful if the
// message tells a user staring at a frozen browser what they are looking at.
func TestDoctorConsentPendingNamesTheDialog(t *testing.T) {
	prev := doctorProbeWait
	doctorProbeWait = 400 * time.Millisecond
	t.Cleanup(func() { doctorProbeWait = prev })

	stubEndpoint(t, "")
	env, _, _ := runDoctorApp(t, nil)
	e, _ := env["error"].(map[string]any)
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, browser.ConsentPromptAdvice) {
		t.Errorf("the consent_pending message does not carry browser.ConsentPromptAdvice:\n%s", msg)
	}
	if !strings.Contains(msg, "--remote-debugging-port=9222") {
		t.Errorf("the consent_pending message does not name the recovery:\n%s", msg)
	}
}

// TestDoctorAnswersThroughARunningDaemon is VS-6.
//
// Probing is a connection request, and on the chrome://inspect path a connection
// request is what raises the modal prompt. A running daemon is already holding a
// verified connection, so doctor must answer through it and open nothing —
// proved here by counting connections to the endpoint.
func TestDoctorAnswersThroughARunningDaemon(t *testing.T) {
	conns := stubEndpoint(t, "") // would classify as consent_pending IF probed

	env, stderr, code := runDoctorApp(t, func(ConnOpts) (map[string]any, error) {
		return map[string]any{"running": true, "connected": true, "socket": "/tmp/x.sock", "target_count": 3}, nil
	})

	if env["ok"] != true || code != result.ExitOK {
		t.Fatalf("doctor with a live daemon: ok=%v exit=%d (%v)", env["ok"], code, env)
	}
	if got := doctorState(t, env); got != browser.WSReady.String() {
		t.Errorf("state = %q, want %q", got, browser.WSReady.String())
	}
	res := env["result"].(map[string]any)
	if res["via"] != "daemon" {
		t.Errorf("via = %v, want daemon", res["via"])
	}
	if res["probed"] != false {
		t.Errorf("probed = %v, want false", res["probed"])
	}
	if res["target_count"] != float64(3) {
		t.Errorf("the daemon's own status fields should survive into the envelope: %v", res)
	}
	if n := conns.Load(); n != 0 {
		t.Errorf("doctor opened %d connection(s) to Chrome while a daemon was running — each one is a fresh consent request", n)
	}
	if strings.Contains(stderr, "opens one connection") {
		t.Errorf("doctor announced a probe it did not make:\n%s", stderr)
	}
}

// TestDoctorRequiresEvidenceFromTheDaemon is the second-order version of the
// defect this RFC's item 3 names: doctor stopped trusting the port file and
// started trusting `running: true` instead, which is just as unverified.
//
// The trigger is ordinary: start a daemon, quit Chrome. The chromedp connection
// is dead, but the daemon holds its listener for the whole idle window, so
// TryConnect still succeeds. Every state short of a daemon that has proved its
// connection must fall through to the probe rather than report ready.
func TestDoctorRequiresEvidenceFromTheDaemon(t *testing.T) {
	prev := doctorProbeWait
	doctorProbeWait = 400 * time.Millisecond
	t.Cleanup(func() { doctorProbeWait = prev })

	for _, c := range []struct {
		name   string
		status map[string]any
		err    error
	}{
		{"running but the CDP connection is dead", map[string]any{"running": true, "connected": false}, nil},
		{"running with no connection evidence at all", map[string]any{"running": true}, nil},
		{"the status call itself failed", nil, errors.New("dial unix: connection refused")},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A stalling endpoint: if doctor falls through and probes, it says
			// consent_pending. Anything claiming `ready` came from the daemon.
			stubEndpoint(t, "")
			env, _, _ := runDoctorApp(t, func(ConnOpts) (map[string]any, error) { return c.status, c.err })
			if got := doctorState(t, env); got == browser.WSReady.String() {
				t.Errorf("doctor reported %q from a daemon that never proved a live CDP connection: %v", got, env)
			}
		})
	}
}

// TestDoctorDoesNotLeakOpenTabURLs. SKILL.md makes `doctor --json` step 1 of
// every agent session, so anything doctor echoes is pulled into the transcript
// before a tab has even been selected. Blanket-copying the daemon's status map
// put every open tab's title and full URL there — OAuth callbacks, reset
// tokens, internal hostnames.
func TestDoctorDoesNotLeakOpenTabURLs(t *testing.T) {
	stubEndpoint(t, "")
	env, _, _ := runDoctorApp(t, func(ConnOpts) (map[string]any, error) {
		return map[string]any{
			"running": true, "connected": true, "socket": "/tmp/x.sock",
			"targets": []map[string]any{
				{"id": "1", "title": "Reset your password", "url": "https://intranet.example/reset?token=s3cret"},
			},
			"target_count": 1,
		}, nil
	})
	blob, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{"s3cret", "Reset your password", "intranet.example"} {
		if strings.Contains(string(blob), leak) {
			t.Errorf("doctor echoed %q into the envelope:\n%s", leak, blob)
		}
	}
	res, _ := env["result"].(map[string]any)
	if res["target_count"] != float64(1) {
		t.Errorf("target_count = %v, want 1 — the count is the useful part, the URLs are not", res["target_count"])
	}
}

// TestDoctorNoDaemonStatusStillProbes: a daemon that is not running is not an
// answer, so doctor falls through to the probe rather than reporting ready.
func TestDoctorNoDaemonStatusStillProbes(t *testing.T) {
	conns := stubEndpoint(t, "HTTP/1.1 101 Switching Protocols")
	env, _, _ := runDoctorApp(t, func(ConnOpts) (map[string]any, error) {
		return map[string]any{"running": false}, nil
	})
	if got := doctorState(t, env); got != browser.WSReady.String() {
		t.Errorf("state = %q, want %q", got, browser.WSReady.String())
	}
	if env["result"].(map[string]any)["via"] != "probe" {
		t.Errorf("with no daemon the answer must come from a probe: %v", env["result"])
	}
	if n := conns.Load(); n != 1 {
		t.Errorf("probed with %d connections, want exactly 1", n)
	}
}

// TestDoctorNoProbeRefusesToClaimReadiness: the escape hatch for a user who does
// not want a diagnostic to open a connection must not resurrect the old lie.
func TestDoctorNoProbeRefusesToClaimReadiness(t *testing.T) {
	conns := stubEndpoint(t, "")
	env, _, _ := runDoctorApp(t, nil, "--no-probe")
	if got := doctorState(t, env); got == browser.WSReady.String() {
		t.Error("--no-probe reported ready without verifying anything, which is the bug this RFC exists to fix")
	}
	if n := conns.Load(); n != 0 {
		t.Errorf("--no-probe opened %d connection(s), want 0", n)
	}
}

// TestConsentTimeoutFlagIsNormalised: the flag is the third way this value can
// be set, and the only one config resolution does not see. An explicit
// `--consent-timeout 0s` used to reach daemon.Ensure as a literal zero while
// the daemon it spawned resolved the same key to 120s from its environment —
// so the client reported "still waiting ... after 0s" for a daemon that was
// still holding the prompt open.
func TestConsentTimeoutFlagIsNormalised(t *testing.T) {
	prev := doctorProbeWait
	doctorProbeWait = 100 * time.Millisecond
	t.Cleanup(func() { doctorProbeWait = prev })
	stubEndpoint(t, "")
	for _, c := range []struct {
		flag string
		want time.Duration
	}{
		{"0s", chrome.DefaultConsentTimeout},
		{"-3s", chrome.DefaultConsentTimeout},
		{"8760h", chrome.MaxConsentTimeout},
		{"45s", 45 * time.Second},
	} {
		t.Run(c.flag, func(t *testing.T) {
			var got time.Duration
			runDoctorApp(t, func(o ConnOpts) (map[string]any, error) {
				got = o.ConsentTimeout
				return map[string]any{"running": false}, nil
			}, "--consent-timeout", c.flag)
			if got != c.want {
				t.Errorf("--consent-timeout %s reached the connector as %v, want %v", c.flag, got, c.want)
			}
		})
	}
}

// TestDoctorHonoursExplicitPort. Every other verb resolves its endpoint from
// --port before the DevToolsActivePort file; doctor called FindPortFile("") and
// never looked at the flag. So `doctor --port 9333` probed whatever Chrome the
// port file happened to name and pronounced THAT one healthy — a diagnostic
// answering a question about a different browser than the one asked about.
func TestDoctorHonoursExplicitPort(t *testing.T) {
	prev := doctorProbeWait
	doctorProbeWait = 300 * time.Millisecond
	t.Cleanup(func() { doctorProbeWait = prev })

	// The port file names a perfectly healthy endpoint...
	stubEndpoint(t, "HTTP/1.1 101 Switching Protocols")

	// ...and --port names one that is holding a consent prompt.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var stalled atomic.Int32
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			stalled.Add(1)
			held = append(held, c)
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	env, _, _ := runDoctorApp(t, nil, "--port", port)
	if got := doctorState(t, env); got != browser.WSPending.String() {
		t.Errorf("state = %q, want %q — doctor diagnosed a different Chrome than --port named: %v", got, browser.WSPending.String(), env)
	}
	if stalled.Load() == 0 {
		t.Error("doctor never contacted the --port endpoint at all")
	}
}

// TestDoctorReadyViaProbeSaysTheClickWillBeSpent.
//
// browser.Upgrade's doc comment states the governing model: "a probe that
// connects, learns the answer and hangs up has spent the user's click on a
// connection nobody kept". ProbeWS closes the socket on every outcome including
// WSReady, so doctor's `ready` verdict is falsified by the act of producing it
// — on the chrome://inspect path the next command raises a second prompt. The
// verdict is still worth having; it just has to say what it cost.
func TestDoctorReadyViaProbeSaysTheClickWillBeSpent(t *testing.T) {
	prev := doctorProbeWait
	doctorProbeWait = 400 * time.Millisecond
	t.Cleanup(func() { doctorProbeWait = prev })

	stubEndpoint(t, "HTTP/1.1 101 Switching Protocols")
	env, _, _ := runDoctorApp(t, nil)
	res, _ := env["result"].(map[string]any)
	status, _ := res["status"].(string)
	if !strings.Contains(status, "prompt again") {
		t.Errorf("doctor reported ready without saying the probe's connection was closed and the next command may prompt again:\n%s", status)
	}
}
