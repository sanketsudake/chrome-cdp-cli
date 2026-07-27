# RFC-0015: `find` — ranked element search from a plain-language query

- **Status:** Accepted — implemented in [#21](https://github.com/sanketsudake/chrome-cdp-cli/pull/21)
- **Priority:** P0
- **Area:** reading
- **Depends on:** — (builds on `snap`'s a11y-tree primitive and `e<id>` refs)
- **Related:** RFC-0010 (page-reading ergonomics), RFC-0014 (`find` returns centre points that feed `--at`)

## Summary

Add a `find <query>` verb: take a short plain-language description of an element ("login button", "search bar", "delete icon in the Invoices row") and return a small ranked list of matching elements — each with its `e<id>` ref, role, accessible name, state, and centre point — ready to feed any acting verb via `--by ref` or `--at`.

Matching is a deterministic heuristic over the accessibility tree (token overlap, role words, visibility, interactivity), not a model call.
The goal is to collapse today's read-parse-retry loop (`snap` → scan hundreds of nodes → guess the exact accessible name → act → miss → repeat) into one call.

## Motivation

Element *discovery* is the weakest step in the tool's loop today, and it is the step agents spend the most tokens on:

- **`snap` returns everything; the caller wants one thing.**
  A snapshot of a heavy enterprise page runs to hundreds of nodes.
  `--role`, `--grep`, and `--region` narrow it, but `--grep` is a regex against a name the caller does not know yet — which is circular when the whole problem is discovering the name.
- **Accessible names are not visible labels.**
  Live driving has repeatedly hit controls whose visible text is "Review" while the accessible name is "Review Approval: Awaiting Action by …".
  `--match contains` helps once you know a fragment; `find` is how you learn the fragment in the first place.
- **Every miss costs a full round trip.**
  For an agent, a wrong guess is a failed action, a re-snapshot, a re-parse, and another attempt — the single largest source of wasted turns when driving the CLI.
- **The building blocks already exist.**
  `snap` already resolves the full a11y tree cross-frame and cross-shadow-DOM, computes visibility, emits stable `e<id>` refs, and knows widget states.
  `find` is a ranking layer over that machinery, not a new resolution engine.

`find` is a read verb: it never dispatches input, so it is safe by construction and belongs in every tool surface including read-only MCP mode.

## User stories

**US-1 — Find by purpose.**
As an automation author, I want to describe an element by what it is for so that I do not need to know its exact accessible name.
*Acceptance:* on a page whose submit control is named "Sign in to your account", `chrome-cdp find "login button"` ranks that button first.

**US-2 — Discover the real accessible name, then act.**
As an agent, I want the ref and exact name of my best match so that my next command addresses it precisely.
*Acceptance:* `find "review button"` returns `e4821` / "Review Approval: Awaiting Action by Sanket"; `click --by ref e4821` succeeds.

**US-3 — Find an input by its hint text.**
As an automation author, I want placeholder and label text to count as evidence so that unlabeled search boxes are findable.
*Acceptance:* `find "search bar"` ranks a textbox whose placeholder is "Search Workday" first.

**US-4 — Disambiguate with role words.**
As an automation author, I want role words in my query to filter by role so that "settings link" and "settings heading" return different elements.
*Acceptance:* both queries rank their respective roles first on a page containing each.

**US-5 — Scope to a region.**
As an automation author, I want to search inside one container so that "delete" in a specific row does not match the page-level Delete.
*Acceptance:* `find "delete" --region "#invoice-4102"` returns only descendants of that container.

**US-6 — Feed a coordinate workflow.**
As an agent on a ref-hostile page, I want each match's centre point so that I can act with `--at` when refs go stale between calls.
*Acceptance:* every match carries `center: {x, y}` in CSS pixels per RFC-0014's coordinate contract.

**US-7 — Nothing found is an answer, not an error.**
As a script author, I want an empty result to exit 0 so that "absent" is a checkable outcome without error handling.
*Acceptance:* `find "nonexistent widget"` exits 0 with `count: 0`.

## Proposed CLI surface

```
chrome-cdp find <query>
```

| Flag | Purpose |
|------|---------|
| `--role <role>` | hard-filter matches to one ARIA role (in addition to soft role words in the query) |
| `--limit <n>` | maximum matches returned; default `10`, max `50` |
| `--region <selector>` | scope the search to a container's subtree, as `snap --region` |
| `--all` | include off-screen and ignored nodes (excluded by default) |
| `--dedupe` | collapse identical role+name matches, as `snap --dedupe` |
| `--min-score <0..1>` | drop matches below a score threshold; default `0` (return best-effort) |

The query is a positional argument, one to a few words, matched case-insensitively.
`find` takes no element-addressing flags (`--by`, `--nth`, …) — it *produces* addresses, it does not consume them.

Examples:

```sh
chrome-cdp find "login button"
chrome-cdp find "search bar" --limit 3
chrome-cdp find "delete" --region "#invoice-4102" --role button
chrome-cdp find "time type" --role textbox
```

## Result envelope

```json
{ "ok": true, "command": "find",
  "target": {"id":"…","title":"…","url":"…"},
  "result": {
    "query": "login button",
    "matches": [
      { "ref": "e4821", "role": "button", "name": "Sign in to your account",
        "score": 0.91, "center": {"x": 640, "y": 412},
        "states": ["focusable"], "visible": true },
      { "ref": "e103", "role": "link", "name": "Login help",
        "score": 0.44, "center": {"x": 640, "y": 470},
        "states": [], "visible": true }
    ],
    "count": 2, "truncated": false },
  "elapsed_ms": 180 }
```

Matches are ordered by descending score; ties break by document order.
`truncated: true` signals that more candidates cleared `--min-score` than `--limit` allowed.
`ref`, `role`, `name`, and `states` use exactly the vocabulary `snap` already emits, so a caller parses one schema across both verbs.
A match also carries `value` when the element has one (masked for password fields, as every read verb masks them), and `occluded: true` when its centre pixel resolves to an overlay rather than the element.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| Empty query; `--limit` out of range; `--min-score` outside 0..1 | `usage` | 2 |
| `--region` names no container on the page | *not an error* — `ok: true`, `count: 0`, plus `region_found: false` | 0 |
| A11y tree unavailable and DOM fallback also failed | `cdp_error` | 5 |
| No matches | *not an error* — `ok: true`, `count: 0` | 0 |

## Scoring model

Deterministic, explainable, and unit-testable as a pure function.
For each candidate node, evidence text is the concatenation of accessible name, value, placeholder, and associated label text.

1. **Normalize** query and evidence: lowercase, strip punctuation, split to tokens.
2. **Extract role words** from the query via a small fixed table ("button" → `button`; "link" → `link`; "field"/"input"/"box"/"bar" → `textbox|searchbox|combobox`; "checkbox", "tab", "menu", "heading", "row", "icon" → their roles).
   Role words are removed from the text tokens and matched against the node's role as a score component, not a hard filter (`--role` is the hard filter).
3. **Text score** per remaining token: exact-phrase match > all-tokens-present > token-subset overlap, with a length-normalized overlap ratio so short exact names beat long names that merely contain the tokens.
4. **Boosts:** interactive role when the query has any role word (+), visible and on-screen (+), focusable (+).
   **Penalties:** ignored/hidden nodes (only reachable under `--all`), `disabled` state (−, never excluded — finding a disabled button is often the answer).
5. **Final score** is clamped to 0..1 and reported, so callers can threshold and humans can see *why* an element ranked.

The table and weights live in one file with golden tests; tuning them must never require touching the traversal code.

## Design notes

- **One new interface method.**
  `chrome.Browser` gains `Find(ctx context.Context, targetID, query string, opts FindOpts) (FindResult, error)` with `FindOpts{Role string; Limit int; Region string; All, Dedupe bool; MinScore float64}`.
  Stub default in `chrometest.StubBrowser` returns an empty match list; daemon `remoteBrowser` forwarder and `dispatch` case are required — `TestDispatchCoversBrowser` enforces them.
- **Reuse `snap`'s traversal, verbatim.**
  Tree acquisition (`GetFullAXTree`), frame and shadow-DOM handling, visibility computation, ref minting, and the `--region`/`--dedupe` semantics must be the same code paths `snap` uses — a second traversal would drift, and the refs must be interchangeable between the two verbs.
- **The hidden-tab DOM fallback applies.**
  A11y-tree acquisition is throttled on backgrounded tabs; when the tree yields nothing and the tab is hidden, fall back to the same DOM-computed accessible-name path `--by name` already has, and mark the envelope with `note: "dom_fallback"` plus the existing `tab_hidden` guidance.
- **No model calls, ever.**
  A heuristic will lose to a language model on paraphrase ("the thing that saves my work" → Save).
  That is acceptable and must be documented: `find` handles descriptive queries; genuinely semantic queries remain the caller's job on top of `snap`.
  Keeping it deterministic keeps it fast, offline, testable, and trustworthy in scripts.
- **Why not `click --by find "<query>"`?**
  Acting on a fuzzy match hides the ranking decision inside a mutating verb — a wrong top match becomes a wrong click instead of a wrong line in a list.
  Search and act stay separate; `session` makes the two-step cheap.
  See Open Questions for a possible future `--first` convenience once real usage shows the top match is reliable enough.
- **MCP surface.**
  A `find` tool joins the default set and the `--read-only` set (it mutates nothing).
  Its input schema mirrors the flags; its output is the same match list, which pairs with the existing `click`/`type_text` tools' ref addressing.
- **Performance.**
  One tree fetch plus an in-process scoring pass over at most a few thousand nodes — comparable to `snap` itself; no per-candidate round trips.
  Geometry is measured only for the matches actually RETURNED (bounded by `--limit`: 10 by default, 50 at most), which costs a pair of CDP calls each.
  It goes through the one shared geometry primitive the pointer verbs measure with, in its non-scrolling variant: a read verb must not scroll the page under a running automation, but it must agree with the pointer verbs about where an element is, or a reported centre would not be a point a click lands on.
  That shared primitive also supplies the occlusion probe, so a match whose centre is covered is reported with `occluded: true` rather than silently handing back a coordinate that would miss.
  RFC-0014's `--at` addressing consumes the same primitive rather than adding a third definition of "where is this element".

## Verification scenarios

**VS-1 — Purpose query ranks the right control first.**
Given a fixture with a button "Sign in to your account", a link "Login help", and a heading "Login"
When `find "login button"` runs
Then the button is first, and both others score strictly lower.

**VS-2 — Placeholder counts as evidence.**
Given an unlabeled `<input placeholder="Search projects">`
When `find "search bar"` runs
Then that textbox is first.

**VS-3 — Role words steer role.**
Given a page with both a "Settings" link and a "Settings" heading
When `find "settings link"` and `find "settings heading"` run
Then each ranks its own role first.

**VS-4 — Hard role filter excludes.**
Given the same page
When `find "settings" --role link` runs
Then no heading appears in the matches.

**VS-5 — Visible beats hidden.**
Given two nodes named "Submit", one visible and one `display:none`
When `find "submit"` runs
Then only the visible one appears by default, and `--all` reveals the second ranked lower.

**VS-6 — Region scoping.**
Given "Delete" buttons in two rows
When `find "delete" --region "#row-2"` runs
Then exactly the row-2 button matches.

**VS-7 — Refs are live addresses.**
Given any VS-1 result
When `click --by ref <first match's ref>` runs in the same `session`
Then the click lands on that element.

**VS-8 — Centre point honours the coordinate contract.**
Given a fixture with an element at a known position
When `find` returns it
Then `center` equals the element's CSS-pixel centre, matching a `screenshot --scale 1` overlay.

**VS-9 — Empty result is exit 0.**
When `find "flux capacitor"` runs on a plain fixture
Then exit 0, `count: 0`, empty `matches`.

**VS-10 — Limit and truncation.**
Given 30 buttons named "Item N"
When `find "item" --limit 5` runs
Then 5 matches and `truncated: true`.

**VS-11 — Verbose-name discovery.**
Given a button whose visible text is "Review" but whose accessible name is "Review Approval: Awaiting Action by Sanket"
When `find "review"` runs
Then the button matches with its full accessible name in the result — the discovery loop the verb exists for.

**VS-12 — Disabled is found, flagged, and penalized.**
Given enabled "Save draft" and disabled "Save"
When `find "save"` runs
Then both appear, the disabled one carries `"disabled"` in `states`, and enablement is visible in the ordering evidence.

## Test plan

**Unit — scoring (pure function, table-driven, `t.Parallel()`).**
The bulk of the suite: golden tables over normalization, role-word extraction (every table entry), exact/subset/overlap tiers, length normalization, each boost and penalty, tie-breaking, and clamping.
These tests pin the ranking so weight tuning is a visible diff, not a silent behavior change.

**Unit — flag validation (`internal/cli`, stub failing on any browser call).**
Empty query, `--limit` bounds, `--min-score` bounds — all `usage`/exit 2 before any connection.

**Unit — command boundary (`chrometest.StubBrowser`).**
Flags map to `FindOpts`; envelope shape matches this RFC; empty result is `ok: true`.

**Live Chrome (`internal/chrome`, `testing.Short()`-guarded).**
VS-1 through VS-12 against fixture pages; VS-7 and VS-8 run inside `session` to prove ref and coordinate interchange with acting verbs.

**Ref-interchange regression.**
A test asserting `find` and `snap` mint the same ref for the same node in the same document — the test that catches the two traversals drifting apart.

## Out of scope

- Model-backed or embedding-based semantic matching, and any network call.
- Fuzzy *spelling* correction (edit-distance typo tolerance) — revisit only with evidence from real usage.
- Searching page *content* (paragraph text) — that is `text`/`--grep` territory; `find` targets addressable elements.
- Acting on a match in the same invocation (see Open Questions).
- Cross-tab search; `find` operates on the current target only.

## Open questions

1. Should role words hard-filter instead of soft-boost when present in the query?
   Soft keeps recall when the page's role differs from the user's word (a "button" that is really a link); hard reads as more predictable.
   **Recommendation:** soft-boost, with `--role` as the explicit hard filter; revisit with usage evidence.
2. Should a `--first` flag print exactly one best match (ref only) for tight scripting, e.g. `` click --by ref `find "save" --first` ``?
   **Recommendation:** defer; the envelope already makes this a one-line `jq`, and a wrong silent pick is costlier than the convenience.
3. Should the role-word table be user-extensible via config?
   **Recommendation:** no until a concrete gap appears; a fixed table keeps behavior identical across machines, which matters for shared recipes.
4. Should synonym pairs beyond role words (e.g. "remove"≈"delete") ship in v1?
   **Recommendation:** no; start with token matching only, and let golden-test additions drive any synonym list, one evidence-backed entry at a time.
