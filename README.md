# chrome-cdp

Drive your **already-running local Chrome** — your real tabs, logins, and extensions — from the command line over the Chrome DevTools Protocol (via [chromedp](https://github.com/chromedp/chromedp)).
Usable by a human and by an AI agent: every command speaks a uniform JSON envelope and a stable exit-code contract.

> This is an initial implementation of the design in `.scratch/chrome-cdp-cli/spec.md` (produced by a `/wayfinder` map).
> The core spine — connection layer, target resolution, the command loop, the output contract, and the raw escape hatch — is built and tested.
> See **Status** below for what's deferred.

## Install

```sh
# Homebrew (macOS):
brew install sanketsudake/tap/chrome-cdp

# go install:
go install github.com/sanketsudake/chrome-cdp-cli/cmd/chrome-cdp@latest

# or grab a prebuilt binary from the GitHub Releases page, or build from a clone:
go build -o chrome-cdp ./cmd/chrome-cdp
```

## Connect

`chrome-cdp` attaches to your real Chrome via the one-time **`chrome://inspect/#remote-debugging`** toggle (it reads Chrome's `DevToolsActivePort` file and connects the WebSocket directly — the classic `--remote-debugging-port` no longer works on the default profile since Chrome M136).
If no debug-enabled Chrome is found, it launches a managed Chrome on a dedicated profile alongside your real one.

A background **daemon** holds the CDP connection, so Chrome's "Allow debugging?" prompt appears once per session (not once per command) and subsequent commands are a fast socket round-trip.
It starts lazily on first use and idles out after 30 minutes; manage it with `chrome-cdp daemon start|stop|status`, or bypass it with `--no-daemon`.

```sh
chrome-cdp doctor    # check the connection and get the exact fix if it's not ready
```

## Use

```sh
chrome-cdp list                     # list open tabs (id, title, url)
chrome-cdp use url:github           # set the sticky current tab
chrome-cdp snap                     # accessibility-tree snapshot (see the page)
chrome-cdp nav https://example.com  # navigate (waits for load)
chrome-cdp html "#main"             # outer HTML of a selector (or the page)
chrome-cdp text ".title"            # visible text of a selector
chrome-cdp click "#submit"          # click (auto-waits for the element)
chrome-cdp click --by search "Sign in"   # match by DevTools text/XPath/CSS search
chrome-cdp click --by name "Request Absence" --role button   # match by ARIA accessible name
chrome-cdp type "#q" "hello"        # type via real keystrokes
chrome-cdp select "Time Type" "Project Plan Tasks > ShiftLeft: Qwiet" --role textbox  # drive a prompt/combobox/cascade
chrome-cdp grid                     # read a table/grid as {headers, rows} (a11y)
chrome-cdp scroll --dy 600          # scroll (or --to <sel> into view, --wheel for lazy grids)
chrome-cdp eval "document.title"    # evaluate JS
chrome-cdp wait --url "/dashboard"  # wait until the tab's URL settles (or --visible/--gone/--for)
chrome-cdp screenshot               # PNG -> ./screenshot-<timestamp>.png (or -o -)
chrome-cdp raw --list               # list the connected Chrome's CDP domains
chrome-cdp raw Network.setCacheDisabled '{"cacheDisabled":true}'   # any CDP method
```

Target a tab with `--target <idprefix|url:<s>|title:<s>|@N>`, or set it once with `use`.
Add `--json` for machine-readable output; branch on the exit code (`chrome-cdp exit-codes`).

### Output contract

Every `--json` command emits one envelope:

```json
{ "ok": true, "command": "eval", "target": {"id":"…","title":"…","url":"…"},
  "result": { "value": "…" }, "elapsed_ms": 12 }
```

Failures use the same envelope with `"ok": false` and an `error{code,message,…}`, plus a nonzero exit code: `0` ok · `1` generic · `2` usage · `3` connection · `4` target/timeout · `5` cdp · `6` daemon.

## Configure

Persist the flags you'd otherwise retype in an optional TOML file at `$XDG_CONFIG_HOME/chrome-cdp/config.toml` (usually `~/.config/chrome-cdp/config.toml`).
See [`config.example.toml`](config.example.toml) for the full set of keys.

```toml
json = true            # default to machine-readable output
timeout = "10s"
by = "search"          # default selector syntax
target = "url:github"  # default tab when neither --target nor `use` is set
no_daemon = false
```

Precedence, highest first: **command-line flags > `CHROME_CDP_*` env vars > config file > built-in defaults**.
So `CHROME_CDP_BY=id` overrides the file, and an explicit `--by css` overrides everything.
A malformed config is a warning on stderr, not a fatal error — the CLI still runs on the built-ins.

Shell completion is built in (cobra): `chrome-cdp completion bash|zsh|fish|powershell` — see `chrome-cdp completion --help` for how to load it.

## Security

- **Loopback only.** chrome-cdp connects to `127.0.0.1` and never binds the debug port to a non-loopback interface.
  It never suppresses Chrome's "Allow debugging?" consent dialog or the automation banner.
- **A live debug endpoint = full control** of whatever Chrome is authenticated to.
  Treat it like a local root shell into your browser's sessions; only enable `chrome://inspect` when you intend to automate.
- **Don't pass secrets as arguments.**
  `type <selector> <text>` takes the text as a positional argument, which is visible in `ps` and shell history.
  A secret-safe input path (stdin / `--secret-file`) is deferred — until then, don't type passwords through it on a shared machine.
- The managed-launch fallback uses your system Chrome with a dedicated profile; it does **not** disable the sandbox.

## Status

**Implemented & tested:** the connection ladder + `DevToolsActivePort` reader, target-grammar resolution, the uniform envelope + exit-code contract, selector options (`--by` incl.
`name` = ARIA accessible-name addressing with `--role`/`--nth`, `--wait`, `--no-wait`), the connection globals (`--port`, `--profile-dir`, `--no-launch`) and output globals (`--json`, `--no-color`, `-v`, `--no-input`, `--quiet`, `--timeout`), and commands `list`, `use`, `nav`, `snap`, `html`, `text`, `value`, `eval`, `click`, `type`, `select` (prompt/combobox/cascade/native-`<select>`), `grid` (a11y table read), `scroll` (`--dy`/`--to`/`--wheel`), `attr` (get/list/set/rm), `screenshot`, `pdf`, `cookie` (list/set/rm/clear), `headers set`, `emulate` (viewport/geo/reset), `frame list`, `wait` (`--url`/`--visible`/`--gone`/`--for`), `--pierce` (shadow-DOM/iframe piercing), `raw` (incl.
`--browser`/`--list`), `doctor`, `daemon` (start/stop/status), `exit-codes`, `version` (and `--version`).
Plus an optional TOML config file (flags > `CHROME_CDP_*` env > config > defaults), shell completion (`completion`), and goreleaser + Homebrew-cask packaging with a CI matrix (Linux/macOS) and a tag-driven release workflow.
Verified with unit tests, a golden output-contract test, an in-process + subprocess command-boundary suite, RPC round-trip tests for the daemon, and an integration test that drives a real headless Chrome.

**Deferred (next increments, per the spec):**
- The streaming/interception verbs: `console`, `network log`, `mock`, `block`, `download wait` (they hang on the always-on observation model), plus `perm` (reachable today via `raw Browser.grantPermissions`).
- Explicit `--frame <selector>` element scoping (the same-origin `FromNode` two-phase resolve the catalog flagged as its own sub-effort).
  `frame list` and `--pierce` (which reaches shadow DOM + iframes via DevTools search) are done; cross-origin frames are also reachable today as their own `--target` (they are separate CDP targets).
- Richer error classification (action failures are currently mapped heuristically to `target/timeout` vs `cdp`).

## Develop

```sh
go test ./...              # full suite (spawns a headless Chrome for the integration test)
go test -short ./...       # skip the live-Chrome integration test
```

Architecture: `internal/result` (envelope + exit codes), `internal/target` (grammar), `internal/browser` (connection logic, no chromedp), `internal/chrome` (chromedp-backed `Browser`), `internal/state` (sticky target), `internal/cli` (cobra command tree).
The full design and rationale live in `.scratch/chrome-cdp-cli/`.
