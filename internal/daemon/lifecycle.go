package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// SocketPath returns the daemon socket path for an endpoint key, under the XDG
// runtime/state dir. Liveness is discovered by connecting (no PID file).
func SocketPath(key string) string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		if s := os.Getenv("XDG_STATE_HOME"); s != "" {
			base = s
		} else {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, ".local", "state")
		}
	}
	dir := filepath.Join(base, "chrome-cdp")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "daemon-"+sanitize(key)+".sock")
}

func sanitize(s string) string {
	if s == "" {
		return "default"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}

// connectErr is the JSON payload written to the .err sidecar so the daemon's
// connect failure crosses the process boundary with its stable code intact.
type connectErr struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// encodeConnectErr serializes a connect failure, preserving a *chrome.ConnectError's
// code so Ensure can reconstruct it.
func encodeConnectErr(err error) []byte {
	e := connectErr{Message: err.Error()}
	var ce *chrome.ConnectError
	if errors.As(err, &ce) {
		e.Code = ce.Code
	}
	b, _ := json.Marshal(e)
	return b
}

// decodeConnectErr reconstructs a connect failure from the sidecar. With a code
// present it returns a *chrome.ConnectError so callers recover the stable code;
// otherwise (including a legacy pre-JSON payload) it surfaces the raw message.
func decodeConnectErr(data []byte) error {
	var e connectErr
	if err := json.Unmarshal(data, &e); err != nil || e.Message == "" {
		return errors.New(strings.TrimSpace(string(data)))
	}
	if e.Code != "" {
		return &chrome.ConnectError{Code: e.Code, Message: e.Message}
	}
	return errors.New(e.Message)
}

// Sidecar files the daemon leaves next to its socket so its state crosses the
// process boundary: the spawned daemon is detached and has no stderr the user
// will ever read.
const (
	errSuffix     = ".err"     // a connect failure, with its stable code
	pendingSuffix = ".pending" // "I am waiting on Chrome's consent prompt"
	lockSuffix    = ".lock"    // the spawn-and-wait exclusion (see lockSpawn)
)

// startupWait is how long a daemon gets to come up before Ensure gives up, and
// (after a pending prompt is seen) the grace on top of the consent budget. It is
// short because a daemon that is going to work works immediately — EXCEPT when
// Chrome is asking the user for consent, which is why the pending sidecar
// extends the deadline rather than this being large. A var only so tests can
// shrink the clock.
var startupWait = 10 * time.Second

// notice prints a one-line advisory to the user while Ensure waits. It is a var
// so a test can capture it without a terminal.
var notice = func(msg string) { fmt.Fprintln(os.Stderr, "chrome-cdp:", msg) }

// lockWaitNotice is said when another chrome-cdp already holds the spawn lock —
// which, when a prompt is pending, means this command is about to wait minutes.
const lockWaitNotice = "another chrome-cdp is already starting the connection; waiting for it rather than opening a second one " +
	"(a second connection would raise a second consent prompt)."

// consentWaitNotice is said WHILE the dialog is on screen, which is the only
// time it can help. Told afterwards it is a post-mortem.
const consentWaitNotice = browser.ConsentPromptAdvice + " Nothing else is needed; this command is waiting for you."

// TryConnect returns a Client if a daemon is already listening on sockPath.
func TryConnect(sockPath string) *Client {
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		return nil
	}
	_ = conn.Close()
	return &Client{path: sockPath}
}

// Ensure connects to a running daemon, or spawns one (detached) and waits for it
// to come up. env carries the connection options (CHROME_CDP_*) for the daemon.
//
// consentTimeout is how long the spawned daemon is allowed to spend waiting out
// Chrome's consent prompt. Ensure has to know it too: the daemon's wait is
// invisible from here, and a client that gave up at ten seconds while its daemon
// was still holding the connection would report a failure that had not happened.
// It arrives normalised (see chrome.ClampConsentTimeout).
//
// ctx bounds the whole thing, including the wait for the spawn lock. Without
// it, --timeout stopped applying the moment a command needed a connection: the
// lock's holder may be sitting on an unanswered prompt for two minutes, and
// every caller behind it inherited that wait with no way to say otherwise.
func Ensure(ctx context.Context, sockPath, exePath string, env []string, consentTimeout time.Duration) (*Client, error) {
	if c := TryConnect(sockPath); c != nil {
		return c, nil
	}

	// From here on, exactly one process at a time. Concurrent invocations that
	// all find no daemon would otherwise each spawn one, and each spawned daemon
	// attaches to Chrome — which raises a SEPARATE browser-modal "Allow remote
	// debugging?" prompt. Several stacked prompts is not a slower version of
	// one: the visible dialog need not be the one holding input, so the whole
	// browser looks frozen with no button that responds. The daemon exists so
	// that prompt happens once per session; nothing was making the FIRST attach
	// single-file.
	//
	// The unlinks below are the other half. Outside the lock they can delete a
	// socket a sibling daemon has just bound, orphaning a live daemon that no
	// client can ever reach.
	unlock, err := lockSpawn(ctx, sockPath)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Re-check under the lock: while we waited, the holder may have started the
	// daemon we were about to duplicate. This is what makes N callers converge
	// on one daemon and one prompt.
	if c := TryConnect(sockPath); c != nil {
		return c, nil
	}

	// The holder may instead have come back with a verdict, and a FRESH
	// consent_pending is one to inherit rather than re-derive. Deriving it
	// again means spawning a daemon, attaching, and raising a second prompt at
	// a browser that is already holding one — so eight queued callers came to
	// eight sequential prompts and about seventeen minutes, which is US-5
	// ("at most one consent request") failing while VS-7 ("never two at once")
	// passed. Only consent_pending is inherited, and only briefly: every other
	// failure is per-attempt, and a verdict older than the TTL has probably
	// been overtaken by the user finding the dialog and clicking Allow.
	if err := recentConsentVerdict(sockPath); err != nil {
		return nil, err
	}

	_ = os.Remove(sockPath)                 // clear a stale socket file
	_ = os.Remove(sockPath + errSuffix)     // and a stale error, so we only read THIS spawn's
	_ = os.Remove(sockPath + pendingSuffix) // ditto a stale consent marker

	proc, err := spawnDaemon(exePath, sockPath, env)
	if err != nil {
		return nil, err
	}

	// The deadline MOVES. A daemon that is merely slow gets ten seconds; one that
	// says it is holding a consent prompt gets the whole consent budget, because
	// the thing it is waiting for is a human. The grace lets the daemon hit its
	// own timeout first, so the error the user sees is the specific one it wrote
	// rather than a generic "daemon did not start".
	deadline := time.Now().Add(startupWait)
	waiting := false
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if c := TryConnect(sockPath); c != nil {
			return c, nil
		}
		// The daemon writes its connect error here before exiting; decode it so
		// the specific code (e.g. consent_pending) survives the process boundary.
		if data, e := os.ReadFile(sockPath + errSuffix); e == nil && len(data) > 0 {
			return nil, decodeConnectErr(data)
		}
		// A child that is gone with nothing written is over, whatever the
		// deadline says. Nothing else can answer this: the sidecars are the
		// daemon's own reports, and a daemon killed outright (or panicking
		// inside chrome.Connect, which RunDaemon does not recover) files none.
		// Before this, such a daemon left the pending marker standing and the
		// caller waited the whole ~130s for a process that no longer existed.
		if proc.gone() {
			return nil, &chrome.ConnectError{Code: result.CodeDaemon,
				Message: "the daemon exited without reporting why (it was killed, or it crashed while connecting) — retry, and if it repeats run with --no-daemon to see the connect error directly"}
		}
		if !waiting {
			if _, e := os.Stat(sockPath + pendingSuffix); e == nil {
				waiting = true
				deadline = time.Now().Add(consentTimeout + startupWait)
				notice(consentWaitNotice)
			}
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if waiting {
		return nil, &chrome.ConnectError{Code: result.CodeConsentPending,
			Message: "the daemon is still waiting for consent after " + consentTimeout.String() + ". " + browser.ConsentPromptAdvice}
	}
	return nil, &chrome.ConnectError{Code: result.CodeDaemon,
		Message: "daemon did not start within " + startupWait.String() + ". " + browser.ConsentPromptAdvice}
}

// daemonProc is Ensure's handle on the daemon it spawned. All it carries is
// "has it exited", which is the one question the sidecar files cannot answer: a
// daemon SIGKILLed, or panicking inside chrome.Connect (RunDaemon has no
// recover), leaves a .pending marker and no .err, and without this the wait had
// no reason to stop before the whole consent budget had elapsed.
//
// The signal comes from Wait rather than from kill(pid, 0), because the daemon
// is our child until it is reparented: a dead one is a zombie, and a zombie
// answers a liveness signal perfectly well. Waiting reaps it AND tells us.
type daemonProc struct{ exited chan struct{} }

// gone reports whether the daemon process has already exited.
func (p *daemonProc) gone() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

// spawnDaemon starts the detached daemon process. It is a variable so a test can
// substitute a spawn it can count, without a real Chrome or a real binary.
var spawnDaemon = func(exePath, sockPath string, env []string) (*daemonProc, error) {
	cmd := exec.Command(exePath, "__daemon", sockPath)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach into its own session
	if err := cmd.Start(); err != nil {
		return nil, &chrome.ConnectError{Code: result.CodeDaemon, Message: "cannot start daemon: " + err.Error()}
	}
	p := &daemonProc{exited: make(chan struct{})}
	// Reaps the child if it dies while we are still here, and leaves the
	// daemon entirely alone if it does not: this process exits within seconds
	// either way, and the daemon (setsid) is reparented and carries on.
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// lockSpawn takes an exclusive advisory lock covering the spawn-and-wait for one
// socket path, and returns the release. The lock file is never removed: unlinking
// it would let a later caller lock a different inode and defeat the exclusion.
//
// The wait is deliberately unbounded. The holder may be waiting out a consent
// prompt the user has not clicked yet, and blocking behind it is the correct
// outcome — spawning our own would add another prompt to the pile, which is the
// failure this exists to prevent.
//
// It is not, however, silent. The non-blocking attempt comes first purely so
// that contention can be NAMED: a second command run during a pending prompt
// used to hang for over two minutes with no output at all, which is the same
// "my tool has frozen and I do not know why" that US-2 exists to end — arrived
// at by the fix for US-2.
func lockSpawn(ctx context.Context, sockPath string) (func(), error) {
	f, err := os.OpenFile(sockPath+lockSuffix, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, &chrome.ConnectError{Code: result.CodeDaemon, Message: "cannot open the daemon spawn lock: " + err.Error()}
	}
	unlock := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	fail := func(err error) (func(), error) {
		_ = f.Close()
		return nil, &chrome.ConnectError{Code: result.CodeDaemon, Message: "cannot take the daemon spawn lock: " + err.Error()}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return unlock, nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return fail(err)
	}
	notice(lockWaitNotice)

	// flock has no deadline, so the blocking wait runs on its own goroutine and
	// the context is honoured here. If the context wins, the goroutine may
	// still acquire the lock afterwards — so it is handed the release to run
	// itself, rather than being abandoned holding it.
	// done is UNBUFFERED on purpose: the send succeeds only while this function
	// is still selecting on it, so "the caller has gone" and "the caller took
	// the lock" cannot both happen.
	done := make(chan error)
	abandoned := make(chan struct{})
	go func() {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		select {
		case done <- err:
		case <-abandoned: // nobody is waiting any more: give the lock straight back
			if err == nil {
				unlock()
			} else {
				_ = f.Close()
			}
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			return fail(err)
		}
		return unlock, nil
	case <-ctx.Done():
		close(abandoned)
		return nil, ctx.Err()
	}
}

// consentVerdictTTL is how long a consent_pending verdict left by the previous
// holder is inherited instead of re-derived. Queued callers are released within
// milliseconds of the verdict being written, so this only has to be long enough
// to drain a queue — and short, because the moment the user finds the dialog
// and clicks Allow the verdict is wrong, and a caller told to go looking for a
// prompt they have already answered is worse off than one that simply retried.
const consentVerdictTTL = 5 * time.Second

// recentConsentVerdict returns the previous holder's consent_pending failure
// when it is recent enough to still be true, and nil otherwise.
func recentConsentVerdict(sockPath string) error {
	path := sockPath + errSuffix
	st, err := os.Stat(path)
	if err != nil || time.Since(st.ModTime()) > consentVerdictTTL {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	var ce *chrome.ConnectError
	if decoded := decodeConnectErr(data); errors.As(decoded, &ce) && ce.Code == result.CodeConsentPending {
		return decoded
	}
	return nil
}

// connectBrowser is chrome.Connect behind a seam, so RunDaemon's behaviour
// AFTER a successful connect is testable without a browser.
var connectBrowser = func(ctx context.Context, opts chrome.Options) (chrome.Browser, error) {
	return chrome.Connect(ctx, opts)
}

// RunDaemon connects Chrome and serves sockPath until idle or stopped. Used by
// the hidden `__daemon` invocation.
func RunDaemon(sockPath string, opts chrome.Options, idle time.Duration) error {
	// The daemon is detached: nothing it writes to stderr will ever be read. So
	// "I am waiting on the consent prompt" is published as a file next to the
	// socket, which is the only channel Ensure has into a connect that has not
	// finished. Written BEFORE the wait, not after — the point is to tell the
	// user while the dialog is still on screen.
	pending := sockPath + pendingSuffix
	_ = os.Remove(pending)
	opts.OnConsentPending = func() {
		_ = os.WriteFile(pending, []byte("waiting for Chrome's remote-debugging consent prompt\n"), 0o600)
	}
	b, err := connectBrowser(context.Background(), opts)
	_ = os.Remove(pending)
	if err != nil {
		// Leave the reason (with its code) for Ensure to surface, then exit.
		_ = os.WriteFile(sockPath+errSuffix, encodeConnectErr(err), 0o600)
		return err
	}
	_ = os.Remove(sockPath + errSuffix)
	defer b.Close()

	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		// Everything after the connect used to report only to a stderr nobody
		// reads: the daemon is detached. So a bind failure — which the darwin
		// sun_path limit makes entirely reachable — left the pending marker as
		// the last thing Ensure had seen, and the user was told about a consent
		// prompt for a failure that had nothing to do with one.
		berr := &chrome.ConnectError{Code: result.CodeDaemon,
			Message: "the daemon connected to Chrome but could not bind its socket at " + sockPath + " (" + err.Error() + ")"}
		_ = os.WriteFile(sockPath+errSuffix, encodeConnectErr(berr), 0o600)
		return berr
	}
	defer os.Remove(sockPath)

	Serve(ln, b, idle)
	return nil
}
