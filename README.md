# chrome-cdp

Drive your **real, already-running Chrome** — your actual tabs, logins, cookies, and extensions — from the command line.
Built for humans and AI agents alike: every command speaks one JSON envelope and one stable exit-code contract.

[![Go Reference](https://pkg.go.dev/badge/github.com/sanketsudake/chrome-cdp-cli.svg)](https://pkg.go.dev/github.com/sanketsudake/chrome-cdp-cli)
[![CI](https://github.com/sanketsudake/chrome-cdp-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/sanketsudake/chrome-cdp-cli/actions/workflows/ci.yml)
[![CodeQL](https://github.com/sanketsudake/chrome-cdp-cli/actions/workflows/codeql.yml/badge.svg)](https://github.com/sanketsudake/chrome-cdp-cli/actions/workflows/codeql.yml)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/sanketsudake/chrome-cdp-cli)](go.mod)

```sh
chrome-cdp open https://example.com     # open a tab, get its id
chrome-cdp snap --role button           # see what's clickable — by accessible name
chrome-cdp click --by name "Sign in"    # act by meaning, not a brittle CSS id
```

Because it attaches to the browser you're already using, an app you're signed into loads **authenticated** — no headless browser, no second login, no credential ever typed.

## Quickstart

1. **Install** — macOS via Homebrew, or Go:

   ```sh
   brew install --cask sanketsudake/tap/chrome-cdp
   # or:
   go install github.com/sanketsudake/chrome-cdp-cli/cmd/chrome-cdp@latest
   ```

   Recent Homebrew may print a tap-trust notice for third-party taps on first install.
   The install still proceeds; to acknowledge it explicitly, run `brew trust --cask sanketsudake/tap/chrome-cdp` first.

2. **Let Chrome accept a debugger.**
   Launch it with the flag — this never prompts:

   ```sh
   open -a "Google Chrome" --args --remote-debugging-port=9222   # macOS
   google-chrome --remote-debugging-port=9222                    # Linux
   ```

   Or, to attach to a Chrome that is already running on the default profile, toggle `chrome://inspect/#remote-debugging` on.
   That path raises a consent prompt on **every fresh attach**, and the prompt is modal to the whole browser — until it is answered Chrome accepts no other input, and it can sit behind the window, so an unanswered one looks like a crash.
   `chrome-cdp` waits for it (see `--consent-timeout`) and never suppresses it.

3. **Check the connection** — `chrome-cdp doctor` actually connects and reports `ready`, `consent_pending`, or `no_endpoint`, with the exact fix.

4. **Drive it:**

   ```sh
   chrome-cdp use url:github        # pick a tab; later commands need no --target
   chrome-cdp snap                  # read the page as an accessibility tree
   chrome-cdp click --by name "New" --role button
   ```

That's the whole loop: **`list → use → snap → act → verify`**.
The [logged-in web app guide](docs/scenarios/automating-a-logged-in-web-app.md) walks it end to end.

## Why chrome-cdp

- **Your real session.**
  It drives the Chrome you're logged into — real cookies, SSO, extensions — so there's nothing to authenticate and no secret to store.
- **Address by meaning.**
  Target controls by ARIA **accessible name**, visible **label**, or grid **cell** — not CSS ids that change every session.
  Reads and writes survive the app's cosmetic churn.
- **Made for automation.**
  One JSON envelope, one [exit-code contract](docs/cli-reference.md#output-contract); a background daemon holds the connection so the consent prompt appears once per session, not per command.
- **Drives the hard widgets.**
  Portal menus, multi-level cascade prompts, and native `<select>`s that a synthetic click can't open — the [`select`](docs/scenarios/driving-widgets-with-select.md) verb opens them.
- **Works on modern Chrome.**
  It reads Chrome's `DevToolsActivePort` and connects directly, so it keeps working where the classic `--remote-debugging-port` flag stopped (default profile, Chrome M136+).

## A taste of the commands

```sh
chrome-cdp list --url outlook              # list tabs (--url/--title filter)
chrome-cdp snap --role button --grep "[AP]M"   # filter the a11y tree server-side
chrome-cdp grid                            # read a table as {headers, rows}
chrome-cdp fill --by cell "Mon, 7/13" "8"  # a grid input by its column header
chrome-cdp fill --by label "Notes" "hi"    # a form control by its visible label
chrome-cdp select "Time Type" "Projects > Acme: Platform > Project > Time Entry" --role textbox
chrome-cdp click --by name "Approve" --role button --wait-text "Success"   # act, then confirm
chrome-cdp wait --idle                     # settle an SPA (network, not a fixed sleep)
chrome-cdp raw Network.setCacheDisabled '{"cacheDisabled":true}'   # any CDP method
```

Full command, flag, and exit-code tables live in the **[CLI reference](docs/cli-reference.md)**.

## For AI agents

`chrome-cdp` is meant to be a tool an agent *calls*: `snap` returns the page's actionable structure as text (roles, names, states, refs) instead of pixels, every result is one parseable envelope, and failures classify by exit code so the agent branches on a number, not on prose.
See **[Using chrome-cdp from an AI agent](docs/using-with-ai-agents.md)**.

An [Agent Skill](https://docs.claude.com/en/docs/claude-code/skills) that teaches the whole loop ships in [`skills/drive-chrome-cdp`](skills/drive-chrome-cdp/SKILL.md) — point your harness at it.

## Use it from an MCP client

`chrome-cdp mcp` runs the same verbs as a [Model Context Protocol](https://modelcontextprotocol.io) server on stdio — one entry in your client's config, no wrapper to write:

```jsonc
{ "mcpServers": { "chrome-cdp": { "command": "chrome-cdp", "args": ["mcp"] } } }
```

It exposes a deliberately small set of tools (grouped, `chrome_cdp_`-prefixed, with a `batch` that collapses a five-step interaction into one round trip), and every result is the same envelope the CLI prints — a failure comes back with its `code` and `exit`, not as prose.

**It requires a policy allow-list and refuses to start without one.**
Run `chrome-cdp policy init` on the tab you want it to drive first: at a shell you decided to run each command, but an assistant holding this connection can act as you on every site you are signed into, and that deserves an explicit boundary.
`eval` and `raw` are denied in this mode unless you pass `--allow-eval` — they can navigate the tab out of the allow-list themselves, which would make the boundary decorative.
`chrome-cdp mcp --read-only` is a good way to try it — it exposes only verbs that cannot change a page or close a tab.
See the [MCP section of the CLI reference](docs/cli-reference.md#mcp-server).

## Configure

Persist flags you'd otherwise retype in `~/.config/chrome-cdp/config.toml` — see [`config.example.toml`](config.example.toml).

```toml
json = true            # default to machine-readable output
timeout = "10s"
target = "url:github"  # default tab when neither --target nor `use` is set
```

Precedence, highest first: **command-line flag > `CHROME_CDP_*` env var > config file > built-in default**.
Shell completion is built in: `chrome-cdp completion bash|zsh|fish|powershell`.

## Security

A live debug endpoint is **full control** of whatever your Chrome is signed into — treat enabling remote debugging (by flag or by toggle) like opening a local root shell into your browser's sessions, and only do it when you intend to automate.

- **Loopback only.**
  It connects to `127.0.0.1` and never binds the debug port to a non-loopback interface.
  It never suppresses Chrome's "Allow debugging?" consent or the automation banner.
- **Don't pass secrets as arguments.**
  `type <selector> <text>` takes text as a positional argument, visible in `ps` and shell history — don't type passwords through it on a shared machine.
- **Managed-launch fallback** uses your system Chrome with a dedicated profile and does not disable the sandbox.

## Documentation

- [CLI reference](docs/cli-reference.md) — commands, flags, exit codes, output contract.
- [Automating a logged-in web app](docs/scenarios/automating-a-logged-in-web-app.md) — the core loop.
- [Forms and grids](docs/scenarios/forms-and-grids.md) — label / cell addressing and batched fills.
- [Driving widgets with `select`](docs/scenarios/driving-widgets-with-select.md) — menus, cascades, native selects.
- [Using chrome-cdp from an AI agent](docs/using-with-ai-agents.md) — the agent-tool design.

## Develop

```sh
go build -o chrome-cdp ./cmd/chrome-cdp
go test ./...          # spawns a headless Chrome for the integration tests
go test -short ./...   # skip the live-Chrome tests
```

Architecture: `internal/result` (envelope + exit codes), `internal/target` (target grammar), `internal/browser` (connection logic), `internal/chrome` (chromedp-backed driver), `internal/daemon` (the held-connection RPC), `internal/cli` (the cobra command tree).

## License

Released under the [MIT License](LICENSE).
