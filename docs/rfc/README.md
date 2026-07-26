# RFCs

Design proposals for `chrome-cdp`, written before the code so the CLI surface, the result envelope, and the exit-code contract are agreed up front.

Each RFC is self-contained: motivation, user stories with acceptance criteria, the proposed command surface, the envelope shape, and a verification plan that maps to real tests.
An RFC is `Draft` until someone implements it; the implementing PR flips it to `Accepted` and links itself.

## Why these, and why in this order

`chrome-cdp` is already strong where browser automation is usually weak: a stable JSON envelope, a stable exit-code contract, batched execution over one connection (`session`), settle-aware waiting (`wait --idle` / `--stable`), and element addressing that survives dynamic-id apps (`--by name` / `cell` / `label` / `in-row`).

The gaps are concentrated in three places:

1. **Input primitives.**
   The CLI can click and type, but cannot press a key, hover, drag, double-click, or attach a file.
   Whole interaction classes — dismiss a modal, move focus, edit a grid cell, open a context menu, upload a document — have no expressible form.
2. **Observability.**
   There is no way to read what the page logged or what it requested.
   `raw` is request/response only and cannot subscribe to CDP events, so the escape hatch does not cover this either.
   The most common reason a developer reaches for a browser automation tool — "why is this page broken" — is currently unserved.
3. **Distribution.**
   The CLI is excellent for humans and shell scripts, and has one Claude skill.
   It has no protocol-level integration, no shareable saved workflows, and no way for a cautious team to bound what it may touch.

The ordering below follows that: make the interaction surface complete, then make the browser observable, then make the tool easy to adopt and safe to hand to an agent.

## Index

| RFC | Title | Priority | Area | Status |
|-----|-------|----------|------|--------|
| [0001](0001-keyboard-key-verb.md) | Keyboard input: the `key` verb | P0 | input | Draft |
| [0002](0002-console-messages.md) | Console messages: the `console` verb | P0 | observability | Draft |
| [0003](0003-network-requests.md) | Network requests: the `net` verb | P0 | observability | Draft |
| [0004](0004-mcp-server-mode.md) | MCP server mode: `chrome-cdp mcp` | P0 | distribution | Draft |
| [0005](0005-pointer-verbs.md) | Pointer verbs: `hover`, `dblclick`, `rclick`, `drag` | P1 | input | Draft |
| [0006](0006-file-upload.md) | File upload: the `upload` verb | P1 | input | Draft |
| [0007](0007-tab-lifecycle.md) | Tab lifecycle: `close`, `activate`, history navigation | P1 | tabs | Draft |
| [0008](0008-screenshot-options.md) | Screenshot options: element, full-page, region, format | P1 | capture | Draft |
| [0009](0009-recipes.md) | Recipes: saved, shareable `session` scripts | P2 | workflow | Draft |
| [0010](0010-page-reading-ergonomics.md) | Page-reading ergonomics: `text --article`, `eval --await` | P2 | reading | Draft |
| [0011](0011-session-recording.md) | Session recording: `record` and GIF export | P2 | capture | Draft |
| [0012](0012-domain-allowlist.md) | Domain allow-list: bounding what the CLI may drive | P2 | safety | Draft |

## Dependency graph

```
0001 key ─────────┐
0005 pointer ─────┼──> 0009 recipes (richer scripts are worth saving)
0006 upload ──────┘
0007 tab lifecycle ──> mitigates the `tab_hidden` limitation that 0001/0005 inherit

0002 console ─┐
              ├──> shared daemon-side CDP event buffer (design once, in 0002)
0003 net ─────┘

0004 mcp ──> exposes whatever verbs exist; ships best after 0001/0005/0006/0007
0012 allowlist ──> should land with or before 0004 (protocol access widens the blast radius)
```

## Conventions every RFC in this folder inherits

- **The envelope is public API.**
  A new command emits exactly one `result.Envelope`.
  A new failure mode needs a `Code*` constant *and* a `codeToExit` entry, or it silently degrades to `ExitGeneric`.
- **Validate before connecting.**
  Usage and argument errors must be `usage` / exit 2 without ever touching Chrome.
- **New `chrome.Browser` methods get a permissive default in `chrometest.StubBrowser`** — one place, so every existing test keeps compiling.
- **Every verb that addresses an element reuses `QueryOpts`** — `--by`, `--wait`, `--role`, `--nth`, `--match`, `--in-row`, `--pierce` — rather than inventing a second addressing scheme.
- **Every new verb works inside `session`**, because that is how agents drive the CLI.
- **Tests follow [the test-writing guidelines](../../.claude/resources/test-writing-guidelines.md)**: table-driven, stub-backed unit tests for the command boundary, `testing.Short()`-guarded live-Chrome tests for the driver.
- **Docs are one sentence per line**, and every shipped verb lands in [`docs/cli-reference.md`](../cli-reference.md) in the same PR.
