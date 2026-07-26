# RFC-0009: Recipes — saved, shareable `session` scripts

- **Status:** Draft
- **Priority:** P2
- **Area:** workflow
- **Depends on:** `session` (exists); benefits from RFC-0001, RFC-0005, RFC-0006
- **Related:** RFC-0004 (recipes become MCP tools)

## Summary

Add a `recipe` command that runs, lists, and scaffolds named automation scripts stored as files.
A recipe is a parameterised `session` script with declared inputs, so a working sequence of commands becomes something a user can name, re-run, version-control, and share with their team.

## Motivation

`session` already does the hard part: many commands over one held connection, with refs that stay valid across the batch.
What it lacks is a way to *keep* a batch.

Today, a user who works out the exact eleven commands that submit their timesheet has that knowledge in shell history, or in a bespoke wrapper script, or in an agent's context that will be gone tomorrow.
There is no artifact.
Consequences:

- **Nothing accumulates.**
  Every run of a recurring task re-derives the same sequence, and every teammate re-derives it independently.
- **Nothing is reviewable.**
  A shell script full of `chrome-cdp` invocations mixed with `jq` is not something a colleague can read and trust.
- **Nothing is shareable.**
  The most valuable thing a user of this tool produces is a working automation for a specific internal app.
  There is no unit in which to hand that to someone else.

Recipes turn `session` into a durable format.
That matters for adoption in a way features do not: a tool people *extend* and share attracts contributions, and a repo whose README shows a recipe someone can copy for their own app is far more compelling than one showing a flag list.

The design constraint is to add as little as possible — a recipe should be recognisably a `session` script with a header, not a new programming language.

## User stories

**US-1 — Save a working sequence.**
As a user who has worked out a multi-step automation, I want to save it under a name so that I can re-run it tomorrow without reconstructing it.
*Acceptance:* `chrome-cdp recipe run submit-timesheet` re-runs the saved sequence.

**US-2 — Parameterise it.**
As a recipe author, I want named inputs so that one recipe covers every week rather than hardcoding one date.
*Acceptance:* `chrome-cdp recipe run submit-timesheet --set week=2026-07-20 --set hours=8` substitutes both values.

**US-3 — Discover what is available.**
As a new user, I want to list recipes with their descriptions and required inputs so that I can use one without reading its source.
*Acceptance:* `chrome-cdp recipe list` shows name, description, and inputs; `recipe show <name>` prints the full definition.

**US-4 — Share with my team.**
As a team lead, I want recipes to be plain files in a directory I can commit so that my team gets our internal-app automations from our repo.
*Acceptance:* recipes resolve from a project-local directory as well as the user config directory, project taking precedence.

**US-5 — Fail before doing half the work.**
As a recipe user, I want missing required inputs to fail before any command runs so that a typo does not leave a form half-filled.
*Acceptance:* omitting a required input exits 2 with `usage` and no browser contact.

**US-6 — See what will happen.**
As a cautious user, I want a dry run that prints the resolved commands so that I can inspect a recipe someone else wrote before letting it drive my browser.
*Acceptance:* `recipe run <name> --dry-run` prints every resolved argv line and performs no action.

**US-7 — Stop at the first failure, and know where.**
As a recipe user, I want a failing step to halt the run and tell me which step failed so that I do not have to diff envelopes to find it.
*Acceptance:* the run stops, the exit code is the failing command's, and the envelope names the step index and label.

**US-8 — Scaffold one.**
As a new recipe author, I want a starting file so that I do not have to learn the format from documentation alone.
*Acceptance:* `chrome-cdp recipe new <name>` writes a commented template and prints its path.

## Format

A recipe is a YAML file: a small header, then a list of steps whose `run` is exactly the argv `session` already accepts.

```yaml
name: submit-timesheet
description: Fill and submit the weekly timesheet in Workday.
inputs:
  week:  { required: true,  description: "Monday of the week, YYYY-MM-DD" }
  hours: { default: "8",    description: "Hours per weekday" }
target: url:workday

steps:
  - label: open the timesheet
    run: ["nav", "https://workday.internal/time/{{week}}"]
  - run: ["wait", "--idle"]
  - label: fill weekdays
    run: ["fill", "--by", "cell", "{{week}}", "{{hours}}"]
  - label: save
    run: ["click", "--by", "name", "Save and Close", "--role", "button",
          "--wait-text", "saved"]
    on_error: abort
```

Rules, chosen to keep this from becoming a language:

- `run` is an argv array — identical to a `session` stdin line.
  Anything valid in `session` is valid here, and vice versa.
- `{{name}}` substitutes an input value.
  Substitution is **into an argv element**, never into a shell — there is no shell, so there is no shell injection surface.
  Unknown placeholders are a `usage` error at load time.
- `inputs` declares required-ness, defaults, and a description.
  That is the whole schema; no types, no validation expressions.
- `on_error` is `abort` (default) or `continue`.
  Nothing else — no retries, no conditionals, no loops.
  If a recipe needs control flow, it should be a program that calls `session`, and the docs should say so plainly.
- `target` is an optional default `--target` spec for the whole recipe.

## Proposed CLI surface

```
chrome-cdp recipe list [--dir <path>]
chrome-cdp recipe show <name>
chrome-cdp recipe new <name>
chrome-cdp recipe run <name> [--set k=v]... [--dry-run] [--from-step <n>]
```

| Flag | Purpose |
|------|---------|
| `--set k=v` | supply an input; repeatable |
| `--dry-run` | resolve and print, do not execute |
| `--from-step <n>` | start at step *n* (1-based), for resuming a partly-failed run |
| `--dir <path>` | additional search directory |

Resolution order for a name, first match wins: `./.chrome-cdp/recipes/`, then `$XDG_CONFIG_HOME/chrome-cdp/recipes/`, then any `--dir`.
Project-local beating user-global is what makes US-4 work.

## Result envelope

`recipe run` emits one NDJSON envelope per step — the same stream `session` produces — followed by a summary envelope:

```json
{ "ok": true, "command": "recipe", "result":
  { "recipe": "submit-timesheet", "steps": 4, "completed": 4, "failed": null,
    "inputs": {"week":"2026-07-20","hours":"8"}, "elapsed_ms": 4120 } }
```

On failure the summary carries `"ok": false`, `completed` short of `steps`, and `failed: {"index": 3, "label": "save", "code": "target_timeout"}`.
The process exit code is the failing step's, so a shell caller branches on the same contract as always.

Per-step envelopes gain `step` and `label` fields so a caller can correlate without counting lines (US-7).

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| Recipe not found in any search dir | `usage` | 2 |
| Malformed YAML, unknown top-level key, `run` not an array of strings | `usage` | 2 |
| Missing required input; unknown `--set` key; unresolved `{{placeholder}}` | `usage` | 2 |
| `--from-step` out of range | `usage` | 2 |
| A step fails | that step's code | that step's exit |

Everything a recipe can get wrong statically is exit 2 before the browser is touched (US-5).

## Design notes

- **Recipes are `session` with a header, deliberately.**
  The runner loads the file, validates it, substitutes inputs, and feeds the resulting argv lines through the existing `session` execution path.
  If the implementation grows a second execution path, the design has gone wrong.
- **Validation is a separate pass from execution.**
  Parse → validate schema → resolve inputs → check every placeholder → *then* connect.
  This ordering is what makes US-5 and US-6 true and is worth enforcing with a test that uses a stub failing on any browser call.
- **`--dry-run` prints resolved argv**, one JSON array per line — the exact bytes that would go to `session`.
  That makes it pipeable into `session` directly, which is a useful property for debugging and a good demonstration that recipes add no hidden magic.
- **No shell, ever.**
  Steps are argv arrays executed in-process.
  There is no `sh -c` anywhere in this design, and there should never be a `shell:` step type.
  Recipes are files that get shared and run against an authenticated browser; a shell escape hatch would make every shared recipe a code-execution vector.
- **Reviewing an untrusted recipe** is the point of `recipe show` and `--dry-run`.
  The docs should say directly: a recipe drives your logged-in browser, so read one before running it, the same way you would read a shell script.
- **`--from-step`** exists because real automations fail halfway and re-running from the top is often wrong (a form already partly submitted).
  It is a sharp tool; document it as such.
- **Bounded size:** cap steps per recipe (proposal: 200) and reject recursion — recipes cannot invoke recipes.
  That single restriction removes a whole category of design questions.
- **Implementation home:** a new `internal/recipe` package for loading, validating, and substituting — pure functions over a file, entirely testable without a browser.
  The CLI wires it to the `session` runner.

## Verification scenarios

**VS-1 — Round trip** Given a recipe with three steps When `recipe run` executes it against a stub Then three step envelopes and one summary are emitted in order, with `completed == 3`.

**VS-2 — Substitution into argv** Given a step `["fill", "#h", "{{hours}}"]` and `--set hours=8` When resolved Then the argv is exactly `["fill", "#h", "8"]` — assert on the array, not a rendered string.

**VS-3 — Defaults apply** Given `hours` has `default: "8"` and is not supplied Then the resolved argv uses `8`.

**VS-4 — Missing required input never connects** Given `week` is required and not supplied Then exit 2 with `usage`, the message names `week`, and no browser method was called.

**VS-5 — Unknown placeholder is caught at load** Given a step referencing `{{nope}}` that is not declared in `inputs` Then exit 2 at load time, before any step runs.

**VS-6 — Unknown `--set` key is rejected** Given `--set typo=1` for a recipe without that input Then exit 2 with `usage` — silently ignoring it would let a typo produce a run with a default the user did not intend.

**VS-7 — Abort on failure with location** Given step 2 fails with `target_timeout` Then step 3 does not run, the summary reports `failed.index == 2` with its label, and the process exit is 4.

**VS-8 — `on_error: continue`** Given step 2 has `on_error: continue` and fails Then step 3 runs, the summary is `ok: false`, and `completed` counts the successful steps.

**VS-9 — Dry run performs nothing** Given `--dry-run` and a stub that fails the test on any browser call Then resolved argv lines are printed and no call occurs.

**VS-10 — Dry-run output is valid `session` input** Given the dry-run output When it is fed to `session` against a stub Then it executes identically to `recipe run`.
This is the test that keeps recipes honest about being a thin layer.

**VS-11 — Resolution precedence** Given the same recipe name in the project dir and the user config dir Then the project one is used, and `recipe list` marks the source of each entry.

**VS-12 — Malformed files** Table: invalid YAML; `run` as a string instead of an array; `run` containing a non-string; unknown top-level key; empty `steps`; more than the step cap; a recipe invoking another recipe.
All exit 2 with `usage`.

**VS-13 — `--from-step`** Given a 4-step recipe run with `--from-step 3` Then only steps 3 and 4 execute, and the summary reports it.

**VS-14 — Scaffold produces a valid recipe** When `recipe new demo` runs, then `recipe show demo` and `recipe run demo --dry-run` Then the template loads and validates cleanly.
A scaffold that emits something the validator rejects is a bad first impression, and this test prevents it.

**VS-15 — No shell interpretation** Given `--set name="; rm -rf /"` substituted into an argv element Then the value is passed through literally as one argv element and nothing is interpreted.

## Test plan

**Unit — `internal/recipe` (pure, `t.Parallel()`, `t.TempDir()`)** The bulk of the suite: loading, schema validation, input resolution, placeholder checking, precedence.
Covers VS-2 through VS-6, VS-11, VS-12, VS-13, VS-15.
Table-driven over fixture files written into `t.TempDir()`, never into the repo.

**Unit — runner (`internal/cli`, `chrometest.StubBrowser`)** VS-1, VS-7, VS-8, VS-9, VS-14, and the ordering guarantee that validation precedes connection (a stub that fails the test if called during a validation-failure case).

**Equivalence test (VS-10)** Run a recipe and its dry-run-through-`session` against the same recording stub and assert identical call sequences.
This is the structural guard on "recipes are `session` with a header".

**Security-shaped test (VS-15)** Assert argv elements are passed through byte-for-byte, including shell metacharacters, quotes, and newlines.

**Live Chrome (`testing.Short()`-guarded)** One end-to-end recipe against a `data:` fixture — nav, fill, click, read back — to prove the wiring.
Deliberately minimal; the format's correctness is covered by pure unit tests.

**Docs** A worked example recipe ships in `docs/scenarios/`, and the README shows one.
The example is the marketing artifact as much as the documentation.

## Out of scope

- Conditionals, loops, retries, and branching.
  If a recipe needs them, it should be a program calling `session`.
- Reading values out of one step's envelope and using them in a later step.
  Tempting and genuinely useful, but it turns the format into a language; revisit only with evidence from real recipes.
- A registry or remote fetching of recipes.
  Files in git are the distribution mechanism.
- Secrets management.
  Recipes must not carry credentials, and the docs should say so explicitly — the CLI's whole premise is reusing an already-authenticated browser, so a recipe never needs a password.

## Open questions

1. YAML or NDJSON-with-a-header?
   YAML is far more readable and reviewable, which is the entire point, at the cost of a dependency.
   **Recommendation:** YAML; readability is the feature.
2. Should `recipe run` support `--json` emitting a single summary envelope instead of a stream?
   **Recommendation:** yes — streaming per-step for interactive use, single summary under `--quiet`, matching how the rest of the CLI treats verbosity.
3. Should recipes be exposed as individual MCP tools in RFC-0004, so an agent sees `submit-timesheet` as a first-class tool with typed inputs?
   That is genuinely powerful — it turns a user's own automations into agent capabilities.
   **Recommendation:** yes, as a follow-up once the format settles; note it in RFC-0004 rather than expanding this one.
4. Should step-level `--timeout` be settable per step?
   **Recommendation:** yes; it is one field and long automations have steps with wildly different durations.
