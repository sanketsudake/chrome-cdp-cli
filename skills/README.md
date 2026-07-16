# Skills

Companion [Agent Skills](https://docs.claude.com/en/docs/claude-code/skills) for `chrome-cdp`, maintained alongside the CLI so they never drift from the commands they document.

- [`drive-chrome-cdp`](drive-chrome-cdp/SKILL.md) — the core usage reference for an agent driving the CLI: the `list → use → snap → act → verify` loop, `--by` addressing (accessible name, `ref`, `cell`, `label`, `in-row`), the `select` verb for prompt/combobox/cascade widgets, `session` batching, the JSON envelope + exit-code contract, and the passkey/consent rules.

## Using a skill

Each skill is a directory with a `SKILL.md` (YAML frontmatter + instructions).
Point your agent harness at this `skills/` directory, or copy an individual skill into your own skill collection.

This repository is the **canonical source** for these skills — downstream collections should vendor from here rather than forking, so updates to the CLI and its skills land together.
