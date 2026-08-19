# RFCs

Design proposals for `chrome-cdp`, written before the code so the CLI surface, the result envelope, and the exit-code contract are agreed up front.

Each RFC is self-contained: motivation, user stories with acceptance criteria, the proposed command surface, the envelope shape, and a verification plan that maps to real tests.
An RFC is `Draft` until someone implements it; the implementing PR flips it to `Accepted` and links itself.

**RFCs 0001–0015 are implemented and merged; 0016 and 0017 are Accepted, implemented on branch `feat/agent-browser-parity`, pending a PR; 0018 is Draft.**
The merged ones are kept as the design record — what was proposed, why, and what the verification plan was — not as a to-do list.
Where the implementation departed from the proposal, the RFC's own Open Questions section records the decision, and the PR records the reason.

RFCs 0014 and 0015 are a second wave, currently Draft: a capability gap analysis (2026-07-27) against coordinate-first automation surfaces found the remaining holes concentrated in pixel-space interaction and element discovery.

## Why these, and why in this order

Written when none of it existed.
Kept in the present tense of the time, because the reasoning is the point: this is what the tool could not do, and why that ordering.

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

RFC-0013 was not part of that plan.
It came out of a failure the work itself caused — several processes attaching at once, each raising Chrome's consent prompt, and the browser wedging with no button that responded.
The reproduction then contradicted the first diagnosis, which is why it is written up rather than just fixed.

## Index

| RFC | Title | Priority | Area | Status |
|-----|-------|----------|------|--------|
| [0001](0001-keyboard-key-verb.md) | Keyboard input: the `key` verb | P0 | input | Accepted — [#10](https://github.com/sanketsudake/chrome-cdp-cli/pull/10) |
| [0002](0002-console-messages.md) | Console messages: the `console` verb | P0 | observability | Accepted — [#13](https://github.com/sanketsudake/chrome-cdp-cli/pull/13) |
| [0003](0003-network-requests.md) | Network requests: the `net` verb | P0 | observability | Accepted — [#13](https://github.com/sanketsudake/chrome-cdp-cli/pull/13) |
| [0004](0004-mcp-server-mode.md) | MCP server mode: `chrome-cdp mcp` | P0 | distribution | Accepted — [#15](https://github.com/sanketsudake/chrome-cdp-cli/pull/15) |
| [0005](0005-pointer-verbs.md) | Pointer verbs: `hover`, `dblclick`, `rclick`, `drag` | P1 | input | Accepted — [#10](https://github.com/sanketsudake/chrome-cdp-cli/pull/10) |
| [0006](0006-file-upload.md) | File upload: the `upload` verb | P1 | input | Accepted — [#12](https://github.com/sanketsudake/chrome-cdp-cli/pull/12) |
| [0007](0007-tab-lifecycle.md) | Tab lifecycle: `close`, `activate`, history navigation | P1 | tabs | Accepted — [#10](https://github.com/sanketsudake/chrome-cdp-cli/pull/10) |
| [0008](0008-screenshot-options.md) | Screenshot options: element, full-page, region, format | P1 | capture | Accepted — [#11](https://github.com/sanketsudake/chrome-cdp-cli/pull/11) |
| [0009](0009-recipes.md) | Recipes: saved, shareable `session` scripts | P2 | workflow | Accepted — [#14](https://github.com/sanketsudake/chrome-cdp-cli/pull/14) |
| [0010](0010-page-reading-ergonomics.md) | Page-reading ergonomics: `text --article`, `eval --await` | P2 | reading | Accepted — [#11](https://github.com/sanketsudake/chrome-cdp-cli/pull/11) |
| [0011](0011-session-recording.md) | Session recording: `record` and GIF export | P2 | capture | Accepted — [#14](https://github.com/sanketsudake/chrome-cdp-cli/pull/14) |
| [0012](0012-domain-allowlist.md) | Domain allow-list: bounding what the CLI may drive | P2 | safety | Accepted — [#12](https://github.com/sanketsudake/chrome-cdp-cli/pull/12) |
| [0013](0013-consent-prompt-lifecycle.md) | Surviving Chrome's consent prompt | P0 | connection | Accepted — [#18](https://github.com/sanketsudake/chrome-cdp-cli/pull/18) |
| [0014](0014-coordinate-space-interaction.md) | Coordinate-space interaction: `--at`, `tripleclick`, drop-zone upload, `window` | P0/P1 | input | Accepted — [#22](https://github.com/sanketsudake/chrome-cdp-cli/pull/22), [#23](https://github.com/sanketsudake/chrome-cdp-cli/pull/23) |
| [0015](0015-find-element-search.md) | `find`: ranked element search from a plain-language query | P0 | reading | Accepted — [#21](https://github.com/sanketsudake/chrome-cdp-cli/pull/21) |
| [0016](0016-screenshot-annotate.md) | `screenshot --annotate`: numbered element labels with a legend in the envelope | P2 | capture | Accepted — pending PR |
| [0017](0017-har-export.md) | `net --har`: export the tab's retained requests as HAR 1.2 | P2 | observability | Accepted — pending PR |
| [0018](0018-dialog-verb.md) | `dialog`: inspect and close a native dialog that is already on screen | P1 | input | Draft |

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

0005 pointer ──> 0014 --at extends its dispatch primitive; tripleclick joins its verb family
0006 upload ───> 0014 drop-zone mode extends it (0012's upload allow-list applies unchanged)
0008 screenshot ──> 0014's coordinate contract is defined against --scale 1 captures
snap refs ────> 0015 find mints the same e<id> refs and reuses snap's traversal
0014 --at <──> 0015 find (find's center points feed --at when refs go stale)

0008 screenshot ─────────┐
0011 encode markers ─────┼──> 0016 screenshot --annotate (labels mapped through 0008's clip, drawn by 0011's marker code;
snap refs + 0015 filter ─┘                                legend centers follow 0014's coordinate contract)

0003 net ──────────────┐
0011 encode (pure) ────┴──> 0017 net --har (the listing's rows, redaction and filters unchanged; the HAR 1.2 encoder lives beside 0011's)

0002 console (attach-time listenCapture hook) ─┐
--on-dialog (withDialog, the per-action handler) ─┴──> 0018 dialog status|accept|dismiss (the same listener, retained per tab instead of per action;
                                                       only the daemon's long-lived attach can see a dialog that opened before the command)
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
