# Implementing an RFC

The design proposals live in [`docs/rfc/`](../../docs/rfc/).
This file is the *how*: the conventions an implementing PR must satisfy, and the traps specific to this codebase.
Read the RFC first, then this.

## The shape of an RFC PR

Every RFC that adds a browser capability touches the same six places, in this order:

1. **`internal/chrome/browser.go`** — add the method to the `Browser` interface, with the option struct it takes.
2. **`internal/chrometest/stub.go`** — add a permissive default.
   This is the single place a new method gets its test default; skipping it breaks every existing test's compile.
3. **`internal/chrome/<feature>.go`** — the CDP implementation.
4. **`internal/daemon/daemon.go`** — **both** halves of the RPC: a `remoteBrowser` forwarder method *and* a `dispatch` case.
   A new option struct also needs an `argXxx` decoder.
5. **`internal/cli/<feature>.go`** — the cobra command, registered in `newRoot`'s `AddCommand` list in `commands.go`.
6. **`docs/cli-reference.md`** — the verb, its flags, and any new exit code.
   Same PR, never a follow-up.

New verbs get their own file in `internal/cli/` rather than growing `commands.go`, which is already large.

### Step 4 is the one that gets forgotten

The daemon is the **default** connection path — it holds the connection so Chrome's consent prompt appears once per session rather than once per command.
`remoteBrowser` implements `chrome.Browser` by RPC, so a method that exists on `*CDP` but has no forwarder and no `dispatch` case **compiles fine and fails only at runtime, only under the daemon** — which is to say, in normal use but not in any stub-backed test.

`TestDispatchCoversBrowser` in `internal/daemon` reflects over the `chrome.Browser` interface and fails when a method has no dispatch case.
If it fails, you skipped step 4.
Do not special-case the method to silence it.

Arg decoders return the zero value for a missing or malformed argument, which is safe *only* because both ends of the protocol are the same binary.
Do not rely on that leniency for anything a user can influence — validate in the CLI, before the RPC.

## Rules that are not negotiable

### Validate before connecting

Argument and flag validation runs **before** `resolveTarget` / `getBrowser`, so a malformed invocation is `usage` / exit 2 without ever touching Chrome.
This is a contract, not a nicety: agents rely on exit 2 meaning "your call was wrong, do not retry".

Enforce it with a test, not a comment — a stub whose methods call `t.Fatal` proves no connection was attempted:

```go
type noCallBrowser struct {
    chrometest.StubBrowser
    t *testing.T
}

func (b noCallBrowser) List(context.Context) ([]target.Info, error) {
    b.t.Fatal("browser was contacted for an invocation that should have failed validation")
    return nil, nil
}
```

### The envelope is public API

A new failure mode needs a `Code*` constant **and** a `codeToExit` entry in `internal/result/result.go`, **and** a row in `ExitCodes()`.
Miss the map entry and the code silently degrades to `ExitGeneric` — the exact bug `TestExitCodeFor` exists to catch.

Human-mode formatting changes must never alter the JSON shape.

### Parsing belongs in a pure function

Keyspecs, status specs, region rectangles, paper sizes, policy patterns — every small grammar is a pure function with a table-driven test and no browser.
Put it wherever it is testable without Chrome; `internal/chrome` is fine for types the interface needs, as long as the parser itself takes no context.

### Reuse `QueryOpts`, don't invent addressing

Any verb that addresses an element takes `QueryOpts` and therefore gets `--by`, `--wait`, `--role`, `--nth`, `--match`, `--in-row`, `--pierce` for free.
A verb with its own selector flags is a design error.

### Every verb must work inside `session`

`session` re-enters the whole command tree per NDJSON line against a cached connection, so an ordinary argv verb works automatically.
A verb that streams (`--follow`) breaks the one-envelope-per-line contract and must be a `usage` error inside `session`.

## Codebase traps

**`Browser.Close() error` already exists** and means "tear down the connection".
The tab-closing verb from RFC-0007 is therefore `CloseTabs`, not `Close`.

**Contexts are rooted at `context.Background()`, deliberately.**
See the comment on the `CDP` struct: the allocator, base, and tab contexts must not be parented on a per-command context, or a command's deferred cancel would `CloseTarget` the user's real tab.
Per-command deadlines are applied as short-lived children in `run`.
Never "fix" this by threading the command context through.

**Pointer geometry is already solved — reuse it.**
`select.go` has `resolveNodeReady` (PRESENT-not-visible wait, works on a hidden tab), `nodeCoord` (clamped centre plus an occlusion hit-test), and the settle loop inside `coordClickNodeN`.
New pointer verbs factor the settle loop into a coordinate resolver and share it.
Do not recompute geometry with `dom.GetBoxModel` — it is wrong on hidden tabs, which is why this code exists.

**The accessibility tree is throttled on a backgrounded tab.**
`--by name` / `ref` / `cell` stall there, which `classifyWithTabHint` surfaces as `tab_hidden: true`.
Anything reading the a11y tree inherits this.
Best-effort enrichment (a `focused` field, say) must degrade to omitted rather than blocking.

**Flags live on `App` as fields**, re-registered per `Execute` via `newRoot`, so they reset between `session` lines.
Do not cache flag-derived state on `App` across invocations.

## Tests

Follow [the test-writing guidelines](test-writing-guidelines.md).
The tiers, in the order they should exist:

| Tier | Where | Runs under `-short` | What belongs here |
|------|-------|--------------------|-------------------|
| Pure | anywhere, `t.Parallel()` | yes | grammars, buffers, policy decisions, encoders |
| Stub-backed | `internal/cli` | yes | envelope shape, exit codes, validation-before-connect |
| Live Chrome | `internal/chrome` | **no** (`testing.Short()` guard) | anything needing a real renderer |

Live tests are not parallel — they share the spawned browser — and must skip gracefully when no Chrome binary exists.

Both CI legs run them: `macos-latest` ships Chrome too, and being slower and more contended it is where a timing-sensitive live test fails first.
A macOS-only failure is a real race, not flake.

Drive live tests from `data:` fixture pages that record events into `window.__log`, then read them back with `eval`.
Assert on structure and sampled values, never on byte-for-byte image equality or exact frame counts.

Write the regression guard for existing behaviour **before** changing a signature.

## Before pushing

```sh
gofmt -l .          # must be empty
go vet ./...
go test -race ./...
go test -short ./... # the no-Chrome path (both CI legs also run the live suite)
```

CI is exactly these four steps on ubuntu + macOS; there is no Makefile and no golangci config.

Markdown is **one sentence per line**.
Branches get pushed; PRs are the user's to open unless they asked otherwise.
