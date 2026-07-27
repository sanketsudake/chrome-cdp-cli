# RFC-0001: Keyboard input — the `key` verb

- **Status:** Accepted — implemented in [#10](https://github.com/sanketsudake/chrome-cdp-cli/pull/10)
- **Priority:** P0
- **Area:** input
- **Depends on:** —
- **Unblocks:** RFC-0009 (recipes), RFC-0004 (MCP)

## Summary

Add a `key` verb that dispatches keyboard events that are not literal text: named keys (`Escape`, `Tab`, `Enter`, `ArrowDown`, `F2`), modifier chords (`cmd+a`, `ctrl+shift+k`), and repeated presses.
It works with or without a selector — with one, it focuses that element first; without one, it sends to whatever currently has focus.

## Motivation

`type` sends literal text through `chromedp.KeyEvent`, which encodes runes.
That covers `\n` and `\t` embedded in a string, and nothing else.
Today there is no expressible way to:

- dismiss a modal, popover, or autocomplete dropdown (`Escape`);
- move focus between fields without knowing a selector for the next one (`Tab`, `shift+Tab`);
- navigate a combobox or menu that only responds to keyboard (`ArrowDown`, `Home`, `End`);
- select-all before retyping in an editor that ignores programmatic clearing (`cmd+a`);
- trigger an app shortcut (`ctrl+s`, `F2` to edit a grid cell, `/` to focus search).

Every one of these is routine on the internal web apps the CLI targets.
Their absence forces users into `eval` with hand-written `KeyboardEvent` dispatch, which many frameworks ignore because it is not trusted input.

This is the cheapest high-value addition in the RFC set: one interface method, one command, no new subsystem.

## User stories

**US-1 — Dismiss a blocking overlay.**
As an automation author, I want to press `Escape` so that a modal or autocomplete dropdown closes and the element underneath becomes clickable again.
*Acceptance:* `chrome-cdp key Escape` succeeds with no selector, and a subsequent `snap` no longer lists the dialog node.

**US-2 — Move focus without a selector.**
As an automation author, I want to press `Tab` so that I can walk a form whose next field has no stable selector.
*Acceptance:* `chrome-cdp key Tab` shifts `snap`'s reported `focused` node to the next tabbable element.

**US-3 — Replace content in a stubborn editor.**
As an automation author, I want to select all and retype so that I can set a value in a rich-text or masked input where `fill` leaves residue.
*Acceptance:* `chrome-cdp key --by name "Description" cmd+a` followed by `type` yields only the newly typed text.

**US-4 — Drive a keyboard-only widget.**
As an automation author, I want to press `ArrowDown` several times and then `Enter` so that I can choose from a listbox that does not respond to clicks on its options.
*Acceptance:* `chrome-cdp key --repeat 3 ArrowDown` then `key Enter` selects the 4th option, confirmed by `value`.

**US-5 — Trigger an application shortcut.**
As a power user, I want to send an app-defined chord so that I can invoke a command the UI only exposes via keyboard.
*Acceptance:* `chrome-cdp key ctrl+s` produces the app's save behaviour, observable via `--wait-text`.

**US-6 — Fail loudly on a bad key name.**
As an agent, I want an unknown key name to be a usage error before Chrome is contacted so that I can correct the call without a wasted round trip.
*Acceptance:* `chrome-cdp key Ecsape` exits 2 with `error.code == "usage"` and never opens a connection.

## Proposed CLI surface

```
chrome-cdp key [selector] <keyspec>
```

| Flag | Default | Purpose |
|------|---------|---------|
| `--repeat <n>` | `1` | press the sequence *n* times (1–100) |
| `--delay <dur>` | `0` | pause between repeats, for apps that debounce |
| `--wait-text <s>` | — | inherited act-and-confirm flag |
| all `QueryOpts` flags | — | `--by`, `--wait`, `--role`, `--nth`, `--match`, `--in-row`, `--pierce`, used only when a selector is given |

`<keyspec>` grammar:

- A single named key: `Escape`, `Tab`, `Enter`, `Backspace`, `Delete`, `Home`, `End`, `PageUp`, `PageDown`, `ArrowUp|Down|Left|Right`, `F1`–`F12`, `Space`.
- A single printable character: `a`, `/`, `7`.
- A chord: modifiers joined by `+`, then exactly one key — `cmd+a`, `ctrl+shift+k`, `alt+ArrowLeft`.
- A space-separated sequence of the above, run in order: `"End shift+Home Backspace"`.

Modifier names accepted: `ctrl`, `shift`, `alt`, `cmd` (aliases `meta`, `super`).
`cmd` maps to Meta on every platform; the CLI does not silently rewrite `cmd` to `ctrl`, because the *page* decides which it listens for, not the host OS.

Examples:

```sh
chrome-cdp key Escape                                  # close the open dialog
chrome-cdp key --repeat 3 ArrowDown                    # walk a listbox
chrome-cdp key --by name "Search" --role textbox "/"   # focus, then press
chrome-cdp key "End shift+Home Backspace"              # clear the focused field
chrome-cdp key ctrl+s --wait-text "Saved"              # shortcut, then confirm
```

## Result envelope

```json
{ "ok": true, "command": "key",
  "target": {"id":"…","title":"…","url":"…"},
  "result": { "keys": ["cmd+a"], "repeat": 1, "focused": "textbox \"Description\"" },
  "elapsed_ms": 14 }
```

`focused` reports the accessible description of the focused node *after* the press, so a caller can verify a `Tab` landed where expected without a second `snap`.
It is best-effort: on a backgrounded tab where the accessibility tree is throttled it is omitted rather than blocking.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| Unparseable keyspec, unknown key or modifier name | `usage` | 2 |
| `--repeat` outside 1–100 | `usage` | 2 |
| Selector given but never resolves | `target_timeout` | 4 |
| Selector ambiguous under `--by name` | `ambiguous_target` | 4 |
| `Input.dispatchKeyEvent` rejected by Chrome | `cdp_error` | 5 |

No new `Code*` constant is required; every case maps to an existing one.

## Design notes

- **Interface:** add one method to `chrome.Browser`: ```go Key(ctx context.Context, targetID, selector string, keys []KeyStroke, opts KeyOpts) (map[string]any, error) ``` with `KeyOpts{Repeat int; Delay time.Duration; Query QueryOpts}`.
- **Parsing lives in the CLI, not the driver.**
  `internal/cli` parses `<keyspec>` into `[]KeyStroke` and rejects bad input as `usage` *before* `resolveTarget` is called.
  This keeps the "validate args before connecting to Chrome" rule and makes the parser trivially unit-testable with no browser.
- **`KeyStroke` is explicit,** not a string: `{Key string; Code string; KeyCode int64; Modifiers int64; Text string}`.
  Chrome's `Input.dispatchKeyEvent` needs `windowsVirtualKeyCode` and `code` for many named keys to be honoured by frameworks; a table in `internal/chrome/keys.go` maps names to that tuple.
  `chromedp/kb` already carries most of it and should be reused rather than re-derived.
- **Focus:** when a selector is present, resolve and focus it with the existing `QueryOpts` machinery (the same path `type` uses), then dispatch.
  When absent, dispatch to the page's current focus without any element resolution — this is what makes `key Escape` work when nothing is addressable.
- **Chords** dispatch as `keyDown(modifier)…keyDown(key), keyUp(key), keyUp(modifier)…` with the `modifiers` bitmask set on every event, which is what pages listening for `event.metaKey` actually check.
- **Stub:** `chrometest.StubBrowser.Key` returns `{"keys": []any{}, "repeat": 1}`.
- **`session` compatibility** is automatic — `key` is an ordinary argv verb.

## Verification scenarios

Written as Given/When/Then so each maps to one test.

**VS-1 — Named key, no selector** Given a fixture page with a `<dialog open>` When `key Escape` runs Then the envelope is `ok`, and `snap` no longer reports the dialog.

**VS-2 — Focus movement** Given a fixture with three text inputs and focus on the first When `key Tab` runs Then `document.activeElement` is the second input.

**VS-3 — Chord reaches the page as a modifier** Given a fixture that records `e.key`, `e.metaKey`, `e.ctrlKey` into `window.__keys` When `key cmd+a` runs Then `eval "window.__keys"` shows one entry with `key == "a"` and `metaKey == true`.

**VS-4 — Repeat** Given a fixture that counts `keydown` events When `key --repeat 5 ArrowDown` runs Then the counter reads 5 and the envelope reports `"repeat": 5`.

**VS-5 — Sequence order** Given a text input containing `hello world` with focus at the end When `key "End shift+Home Backspace"` runs Then the input's value is empty.

**VS-6 — Selector focuses first** Given a fixture with two inputs, focus on the first When `key --by name "Second field" x` runs Then the second input's value is `x` and the first is unchanged.

**VS-7 — Bad keyspec never connects** Given no Chrome running at all When `key Ecsape` runs Then exit is 2, `error.code` is `usage`, and no connection attempt is made.

**VS-8 — Repeat bounds** When `key --repeat 0 Tab` or `--repeat 500 Tab` runs Then exit is 2 with `usage`.

**VS-9 — Unresolvable selector** Given a page without a matching element When `key --by name "Nope" Enter` runs with a short `--timeout` Then exit is 4 with `target_timeout`.

**VS-10 — Works inside `session`** Given a `session` stdin of `["key","Escape"]` then `["snap"]` When the batch runs over one connection Then both lines emit `ok` envelopes in order.

## Test plan

**Unit — keyspec parser (`internal/cli`, no browser, `t.Parallel()`)** Table-driven over valid and invalid specs, asserting the parsed `[]KeyStroke` and the rejection reason.
Cases: single named key; single char; each modifier alone; multi-modifier chord; space-separated sequence; unknown key name; unknown modifier; empty string; chord with two non-modifier keys (`cmd+a+b`); trailing `+`; case-insensitivity of modifiers but case-sensitivity of named keys (`Escape` valid, `escape` — decide in Open Questions).
This is a good property-target: parse→format→parse round-trips unchanged.

**Unit — command boundary (`internal/cli`, `chrometest.StubBrowser`)** Assert the envelope `command` is `key`, `result` carries the parsed keys, and `--wait-text` composes.
Assert VS-7 and VS-8 exit 2 using a stub that fails the test if any browser method is called — this is how "validate before connecting" gets enforced, not just documented.

**Live Chrome (`internal/chrome`, guarded by `testing.Short()`)** VS-1 through VS-6 against `data:` fixture pages, driven by the spawned browser, torn down in `t.Cleanup`.
Do not parallelize — these share the browser.

**Envelope/exit-code test (`internal/result`)** No new codes, so no change; the existing "unknown code maps to ExitGeneric" property test continues to cover regressions.

## Out of scope

- IME / composition events.
- Holding a modifier across several separate commands (each invocation is self-contained; use a sequence within one `key` call instead).
- Key events targeted at a specific iframe by frame id — `--pierce` behaviour is inherited from `QueryOpts` and not extended here.

## Open questions

1. Should named keys be case-insensitive (`escape` == `Escape`)?
   Leniency helps agents; strictness catches typos like `Ecsape` that would otherwise be interpreted as five character presses.
   **Recommendation:** case-insensitive match against the known-name table, but reject anything not in the table — so `escape` works and `Ecsape` is a usage error.
2. Should a bare multi-character token that is *not* a known key name be treated as literal text (typing `h`,`i` for `hi`) or rejected?
   **Recommendation:** reject.
   `type` already covers literal text, and silent reinterpretation is the failure mode US-6 exists to prevent.
3. Should `key` report `tab_hidden` like the a11y-backed verbs?
   Dispatch itself does not need the accessibility tree, but the `focused` field does.
   **Recommendation:** never fail for this — omit `focused` and continue.
