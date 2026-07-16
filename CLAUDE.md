# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`chrome-cdp` is a Go CLI that drives the user's **already-running** local Chrome over the DevTools Protocol (via chromedp).
It reuses the live session's logins and cookies, and speaks one JSON envelope + one stable exit-code contract to both humans and AI agents.

## Detailed guidance

Read the resource file that matches your task before making changes:

- **[Architecture](.claude/resources/architecture.md)** — the envelope/exit-code contract, the `internal/` package map, the `chrome.Browser` seam, and the connection/daemon model. Read this before touching anything cross-cutting.
- **[Development](.claude/resources/development.md)** — build, test (`go test -short` to skip live Chrome), lint, what CI runs, and release.
- **[Test-writing guidelines](.claude/resources/test-writing-guidelines.md)** — how tests are structured here (stub-driven unit tests, `testing.Short()`-guarded live-Chrome tests) and which conventions are adopted vs. deviated from.

## The two things most likely to trip you up

- **The result envelope is public API.** Every command emits one `result.Envelope`; both humans and the Claude skill parse it. A new failure mode needs a `Code*` constant *and* a `codeToExit` entry, or it silently degrades to `ExitGeneric`. Never let human-formatting changes alter the JSON shape. Validate usage/args *before* connecting to Chrome.
- **The CLI never connects to Chrome directly.** `cli.App` holds function seams (`WithConnector`, `WithStickyTarget`, `WithDaemonCtl`) that `cmd/chrome-cdp/main.go` wires to the daemon. To add a browser capability: extend the `chrome.Browser` interface → add a default in `chrometest.StubBrowser` (one place) → implement in `internal/chrome`.

## Docs & style

User-facing docs live in `docs/` and `README.md`; the Agent Skill lives in `skills/drive-chrome-cdp/`.
When editing any markdown, follow the repo style: **one sentence per line**.
Push branches and let the user open PRs unless asked otherwise.
