---
name: drive-chrome-cdp
description: Drive the user's real, already-running local Chrome — its live tabs, logins, and cookies, so it types no credentials — from the shell via the `chrome-cdp` CLI, which emits a uniform JSON envelope and a stable exit-code contract to branch on. Use when a skill names `chrome-cdp`, or when a task needs its primitives — accessibility `snap`/`find` with element refs, `--by name|ref|cell|label` addressing, `select` for cascade/combobox widgets, `grid` to read tables, `wait --request` to confirm a write, `console`/`net` to read why an action "did nothing", `activate` a throttled tab, `upload` without the OS dialog, `session` batches, saved `recipe` flows, or any raw CDP method. Triggers include "click X in my browser", "read what's on my screen", "fill in this form in Chrome", "check console/network errors on this page", "automate this web app in my logged-in session". The building block other logged-in-app skills follow to get a driven, signed-in tab.
license: MIT
metadata:
  author: sanketsudake
  version: "1.0"
---

# Drive local Chrome via chrome-cdp

Start here — the CLI serves the guide that matches the installed version:

```sh
chrome-cdp skill            # the core loop: doctor → list → use → snap → act → verify
chrome-cdp skill --full     # plus every reference below
chrome-cdp skill list       # every embedded skill, then these reference names
chrome-cdp skill get <name> # one reference, or another skill by name
```

If the binary is missing, read [`references/core.md`](references/core.md) and install per the [README](https://github.com/sanketsudake/chrome-cdp-cli#quickstart).

References (same content `skill get <name>` prints):

- [`core`](references/core.md) — setup, consent states, the loop, reading, addressing, waiting, scrolling, the output contract, safety, session & passkeys.
- [`widgets`](references/widgets.md) — `select` cascades, `--at x,y` coordinates, `upload`.
- [`debugging`](references/debugging.md) — `console`, `net`.
- [`batch-and-recipes`](references/batch-and-recipes.md) — `session`, `recipe`, `record`.
- [`examples`](references/examples.md) — worked, end-to-end command sequences.

Never type credentials or drive a passkey; stop at a login page and ask the user to sign in.
