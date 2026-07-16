# chrome-cdp

Drive your **real, already-running Chrome** — your actual tabs, logins, cookies, and extensions — from the command line.
Built for humans and AI agents alike: every command speaks one JSON envelope and one stable exit-code contract.

[![CI](https://github.com/sanketsudake/chrome-cdp-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/sanketsudake/chrome-cdp-cli/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/sanketsudake/chrome-cdp-cli.svg)](https://pkg.go.dev/github.com/sanketsudake/chrome-cdp-cli) [![Go Report Card](https://goreportcard.com/badge/github.com/sanketsudake/chrome-cdp-cli)](https://goreportcard.com/report/github.com/sanketsudake/chrome-cdp-cli)

```sh
chrome-cdp open https://example.com     # open a tab, get its id
chrome-cdp snap --role button           # see what's clickable — by accessible name
chrome-cdp click --by name "Sign in"    # act by meaning, not a brittle CSS id
```

Because it attaches to the browser you're already using, an app you're signed into loads **authenticated** — no headless browser, no second login, no credential ever typed.

## Quickstart

1. **Install** — macOS via Homebrew, or Go:

   ```sh
   brew install sanketsudake/tap/chrome-cdp
   # or:
   go install github.com/sanketsudake/chrome-cdp-cli/cmd/chrome-cdp@latest
   ```

2. **Let Chrome accept a debugger** — open `chrome://inspect/#remote-debugging` and toggle it on (a one-time consent; `chrome-cdp` never suppresses it).

3. **Check the connection** — `chrome-cdp doctor` confirms it's ready, or prints the exact fix.

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

A live debug endpoint is **full control** of whatever your Chrome is signed into — treat enabling `chrome://inspect` like opening a local root shell into your browser's sessions, and only do it when you intend to automate.

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

Not yet licensed — a `LICENSE` file will be added before the first tagged release.
