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
| `--for <dur>` | a fixed duration (fallback) |

```sh
chrome-cdp wait --idle                 # after nav/open on an SPA
chrome-cdp wait --text "Success"       # confirm a write landed
```

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

Host matching is case-insensitive, and a URL with no host at all (`about:blank`, a `data:` URL, `chrome://settings`) matches nothing — so an `allow` list refuses it.
A pattern the CLI cannot parse is a **fatal** error: unlike the rest of the config, which warns and carries on, a policy that could not be read refuses to run, because a policy that fails open is worse than no policy.
Use `--policy-off` to run while you fix it.

### What is checked

| Verb class | Checked against |
|-----------|-----------------|
| Acting (`click`, `type`, `fill`, `select`, `scroll`, `key`, pointer verbs, `upload`, `attr set/rm`, `cookie set/rm/clear`, `headers`, `emulate`, `eval`, `raw`) | `allow`/`deny`, `read_only`, `verbs_denied` |
| Reading (`snap`, `text`, `html`, `value`, `grid`, `screenshot`, `pdf`, `frame`, `wait`, `attr get/list`, `cookie list`) | `allow`/`deny`, `verbs_denied` |
| Navigating (`nav <url>`, `open`) | the **destination** origin, before navigating |
| Tabs and meta (`list`, `use`, `close`, `activate`, `version`, …) | not checked; `list` shows non-allowed tabs as a bare origin, without the full URL or the title |

A verb that is not classified is treated as **acting**, so a new verb over-restricts rather than slipping through.

Redirects are the honest limitation: a `nav` to an allowed origin that redirects elsewhere cannot be stopped, so the policy is re-evaluated on the **settled** URL and the *next* command is refused.

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

### Audit log

`audit_log` is append-only NDJSON, one record per decision:

```json
{"ts":"2026-07-26T09:12:03Z","origin":"other.test","verb":"click","decision":"refused","rule":"allow: no match"}
```

Refusals are always recorded; set `audit_all = true` to record permitted actions too.
It records the **origin**, never the URL, and never any value — no typed text, no cookie values, no selectors — because a log that captured those would be the most sensitive file this tool produces.

## Configuration

Persist flags you'd otherwise retype in `$XDG_CONFIG_HOME/chrome-cdp/config.toml` (usually `~/.config/chrome-cdp/config.toml`); see [`config.example.toml`](../config.example.toml) for the full key set.

```toml
json = true            # default to machine-readable output
timeout = "10s"
by = "search"          # default selector syntax
target = "url:github"  # default tab when neither --target nor `use` is set
```

A malformed config is a warning on stderr, not a fatal error — the CLI still runs on the built-ins.
The one exception is the [`[policy]`](#policy) table: a policy the CLI cannot read makes it refuse to act rather than act unbounded.
