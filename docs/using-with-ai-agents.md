# Using chrome-cdp from an AI agent

`chrome-cdp` is designed to be a tool an AI agent calls, not just one a human types.
The design choices that make it agent-friendly are the same ones that make it scriptable: one output shape, one failure contract, and reads that describe the page in tokens instead of pixels.

## Why not screenshots

A screenshot forces the model to re-derive structure from pixels on every step, burns vision tokens, and gives you nothing to assert on.
`snap` returns the page's actionable structure as text — roles, accessible names, states, live-region `alerts`, and a stable `ref` per node — so the agent reads *what it can do* directly, and you can branch on it in code.

```sh
chrome-cdp snap --role button --grep "Save|Submit" --json
```

## One shape to parse

Every command emits the same envelope, so an agent's tool wrapper parses once:

```json
{ "ok": true, "command": "click", "target": {"id":"…","url":"…"},
  "result": { "clicked": "Save" }, "elapsed_ms": 42 }
```

On failure, `ok` is `false`, `error{code,message,details}` explains it, and the **exit code** classifies it — so the agent branches on a number, not on prose it has to interpret:

| Exit | Meaning | Typical agent response |
|-----:|---------|------------------------|
| 0 | success | continue |
| 2 | bad flags/args | fix the call |
| 3 | connection | check `error.code`: `consent_pending` means a modal "Allow remote debugging?" dialog is unanswered (tell the user it is behind the window and freezes all of Chrome); otherwise ask them to relaunch Chrome with `--remote-debugging-port=9222` |
| 4 | not found / timeout / ambiguous | re-`snap` and retry, or foreground the tab |
| 5 | CDP error | surface it |

Details carry actionable hints — e.g. `tab_hidden: true` tells the agent to foreground Chrome rather than keep retrying a throttled read.

## Address by meaning, not by markup

CSS ids churn between sessions; accessible names don't.
Addressing controls by `--by name` (with `--role` / `--match` / `--nth` / `--in-row` to disambiguate) keeps an agent's actions stable across the app's cosmetic changes, and matches how the agent already refers to the control ("the Approve button").

## Batch a plan over one connection

When the agent has decided a sequence, `session` runs it over a single held connection — no per-call process spawn, and `snap` refs stay valid across the batch:

```sh
printf '%s\n' \
  '["snap","--role","button"]' \
  '["click","e42","--by","ref"]' \
  '["wait","--text","Saved"]' \
  | chrome-cdp session
```

## Act and confirm atomically

`--wait-text` folds the write and its confirmation into one call, so the agent gets a single success/fail signal instead of a click-then-poll dance:

```sh
chrome-cdp click --by name "Submit" --role button --wait-text "received" --json
```

## Guardrails worth keeping

- Drive one command at a time on a bounded `--timeout`; don't fire bursts at the user's live session.
- Confirm before any irreversible or outward-facing action (submit, approve, delete, send).
- Never drive a passkey or credential entry — hand those back to the user.
- Prefer a condition (`wait --idle` / `--stable` / `--text`) over a fixed sleep.

## Skills

An [Agent Skill](https://docs.claude.com/en/docs/claude-code/skills) that teaches these patterns end-to-end ships in [`skills/drive-chrome-cdp`](../skills/drive-chrome-cdp/SKILL.md) — point your harness at it to give an agent the whole loop, addressing model, and etiquette in one reference.
