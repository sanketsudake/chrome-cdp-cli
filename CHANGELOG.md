# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

PR: [#31](https://github.com/sanketsudake/chrome-cdp-cli/pull/31).

### Added

- `--endpoint <url>` — attach to an explicit `ws://` or `http://` debug endpoint, wired through `CHROME_CDP_ENDPOINT` / the `endpoint` config key and the daemon.
- Chromium-family browser detection — Chrome, Chromium, Brave, Edge, Vivaldi, and Arc `DevToolsActivePort` files are found automatically; `CHROME_CDP_BROWSER_BIN` (`browser_bin` config key) launches a different browser for the managed fallback.
- Windows release target — `windows/amd64` and `windows/arm64` zip archives, and the short test suite now runs on `windows-latest` in CI.
- `chrome-cdp skill` verb — serves the embedded `drive-chrome-cdp` agent skill (`--full` for the complete text, a short stub otherwise) so an agent can fetch the skill without a separate install step.
  Two scenario skills (`check-logged-in`, `fill-grid-and-confirm`) and their evals ship alongside it.
- `--session <name>` / `CHROME_CDP_SESSION` — namespaces the sticky current tab, so several agents can share one Chrome without stealing each other's tab (see [Several agents, one Chrome](docs/cli-reference.md#several-agents-one-chrome)).
  Connection flags (`--endpoint`, `--port`, `--profile-dir`, `--session` itself) are frozen into `session`'s per-line defaults for the whole batch, so a stdin line with none of its own doesn't silently reset to the config default mid-batch.
- `screenshot --annotate` — numbered reference markers with a legend in the envelope ([RFC-0016](docs/rfc/0016-screenshot-annotate.md)).
- `net --har` — exports retained requests as a HAR 1.2 file ([RFC-0017](docs/rfc/0017-har-export.md)).
- `dialog status|accept|dismiss` — act on a native dialog already on screen ([RFC-0018](docs/rfc/0018-dialog-verb.md)).
- `storage local|session` — read, write, and clear `localStorage`/`sessionStorage` ([RFC-0019](docs/rfc/0019-web-storage.md)).
- npm shim `@sanketsudake/chrome-cdp` — `npx @sanketsudake/chrome-cdp` / `npm i -g` downloads the matching release binary, with version validation and temp-file cleanup on every path.
- README positioning section and explicit non-goals, contrasting `chrome-cdp` with agent-browser, chrome-devtools-mcp, and Playwright-style tools.

### Changed

- The managed-launch browser knob was renamed from the ambiguous `CHROME_CDP_BIN` to `CHROME_CDP_BROWSER_BIN` / `browser_bin`.
- The short test suite (`go test -short`) is now OS-neutral: paths, case-insensitive filesystem handling, and `.exe` suffixes are handled per-platform.

### Fixed

- A malformed `endpoint` config value is dropped instead of bricking the CLI.
- An explicit `--endpoint` never falls back to a managed launch, and a TLS scheme (`wss://`/`https://`) is refused rather than silently accepted.
- `--endpoint` is frozen into the MCP runner's per-call defaults and a recipe run's per-step defaults, so neither re-entrant command tree silently resets it to the config/env default mid-batch.

## [0.2.2] - 2026-08-16

### Fixed

- `--by cell` ranks candidates by grid, re-resolves replaced nodes, and names the cover; `key` reports `focused_id` ([#30](https://github.com/sanketsudake/chrome-cdp-cli/pull/30)).

## [0.2.1] - 2026-07-28

### Fixed

- Grid-cell addressing, an empty-selector usage error, and stale-daemon visibility ([#29](https://github.com/sanketsudake/chrome-cdp-cli/pull/29)).
- The RFC 6455 accept-key GUID, so any attach can connect ([#26](https://github.com/sanketsudake/chrome-cdp-cli/pull/26)).

### Docs

- `text`, `type`, `select`, and `open` guidance in the `drive-chrome-cdp` skill, corrected from a live run ([#28](https://github.com/sanketsudake/chrome-cdp-cli/pull/28)).

## [0.2.0] - 2026-07-28

Baseline tagged release; see [Releases](https://github.com/sanketsudake/chrome-cdp-cli/releases) for the full history up to this point.

[Unreleased]: https://github.com/sanketsudake/chrome-cdp-cli/compare/v0.2.2...HEAD
[0.2.2]: https://github.com/sanketsudake/chrome-cdp-cli/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/sanketsudake/chrome-cdp-cli/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/sanketsudake/chrome-cdp-cli/releases/tag/v0.2.0
