# RFC-0019: `storage` — read and write the tab's `localStorage` and `sessionStorage`

- **Status:** Accepted — pending PR
- **Priority:** P2
- **Area:** reading / acting
- **Depends on:** RFC-0003 (the credential redaction predicates `RedactedHeaderName`, `RedactedParamName` and `RedactBody` in `internal/chrome/net.go`, and the `NetRedacted` placeholder, all reused verbatim), RFC-0012 (the policy classes the ten subcommands take)
- **Related:** RFC-0004 (the MCP surface; the verb is a new tool behind `--tools full`, for the reason given below), the `cookie` verb (`cmdCookie` in `internal/cli/commands.go`, whose shape this verb mirrors deliberately)

## Summary

Add a `storage` verb with two scopes and five actions:

```
chrome-cdp storage local|session list
chrome-cdp storage local|session get <key>
chrome-cdp storage local|session set <key> <value>
chrome-cdp storage local|session rm <key>
chrome-cdp storage local|session clear
```

It reads and writes the DOM Storage area of the tab's **top frame** — `localStorage` under `local`, `sessionStorage` under `session` — over the DevTools `DOMStorage` domain (`getDOMStorageItems`, `setDOMStorageItem`, `removeDOMStorageItem`, `clear`), keyed by the security origin of the main frame from `Page.getFrameTree`.
Nothing runs in the page's JavaScript context, so it works on a hidden tab, needs no `eval` and is not affected by a page that has shadowed `window.localStorage`.

The verb is the `cookie` verb's sibling and is shaped like it on purpose: subcommands rather than flags, positional key and value, the family name `storage` as the envelope's `command`, no flags of its own except on `list`.
`list` carries the one thing `cookie list` does not: **values are redacted by default** using the very predicates `net` applies to headers, URL parameters and bodies, because web storage is where single-page applications keep their session tokens.
`--no-redact` is the explicit opt-out; `get <key>` is itself the explicit ask and returns the raw value.

`--origin <o>` to address another frame's storage partition is out of scope; the top frame is the contract.

## Motivation

- **The state an agent needs to read lives in web storage, not in the DOM.**
  Feature flags, onboarding-dismissed markers, draft form contents, the "last selected project", the theme — on a modern SPA these are `localStorage` keys, and the only way to read them today is `eval "localStorage.getItem('x')"`, which every agent has to rediscover and which the policy layer classifies as Mutating.
- **Clearing it is the standard repro step.**
  "Does it still happen after clearing site data?" is the first question on any front-end bug, and `clear` here is the half of site data `cookie clear` does not cover.
- **Reading it must not print the session token.**
  Supabase keeps `sb-<ref>-auth-token`, Firebase keeps `firebase:authUser:<key>`, MSAL keeps its access tokens, redux-persist keeps whatever the app put in state — all in `localStorage`, all JSON strings with the token inside.
  An agent that `list`s the area to find a feature flag must not be handed the credential on the same line; `net` already solved exactly this for headers and bodies, and the same rules apply here.
- **`eval` is the wrong tool on three counts.**
  It is Mutating under policy, so a read-only operator cannot allow it; it returns the value unredacted; and on a page with an opaque origin it throws a `SecurityError` that surfaces as a generic `cdp_error` rather than as the answer "this page has no storage".
- **Every piece exists.**
  `cmdCookie` is the template for the command tree, `Frames` already calls `Page.getFrameTree`, `c.run` already attaches to the tab, and the redaction predicates and the `TruncateText` helper are exported.

## User stories

**US-1 — What has the app stored?**
As an agent, I want `storage local list` to show the keys and (redacted) values the page has in `localStorage`, so that I can read a feature flag or a draft without writing JavaScript.
*Acceptance:* on a page that ran `localStorage.setItem('theme','dark'); localStorage.setItem('access_token','eyJ…')`, the result lists both keys, `theme` with value `dark` and `access_token` with value `<redacted>`, and `count: 2`.

**US-2 — Read one value raw.**
As an agent, I want `storage local get sb-abc-auth-token` to return the full stored string, so that I can inspect it when I have decided I need to.
*Acceptance:* the result carries the value verbatim, unredacted and uncut, with `present: true`; for a key that is not there it is `present: false` with an empty `value`, exit 0.

**US-3 — Set a flag and see the page react.**
As an agent, I want `storage local set onboarding_done 1` followed by `nav --reload` to take effect, so that I can put an app into a known state before driving it.
*Acceptance:* after `set`, `eval "localStorage.getItem('onboarding_done')"` returns `"1"`; the result carries `set: true`.

**US-4 — Clear site state for a repro.**
As a developer, I want `storage local clear` and `storage session clear`, so that "clear site data" is two commands in a `session` script rather than a DevTools click.
*Acceptance:* after `clear`, `list` reports `count: 0` and the page's `localStorage.length` is 0.

**US-5 — A read-only operator can read but not write.**
As an operator who configured `read_only = true` for an origin, I want `storage local list` and `get` to work there and `set` / `rm` / `clear` to be refused with `permission_denied`.
*Acceptance:* the policy table classifies `list` and `get` as Reading and the other three as Mutating, and the existing enforcement does the rest.

**US-6 — The verb tells me when there is nothing to read.**
As an agent on a `data:` page or a fresh `about:blank` tab, I want `storage local list` to say the tab has no web storage, so that I do not retry.
*Acceptance:* the call fails with `target_not_found`, `opaque_origin: true`, and a message naming the cause, without issuing any `DOMStorage` command.

## Proposed CLI surface

```
chrome-cdp storage local|session list [--no-redact] [--max-value <bytes>]
chrome-cdp storage local|session get <key>
chrome-cdp storage local|session set <key> <value>
chrome-cdp storage local|session rm <key>
chrome-cdp storage local|session clear
```

| Subcommand | Args | Purpose |
|------------|------|---------|
| `list` | none | every key in the area, values redacted and size-capped |
| `get <key>` | exactly one | one value, raw and uncut; `present: false` when the key is absent |
| `set <key> <value>` | exactly two | create or overwrite one key |
| `rm <key>` | exactly one | remove one key; removing an absent key succeeds silently |
| `clear` | none | remove every key in the area |

| Flag (on `list` only) | Default | Meaning |
|-----------------------|---------|---------|
| `--no-redact` | off | do NOT redact credential-shaped keys and values (the same opt-out `net` has) |
| `--max-value <bytes>` | `4096` | cut each listed value at this many bytes and mark it `truncated: true`; `0` means no cap; a negative value is `usage` |

The scope is a subcommand, not a flag, so it reads as a sentence (`storage local get theme`) and so every policy row, skill sentence and MCP action names the scope it touches.
The verb addresses the **tab**, not an element, so the `QueryOpts` flags (`--by`, `--wait`, …) do not apply; it takes the global `--target`, `--timeout`, `--session`, `--json` like every other verb.
`--wait-text` is not offered: a storage write changes nothing on screen by itself.

`get` has no `--no-redact` and no cap because naming the key **is** the explicit ask, and a single value the caller asked for is the one case where cutting or masking it would make the command useless.
`list` is the orientation call, which is precisely where a token must not appear and where a 2 MB redux blob must not swamp the caller's context.

The key may be the empty string: `localStorage.setItem('', 'v')` is legal on the platform and Chrome's `DOMStorage.setDOMStorageItem` accepts it (measured, below), so the CLI does not refuse it; `cobra.ExactArgs` counts arguments, not bytes.

Examples:

```sh
chrome-cdp storage local list                        # {"scope":"local","origin":"https://app.example","items":[{"key":"theme","value":"dark"},{"key":"access_token","value":"<redacted>"}],"count":2,"truncated":false}
chrome-cdp storage local get theme                   # {"scope":"local","origin":"https://app.example","key":"theme","value":"dark","present":true}
chrome-cdp storage local set onboarding_done 1       # {"scope":"local","origin":"https://app.example","key":"onboarding_done","set":true}
chrome-cdp storage session rm draft                  # {"scope":"session","origin":"https://app.example","key":"draft","removed":true}
chrome-cdp storage local clear                       # {"scope":"local","origin":"https://app.example","cleared":true}
chrome-cdp storage local list --no-redact --max-value 0   # everything, verbatim
```

## Result envelope

`storage local list`:

```json
{ "ok": true, "command": "storage",
  "target": {"id":"…","title":"…","url":"https://app.example/home"},
  "result": { "scope": "local", "origin": "https://app.example",
              "items": [ {"key": "access_token", "value": "<redacted>"},
                         {"key": "persist:root", "value": "{\"auth\":\"<redacted>\",\"ui\":\"{\\\"theme\\\":\\\"dark\\\"}\"}"},
                         {"key": "theme", "value": "dark"},
                         {"key": "trace", "value": "…4096 bytes…", "truncated": true} ],
              "count": 4, "truncated": true },
  "elapsed_ms": 7 }
```

| Field | Present | Meaning |
|-------|---------|---------|
| `scope` | always | `local` \| `session`, echoed |
| `origin` | always | the top frame's security origin (`Page.getFrameTree` → `frameTree.frame.securityOrigin`), e.g. `https://app.example` or `http://127.0.0.1:8080`; the area that was read |
| `items` | always | one entry per key, **sorted by key** (byte order) so two listings diff cleanly; Chrome's own order is neither insertion nor alphabetical |
| `items[].key` | always | the key, never redacted or cut |
| `items[].value` | always | the value after redaction and then the size cap (in that order, see Design notes); `<redacted>` (`chrome.NetRedacted`) when withheld wholesale |
| `items[].truncated` | only when cut | `true`; the value was longer than `--max-value` bytes |
| `count` | always | `len(items)` — the number of keys in the area, not the number shown (every key is shown) |
| `truncated` | always | whether any value was cut; `false` under `--max-value 0` |

`origin`, not `url`: `targetAction` rewrites the envelope's `target.url` from any `result.url`, and the origin is not where the tab is.

`storage local get <key>`:

```json
{ "ok": true, "command": "storage",
  "target": {"id":"…","title":"…","url":"…"},
  "result": { "scope": "local", "origin": "https://app.example",
              "key": "theme", "value": "dark", "present": true },
  "elapsed_ms": 5 }
```

| Field | Present | Meaning |
|-------|---------|---------|
| `key` | always | the key asked for |
| `value` | always | the stored string, raw and uncut; `""` when `present` is `false` |
| `present` | always | whether the key exists — the `attr get` precedent (`{name, value, present}`), so `""` stored and "not there" stay distinguishable |

`storage local set|rm|clear`:

```json
{ "scope": "local", "origin": "https://app.example", "key": "onboarding_done", "set": true }
{ "scope": "local", "origin": "https://app.example", "key": "draft", "removed": true }
{ "scope": "local", "origin": "https://app.example", "cleared": true }
```

`set`, `removed` and `cleared` are always `true` on success; the failure case is an error envelope, never `set: false`.
`rm` does not report whether the key existed: Chrome's `DOMStorage.removeDOMStorageItem` of an absent key succeeds (measured), and `cookie rm` of an absent cookie reports `{deleted: name}` the same way; a pre-read to say `present` would double the round trips of the verb least likely to need it.
`clear` does not report a count for the same reason; `list` before it does.

Human mode goes through the generic `oneLine` path: `get` prints `value: dark` (the formatter's second preferred key), and the other four print the result map, exactly as `cookie` does today.
Human-mode rendering never alters the JSON shape.

## Errors and exit codes

| Situation | `error.code` | Exit | Extra fields |
|-----------|--------------|------|--------------|
| `storage` alone, `storage <bad-scope> …` (`storage lcoal list`) | `usage` | 2 | — ; the `storage` group command is runnable and emits this itself, before connecting (see Design notes) |
| `get`/`rm` with no or two args, `set` with one or three, `list`/`clear` with any | `usage` | 2 | — (cobra `ExactArgs` / `NoArgs`, refused before connecting) |
| `--max-value -1` | `usage` | 2 | — (validated in the CLI before `resolveTarget`) |
| the top frame has an opaque origin (`data:`, `about:blank`, a sandboxed document): `securityOrigin` is `"://"` | `target_not_found` | 4 | `opaque_origin: true` |
| Chrome refuses a `DOMStorage` command for any other reason | `cdp_error` | 5 | — (Chrome's message verbatim) |
| policy refuses the verb or the origin | `permission_denied` | 7 | unchanged (RFC-0012) |
| the tab cannot be resolved / the connection fails / the attach runs out the deadline | unchanged | unchanged | the existing codes from `resolveTarget` and `classifyActionErr` |
| `get` of an absent key | **not an error**: `ok: true`, `present: false` | 0 | — |
| `rm` of an absent key | **not an error**: `ok: true`, `removed: true` | 0 | — |

No new codes.
`target_not_found` for an opaque origin mirrors `chrome.IsNoHistory` → `target_not_found` with `no_history: true` in `classifyWithTabHint`: the invocation is well-formed and the tab is fine, but the thing asked about — a storage area — does not exist for this page, and an agent's right reaction is "do not retry", which is what exit 4 tells it.
Error detail fields are flattened onto the error object, as `occluded` and `no_history` are.

## Design notes

### Which storage area: the top frame's origin, obtained once from `Page.getFrameTree`

`DOMStorage` commands take a `StorageId{securityOrigin?, storageKey?, isLocalStorage}`.
Measured against headless Chrome 151 (chromedp v0.15.1, cdproto 2026-07-14) from a standalone probe:

- `Page.getFrameTree` on the tab's session returns the main frame with `securityOrigin` = `http://127.0.0.1:<port>` (scheme, host, port; no trailing slash, no path).
- `DOMStorage.getDOMStorageItems` with `{securityOrigin, isLocalStorage}` and with `{storageKey, isLocalStorage}` return the same items for both areas; passing both works too; passing neither is `Frame not found for the given storage id`.
- `Storage.getStorageKey` **without** `frameId` fails on a page target (`Target is not a supported worker type for storage inspection`); with the main frame's id it returns the origin plus a trailing slash (`http://127.0.0.1:<port>/`).
- `DOMStorage.enable` is not required for any of the four commands; it only turns on the change events, which this RFC does not use.
- `setDOMStorageItem` accepted an empty key and a 6 MiB value without error on this path, so the page-side quota is not enforced here; whatever Chrome does refuse arrives as `cdp_error`.
- On a `data:` page and on `about:blank`, `securityOrigin` is the literal `"://"`, `Storage.getStorageKey` fails with `Frame corresponds to an opaque origin and its storage key cannot be serialized`, `DOMStorage.*` with that origin fails with `Security origin cannot access local storage`, and page-side `localStorage` throws `SecurityError`.

Decision: one `Page.getFrameTree` call, whose `frameTree.frame.securityOrigin` is both the `StorageId.securityOrigin` sent to `DOMStorage` and the `origin` reported in the envelope.
`storageKey` is the documented alternative — `Storage.getStorageKey` with the main frame's id — and is not used because it costs a second round trip to obtain a value that, for a top-level first-party frame, is the origin with a slash appended; storage-key partitioning differs from the origin only for third-party iframes, which `--origin` would address and which is out of scope.
If a future Chrome stops accepting `securityOrigin` in `StorageId`, the fallback is that second call, and the envelope's `origin` field keeps its meaning because it is reported from the frame, not from the key.

The opaque-origin check is a **driver pre-check**, before any `DOMStorage` command: a `securityOrigin` equal to `"://"` or empty returns `chrome.ErrOpaqueOrigin` wrapped with the frame URL — `storage: the tab's top frame has an opaque origin (data:, about:blank and sandboxed documents have no web storage): <url>` — and `chrome.IsOpaqueOrigin(err)` is what the CLI classifies.
The pre-check exists so the message names the cause instead of Chrome's `Security origin cannot access local storage`, and so the answer is one round trip.

### `internal/chrome/storage.go`: five methods, one small options struct

The `Browser` interface gains, next to `Cookie*`:

```go
// StorageListOpts are the render-time options of StorageList. Like NetOpts,
// they shape what is REPORTED, never what is read: redaction and the cap
// are applied to Chrome's answer before it leaves the driver.
type StorageListOpts struct {
    NoRedact bool // report values verbatim (the explicit opt-out)
    MaxValue int  // cut each value at this many bytes; 0 = no cap
}

StorageList(ctx context.Context, targetID, scope string, opts StorageListOpts) (map[string]any, error)
StorageGet(ctx context.Context, targetID, scope, key string) (map[string]any, error)
StorageSet(ctx context.Context, targetID, scope, key, value string) (map[string]any, error)
StorageRemove(ctx context.Context, targetID, scope, key string) (map[string]any, error)
StorageClear(ctx context.Context, targetID, scope string) (map[string]any, error)
```

Five methods, not one `Storage(ctx, id, StorageOpts{Op: …})`: the cookie precedent is one method per subcommand with scalar key/value parameters (`CookieList/Set/Delete/Clear`), and so is `Attr*`; a discriminated single method would be the only one of its kind on the interface and would push the "which op" switch into every implementation, including the stub.
`StorageListOpts` is the one struct because `list` is the one subcommand with render options, and options travel as a struct on every recent method (`NetOpts`, `ConsoleOpts`, `WindowOpts`).
`scope` is the string `"local"` | `"session"`, validated in the CLI before the RPC; the driver maps it to `IsLocalStorage` and returns an error for anything else as a belt to the braces, never trusting the RPC's lenient decoding.

The implementation is `c.run(ctx, id, chromedp.ActionFunc(...))` exactly as `CookieList` is, with a shared `storageID(ctx, scope)` helper that does the `Page.getFrameTree` call, the opaque check and the `StorageId` construction:

- `StorageList`: `getDOMStorageItems`, then the pure `storageListResult(origin, scope, entries, opts)` builds the map (sort, redact, cut).
- `StorageGet`: `getDOMStorageItems` and a scan for the key — the protocol has no single-item read — returning `present: false` and `value: ""` when absent.
- `StorageSet` / `StorageRemove` / `StorageClear`: the one corresponding command, returning the fixed success shape.

`DOMStorage.getDOMStorageItems` returns `[]domstorage.Item`, each a two-element `[]string` (`[key, value]`); the builder indexes `[0]` and `[1]` and skips any malformed entry rather than panicking.

### Redaction: the `net` predicates, applied to keys and then to values, and before the cap

`list` withholds a value when the **key** is credential-shaped, and otherwise redacts credential-shaped **fields inside** the value, using only exported `net` functions and no new pattern:

```go
// storageRedact is the pure rule `list` applies to one entry.
func storageRedact(key, value string) string {
    if RedactedHeaderName(key) || RedactedParamName(key) {
        return NetRedacted
    }
    return RedactBody(value, false)
}
```

- `RedactedHeaderName` is the substring rule (`(?i)token|secret|password`, plus `authorization`, `cookie`, `set-cookie`, `x-api-key`, `proxy-authorization`): it catches `sb-abc-auth-token`, `msal.<id>.accesstoken`, `my_app_secret`, `password_hint`.
- `RedactedParamName` is the anchored rule (`access_token`, `refresh_token`, `id_token`, `api_key`, `jwt`, `auth`, `session`, `sid`, `sig`, `signature`, `key`, `code`, `credentials`, …): it catches the exact-name keys the substring rule misses (`auth`, `jwt`, `session`, `sid`).
- `RedactBody` handles the values that are JSON or form-encoded — which is what SPA state libraries store: a redux-persist `persist:root` of `{"auth":"{\"token\":…}","ui":…}` keeps `ui` and withholds `auth`; a Firebase `firebase:authUser:…` value keeps the profile and withholds `accessToken` / `refreshToken` / `idToken` (the `access_?token` pattern matches both spellings); a plain string value (`dark`, `1`, `2026-08-19`) passes through unchanged.

The union of the two name predicates over-redacts a key literally named `key` or `code`, exactly as `net` over-redacts a URL parameter so named; `--no-redact` lifts it, and inventing a storage-specific list would be a second rule set to drift from the first.

**Redaction runs before the size cap, and the order is load-bearing.**
`RedactBody`'s JSON rule (`netRedactJSONRe`) matches a complete `"name": "value"` member; a value cut mid-token no longer has its closing quote, the member does not match, and the token's prefix is reported in clear.
So `storageListResult` redacts the whole value, then `eventbuf.TruncateText(v, opts.MaxValue)` cuts the redacted string rune-safely, then sets `truncated` from the second return.
`TestStorageListRedactsBeforeTruncating` pins it with a JSON value whose `access_token` member straddles the cap.

`get` applies neither rule, by decision: the caller named the key.

### `--max-value`: 4096 bytes, `0` is no cap

`DefaultStorageMaxValue = 4 << 10`, exported from `internal/chrome` so the CLI's flag default and the driver's constant are one number.
A JWT with a fat claim set is one to two kilobytes and survives intact; an analytics queue, a redux-persist tree or a cached API response is cut, which is the point of `list`.
`0` means no cap because `eventbuf.TruncateText` already treats a non-positive max as "do not cut", so the CLI passes the flag's value straight through; a negative flag value is refused as `usage` before connecting so that "no cap" has exactly one spelling.
The cap is in bytes of the UTF-8 string, as `net_max_body` is, and the cut never splits a rune.

### The CLI: `internal/cli/storage.go`

`cmdStorage()` returns a `storage` group with two scope groups, `local` and `session`, each built by one helper `storageScope(scope string) *cobra.Command` that adds the five leaf commands, so the two subtrees cannot drift:

```go
func (a *App) cmdStorage() *cobra.Command {
    storage := &cobra.Command{
        Use: "storage", Short: "Read and write the tab's localStorage / sessionStorage",
        Args: cobra.ArbitraryArgs,
        RunE: func(_ *cobra.Command, args []string) error {
            a.emitErr("storage", result.CodeUsage, "storage needs a scope (local|session) and an action (list|get|set|rm|clear)", nil)
            return nil
        },
    }
    storage.AddCommand(a.storageScope("local"), a.storageScope("session"))
    return storage
}
```

The group command is **runnable** so that a wrong scope is `usage` / exit 2.
Cobra's `Find` resolves `storage lcoal list` to `storage` with the leftover args, and a non-runnable group there prints help to stdout and exits 0 — which is what `cookie foo` does today, and which breaks the one-envelope and exit-code contracts for an agent at the level where a typo is likeliest.
The price is one policy row, `"storage": Exempt` (the command never reaches Chrome; `TestEveryCommandIsClassified` walks every runnable command, group or leaf), with a comment saying why it exists.
`storage --help` still prints help, because cobra handles `--help` before `RunE`.
A wrong **action** (`storage local lsit`) is the unchanged repo-wide unknown-subcommand behaviour and is named in Out of scope.

Inside each scope, the five leaves use `a.targetAction("storage", …)` like `cookie`'s do, with one difference on `list`: its `--max-value` is validated in the `RunE` **before** `targetAction` resolves the target, via a small `storageListAction` wrapper, so a negative cap never connects (`noCall` proves it).
The flags `--no-redact` and `--max-value` are local to `list` and live on `App` as fields (`a.storageNoRedact`, `a.storageMaxValue`), re-registered per `Execute` via `newRoot`; the two scope trees share the two fields because only one leaf runs per invocation.
The envelope's `command` is `"storage"` for every leaf, as `cookie`, `record` and `window` report their family name.
The `classifyWithTabHint` chain gains one branch, before the `classifyActionErr` fallthrough and beside `IsNoHistory`:

```go
if chrome.IsOpaqueOrigin(err) {
    return result.CodeTargetNotFound, err.Error(), map[string]any{"opaque_origin": true}
}
```

Inside `session` the ten leaves are ordinary argv lines; nothing to refuse.

### The daemon

`remoteBrowser` gains five forwarders — `r.c.call(ctx, "StorageList", &out, id, scope, opts)`, `…"StorageGet", &out, id, scope, key`, `…"StorageSet", &out, id, scope, key, value`, `…"StorageRemove", &out, id, scope, key`, `…"StorageClear", &out, id, scope` — and `dispatch` five cases, using `argStr` for every scalar and one new decoder `argStorageList(a, i) chrome.StorageListOpts` for the struct.
`TestDispatchCoversBrowser` stays green with the five cases and fails without them.
The calls are ordinary unary calls under `s.mu`; nothing here blocks the renderer.

### Policy, MCP, docs, skill

- **Policy** (`internal/policy/policy.go`, same commit as the verb): eleven rows.
  `"storage local list"`, `"storage local get"`, `"storage session list"`, `"storage session get"` are Reading; `"storage local set"`, `"storage local rm"`, `"storage local clear"`, `"storage session set"`, `"storage session rm"`, `"storage session clear"` are Mutating; `"storage"` is Exempt with the comment above.
  Keys are the full cobra path minus the root (`a.verbPath = strings.TrimPrefix(cmd.CommandPath(), root+" ")`), so a three-word key needs no classifier change; `TestEveryCommandIsClassified` enforces the rows.
  `clear` is Mutating and irreversible and takes no confirmation flag: the policy layer is where an operator bounds it (`read_only`, `verbs_denied = ["storage local clear"]`), exactly as for `cookie clear`.
  The policy section of `docs/cli-reference.md` lists `storage set/rm/clear` under Acting and `storage list/get` under Reading.
- **MCP** (`internal/mcp/tools.go`): **one new tool, `chrome_cdp_storage`, behind `--tools full` only** (`full: true`).
  The default set is at RFC-0004's cap of 18 and the "≤ 18" comment stays true; the verb does not fold into an existing tool because no default tool reads page-held credential-shaped state (`read` is read-only by construction and reads only DOM/text content; `tabs` folds in tab lifecycle plus dialog inspection, neither of which touches a session token), and because reading the area that holds the session token is the kind of capability a user opts into — the same reasoning that keeps `raw_cdp`, the one tool already gated by `full: true`, out of the default set.
  `cookie` is not exposed over MCP at all; `storage` is, because an MCP agent driving an SPA needs the feature-flag read and the clear far more often than it needs cookies.
  Shape: `disc: "action"` with `actions: {"list": "storage local list", "get": "storage local get", "set": "storage local set", "rm": "storage local rm", "clear": "storage local clear"}`, plus a required `scope` argument (enum `local`, `session`, no flag — it is a path segment), `key` and `value` (positional; required-ness enforced in `build`, never in the schema, per the RFC-0014 lesson), `no_redact` (flag `no-redact`) and `max_value` (flag `max-value`, `c.num`).
  `build` returns the verb `"storage " + scope + " " + action` with the positionals after `--`, so the per-call `refusedByReadOnly` check at dispatch sees the real verb; the `actions` map names the `local` verb as the representative for schema narrowing, which is honest because each action has the same class in both scopes — `TestStorageActionsShareClassAcrossScopes` asserts `Classify("storage local X") == Classify("storage session X")` for all five.
  `verbs` lists all ten leaf verbs, so `TestMCPToolArgumentsMirrorCLIFlags` finds `--no-redact` and `--max-value` on `storage local list` and every verb classified.
  Under `--read-only`, `allowedActions` keeps `list` and `get` and drops the other three with no further code.
- **Docs**: a row in the Browser state table of `docs/cli-reference.md` (`storage local|session list|get|set|rm|clear`) and a `### Web storage` subsection after it — the five subcommands, the two `list` flags, the envelope fields, the redaction rule in one paragraph that cross-references the `net` paragraph, the exit codes including `opaque_origin`, and one sentence on `clear` and policy.
- **Skill** (`skills/drive-chrome-cdp/references/core.md`): the verbs line gains `storage local|session list|get|set|rm|clear` beside `cookie …`, and one sentence: "`storage local list` shows what the app persisted (flags, drafts; tokens redacted unless `--no-redact`); `storage local clear` plus `nav --reload` is 'clear site data'."

### Stub

`chrometest.StubBrowser` gains permissive defaults like every other method: `StorageList` returns `{"scope": scope, "origin": "https://stub.test", "items": [], "count": 0, "truncated": false}`; `StorageGet` returns `{"scope": scope, "origin": "https://stub.test", "key": key, "value": "stub", "present": true}`; `StorageSet` / `StorageRemove` / `StorageClear` return the success shapes with the `scope`, `origin` and (where applicable) `key` echoed.
A test that wants `present: false` or an opaque origin overrides the one method.

## Verification scenarios

**VS-1 — `list` reads both areas of the top frame, sorted, redacted, counted.**
Given an `httptest` page whose script ran `localStorage.setItem('theme','dark'); localStorage.setItem('access_token','SECRET1'); sessionStorage.setItem('draft','hello')`
When `StorageList(local, {})` and `StorageList(session, {})` run
Then the local result has `origin` equal to the server's origin, `items` `[{access_token, <redacted>}, {theme, dark}]` in that order, `count: 2`, `truncated: false`; the session result has `[{draft, hello}]`, `count: 1`; neither contains `SECRET1`.

**VS-2 — `--no-redact` reports the token.**
Given VS-1
When `StorageList(local, {NoRedact: true})` runs
Then `access_token`'s value is `SECRET1`.

**VS-3 — `get` is raw, uncut, and says when the key is absent.**
Given VS-1 plus `localStorage.setItem('blob', <100 000 bytes>)`
When `StorageGet(local, "access_token")`, `StorageGet(local, "blob")` and `StorageGet(local, "nope")` run
Then the results are `{value: "SECRET1", present: true}`, a 100 000-byte value with `present: true` and no `truncated` key, and `{value: "", present: false}`.

**VS-4 — `set` is visible to the page, `rm` removes, `clear` empties, scopes are independent.**
Given VS-1
When `StorageSet(local, "flag", "1")`, then `StorageRemove(local, "theme")`, then `StorageClear(session)` run
Then `eval "localStorage.getItem('flag')"` is `"1"`, `eval "localStorage.getItem('theme')"` is `null`, `eval "sessionStorage.length"` is `0`, `eval "localStorage.length"` is `2` (`access_token`, `flag`), and the results carry `set: true`, `removed: true`, `cleared: true`.

**VS-5 — `rm` of an absent key succeeds.**
Given VS-1
When `StorageRemove(local, "never")` runs
Then it returns `removed: true` and no error.

**VS-6 — The cap cuts, marks, and never splits a rune.**
Given a value of 5000 bytes ending in multi-byte runes
When `StorageList(local, {MaxValue: 4096})` runs
Then that item's value is at most 4096 bytes, valid UTF-8, `truncated: true` on the item and `truncated: true` at the top; with `{MaxValue: 0}` the value is whole and both flags are absent / `false`.

**VS-7 — Redaction before truncation.**
Given `localStorage.setItem('state', '{"ui":"<4000 bytes>","access_token":"SECRETLONG"}')` so the `access_token` member straddles the 4096-byte cap
When `StorageList(local, {MaxValue: 4096})` runs
Then the reported value contains `"access_token":"<redacted>"` — or is cut before the member begins — and never a prefix of `SECRETLONG`.
*(Pure: the same assertion over `storageListResult` with a constructed entry.)*

**VS-8 — Opaque origin is refused before any `DOMStorage` call.**
Given a tab on `data:text/html,<title>d</title>` (and separately `about:blank`)
When `StorageList(local, {})` runs
Then it returns an error for which `IsOpaqueOrigin` is true, whose message contains `opaque origin` and the page URL.

**VS-9 — The pure redaction rule.**
Given the table: (`theme`, `dark`) → `dark`; (`access_token`, `x`) → `<redacted>`; (`sb-abc-auth-token`, `x`) → `<redacted>`; (`jwt`, `x`) → `<redacted>`; (`session`, `x`) → `<redacted>`; (`persist:root`, `{"auth":"{\"token\":\"t\"}","ui":"dark"}`) → `{"auth":"<redacted>","ui":"dark"}`; (`firebase:authUser:k`, `{"uid":"u","stsTokenManager":{"accessToken":"a","refreshToken":"r"}}`) → both tokens `<redacted>`, `uid` kept; (`draft`, `a=1&token=t`) → `a=1&token=<redacted>`; (`note`, `plain text with token inside`) → unchanged
When `storageRedact(key, value)` runs
Then the outputs match.

**VS-10 — The CLI envelope, `list` and `get`.**
Given a stub whose `StorageList` returns two items and whose `StorageGet` returns `present: false`
When `storage local list --json` and `storage session get nope --json` run
Then the envelopes have `command: "storage"`, `ok: true`, the result maps as returned, and the stub received `scope` `"local"` then `"session"`.

**VS-11 — The CLI forwards flags and positionals.**
Given a recording stub
When `storage local list --no-redact --max-value 0 --json` and `storage session set k v --json` run
Then the stub saw `StorageListOpts{NoRedact: true, MaxValue: 0}` and `StorageSet("session", "k", "v")`; with no flags it saw `MaxValue: 4096`.

**VS-12 — Validation never connects.**
Given `noCall(t)`
When `storage`, `storage lcoal list`, `storage local get`, `storage local set k`, `storage local set k v w`, `storage local rm`, `storage local list extra`, `storage local clear x`, `storage local list --max-value -1` run, each with `--json`
Then every exit is 2 with `error.code: "usage"`, and the browser is never contacted.

**VS-13 — Opaque origin through the CLI.**
Given a stub whose `StorageList` returns `ErrOpaqueOrigin`
When `storage local list --json` runs
Then exit 4, `error.code: "target_not_found"`, `opaque_origin: true`.

**VS-14 — `session` batches the verb.**
Given a `session` reading `storage local set k v` then `storage local get k`
When it runs
Then two envelopes, one per line, both `command: "storage"`.

**VS-15 — The daemon forwards the struct.**
Given the daemon test harness with a recording browser
When `remoteBrowser.StorageList(ctx, id, "session", StorageListOpts{NoRedact: true, MaxValue: 12})` runs
Then the browser saw the same scope and options; `TestDispatchCoversBrowser` passes.

**VS-16 — MCP exposure.**
Given a server with default tools, then `--tools full`, then `--tools full --read-only`
When `tools/list` runs
Then `chrome_cdp_storage` is absent, present, and present with `action` narrowed to `list`, `get`; a call with `action: "set", scope: "session", key: "k", value: "v"` builds argv `storage session set -- k v`; a call with `action: "get"` and no `key` is `usage` and runs nothing.

**VS-17 — Policy rows and class symmetry.**
`TestEveryCommandIsClassified` passes with the eleven rows; `TestStorageActionsShareClassAcrossScopes` passes; `read_only` on the origin refuses `storage local set` with `permission_denied` and allows `storage local list` (`TestReadOnlyOriginAtTheBoundary` gains the two cases).

## Test plan

**Pure (`t.Parallel()`), `internal/chrome/storage_test.go`.**
`TestStorageRedact` (VS-9, table-driven), `TestStorageListResultSortsCountsAndCaps` (VS-6 over `storageListResult` with constructed `[]domstorage.Item`, including a malformed one-element item that is skipped), `TestStorageListRedactsBeforeTruncating` (VS-7), `TestStorageScopeIsValidated` (the driver's `"local"` / `"session"` → `IsLocalStorage` mapping and the error for anything else).

**Stub-backed, `internal/cli/storage_test.go`.**
`TestStorageListAndGetEnvelope` (VS-10), `TestStorageFlagsAndPositionalsReachTheBrowser` (VS-11), `TestStorageValidationNeverConnects` (VS-12, `noCall(t)`, table-driven over the nine invocations), `TestStorageOpaqueOriginIsTargetNotFound` (VS-13), `TestStorageInsideSession` (VS-14).

**Daemon, `internal/daemon/daemon_test.go`.**
`TestStorageListOptsCrossTheRPC` (VS-15); `TestDispatchCoversBrowser` unchanged.

**MCP, `internal/mcp/server_test.go` / `tools_test.go`.**
`TestStorageToolIsFullOnly`, `TestReadOnlyKeepsStorageListAndGet`, `TestStorageToolBuildsScopedVerb`, `TestStorageActionsShareClassAcrossScopes` (VS-16); one row added to `TestArgvMirrorsTheCLISpelling`.

**Policy, `internal/cli/policy_test.go`.**
`TestEveryCommandIsClassified` unchanged; two cases added to the read-only origin test (VS-17).

**Live Chrome (`internal/chrome/storage_test.go`, `testing.Short()`-guarded, not parallel, `liveCDP(t)` plus an `httptest` fixture in the `captureFixture` style).**
`TestStorageRoundTrip` (VS-1 to VS-5 on one fixture, reading the page's view back with `b.Eval`), `TestStorageCapLive` (VS-6 with a real 5000-byte value), `TestStorageOpaqueOriginIsRefused` (VS-8, on `data:` and `about:blank` tabs).
The fixture **must** be an `http://127.0.0.1` page: web storage is disabled inside `data:` URLs (`SecurityError: Storage is disabled inside 'data:' URLs`, measured), so the `data:` fixtures the rest of the live suite uses cannot hold a storage area — they are instead the fixture for VS-8.
`httptest` gives every test its own port and therefore its own origin, so areas never collide across tests and no cleanup is needed.

## Out of scope

- `--origin <o>` / `--frame`: reading or writing another frame's storage partition; the top frame is the contract, and `storageKey` partitioning only differs from the origin for third-party iframes.
- Redaction or a cap on `get`; the caller named the key.
- Reporting whether `rm` removed anything, or how many keys `clear` removed.
- Watching for changes (`DOMStorage.domStorageItemUpdated` events); `storage … list` in a poll, or `wait`, covers it.
- IndexedDB, Cache Storage, cookies (the `cookie` verb), and "clear all site data" as one verb (`cookie clear` + `storage local clear` + `storage session clear` in a `session` is three lines).
- Exposing `storage` in the default MCP tool set; `--tools full` or `--tools storage` names it.
- Changing how a mistyped **action** under a group is reported: `storage local lsit` prints help and exits 0 like `cookie lsit` and `window foo`; a repo-wide fix for non-runnable groups is a separate change, and this RFC only closes the hole at the scope level it introduces.
- Redacting or masking `key` names; a key is not a secret.

## Open questions

None at proposal time.
Two implementation notes, recorded here after the fact rather than as silent edits to the proposal above:

- The MCP rationale originally cited `record` as the precedent for keeping a capability out of the default tool set behind `full: true`.
  `record` is not exposed as an MCP tool at all — it never appears in `registry()` (internal/mcp/tools.go) — so that citation was fabricated rather than checked against the code.
  The real precedent is `raw_cdp`, the only tool `full: true` gated before this RFC.
  The bullet above is corrected to cite it.
- The MCP rationale also said `tabs` is "tab lifecycle" as part of why `storage` does not fold into it.
  RFC-0018 (implemented on this branch ahead of this one) folded `dialog_status`/`dialog_accept`/`dialog_dismiss` into the default `tabs` tool, so `tabs` is no longer pure lifecycle by the time this RFC ships.
  The bullet above is softened to name what actually distinguishes `storage`: no default tool — `tabs` included — reads page-held credential-shaped state, which is the property that matters here.
