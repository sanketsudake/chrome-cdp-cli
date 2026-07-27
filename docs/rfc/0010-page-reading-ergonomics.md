# RFC-0010: Page-reading ergonomics — `text --article` and `eval --await`

- **Status:** Accepted — implemented in [#11](https://github.com/sanketsudake/chrome-cdp-cli/pull/11)
- **Priority:** P2
- **Area:** reading
- **Depends on:** —

## Summary

Two small, independent improvements to how the CLI reads a page:

1. `text --article` — extract the main readable content of a page, stripped of navigation, ads, cookie banners, and boilerplate.
2. `eval --await` — evaluate JavaScript with REPL semantics: top-level `await` works, and the last expression's value is returned without an explicit `return`.

They are grouped because both are small, both target the same friction (getting useful data out of a page without ceremony), and neither justifies its own RFC.

## Motivation

**`text` returns everything.**
On a real page, the visible text is 80% chrome — nav bars, footers, cookie notices, related links, newsletter prompts.
A caller who wants the article, the ticket description, or the error page's actual message has to either write a selector for a container whose class changes weekly, or post-process a wall of text.
For an agent, the noise is charged directly against a context budget; for a human piping to a file, it is manual cleanup.
Chrome's own Reader Mode proves the extraction is tractable, and the heuristic (Readability-style scoring of block elements by text density and link ratio) is well-understood.

**`eval` requires ceremony that trips people up.**
The current `eval` evaluates an expression, so the two most natural things a user types both fail:

```sh
chrome-cdp eval 'await fetch("/api/me").then(r => r.json())'   # top-level await
chrome-cdp eval 'const t = document.title; t'                   # statements then a value
```

The first is a syntax error outside an async context; the second is a statement list, not an expression.
Users hit this immediately, work around it with `(async () => …)()` and IIFE wrappers, and the workaround is verbose enough to be error-prone in shell quoting.
CDP's `Runtime.evaluate` already supports `awaitPromise` and `replMode`; the ergonomic fix is passing them.

Neither of these adds capability so much as removes friction — which is exactly the kind of thing that determines whether a first-time user keeps using a tool.

## User stories

**US-1 — Read an article without the furniture.**
As a user extracting content, I want just the main body text so that I do not have to strip navigation and footers myself.
*Acceptance:* `chrome-cdp text --article` on a content page returns the body text with nav, footer, and cookie banner excluded.

**US-2 — Read a page cheaply as an agent.**
As an agent summarising a page, I want the smallest faithful representation so that I spend context on content rather than boilerplate.
*Acceptance:* on a typical article page, `--article` output is substantially shorter than `text` output while retaining the body.

**US-3 — Get structure with the text.**
As a user converting a page to notes, I want optional markdown structure so that headings, lists, and links survive.
*Acceptance:* `chrome-cdp text --article --markdown` preserves heading levels, list items, and link targets.

**US-4 — Know what got extracted.**
As a script author, I want the extraction to report the title, byline, and how much it kept so that I can detect a page where extraction failed.
*Acceptance:* the envelope reports `title`, `excerpt`, `chars`, and `ratio` (kept ÷ total).

**US-5 — Not be silently wrong.**
As a user, I want a page with no article-like content to say so rather than return a plausible-looking fragment.
*Acceptance:* when extraction confidence is low, the envelope sets `extracted: false` and returns full text, with the reason stated.

**US-6 — Await a promise directly.**
As a user, I want `await` to work at the top level so that I can call an API from the page's authenticated context in one line.
*Acceptance:* `chrome-cdp eval --await 'await fetch("/api/me").then(r => r.json())'` returns the parsed JSON.

**US-7 — Write statements, get the last value.**
As a user, I want to write a few statements and have the final expression returned so that I do not need an IIFE.
*Acceptance:* `chrome-cdp eval --await 'const rows = [...document.querySelectorAll("tr")]; rows.length'` returns the count.

**US-8 — See a rejection as an error, not a value.**
As a script author, I want a rejected promise to be a nonzero exit with the error message so that a failed fetch does not look like success.
*Acceptance:* an awaited rejection exits nonzero with the rejection's message in `error.message`.

## Proposed CLI surface

```
chrome-cdp text [selector] [--article] [--markdown] [--min-chars <n>]
chrome-cdp eval <js> [--await] [--timeout <dur>]
```

| Flag | Applies to | Default | Purpose |
|------|-----------|---------|---------|
| `--article` | `text` | off | extract main content only |
| `--markdown` | `text --article` | off | preserve structure as markdown |
| `--min-chars <n>` | `text --article` | `250` | below this, treat extraction as failed |
| `--await` | `eval` | off | REPL semantics: top-level await, last-expression value |

Examples:

```sh
chrome-cdp text --article
chrome-cdp text --article --markdown -o notes.md
chrome-cdp eval --await 'await fetch("/api/me").then(r => r.json())'
chrome-cdp eval --await 'const t = [...document.querySelectorAll("tr")]; t.length'
```

## Result envelope

`text --article`:

```json
{ "ok": true, "command": "text",
  "target": {"id":"…","title":"…","url":"…"},
  "result": { "text": "…", "title": "Quarterly report",
              "byline": "A. Author", "excerpt": "First paragraph…",
              "chars": 4821, "total_chars": 24193, "ratio": 0.199,
              "extracted": true, "format": "text" },
  "elapsed_ms": 74 }
```

`eval --await` keeps the existing shape, with the resolved value in `result.value` and `awaited: true` recorded so a caller knows which path ran.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| `--markdown` or `--min-chars` without `--article`; `--article` with a selector argument | `usage` | 2 |
| JS syntax error (both modes) | `cdp_error` | 5 |
| Awaited promise rejects | `cdp_error` | 5, with the rejection message and stack in `error.details` |
| `--await` evaluation exceeds `--timeout` | `target_timeout` | 4 |
| Extraction produced too little content | `ok` with `extracted: false` | 0 |

Low-confidence extraction is deliberately **not** an error (US-5): the caller gets full text plus an honest flag, because failing a read that did return usable text would be unhelpful.

## Design notes

**`text --article`**

- **Extraction runs in the page** as an injected, self-contained script evaluated in an isolated world, so it cannot be observed or interfered with by page code and leaves no globals behind.
  It must not mutate the live DOM — score a cloned subtree, never the document the user is also automating.
  A reading verb that changes the page under an automation would be a genuinely nasty bug.
- **Algorithm:** Readability-style scoring — candidate block elements scored on text length, comma count, and inverse link density; the top-scoring subtree wins; known boilerplate roles and landmarks (`nav`, `header`, `footer`, `aside`, `[role=navigation]`, `[aria-hidden=true]`) are excluded up front.
  This is well-trodden; the risk is not the algorithm but scope creep in tuning it.
- **`ratio` and `min-chars` are the honesty mechanism.**
  Extraction is a heuristic and will sometimes pick the wrong subtree.
  Reporting how much was kept lets a caller detect it; `extracted: false` is the explicit signal.
- **`--markdown`** emits headings, lists, links, code blocks, and blockquotes.
  Deliberately not a general HTML-to-markdown converter — tables, footnotes, and embedded media are out of scope, and the docs should say so rather than half-supporting them.
- **Interface:** extend the existing signature rather than adding a method: ```go Text(ctx context.Context, targetID, selector string, opts TextOpts) (map[string]any, error) ``` with `TextOpts{Article, Markdown bool; MinChars int; Query QueryOpts}`.
  This is a breaking change to `chrome.Browser` and `StubBrowser`, contained in-tree.

**`eval --await`**

- Maps to `Runtime.evaluate` with `awaitPromise: true` and `replMode: true`.
  `replMode` is what makes statement-then-expression work and is exactly what DevTools' own console uses, so behaviour matches what users already expect from that console.
- **Rejections must surface as errors, not values.**
  `Runtime.evaluate` returns an `exceptionDetails` for a rejected promise; mapping that to a successful envelope with an odd-looking value would be the worst possible outcome (US-8).
  This is the single most important correctness point in the RFC.
- **Timeouts** are enforced by the existing command context; a never-settling promise must not hang past `--timeout`, and the connection must remain usable afterwards.
- **Should `--await` be the default?**
  It is more useful and matches DevTools.
  But changing `eval`'s semantics silently would alter behaviour for existing scripts — `replMode` changes how bare object literals and `let` redeclarations are treated.
  Opt-in now; consider flipping the default in a major version.
  See Open Questions.
- **Interface:** `Eval(ctx, targetID, expr string, opts EvalOpts) (any, error)` with `EvalOpts{Await bool}`.

## Verification scenarios

**VS-1 — Article extraction drops boilerplate** Given a fixture with `<nav>`, `<footer>`, a cookie banner, and an `<article>` of known text When `text --article` runs Then the output contains the article text and none of the nav, footer, or banner strings.

**VS-2 — Ratio and counts are reported** On the same fixture, `chars` matches the extracted length, `total_chars` exceeds it, and `ratio` is their quotient.

**VS-3 — Low-confidence extraction is flagged, not faked** Given a fixture that is only a navigation menu When `text --article` runs Then `extracted` is false, the full text is returned, and the exit code is 0.

**VS-4 — `--min-chars` threshold** Given a fixture with a 100-character article and `--min-chars 250` Then `extracted` is false; with `--min-chars 50`, it is true.

**VS-5 — Markdown preserves structure** Given a fixture with `h1`/`h2`, a `ul`, and a link When `--article --markdown` runs Then the output contains `#`, `##`, `- `, and `[text](href)` with the correct href.

**VS-6 — Extraction does not mutate the page** Given a fixture recording DOM mutations via `MutationObserver` into `window.__mutations` When `text --article` runs Then `eval "window.__mutations.length"` is 0, and no extraction globals exist on `window`.
This is the regression test that matters most in this RFC.

**VS-7 — Flag validation** Table: `--markdown` without `--article`; `--min-chars` without `--article`; `--article` with a selector.
All `usage` exit 2, no browser call.

**VS-8 — Top-level await resolves** Given a fixture defining `window.p = new Promise(r => setTimeout(() => r(42), 50))` When `eval --await 'await window.p'` runs Then `result.value` is 42 and `awaited` is true.

**VS-9 — Statements then a value** When `eval --await 'const a = 1; const b = 2; a + b'` runs Then `result.value` is 3.

**VS-10 — Rejection is an error** Given `eval --await 'await Promise.reject(new Error("boom"))'` Then the exit is nonzero, `error.message` contains `boom`, and `result` does not present it as a successful value.

**VS-11 — Syntax errors are reported in both modes** Table over a malformed expression with and without `--await`: both exit 5 with the syntax error message.

**VS-12 — Timeout does not wedge the connection** Given `eval --await 'await new Promise(() => {})'` with `--timeout 2s` Then the command exits 4 within the timeout, and a following `eval "1+1"` on the same connection succeeds.

**VS-13 — Existing `eval` behaviour is unchanged** Without `--await`, current expressions evaluate exactly as before — a regression guard written before the change.

**VS-14 — Fetch from the page's authenticated context** Given a local `httptest` server requiring a cookie the fixture page holds When `eval --await 'await fetch("/api/me").then(r => r.json())'` runs Then the parsed JSON is returned, demonstrating the use case that motivates US-6.

## Test plan

**Unit — flag validation (`internal/cli`, stub failing on any browser call, `t.Parallel()`)** VS-7, plus `--min-chars` bounds.

**Unit — extraction scoring** If the scoring is implemented as an injected JS blob, it is not directly unit-testable in Go, which is a reason to keep the Go side thin and cover scoring through the live-Chrome fixtures below.
Whatever is testable in Go — threshold application, ratio computation, `extracted` determination — should be pure functions with their own table-driven tests, so the heuristic's *reporting* is verified even when the heuristic itself is only covered end-to-end.

**Live Chrome (`internal/chrome`, `testing.Short()`-guarded, not parallel)** VS-1 through VS-6 and VS-8 through VS-14 against `data:` fixtures plus an `httptest` server for VS-14.
Fixtures should include a realistic article page, a navigation-only page, a single-page-app shell, and a page with multiple plausible article candidates — the last is where extraction most often picks wrong, and having it in the suite keeps tuning honest.

**Non-mutation test (VS-6)** Called out separately because it is the correctness property that protects every other command running in the same session.

**Regression guard (VS-13)** Written before the `Eval` signature change, so the existing contract is pinned first.

## Out of scope

- Converting entire pages to markdown with full fidelity (tables, footnotes, embedded media).
- Content extraction tuned per site or via user-supplied rules.
- Summarisation of any kind — this returns text, it does not condense it.
- Changing `eval`'s default semantics in this RFC.

## Open questions

1. Should `--await` eventually become the default?
   DevTools behaves this way and users expect it.
   **Recommendation:** ship opt-in, gather usage, flip in a major version with a changelog entry — the `replMode` semantic differences are real enough that a silent change would break someone.
2. Should the extractor be a vendored, well-tested implementation rather than a hand-rolled one?
   Hand-rolling a Readability variant is a known tar pit.
   **Recommendation:** vendor a small, license-compatible implementation as an embedded JS asset and treat it as a dependency to update, rather than as code to maintain.
3. Should `--article` also apply when a selector is given (extract within that subtree)?
   Currently a `usage` error.
   **Recommendation:** keep it an error for now; the combination has no clear semantics and can be added later without breaking anything.
4. Should `text --article` gain `--json`-friendly structure (paragraphs as an array) for agents that want to chunk?
   **Recommendation:** defer until someone asks; `chars` and the text itself cover the current need.
