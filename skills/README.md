# Skills

Companion [Agent Skills](https://docs.claude.com/en/docs/claude-code/skills) for `chrome-cdp`, maintained alongside the CLI so they never drift from the commands they document.

- [`drive-chrome-cdp`](drive-chrome-cdp/SKILL.md) — the core usage reference for an agent driving the CLI: the `list → use → snap → act → verify` loop, `--by` addressing (accessible name, `ref`, `cell`, `label`, `in-row`), the `select` verb for prompt/combobox/cascade widgets, `session` batching, the JSON envelope + exit-code contract, and the passkey/consent rules.
  `SKILL.md` is a thin stub; the body lives in [`drive-chrome-cdp/references/`](drive-chrome-cdp/references/):
  [`core`](drive-chrome-cdp/references/core.md), [`widgets`](drive-chrome-cdp/references/widgets.md), [`debugging`](drive-chrome-cdp/references/debugging.md), [`batch-and-recipes`](drive-chrome-cdp/references/batch-and-recipes.md), and [`examples`](drive-chrome-cdp/references/examples.md).
  Golden tasks for this skill live in [`drive-chrome-cdp/evals/evals.json`](drive-chrome-cdp/evals/evals.json).
- [`check-logged-in`](check-logged-in/SKILL.md) — confirm a Chrome tab is signed in to a web app by page content, not URL; a three-state verdict (`signed-in` / `login-page` / `unknown`) other skills branch on before they drive the app.
  Builds on `drive-chrome-cdp`.
- [`fill-grid-and-confirm`](fill-grid-and-confirm/SKILL.md) — fill several cells of a grid-style form with `--by cell` inside one `session`, read every cell back, and confirm the save actually persisted before reporting success.
  Builds on `drive-chrome-cdp`.

## Using a skill

Each skill is a directory with a `SKILL.md` (YAML frontmatter + instructions).
Point your agent harness at this `skills/` directory, or copy an individual skill into your own skill collection.

`chrome-cdp skill --full` prints the drive-chrome-cdp core doc plus every reference, straight from the installed binary — run it whenever you want it without leaving the shell, or to confirm a vendored copy hasn't drifted.
`chrome-cdp skill list` names every embedded skill and drive-chrome-cdp reference; `chrome-cdp skill get <name>` prints any one of them, addressed by reference name, skill name, or `<skill>/<reference>`.

This repository is the **canonical source** for these skills — downstream collections should vendor from here rather than forking, so updates to the CLI and its skills land together.
