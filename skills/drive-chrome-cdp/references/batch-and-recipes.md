# Batch Mode & Recipes

## Batch mode & refs

For a multi-step flow, `session` avoids a process spawn + reconnect per command:

- **`session`** reads one command per stdin line as a **JSON argv array**, runs each over a single held connection, and emits one JSON envelope per line (NDJSON).
  Comment (`#`) and blank lines are skipped; it exits 0 on clean EOF with per-line status in the envelopes.
- Combine with `snap`'s `ref` and `--by ref` to act on nodes without re-resolving them:

```sh
printf '%s\n' \
  '["use","url:workday"]' \
  '["snap"]' \
  '["click","e42","--by","ref"]' | chrome-cdp session
```

Filling a grid is the natural fit — cell-addressed `fill`s + a single `value --all` read-back over one connection, then act-and-confirm the save:

```sh
printf '%s\n' \
  '["fill","--by","cell","Mon, 7/13","8"]' \
  '["fill","--by","cell","Tue, 7/14","8"]' \
  '["fill","--by","cell","Wed, 7/15","8"]' \
  '["value","--all","input[data-automation-id=numericInput]"]' \
  '["click","--by","name","Save and Close","--role","button","--wait-text","saved"]' \
  | chrome-cdp session
```

## Saved flows (`recipe`) & recording (`record`)

- **`recipe`** — a saved, parameterised flow: a YAML file whose steps are the same argv arrays `session` reads, with declared inputs substituted into argv elements.
  There is no shell in the format, so a recipe is reviewable by reading it: `recipe show <name>` prints the source, `recipe run <name> --dry-run` prints the exact commands it would run — read before running one you didn't write.
  Resolution order: `./.chrome-cdp/recipes` (a recipe committed to a repo wins) → `$XDG_CONFIG_HOME/chrome-cdp/recipes` → `--dir`; scaffold with `recipe new`.
  A flow a skill repeats every run (a timesheet fill, an approval sweep) belongs in a shipped recipe, not re-derived from `snap` each time.
- **`record start` … drive … `record stop -o demo.gif`** — record the tab while other commands drive it; export a GIF (or MP4/WebM, or PNG frames).
  The daemon holds the frames, not the starting command, so a run that crashed half-way still has the failure on film — `record stop` afterwards still writes it.
  Per-tab (a batch that opens new tabs records the one it started on); `--annotate` marks action positions; `record status`/`record cancel` manage it; `session --record out.gif` wraps a whole batch in one flag.
  It records the user's real logged-in browser — look at the file before attaching it anywhere public.

