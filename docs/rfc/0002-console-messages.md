# RFC-0002: Console messages — the `console` verb

- **Status:** Accepted — implemented in [#13](https://github.com/sanketsudake/chrome-cdp-cli/pull/13), with follow-up fixes in [#16](https://github.com/sanketsudake/chrome-cdp-cli/pull/16)
- **Priority:** P0
- **Area:** observability
- **Depends on:** —
- **Unblocks:** RFC-0003 (shares the event-buffer design decided here)

## Summary

Add a `console` verb that reads JavaScript console output and uncaught exceptions from the target tab, with server-side filtering (`--grep`, `--only-errors`, `--since`), a `--clear` option, and a `--follow` streaming mode.
Introduce the daemon-side CDP **event buffer** this requires — the first piece of infrastructure in `chrome-cdp` that listens to Chrome rather than asking it questions.

## Motivation

`chrome-cdp` can drive a page but cannot tell you what the page said.
There is no console access at any level, and no workaround:

- `eval` can read variables, but console output is not retained anywhere in the DOM.
- Monkey-patching `console.log` from `eval` only catches calls made *after* the patch, misses uncaught exceptions entirely, and mutates the page under test.
- `raw` issues one CDP command and returns one response.
  It cannot subscribe to `Runtime.consoleAPICalled` or `Runtime.exceptionThrown`, so the escape hatch does not reach this either.

This is a structural gap, not a missing convenience.
It also closes off the single most common reason a developer picks up a browser automation tool: *"the page is broken, tell me why."*
Today an automation run that fails because the app threw a TypeError reports only that a selector timed out.

Adding this makes `chrome-cdp` useful to a second audience — web developers debugging their own app — beyond the agents and scripters it serves now.

## User stories

**US-1 — Explain a failed automation step.**
As an automation author, I want to read the errors the page logged so that when a `click` times out I learn the app threw, instead of guessing at my selector.
*Acceptance:* after a failing step, `chrome-cdp console --only-errors` lists the exception with its message and stack.

**US-2 — Debug my own app from the terminal.**
As a web developer, I want to watch console output while I exercise the page so that I do not have to keep DevTools open and hand-copy messages.
*Acceptance:* `chrome-cdp console --follow --grep "\\[MyApp\\]"` streams matching lines until interrupted.

**US-3 — Assert a clean console in CI.**
As a CI author, I want a nonzero exit when the page logged any error so that a regression that only shows up in the console still fails the build.
*Acceptance:* `chrome-cdp console --only-errors --fail-on-match` exits nonzero when at least one error is buffered.

**US-4 — Scope output to one action.**
As an agent, I want to clear the buffer, act, then read, so that the messages I see belong to the step I just performed and not the whole session.
*Acceptance:* `console --clear` then `click` then `console` returns only the messages produced by the click.

**US-5 — Keep output small.**
As an agent with a context budget, I want to filter server-side so that a chatty app does not flood my context with framework noise.
*Acceptance:* `--grep`, `--level`, and `--limit` are applied before the envelope is built, not after.

**US-6 — Not pay for it when I do not use it.**
As a user driving a long automation, I do not want an unbounded buffer to grow in the daemon so that a day-long session does not leak memory.
*Acceptance:* the buffer is bounded and documented; oldest entries are dropped first and the envelope reports that it happened.

## Proposed CLI surface

```
chrome-cdp console [--grep <re>] [--level <l>] [--only-errors] [--limit <n>]
                   [--since <dur>] [--clear] [--follow] [--fail-on-match]
```

| Flag | Default | Purpose |
|------|---------|---------|
| `--grep <re>` | — | keep only messages whose text matches this regex |
| `--level <l>` | all | `log` \| `info` \| `warn` \| `error` \| `debug`; repeatable |
| `--only-errors` | off | shorthand for `--level error` plus uncaught exceptions |
| `--limit <n>` | `100` | most recent *n* matching messages |
| `--since <dur>` | — | only messages newer than this (e.g. `30s`) |
| `--clear` | off | drop buffered messages after reading (or, with no other flag, just drop them) |
| `--follow` | off | stream new messages as NDJSON until interrupted |
| `--fail-on-match` | off | exit 1 if at least one message is returned |

Examples:

```sh
chrome-cdp console --only-errors                       # what broke
chrome-cdp console --grep "\\[Checkout\\]" --limit 20  # one subsystem
chrome-cdp console --clear                             # reset before an action
chrome-cdp console --follow --level error              # watch while you work
```

## Result envelope

```json
{ "ok": true, "command": "console",
  "target": {"id":"…","title":"…","url":"…"},
  "result": {
    "messages": [
      { "level": "error", "text": "TypeError: x.map is not a function",
        "url": "https://app/bundle.js", "line": 4210, "column": 17,
        "ts": "2026-07-26T11:04:02.481Z",
        "stack": ["render (bundle.js:4210:17)", "commit (bundle.js:881:3)"] }
    ],
    "count": 1, "buffered": 143, "dropped": 0, "truncated": false
  },
  "elapsed_ms": 3 }
```

- `count` — messages in this envelope after filtering.
- `buffered` — how many the daemon currently holds for this tab.
- `dropped` — how many were evicted by the ring buffer since the session started; nonzero means the caller is reading too late.
- `truncated` — `--limit` cut the result.

`--follow` emits one such object **per message** as NDJSON on stdout, matching how `session` already streams, so a caller parses one shape in both modes.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| Invalid `--grep` regex, unknown `--level`, bad `--since` | `usage` | 2 |
| `--follow` combined with `--fail-on-match` | `usage` | 2 |
| Daemon not reachable and `--no-daemon` given without a live connection | `connection_failed` | 3 |
| Event capture could not be enabled on the tab | `cdp_error` | 5 |
| Buffer unavailable because capture started after the messages were emitted | `ok` with `dropped`/`buffered` reporting | 0 |
| `--fail-on-match` and at least one message matched | `generic` | 1 |

**New `Code*` constant:** none strictly required, but `--fail-on-match` deliberately reuses `generic`/exit 1 rather than inventing a code, because it is an assertion result, not a tool failure.
If review prefers to distinguish it, add `CodeAssertFailed = "assertion_failed"` with its own `codeToExit` entry — the RFC's Open Questions cover this.

## Design notes

This RFC introduces the **CDP event buffer**, and that design decision matters more than the verb itself.

- **The buffer lives in the daemon.**
  The daemon already owns the connection and its lifetime; a per-command process cannot retain events it was not running to receive.
  Concretely: when the daemon attaches to a tab it enables `Runtime` and `Log` and retains `Runtime.consoleAPICalled`, `Runtime.exceptionThrown`, and `Log.entryAdded` into a per-target ring buffer.
- **Bounded by default.**
  Ring buffer of *N* entries per target (proposal: 1000) and a total cap across targets, with per-entry text truncation (proposal: 8 KB).
  Eviction increments `dropped`.
  Both bounds are config keys (`console_buffer`, `console_max_entry`).
- **Capture starts when the daemon attaches**, not when `console` is first called.
  Otherwise US-1 fails exactly when it matters — the error happened *before* anyone thought to look.
  This means capture cost is paid on every session; the ring buffer and truncation are what keep it acceptable.
- **`--no-daemon` degrades honestly.**
  Without a daemon there is no process alive to have received earlier events.
  In that mode `console` enables capture, and returns only what arrives during its own `--timeout` window, with `"buffered": 0` and a note in the envelope.
  It must not pretend to have history it never saw.
- **Interface:** add to `chrome.Browser`: ```go Console(ctx context.Context, targetID string, opts ConsoleOpts) (any, error) ConsoleStream(ctx context.Context, targetID string, opts ConsoleOpts, emit func(any) error) error ``` `ConsoleOpts{Grep string; Levels []string; Limit int; Since time.Duration; Clear bool}`.
- **Filtering is server-side** — the regex and level filter are applied in the daemon before the envelope is marshalled, so US-5 holds and a chatty app cannot blow up a caller's context.
- **Stub:** `chrometest.StubBrowser.Console` returns `{"messages": []any{}, "count": 0, "buffered": 0}`; `ConsoleStream` returns nil immediately.
- **`session` compatibility:** `console` (non-follow) works as an ordinary batch line.
  `--follow` inside `session` is a `usage` error — a streaming line would break the one-envelope-per-line contract.

## Verification scenarios

**VS-1 — Captures a log emitted before the read** Given a fixture page that calls `console.log("hello")` on load When the tab is opened and `console` is run afterwards Then the message appears with `level == "log"`.

**VS-2 — Captures an uncaught exception with a stack** Given a fixture that throws a `TypeError` from a nested function When `console --only-errors` runs Then one entry appears with the type name in `text` and a non-empty `stack`.

**VS-3 — Level filtering** Given a fixture emitting one each of `log`, `warn`, `error` When `console --level warn --level error` runs Then `count == 2` and no `log` entry is present.

**VS-4 — Grep is applied server-side** Given a fixture emitting 50 `[Noise]` lines and 2 `[App]` lines When `console --grep "\\[App\\]"` runs Then `count == 2` and `buffered == 52`.

**VS-5 — Clear scopes to one action** Given a page with existing buffered messages When `console --clear` runs, then a click that logs once, then `console` Then the second read returns exactly the one message.

**VS-6 — Ring buffer evicts oldest and reports it** Given `console_buffer = 10` and a fixture emitting 25 messages When `console --limit 100` runs Then `count == 10`, `dropped == 15`, and the retained messages are the newest.

**VS-7 — Follow streams NDJSON** Given `console --follow` running with a short `--timeout` When the page logs three messages Then three NDJSON objects are written to stdout in order and the command exits 0 at timeout.

**VS-8 — Invalid regex never connects** When `console --grep "("` runs Then exit is 2 with `usage` and no browser method is called.

**VS-9 — `--fail-on-match` exit contract** Given a page that logged one error When `console --only-errors --fail-on-match` runs Then exit is 1 and the envelope still lists the message (the assertion failing must not suppress the evidence).

**VS-10 — `--no-daemon` does not fabricate history** Given `--no-daemon` and a page that logged before the command started When `console` runs Then the pre-existing message is absent, `buffered` is 0, and the envelope carries the "no retained history" note rather than an error.

**VS-11 — Per-target isolation** Given two open tabs each logging distinct text When `console --target @1` runs Then only the first tab's messages are returned.

## Test plan

**Unit — flag validation (`internal/cli`, stub that fails on any browser call)** VS-8, bad `--level`, bad `--since`, `--follow` with `--fail-on-match`, `--follow` inside `session`.

**Unit — filtering and ring-buffer logic (`internal/daemon` or a new `internal/eventbuf`, `t.Parallel()`)** Test the buffer as a pure unit with synthetic events: eviction order, `dropped` accounting, per-entry truncation, level filter, regex filter, `--limit`, `--since`, `--clear`.
This is where VS-3, VS-4, VS-6 belong — no Chrome needed, and they are the cases most likely to regress.

**Unit — command boundary (`chrometest.StubBrowser`)** Envelope shape, `command == "console"`, `--fail-on-match` mapping to exit 1 while retaining `result.messages` (VS-9).

**Live Chrome (`internal/chrome`, `testing.Short()`-guarded)** VS-1, VS-2, VS-5, VS-10, VS-11 against `data:` fixtures.
VS-2 specifically must assert the stack is populated — that is the field most likely to be dropped by a marshalling mistake and the one users need most.

**Streaming (`internal/cli`)** VS-7 with a fake emitter driving `ConsoleStream`, asserting NDJSON framing and that each line is independently parseable.

**Config (`internal/config`)** `console_buffer` / `console_max_entry` precedence: flag > env > config > default, and a malformed value warns rather than being fatal, matching existing config behaviour.

## Out of scope

- Structured console arguments beyond a rendered text form (`console.log(obj)` is captured as its preview string, not as a live remote object graph).
- `console.table` / `console.group` fidelity.
- Log persistence across daemon restarts.
- Source-map resolution of stack frames.

## Open questions

1. Should `--fail-on-match` reuse `generic`/exit 1, or get a dedicated `assertion_failed` code?
   A dedicated code lets CI distinguish "the assertion tripped" from "the tool broke", which is worth something.
   **Recommendation:** add `CodeAssertFailed` with its own exit code, since RFC-0003 will want the same thing and defining it once here is cheaper than retrofitting.
2. Should capture be opt-out (`--no-capture` on the daemon) for users who care about the overhead?
   **Recommendation:** yes, a config key, default on.
3. Should `console` without `--follow` implicitly wait for at least one message when the buffer is empty, or return empty immediately?
   **Recommendation:** return immediately.
   Waiting is `wait`'s job, and a silent block would be surprising.
4. Do we buffer for *all* targets the daemon knows about, or only the sticky/most-recently-used one?
   Buffering everything is simpler to reason about but multiplies memory.
   **Recommendation:** buffer per attached target, with the total cap as the backstop.
