# RFC-0007: Tab lifecycle — `close`, `activate`, and history navigation

- **Status:** Draft
- **Priority:** P1
- **Area:** tabs
- **Depends on:** —
- **Mitigates:** the `tab_hidden` limitation inherited by every accessibility-backed verb

## Summary

Complete the tab surface with three additions:

- `close` — close a tab (or several, by filter).
- `activate` — bring a tab to the foreground, which is the documented fix for the `tab_hidden` throttling that breaks `--by name` / `ref` / `cell`.
- `nav --back` / `--forward` / `--reload` — history navigation and reload.

## Motivation

Tab handling today is `list`, `open`, `use`, `nav <url>`.
Three things are missing, and one of them is not merely missing — it is the documented workaround for an existing failure mode that has no command.

**`activate` is the important one.**
`cli-reference.md` states that `--by name`, `ref`, and `cell` read the accessibility tree, which Chrome throttles on a tab it cannot foreground, and that a timeout there returns `tab_hidden: true` "so you know to foreground Chrome."
The CLI tells the user to do something it does not offer a way to do.
Every automation that hits this must be resolved by a human switching tabs by hand — which defeats the point of an unattended run, and makes the failure unrecoverable for an agent.
One verb turns a dead end into a one-line retry, and lets the Claude skill and MCP layer auto-recover on `tab_hidden`.

**`close` is missing entirely.**
Any automation that opens tabs leaks them for the life of the browser session — and since this drives the user's *real* Chrome, that debris accumulates in the window they actually work in.
`open` without `close` is an asymmetric API.

**History navigation is missing.**
Multi-step wizards, search-result drill-downs, and OAuth redirect flows all need "go back".
Re-navigating by URL is not equivalent: it loses form state, discards SPA history, and for POST-result pages is simply wrong.
`--reload` is likewise not the same as navigating to the current URL — reload preserves scroll and history position, and `--hard` bypasses cache.

## User stories

**US-1 — Recover from a throttled accessibility tree.**
As an automation author, I want to foreground the tab I am driving so that `--by name` stops timing out with `tab_hidden`.
*Acceptance:* after `chrome-cdp activate`, a `snap` that previously failed with `tab_hidden: true` succeeds.

**US-2 — Auto-recover in a script.**
As an agent, I want to detect `tab_hidden` and fix it myself so that I do not have to ask the user to switch tabs.
*Acceptance:* on exit 4 with `details.tab_hidden`, running `activate` and retrying the same command succeeds, and this loop is documented in the CLI reference.

**US-3 — Clean up after myself.**
As an automation author, I want to close a tab I opened so that a long run does not leave dozens of tabs in the user's browser.
*Acceptance:* `chrome-cdp close <id>` closes exactly that tab and `list` no longer shows it.

**US-4 — Bulk cleanup.**
As an automation author, I want to close every tab matching a filter so that I can tidy up after a batch job.
*Acceptance:* `chrome-cdp close --url "report.internal" --all` closes only matching tabs and reports how many.

**US-5 — Go back in a wizard.**
As an automation author, I want to go back one step so that I can correct an earlier field without restarting the flow.
*Acceptance:* `chrome-cdp nav --back` returns to the previous page with its state, and the envelope reports the settled URL.

**US-6 — Reload after a change.**
As a developer, I want to reload the page, optionally bypassing cache, so that I can see my rebuilt assets.
*Acceptance:* `chrome-cdp nav --reload --hard` refetches rather than serving from cache, verifiable via `net` (RFC-0003).

**US-7 — Not lose my sticky target.**
As a user, I want closing a tab that is not my sticky target to leave the sticky target alone, and closing the sticky target itself to fail loudly rather than leave me pointing at nothing.
*Acceptance:* closing the sticky tab clears the sticky state and the envelope says so; subsequent commands fail with `no_current_target`, not an obscure error.

## Proposed CLI surface

```
chrome-cdp close [<target>] [--url <s>] [--title <s>] [--all]
chrome-cdp activate [<target>]
chrome-cdp nav (--back | --forward | --reload [--hard] | <url>)
```

| Flag | Applies to | Purpose |
|------|-----------|---------|
| `--url <s>` / `--title <s>` | `close` | substring filters, matching `list`'s existing filter flags |
| `--all` | `close` | close every match; without it, more than one match is an error |
| `--back` / `--forward` | `nav` | history navigation |
| `--reload` | `nav` | reload the current page |
| `--hard` | `nav --reload` | bypass cache |

`close` and `activate` with no argument act on the sticky target, consistent with every other verb.
`nav` continues to accept a URL positionally; the new flags are mutually exclusive with it and with each other.

Examples:

```sh
chrome-cdp activate                              # foreground the sticky tab
chrome-cdp close @3                              # close the third tab
chrome-cdp close --url "staging.internal" --all  # bulk cleanup
chrome-cdp nav --back
chrome-cdp nav --reload --hard
```

## Result envelope

`activate`:

```json
{ "ok": true, "command": "activate",
  "target": {"id":"…","title":"…","url":"…"},
  "result": { "activated": true, "was_active": false, "window_focused": true },
  "elapsed_ms": 31 }
```

`close`:

```json
{ "ok": true, "command": "close",
  "result": { "closed": [{"id":"…","url":"…","title":"…"}], "count": 1,
              "sticky_cleared": false },
  "elapsed_ms": 18 }
```

`close` deliberately omits the usual `target` object when `--all` is used — the envelope's `target` describes one tab, and a bulk close has none.
`closed` carries the list instead.

`nav --back/--forward/--reload` reuses `nav`'s existing shape and reports the **settled** URL, consistent with the existing behaviour where a redirect updates `target.url`.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| More than one `--back`/`--forward`/`--reload`/URL given; `--hard` without `--reload` | `usage` | 2 |
| Multiple tabs match `close` filters without `--all` | `ambiguous_target` | 4 |
| No tab matches | `target_not_found` | 4 |
| No sticky target and none given | `no_current_target` | 4 |
| Back/forward with no history entry in that direction | `target_not_found` with `no_history: true` | 4 |
| `Target.closeTarget` / `Page.bringToFront` rejected | `cdp_error` | 5 |

No new codes.
Back/forward with no entry is deliberately an error rather than a silent no-op — a wizard script that silently fails to go back would proceed against the wrong page.

## Design notes

- **Interface:** ```go Close(ctx context.Context, targetIDs []string) (map[string]any, error) Activate(ctx context.Context, targetID string) (map[string]any, error) History(ctx context.Context, targetID string, delta int) (map[string]any, error) Reload(ctx context.Context, targetID string, hard bool) (map[string]any, error) ``` `History` takes a delta rather than a boolean so `--back 2` is a later flag addition, not a signature change.
- **`activate` needs two things, not one.**
  `Page.bringToFront` foregrounds the tab within its window; it does **not** raise that window above other applications.
  On macOS in particular, a backgrounded *window* still throttles.
  The verb should attempt both — `Page.bringToFront` plus `Browser.getWindowForTarget` + `Browser.setWindowBounds` with `windowState: "normal"` — and report `window_focused` honestly, including `false` when the OS refused.
  Overpromising here would make US-1 flaky in exactly the environments it exists to fix.
- **`was_active`** lets a retry loop tell "I fixed it" from "it was already foreground, so `tab_hidden` has another cause" — which matters, because a genuinely offscreen or minimized window may need user action the CLI cannot take.
- **Closing the sticky target must clear it.**
  Otherwise every subsequent command fails against a dead id with a confusing CDP error instead of the `no_current_target` the state machine already models (US-7).
  The `internal/state` sticky-target store is the place for this, and the envelope reports `sticky_cleared: true` so a caller knows to `use` a new tab.
- **`close` without `--all` on multiple matches is an error, not a "close the first".**
  Destructive operations should not guess.
  This mirrors how `--by name` already treats ambiguity.
- **History navigation** uses `Page.getNavigationHistory` to determine whether an entry exists in the requested direction *before* navigating, so `no_history` is a clean typed error rather than a timeout.
- **Reload with `--hard`** maps to `Page.reload{ignoreCache: true}`.
- **Settle behaviour** matches `nav`'s existing semantics — wait for load — so `--back` composes with `wait --idle` the same way.
- **Stub defaults:** `Close` → `{"closed": []any{}, "count": 0}`; `Activate` → `{"activated": true, "was_active": false}`; `Reload` → `{"url": "https://example.com/", "status": 200}`; `History` → `{"url": "https://example.com/"}` (a history move has no HTTP response of its own, so it reports no `status`).

## Verification scenarios

**VS-1 — Activate foregrounds and unblocks the a11y tree** Given two tabs where the driven one is backgrounded and `snap --by name` fails with `tab_hidden: true` When `activate` runs and the snap is retried Then the retry succeeds.

**VS-2 — `was_active` reports the truth** Given the sticky tab is already foreground When `activate` runs Then `was_active` is true and the envelope is `ok`.

**VS-3 — Close removes exactly one tab** Given three open tabs When `close @2` runs Then `list` returns two tabs and the closed id is absent.

**VS-4 — Bulk close by filter** Given four tabs, two matching a URL substring When `close --url <substr> --all` runs Then `count == 2`, the two remain that did not match, and `closed` lists exactly the two.

**VS-5 — Ambiguous close without `--all`** Given two matching tabs When `close --url <substr>` runs without `--all` Then exit 4 with `ambiguous_target` and **no tab is closed** — assert the tab count is unchanged, not just the exit code.

**VS-6 — No match** When `close --url "nothing-matches"` runs Then exit 4 with `target_not_found`.

**VS-7 — Closing the sticky target clears it** Given the sticky target is tab A When `close` runs with no argument Then `sticky_cleared` is true, and a following `snap` exits 4 with `no_current_target`.

**VS-8 — Back restores the previous page** Given a tab navigated to page 1 then page 2 When `nav --back` runs Then the settled URL in the envelope is page 1's.

**VS-9 — Forward after back** Continuing VS-8, when `nav --forward` runs Then the settled URL is page 2's.

**VS-10 — No history is a typed error** Given a freshly opened tab with no prior entry When `nav --back` runs Then exit 4 with `target_not_found` and `no_history: true`.

**VS-11 — Reload preserves the URL, hard reload refetches** Given a page whose asset is cacheable When `nav --reload` then `nav --reload --hard` run Then the URL is unchanged in both, and (with RFC-0003) the hard reload shows a non-cached fetch while the soft one may show `from_cache: true`.

**VS-12 — Mutually exclusive nav flags** Table: `--back` alone → ok; URL alone → ok; `--back` with a URL → `usage` 2; `--back --forward` → `usage` 2; `--hard` without `--reload` → `usage` 2.
No browser call in the failing cases.

**VS-13 — Closing the last tab** Given exactly one tab When `close` runs Then either the browser exits cleanly or a blank tab remains, and the CLI reports the outcome without hanging — assert a bounded runtime, not a specific browser behaviour.

## Test plan

**Unit — flag validation (`internal/cli`, stub failing on any browser call, `t.Parallel()`)** VS-12 as a table; `--all` interaction with filters; positional target plus filters together.

**Unit — target resolution and sticky-state transitions (`internal/cli` + `internal/state`, `chrometest.StubBrowser`)** VS-5, VS-6, VS-7.
VS-5's "no tab closed" assertion is the valuable half: use a stub that records calls and assert `Close` was never invoked.
Sticky-state clearing should also be tested directly against `internal/state` with `t.TempDir()`, since it persists.

**Live Chrome (`internal/chrome`, `testing.Short()`-guarded, not parallel)** VS-1 through VS-4, VS-8 through VS-11, VS-13, driving `data:` fixture pages and a local `httptest` server for the cache behaviour in VS-11.
VS-1 is the flagship test and the most environment-sensitive: it must skip gracefully rather than fail when the CI environment cannot background a window (headless CI may never throttle).
Guard it explicitly and document why, rather than letting it be flaky.

**Documentation test-of-record** The `tab_hidden` → `activate` → retry loop is added to `cli-reference.md` and to the `drive-chrome-cdp` skill in the same PR.
An RFC that fixes a documented dead end should also fix the documentation that named it.

## Out of scope

- Creating, closing, moving, or focusing browser *windows* (as opposed to tabs).
- Tab groups, pinning, and muting.
- Restoring recently closed tabs.
- Navigating a specific iframe's history.

## Open questions

1. Should `activate` be attempted automatically when a verb fails with `tab_hidden`, behind an opt-in flag like `--auto-activate`?
   It would make US-2 free rather than a documented loop.
   **Recommendation:** yes as a config key defaulting to off — implicit foregrounding steals the user's attention mid-run, which is exactly the kind of surprise a tool driving your real browser should not spring on you.
2. Should `close` require `--all` even for a single matching filter, on the grounds that destructive plus fuzzy is a bad combination?
   **Recommendation:** no; an unambiguous single match is precise enough, and the ambiguity guard in VS-5 is the real protection.
3. Should `nav --back` accept a count (`--back 2`)?
   **Recommendation:** not in this RFC, but the `History(delta int)` signature above leaves room so it costs nothing later.
