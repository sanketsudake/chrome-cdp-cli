# CLI reference

Every command speaks the same JSON envelope and the same exit-code contract, so a script or an agent parses one shape and branches on one number.
This page is the lookup table for all of it; for the ideas behind the commands, start with the [README](../README.md) and the [scenario guides](README.md).

## Output contract

Add `--json` to any command and it emits exactly one envelope on stdout:

```json
{ "ok": true, "command": "eval", "target": {"id":"…","title":"…","url":"…"},
  "result": { "value": "…" }, "elapsed_ms": 12 }
```

A failure uses the same shape with `"ok": false` and an `error{code,message,details}`, plus a nonzero exit code.
Branch on the exit code, not on message text.

| Exit | Code | Meaning |
|-----:|------|---------|
| 0 | — | success |
| 1 | generic | unclassified failure |
| 2 | usage | bad flags or arguments |
| 3 | connection | attach / launch failed |
| 4 | target/timeout | selector not found, timed out, or ambiguous/unknown target |
| 5 | cdp | CDP protocol error |
| 6 | daemon | daemon error |
| 7 | permission_denied | refused by [policy](#policy) — the origin, the verb, or the upload path is out of bounds |

Exit 7 is deliberately distinct from exit 4: an agent has to be able to tell "policy forbids this, stop and tell the user" from "element not found, retry differently".

Without `--json` the same information renders as a short human line (result to stdout, errors to stderr).

## Global flags

These apply to every command.

| Flag | Default | Purpose |
|------|---------|---------|
| `--json` | off | one JSON value to stdout |
| `--target <spec>` | sticky tab | tab to act on (see [Targeting](#targeting-a-tab)) |
| `--timeout <dur>` | `30s` | max time to wait for the command |
| `--by <mode>` | `css` | selector syntax (see [Addressing](#addressing-elements)) |
| `--wait <cond>` | `visible` | element wait: `visible` \| `ready` \| `enabled` |
| `--no-wait` | off | act immediately; fail fast instead of waiting |
| `--role <role>` | — | with `--by name`: constrain to an ARIA role |
| `--nth <n>` | — | with `--by name`: pick the Nth (1-based) match |
| `--match <mode>` | `exact` | with `--by name`: `exact` \| `contains` \| `regex` |
| `--in-row <text>` | — | with `--by name`: scope to the table row whose text contains this |
| `--on-dialog <policy>` | — | on click/type/fill: `accept` \| `dismiss` a native dialog raised during the action |
| `--pierce` | off | reach into shadow DOM / iframes (via DevTools search) |
| `--no-daemon` | off | connect directly instead of via the shared daemon |
| `--no-launch` | off | don't auto-launch a fallback Chrome |
| `--port <n>` | auto | explicit Chrome debug port |
| `--profile-dir <dir>` | default | managed-launch Chrome profile dir |
| `--no-color` | off | plain output (also honors `$NO_COLOR`) |
| `-q, --quiet` | off | suppress non-essential output |
| `-v, --verbose` | off | verbose diagnostics on stderr |
| `--allow <pattern>` | — | [policy](#policy): act only on these origins (repeatable) |
| `--policy-off` | off | [policy](#policy): don't enforce the configured policy for this command |

Precedence, highest first: **command-line flag > `CHROME_CDP_*` env var > config file > built-in default** (see [Configuration](#configuration)).

## Targeting a tab

A command acts on one tab, chosen (highest precedence first) by `--target`, then the sticky tab set with `use`, then a `target` default in config.

| Spec | Matches |
|------|---------|
| `<idprefix>` | a tab whose id starts with this |
| `url:<substr>` | first tab whose URL contains the substring |
| `title:<substr>` | first tab whose title contains the substring |
| `@N` | the Nth tab (1-based) in `list` order |

```sh
chrome-cdp use url:github     # set the sticky tab once…
chrome-cdp snap               # …then omit --target on later commands
```

## Addressing elements

`--by` chooses how a selector argument is interpreted.
On real apps, prefer `name` — it reads the accessibility tree, skips hidden/utility nodes, and crosses shadow DOM and same-origin iframes.

| `--by` | Selector is | Notes |
|--------|-------------|-------|
| `css` | a CSS selector | default; dynamic-id apps make it brittle |
| `id` | an element id | |
| `name` | an ARIA accessible name | prefer on real apps; pair with `--role` / `--nth` / `--match` |
| `ref` | a `snap`-issued `e<id>` ref | act on the exact node `snap` reported, no re-resolve |
| `cell` | a `[row\|]column` grid header | resolves the editable input in that grid cell |
| `label` | a form control's visible label | for controls whose label isn't wired (no `aria-label` / `<label for>`) |
| `search` | DevTools text/XPath/CSS search | broad; first match wins |
| `jspath` | a JS path | |
| `css-all` | a CSS selector (all matches) | |

Modifiers that refine a `--by name` match:

| Modifier | Effect |
|----------|--------|
| `--role button` | keep only nodes with that ARIA role |
| `--match contains` | match a case-insensitive substring (real apps have verbose names) |
| `--nth 2` | pick the 2nd match among visible candidates |
| `--in-row "<text>"` | keep only the match inside the table row containing `<text>` |

**Backgrounded tabs:** `--by css` / `id` / `search` resolve via `querySelector` and work regardless.
`--by name` / `ref` / `cell` read the accessibility tree, which Chrome throttles on a tab it can't foreground — a timeout there returns `tab_hidden: true` so you know to foreground Chrome (`--by name` also falls back to a DOM name match, and `--in-row` / `label` resolve via the DOM, so those keep working).

## Commands

### Tabs and navigation

| Command | Does |
|---------|------|
| `list [--url <s>] [--title <s>]` | list open tabs (`id`, title, URL); filters narrow by substring |
| `open <url>` | new tab → navigate → make it current; returns its id |
| `use <target>` | set the sticky current tab |
| `nav <url>` | navigate the target tab, wait for load |
| `nav --back` \| `--forward` | move through the tab's history (errors if there's no entry that way) |
| `nav --reload [--hard]` | reload; `--hard` bypasses the cache |
| `activate [<target>]` | bring the tab to the foreground — the fix for `tab_hidden` |
| `close [<target>] [--url <s>] [--title <s>] [--all]` | close a tab; `--all` closes every match |

```sh
chrome-cdp list --url outlook          # just the Outlook tabs
chrome-cdp open https://example.com    # returns the new tab's id
chrome-cdp nav --back                  # back one step in a wizard
chrome-cdp close --url staging --all   # tidy up after a batch job
```

`close` refuses to guess: when a filter matches more than one tab without `--all`, it closes **nothing** and exits `ambiguous_target`.
Closing the sticky tab clears it (`sticky_cleared: true`), so the next command reports `no_current_target` rather than failing against a dead id.

**Recovering from `tab_hidden`.**
Chrome throttles the accessibility tree on a tab it can't foreground, so `--by name` / `ref` / `cell` stall there and return `tab_hidden: true`.
`activate` is the remedy, and it makes the failure recoverable without a human switching tabs:

```sh
chrome-cdp snap --by name … || { chrome-cdp activate && chrome-cdp snap --by name …; }
```

`activate` reports `was_active` so a retry loop can tell "I fixed it" from "it was already foreground, so the stall has another cause", and `window_focused: false` when the OS refused to raise the window.

### Reading the page

| Command | Returns |
|---------|---------|
| `snap [--role <r>] [--grep <re>] [--region <name>] [--dedupe]` | accessibility snapshot: roles + names of actionable nodes, plus `alerts`, `focused`, per-node `states`/`value`/`ref` |
| `grid [selector]` | a table/grid as `{headers, rows, count}` |
| `value <selector> [--all]` | a form field's value (`--all`: every match, as a list) |
| `text <selector>` | visible text of a selector |
| `text --article [--markdown] [--min-chars <n>]` | the page's main readable content, boilerplate dropped |
| `html <selector> [--inner]` | outer (or inner) HTML |
| `eval <js> [--await]` | evaluate JS in the top frame |

The `snap` filters run server-side, so a read returns just the relevant nodes instead of a whole page.

```sh
chrome-cdp snap --role button --grep "[AP]M"    # calendar-event buttons only
chrome-cdp grid                                 # read a table without parsing snap
chrome-cdp value --all "input.hours"            # every hour cell in one call
```

#### `text --article` — the page without the furniture

On a real page most of the visible text is chrome: navigation, footers, cookie notices, related-link rails.
`--article` scores the page's blocks the way Reader Mode does and returns only the winning subtree, so a caller spends its attention (or its context budget) on content.

```sh
chrome-cdp text --article                       # the body text, nothing else
chrome-cdp text --article --markdown > notes.md # keep the structure
chrome-cdp text --article --min-chars 500       # demand a longer article
```

Extraction runs in an isolated world and scores a **clone** of the DOM — it never writes to the page you are automating, and it leaves nothing on `window`.

Because extraction is a heuristic, the envelope reports what it kept, so a script can tell a good extraction from a bad one:

```json
{ "ok": true, "command": "text",
  "result": { "text": "…", "title": "Quarterly report", "byline": "A. Author",
              "excerpt": "Revenue for the quarter reached…",
              "chars": 4821, "total_chars": 24193, "ratio": 0.199,
              "extracted": true, "format": "text" } }
```

When it keeps fewer than `--min-chars` characters (default 250) it says so instead of handing back a plausible-looking fragment: `extracted` is `false`, `reason` explains it, `article_chars` reports how little it found, and `text` falls back to the **full page text** — at exit 0.
A read that did return usable text is not a failure, so it is not an error; the flag is there to be checked.

`--markdown` preserves headings, lists, links, code blocks, and blockquotes.
It is deliberately **not** a general HTML-to-markdown converter: tables, footnotes, and embedded media are out of scope — tables come through as plain text and images are dropped.

`--markdown` or `--min-chars` without `--article`, and `--article` together with a selector, are `usage` errors (exit 2), rejected before Chrome is contacted.
The selector combination is an error on purpose: "extract the main content, but only within this subtree" has no clear meaning yet, and it can be defined later without breaking anything.

#### `eval --await` — DevTools console semantics

Plain `eval` evaluates an *expression*, which is why the two most natural things to type both fail: a top-level `await` is a syntax error, and a statement list is not an expression.
`--await` switches on `awaitPromise` and `replMode`, which is exactly what DevTools' own console does:

```sh
chrome-cdp eval --await 'await fetch("/api/me").then(r => r.json())'
chrome-cdp eval --await 'const rows = [...document.querySelectorAll("tr")]; rows.length'
```

The result records `awaited: true`, so a caller can tell which path ran.

A rejected promise is an **error**, never a value: exit 5 with `error.code: cdp_error`, the rejection's message in `error.message`, and its stack in `error.stack`.
A never-settling promise is bounded by `--timeout` (exit 4), and the connection stays usable afterwards.

`--await` is opt-in. `replMode` changes how bare object literals and `let`/`const` re-declaration behave, so plain `eval` keeps its existing semantics rather than changing silently under scripts that already work.

### Acting

| Command | Does |
|---------|------|
| `click <selector>` | click at the element's occlusion-verified centre |
| `hover <selector>` | move the pointer there without pressing — reveals hover-only menus and tooltips |
| `dblclick <selector>` | double-click (one `dblclick` event, `detail: 2`) — grid cells that edit on double-click |
| `rclick <selector>` | right-click, opening the context menu |
| `drag <selector> (--to <sel> \| --dx <p> --dy <p>)` | press, move, release — reordering, kanban, sliders |
| `key [selector] <keyspec>` | press keys that aren't literal text (`Escape`, `Tab`, `cmd+a`, `ArrowDown`) |
| `type <selector> <text>` | type via real keystrokes (**appends**; end with `\n` to press Enter) |
| `fill <selector> <value>` | set a field, **replacing** its content (clears, then types) |
| `select <field> <option>` | choose an option in a prompt / combobox / cascade / native `<select>` |
| `upload <selector> <path> [<path>...]` | attach local files to an `<input type=file>` |
| `scroll [selector] [--dx <p>] [--dy <p>] [--to] [--wheel]` | scroll by a delta, `--to` a selector into view, or a real `--wheel` |
| `attr get\|list\|set\|rm <selector> [name] [value]` | read/write element attributes |

`click`, `hover`, `dblclick`, `rclick` and `drag` are one driver method behind five names: they resolve the identical occlusion-verified centre and all take `--modifiers` (`ctrl`/`shift`/`alt`/`cmd`, joined with `+`) — `click --modifiers cmd` is the multi-select in a table.
An element that resolves but never presents an unoccluded centre fails as `target_timeout` with `occluded: true`, so it's distinguishable from "not found".

`key` takes a named key, a printable character, a chord, or a space-separated sequence of those, and works with no selector at all — which is what makes it usable when nothing is addressable:

| Form | Example |
|------|---------|
| named key | `Escape`, `Tab`, `ArrowDown`, `F2`, `Space` |
| printable character | `a`, `/` |
| chord | `cmd+a`, `ctrl+shift+k` |
| sequence | `"End shift+Home Backspace"` |

| `key` flag | Purpose |
|------------|---------|
| `--repeat <n>` | press the sequence *n* times (1–100) |
| `--delay <dur>` | pause between repeats, for apps that debounce |

`cmd` maps to Meta on every platform — the *page* decides which modifier it listens for, so the CLI never rewrites `cmd` to `ctrl` for you.
`shift+<character>` presses the character that key actually produces, so `shift+a` is the same press as `A` (and `shift+1` is `!`) rather than a lowercase `a` with a Shift bit set.
An unknown key name is a `usage` error rather than being typed as literal characters.

`drag` takes either a drop target or a pixel delta, never both:

| `drag` flag | Purpose |
|-------------|---------|
| `--to <selector>` | drop target |
| `--to-by <mode>` | `--by` mode for the drop target (defaults to `--by`) |
| `--dx`, `--dy <px>` | pixel delta from the source's centre |
| `--steps <n>` | interpolated move events, default `10` |
| `--hold <dur>` | pause after pressing before moving, for long-press-to-drag UIs |

The intermediate moves aren't cosmetic: a press and release at two points is silently a click to most drag implementations.
The drop target inherits only *how* to read a selector — `--by` (overridable with `--to-by`), `--wait`, `--pierce` — and never the flags that narrow a match (`--role`, `--nth`, `--match`, `--in-row`), which describe which candidate the *source* is: a `--in-row` applied to the drop target would scope it to the source's row and make any target elsewhere unresolvable.

```sh
chrome-cdp key Escape                                # close the open dialog
chrome-cdp key --repeat 3 ArrowDown                  # walk a listbox
chrome-cdp key --by name "Description" cmd+a         # select all, then retype
chrome-cdp hover --by name "Invoice 4102"            # reveal the row's actions
chrome-cdp dblclick --by cell "Mon, 7/13"            # edit a grid cell
chrome-cdp drag --by name "Task A" --to "Done" --to-by name
chrome-cdp click --by name "Row 2" --modifiers cmd   # add to the selection
```

Every acting verb — `nav`, `click`, `type`, `fill`, `select`, `key`, `hover`, `dblclick`, `rclick`, `drag`, `upload` — also takes **`--wait-text "<substr>"`**: after the action, block until the page contains the text (a `Saved` toast), folding act-and-confirm into one call.

`select` addresses the field by accessible name by default; a cascade path is `>`-separated:

| `select` flag | Purpose |
|---------------|---------|
| `--option-match <mode>` | how each option segment matches: `contains` (default) \| `exact` \| `regex` |
| `--filter <text>` | type this into the prompt to narrow options before selecting |
| `--sep <char>` | cascade path separator between levels (default `>`) |

```sh
chrome-cdp click --by name "Sign in" --role button
chrome-cdp fill "#hours" "8"
chrome-cdp fill --by cell "Mon, 7/13" "8"                 # grid input by column header
chrome-cdp select --by label "Category" "Direct Revenue"  # native <select> by label
chrome-cdp select "Time Type" "Projects > Acme: Platform > Project > Time Entry" --role textbox
chrome-cdp click --by name "Delete" --in-row "row two" --role button
chrome-cdp click "#delete" --on-dialog accept             # auto-accept a native confirm()
```

`upload` sets the files on the input directly (`DOM.setFileInputFiles`, which also fires `change`).
It never clicks the input: a click opens the **native OS file dialog**, which lives outside the page, is invisible to CDP, blocks the browser's main thread, and — unlike a JavaScript dialog, which `--on-dialog` handles — has no CDP method that can dismiss it.

| `upload` flag | Purpose |
|---------------|---------|
| `--append` | add to the files **this session** set on the input instead of replacing them |
| `--wait-text <substr>` | after the upload, block until the page contains the text |

`--wait` defaults to **`ready`** for this verb alone, because the real input behind a styled drop zone is usually `display:none` and waiting for visibility would fail on exactly the targets that need it.

Paths are `~`-expanded, resolved to the absolute path CDP requires, and checked **before** Chrome is contacted, so a missing path, a directory, or an unreadable file is `usage` / exit 2 with no connection and no consent prompt.
Set `upload_roots` in the config file's `[policy]` table to bound what may be uploaded: a path outside those directories is `permission_denied` / exit 7, compared on the cleaned absolute path with symlinks resolved on both sides, so `../` traversal and symlink escapes are both refused.
Unset means unrestricted, and it is deliberately not a flag or an environment variable — an allow-list the calling agent could widen would not be one.
`--policy-off` does not lift it either, for the same reason: it is argv, and argv is what the threat model assumes the caller controls.

The result reports the files **read back from the input** after the call — not the arguments — plus `multiple` and `accept`, because an `accept`/`multiple` mismatch is the usual reason an upload appears to work and then silently does nothing.
A file outside `accept` adds `accept_mismatch: true` but is not refused: `accept` is advisory in HTML and plenty of apps set it loosely.

Two limitations are deliberate.
Passing several paths to an input without `multiple` is `usage` / exit 2 and leaves the input untouched, and an element that resolves but is not a file input is also `usage` / exit 2 (naming the tag and type found) rather than a timeout — the selector resolved, so retrying cannot help.
`--append` only works for files this CLI set earlier in the same session: `setFileInputFiles` replaces the list wholesale and the DOM does not expose existing files' paths, so appending onto anything else is refused instead of silently dropping what was there.
A drop zone with no underlying `<input type=file>` is out of scope — there is nothing to set.

```sh
chrome-cdp upload --by label "Receipt" ./receipt.pdf
chrome-cdp upload "#attachments" a.pdf b.png c.csv         # a `multiple` input
chrome-cdp upload "input[type=file]" ~/docs/report.pdf --wait-text "Uploaded"
```

### Waiting

`wait` blocks until one condition holds (or `--timeout`).
Prefer a condition over a fixed `--for` sleep.

| Condition | Waits until |
|-----------|-------------|
| `--url <substr>` | the tab's URL contains the substring |
| `--visible <selector>` | the selector is visible |
| `--gone <selector>` | the selector is gone |
| `--text <substr>` | the accessibility tree (incl. alerts) contains the text |
| `--stable` | the accessibility tree stops changing (page settled) |
| `--idle` | network activity settles (no in-flight requests) — for SPA loads |
| `--request <substr>` | a matching HTTP request **completes** (see [Network](#network)) |
| `--for <dur>` | a fixed duration (fallback) |

```sh
chrome-cdp wait --idle                            # after nav/open on an SPA
chrome-cdp wait --text "Success"                  # confirm a write landed
chrome-cdp wait --request "/api/save" --status 2xx # confirm the write actually POSTed
```

`--idle` and `--request` answer different questions.
`--idle` is "the page settled"; `--request` is "this specific call finished with this outcome", which is the sharper tool on a page whose polling or long-lived stream never lets the network go quiet.

### Console

`console` reads what the page said: `console.*` output and uncaught exceptions, with their stack.

Capture starts when the connection **attaches to a tab**, not when `console` first runs.
That is what makes it useful after the fact: the exception behind a failed `click` is already buffered by the time you go looking for it.

| Flag | Default | Purpose |
|------|---------|---------|
| `--grep <re>` | — | only messages whose text matches this regex |
| `--level <l>` | all | `debug`\|`log`\|`info`\|`warn`\|`error`; repeatable |
| `--only-errors` | off | shorthand for `--level error` (uncaught exceptions are reported at error level) |
| `--limit <n>` | `100` | most recent *n* matching messages |
| `--since <dur>` | — | only messages newer than this (e.g. `30s`) |
| `--clear` | off | drop the buffered messages after reading |
| `--follow` | off | stream new messages as NDJSON until `--timeout` or interrupt |
| `--fail-on-match` | off | exit 1 if at least one message is returned |

```sh
chrome-cdp console --only-errors                       # what broke
chrome-cdp console --grep "\[Checkout\]" --limit 20    # one subsystem
chrome-cdp console --clear                             # reset before an action
chrome-cdp console --follow --level error              # watch while you work
```

Every filter is applied where the buffer lives, before the result is built, so a chatty app cannot flood a caller's context.
The result carries `messages`, `count` (after filtering), `buffered` (held for this tab), `dropped`, and `truncated` (`--limit` cut the list).
Each message carries `level`, `source` (`console` \| `exception` \| `log`), `text`, `ts`, and — for an exception — its `stack`.

**`dropped > 0` means you read too late**: the ring buffer evicted older messages before this read.
Raise `console_buffer`, or read closer to the action.

**`--fail-on-match` exits 1 and still reports the messages** (`error.code` is `assertion_failed`), so a CI log shows *what* failed, not just that something did.

**`--follow`** writes one JSON envelope per line, the same shape `session` streams.
It cannot combine with `--fail-on-match`, and it is a usage error inside `session`, where it would break the one-envelope-per-line contract.

**`--no-daemon` has no retained history.**
Without the daemon there was no process alive to receive the tab's earlier events, so the read reports `"buffered": 0` and carries a `note` saying so, rather than passing an empty list off as a quiet page.

The buffer is bounded by `console_buffer` (messages per tab, default 1000) and `console_max_entry` (per-message text cap, default 8192 bytes); see [Configuration](#configuration).

### Network

`net` reads the HTTP requests the tab made: method, URL, status, timing, sizes, and — on request — headers and bodies.

Like `console`, capture starts when the connection **attaches to a tab**, not when `net` first runs, so the 401 behind an empty screen is already buffered by the time you go looking for it.

| Flag | Default | Purpose |
|------|---------|---------|
| `--url <s>` | — | substring match; a `re:` prefix switches to regex |
| `--method <m>` | all | `GET`, `POST`, …; repeatable |
| `--status <spec>` | all | `200`, `2xx`, `>=400`, `!2xx` |
| `--type <t>` | all | `document`\|`xhr`\|`fetch`\|`script`\|`stylesheet`\|`image`\|`font`\|`websocket`\|`other`; repeatable |
| `--xhr` | off | shorthand for `--type xhr --type fetch` |
| `--failed` | off | non-2xx **or** network-level failure |
| `--limit <n>` | `100` | most recent *n* matching |
| `--since <dur>` | — | only requests newer than this |
| `--headers` | off | include request and response headers |
| `--body` | off | include request and response bodies (size-capped) |
| `--no-redact` | off | do **not** redact credential-shaped values |
| `--clear` | off | drop the buffered requests after reading |
| `--follow` | off | stream completed requests as NDJSON |
| `--fail-on-match` | off | exit 1 if any request matched |

```sh
chrome-cdp net --xhr --limit 20                          # recent API calls
chrome-cdp net --failed                                  # what broke
chrome-cdp net --url "/api/save" --method POST --body    # inspect the payload
chrome-cdp net --clear && chrome-cdp click "#save" && chrome-cdp net --xhr
```

Every filter is applied where the buffer lives, before the result is built, so a chatty page cannot flood a caller's context.
The result carries `requests`, `count` (after filtering), `buffered`, `dropped`, `truncated`, and `pending`.

**`pending` counts requests that started but have not finished**, so you can tell "nothing matched" from "not finished yet" — an empty listing during a slow save is otherwise indistinguishable from a save that never fired.

Each request carries `id`, `method`, `url`, `type`, `status`, `status_text`, `started_ms` (milliseconds since capture began on this tab), `duration_ms`, `request_size`, `response_size`, `from_cache`, `failed`, and `error`.
`status`, `duration_ms`, and `error` are `null` when they do not exist yet; `failed` means a non-2xx status **or** a network-level failure, so a delivered 500 and a DNS failure both show up under `--failed`.

**Redaction is on by default.**
This CLI drives your real, logged-in Chrome, so its buffers hold live session credentials by construction.
The values of `authorization`, `cookie`, `set-cookie`, `x-api-key`, `proxy-authorization`, and any header whose name contains `token`, `secret`, or `password` are replaced with `<redacted>` — the name stays, so a 401 is still diagnosable.
Credential-shaped URL query and fragment parameters (`access_token`, `api_key`, `sig`, `code`, `key`, …) are redacted the same way.
`--no-redact` is the explicit, deliberate opt-out.

**Headers and bodies are absent, not null, unless you ask.**
Without `--headers` / `--body` those keys do not appear at all, so a routine listing stays small and does not spill tokens or PII into a log.

**Response bodies are fetched lazily, never buffered.**
They are pulled with `Network.getResponseBody` at read time, only when `--body` is passed — buffering every body would multiply the daemon's memory and retain payloads you never asked to see.
The consequence: **a body may already be gone** if the page navigated away, or if it is not UTF-8 text.
That is reported as `"response_body": null` with `"body_unavailable": true`, and the read still succeeds — a partial answer beats no answer.
Bodies over `net_max_body` (default 65536 bytes) are cut, with `"body_truncated": true`.
Request bodies arrive inline with the request, so they are retained and available retroactively.

**`net wait` / `wait --request`** blocks until one specific request completes.

```sh
chrome-cdp wait --request "/api/save" --status 2xx   # the primary form
chrome-cdp net wait --url "/api/save" --status 2xx   # the alias
```

It matches **already-buffered** requests first, so a request that completed between the action and the wait is not missed.
It needs a URL substring or `--failed` to identify the request; `--method`, `--status`, and `--type` only narrow the match.
The matched record rides in `result.request`, in the same shape a listing uses.
No match before `--timeout` is `target_timeout` / exit 4.

**`--fail-on-match` exits 1 and still reports the requests** (`error.code` is `assertion_failed`), so `chrome-cdp net --failed --fail-on-match` is a usable CI assertion that shows *what* failed.

**`--follow`** writes one JSON envelope per **completed** request, the same shape `session` streams.
It cannot combine with `--fail-on-match`, and it is a usage error inside `session`.

**`--no-daemon` has no retained history**, exactly as with `console`: the read reports `"buffered": 0` and carries a `note` rather than passing an empty list off as a quiet page.

Bad `--status` / `--type` / `--url` regex / `--since` values are `usage` / exit 2, validated before anything connects to Chrome.

The buffer is bounded by `net_buffer` (records per tab, default 500) and `net_max_body` (per-body cap, default 65536 bytes); see [Configuration](#configuration).

### Batch mode

`session` reads NDJSON argv lines on stdin and runs each over **one** held connection, emitting one NDJSON envelope per line — no per-command process spawn, and `snap` refs stay valid across the batch.

```sh
printf '%s\n' \
  '["fill","--by","cell","Mon, 7/13","8"]' \
  '["fill","--by","cell","Tue, 7/14","8"]' \
  '["value","--all","input[data-automation-id=numericInput]"]' \
  '["click","--by","name","Save and Close","--role","button","--wait-text","saved"]' \
  | chrome-cdp session
```

| Flag | Default | Purpose |
|------|---------|---------|
| `--record <path>` | — | [record](#recording) the whole batch and write it here |
| `--record-fps <n>` | `4` | with `--record`: frames per second to retain |
| `--record-annotate` | off | with `--record`: mark action positions on the exported frames |

`--record` starts as soon as a line resolves a tab (usually the first `use`) and stops after the last line, so it needs no manual bracketing.
The file is written even when a step failed — which is when a recording is worth the most — and the batch emits one extra NDJSON line describing it.

### Recipes

A **recipe** is a saved `session` script with a small header: a YAML file whose steps are argv arrays, with declared inputs substituted into argv elements.
It is the unit in which a working automation becomes something you can name, re-run, commit, and hand to a colleague.

| Command | Does |
|---------|------|
| `recipe list [--dir <path>]` | list recipes with their description, inputs, and source |
| `recipe show <name>` | print the recipe's source (read it before you run it) |
| `recipe new <name>` | write a commented template and print its path |
| `recipe run <name> [--set k=v]… [--dry-run] [--from-step <n>]` | run it |

```yaml
# .chrome-cdp/recipes/submit-timesheet.yaml
name: submit-timesheet
description: Fill and submit the weekly timesheet.
inputs:
  week:  { required: true, description: "Monday of the week, YYYY-MM-DD" }
  hours: { default: "8",   description: "Hours per weekday" }
target: url:workday
steps:
  - label: open the timesheet
    run: ["nav", "https://workday.internal/time/{{week}}"]
  - run: ["wait", "--idle"]
  - label: save
    run: ["click", "--by", "name", "Save and Close", "--role", "button", "--wait-text", "saved"]
    on_error: abort
```

```sh
chrome-cdp recipe run submit-timesheet --set week=2026-07-20
chrome-cdp recipe run submit-timesheet --set week=2026-07-20 --dry-run   # print, run nothing
```

**The format, in full.**
`name` (which must match the filename), `description`, `inputs`, `target`, `steps`; each step has `run`, an optional `label`, and an optional `on_error`.
That is everything — there is no other key, and an unrecognised one is an error rather than a silently ignored typo.

| Field | Means |
|-------|-------|
| `run` | an argv array, identical to a `session` stdin line — anything valid in `session` is valid here |
| `label` | a name for the step, echoed in its envelope and in the failure summary |
| `on_error` | `abort` (default) or `continue`; there are no retries, conditionals, or loops |
| `inputs` | `required`, `default`, `description` — no types and no validation expressions |
| `target` | a default `--target` for every step, overridden by a step's own `--target` or by `--target` on the run |

`{{name}}` substitutes an input **into one argv element**.
There is no shell anywhere in this design and there is no `shell:` step type, so a value is passed through byte for byte and nothing in it is interpreted.
Per-step flags a verb already accepts — `--timeout 60s`, `--by name`, `--wait-text` — go in that step's own `run` array, where a reader of the recipe can see them.

**Where recipes live.**
Resolution order for a name, first match wins:

1. `./.chrome-cdp/recipes/` — project-local; commit this directory and your team gets your internal-app automations from the repo
2. `$XDG_CONFIG_HOME/chrome-cdp/recipes/` — your own
3. `--dir <path>`

`recipe list` marks each entry's source, so you can see which copy is about to run.
`recipe new` writes into the project-local directory unless `--dir` says otherwise.

**Output.**
`recipe run` emits one NDJSON envelope per step — the same stream `session` produces, plus `step` and `label` fields so a caller can correlate without counting lines — then a summary:

```json
{"ok":true,"command":"recipe","result":{"recipe":"submit-timesheet","steps":4,"completed":4,
 "failed":null,"inputs":{"week":"2026-07-20","hours":"8"},"from_step":1,"elapsed_ms":4120}}
```

A failing step stops the run (unless it says `on_error: continue`), and the summary carries `failed: {"index":3,"label":"save","code":"target_timeout"}`.
**The process exit code is the failing step's**, so a shell caller branches on the same contract as for a single command.
Under `--quiet` only the summary is printed.

**Reviewing a recipe someone sent you.**
A recipe drives the browser you are already signed into, so read one before running it, exactly as you would a shell script:

```sh
chrome-cdp recipe show their-recipe                    # the source, comments and all
chrome-cdp recipe run their-recipe --dry-run           # the resolved argv, one array per line
chrome-cdp recipe run their-recipe --dry-run | chrome-cdp session   # the same thing, executed
```

The dry run prints the exact bytes `session` consumes.
That is both a debugging tool and the proof that recipes add no hidden magic: the two paths run the identical commands.

Never put a credential in a recipe.
The whole premise of `chrome-cdp` is reusing an already-authenticated browser, so a recipe never needs one.

**Errors.**
Everything a recipe can get wrong statically is exit 2, with Chrome never contacted:

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| Recipe not found in any search dir | `usage` | 2 |
| Malformed YAML, unknown key, `run` not an array of strings | `usage` | 2 |
| Missing required input, unknown `--set` key, undeclared `{{placeholder}}` | `usage` | 2 |
| `--from-step` out of range | `usage` | 2 |
| A step fails | that step's code | that step's exit |

An unknown `--set` key is rejected rather than ignored: silently dropping `--set hurs=9` would run the recipe with the default you were trying to override.

**`--from-step <n>` is a sharp tool.**
It starts at step *n* (1-based) and assumes every earlier step's effect is already in place.
That is what you want when a ten-step automation failed at step 8 and re-running from the top would submit a form twice — and it is exactly wrong when the page is not where step *n* expects it.
Validation still covers the whole file: an undeclared placeholder in a skipped step is still an error.

**What recipes deliberately do not have.**
Conditionals, loops, retries, branching, and reading one step's output into a later step.
Recipes cannot invoke recipes, and a recipe is capped at 200 steps.
If an automation needs control flow, write a program that calls `session` — that is the supported answer, not a bigger recipe format.

### Capture

| Command | Writes |
|---------|--------|
| `screenshot [-o <path>]` | an image of the viewport, an element, a region, or the full page |
| `pdf [-o <path>]` | a PDF of the page |

Both write to `./<command>-<timestamp>.<ext>` by default — with a `-1`, `-2`, … counter rather than overwriting — or to `-o <path>`, or to stdout with `-o -`.

**screenshot**

| Flag | Default | Purpose |
|------|---------|---------|
| `--selector <sel>` | — | capture this element's box; honours every addressing flag (`--by`, `--role`, `--nth`, `--match`, `--in-row`, `--pierce`) |
| `--full-page` | off | capture the whole scrollable page, beyond the fold |
| `--region x,y,w,h` | — | capture an explicit page-coordinate rectangle |
| `--format` | `png` | `png` \| `jpeg` \| `webp` |
| `--quality <n>` | `80` | 0–100; **jpeg/webp only** — passing it with `png` is a usage error, not a silent no-op |
| `--scale <f>` | `1` | output scale factor, 0.1–3, applied in the renderer so text stays crisp |
| `--padding <px>` | `0` | expand an element capture, clamped to the page so an element at an edge keeps a non-negative origin |

`--selector`, `--full-page` and `--region` select different modes and are mutually exclusive.

```sh
chrome-cdp screenshot --selector "#invoice-table" --padding 8 -o invoice.png
chrome-cdp screenshot --full-page -o report.png
chrome-cdp screenshot --format jpeg --quality 60 --scale 0.5 -o small.jpg
chrome-cdp screenshot --selector "Summary card" --by name --role region
```

The result reports `path`, `bytes`, `width`, `height`, `format`, `scale`, `mode` (`viewport` \| `element` \| `full_page` \| `region`) and the resolved `clip` in page coordinates.
`mode` and `clip` are what make a capture that came out wrong debuggable without opening the image.

Full-page capture does **not** force lazy-loaded content to load: images below the fold that appear on scroll may come out blank.
Scroll through the page first (`scroll --dy …`, then `wait --idle`) when that matters.

**pdf**

| Flag | Default | Purpose |
|------|---------|---------|
| `--landscape` | off | orientation |
| `--paper <name>` | `letter` | `letter` \| `legal` \| `tabloid` \| `ledger` \| `a0`–`a6` (case-insensitive), or `WxH` in inches |
| `--margin <spec>` | `0.4in` | one value, or `top,right,bottom,left`; units `in` (default), `cm`, `mm`, `px`, `pt` |
| `--scale <f>` | `1` | render scale, 0.1–2 |
| `--background` | off | print background graphics |
| `--pages <ranges>` | all | e.g. `1-3,5` |
| `--header <tpl>` / `--footer <tpl>` | — | HTML templates (classes `date`, `title`, `url`, `pageNumber`, `totalPages`) |

```sh
chrome-cdp pdf --landscape --paper a4 --background -o report.pdf
chrome-cdp pdf --pages 1-3,7 --margin 0.5in,1in,0.5in,1in
```

The result reports `path`, `bytes`, and `pages`.

Every value above is parsed before anything connects to Chrome, so a malformed rectangle, paper name, margin spec or page range — or an out-of-range quality or scale — is `usage` / exit 2 with the browser untouched.
An element that resolves but has a zero-area box (`display:none`, collapsed) is exit 4 with `zero_area: true`: the selector was right, so it is reported differently from "not found".

### Recording

`record` captures the tab while other commands drive it, and exports the frames as an animated GIF (or an MP4/WebM, or a directory of numbered PNGs).

```sh
chrome-cdp record start --annotate
chrome-cdp click --by name "Save"
chrome-cdp record stop -o demo.gif
```

> **This records your real, logged-in browser.**
> A recording attached to a public issue may contain your own data — your tabs, your name, whatever the page was showing.
> Nothing in the CLI can know which pixels are sensitive, so look at the file before you share it.
> `record stop` prints the frame count and the path in human mode, so it is never ambiguous that a file was written.

| Command | Does |
|---------|------|
| `record start` | begin capturing the target tab |
| `record stop [-o <path>]` | stop and write the animation (default `./record-<timestamp>.gif`) |
| `record status` | report whether the tab is being recorded, and how much is held |
| `record cancel` | discard the recording without writing anything |

**record start**

| Flag | Default | Purpose |
|------|---------|---------|
| `--fps <n>` | `4` | frames per second to retain; a **ceiling**, not a fixed interval |
| `--scale <f>` | `0.5` | capture scale relative to the viewport, 0.1–1 |
| `--annotate` | off | mark action positions when exporting (overridable at `record stop`) |
| `--max-duration <dur>` | `2m` | stop capturing after this long; the frames stay exportable |
| `--max-frames <n>` | `600` | ring-buffer size (config key `record_buffer`) |

**record stop**

| Flag | Default | Purpose |
|------|---------|---------|
| `-o, --output <path>` | `./record-<timestamp>.<ext>` | output path, or `-` for stdout |
| `--format` | from the extension, else `gif` | `gif` \| `mp4` \| `webm` \| `frames` |
| `--max-size <size>` | — | best-effort ceiling, e.g. `2MB` or `1500000` |
| `--loop <n>` | `0` (forever) | how many times the GIF plays; `--loop 3` plays three times |
| `--annotate` | from `record start` | draw the action markers |

The result reports `path`, `format`, `frames`, `fps`, `duration_ms`, `width`, `height`, `bytes`, `annotated`, and the capture's own `dropped_frames` / `truncated` / `reason`.

**Frames live in the daemon, not in the command that started the recording.**
That is what makes a run which crashed half way still have a recording of the failure: `record stop` afterwards writes it, and it writes it even when the tab itself has since closed (the result then says `tab_closed: true`).

**Capture is a screencast, not a screenshot loop.**
Chrome pushes a frame only when the page actually changes, so a static page costs almost nothing and `--fps` throttles a busy one rather than polling a quiet one.
A frame skipped by that throttle is *not* a dropped frame — you asked for 4fps.
A tab that renders nothing at all (fully backgrounded, on some platforms) produces no frames, and `record stop` says so rather than writing an empty file.

**Truncation is always reported.**
The ring keeps the most recent `--max-frames`, a byte ceiling (`record_max_bytes`) keeps the retained frames bounded no matter how large the viewport is, and `--max-duration` stops the capture.
Whichever fires sets `truncated: true` with a `reason` and counts the loss in `dropped_frames`, so a partial recording is never presented as a complete one.

**Annotation is composited at export, never at capture.**
The daemon records `(timestamp, command, coordinates)` alongside the frames — the coordinates come from the pointer verbs, which already resolve and report a centre point — and the exporter draws position markers.
Without `--annotate` the exported frames are pixel-identical to what Chrome captured, which is what makes a recording usable as a README asset.
One recording can therefore be exported both ways.
`annotated` in the result is a claim about the pixels: it is `true` only when a marker was actually drawn, so a recording with no actions in it — or one whose only mark fell outside the frame — reports `false` rather than implying markers you will not find.
`--max-size` dropping frames does not drop their markers: a dropped frame's marks move to the nearest kept one.

**Formats.**
`gif` and `frames` need nothing installed, and `gif` is the default because the dependency-free path should be the one that works out of the box.
`mp4` and `webm` need `ffmpeg` on `PATH`, and its absence is a `usage` error naming the requirement — checked **before** the recording is drained, so the frames survive to be exported as a GIF instead.
`--format` conflicting with the output extension is an error rather than a guess, since a WebM in a file called `demo.gif` plays nowhere.

**A window resized mid-recording is letterboxed, not stretched.**
The canvas comes from the first frame; a later frame with a different shape is scaled to fit and padded, so the export never shows a page at an aspect ratio it never had.

**`--max-size` is best-effort.**
It re-encodes at a smaller scale, and then at a lower frame count, until the file fits; the result reports `reduced`, the `export_scale` used, and `within_max_size: false` when the ceiling could not be met.

Recording is **per-tab**.
A batch that opens new tabs records the one it started on; a multi-tab recording is out of scope.

`session --record <path>` brackets a whole batch (see [Batch mode](#batch-mode)).

Agents: `record` is deliberately outside the default MCP tool set — an agent silently recording the user's browser is a surprising capability — and is available only under the full tool set.

### Browser state

| Command | Does |
|---------|------|
| `cookie list\|set\|rm\|clear` | read and write cookies for the tab |
| `headers set <k=v> …` | set extra request headers |
| `emulate viewport\|geo\|reset` | override viewport size / geolocation, or clear overrides |
| `frame list` | list the tab's frame tree |

### Escape hatch

`raw` calls any CDP method by name — full protocol coverage, no per-method wrapper.

```sh
chrome-cdp raw --list                                        # list CDP domains
chrome-cdp raw Network.setCacheDisabled '{"cacheDisabled":true}'
chrome-cdp raw Browser.getVersion --browser                  # browser-level method
```

### Meta

| Command | Does |
|---------|------|
| `doctor` | check the connection and print the exact fix if it's not ready |
| `daemon start\|stop\|status` | manage the background connection |
| `policy init` | write a starter [`[policy]`](#policy) table allow-listing the current tab's origin (`--wildcard`, `--print`, `-o`) |
| `exit-codes` | print the exit-code table |
| `version` | print the version |
| `completion bash\|zsh\|fish\|powershell` | shell completion script |

## Connection model

`chrome-cdp` attaches to your **real** Chrome via the one-time `chrome://inspect/#remote-debugging` toggle; it reads Chrome's `DevToolsActivePort` file and connects the WebSocket directly (the classic `--remote-debugging-port` flag no longer works on the default profile since Chrome M136).
If no debug-enabled Chrome is found, it launches a managed Chrome on a dedicated profile alongside your real one.

A background **daemon** holds the connection, so Chrome's "Allow debugging?" prompt appears once per session rather than once per command.
It starts lazily on first use and idles out after 30 minutes; manage it with `daemon start|stop|status`, or bypass it with `--no-daemon`.

Run `chrome-cdp doctor` to check the connection and get the exact fix when it isn't ready.

## Policy

`chrome-cdp` drives your real, already-authenticated Chrome, which means anything holding a connection to it can act as you on every site you are logged into — not just the one you meant.
The optional policy layer bounds that: which origins the CLI may act on, which verbs are permitted there, and which local paths may be uploaded.

**It is off unless you configure it**, and it changes nothing until you do.

### What it is not

This bounds a **cooperative** caller.
It is not a sandbox.
Anything that can run `chrome-cdp` can also edit this config, or connect to Chrome directly and skip the CLI entirely.
It is a guardrail against a confused or misdirected caller — an agent that read "now go to the admin console" off a web page, a shared recipe you did not read line by line — and it is worth having for exactly that.
Overstating it would be worse than not shipping it.

### Getting started

```sh
chrome-cdp use url:myapp                 # be on the tab you want to bound
chrome-cdp policy init                   # writes [policy] allow = ["app.example.com"]
chrome-cdp policy init --wildcard --print # see the *.example.com version without writing
```

### Configuration

```toml
[policy]
enabled = true
allow   = ["*.workday.com", "intranet.corp.local", "localhost:*"]
deny    = ["*.bank.example", "admin.corp.local"]
read_only = ["*.wikipedia.org"]
verbs_denied = ["raw"]
upload_roots = ["~/Documents/receipts"]
audit_log = "~/.local/state/chrome-cdp/audit.log"
audit_all = false
on_violation = "error"     # error | prompt
```

| Key | Purpose |
|-----|---------|
| `enabled` | master switch; a present table is on unless you set this to `false` |
| `allow` | origins that may be acted on; **empty means "everything except `deny`"** |
| `deny` | always refused, and it beats `allow` |
| `read_only` | origins where reading verbs work and acting verbs are refused |
| `verbs_denied` | verbs refused on every origin (e.g. `raw`, `eval`) |
| `upload_roots` | directories files may be uploaded from |
| `audit_log` | append-only NDJSON of refusals (and of every action with `audit_all`) |
| `on_violation` | `error` (default), or `prompt` to confirm interactively |

### Pattern syntax

Patterns are `[scheme://]host[:port]`, matched against the **parsed** URL's host — never against a substring of the raw URL.
There is no regex: a policy language that is hard to read is a policy that is wrong without anyone noticing.

| Pattern | Matches | Does not match |
|---------|---------|----------------|
| `example.com` | `example.com`, on any scheme or port | `a.example.com` |
| `*.example.com` | `a.example.com`, `a.b.example.com` | `example.com` (needs its own entry), `notexample.com`, `example.com.evil.io` |
| `localhost:3000` | `localhost` on port 3000 | `localhost:8080` |
| `localhost:*` | `localhost` on any port | — |
| `https://x.test` | `x.test` over https | `http://x.test` |

Host matching is case-insensitive, and ports are compared numerically, so `host:443` and `host:0443` are the same port.

**`*.host` in `deny` covers `host` itself**, which is the one place the wildcard reads differently from the table above.
In `allow` and `read_only`, excluding the apex is the strict reading a boundary needs — `*.example.com` must not quietly widen to `example.com`.
In `deny` it would be a hole: `deny = ["*.bank.example"]` means "not my bank", and reading it as "every subdomain of my bank, but the bank itself is fine" would protect you everywhere except the host you were thinking of.
Over-blocking is the safe direction in a list of what may never be touched.

Matching is on the origin **Chrome** resolves, not on the string as typed.
`view-source:`, `blob:` and `filesystem:` URLs are unwrapped to the origin whose content they actually serve, and a `\` in the authority is normalised to `/` the way Chrome normalises it.
So `https://bank.example\@evil.io/` is `bank.example`, and `view-source:https://bank.example/statement` is checked — and refused — as `bank.example`.
Unwrapping runs in both directions: `view-source:` of an allowed origin stays allowed.

A URL with no identifiable origin at all — `about:blank`, `data:`, `file://`, `javascript:` — is **refused whenever a policy is active**, whatever shape the rules take.
A policy cannot decide about an origin it cannot identify, and "matches nothing" would be the safe answer under an `allow` list and a free bypass under a `deny` list.
(`chrome://settings` does parse, to the host `settings`, and is decided about like any other origin: an `allow` list refuses it because nothing named it.)

A pattern the CLI cannot parse is a **fatal** error: unlike the rest of the config, which warns and carries on, a policy that could not be read refuses to run, because a policy that fails open is worse than no policy.
A config file that exists but cannot be *read* — wrong permissions, a bad mount — is treated exactly the same way, rather than as a policy that was never there.
Use `--policy-off` to run while you fix it.

### What is checked

| Verb class | Checked against |
|-----------|-----------------|
| Acting (`click`, `type`, `fill`, `select`, `scroll`, `key`, pointer verbs, `upload`, `attr set/rm`, `cookie set/rm/clear`, `headers`, `emulate`, `eval`, `raw`) | `allow`/`deny`, `read_only`, `verbs_denied` |
| Reading (`snap`, `text`, `html`, `value`, `grid`, `screenshot`, `pdf`, `frame`, `wait`, `attr get/list`, `cookie list`) | `allow`/`deny`, `verbs_denied` |
| Navigating (`nav <url>`, `open`) | the **destination** origin, before navigating |
| Tabs and meta (`list`, `use`, `close`, `activate`, `version`, …) | not checked; every envelope's `target`, and every tab `list` or an ambiguous `close` enumerates, is reduced to a bare origin with no full URL and no title when the policy does not cover it |

A verb that is not classified is treated as **acting**, so a new verb over-restricts rather than slipping through.

Redirects are the honest limitation: a `nav` to an allowed origin that redirects elsewhere cannot be stopped, so the policy is re-evaluated on the **settled** URL and the *next* command is refused.

### Destination checking needs `verbs_denied`

`eval` and `raw` can navigate the tab themselves.
`eval "location='https://bank.example/'"` on an allowed tab issues an authenticated GET to an origin the allow-list would have refused, and no check in front of `nav` and `open` can see it coming.
What the policy still gives you is that the tab is then off-limits: the next command is refused on the settled origin, so nothing is read back.
But the request happened.

**So an origin allow-list is only meaningful alongside `verbs_denied = ["eval", "raw"]`**, and that is what `chrome-cdp policy init` writes.
If you need `eval`, understand that you have kept a verb that can walk out of the boundary and come back — the boundary still bounds what you can *read*, not what you can *reach*.

### A refusal

```json
{ "ok": false, "command": "click",
  "error": { "code": "permission_denied",
             "message": "origin admin.corp.local is not permitted by policy",
             "origin": "admin.corp.local", "verb": "click",
             "rule": "deny: admin.corp.local",
             "config": "~/.config/chrome-cdp/config.toml" },
  "elapsed_ms": 2 }
```

`rule` names the entry that decided it, so a refusal points at the line to edit.
The browser is never asked to act on a refused command.

### Overrides

```sh
chrome-cdp --allow "*.example.com" click "#save"   # one-off allow-list, replacing the configured one
chrome-cdp --policy-off click "#save"              # run without the policy — explicit, and logged
```

`--allow` narrows; it never unblocks something `deny` or `verbs_denied` refused.
`--policy-off` exists because a bad policy that cannot be bypassed is worse than none, but it is never implicit: it warns on stderr and lands in the audit log.

`--policy-off` covers the **origin** policy only.
`upload_roots` stays in force regardless, because it is a filesystem boundary rather than an origin rule: its threat model is a caller that writes the argv, and `--policy-off` is argv.
Widen the roots or move the file.

### Audit log

`audit_log` is append-only NDJSON, one record per decision:

```json
{"ts":"2026-07-26T09:12:03Z","origin":"other.test","verb":"click","decision":"refused","rule":"allow: no match"}
```

Refusals are always recorded; set `audit_all = true` to record permitted actions too.
It records the **origin**, never the URL, and never any value — no typed text, no cookie values, no selectors — because a log that captured those would be the most sensitive file this tool produces.
A URL with no origin to record is written as its scheme plus a placeholder (`file:(unparseable)`, `javascript:(unparseable)`), never as the string itself: a refused URL's query is exactly where a session token lives.

## Configuration

Persist flags you'd otherwise retype in `$XDG_CONFIG_HOME/chrome-cdp/config.toml` (usually `~/.config/chrome-cdp/config.toml`); see [`config.example.toml`](../config.example.toml) for the full key set.

```toml
json = true            # default to machine-readable output
timeout = "10s"
by = "search"          # default selector syntax
target = "url:github"  # default tab when neither --target nor `use` is set
```

A malformed config is a warning on stderr, not a fatal error — the CLI still runs on the built-ins.
The one exception is the [`[policy]`](#policy) table: a policy the CLI cannot read makes it refuse to act rather than act unbounded, and that covers a file that cannot be *read* (wrong permissions, a bad mount) as well as one that does not parse.

`XDG_CONFIG_HOME` chooses **which** file is read, so an environment that points it at a directory without one leaves you with no `[policy]` table at all.
No `CHROME_CDP_*` variable can set a policy key, but that is a statement about the table's contents, not about which file supplies them.
When `XDG_CONFIG_HOME` is set and there is no config file there, the CLI says so on stderr rather than letting a boundary disappear quietly.
