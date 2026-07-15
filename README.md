# chrome-cdp

Drive your **already-running local Chrome** — your real tabs, logins, and extensions — from the command line over the Chrome DevTools Protocol (via [chromedp](https://github.com/chromedp/chromedp)).
Usable by a human and by an AI agent: every command speaks a uniform JSON envelope and a stable exit-code contract.

> This is an initial implementation of the design in `.scratch/chrome-cdp-cli/spec.md` (produced by a `/wayfinder` map).
> The core spine — connection layer, target resolution, the command loop, the output contract, and the raw escape hatch — is built and tested. See **Status** below for what's deferred.

## Install

```sh
go install github.com/sanketsudake/chrome-cdp-cli/cmd/chrome-cdp@latest
# or, from a clone:
go build -o chrome-cdp ./cmd/chrome-cdp
```

## Connect

`chrome-cdp` attaches to your real Chrome via the one-time **`chrome://inspect/#remote-debugging`** toggle (it reads Chrome's `DevToolsActivePort` file and connects the WebSocket directly — the classic `--remote-debugging-port` no longer works on the default profile since Chrome M136). If no debug-enabled Chrome is found, it launches a managed Chrome on a dedicated profile alongside your real one.

```sh
chrome-cdp doctor    # check the connection and get the exact fix if it's not ready
```

## Use

```sh
chrome-cdp list                     # list open tabs (id, title, url)
chrome-cdp use url:github           # set the sticky current tab
chrome-cdp snap                     # accessibility-tree snapshot (see the page)
chrome-cdp nav https://example.com  # navigate (waits for load)
chrome-cdp click "#submit"          # click (auto-waits for the element)
chrome-cdp type "#q" "hello"        # type via real keystrokes
chrome-cdp eval "document.title"    # evaluate JS
chrome-cdp screenshot               # PNG -> ./screenshot-<timestamp>.png
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

Failures use the same envelope with `"ok": false` and an `error{code,message,…}`, plus a nonzero exit code:
`0` ok · `1` generic · `2` usage · `3` connection · `4` target/timeout · `5` cdp · `6` daemon.

## Security

- **Loopback only.** chrome-cdp connects to `127.0.0.1` and never binds the debug port to a non-loopback interface. It never suppresses Chrome's "Allow debugging?" consent dialog or the automation banner.
- **A live debug endpoint = full control** of whatever Chrome is authenticated to. Treat it like a local root shell into your browser's sessions; only enable `chrome://inspect` when you intend to automate.
- **Don't pass secrets as arguments.** `type <selector> <text>` takes the text as a positional argument, which is visible in `ps` and shell history. A secret-safe input path (stdin / `--secret-file`) is deferred — until then, don't type passwords through it on a shared machine.
- The managed-launch fallback uses your system Chrome with a dedicated profile; it does **not** disable the sandbox.

## Status

**Implemented & tested:** the connection ladder + `DevToolsActivePort` reader, target-grammar resolution, the uniform envelope + exit-code contract, and commands `list`, `use`, `nav`, `snap`, `eval`, `click`, `type`, `screenshot`, `raw`, `doctor`, `daemon (status)`, `exit-codes`, `version`. Verified with unit tests, a golden output-contract test, an in-process + subprocess command-boundary suite, and an integration test that drives a real headless Chrome.

**Deferred (next increments, per the spec):**
- **Shared daemon** — commands currently connect per invocation ("direct-connect"). Without the daemon, attach mode may leave one stray helper tab and re-prompts "Allow" more often. The daemon (attach-and-hold) resolves both.
- More verbs from the spec (`html`, `text`, `value`, `attr`, `cookie`, `console`, `network`, `wait`, `emulate`, `pdf`, `--frame`, `--pierce`, live observation).
- Some universal globals from the spec: **`--by`** (selector syntax; verbs are CSS-only for now), **`--no-wait`/`--wait`**, and **`--port`** (connection is `DevToolsActivePort`-driven for now).
- Richer error classification (action failures are currently mapped heuristically to `target/timeout` vs `cdp`).
- goreleaser/Homebrew packaging, TOML config, shell completion.

## Develop

```sh
go test ./...              # full suite (spawns a headless Chrome for the integration test)
go test -short ./...       # skip the live-Chrome integration test
```

Architecture: `internal/result` (envelope + exit codes), `internal/target` (grammar), `internal/browser` (connection logic, no chromedp), `internal/chrome` (chromedp-backed `Browser`), `internal/state` (sticky target), `internal/cli` (cobra command tree). The full design and rationale live in `.scratch/chrome-cdp-cli/`.
