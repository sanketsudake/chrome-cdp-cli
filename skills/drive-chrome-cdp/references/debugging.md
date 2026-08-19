# Debugging

## Debugging (console & network)

When an action "did nothing", the page usually said why — read that instead of re-clicking or screenshotting:

- **`console`** — the tab's `console.*` output and uncaught exceptions, with stacks.
  Capture is buffered from the moment the connection attached to the tab, not from when you ask — so the error behind a click that already failed is there.
  `--only-errors` for what broke; `--grep "<regex>"` / `--level` / `--since 30s` / `--limit` filter server-side; `--follow` streams NDJSON while you act.
  A nonzero `dropped` in the result means the bounded buffer evicted messages before you read.
- **`net`** — every HTTP request the tab made: method, URL, status, timing, sizes.
  `--xhr` for API calls, `--failed` for non-2xx and network-level failures (the 401 behind an empty screen), `net --url /api/save --method POST --body` to inspect a payload.
  Headers/bodies appear only with `--headers`/`--body`, and credential-shaped values are redacted unless `--no-redact` — leave redaction on.
- **The reset → act → read pattern**: `console --clear` and `net --clear` before the action, act, then `console --only-errors` / `net --failed` — whatever is left is what the action caused.
- With `--no-daemon` no process was alive to buffer earlier events, so the history is partial (the envelope carries a note saying so).

