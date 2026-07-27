# RFC-0012: Domain allow-list — bounding what the CLI may drive

- **Status:** Accepted — implemented in [#12](https://github.com/sanketsudake/chrome-cdp-cli/pull/12)
- **Priority:** P2 *(raise to P0 if RFC-0004 is scheduled — MCP should not ship without it)*
- **Area:** safety
- **Depends on:** —
- **Blocks (recommended):** RFC-0004 (MCP server mode)

## Summary

Add an optional policy layer that restricts which origins the CLI will act on, which verbs are permitted, and — with RFC-0006 — which local paths may be uploaded.
Off by default for the CLI, so nothing changes for existing users; on by default in MCP mode, where the caller is an agent rather than the person at the keyboard.

## Motivation

`chrome-cdp`'s core premise is also its sharpest edge: it drives the user's **real, already-authenticated Chrome**.
That is what makes it useful — no credential handling, no separate login, the live session's cookies just work.
It also means that a process holding a connection to it can act as the user on *every site the user is logged into*, not just the one they intended.

Today the only boundary is the tab: `--target` picks one, and nothing stops a command from navigating that tab somewhere else and acting there.
For a human typing commands, that is fine — they are the one typing.
The calculus changes as soon as the caller is not the user:

- **An agent** (via the skill today, via MCP after RFC-0004) acts on instructions that can be influenced by page content.
  A page that says "now go to the admin console and delete X" is data, not a command — but the tool offers no mechanism to *enforce* that distinction.
  A domain boundary is that mechanism.
- **A shared recipe** (RFC-0009) is a file from someone else that drives your browser.
  `recipe show` lets you read it; an allow-list means you do not have to read it perfectly.
- **A cautious first adopter** — the security-minded engineer evaluating this for their team — currently has to answer "what stops it touching our production admin panel?" with "nothing".
  That is often where evaluation ends.

This is why the RFC is framed as an adoption feature rather than a restriction.
The bounded version of the tool is the one a team can say yes to.

## User stories

**US-1 — Bound to the app I am automating.**
As a user, I want to restrict the CLI to my internal app's domains so that an automation cannot act on my email or bank.
*Acceptance:* with `allow = ["*.workday.com"]`, a command against a tab on another origin is refused, and no browser action occurs.

**US-2 — Block specific sites while leaving the rest open.**
As a user who prefers a deny-list, I want to name the origins that are always off limits so that I keep flexibility elsewhere.
*Acceptance:* `deny` entries are refused even when `allow` is unset or would permit them.

**US-3 — Read-only on some origins.**
As a user, I want to read a site without being able to act on it so that an agent can gather information without changing anything.
*Acceptance:* an origin marked read-only permits `snap`/`text`/`grid` and refuses `click`/`type`/`fill`/`eval`.

**US-4 — Not be surprised.**
As a user, I want a refusal to be a clear typed error naming the origin and the rule so that I can fix my policy rather than guess.
*Acceptance:* the envelope carries `permission_denied`, the origin, the matched rule, and the config file path.

**US-5 — Nothing changes if I do not opt in.**
As an existing user, I want the CLI to behave exactly as before when I configure no policy so that an upgrade does not break my scripts.
*Acceptance:* with no policy configured, every command behaves identically to the previous release.

**US-6 — Safe defaults where the caller is not me.**
As a user handing this to an agent, I want the agent-facing mode to require an explicit allow-list so that "no policy" does not silently mean "everything".
*Acceptance:* `chrome-cdp mcp` with no policy configured refuses to act and explains how to configure one.

**US-7 — Cover navigation, not just action.**
As a user, I want a navigation to a disallowed origin to be refused too so that the boundary cannot be stepped around by navigating first.
*Acceptance:* `nav` to a non-allowed origin is refused, and a redirect that lands on one causes subsequent commands to be refused.

**US-8 — Audit what happened.**
As a security-conscious user, I want refusals recorded so that I can see what an automation attempted.
*Acceptance:* refusals are logged to stderr with origin, verb, and rule, and optionally to an audit file.

## Proposed configuration

Policy lives in the existing config file, with a `[policy]` table:

```toml
[policy]
enabled = true
allow   = ["*.workday.com", "intranet.corp.local", "localhost:*"]
deny    = ["*.bank.example", "admin.corp.local"]
read_only = ["*.wikipedia.org"]
verbs_denied = ["raw"]
upload_roots = ["~/Documents/receipts"]
audit_log = "~/.local/state/chrome-cdp/audit.log"
on_violation = "error"     # error | prompt
```

| Key | Purpose |
|-----|---------|
| `enabled` | master switch; when false the whole layer is inert |
| `allow` | origin patterns permitted; empty means "all except `deny`" |
| `deny` | always refused; takes precedence over `allow` |
| `read_only` | origins where only non-mutating verbs are permitted |
| `verbs_denied` | verbs refused everywhere (e.g. `raw`, `eval`) |
| `upload_roots` | directories uploadable (RFC-0006) |
| `audit_log` | append refusals and, optionally, all actions |
| `on_violation` | `error` (default) or `prompt` for an interactive confirmation |

Pattern syntax, deliberately minimal: exact host, `*.` prefix wildcard for subdomains, optional `:port`, and a `scheme://` prefix when it matters.
No regex — a policy language that is hard to read is a policy that is wrong without anyone noticing.

Command-line overrides for one-off use:

```
chrome-cdp --allow "*.example.com" ...
chrome-cdp --policy-off ...          # explicit, logged, never implicit
```

## Enforcement points

The check runs **after** target resolution and **before** any action:

| Verb class | Checked against |
|-----------|-----------------|
| Acting (`click`, `type`, `fill`, `select`, `scroll`, `key`, pointer verbs, `upload`, `attr set/rm`, `cookie set/rm/clear`, `headers`, `emulate`, `eval`, `raw`) | `allow`/`deny`, `read_only`, `verbs_denied` |
| Reading (`snap`, `text`, `html`, `value`, `grid`, `screenshot`, `pdf`, `console`, `net`, `frame`) | `allow`/`deny` only |
| Navigating (`nav`, `open`) | the **destination** origin, before navigating |
| Tab (`list`, `use`, `close`, `activate`) | not checked; `list` output redacts non-allowed URLs to their origin when a policy is active |

Redirects are the subtle case (US-7): a `nav` to an allowed origin that redirects elsewhere cannot be prevented, so the policy is re-evaluated on the **settled** URL and the *next* command is refused.
The docs must state this honestly — this is a guardrail against a confused or misdirected caller, not a sandbox against a hostile one.

## Result envelope

A refusal:

```json
{ "ok": false, "command": "click",
  "target": {"id":"…","title":"…","url":"https://admin.corp.local/users"},
  "error": { "code": "permission_denied",
             "message": "origin admin.corp.local is not permitted by policy",
             "details": { "origin": "admin.corp.local", "verb": "click",
                          "rule": "deny: admin.corp.local",
                          "config": "~/.config/chrome-cdp/config.toml" } },
  "elapsed_ms": 2 }
```

**New contract entries:** `CodePermissionDenied = "permission_denied"` and a `codeToExit` mapping to a new exit code 7.
Shared with RFC-0006; whichever lands first defines both, and `exit-codes` gains the row.

A distinct code matters: an agent must be able to tell "policy forbids this" (do not retry, tell the user) from "element not found" (retry differently).

## Design notes

- **Off by default for the CLI, required in MCP mode.**
  These are different threat models and should not share a default.
  A human at a shell is the trust boundary; an agent behind a protocol is not.
  `chrome-cdp mcp` with `policy.enabled` unset should refuse to start and print the config it needs (US-6), rather than starting wide open.
- **Enforcement lives at the command boundary, not in the driver.**
  `internal/cli` (and the MCP layer) consult a `policy.Checker` before calling into `chrome.Browser`.
  Putting it in the driver would mean every future `Browser` method has to remember to check; putting it at the boundary means the check is in the same place argument validation already is.
  A test that enumerates all registered commands and asserts each is covered by the policy table is what keeps a new verb from silently bypassing it.
- **`policy.Checker` is a pure package** — `Check(origin, verb) Decision` over a parsed config, with no I/O.
  That makes exhaustive table-driven testing cheap, which is what this feature needs most.
- **Pattern matching must be strict.**
  `*.example.com` matches `a.example.com` and `a.b.example.com` but **not** `example.com` (require an explicit entry) and **not** `notexample.com` or `example.com.evil.io`.
  Matching is on the parsed URL's host, never on a substring of the raw URL.
  Every one of these is a test case, and the suffix-confusion cases are the ones that make a naive implementation wrong.
- **`read_only` needs a verb classification table**, and that table is the mechanism, so it must be exhaustive by construction: a test iterates every registered command and fails if it is not classified.
  An unclassified verb defaults to *mutating* — fail closed.
- **`on_violation = "prompt"`** asks on a TTY and refuses when there is none (piped, daemonised, MCP).
  It must never block a non-interactive run waiting for input nobody will provide.
- **Audit log** is append-only NDJSON: timestamp, origin, verb, decision, rule.
  Refusals always; all actions when `audit_all = true`.
  It must never record values — no typed text, no cookie values, no `--set` inputs — because the log would otherwise become the most sensitive file the tool produces.
- **`--policy-off`** exists because a bad policy that cannot be bypassed is worse than none, but it is explicit and logged.
  There is no implicit disable.
- **Honest scope statement, which belongs in the user docs verbatim:** this bounds a cooperative caller.
  It is not a sandbox.
  Anything that can run `chrome-cdp` can also edit its config or connect to Chrome directly.
  Overstating this would be worse than not shipping it.

## Verification scenarios

**VS-1 — Allow-list permits and refuses** Given `allow = ["*.example.com"]` and a tab on `app.example.com` Then `click` succeeds; on `other.test`, `click` exits 7 with `permission_denied` and the stub records no call.

**VS-2 — Deny beats allow** Given `allow = ["*.example.com"]` and `deny = ["admin.example.com"]` Then `app.example.com` is permitted and `admin.example.com` is refused, with `rule` naming the deny entry.

**VS-3 — Pattern matching table** Table over pattern/host pairs: `*.example.com` vs `a.example.com` (match), `a.b.example.com` (match), `example.com` (**no** match), `notexample.com` (no), `example.com.evil.io` (no), `EXAMPLE.COM` host casing (match, case-insensitive); `example.com` exact vs `a.example.com` (no match); `localhost:*` vs `localhost:3000` (match), `localhost:3000` vs `localhost:8080` (no); `https://x.test` vs `http://x.test` (no match when scheme is specified).

**VS-4 — Read-only classification** Given `read_only = ["*.wiki.test"]` Then `snap`, `text`, `grid`, `screenshot` succeed and `click`, `fill`, `type`, `eval`, `cookie set` are refused with exit 7.

**VS-5 — Every command is classified** Iterate the registered command list; fail if any command is absent from the mutating/reading classification table.
This is the test that keeps the mechanism from rotting as verbs are added.

**VS-6 — Unclassified defaults to mutating** Given a synthetic command absent from the table Then it is treated as mutating under `read_only`.

**VS-7 — `verbs_denied`** Given `verbs_denied = ["raw"]` Then `raw` is refused on every origin, including allowed ones.

**VS-8 — Navigation checks the destination** Given `allow = ["*.example.com"]` When `nav https://other.test` runs Then it is refused before navigating — assert the tab's URL is unchanged.

**VS-9 — Redirect is caught on the next command** Given a fixture on an allowed origin that redirects to a disallowed one When `nav` completes and a `click` follows Then the click is refused, and the refusal names the settled origin.

**VS-10 — Policy off means no change** Given no `[policy]` table Then a representative command set behaves exactly as before — a regression suite run with and without the policy code path.

**VS-11 — MCP requires a policy** Given `chrome-cdp mcp` with `policy.enabled` unset Then the server refuses to start, exits nonzero, and prints the required configuration.

**VS-12 — `--policy-off` is explicit and logged** Given a policy that would refuse and `--policy-off` Then the command succeeds and a warning is written to stderr and the audit log.

**VS-13 — `prompt` never blocks non-interactively** Given `on_violation = "prompt"` and no TTY Then the command is refused rather than hanging — assert it returns within a bounded time.

**VS-14 — Audit log content and hygiene** Given a refusal and an allowed `type` with sensitive text Then the log contains the refusal with origin, verb, and rule, and **does not** contain the typed text.

**VS-15 — Malformed policy is not silently permissive** Given an unparseable pattern in `allow` Then the CLI reports a config error and — unlike other malformed config, which warns and continues — refuses to run with a policy it could not parse.
A policy that fails open is worse than no policy.

**VS-16 — Upload roots (with RFC-0006)** Given `upload_roots`, a path outside them is refused with exit 7 before any browser contact, including via `../` traversal and symlinks.

**VS-17 — `list` redacts under policy** Given an active policy and tabs on non-allowed origins Then `list` shows those tabs' origins without full URLs or titles.

## Test plan

**Unit — `internal/policy` (pure, `t.Parallel()`)** The centre of gravity: VS-1 through VS-4, VS-6, VS-7, VS-15.
VS-3 is exhaustive and table-driven, and the suffix-confusion rows are the ones worth writing first.
Good fuzz target: a random host string is never permitted by a pattern it does not legitimately match — the property that a naive `strings.Contains` implementation would fail immediately.

**Unit — command-boundary coverage (`internal/cli`)** VS-5 and VS-6 by enumerating registered commands.
This test is the reason the feature stays correct over time, and should be written before the enforcement code.

**Unit — enforcement (`chrometest.StubBrowser` recording calls)** VS-1's negative half, VS-8, VS-12, VS-13, VS-16: assert that on refusal **no browser method was called**.
"Refused with the right code but acted anyway" is the failure mode that matters, and only a recording stub catches it.

**Unit — config (`internal/config`)** VS-15, plus precedence of `--allow` over config, and `--policy-off` overriding both.

**Regression suite (VS-10)** Run the existing CLI test suite with the policy layer compiled in but unconfigured, asserting no behavioural change.
This is what makes US-5 credible rather than asserted.

**Audit-log test (VS-14)** Assert the log's content and, specifically, the absence of typed values — a negative assertion on the file bytes.

**Live Chrome (`testing.Short()`-guarded)** VS-9 only, using an `httptest` server that issues a redirect, since it is the one scenario that needs real navigation.
Everything else is stub-testable and should stay that way.

## Out of scope

- Sandboxing or preventing a determined local process from bypassing the policy.
  Explicitly not a security boundary against local code.
- Per-recipe or per-agent policies (a single global policy in this RFC).
- Rate limiting or quotas.
- Prompting for per-action approval in a GUI.
- Content-level filtering of what is read.

## Open questions

1. Should `read_only` be the default posture for MCP mode's initial connection, requiring an explicit opt-in to act?
   Safest, but likely too restrictive to be adopted.
   **Recommendation:** require an explicit `allow` list in MCP mode (US-6) but permit acting within it — a boundary users will actually configure beats a stricter one they will disable.
2. Should refusals count as failures for `--fail-on-match`-style automation, or be a distinct signal?
   **Recommendation:** distinct — exit 7 exists precisely so this is unambiguous.
3. Should the policy also constrain `--target` selection, so a disallowed tab cannot even be made sticky?
   **Recommendation:** no; `use` is inert, and the check at action time is sufficient.
   Blocking `use` would produce confusing errors far from the cause.
4. Should there be a curated starter policy (`chrome-cdp policy init`) that allow-lists the current tab's origin?
   **Recommendation:** yes — the gap between "I should configure this" and "I did" is almost entirely friction, and a one-command starting point closes most of it.
