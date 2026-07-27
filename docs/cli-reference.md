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
| 3 | connection | attach / launch failed, or Chrome's consent prompt is unanswered |
| 4 | target/timeout | selector not found, timed out, or ambiguous/unknown target |
| 5 | cdp | CDP protocol error |
| 6 | daemon | daemon error |
| 7 | permission_denied | refused by [policy](#policy) — the origin, the verb, or the upload path is out of bounds |

Exit 3 covers three `error.code` values: `connection_failed`, `not_debug_enabled`, and `consent_pending`.
The last one means Chrome accepted the connection and then went silent because it is holding its browser-modal "Allow remote debugging?" dialog — nothing is broken, a human has not answered yet.
It is a distinct code on the same number because the remedy is "click the dialog", not "check your setup"; the numbers are contract and do not grow for a new failure mode.

Exit 7 is deliberately distinct from exit 4: an agent has to be able to tell "policy forbids this, stop and tell the user" from "element not found, retry differently".

Without `--json` the same information renders as a short human line (result to stdout, errors to stderr).

## Global flags

These apply to every command.

| Flag | Default | Purpose |
|------|---------|---------|
| `--json` | off | one JSON value to stdout |
| `--target <spec>` | sticky tab | tab to act on (see [Targeting](#targeting-a-tab)) |
| `--timeout <dur>` | `30s` | max time to wait for the command |
| `--consent-timeout <dur>` | `120s` | how long to wait for Chrome's "Allow remote debugging?" prompt (a refused endpoint still fails fast) |
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
| `find <query> [--role <r>] [--limit <n>] [--region <name>] [--all] [--dedupe] [--min-score <s>]` | ranked element matches for a plain-language query, each with `ref`, exact name, `states`, `score`, and `center` |
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

#### `find` — describe the element, get its address

`snap` answers "what is on this page"; `find` answers "where is the thing I already know I want".
A short query — the element's purpose, a fragment of its text, or its hint text — is ranked against the accessibility tree, and the best matches come back with everything an acting verb needs:

```sh
chrome-cdp find "login button"
chrome-cdp find "delete" --region "Invoice 4102" --role button
chrome-cdp find "time type" --role textbox --limit 3
```

```json
{ "ok": true, "command": "find",
  "result": { "query": "login button",
    "matches": [
      { "ref": "e4821", "role": "button", "name": "Sign in to your account",
        "score": 0.91, "center": {"x": 640, "y": 412},
        "states": ["focusable"], "visible": true } ],
    "count": 1, "truncated": false } }
```

The `ref` feeds `--by ref`, the exact `name` feeds `--by name`, and `center` is a viewport CSS-pixel point.
This is the cure for verbose accessible names: `find "review"` returns the real name (`"Review Approval: Awaiting Action by …"`) so you never have to guess it.

Matching is a deterministic heuristic — token overlap plus role words (button, link, field, box, bar, checkbox, tab, menu, heading, row, icon) that softly steer the ranking — not a model call.
It handles descriptive queries, not paraphrase; for "the thing that saves my work" you still read a `snap`.
Role words are a nudge; `--role` is the hard filter.

Finding nothing is an answer, not an error: `count: 0`, exit 0.
`--min-score` drops weak matches, `--all` includes hidden/ignored nodes (ranked lower), and `--dedupe` collapses identical role+name pairs in virtualized grids.
On a backgrounded tab the a11y tree may be throttled; when it yields nothing there, `find` falls back to a DOM-computed pass (`note: "dom_fallback"`) whose matches carry centres but no refs, and `--region` is not honoured.

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

`--await` is opt-in.
`replMode` changes how bare object literals and `let`/`const` re-declaration behave, so plain `eval` keeps its existing semantics rather than changing silently under scripts that already work.

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
It cannot combine with `--fail-on-match`, and it is a usage error inside `session` or a recipe step, where it would break the one-envelope-per-line contract a batch promises.
A page that says nothing produces **no output at all** and exits 0 — there is no closing summary, because a terminating envelope would be a second shape for a caller to parse.
Treat empty stdout as "nothing was logged in the window", not as a failure.
A follow does not block other commands: you can drive the page from another terminal while one is running, and a follow longer than the daemon's idle window keeps it alive rather than being cut off mid-stream.

**`--no-daemon` has only partial history.**
Without the daemon there was no process alive to receive the tab's earlier events, so what appears is whatever Chrome replays when capture is enabled (recent console output and uncaught exceptions it still holds) plus what arrives during the command.
The read carries a `note` saying so, rather than passing a short list off as a full session record.

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
It is scoped to the same `--url` / `--method` / `--type` / `--since` filter as the listing, so a permanently open SSE stream or long poll does not make every read look unfinished forever.
`--status` and `--failed` are deliberately *not* applied to it: a request still in flight has no status, so applying them would make `pending` a constant zero for exactly the reads that ask about an outcome.

Each request carries `id`, `method`, `url`, `type`, `status`, `status_text`, `started_ms` (milliseconds since capture began on this tab), `duration_ms`, `request_size`, `response_size`, `from_cache`, `failed`, and `error`.
`status`, `duration_ms`, and `error` are `null` when they do not exist yet; `failed` means a non-2xx status **or** a network-level failure, so a delivered 500 and a DNS failure both show up under `--failed`.

**Redaction is on by default.**
This CLI drives your real, logged-in Chrome, so its buffers hold live session credentials by construction.
The values of `authorization`, `cookie`, `set-cookie`, `x-api-key`, `proxy-authorization`, and any header whose name contains `token`, `secret`, or `password` are replaced with `<redacted>` — the name stays, so a 401 is still diagnosable.
Credential-shaped URL query and fragment parameters (`access_token`, `api_key`, `sig`, `code`, `key`, …) are redacted the same way, including the query string of a hash-router fragment (`#/callback?access_token=…`).
Headers whose value is itself a URL (`location`, `content-location`, `referer`) go through the same URL redaction rather than being withheld wholesale, so the 302 that ends an OAuth flow stays readable without carrying the code.
`--no-redact` is the explicit, deliberate opt-out.

**Headers and bodies are absent, not null, unless you ask.**
Without `--headers` / `--body` those keys do not appear at all, so a routine listing stays small and does not spill tokens or PII into a log.

**Response bodies are fetched lazily, never buffered.**
They are pulled with `Network.getResponseBody` at read time, only when `--body` is passed — buffering every body would multiply the daemon's memory and retain payloads you never asked to see.
The consequence: **a body may already be gone** if the page navigated away, or if it is not UTF-8 text.
That is reported as `"response_body": null` with `"body_unavailable": true`, and the read still succeeds — a partial answer beats no answer.
Whether a body is text is judged on the payload Chrome delivered, not on what survives the cap, so the same image reports `body_unavailable` at any size.
Bodies over `net_max_body` (default 65536 bytes) are cut, with `"body_truncated": true`.
Request bodies arrive inline with the request, so they are retained and available retroactively; a request body that is not text is withheld the same way, as `"request_body": null` with `"request_body_unavailable": true`.

**Bodies are redacted too.**
Credential-shaped fields in form-encoded and JSON bodies (`password`, `access_token`, `client_secret`, `api_key`, …) are replaced with `<redacted>`, on requests and responses alike — a password is no less a secret for having travelled in a POST body than in the query string, which is already withheld.
The rest of the payload is reported exactly as sent, including a body the cap already cut.
A body in any other encoding (`multipart/form-data`, protobuf, a bare token) has no field structure to key on and is passed through unchanged, so treat `--body` output from those as sensitive.

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
It cannot combine with `--fail-on-match`, and it is a usage error inside `session` or a recipe step.
A window in which nothing completed produces **no output at all** and exits 0, exactly as with `console --follow`, and it does not block other commands against the same daemon.

**`--no-daemon` has only partial history**, exactly as with `console`: enabling the domain surfaces the handful of resources Chrome still holds for the page, never the session, so the read carries a `note` rather than passing a short list off as the whole story.

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
| `target` | a default `--target` for every step, overridden by a step's own `--target` or by `--target` on the run; takes `{{placeholders}}` like any argv element |

`{{name}}` substitutes an input **into one argv element**.
There is no shell anywhere in this design and there is no `shell:` step type, so a value is passed through byte for byte and nothing in it is interpreted.
Per-step flags a verb already accepts — `--timeout 60s`, `--by name`, `--wait-text` — go in that step's own `run` array, where a reader of the recipe can see them.

**`--timeout` on the run applies to each step, not to the run.**
Each step is one command and gets the whole budget, exactly as each line of a `session` does — so a 200-step recipe with `--timeout 60s` can take 200 minutes in the worst case, not one.
There is no whole-run budget on purpose: it would make `recipe run` and `recipe run --dry-run | chrome-cdp session` behave differently, and their equivalence is what keeps a recipe a `session` script with a header.
Give a slow step its own `--timeout` in its `run` array and leave the run's default low.

A step must name its command in the **first** element of `run`.
A leading flag (`run: ["--json", "snap"]`) is a load-time error: it would hide the command from validation, since the command tree resolves the verb only after stripping flags.

Which elements of a step are flags is decided by the recipe **as written**, never by an input value.
When a step is resolved, its data elements are emitted after a `--` terminator — so `run: ["text", "{{sel}}"]` with `--set sel=--target=@2` runs `text --target <the recipe's target> -- --target=@2`, and the value arrives as a selector rather than as a second `--target` pointing the step at another tab.
This is visible in `--dry-run` output, which is still exactly what runs.

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
**The process exit code is the failing step's**, so a shell caller branches on the same contract as for a single command — and it is always the exit that `failed.code` maps to, so the number and the envelope cannot disagree.
Under `--quiet` only the summary is printed.

Because the run's output is NDJSON, a step may not write raw bytes to stdout.
`screenshot -o -` and `pdf -o -` write the file itself to stdout and emit no envelope, so such a step fails with `usage` instead of corrupting the stream — give it a path (`-o shot.png`) and read the path back out of its envelope.
Streaming steps (`console --follow`, `net --follow`) are a `usage` error for the same reason, exactly as inside `session`.

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

### MCP server

`mcp` runs the CLI as a [Model Context Protocol](https://modelcontextprotocol.io) server over stdio, exposing the verbs as MCP tools.
It is a front end, not a fork: a tool call becomes the same argv you would type, runs through the same command tree against the same connection, and comes back as the same envelope — so a flow you debug at the shell behaves identically when an assistant runs it.

```jsonc
// claude_desktop_config.json, .mcp.json, or your client's equivalent
{ "mcpServers": { "chrome-cdp": { "command": "chrome-cdp", "args": ["mcp"] } } }
```

| Flag | Default | Purpose |
|------|---------|---------|
| `--read-only` | off | expose only tools that cannot modify page state |
| `--tools <set>` | `default` | `default`, `full`, or a comma-separated list of tool names |
| `--allow-eval` | off | expose `eval` and `raw`, which are denied in this mode by default |
| `--target <spec>` | — | pin the server to one tab; otherwise each tool takes a `target` |

Global flags (`--timeout`, `--no-daemon`, `--port`, `--profile-dir`, `--allow`, config file) apply unchanged, and the daemon still holds the connection — a long-lived server over one shared connection is exactly what it is for.
Transport is stdio only; there is no HTTP or SSE mode, because a network-reachable server driving your authenticated browser is a different security posture.

**A policy allow-list is required.**
`chrome-cdp mcp` refuses to start unless a `[policy]` table with a non-empty `allow` is configured (or `--allow` is passed): it exits 2 and prints the block it needs.
The CLI's unrestricted default is right for a person who typed a command; handing an assistant a browser that is signed in to everything is a different question, and it should be answered on purpose.
Run [`chrome-cdp policy init`](#policy) on the tab you want it to drive.
`--policy-off` is refused in this mode, and an injected one cannot reach the parser: the server freezes its own policy flags, and every tool argument is passed after a `--` terminator so a value that looks like a flag is data.

**`eval` and `raw` are denied here unless you pass `--allow-eval`.**
They can [navigate the tab themselves](#destination-checking-needs-verbs_denied), so an origin allow-list only means something while they are off — and the one-liner form of the gate (`chrome-cdp mcp --allow '*.example.com'`) writes no config file and so set no `verbs_denied` at all, which left the recommended setup with a decorative boundary.
The mode now supplies that default itself.
A denied verb's tool is not listed either: it could only answer `permission_denied`, and an agent pays for the description.
`--allow-eval` opts back in to the mode's default only — a `verbs_denied` you configured yourself still stands.

**The tool surface is bounded** — an agent pays for every tool description in its context window — so related verbs are grouped behind an `action` or `kind` argument.
Names are prefixed `chrome_cdp_` so they stay unambiguous in clients that flatten every server into one namespace.

| Tool | Wraps | Notes |
|------|-------|-------|
| `chrome_cdp_tabs` | `list`, `open`, `use`, `close`, `activate` | one `action` argument |
| `chrome_cdp_navigate` | `nav`, including back/forward/reload | |
| `chrome_cdp_snapshot` | `snap` | the primary read |
| `chrome_cdp_read` | `text`, `html`, `value`, `grid` | one `kind` argument |
| `chrome_cdp_click` | `click` | |
| `chrome_cdp_type_text` | `type`, `fill` | `replace: true` picks `fill` |
| `chrome_cdp_key` | `key` | |
| `chrome_cdp_pointer` | `hover`, `dblclick`, `rclick`, `drag` | one `action` argument |
| `chrome_cdp_select_option` | `select` | cascade paths included |
| `chrome_cdp_scroll` | `scroll` | |
| `chrome_cdp_upload` | `upload` | `upload_roots` still applies |
| `chrome_cdp_wait_for` | `wait` | every condition, `--request` included |
| `chrome_cdp_screenshot` | `screenshot` | returns an image content block |
| `chrome_cdp_console` | `console` | |
| `chrome_cdp_network` | `net` | |
| `chrome_cdp_evaluate` | `eval` | powerful and unconstrained; needs `--allow-eval` |
| `chrome_cdp_batch` | `session` | several tools over one round trip |
| `chrome_cdp_raw_cdp` | `raw` | `--tools full` (or named in `--tools`), plus `--allow-eval` |

Each tool's arguments mirror the CLI flags they wrap, in `snake_case` (`in_row` for `--in-row`), and the element-addressing arguments (`by`, `role`, `nth`, `match`, `in_row`, `wait`, `pierce`) are documented in every schema that takes them — the accessible-name addressing is this tool's advantage on real applications, and an agent only gets it if the schema says so.
The streaming forms (`console --follow`, `net --follow`) are not exposed: they would break the one-result-per-call contract.
`recipe` is not either — a recipe is authored and reviewed at the shell.

`--read-only` reuses the [policy layer's verb classification](#what-is-checked) rather than a second table, so it can never disagree with a `read_only` origin.
It exposes `tabs` (without `open` or `close`), `snapshot`, `read`, `wait_for`, `screenshot`, `console`, `network` and `batch`; invoking anything else by name returns a typed `usage` error rather than a protocol error.
`close` is withheld even though the classification table calls it exempt — it touches no page content, but it does change the browser, and a server that says it cannot modify anything should not close your tabs.

**`close` is bounded by the allow-list here**, unlike at a shell: an MCP client may close a tab only on an origin the policy permits, per tab, and a bulk close closes the permitted ones and reports the rest under `refused`.

**Results keep the contract.**
A success carries the envelope's `result` object as `structuredContent`, plus a one-line text summary.
A failure is `isError: true` with `structuredContent` carrying `code` and `exit` — and the recoverable details (`tab_hidden`, `occluded`, `zero_area`) — so an agent branches on the same values a shell script does rather than on prose.

**stdout is the protocol; diagnostics go to stderr.**
Nothing else may write to stdout while the server runs, and the process enforces that rather than trusting it.

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
| `-o, --output <path>` | `./record-<timestamp>.<ext>` | output path, or `-` for stdout (no envelope, as with `screenshot`/`pdf`) |
| `--format` | from the extension, else `gif` | `gif` \| `mp4` \| `webm` \| `frames` |
| `--max-size <size>` | — | best-effort ceiling, e.g. `2MB` or `1500000` |
| `--loop <n>` | `0` (forever) | how many times the GIF plays; `--loop 3` plays three times |
| `--annotate` | from `record start` | draw the action markers |

The result reports `path`, `format`, `frames`, `fps`, `duration_ms`, `width`, `height`, `bytes`, `annotated`, `decode_failures`, and the capture's own `dropped_frames` / `truncated` / `reason`.

**A failed export does not cost you the recording.**
Everything that can be checked before the frames are handed over is: the encoder's availability, and whether the output path can actually be written — a missing directory, a path that is itself a directory, or a directory you have no permission to write to are all errors with the recording untouched, so `record stop` can be retried at a different path.
If the write fails anyway (a full disk, an `ffmpeg` that dies) the frames are handed back to the daemon and the error says so, so the retry still works.
A frame the encoder cannot read is skipped and counted in `decode_failures` rather than failing the export; only a recording with no decodable frame at all is an error.

**Frames live in the daemon, not in the command that started the recording.**
That is what makes a run which crashed half way still have a recording of the failure: `record stop` afterwards writes it, and it writes it even when the tab itself has since closed (the result then says `tab_closed: true`).
A recording whose tab closed is held for ten minutes and then released, so an abandoned one does not stay a hole in a long-lived daemon.

**`--format frames` replaces the previous export's PNGs.**
A shorter recording written over a longer one removes the `frame-NNNNN.png` files the last export left, so the directory holds exactly the frames the result reports; anything else you keep in that directory is untouched.

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
For `mp4` and `webm` the reported `width`/`height`/`fps`/`duration_ms` are what ffmpeg actually wrote: the canvas is rounded down to even dimensions (yuv420p requires them) and the frame rate is floored at 1, which any recording containing a pause reaches.

**A window resized mid-recording is letterboxed, not stretched.**
The canvas comes from the first frame; a later frame with a different shape is scaled to fit and padded, so the export never shows a page at an aspect ratio it never had.

**`--max-size` is best-effort.**
It re-encodes at a smaller scale, and then at a lower frame count, until the file fits; the result reports `reduced` and the `export_scale` used when a reduction happened.
`within_max_size` is reported whenever `--max-size` was given at all — including when no reduction step was possible, since a small canvas with few frames is refused at the first step and misses the ceiling just the same.
The ladder is bounded by `--timeout` like everything else: a long recording can run out of time before it works through every step, and the result then carries `max_size_timed_out: true` alongside the best attempt that did finish, so a larger `--timeout` is a visible next move rather than a guess.

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
| `doctor` | probe the connection, report `ready` \| `consent_pending` \| `no_endpoint` \| `unverified`, and print the exact fix (`--no-probe` to connect to nothing) |
| `daemon start\|stop\|status` | manage the background connection |
| `policy init` | write a starter [`[policy]`](#policy) table allow-listing the current tab's origin (`--wildcard`, `--print`, `-o`) |
| `exit-codes` | print the exit-code table |
| `version` | print the version |
| `completion bash\|zsh\|fish\|powershell` | shell completion script |

## Connection model

`chrome-cdp` attaches to your **real** Chrome.
There are two ways to let it, and they are not equivalent.

**Recommended — launch Chrome with the flag.**
It never prompts:

```sh
open -a "Google Chrome" --args --remote-debugging-port=9222   # macOS
google-chrome --remote-debugging-port=9222                    # Linux
```

**Alternative — the `chrome://inspect/#remote-debugging` toggle.**
It works on the default profile where the classic flag does not (Chrome M136+ dropped `--remote-debugging-port` for the *default* profile, which is why the toggle exists), but it raises a consent prompt on **every fresh attach**.
That prompt is browser-modal: until it is answered, Chrome accepts no other input, so an unanswered one looks exactly like a crashed browser.
Read [The consent prompt](#the-consent-prompt) before choosing it.

Either way, `chrome-cdp` reads Chrome's `DevToolsActivePort` file and connects the WebSocket directly.
If no debug-enabled Chrome is found, it launches a managed Chrome on a dedicated profile alongside your real one.

A background **daemon** holds the connection, so the consent prompt appears once per session rather than once per command.
It starts lazily on first use and idles out after 30 minutes; manage it with `daemon start|stop|status`, or bypass it with `--no-daemon`.

### The consent prompt

On the `chrome://inspect` path, a fresh attach makes Chrome ask "Allow remote debugging?".
Three things about it are worth knowing before it happens:

- It is **modal to the whole browser**, not to a tab.
  Nothing else in Chrome responds until it is answered.
- It can sit **behind** the Chrome window, so the usual experience is a browser that appears frozen with no visible dialog.
- Answering it late is fine.
  `chrome-cdp` holds the connection open for `--consent-timeout` (default 120s) and connects the moment you click Allow.

If the wait runs out you get exit 3 with `error.code: consent_pending` and a message naming the dialog.
A **refused** endpoint is unaffected by any of this and still fails in milliseconds — only an open port whose upgrade is hanging earns the long wait.

While the prompt is up, `chrome-cdp` says so on stderr rather than waiting silently, on the daemon path and with `--no-daemon` alike.
A second command started during the wait says that it is queueing behind the first rather than opening a second connection, and if the first gives up with `consent_pending` the ones behind it inherit that answer instead of each raising a fresh prompt.

`--consent-timeout` (config key `consent_timeout`) is clamped to between `1s` and `10m`.
`0s` or a negative value means the 120s default rather than "do not wait", which would abandon the prompt the moment it was raised; the ceiling exists because the value is also how long a queued command can be held up.

### `doctor`

`chrome-cdp doctor` answers "can I connect?" by connecting, and reports one of four states:

| `state` | Means | Envelope |
|---------|-------|----------|
| `ready` | the WebSocket upgrade completed, or a running daemon answered a live CDP round trip | `ok: true` |
| `consent_pending` | the port accepted and went silent — Chrome is holding the prompt | exit 3, `consent_pending` |
| `no_endpoint` | nothing usable answered (no port file, a stale one, or another process on the port) | exit 3, `connection_failed` |
| `unverified` | `--no-probe`: an endpoint exists and nothing was checked | `ok: true` |

When the daemon is running AND it has just proved its connection to Chrome, `doctor` answers **through it** (`via: "daemon"`) and opens no new connection — probing is itself a connection request, and on the toggle path that is what raises the prompt.
A daemon that is merely *running* is not an answer: it holds its socket for its whole idle window, so quitting Chrome leaves a reachable daemon with a dead connection behind it, and `doctor` falls through to the probe rather than reporting ready.
Otherwise it says on stderr that it is about to connect, then probes (`via: "probe"`).
`--no-probe` reports only what the port file says, clearly marked `state: "unverified"`.

`doctor` honours `--port` like every other verb: `doctor --port 9333` diagnoses the Chrome on that port, not whichever one the `DevToolsActivePort` file names.

A `ready` verdict reached by probing says so: the probe's own connection is closed once it has its answer, so on the toggle path the next command is a fresh attach and can prompt again.
Run `chrome-cdp daemon start` to be asked once per session instead.

The result carries `state`, `via`, `probed`, and the endpoint it looked at (`endpoint`, plus `port_file` and `ws` where they apply).
The daemon-backed answer adds `running`, `connected`, `socket`, and `target_count` — a **count**, not the tab list: `doctor --json` is the first thing many callers run, and open tab titles and URLs are not an answer to "can I connect?".

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
| Tabs and meta (`list`, `use`, `close`, `activate`, `version`, `session`, `recipe run`, …) | `verbs_denied` only; no origin check, and every envelope's `target`, and every tab `list` or an ambiguous `close` enumerates, is reduced to a bare origin with no full URL and no title when the policy does not cover it |

A verb that is not classified is treated as **acting**, so a new verb over-restricts rather than slipping through.

`verbs_denied` is checked **first**, ahead of the class, so it reaches every verb including the tab and meta ones.
`verbs_denied = ["recipe run"]` therefore refuses running a saved recipe — a file someone else wrote, driving your authenticated browser — while leaving `recipe show` and `recipe run --dry-run` available for reading one.

`close` is the one exception to the last row, and only under [MCP mode](#mcp-server): there it is checked against `allow`/`deny` per tab.
At a shell you decided to close your own tab, and refusing it would produce an error a long way from its cause.
An assistant driving the browser under a boundary you wrote is a different caller, and a server that enforced the allow-list for reads but not for destruction would be enforcing half a boundary.
A bulk close under MCP closes the tabs the policy permits and reports the rest under `refused`.

Redirects are the honest limitation: a `nav` to an allowed origin that redirects elsewhere cannot be stopped, so the policy is re-evaluated on the **settled** URL and the *next* command is refused.

### Destination checking needs `verbs_denied`

`eval` and `raw` can navigate the tab themselves.
`eval "location='https://bank.example/'"` on an allowed tab issues an authenticated GET to an origin the allow-list would have refused, and no check in front of `nav` and `open` can see it coming.
What the policy still gives you is that the tab is then off-limits: the next command is refused on the settled origin, so nothing is read back.
But the request happened.

**So an origin allow-list is only meaningful alongside `verbs_denied = ["eval", "raw"]`**, and that is what `chrome-cdp policy init` writes — and what [MCP mode](#mcp-server) applies by default, whether or not a config file says so.
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
consent_timeout = "2m" # how long to wait for Chrome's consent prompt (1s-10m; 0 means the 120s default)
by = "search"          # default selector syntax
target = "url:github"  # default tab when neither --target nor `use` is set
```

A malformed config is a warning on stderr, not a fatal error — the CLI still runs on the built-ins.
The one exception is the [`[policy]`](#policy) table: a policy the CLI cannot read makes it refuse to act rather than act unbounded, and that covers a file that cannot be *read* (wrong permissions, a bad mount) as well as one that does not parse.

`XDG_CONFIG_HOME` chooses **which** file is read, so an environment that points it at a directory without one leaves you with no `[policy]` table at all.
No `CHROME_CDP_*` variable can set a policy key, but that is a statement about the table's contents, not about which file supplies them.
When `XDG_CONFIG_HOME` is set and there is no config file there, the CLI says so on stderr rather than letting a boundary disappear quietly.
