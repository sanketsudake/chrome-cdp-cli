# RFC-0018: `dialog` — inspect and close a native dialog that is already on screen

- **Status:** Accepted — pending PR
- **Priority:** P1
- **Area:** input
- **Depends on:** RFC-0002 (the attach-time capture hook, `listenCapture`, where every per-tab listener is registered before the tab is attached), the `--on-dialog` action option (`withDialog` in `internal/chrome/cdp.go`, whose CDP mechanics this RFC reuses)
- **Related:** RFC-0004 (the MCP surface; the verb is folded into the existing `tabs` tool), RFC-0012 (policy classes for the three subcommands), RFC-0013 (the other "a dialog is holding the browser" failure, at the browser level rather than the tab level)

## Summary

Add a `dialog` verb with three subcommands:

- `dialog status` reports the native JavaScript dialog — `alert`, `confirm`, `prompt`, or `beforeunload` — currently open on the target tab: `{open, type, message, default_prompt, frame_url, opened_at}`, or `{open: false}`.
- `dialog accept [text]` closes it the way OK would: `confirm()` returns `true`, `prompt()` returns `text` (the empty string when none is given), a `beforeunload` dialog lets the navigation proceed.
- `dialog dismiss` closes it the way Cancel would: `confirm()` returns `false`, `prompt()` returns `null`, `beforeunload` stays on the page.

Nothing here talks to the page's renderer, because while a native dialog is up the renderer is blocked: every `Runtime.evaluate`, every DOM read, every screenshot hangs until the dialog closes.
`status` answers from an event the connection **retained when the dialog opened**, and `accept` / `dismiss` issue the one CDP command that works while the renderer is blocked, `Page.handleJavaScriptDialog` — the same command `--on-dialog` already issues from inside an action.

The retention is the design.
A dialog is only visible to CDP if a session with the `Page` domain enabled was attached to the tab **when it opened**; Chrome does not replay `Page.javascriptDialogOpening` to a session that attaches later, and a later session's `Page.handleJavaScriptDialog` is forwarded to the blocked renderer and hangs.
So the listener lives where the console and network listeners live — in the daemon's long-lived per-tab attach — and keeps the last open event per tab, cleared on `Page.javascriptDialogClosed`.
A tab the daemon had not touched before the dialog opened is honestly reported: `open: false` with a `note`, exactly as `console` reports partial history, and `accept` / `dismiss` refuse with `target_not_found` instead of hanging.

`--on-dialog` is unchanged.

## Motivation

`--on-dialog accept|dismiss` handles the dialog an action *causes*.
It does not help once a dialog is already up:

- **The common failure is the unguarded click.**
  An agent clicks Delete without `--on-dialog`, `confirm()` opens, the click blocks until its deadline and reports `target_timeout`, and every command after it — `snap`, `eval`, `screenshot`, `wait` — hangs the same way.
  The skill says "avoid native dialogs: they block CDP", and until now the only remedies were a human clicking OK in the Chrome window or closing the tab.
- **The connection already knows what is on screen.**
  The daemon attaches once per tab and holds the attach, which is precisely the session Chrome sends `javascriptDialogOpening` to.
  The event carries the type, the message and the prompt default; `Page.handleJavaScriptDialog` closes it in two milliseconds from that same session.
  The fact is received and thrown away because nobody stores it.
- **`beforeunload` has no other answer.**
  A page that asks "Leave site?" on navigation blocks `nav`, `close` and `open` of the same tab; `--on-dialog` is not on those verbs, and a dialog that appears *after* the navigating verb returned is nobody's.
- **Every piece exists.**
  `dialogSink` and `withDialog` already listen for the event and issue the handle command; `listenCapture` is the established hook for "register a listener before the attach"; the daemon already keeps per-tab state (console, net, recordings) with its own mutex.
  This RFC connects them behind one verb.

## User stories

**US-1 — What is blocking my tab?**
As an agent whose last command timed out, I want `dialog status` to tell me whether a native dialog is up and what it says, so that I can decide to accept or dismiss it instead of retrying a command that will hang again.
*Acceptance:* while a `confirm('Delete 3 items?')` is open on a tab the daemon is attached to, `dialog status` returns within the command's normal latency with `open: true`, `type: "confirm"`, `message: "Delete 3 items?"`.

**US-2 — Unblock it.**
As an agent, I want `dialog accept` to close the dialog and let the blocked page continue, so that the command that opened it completes and the next command works.
*Acceptance:* `dialog accept` returns `handled: true`; the `eval` that called `confirm()` returns `true`; a following `dialog status` is `open: false`.

**US-3 — Answer a prompt.**
As an agent, I want `dialog accept "Quarterly report"` to answer a `prompt()` with that text, so that a page asking for a name gets one.
*Acceptance:* `prompt('Name?', 'untitled')` returns `"Quarterly report"`; the result carries `text: "Quarterly report"`.

**US-4 — Nothing open is an answer, not an error.**
As a script author, I want `dialog status` on a quiet tab to exit 0 with `open: false`, so that "check, then act" does not need error handling for the normal case.
*Acceptance:* on an attached tab with no dialog, exit 0 and `result.open == false`.

**US-5 — Nothing to accept is an error I can tell apart.**
As an agent, I want `dialog accept` on a tab with no dialog to fail with a code that means "the thing is not there", so that I neither treat it as a malformed command nor as a browser failure.
*Acceptance:* `error.code == "target_not_found"`, exit 4, `error.details.dialog == "none"`.

**US-6 — Tell me when you could not have seen it.**
As a user running `--no-daemon`, or whose daemon never touched this tab before the dialog opened, I want the envelope to say that the answer is partial, so that `open: false` is not read as "no dialog".
*Acceptance:* the first `dialog status` on a tab the connection had not attached carries a `note`; the second does not.

**US-7 — Works in a batch and under policy.**
As an agent, I want `dialog …` inside `session`, and I want a read-only policy to allow `dialog status` and refuse `dialog accept` / `dismiss`.
*Acceptance:* the line produces one envelope inside `session`; `dialog status` is classified `Reading`, the other two `Mutating`.

## Proposed CLI surface

```
chrome-cdp dialog status
chrome-cdp dialog accept [text]
chrome-cdp dialog dismiss
```

| Subcommand | Args | Purpose |
|------------|------|---------|
| `status` | none | report the native dialog open on the target tab, or `open: false` |
| `accept [text]` | at most one | close it as OK would; `text` answers a `prompt()` and is ignored for the other types |
| `dismiss` | none | close it as Cancel would |

No flags of its own.
The verb addresses the **tab**, not an element, so the `QueryOpts` flags (`--by`, `--wait`, …) do not apply; it takes the global `--target`, `--timeout`, `--session`, `--json` like every other verb.
`--on-dialog` is an action option and has no meaning here; it is accepted and ignored as on every non-action verb.

Semantics per dialog type, which are Chrome's and not this tool's (the first three rows measured against headless Chrome with chromedp v0.15.1; `beforeunload` is the protocol's definition of `accept`, "leave the page"):

| Type | `accept` | `accept <text>` | `dismiss` |
|------|----------|-----------------|-----------|
| `alert` | closes | closes (text ignored) | closes |
| `confirm` | `confirm()` returns `true` | same (text ignored) | returns `false` |
| `prompt` | `prompt()` returns `""` — **not** the default | returns `text` | returns `null` |
| `beforeunload` | the navigation proceeds | same (text ignored) | the page stays |

`accept` with no text answers a prompt with the empty string, because `Page.handleJavaScriptDialog` with no `promptText` does; a caller who wants the default passes `default_prompt` back explicitly.

Examples:

```sh
chrome-cdp dialog status                      # {"open":true,"type":"confirm","message":"Delete 3 items?",…}
chrome-cdp dialog accept                      # confirm() → true; the blocked click completes
chrome-cdp dialog accept "Quarterly report"   # prompt() → "Quarterly report"
chrome-cdp dialog dismiss                     # confirm() → false / prompt() → null / stay on page
```

## Result envelope

`dialog status`:

```json
{ "ok": true, "command": "dialog",
  "target": {"id":"…","title":"…","url":"…"},
  "result": { "open": true, "type": "confirm", "message": "Delete 3 items?",
              "default_prompt": "", "frame_url": "https://app.example/items",
              "opened_at": "2026-08-19T10:15:00.412Z" },
  "elapsed_ms": 4 }
```

| Field | Present | Meaning |
|-------|---------|---------|
| `open` | always | whether a native dialog is retained as open for this tab |
| `type` | when `open` | `alert` \| `confirm` \| `prompt` \| `beforeunload`, Chrome's `Page.DialogType` verbatim |
| `message` | when `open` | the dialog's text |
| `default_prompt` | when `open` | the `prompt()` default; `""` for the other types (the key is always there when `open`, so the shape is total) |
| `frame_url` | when `open` | the URL of the frame that opened it (`javascriptDialogOpening.url`) — an iframe's dialog names the iframe |
| `opened_at` | when `open` | when the connection received the opening event, RFC 3339 UTC with millisecond precision, the same encoding as `console`'s `ts`; Chrome's event carries no timestamp of its own |
| `note` | only on a fresh attach | the tab was not being watched before this command; see *When the connection was not listening* |

`frame_url`, not `url`: `targetAction` rewrites the envelope's `target.url` from any `result.url` (so a verb that navigated reports where it landed), and an iframe's dialog must not be reported as the tab's address.
A `url` key would also be picked up by the human formatter's `oneLine` key priority.

`dialog accept` / `dialog dismiss`:

```json
{ "ok": true, "command": "dialog",
  "target": {"id":"…","title":"…","url":"…"},
  "result": { "handled": true, "action": "accept", "type": "prompt",
              "message": "Name?", "text": "Quarterly report" },
  "elapsed_ms": 6 }
```

| Field | Present | Meaning |
|-------|---------|---------|
| `handled` | always | `true`; the failure case is an error envelope, never `handled: false` |
| `action` | always | `accept` \| `dismiss` |
| `type`, `message` | always | the dialog that was closed, from the retained event |
| `text` | `type == "prompt"` and `action == "accept"` | the answer sent, `""` when none was given |
| `text_ignored` | text given and `type != "prompt"` | `true`; the text was not sent because the dialog has no input |

Human mode goes through the generic `oneLine` path, which finds none of its preferred keys and prints the map; no formatter change.
Human-mode rendering never alters the JSON shape.

## Errors and exit codes

| Situation | `error.code` | Exit | `details` |
|-----------|--------------|------|-----------|
| `dialog status <arg>`, `dialog dismiss <arg>`, `dialog accept a b` | `usage` | 2 | — (cobra `NoArgs` / `MaximumNArgs(1)`, refused before connecting) |
| `dialog accept` / `dismiss` with no dialog retained for the tab | `target_not_found` | 4 | `{"dialog": "none"}` |
| same, on a tab the connection had not attached before | `target_not_found` | 4 | `{"dialog": "none"}`; the message carries the not-watched note |
| Chrome answers `Page.handleJavaScriptDialog` with `No dialog is showing` (closed in the UI a moment ago) | `target_not_found` | 4 | `{"dialog": "none"}` |
| the tab cannot be resolved / the connection fails / the attach runs out the deadline | unchanged | unchanged | the existing codes from `resolveTarget` and `classifyActionErr` |
| `dialog status` with no dialog | **not an error**: `ok: true`, `open: false` | 0 | — |

No new codes.
`target_not_found` rather than `usage` for "nothing to accept": the invocation is well-formed, and what is missing is the thing on the page, which is what exit 4 means (`selector not found`); an agent's right reaction is "re-read the state", not "fix the command".
`record stop` with no recording chose `usage`, and the two are distinguishable by their `details` (`recording: false` there, `dialog: "none"` here); the codes differ because a missing recording is a sequencing mistake by the same caller, while a missing dialog is a page state the caller did not create.

## Design notes

### Where a dialog is visible from, and why the daemon is the only place that works

Measured against headless Chrome with chromedp v0.15.1, from a probe outside the repo:

- A session attached with `Page` enabled **when the dialog opens** receives `Page.javascriptDialogOpening` (`type`, `message`, `defaultPrompt`, `url`, `hasBrowserHandler`); its `Page.handleJavaScriptDialog` returns in about 2 ms; it then receives `Page.javascriptDialogClosed` (`result`, `userInput`).
  Closing the dialog in the Chrome window sends the same `closed` event.
- With no dialog open, the same session's `Page.handleJavaScriptDialog` fails immediately with `No dialog is showing (-32602)`.
- While the dialog is open, `Runtime.evaluate` and `Page.getLayoutMetrics` hang on that session; browser-level `Target.getTargetInfo` answers at once.
- A session that attaches **after** the dialog opened receives no opening event (there is no replay on `Page.enable`), and its `Page.handleJavaScriptDialog` hangs instead of answering: the browser-side handler has no pending dialog for that session and forwards the command to the blocked renderer.
- Whether the late attach itself succeeds was observed both ways: it returned at once when no other session was attached, and hung until the deadline when another `Page`-enabled session was.
  The design below is indifferent to which: `status` reads retained state, never the renderer, and `accept` / `dismiss` never issue the handle command without a retained event.

So the only process that can see and close a pre-existing dialog is the one that was attached when it opened, and in normal use that is the daemon.
Chromium keeps one pending-dialog callback per *enabled* `PageHandler`, which is what these observations reflect; the RFC relies on the observations, not on the source.

### The listener: `listenDialog`, registered in `listenCapture`, retained per tab

New file `internal/chrome/dialog.go`.

```go
// dialogState is the last Page.javascriptDialogOpening this connection received
// for a tab and has not yet seen closed. One per tab, replaced by the next
// opening and deleted on javascriptDialogClosed (RFC-0018).
type dialogState struct {
    Type          string
    Message       string
    DefaultPrompt string
    FrameURL      string
    OpenedAt      time.Time
}

// ErrNoDialog reports an accept/dismiss with nothing retained as open.
var ErrNoDialog = errors.New("no native dialog is open on this tab")

// IsNoDialog reports whether err is ErrNoDialog, by type or by message — the
// daemon returns errors as strings (see IsZeroArea).
func IsNoDialog(err error) bool { return errIs(err, ErrNoDialog) }

// dialogUnwatchedNote is the honest answer when nothing was listening before this
// command started: Chrome does not replay a dialog that is already open, and a
// session that was not attached when it opened can neither see nor close it.
const dialogUnwatchedNote = "nothing was listening to this tab before this command started, so a dialog that opened earlier " +
    "is neither visible to nor closable by this command; close it in the Chrome window, or close the tab. " +
    "Use the daemon (drop --no-daemon) so dialogs are retained from the moment it attaches."
```

On `CDP` (declared in `cdp.go` beside `recMu` / `rec`, initialised in `newCDP`): `dialogMu sync.Mutex` and `dialogs map[string]dialogState`, keyed by target id.
Three small helpers — `dialogEvent(id string, ev any)` (apply one event: store on opening, delete on closed), `dialog(id) (dialogState, bool)` and `clearDialog(id)` — all under `dialogMu`.
The state is not an `eventbuf.Set`: there is at most one open dialog per tab, the next opening replaces it, and a closed one is gone; a ring would be the wrong shape.
Memory is one small struct per attached tab at most, and a tab the daemon attached keeps its `tabConn` for the daemon's life already.

```go
// listenDialog retains the dialog that is open on a tab. It is registered from
// listenCapture, BEFORE the attach and on the long-lived tab context, for the
// same reason listenConsole is: the process holding the connection has to
// already be listening when the dialog opens, because Chrome tells nobody later.
// It runs on chromedp's event loop and only writes the map; it never issues a
// CDP command.
func (c *CDP) listenDialog(tctx context.Context, id string) {
    chromedp.ListenTarget(tctx, func(ev any) { c.dialogEvent(id, ev) })
}
```

`listenCapture` gains the call; `enableCapture` gains `page.Enable()` beside `consoleEnable()` / `netEnable()` — chromedp's attach already enables `Page` (it is in its `run` sequence), so this is the same idempotent belt `enableCapture` already documents, and `withDialog` re-enables `Page` per action for the same reason.
`dialogEvent` takes the event as `any` so a pure test can feed it synthetic `*page.EventJavascriptDialogOpening` / `*page.EventJavascriptDialogClosed` values with no Chrome.

`--on-dialog` keeps working unchanged, and composes: its `withDialog` listener and `listenDialog` both receive the opening event; `withDialog` handles it from a goroutine; the `closed` event clears the retained state.
A `dialog status` that lands in those few milliseconds says `open: true`, which was true.

### `DialogStatus`: zero CDP traffic on an attached tab

```go
// Browser interface, after NetWait.

// DialogStatus reports the native JavaScript dialog (alert / confirm / prompt /
// beforeunload) open on a tab, from the opening event this connection retained
// (RFC-0018). It never talks to the renderer, which is blocked for as long as
// the dialog is up; on a tab this connection had not attached it attaches (and
// so starts retaining) and says in `note` that it could not have seen an
// earlier dialog.
DialogStatus(ctx context.Context, targetID string) (map[string]any, error)
// DialogHandle closes that dialog — accept (OK / confirm true / prompt text /
// leave the page) or dismiss — with Page.handleJavaScriptDialog, the one command
// that works while the renderer is blocked. text answers a prompt and is
// ignored for the other types. ErrNoDialog when nothing is retained as open:
// the command is NOT issued blind, because to a session that did not see the
// dialog open it hangs instead of failing.
DialogHandle(ctx context.Context, targetID string, accept bool, text string) (map[string]any, error)
```

`DialogStatus`:

1. `fresh := !c.attached(id)` — asked before attaching, exactly as `Console` does, because afterwards every tab looks like it was always watched.
2. `c.run(ctx, id)` with no actions — the attach if needed, and a no-op otherwise: on an already-attached tab `chromedp.Run` with an empty task list issues nothing.
3. Read `c.dialog(id)`; build the result per the field table; add `note` when `fresh`.
   The map is built by a pure `dialogStatusResult(st dialogState, ok, fresh bool) map[string]any`, so its shape is pinned by a test that needs no Chrome.

Target resolution before it is browser-level (`List` is `chromedp.Targets(c.base)`, the class of call that answered while the renderer was blocked), so the whole command works on a blocked tab.
The headline property: **on an attached tab, `dialog status` sends no CDP message at all.**

`DialogHandle`:

1. `fresh` and `c.run(ctx, id)` as above.
2. `st, ok := c.dialog(id)`; if `!ok`, return `ErrNoDialog` — wrapped as `fmt.Errorf("%w; %s", ErrNoDialog, dialogUnwatchedNote)` when `fresh`, so the message explains itself after crossing the daemon as a string and `IsNoDialog` still matches.
3. `p := page.HandleJavaScriptDialog(accept)`; `if text != "" && st.Type == "prompt" { p = p.WithPromptText(text) }`; `c.run(ctx, id, p)`.
   The text is not sent for the other types: Chrome echoes whatever `promptText` it was given in the `closed` event's `userInput` even for a `confirm`, and a retained `text_ignored: true` is a clearer record than a misleading echo.
4. An error whose message contains `No dialog is showing` means the dialog closed between the retained event and now (the `closed` event is in flight, or was missed): clear the retained entry and return `ErrNoDialog`, so the race is benign and self-healing.
   Any other error is returned as is.
5. On success, `c.clearDialog(id)` eagerly (the `closed` event will also do it), then the result per the field table.

`Page.handleJavaScriptDialog` is sent on the tab context through `c.run` like every other action, under the caller's deadline.

### The daemon: forwarders, dispatch, and why these two calls bypass the dispatch mutex

`remoteBrowser.DialogStatus` / `DialogHandle` forward with `r.c.call(ctx, "DialogStatus", &out, id)` and `r.c.call(ctx, "DialogHandle", &out, id, accept, text)`; `dispatch` gains `case "DialogStatus": return b.DialogStatus(ctx, argStr(args, 0))` and `case "DialogHandle": return b.DialogHandle(ctx, argStr(args, 0), argBool(args, 1), argStr(args, 2))`.
No options struct, so no new `argXxx` decoder; `argStr` and `argBool` cover both.
`TestDispatchCoversBrowser` stays green with the two cases and fails without them.

Unary calls are serialised by `s.mu` in `handle`, so that multi-step chromedp action sequences on one connection do not interleave.
The verb this RFC exists for is run **while another call is blocked**: an unguarded `click` whose `Input.dispatchMouseEvent` is waiting on the dialog holds that mutex for up to its deadline (30 s by default), and a `dialog accept` that queues behind it cannot unblock it.
So `handle` dispatches `DialogStatus` and `DialogHandle` **without taking `s.mu`**, through a `bypassesDispatchMutex(method)` predicate kept next to `isStreamMethod`, the way streams and `__stop` already run outside it.
It is safe for the same reasons the stream path is: `DialogStatus` touches no CDP and reads a map under its own mutex; `DialogHandle` issues exactly one browser-side command that *is* the unblocker, which `withDialog` already issues from a goroutine concurrent with a running action, and chromedp targets are safe for concurrent use; the attach either may trigger is serialised by `c.mu`.
The blocked `click` then completes normally — its `mouseReleased` returns once the handler returns — and reports success within its own deadline.
`TestDialogHandleRespondsWhileBusy` (the `gateBrowser` pattern of `TestStopRespondsWhileBusy`) pins it.

### The CLI: `internal/cli/dialog.go`

`cmdDialog()` returns a `dialog` command with `status` (`cobra.NoArgs`), `accept [text]` (`cobra.MaximumNArgs(1)`) and `dismiss` (`cobra.NoArgs`), registered in `newRoot`'s `root.AddCommand` list in `commands.go`.
Each runs its own resolve-call-emit sequence (as `console` and `screenshot` do, rather than `targetAction`) because its error mapping is its own and it must not inherit `--wait-text`:

```go
func classifyDialogErr(err error) (string, string, map[string]any) {
    if chrome.IsNoDialog(err) {
        return result.CodeTargetNotFound, err.Error(), map[string]any{"dialog": "none"}
    }
    return classifyActionErr(err), err.Error(), nil
}
```

The envelope's `command` is `"dialog"` for all three, as `cookie`, `record` and `window` report their family name.
Cobra's arity checks run before `resolveTarget`, so a wrong arg count never touches Chrome (`noCallBrowser` proves it).
Inside `session` the three are ordinary argv lines; nothing to refuse.

### Policy, MCP, docs, skill

- **Policy** (`internal/policy/policy.go`, same commit as the verb): `"dialog status": Reading`, `"dialog accept": Mutating`, `"dialog dismiss": Mutating`.
  Status observes; accept and dismiss change what the page's script sees next (`confirm()`'s return value is page state), and `beforeunload` accept navigates.
  `TestEveryCommandIsClassified` enforces the rows; the policy section of `docs/cli-reference.md` lists `dialog accept/dismiss` under Acting and `dialog status` under Reading.
- **MCP** (`internal/mcp/tools.go`): **no new tool** — the default set is at RFC-0004's cap of 18 and the "≤ 18" comment stays true.
  The verb is folded into the `tabs` tool, whose actions already mix tab lifecycle with window state: new actions `dialog_status` → `dialog status`, `dialog_accept` → `dialog accept`, `dialog_dismiss` → `dialog dismiss`, one new string argument `text` (allowed only with `dialog_accept`, passed as the positional), and the three verbs added to the tool's `verbs` list.
  Under `--read-only`, `allowedActions` keeps `dialog_status` and drops the other two from the classification table with no further code, exactly as it keeps `list` and drops `open`.
  The tool description gains one sentence naming the three actions as the remedy when a call reports `target_timeout` after a native dialog.
  An MCP agent is the caller most likely to hit a wedged `confirm()` and least able to reach a terminal, which is why this is exposed rather than left to `raw_cdp`.
- **Docs**: a `### Native dialogs` subsection in `docs/cli-reference.md` under Commands, after Waiting — the three subcommands, the per-type table, the envelope fields, the exit codes, the `--no-daemon` / `session` caveat — and a cross-reference from the `--on-dialog` row.
- **Skill** (`skills/drive-chrome-cdp/references/core.md`): the "Avoid native dialogs" bullet becomes "Guard against native dialogs (`--on-dialog`) and recover with `dialog status` / `dialog accept` / `dialog dismiss` when a command times out behind one"; the `--on-dialog` paragraph gains the recovery sentence.

### When the connection was not listening: `--no-daemon`, fresh tabs, `session`

With the daemon, a tab is watched from the first command that touches it; the typical flow (navigate, click, time out, `dialog accept`) is watched throughout.
The first command on a tab the daemon has never touched cannot know about an earlier dialog, and says so once (`note`); from then on the tab is watched.

With `--no-daemon`, every invocation is a fresh attach, so `dialog status` always answers `open: false` with the note and `dialog accept` / `dismiss` always refuse with `target_not_found` — unless the commands run inside one `session`, where the connection lives across lines and a dialog opened by an earlier line is retained for a later one.
The `note` text names the daemon as the fix, as `partialHistoryNote` does.
A fresh attach to a tab whose renderer is blocked may itself block until the caller's deadline (observed both ways, above); that is `target_timeout` from `c.run`, and it is a property of attaching to such a tab that every verb shares, not of this one.

### Stub

`chrometest.StubBrowser` gains permissive defaults like every other method: `DialogStatus` returns `{"open": false}`, `DialogHandle` returns `{"handled": true, "action": "accept"|"dismiss" (from the bool), "type": "confirm", "message": "stub"}`.
The brief sketched `{handled: false}`; a permissive default is success, because a stub whose default is a failure shape makes every test that does not care about dialogs look like the none case, and the none case is an error (`ErrNoDialog`) that a test overrides to produce.

## Verification scenarios

**VS-1 — Status reports an open confirm on a watched tab, without touching the renderer.**
Given an attached tab on which `eval "String(confirm('Sure?'))"` is blocked in a goroutine
When `DialogStatus` runs
Then it returns within one second with `open: true`, `type: "confirm"`, `message: "Sure?"`, `default_prompt: ""`, `frame_url` equal to the page URL, and `opened_at` within the test's wall-clock window.

**VS-2 — Accept unblocks and the page sees `true`.**
Given VS-1
When `DialogHandle(accept=true, text="")` runs
Then it returns `handled: true`, `action: "accept"`, `type: "confirm"`; the goroutine's `eval` returns `"true"`; a following `DialogStatus` is `open: false` with no `note`.

**VS-3 — Dismiss gives `false`.**
Given a blocked `confirm()` as in VS-1
When `DialogHandle(accept=false, text="")` runs
Then `eval` returns `"false"` and the result has `action: "dismiss"`.

**VS-4 — Prompt text round-trips; the default is not implied.**
Given a blocked `String(prompt('Name?', 'dflt'))`
When `DialogHandle(true, "bob")` runs, then (on a second prompt) `DialogHandle(true, "")`
Then `status` before each shows `type: "prompt"`, `default_prompt: "dflt"`; `eval` returns `"bob"` and then `""`; the results carry `text: "bob"` and `text: ""`.

**VS-5 — Text on a confirm is not sent and is reported.**
Given a blocked `confirm()`
When `DialogHandle(true, "ignored")` runs
Then `eval` returns `"true"`, the result has `text_ignored: true` and no `text` key.

**VS-6 — Nothing open on a watched tab.**
Given an attached tab with no dialog
When `DialogStatus` and then `DialogHandle(true, "")` run
Then status is `{open: false}` with no `note`, and handle returns `ErrNoDialog` without issuing `Page.handleJavaScriptDialog` (it returns at once, not after a deadline).

**VS-7 — A tab never touched before carries the note once.**
Given a fresh target created at the browser level and never attached by this `CDP`
When `DialogStatus` runs twice
Then the first result is `{open: false, note: dialogUnwatchedNote}` and the second `{open: false}` with no `note`.

**VS-8 — `--on-dialog` still works and leaves nothing retained.**
Given the existing `TestOnDialog` flow
When `click --on-dialog accept` handles a confirm
Then the click reports `dialogs` as before, and `DialogStatus` afterwards is `open: false`.

**VS-9 — The listener retains the last opening and clears on close (pure).**
Given synthetic events fed to `dialogEvent`: opening A, opening B, closed
When read after each
Then the state is A, then B (replaced, not queued), then absent; a `closed` with nothing retained is a no-op; the state is per tab id.

**VS-10 — Envelope shapes (stub-backed).**
Given a stub whose `DialogStatus` returns the full open map, then `{open: false}`
When `dialog status` runs each time
Then the first envelope's `result` carries the six keys, the second exits 0 with `open: false` and no `type`.
Given a stub recording its `DialogHandle` arguments
When `dialog accept "bob"` and `dialog dismiss` run
Then the stub saw `(true, "bob")` and `(false, "")`, and `result.action` is `accept` then `dismiss`.

**VS-11 — No dialog is `target_not_found` (stub-backed).**
Given a stub whose `DialogHandle` returns `chrome.ErrNoDialog` (once bare, once wrapped with the note the way the daemon would deliver it as a string)
When `dialog accept` runs
Then `error.code == "target_not_found"`, exit 4, `error.details.dialog == "none"`, and the message is the error's text.

**VS-12 — Arity errors never connect (stub-backed).**
Table, on `noCallBrowser`: `dialog status x`, `dialog dismiss x`, `dialog accept a b` → `usage` exit 2.

**VS-13 — Inside `session`.**
Given a `session` stdin with `dialog status` and `dialog accept`
When it runs
Then exactly one envelope line per command.

**VS-14 — Daemon: dispatch and RPC.**
`TestDispatchCoversBrowser` passes with the two cases.
`accept` and `text` cross the RPC intact (`DialogHandle` through `Remote` against a recording stub sees `(true, "bob")`).
With a `gateBrowser` holding the dispatch mutex in a blocked `Eval`, `Remote(...).DialogHandle` returns within one second.

**VS-15 — MCP.**
`tabs` with `action: "dialog_status"` builds `dialog status`; `dialog_accept` with `text: "bob"` builds `dialog accept bob`; `dialog_dismiss` builds `dialog dismiss`; `text` with `dialog_status` is a `usage` refusal.
Under `--read-only`, `allowedActions(tabs)` contains `dialog_status` and neither `dialog_accept` nor `dialog_dismiss`.
The registry still has 19 tools, 18 without `raw_cdp`.

**VS-16 — Policy.**
`policy.Classify("dialog status")` is `Reading`; `"dialog accept"` and `"dialog dismiss"` are `Mutating`; `TestEveryCommandIsClassified` passes.

## Test plan

**Pure (`t.Parallel()`), `internal/chrome/dialog_test.go` (the file exists; these sit beside `TestOnDialog`).**
`TestDialogEventRetainsLastAndClearsOnClose` (VS-9), `TestDialogStatusResultKeys` (the field table: the open map has the six keys, the closed map only `open`, `note` only when fresh — over the small pure result builder `dialogStatusResult(st, ok, fresh)`).

**Stub-backed, `internal/cli/dialog_test.go`.**
`TestDialogStatusEnvelope` (VS-10 status half), `TestDialogHandlePassesActionAndText` (VS-10 handle half), `TestDialogHandleNoDialogIsTargetNotFound` (VS-11), `TestDialogArityNeverConnects` (VS-12, `noCall(t)`), `TestDialogInsideSessionIsOneEnvelopePerLine` (VS-13).

**Daemon, `internal/daemon/daemon_test.go`.**
`TestDialogHandleArgsCrossTheRPC` and `TestDialogHandleRespondsWhileBusy` (VS-14); `TestDispatchCoversBrowser` unchanged.

**MCP, `internal/mcp/tools_test.go` / `server_test.go`.**
`TestTabsToolBuildsDialogVerbs`, `TestReadOnlyKeepsDialogStatusOnly` (VS-15).

**Policy, `internal/cli/policy_test.go`.**
`TestEveryCommandIsClassified` (VS-16), unchanged.

**Live Chrome (`internal/chrome/dialog_test.go`, `testing.Short()`-guarded, not parallel, driven from a `data:` / `httptest` fixture).**
`TestDialogStatusAndHandleWhileBlocked` (VS-1, VS-2, VS-3, VS-5: `b.Eval` in a goroutine — `eval` itself blocks until the dialog is handled, so it must not run on the test goroutine — then poll `DialogStatus` at 50 ms up to 5 s until `open`), `TestDialogPromptText` (VS-4), `TestDialogNothingOpen` (VS-6), `TestDialogFreshTabCarriesNote` (VS-7, a target created through `Open` and never otherwise touched), and one assertion added to `TestOnDialog` (VS-8).
The pre-existing-dialog-on-a-never-watched-tab case (a dialog opened with no `Page`-enabled session at all) is established by the probe described in *Design notes* and is not reproduced in the suite: arranging it needs a detached session and a timer, and the design's answer to it — `open: false` plus the note, and `ErrNoDialog` without a blind handle — is covered by VS-6 and VS-7.

## Out of scope

- Waiting for a dialog to open (`wait --dialog`); `dialog status` in a poll, or `--on-dialog` on the action, covers it.
- A retained history of closed dialogs; only the open one is kept, and `--on-dialog`'s `dialogs` report covers the per-action case.
- Reporting the dialog's `frame_id`, or which element's handler opened it.
- Putting `--on-dialog` on `nav`, `open`, `close` or `key` (the `beforeunload` case is served by `dialog accept` after the fact).
- The OS file chooser and Chrome's own browser-modal prompts (RFC-0013): not JavaScript dialogs, not reachable with `Page.handleJavaScriptDialog`.
- Bounding the attach (`c.on`) by the caller's deadline; a pre-existing property of every verb, noted above, and a separate change if it is ever wanted.
- A `dialog` MCP tool of its own.

## Open questions

None.
