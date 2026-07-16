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

```sh
chrome-cdp list --url outlook          # just the Outlook tabs
chrome-cdp open https://example.com    # returns the new tab's id
```

### Reading the page

| Command | Returns |
|---------|---------|
| `snap [--role <r>] [--grep <re>] [--region <name>] [--dedupe]` | accessibility snapshot: roles + names of actionable nodes, plus `alerts`, `focused`, per-node `states`/`value`/`ref` |
| `grid [selector]` | a table/grid as `{headers, rows, count}` |
| `value <selector> [--all]` | a form field's value (`--all`: every match, as a list) |
| `text <selector>` | visible text of a selector (or the page) |
| `html <selector> [--inner]` | outer (or inner) HTML |
| `eval <js>` | evaluate JS in the top frame |

The `snap` filters run server-side, so a read returns just the relevant nodes instead of a whole page.

```sh
chrome-cdp snap --role button --grep "[AP]M"    # calendar-event buttons only
chrome-cdp grid                                 # read a table without parsing snap
chrome-cdp value --all "input.hours"            # every hour cell in one call
```

### Acting

| Command | Does |
|---------|------|
| `click <selector>` | click at the element's occlusion-verified centre |
| `type <selector> <text>` | type via real keystrokes (**appends**; end with `\n` to press Enter) |
| `fill <selector> <value>` | set a field, **replacing** its content (clears, then types) |
| `select <field> <option>` | choose an option in a prompt / combobox / cascade / native `<select>` |
| `scroll [selector] [--dx <p>] [--dy <p>] [--to] [--wheel]` | scroll by a delta, `--to` a selector into view, or a real `--wheel` |
| `attr get\|list\|set\|rm <selector> [name] [value]` | read/write element attributes |

`click` / `type` / `fill` / `select` also take **`--wait-text "<substr>"`** — after the action, block until the page contains the text (a `Saved` toast), folding act-and-confirm into one call.

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
| `screenshot [-o <path>]` | a PNG (to cwd, or `-o -` for stdout) |
| `pdf [-o <path>]` | a PDF of the page |

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
| `exit-codes` | print the exit-code table |
| `version` | print the version |
| `completion bash\|zsh\|fish\|powershell` | shell completion script |

## Connection model

`chrome-cdp` attaches to your **real** Chrome via the one-time `chrome://inspect/#remote-debugging` toggle; it reads Chrome's `DevToolsActivePort` file and connects the WebSocket directly (the classic `--remote-debugging-port` flag no longer works on the default profile since Chrome M136).
If no debug-enabled Chrome is found, it launches a managed Chrome on a dedicated profile alongside your real one.

A background **daemon** holds the connection, so Chrome's "Allow debugging?" prompt appears once per session rather than once per command.
It starts lazily on first use and idles out after 30 minutes; manage it with `daemon start|stop|status`, or bypass it with `--no-daemon`.

Run `chrome-cdp doctor` to check the connection and get the exact fix when it isn't ready.

## Configuration

Persist flags you'd otherwise retype in `$XDG_CONFIG_HOME/chrome-cdp/config.toml` (usually `~/.config/chrome-cdp/config.toml`); see [`config.example.toml`](../config.example.toml) for the full key set.

```toml
json = true            # default to machine-readable output
timeout = "10s"
by = "search"          # default selector syntax
target = "url:github"  # default tab when neither --target nor `use` is set
```

A malformed config is a warning on stderr, not a fatal error — the CLI still runs on the built-ins.
