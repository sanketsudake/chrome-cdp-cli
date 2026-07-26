# RFC-0003: Network requests — the `net` verb

- **Status:** Draft
- **Priority:** P0
- **Area:** observability
- **Depends on:** RFC-0002 (reuses the daemon-side CDP event buffer)

## Summary

Add a `net` verb that reads the HTTP requests a tab made — method, URL, status, timing, sizes, and optionally headers and bodies — with server-side filtering and a `--follow` mode.
Add `net wait` so an automation can block until a specific request completes, which is a sharper tool than `wait --idle` for the common "did my save actually POST" question.

## Motivation

`wait --idle` already knows when the network is quiet, so the plumbing to observe requests partially exists — but nothing surfaces *what* those requests were.
That leaves three concrete holes:

- **Verification.**
  After a `click --wait-text "Saved"`, the toast proves the UI rendered; it does not prove the server accepted anything.
  Confirming `POST /api/timesheet → 200` is the assertion that actually matters, and today it is unexpressible.
- **Debugging.**
  When an app silently fails, the answer is usually a 401, a 500, or a CORS rejection.
  `chrome-cdp` currently reports none of them.
- **Discovery.**
  Users automating an internal app frequently want to skip the UI entirely once they find the endpoint behind a button.
  Observing the request is how they find it.

Like RFC-0002, this is unreachable through `raw`, which cannot subscribe to `Network.*` events.
Both RFCs need the same infrastructure; RFC-0002 designs it and this one consumes it.

## User stories

**US-1 — Prove a write landed.**
As an automation author, I want to confirm the save request returned 2xx so that a green toast rendered by optimistic UI does not convince me a failed write succeeded.
*Acceptance:* `chrome-cdp net --url "/api/timesheet" --method POST --limit 1` reports `status: 200`.

**US-2 — Diagnose a broken page.**
As a web developer, I want to list failed requests so that I immediately see the 401 or 500 behind an empty screen.
*Acceptance:* `chrome-cdp net --failed` lists non-2xx and network-level failures with their status or error text.

**US-3 — Block until a request completes.**
As an automation author, I want to wait for a specific endpoint rather than for general network idle so that a page with polling or a long-lived stream does not make my wait unreliable.
*Acceptance:* `chrome-cdp net wait --url "/api/save" --status 2xx` returns as soon as that request completes, and exits 4 on timeout.

**US-4 — Find the endpoint behind a button.**
As a power user, I want to see what a click requested so that I can call the API directly next time.
*Acceptance:* `net --clear`, click, `net --xhr` shows the request with its method, URL, and request body.

**US-5 — Inspect a payload without leaking it everywhere.**
As a developer, I want response bodies only when I ask for them so that a routine listing stays small and does not spill tokens or PII into logs by default.
*Acceptance:* bodies and headers are omitted unless `--body` / `--headers` is passed, and `Authorization`/`Cookie` are redacted unless `--no-redact` is given.

**US-6 — Keep the listing readable.**
As an agent, I want to exclude images, fonts, and stylesheets so that the response is the handful of API calls I care about.
*Acceptance:* `--xhr` (alias for `--type xhr --type fetch`) returns only those.

## Proposed CLI surface

```
chrome-cdp net [--url <substr|re>] [--method <m>] [--status <spec>] [--type <t>]
               [--xhr] [--failed] [--limit <n>] [--since <dur>]
               [--headers] [--body] [--no-redact] [--clear] [--follow]
               [--fail-on-match]

chrome-cdp net wait --url <substr> [--method <m>] [--status <spec>] [--timeout <dur>]
```

| Flag | Default | Purpose |
|------|---------|---------|
| `--url <s>` | — | substring match; `re:` prefix switches to regex |
| `--method <m>` | all | `GET`, `POST`, …; repeatable |
| `--status <spec>` | all | `200`, `2xx`, `>=400`, `!2xx` |
| `--type <t>` | all | `document`, `xhr`, `fetch`, `script`, `stylesheet`, `image`, `font`, `websocket`, `other`; repeatable |
| `--xhr` | off | shorthand for `--type xhr --type fetch` |
| `--failed` | off | non-2xx **or** network-level failure |
| `--limit <n>` | `100` | most recent *n* matching |
| `--since <dur>` | — | only newer than this |
| `--headers` | off | include request and response headers |
| `--body` | off | include request and response bodies (size-capped) |
| `--no-redact` | off | do not redact sensitive headers |
| `--clear` | off | drop buffered entries after reading |
| `--follow` | off | stream completions as NDJSON |
| `--fail-on-match` | off | exit nonzero if any entry matched |

Examples:

```sh
chrome-cdp net --xhr --limit 20                          # recent API calls
chrome-cdp net --failed                                  # what broke
chrome-cdp net --url "/api/save" --method POST --body    # inspect the payload
chrome-cdp net wait --url "/api/save" --status 2xx        # block until it lands
chrome-cdp net --clear && chrome-cdp click "#save" && chrome-cdp net --xhr
```

## Result envelope

```json
{ "ok": true, "command": "net",
  "target": {"id":"…","title":"…","url":"…"},
  "result": {
    "requests": [
      { "id": "req-41", "method": "POST", "url": "https://app/api/timesheet",
        "type": "xhr", "status": 200, "status_text": "OK",
        "started_ms": 1420, "duration_ms": 213,
        "request_size": 312, "response_size": 88,
        "from_cache": false, "failed": false, "error": null,
        "request_headers": {"content-type":"application/json","authorization":"<redacted>"},
        "response_headers": {"content-type":"application/json"},
        "request_body": "{\"hours\":8}", "response_body": "{\"ok\":true}" }
    ],
    "count": 1, "buffered": 214, "dropped": 0, "truncated": false, "pending": 2
  },
  "elapsed_ms": 4 }
```

`pending` counts requests started but not finished — the same signal `wait --idle` uses, exposed so a caller can tell "no matches" from "not finished yet".
Header and body fields are absent entirely (not `null`) unless requested, so the default envelope stays small.

`net wait` emits the single matched request in the same shape under `result.request`.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| Bad `--status` spec, unknown `--type`, invalid regex, bad `--since` | `usage` | 2 |
| `--follow` with `--fail-on-match`, or `--follow` inside `session` | `usage` | 2 |
| `net wait` timed out with no matching request | `target_timeout` | 4 |
| Network capture could not be enabled | `cdp_error` | 5 |
| `--fail-on-match` matched | `assertion_failed` (see RFC-0002 Q1) | 1 |

## Design notes

- **Same buffer, different events.**
  Reuses the daemon-side ring buffer from RFC-0002, retaining `Network.requestWillBeSent`, `responseReceived`, `loadingFinished`, and `loadingFailed`, correlated by request id into one record per request.
  Separate size bounds (`net_buffer`, default 500 records) because network records are larger than console lines.
- **Bodies are fetched lazily, not buffered.**
  Response bodies are pulled with `Network.getResponseBody` **only when `--body` is passed**, at read time.
  Buffering every body would multiply daemon memory by orders of magnitude and retain payloads the user never asked to see.
  Consequence, which must be documented: a body may no longer be available if the page navigated away, in which case `response_body` is `null` with a `body_unavailable: true` marker rather than an error.
- **Redaction is on by default** for `authorization`, `cookie`, `set-cookie`, `x-api-key`, `proxy-authorization`, and any header matching `token|secret|password` — replaced with `<redacted>`.
  `--no-redact` opts out explicitly.
  This matters because the CLI drives the user's *real, logged-in* browser, so its buffers contain live session credentials by construction.
  Bodies are capped (proposal: 64 KB, `net_max_body`) and truncated with a marker.
- **`--status` grammar** parses in the CLI before connecting: exact (`200`), class (`2xx`, `4xx`), comparison (`>=400`), negation (`!2xx`).
  Invalid specs are `usage`.
- **Interface:** ```go Net(ctx context.Context, targetID string, opts NetOpts) (any, error) NetStream(ctx context.Context, targetID string, opts NetOpts, emit func(any) error) error NetWait(ctx context.Context, targetID string, cond NetCond) (map[string]any, error) ```
- **`net wait` vs `wait --idle`.**
  They answer different questions and both should exist: `--idle` is "the page settled", `net wait` is "this specific call finished with this outcome".
  `net wait` matches against already-buffered requests first, so a request that completed between the action and the wait is not missed — a race the naive implementation would lose.
- **Stub:** `Net` returns `{"requests": []any{}, "count": 0, "buffered": 0, "pending": 0}`; `NetWait` returns `{"matched": false}`; `NetStream` returns nil.

## Verification scenarios

**VS-1 — Captures a completed XHR with status and timing** Given a fixture page that fetches a local test-server endpoint returning 200 When `net --xhr` runs Then one record appears with the method, URL, `status: 200`, and a positive `duration_ms`.

**VS-2 — Captures a failed request** Given a fixture fetching an endpoint that returns 500 When `net --failed` runs Then the record has `status: 500` and `failed: true`.

**VS-3 — Captures a network-level failure** Given a fixture fetching an unroutable host When `net --failed` runs Then the record has `failed: true`, a null `status`, and a non-empty `error`.

**VS-4 — Status spec parsing** Table over `200`, `2xx`, `>=400`, `!2xx`, `20x`, `abc`, empty Then the first four parse and match as expected; the last three are `usage` exit 2 with no connection.

**VS-5 — Type filtering** Given a fixture loading an image, a stylesheet, and making one fetch When `net --xhr` runs Then `count == 1` and `buffered >= 3`.

**VS-6 — Bodies are opt-in** Given a POST with a JSON body When `net --url /api` runs without `--body` Then no `request_body` or `response_body` key is present; When it runs with `--body` Then both are present and match the fixture.

**VS-7 — Redaction by default** Given a request sent with an `Authorization: Bearer secret123` header When `net --headers` runs Then the value is `<redacted>` and the literal `secret123` appears nowhere in the envelope; When `net --headers --no-redact` runs Then the real value is present.

**VS-8 — Body cap** Given a response larger than `net_max_body` When `net --body` runs Then the body is truncated to the cap and `body_truncated` is true.

**VS-9 — `net wait` returns on completion** Given `net wait --url /api/slow --status 2xx` running against an endpoint that responds after a delay Then the command returns after the response, within the delay plus a margin, with `ok`.

**VS-10 — `net wait` does not miss an already-completed request** Given a request that completed *before* `net wait` was invoked and is still buffered When `net wait --url` for it runs Then it returns immediately rather than timing out.

**VS-11 — `net wait` timeout** Given no matching request When `net wait --url /never --timeout 1s` runs Then exit is 4 with `target_timeout`.

**VS-12 — Clear scopes to one action** Given `net --clear`, then a click that triggers one fetch, then `net --xhr` Then exactly one record is returned.

**VS-13 — `pending` distinguishes empty from in-flight** Given a request that has started but not finished When `net` runs Then `pending >= 1`.

**VS-14 — Body unavailable after navigation** Given a request whose body is evicted by a navigation When `net --body` runs Then `response_body` is null and `body_unavailable` is true, and the envelope is still `ok`.

## Test plan

**Unit — status/type/url spec parsing (`internal/cli`, `t.Parallel()`, no browser)** VS-4 plus `--type` validation and `re:` prefix handling.
Good fuzz target: any input either parses to a matcher or is rejected — never panics, never silently matches everything.

**Unit — record correlation and redaction (`internal/eventbuf`, `t.Parallel()`)** Feed synthetic `requestWillBeSent` / `responseReceived` / `loadingFinished` / `loadingFailed` sequences and assert one correlated record, correct `pending` accounting, out-of-order event tolerance, eviction/`dropped`, and the redaction table (VS-7's redaction half needs no browser and should be tested here, exhaustively, per header name).

**Unit — command boundary (`chrometest.StubBrowser`)** Envelope shape, absence of header/body keys when not requested (VS-6's negative half), `--fail-on-match` exit mapping, `--follow` rejections.

**Live Chrome + `httptest` (`internal/chrome`, `testing.Short()`-guarded)** Stand up an `httptest.Server` inside the test that serves: a 200 JSON endpoint, a 500, a slow endpoint, an oversized body, an image, and a stylesheet.
Point a `data:` or served fixture page at it and cover VS-1, VS-2, VS-3, VS-5, VS-6, VS-8, VS-9, VS-10, VS-11, VS-12, VS-13, VS-14.
Using a local test server rather than the public internet keeps these hermetic and CI-safe — no external network dependency.
Do not parallelize; they share the spawned browser.

**Security regression test** An explicit test asserting the redaction default: construct a request carrying a credential-shaped header and assert the secret is absent from the marshalled envelope bytes.
This is a test worth having fail loudly, because the failure mode is leaking a live session token into a log.

## Out of scope

- Request interception, blocking, or mocking (`Fetch.enable`).
  That is a separate, much larger RFC and changes the CLI from an observer to a proxy.
- WebSocket frame contents (connections are listed; frames are not captured).
- HAR export.
  Worth a follow-up once the record shape is stable — the envelope above is deliberately close to HAR's fields to make that cheap later.
- Server-timing and detailed connection-phase breakdowns.

## Open questions

1. Should `net` buffer bodies for `--xhr` responses under a small size cap so US-4 works retroactively rather than only when `--body` was anticipated?
   **Recommendation:** no by default, but expose `net_buffer_bodies = "xhr"` as a config opt-in for interactive debugging sessions.
2. Should redaction also apply to URL query parameters that look like tokens?
   **Recommendation:** yes for values matching common token patterns in `access_token`/`api_key`/`sig` params, since the same leak risk applies.
3. Should `net wait` be a subcommand of `net` or a new condition on the existing `wait` verb (`wait --request /api/save`)?
   The latter is more discoverable and keeps all blocking in one place; the former keeps `net`'s flags together.
   **Recommendation:** add `wait --request <spec>` as the primary form and keep `net wait` as an alias, so users find it where they already look for waiting.
