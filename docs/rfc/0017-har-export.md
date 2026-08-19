# RFC-0017: `net --har` — export the tab's retained requests as HAR 1.2

- **Status:** Accepted — implemented on branch feat/agent-browser-parity
- **Priority:** P2
- **Area:** observability
- **Depends on:** RFC-0003 (the `net` buffer, its filters, its redaction rules and its envelope rows), RFC-0011 (`internal/encode` as the home of pure, browser-free encoders)
- **Related:** RFC-0004 (the MCP surface, which this RFC deliberately does not extend), RFC-0008 (the `-o` file-writing conventions `--har` follows)

## Summary

Add `--har <path>` to `net`.
It runs the same filtered read `net` already performs, then writes the matching requests to `<path>` as an [HTTP Archive 1.2](http://www.softwareishard.com/blog/har-12-spec/) file, and prints a short summary envelope instead of the request listing.

The file is built by a **pure encoder**, `encode.HAR`, over a typed copy of the rows `net`'s envelope already carries.
Nothing about the browser interface, the daemon RPC, the filters, or the redaction rules changes: `--har` is an output mode of the existing read, the way `-o` is an output mode of `screenshot`.
One additive envelope field (`started_at`, the wall-clock start of each request) is introduced because HAR requires an absolute timestamp and the listing only carried a relative one.

Without `--har`, `net` behaves exactly as today apart from that one added field.

## Motivation

`net` answers "what did the page request" well inside a terminal, but the answer stays inside the terminal.

- **The evidence cannot be handed to anybody.**
  A failing login flow, a slow dashboard, a CORS refusal — the diagnosis is a sequence of requests with their headers and timings, and the people who need to see it (a backend team, a vendor, a bug report) expect a HAR, which every browser's DevTools, Charles, Fiddler, and a dozen web viewers open.
  Today the only option is to paste JSON envelopes that no tool reads.
- **Redaction is already solved, and that is what makes an export safe to hand over.**
  RFC-0003's redaction of credential headers, URL parameters and body fields runs where the buffer lives; an export that reuses those rows is credential-safe by default without a second rule set.
- **Every piece exists.**
  The buffer retains method, URL, headers, request bodies, timings, cache and failure state; `--body` fetches response bodies on demand; `internal/encode` is the established place for a pure encoder with a table-driven test.
  This RFC wires them together behind one flag.

## User stories

**US-1 — Export what I just saw.**
As a developer, I want `net --failed --har bad.har` to write exactly the requests `net --failed` would list, so that the file and the terminal agree.
*Acceptance:* the HAR has one entry per listed request, in `startedDateTime` order, and the envelope reports `entries` equal to that count.

**US-2 — Hand it over without leaking my session.**
As a user of my real, logged-in Chrome, I want the HAR redacted by default so that sharing it does not share my cookies or tokens.
*Acceptance:* without `--no-redact`, every header value RFC-0003 redacts appears as `<redacted>` in the file, credential-shaped query parameters are redacted in `request.url` and `queryString`, and the envelope says `redacted: true`.

**US-3 — Include payloads when I ask.**
As a developer debugging an API, I want `--har --body` to include request and response bodies, so that the HAR can be replayed or diffed.
*Acceptance:* with `--body`, `request.postData.text` and `response.content.text` are present for every entry whose body was text and available; without `--body`, neither is present and the read issues no body fetches.

**US-4 — Never reach Chrome for a bad path.**
As a script author, I want `--har ''` and an unwritable destination refused before the CLI connects, so that a typo does not cost a consent prompt or a cleared buffer.
*Acceptance:* `--har ''` is `usage` / exit 2 and `--har /nonexistent/dir/out.har` is `generic` / exit 1, both with no browser call.

**US-5 — Works in a batch.**
As an agent, I want `net --har` inside `session` so that an "act, then export" flow is one connection.
*Acceptance:* inside `session` the line produces exactly one envelope, the summary.

**US-6 — The listing is untouched.**
As an existing user, I want `net` without `--har` to return what it does today.
*Acceptance:* without the flag, no file is written, the result carries `requests` as before, and the only difference is the new `started_at` field on each row.

## Proposed CLI surface

```
chrome-cdp net --har <path> [existing filter and rendering flags…]
```

| Flag | Default | Purpose |
|------|---------|---------|
| `--har <path>` | — | write the matching requests to `<path>` as HAR 1.2 (headers always included, redacted unless `--no-redact`; add `--body` for payloads) and print a summary instead of the listing |

A flag, not a subcommand: the export is the same read with the same `--url` / `--method` / `--status` / `--type` / `--xhr` / `--failed` / `--since` / `--limit` / `--clear` grammar, and a second command would have to re-register every one of those flags and keep them in step.

Interactions, all decided:

- **Filters apply before export.**
  The HAR holds exactly what the listing would have held; the daemon-side filter runs unchanged.
- **`--limit` applies, default 100.**
  An export of the whole buffer is `--limit 0`; the default is not changed for `--har` because one grammar with one default is the point of a flag.
- **`--headers` is implied.**
  `--har` sets `NetOpts.Headers = true` regardless of the flag, because `render` emits `request_headers` / `response_headers` only when that option is set and a HAR without headers is not worth writing.
  Passing `--headers` explicitly is accepted and changes nothing.
- **`--body` is the "with content" switch.**
  Chrome's own DevTools offers "Save all as HAR" and "Save all as HAR with content"; `--har` is the former and `--har --body` the latter.
  Bodies stay opt-in because response bodies are fetched at read time with one `Network.getResponseBody` per entry (`netFetchBodies`), and `render` gates **both** `request_body` and `response_body` on `NetOpts.Body`, so a bare `--har` carries no `postData` either.
  Splitting the two would need a new `NetOpts` field and an RPC change for a distinction nobody has asked for.
- **`--no-redact` is the only opt-out**, exactly as for the listing; it is passed through and reported as `redacted: false`.
- **`--clear` composes**: read and drop, then export — the "export and reset" idiom.
  The buffer is dropped inside the read itself (`Query.Clear` runs daemon-side under the buffer's lock), and the file is written afterwards in the CLI, so a write that fails at that point has lost the data; the pre-connect directory check below removes the likely cause, and the residual risk is accepted.
- **`--fail-on-match` composes**: the file is written, then the assertion is judged on the same count, and a tripped assertion reports the summary as `result` with `assertion_failed`.
- **`--follow` is refused**: a stream has no end to write at.
- **`net wait` / `wait --request` do not take `--har`**; they report one request, not an archive.

Examples:

```sh
chrome-cdp net --har requests.har                      # everything listed, headers redacted
chrome-cdp net --failed --har bad.har                  # only what broke
chrome-cdp net --xhr --body --har api.har              # API calls with payloads
chrome-cdp net --limit 0 --har all.har --clear         # the whole buffer, then reset
```

## Result envelope

With `--har` the result is a summary; the `requests` array is **not** printed, because the file is the deliverable and a second copy of it on stdout would defeat the point of exporting.

```json
{ "ok": true, "command": "net",
  "target": {"id":"…","title":"…","url":"…"},
  "result": { "path": "requests.har", "bytes": 18342,
              "entries": 37, "redacted": true, "with_content": false,
              "truncated_bodies": 0, "pending": 2,
              "buffered": 214, "dropped": 3, "truncated": false },
  "elapsed_ms": 41 }
```

| Field | Meaning |
|-------|---------|
| `path` | the path as given on the command line (not made absolute), as `screenshot` reports it |
| `bytes` | size of the file written |
| `entries` | number of HAR entries, equal to the listing's `count` |
| `redacted` | `true` unless `--no-redact` was passed |
| `with_content` | `true` when `--body` was passed and bodies were requested |
| `truncated_bodies` | number of entries whose request or response body was cut at `net_max_body` (the listing's `body_truncated`); always present, `0` without `--body` |
| `pending` | the listing's `pending`, unchanged in meaning |
| `buffered`, `dropped`, `truncated` | the listing's accounting, unchanged in meaning — `truncated` is "`--limit` cut the match list", distinct from `truncated_bodies` |
| `note` | passed through when the listing carries it (capture was not yet running on this tab) |

Human mode prints `✓ path: requests.har`, because `oneLine` already prefers a `path` key; no formatter change.

### The one envelope change: `started_at`

Every request row — in the listing, in `--follow` payloads and in `net wait`'s `request`, since all three go through `render` — gains `started_at`: the request's start as an RFC 3339 UTC timestamp with millisecond precision (`"2026-08-19T10:15:00.412Z"`), or `null` when the record has no start.
It sits beside `started_ms`, which keeps its relative meaning.
The layout is Go's `"2006-01-02T15:04:05.000Z07:00"` applied to `Started.UTC()`, so the value always ends in `Z` and always carries three fractional digits, the precision `netTime` truncates to.
HAR requires `startedDateTime` and the listing only carried milliseconds since capture began on the tab, with no epoch to add them to, so the absolute instant has to come from the record.
It is additive, always present, and useful on its own (it is what correlates a request with a server log line).
`netRecord.Started` already holds the value; every `netApply` branch calls `netStart`, so a buffered record always has one, and the `null` case exists only to keep the row shape total.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| `--har ''` (empty path) | `usage` | 2 |
| `--har` with `--follow` | `usage` | 2 |
| `--har` path names an existing directory, or its parent directory does not exist or is not a directory | `generic` | 1 |
| The file write fails after the read (permissions, disk full) | `generic` | 1 |
| Every existing RFC-0003 failure | unchanged | unchanged |

No new codes.
The path checks run **before** `resolveTarget`, so none of them touches Chrome.
A bad parent directory is `generic` rather than `usage` because detecting it needs `os.Stat` and the outcome depends on the environment, not on the form of the invocation — the same split `record stop -o` made (`checkExportTarget` failures are `CodeGeneric`, before the recording is drained).
The late write failure is `generic` as it is for `screenshot -o` (`emitArtifact`).

## Design notes

### No interface change, no RPC change

`Browser.Net(ctx, id, opts NetOpts) (any, error)` is unchanged, `NetOpts` is unchanged, `remoteBrowser.Net` and the `dispatch` case are unchanged, and `chrometest.StubBrowser` needs no edit.
The CLI sets `opts.Headers = true` (and `opts.Body`, `opts.NoRedact` from the flags), calls `Net` exactly as the listing does, and hands the returned rows to the encoder.
`TestDispatchCoversBrowser` stays green with no new case.

What the implementation touches, beyond the brief's list: `render` in `internal/chrome/net.go` gains the `started_at` key, its test gains a row-shape assertion, and the per-request field list in `docs/cli-reference.md` gains the field.

### The encoder's input type: `encode.NetEntry`, filled from the rows by a JSON round trip

`Net` returns `any`: in-process it is the `map[string]any` `render` builds, with Go-typed values (`int64`, `map[string]string`, `nil`); through the daemon it is the same map after a JSON round trip, with `float64` numbers — the split `netCount` already tolerates.
`render` deliberately builds a map, because several keys must be *absent* rather than `null` (RFC-0003 VS-6), and there is no public row type to pass around.

The brief's preferred option — a `chrome.NetEntry` that `render` also fills — is not available: `internal/chrome` already imports `internal/encode` (`browser.go`, `annotate.go`, `capture.go`), so `encode` importing `chrome` would be an import cycle, and moving the encoder into `chrome` would put a pure, browser-free encoder next to the CDP code RFC-0011 split it away from.
Converting `render` to a struct is also out: `omitempty` cannot express "present but empty" for `request_headers: {}` nor "present and null" for `status`, so the envelope shape would change.

So the row type lives in the pure package and is filled from the rows:

```go
// internal/encode/har.go

// NetEntry is one request as the `net` envelope reports it. The JSON tags ARE
// the envelope keys (RFC-0003, RFC-0017): the CLI fills it from the rows `Net`
// returns by a JSON round trip, which also normalises the int64/float64 split
// between the in-process and daemon paths.
type NetEntry struct {
    ID           string            `json:"id"`
    Method       string            `json:"method"`
    URL          string            `json:"url"`
    Type         string            `json:"type"`
    Status       *int64            `json:"status"`      // nil: no status (pending or network-level failure)
    StatusText   string            `json:"status_text"`
    StartedAt    string            `json:"started_at"`  // RFC 3339 UTC, ms; "" when the row carries none
    StartedMs    int64             `json:"started_ms"`
    DurationMs   *int64            `json:"duration_ms"` // nil: not finished
    RequestSize  int64             `json:"request_size"`
    ResponseSize int64             `json:"response_size"`
    FromCache    bool              `json:"from_cache"`
    Failed       bool              `json:"failed"`
    Error        *string           `json:"error"`
    Pending      bool              `json:"pending"`
    RequestHeaders         map[string]string `json:"request_headers"`
    ResponseHeaders        map[string]string `json:"response_headers"`
    RequestBody            *string           `json:"request_body"`
    RequestBodyUnavailable bool              `json:"request_body_unavailable"`
    ResponseBody           *string           `json:"response_body"`
    BodyUnavailable        bool              `json:"body_unavailable"`
    BodyTruncated          bool              `json:"body_truncated"`
}

// DecodeNetEntries turns the `requests` value of a net result into typed rows.
func DecodeNetEntries(rows any) ([]NetEntry, error)

// HAROpts parameterises an export. Version is the CLI build version for
// log.creator; Now is the export instant, used only for a row with no start.
type HAROpts struct {
    Version string
    Now     time.Time
}

// HAR renders the entries as an HTTP Archive 1.2 document.
func HAR(entries []NetEntry, opts HAROpts) ([]byte, error)
```

`DecodeNetEntries` is `json.Marshal` of `rows` followed by `json.Unmarshal` into `[]NetEntry`; a `rows` that is not a JSON array of objects is an error.
Drift between `render`'s keys and the struct tags is pinned by `TestRenderRowDecodesIntoNetEntry` in `internal/chrome/net_test.go` (see the test plan), which is legal because `chrome` already imports `encode` in non-test code.

### HAR 1.2 mapping

Values marked *ext* are HAR custom fields (leading underscore), which the spec allows and viewers ignore.
Chrome's own names are reused where Chrome has one (`_resourceType`).

**Log:**

| HAR | Value |
|-----|-------|
| `log.version` | `"1.2"` |
| `log.creator` | `{"name": "chrome-cdp", "version": <HAROpts.Version>}` |
| `log.browser` | omitted (the read does not learn Chrome's version) |
| `log.pages` | omitted; entries carry no `pageref` |
| `log.entries` | one per `NetEntry`, sorted ascending by `startedDateTime` (stable), as the spec recommends and independent of buffer order |

**Entry, per row:**

| HAR | Value | Notes |
|-----|-------|-------|
| `startedDateTime` | `StartedAt` | when `""` (a row from a daemon built before this RFC): `Now` in RFC 3339 UTC ms, plus *ext* `_startUnknown: true` — the waterfall is wrong, the entry is kept and the cause is named |
| `time` | `*DurationMs` when non-nil, else `0` | the spec defines `time` as the sum of the non-`-1` timings, so it cannot be `-1` |
| `request.method` | `Method` | |
| `request.url` | `URL` with any `#fragment` removed | the spec excludes fragments; redaction already happened in the row, so a redacted hash-router token is dropped, not leaked |
| `request.httpVersion` | `""` | the record does not retain the protocol; retaining `Response.protocol` is a possible follow-up, and fabricating `HTTP/1.1` for an h2 exchange is worse than an empty string |
| `request.headers` | `[{name, value}]` from `RequestHeaders`, sorted by name | values are already redacted or not, as the row says |
| `request.queryString` | `[{name, value}]` parsed from the query of `request.url`, in URL order: split on `&`, then on the first `=`; `url.QueryUnescape` each half, keeping the raw text on an unescape error; a part with no `=` has `value: ""` | redacted values appear as `<redacted>` because `RedactURL` rewrote them in the string |
| `request.cookies` | `[]` | the `cookie` header is redacted by default, and parsing it with `--no-redact` is out of scope; the raw header is in `request.headers` |
| `request.headersSize` | `-1` | |
| `request.bodySize` | `RequestSize` | bytes of post data as Chrome reported it, `0` when none |
| `request.postData` | present only when `RequestBody != nil`: `{"mimeType": <request `content-type` header, case-insensitive lookup, `""` if absent>, "text": *RequestBody}` | `RequestBodyUnavailable` sets *ext* `_requestBodyUnavailable: true` and no `postData` |
| `response.status` | `*Status`, or `0` when nil | `0` is what Chrome's own export writes for a failed or in-flight request |
| `response.statusText` | `StatusText` | |
| `response.httpVersion` | `""` | as for the request |
| `response.headers` | from `ResponseHeaders`, as for the request | |
| `response.cookies` | `[]` | as for the request |
| `response.content.size` | `ResponseSize` | encoded bytes; Chrome gives no decoded length without a body fetch, and the spec's distinction is not worth a second fetch |
| `response.content.mimeType` | response `content-type` header verbatim (case-insensitive lookup), `""` if absent | |
| `response.content.text` | `*ResponseBody` when non-nil; key omitted otherwise | no `encoding` key: the text is UTF-8 as `netTextBody` guaranteed |
| `response.redirectURL` | the response `location` header (already URL-redacted by `RedactHeaders`), `""` if absent | |
| `response.headersSize` | `-1` | |
| `response.bodySize` | `ResponseSize` when `Status != nil`, else `-1` | |
| `cache` | `{}`; with `FromCache`, `{"_fromCache": true}` | *ext*; Chrome writes a string (`"disk"` / `"memory"`) here, and the record cannot tell disk from memory from service worker, so a bool is honest where a string would be a guess |
| `timings` | `{"blocked": -1, "dns": -1, "connect": -1, "ssl": -1, "send": 0, "wait": <time>, "receive": 0}` | the record has start and end only; everything it does not know is `-1`, and the whole duration is attributed to `wait` so `time` equals the sum of the known timings |
| `_resourceType` | `Type` | *ext*, Chrome's own field name and vocabulary |
| `_id` | `ID` | *ext*, the CDP request id, so an entry can be matched back to a `net` row |
| `_startedMs` | `StartedMs` | *ext*, the listing's relative time, for correlation with `console` |
| `_pending` | `true` when `Pending` | *ext*, omitted when false |
| `_failed` | `true` when `Failed` | *ext*, omitted when false; the envelope's meaning (network-level failure **or** non-2xx), so a delivered 500 carries it too |
| `_error` | `*Error` when non-nil | *ext*, the network-level failure text |
| `_bodyUnavailable` | `true` when `BodyUnavailable` | *ext*, omitted when false |
| `_bodyTruncated` | `true` when `BodyTruncated` | *ext*, omitted when false; these entries are what `truncated_bodies` counts |

Worked states, so the implementer interpolates nothing:

| State | `response.status` | `time` / `timings.wait` | `response.bodySize` | Extensions |
|-------|-------------------|-------------------------|---------------------|------------|
| delivered 200 | `200` | `DurationMs` | `ResponseSize` | `_resourceType`, `_id`, `_startedMs` |
| delivered 500 | `500` | `DurationMs` | `ResponseSize` | + `_failed: true` |
| network-level failure | `0` | `DurationMs` (failure time is an end) | `-1` | + `_failed: true`, `_error: "net::ERR_…"` |
| pending | `0` | `0` | `-1` | + `_pending: true` |
| from cache | `200` | `DurationMs` | `ResponseSize` | + `cache._fromCache: true` |

### Output bytes

`json.Encoder` with `SetIndent("", "  ")` and `SetEscapeHTML(false)`, then a trailing newline.
`SetEscapeHTML(false)` matters: `json.Marshal` would write `NetRedacted` (`<redacted>`) as `\u003credacted\u003e`, which a reader grepping the file for redaction would not find.
Indented, because a HAR is read by people as often as by tools and size is not a concern at the buffer's bounds (500 records, 64 KB per body).

### Redaction

The encoder does **not** redact; it copies.
Redaction is applied where the buffer lives, by `render` via `RedactHeaders` / `RedactURL` / `RedactBody`, before the rows reach the CLI, and `--no-redact` is the only way round it (RFC-0003).
This is what makes the export safe with one rule set: the HAR can contain nothing the listing would not have printed under the same flags.
`redacted` in the summary is `!NoRedact`.

### File write

`os.WriteFile(path, data, 0o600)`; an existing file is overwritten, because the user named the path.
Mode `0o600`, not the `0o644` screenshots use: a HAR is a record of the user's logged-in session, with `--no-redact` it holds live cookies and tokens verbatim, and the policy audit log already uses `0o600` for the same reason.

Before connecting, in this order: an empty path is `usage`; a path that is an existing directory, or whose `filepath.Dir` does not exist or is not a directory (`os.Stat`), is `generic` with `cannot write the HAR to %q: …`.
No `CreateTemp` probe: `record` needs one because a failed write there loses a recording, whereas here a retry is free.

### Policy and MCP

`net` is `Reading` in `verbClass` and stays so; `--har` writes a local file in the CLI's own process and reads nothing new from the browser, so no policy entry changes.

`--har` is **not** exposed to the MCP `network` tool.
An MCP client is on the other side of a protocol from the server's disk, so a path in the response is unusable to it.
`screenshot`'s `output` argument is different in kind: it is an *also*, beside the image returned inline, whereas `--har` has no inline form and would hand the client nothing but a path.
The rows themselves are already available inline through the `network` tool's `headers` and `body` arguments, which is the shape an MCP client can use.
The registry is an explicit allow-list, so exclusion is simply not adding the argument; `TestNetworkToolDoesNotExposeHar` states the intent the way `TestRecordAndRecipeAreNotExposed` does.

### Inside `session`

Allowed: the line is one envelope, the summary, and the write happens in the CLI process that is running the session.
`--follow` remains the only `net` form `session` refuses.

## Verification scenarios

**VS-1 — Filters decide the contents.**
Given a stub whose `Net` returns three rows
When `net --failed --har out.har` runs
Then `NetOpts.Failed` is `true`, `NetOpts.Headers` is `true`, the file has three `log.entries`, and `result.entries` is `3`.

**VS-2 — Redaction reaches the file literally.**
Given a stub row with `request_headers: {"authorization": "<redacted>"}` and `url: "https://app/cb?code=<redacted>"`
When `net --har out.har` runs
Then the file contains the byte sequence `"<redacted>"` (not `\u003credacted\u003e`), in `request.headers` and in `request.queryString`, and `result.redacted` is `true`.

**VS-3 — `--no-redact` passes through.**
Given VS-2's stub
When `net --har out.har --no-redact` runs
Then `NetOpts.NoRedact` is `true` and `result.redacted` is `false`.

**VS-4 — Bodies are opt-in.**
Given a stub row with `request_body` and `response_body`
When `net --har out.har` runs
Then `NetOpts.Body` is `false`, `result.with_content` is `false`, and no entry has `postData` or `content.text`;
When `net --har out.har --body` runs
Then `NetOpts.Body` is `true`, `result.with_content` is `true`, and the entry has both.

**VS-5 — Bad paths never connect.**
Table over `noCallBrowser`: `--har ''` → `usage` exit 2; `--har out.har --follow` → `usage` exit 2; `--har <tmpdir>` (a directory) → `generic` exit 1; `--har <tmpdir>/missing/out.har` → `generic` exit 1.

**VS-6 — The file is private and overwritten.**
Given an `out.har` that does not exist, and then one that exists with stale content
When `net --har out.har` runs each time
Then the fresh file is created with mode `0o600`, the existing file's content is replaced by the new HAR, and `result.bytes` equals the file's size both times.
`os.WriteFile` applies the mode only on creation, so the overwritten file keeps its own mode and the test does not assert it.

**VS-7 — HAR skeleton and required fields.**
Given two `NetEntry` values — one delivered 200 from cache, one network-level failure — and one pending
When `encode.HAR` runs
Then `log.version == "1.2"`, `log.creator.name == "chrome-cdp"`, each entry has `startedDateTime`, `time`, `request.{method,url,httpVersion,headers,queryString,cookies,headersSize,bodySize}`, `response.{status,statusText,httpVersion,headers,cookies,content,redirectURL,headersSize,bodySize}`, `cache`, `timings.{send,wait,receive}`, the cached entry has `cache._fromCache == true`, the failed entry has `response.status == 0`, `_failed == true` and `_error` set, the pending entry has `_pending == true` and `time == 0`, and `time` equals `timings.wait` on every entry.

**VS-8 — Entries are sorted by start.**
Given entries supplied newest-first
When `encode.HAR` runs
Then `log.entries[i].startedDateTime <= log.entries[i+1].startedDateTime`.

**VS-9 — A row with no start is kept and flagged.**
Given an entry with `StartedAt == ""` and `HAROpts.Now` fixed
When `encode.HAR` runs
Then `startedDateTime` equals `Now` in RFC 3339 UTC ms and `_startUnknown == true`.

**VS-10 — Rows decode from both connection paths.**
Given a `[]any` of `map[string]any` rows once with `int64` values and once with `float64` values and a `nil` status
When `DecodeNetEntries` runs
Then both produce identical `[]NetEntry` and the `nil` status is a nil pointer.

**VS-11 — `render` and `NetEntry` agree.**
Given a `netRecord` with every field set, rendered with `Headers: true, Body: true` and an available body
When the row is marshalled and unmarshalled into `encode.NetEntry`
Then every `NetEntry` field is populated and `StartedAt` parses with `time.RFC3339Nano`.

**VS-12 — `started_at` is in the envelope.**
Given a buffered request (live)
When `net` runs without `--har`
Then each row has `started_at` parsing as RFC 3339 UTC, within the test's wall-clock window, and the existing fields are unchanged.

**VS-13 — Inside `session`.**
Given a `session` stdin with `net --har out.har`
When it runs
Then exactly one envelope line is emitted for it and the file exists.

**VS-14 — The MCP surface is unchanged.**
The `network` tool's argument list has no `har`.

## Test plan

**Pure (`t.Parallel()`), `internal/encode/har_test.go`.**
`TestHARLogSkeleton` (VS-7, the table of three entries), `TestHAREntriesAreSortedByStart` (VS-8), `TestHARMissingStartFallsBackToNow` (VS-9), `TestHARWritesRedactedValuesLiterally` (the `SetEscapeHTML` half of VS-2: the bytes contain `"<redacted>"`), `TestHARQueryStringParsing` (order, unescape, a part with no `=`, a redacted value), `TestDecodeNetEntriesNormalisesNumbers` (VS-10).

**Pure, `internal/chrome/net_test.go`.**
`TestRenderCarriesStartedAt` (the key is present, RFC 3339 UTC ms, `null` for a record with `HasStart == false`), `TestRenderRowDecodesIntoNetEntry` (VS-11).
The existing `TestRenderOmitsHeaderAndBodyKeysUnlessRequested` gains `started_at` in its always-present list.

**Stub-backed, `internal/cli/net_test.go`.**
`TestNetHarWritesTheFileAndSummarises` (VS-1, VS-2, VS-6: file written in `t.TempDir()`, mode, summary keys, `log.version`), `TestNetHarBodiesAreOptIn` (VS-4), `TestNetHarNoRedactPassesThrough` (VS-3), `TestNetHarValidationNeverConnects` (VS-5, added as cases to `TestNetValidationNeverConnects`'s `noCallBrowser` pattern, with the `generic` cases asserting exit 1), `TestNetHarInsideSessionIsOneEnvelope` (VS-13).

**MCP, `internal/mcp/tools_test.go`.**
`TestNetworkToolDoesNotExposeHar` (VS-14).

**Live Chrome (`internal/chrome/net_test.go`, `testing.Short()`-guarded, not parallel).**
VS-12 as an assertion added to `TestNetCapturesACompletedXHR`; no new live test, because the encoder and the flag are fully exercised without a browser.

## Out of scope

- `--har -` (HAR to stdout); the envelope owns stdout, and a file path is what every HAR consumer takes.
- `--har` on `net wait` / `wait --request`, or on `console`.
- `log.pages`, `pageref`, `serverIPAddress`, `connection`, and the `blocked` / `dns` / `connect` / `ssl` timings; the record does not carry them, and `Network.responseReceived`'s `timing` object would have to be retained to fill them.
- Parsing `cookie` / `set-cookie` headers into the `cookies` arrays.
- Retaining `Response.protocol` for `httpVersion`.
- Exposing `--har` to MCP.
- HAR import or replay.

## Open questions

None at proposal time.

**Implementation note — `--clear` gets the `record`-style writability probe, not just a stat.**
The body text above ("No `CreateTemp` probe: `record` needs one because a failed write there loses a recording, whereas here a retry is free") holds for `--har` on its own.
It stops holding once `--clear` is also passed: `--clear` drops the buffer *inside the read itself* (`Query.Clear` runs daemon-side under the buffer's lock), before the CLI writes the file, so a write failure at that point loses the read's data exactly the way a failed `record stop -o` write loses the recording — a retry is no longer free.
The implementation therefore runs `record stop -o`'s `os.CreateTemp`-in-the-target-directory probe (create, close, remove) before connecting whenever `--har` is combined with `--clear`, in addition to the stat-only existing-directory / missing-parent check that applies unconditionally.
Without `--clear` the stat-only check stands exactly as specified above.
