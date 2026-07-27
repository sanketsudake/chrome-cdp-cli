# Architecture

`chrome-cdp` attaches to the user's **already-running** Chrome over the DevTools Protocol (CDP) and drives it from the command line.
It never launches a headless browser for real work — the whole point is to reuse the live session's cookies, logins, and extensions.

## The one-envelope, one-exit-code contract

Every command emits a single `result.Envelope` (`internal/result/result.go`) and exits with a code derived from it.
This contract is the load-bearing interface: both humans and the Claude skill parse against it, so treat it as public API.

- `error.code` strings (fine-grained, e.g. `target_not_found`) map to process exit codes via `result.codeToExit` / `ExitCodeFor`.
- Exit codes are the coarse, stable contract: `0` ok, `2` usage, `3` connection, `4` target/timeout, `5` cdp, `6` daemon.
- Adding a new failure mode means adding a `Code*` constant **and** its `codeToExit` entry — an unmapped code silently degrades to `ExitGeneric`.
- Usage/validation errors (exit `2`) are checked **before** touching Chrome, so a bad flag never launches or connects.

## Package map (`internal/`)

Data flows outermost → innermost: `cli` parses → resolves a `target` → gets a `chrome.Browser` (via `daemon` or direct) → emits a `result`.

- `result` — the envelope, `Err`, and the exit-code table. No dependencies; the root of the contract.
- `target` — the target grammar (`idprefix | url:<s> | title:<s> | @N`) and `Resolve` against a tab list.
- `config` — layered defaults: built-in < config file (`~/.config/chrome-cdp/config.toml`) < `CHROME_CDP_*` env < flag. `Builtin()`, `Resolve()`, `FromEnv()`.
- `browser` — endpoint discovery and classification: finds Chrome's `DevToolsActivePort` file, computes the per-endpoint key, and probes the debug endpoint's WebSocket upgrade (`WSState`, `AwaitUpgrade`) — see connection model below.
- `chrome` — the `Browser` interface and its chromedp-backed implementation: snapshot, click/type/fill/select, grid, wait, raw CDP. The real driver logic lives here.
- `chrometest` — `StubBrowser`, a permissive `chrome.Browser` double embedded by the `cli` and `daemon` tests.
- `state` — the sticky current-target store, keyed per endpoint so distinct `--port`s don't share a "current tab".
- `daemon` — the held-connection RPC: a background process holds the CDP attach so Chrome's consent prompt appears once per session, not per command.
- `cli` — the cobra command tree (`app.go` = wiring + envelope emission, `commands.go` = the verbs). Knows nothing about how the browser connects; `main` injects that.

## Dependency injection at the seams (`cmd/chrome-cdp/main.go`)

`cli.App` is deliberately ignorant of daemons, sockets, and process spawning — it holds function seams that `main` wires up:

- `WithConnector` — how to get a `chrome.Browser` (daemon client vs. `--no-daemon` direct connect), invoked lazily only when a command needs Chrome.
- `WithStickyTarget` — get/set the current target; keyed by `ConnOpts` so each endpoint has its own.
- `WithDaemonCtl` — start/stop/status for the per-endpoint daemon.
- `WithDefaults` — inject config+env defaults (tests keep `config.Builtin()`).

This is why tests can inject a `chrometest.StubBrowser` directly and never spawn a process.
When adding a command that needs a new capability, add the method to the `chrome.Browser` interface, give it a default in `chrometest.StubBrowser` (one place), then implement it in `internal/chrome`.

## Connection & daemon model

- The **endpoint key** derives from the port file + effective `--port`; it names both the daemon socket and the sticky-state file.
  It cannot be computed once at startup — `--port` isn't known until cobra parses flags — so `main` computes it per command from `ConnOpts`.
- The **daemon** (`chrome-cdp __daemon <socket>`, a hidden mode) holds one CDP connection for ~30 min and serves commands over a Unix socket, so the "Allow debugging?" consent fires once.
  `--no-daemon` bypasses it and connects directly (used by tests and one-shot scripts).
- Chrome M136+ dropped the classic `--remote-debugging-port` for the default profile; `browser` reads `DevToolsActivePort` and connects directly, which is why it keeps working where older tools broke.
- The **consent prompt** is a third connection state, not a failure (RFC-0013).
  While Chrome holds "Allow remote debugging?" it accepts the TCP connect and then stalls the WebSocket upgrade forever — no error, only silence — so `browser.WSState` is three-way (`WSRefused` / `WSPending` / `WSReady`) and `DecideConnection` maps an open-but-hanging endpoint to its own `ConsentPending` action.
  The daemon holds that upgrade open for `consent_timeout` (default 120s) and publishes a `<socket>.pending` marker so `Ensure` extends its own deadline instead of declaring a live daemon dead; a refused endpoint still fails in milliseconds, which is what makes the long wait safe.
  Never lead a failure message with the `chrome://inspect` toggle: `browser.EnableAdvice` is the one authored answer, and it recommends `--remote-debugging-port` first because that path never prompts.

## Human vs. JSON rendering

`--json` emits the raw envelope; otherwise `renderHuman` prints a terse line (`✓ …` / `✗ …` to stderr), honoring `NO_COLOR` and `--quiet`.
The human path is a courtesy; the JSON envelope is the contract. Never let a human-formatting change alter the envelope shape.
