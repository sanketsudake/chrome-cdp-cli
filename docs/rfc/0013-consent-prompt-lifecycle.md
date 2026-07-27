# RFC-0013: Surviving Chrome's consent prompt

- **Status:** Draft
- **Priority:** P0
- **Area:** connection
- **Depends on:** the spawn serialisation in #17 (necessary, not sufficient)

## Summary

Stop `chrome-cdp` from wedging the user's browser on the one-time "Allow remote debugging?" prompt.
Three changes: wait for the consent it asks for instead of abandoning it, detect the pending state instead of reporting a generic failure, and prefer the connection path that never prompts.

## What happened

A user's Chrome froze with the consent dialog on screen and no button responding.
It was reproduced deliberately afterwards, and the reproduction contradicted the first diagnosis.

The initial theory was **stacked prompts**: several `chrome-cdp` processes had started at once, each found no daemon, each spawned one, and each spawned daemon attached to Chrome and raised its own prompt.
That much is real, and #17 fixes it — eight concurrent callers produced eight daemons before the fix and one after.

But the controlled reproduction showed a **single** prompt wedges Chrome just as thoroughly.
Serialising the spawn is necessary and not sufficient.

### What the reproduction established

Measured against a real Chrome made debug-enabled through the `chrome://inspect/#remote-debugging` toggle:

| Probe | While consent is pending |
|-------|--------------------------|
| TCP connect to `127.0.0.1:9222` | succeeds immediately |
| `GET /json/version` | 404 in under a millisecond |
| WebSocket upgrade to the browser endpoint | **hangs — never completes, never refuses** |

The hang is the mechanism.
Chrome does not reject the connection while it waits for the user; it holds the upgrade open and says nothing.
So the client has no error to classify — only silence — which is why the failure surfaces as an undifferentiated timeout.

Three consequences followed, each independently a defect:

1. **The daemon abandons the prompt it raised.**
   `chrome.Connect` dials, the upgrade hangs, the dial times out in about ten seconds, and the daemon writes its error and exits.
   The modal is left on screen with nothing behind it.
   Clicking Allow then grants consent to a connection that no longer exists.
2. **Ten seconds is not a human timescale for a dialog that may be invisible.**
   The prompt is browser-modal and can sit behind the window.
   A user who has not clicked it has usually not *seen* it, and by the time they do, the process that asked is gone.
3. **`doctor` reports readiness it never verified.**
   It reads the `DevToolsActivePort` file and reports "debug endpoint reachable — Path B attach ready", handing back a `ws://` URL.
   It does not probe.
   The one command whose job is to answer "can I connect?" answered yes while every connection was hanging.

A fourth observation is recorded because it cost time: **`/json/version` returning 404 is not a consent signal.**
It returns 404 in this connection mode whether or not consent has been granted — the toggle path exposes the WebSocket without the HTTP JSON API.
An early reading of that 404 as "consent pending" was wrong, and any detection built on it would be too.

## User stories

**US-1 — Do not freeze my browser.**
As a user, I want a tool that asks for consent to still be there when I answer, so that a prompt I did not see immediately does not leave my browser unusable.
*Acceptance:* a prompt answered minutes after it appears results in a working connection, not an orphaned dialog.

**US-2 — Tell me what is happening.**
As a user staring at an unresponsive browser, I want to be told a consent prompt is pending and where to find it, so that I know this is a dialog and not a crash.
*Acceptance:* while the upgrade is hanging, the CLI reports a distinct pending state naming the prompt, not a generic connection failure.
This holds on every path that can wait: the daemon, `--no-daemon`, and a second command queued behind the first — which says it is queueing rather than blocking in silence.

**US-3 — Do not ask at all when you do not have to.**
As a user, I want to be steered to the launch flag that skips consent entirely, so that routine use never involves a modal.
*Acceptance:* when no usable endpoint exists, the CLI recommends `--remote-debugging-port` before it recommends the toggle that prompts.

**US-4 — Recover without losing my tabs.**
As a user whose browser is already wedged, I want to be told the actual remedy, so that I am not left force-quitting on a guess.
*Acceptance:* the failure message names the recovery and does not imply the browser has crashed.

**US-5 — One prompt, not many.**
As a user running several commands at once, I want at most one consent request.
*Acceptance:* covered by #17 for concurrent spawns; this RFC keeps it true across the wait as well.
Serialising the spawn only guarantees one prompt *at a time*: without more, each queued caller in turn cleared the previous verdict and raised its own, so eight commands became eight sequential prompts.
A `consent_pending` verdict is inherited by callers released within a few seconds of it, so a queue drains on one answer.

## Proposed changes

### 1. Wait for the consent

The daemon's initial connect must outlive the dialog.
Replace the single short dial with a bounded wait — proposal: `consent_timeout`, default **120s** — during which the daemon stays alive and keeps the pending upgrade open.

Distinguishing the pending state from a dead endpoint is what makes a long wait safe.
A refused TCP connection is a real failure and must stay fast; a hanging *upgrade* against an *open* port is the consent signature and is the only case that earns the long wait.

### 2. Report the pending state

Add `CodeConsentPending = "consent_pending"` mapped to the existing connection exit code, so a caller can branch on it without a new number.

While waiting, the CLI says a consent prompt is pending, that it is browser-modal and may be behind the window, and that Chrome will accept no other input until it is answered.
That last clause is the part a user cannot deduce, and is why a frozen browser reads as a crash.

### 3. Make `doctor` probe

`doctor` must attempt the upgrade rather than trusting the port file, and report one of: no endpoint, consent pending, or ready (plus `unverified`, which is what `--no-probe` reports and is explicitly not an answer).
A diagnostic that reports readiness without testing it is worse than no diagnostic, because it sends the user looking somewhere else.

Probing is itself a connection request, so `doctor` must reuse a live daemon when one exists rather than raising a prompt of its own.
"Live" means the daemon has just completed a round trip to Chrome, not that its socket answered: the socket outlives the connection by up to the daemon's whole idle window, so `running: true` is the same unverified claim as the port file, one level up.

Two consequences of `doctor` being the first thing many callers run:

- It reports a **count** of open tabs, never their titles or URLs.
  The Agent Skill makes `doctor --json` step 1 of every session, and a diagnostic that answers "can I connect?" with a list of the user's OAuth callbacks and reset tokens is answering a question nobody asked.
- A `ready` reached by probing says what it cost.
  The probe closes its own connection, so on the toggle path the next command is a fresh attach and prompts again — a verdict falsified by the act of producing it, unless it is disclosed.

`doctor` resolves `--port` like every other verb.
Diagnosing a different browser than the one the flag names is the same class of error as diagnosing one nobody connected to.

### 4. Prefer the path that never prompts

When no usable endpoint exists, recommend relaunching Chrome with `--remote-debugging-port=9222` **first**, and the `chrome://inspect` toggle second with a note that it prompts on every fresh attach.
The toggle is currently presented as the primary route, which routes every new user through the failure this RFC exists to remove.

## Verification scenarios

**VS-1 — A hanging upgrade is classified as consent pending, not as a timeout.**
Given a listener that accepts TCP and never completes the WebSocket upgrade, when the daemon connects, then it reports `consent_pending` and stays alive rather than exiting.

**VS-2 — A refused endpoint still fails fast.**
Given a closed port, when the daemon connects, then it fails within a second or two with `connection_failed`, not after the consent timeout.

**VS-3 — Consent answered late still works.**
Given a listener that completes the upgrade after 30 seconds, when the daemon connects, then the connection succeeds and no prompt is orphaned.

**VS-4 — The consent wait is bounded.**
Given a listener that never completes the upgrade, when `consent_timeout` elapses, then the daemon exits with a message naming the prompt and the recovery.

**VS-5 — `doctor` distinguishes all three states.**
Table over: no endpoint, open-but-hanging, and ready — each reported distinctly, and the ready case verified by a completed upgrade rather than by the port file alone.
"Verified" excludes every proxy for a completed round trip, including a daemon that is merely running.

**VS-6 — `doctor` does not raise its own prompt.**
Given a running daemon, when `doctor` runs, then it answers through the daemon and initiates no new connection.

**VS-8 — the probe reads what it is owed and no more.**
Given a listener that accepts and then streams bytes with no newline, the probe classifies it as refused at its read limit rather than accumulating for the whole consent budget.
Chrome's debug port is a loopback port any local process can bind, and the budget this RFC introduces is what turns an unbounded read into gigabytes.

**VS-9 — a 101 that is not our handshake is not ready.**
Given a listener answering `101` without a valid `Sec-WebSocket-Accept` for the key we sent, the endpoint is classified refused.

**VS-7 — Concurrency stays at one prompt.**
The guard from #17, restated here so this RFC's changes cannot regress it.

## Test plan

The pending state is a **local listener that accepts and stalls**, so almost all of this is testable with `net.Listen` and no browser at all — which matters, because the manual reproduction wedged a real browser twice and must not be the regression test.

- **Pure/stub (`-short`):** VS-1 through VS-5 against hand-built listeners — refusing, stalling, and completing-after-a-delay.
  This is where the classification logic belongs.
- **Daemon:** VS-6 and VS-7 against the existing socket harness.
- **Live Chrome:** none.
  Consent cannot be granted programmatically, and a test that needs a human click is not a test.

Note for anyone extending this: a long `t.TempDir()` path breaks a Unix socket bind on darwin — `sun_path` caps near 104 bytes and the directory embeds the test name, which fails with a bare `bind: invalid argument`.

## Out of scope

- Granting or suppressing consent programmatically.
  It is a deliberate user decision and the tool should not try to route around it.
- Anything about Chrome's own modal behaviour, which is not ours to change.
- Windows: the project ships linux and darwin only.

## Open questions

1. Should the CLI **refuse to raise a prompt at all** unless the user opts in — erroring with "relaunch with `--remote-debugging-port`" instead?
   Safest, but it removes the zero-config path that makes the tool pleasant on first use.
   **Recommendation:** keep the prompt, fix the waiting, and lead with the flag in the docs.
2. Is 120s the right consent timeout?
   Long enough for a hidden dialog, short enough that a genuinely dead endpoint is not mistaken for a slow human.
   **Recommendation:** 120s, as a config key so it can be argued with.
   **Resolved:** 120s, clamped to `[1s, 10m]` and normalised once where flag, environment and config file resolve.
   The clamp is not tidiness: `0s` meant "the default" to one layer and "do not wait" to another, which restored the orphaned-prompt failure through the parameter's own zero value, and an inherited `CHROME_CDP_CONSENT_TIMEOUT=8760h` would hold the spawn lock — and therefore every other command — for a year.
3. Should `doctor` be able to probe *without* a daemon, accepting that it may raise a prompt?
   **Recommendation:** yes, but say so before doing it, since the user ran a diagnostic and did not ask to connect.
