# Documentation

Start with the [project README](../README.md) for what `chrome-cdp` is and a five-minute quickstart.
From there:

## Reference

- **[CLI reference](cli-reference.md)** — every command, flag, exit code, and the output contract, in lookup tables.

## Scenario guides

- **[Automating a logged-in web app](scenarios/automating-a-logged-in-web-app.md)** — the `list → use → snap → act → verify` loop against an app you're already signed into.
- **[Forms and grids](scenarios/forms-and-grids.md)** — fill fields by visible label, grid cells by column header, and a whole grid over one batched connection.
- **[Driving widgets with `select`](scenarios/driving-widgets-with-select.md)** — portal menus, cascade prompts, and native `<select>`s that a plain click can't open.

## For tool authors

- **[Using chrome-cdp from an AI agent](using-with-ai-agents.md)** — why the JSON envelope, exit codes, and accessibility reads make this a good agent tool, and the guardrails to keep.

## For contributors

- **[RFCs](rfc/README.md)** — design proposals for what's next, in priority order, each with user stories and a verification plan.
  Start here if you want to pick something up.

---

Missing a scenario you'd find useful?
Candidates not yet written: reading virtualized lists/calendars (scroll + `--dedupe`), piercing shadow DOM and iframes, debugging a page with `raw` CDP methods, and emulating viewport/geolocation for responsive checks.
Open an issue and say what you're automating.
