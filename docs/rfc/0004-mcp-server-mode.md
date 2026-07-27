# RFC-0004: MCP server mode — `chrome-cdp mcp`

- **Status:** Accepted — implemented in [#15](https://github.com/sanketsudake/chrome-cdp-cli/pull/15), with follow-up fixes in [#18](https://github.com/sanketsudake/chrome-cdp-cli/pull/18)
- **Priority:** P0
- **Area:** distribution
- **Depends on:** RFC-0012 (should land with or before this)
- **Benefits from:** RFC-0001, RFC-0005, RFC-0006, RFC-0007 landing first

## Summary

Add a `chrome-cdp mcp` subcommand that runs the CLI as a Model Context Protocol server over stdio, exposing the existing verbs as MCP tools backed by the same `chrome.Browser` seam and the same result envelope.

## Motivation

`chrome-cdp` is adoptable today by two audiences: humans at a shell, and anything that can spawn a subprocess and parse NDJSON.
That second group is smaller than it looks.
Most AI tooling — editors, desktop assistants, agent frameworks — integrates capabilities through MCP, not through subprocess conventions.
A tool that is not an MCP server has to be wrapped by every user who wants it, and most will not bother.

The cost of closing this is unusually low here, because the hard parts already exist:

- `session` already runs many commands over one held connection with one envelope per command.
  That is structurally an RPC loop; MCP is a different frame around the same loop.
- The envelope is already a stable, machine-first JSON contract with typed error codes.
  MCP tool results want exactly that.
- The `chrome.Browser` seam already isolates the command boundary from the transport, so a second front end costs no duplication of browser logic.
- `chrometest.StubBrowser` already makes the whole surface testable without a browser, so the MCP layer is testable end-to-end in unit tests.

This is the single largest adoption lever in the RFC set: it does not add a capability, it multiplies the number of places every existing capability can be used.

The ordering note matters.
Shipping MCP before the input gaps (RFC-0001, 0005, 0006) are closed exposes a tool surface that cannot press Escape or attach a file, and first impressions of an integration are hard to revise.

## User stories

**US-1 — Use it from an MCP client without writing glue.**
As a developer, I want to add `chrome-cdp` to my MCP client config so that I can drive my logged-in browser from my assistant without writing a wrapper.
*Acceptance:* adding one stdio server entry with command `chrome-cdp` and args `["mcp"]` exposes the tools, with no other setup beyond the one-time Chrome debugging toggle.

**US-2 — Get the same contract in both front ends.**
As a user of both the CLI and MCP, I want identical semantics so that a recipe I debug at the shell behaves the same when an agent runs it.
*Acceptance:* for every exposed verb, the MCP tool result payload is the same envelope `result` object the CLI emits, and errors carry the same `code`.

**US-3 — Discover what the tool can do.**
As an agent, I want tool descriptions and JSON schemas that explain addressing so that I use `--by name` on a dynamic app instead of guessing CSS selectors.
*Acceptance:* each tool's schema documents `by`, `role`, `match`, `nth`, `in_row`, and the description states when to prefer accessible-name addressing.

**US-4 — Not be flooded by a huge tool list.**
As an agent with a limited context budget, I want a compact, well-chosen tool set so that the browser server does not crowd out everything else.
*Acceptance:* the default surface is a bounded set of tools (target: ≤ 18), with rarely-used verbs grouped rather than each getting its own tool.

**US-5 — Bound what an agent may drive.**
As a security-conscious user, I want the MCP server to honour a domain allow-list so that handing an assistant my authenticated browser does not mean handing it every site I am logged into.
*Acceptance:* with an allow-list configured, a tool call targeting a non-allowed origin is refused with a typed error and no action is performed.

**US-6 — Run it read-only.**
As a cautious first-time user, I want a mode that exposes only reading verbs so that I can evaluate the tool without it being able to click anything.
*Acceptance:* `chrome-cdp mcp --read-only` exposes list/snap/text/html/value/grid/screenshot/console/net and nothing that mutates page state.

**US-7 — Diagnose a broken integration.**
As a user whose client shows the server as failed, I want a way to check the server outside the client so that I can tell an MCP problem from a Chrome problem.
*Acceptance:* `chrome-cdp doctor` covers MCP readiness, and the server logs diagnostics to stderr (never stdout, which carries the protocol).

## Proposed CLI surface

```
chrome-cdp mcp [--read-only] [--tools <set>] [--target <spec>]
```

| Flag | Default | Purpose |
|------|---------|---------|
| `--read-only` | off | expose only non-mutating tools |
| `--tools <set>` | `default` | `default` \| `full` \| a comma-separated allow-list of tool names |
| `--target <spec>` | — | pin the server to one tab; otherwise tools take a `target` argument |

Global flags (`--timeout`, `--no-daemon`, `--port`, `--profile-dir`, config file) apply unchanged.

Client config (illustrative):

```json
{ "mcpServers": { "chrome-cdp": { "command": "chrome-cdp", "args": ["mcp"] } } }
```

## Tool surface

Grouped deliberately, to hold US-4.
Each tool's arguments mirror the CLI flags in `snake_case`.

| Tool | Wraps | Notes |
|------|-------|-------|
| `tabs` | `list`, `open`, `use`, `close`, `activate` | one tool, `action` argument |
| `navigate` | `nav`, back/forward/reload | |
| `snapshot` | `snap` | the primary read; documents `role`/`grep`/`region`/`dedupe` |
| `read` | `text`, `html`, `value`, `grid` | one tool, `kind` argument |
| `click` | `click` | full `QueryOpts` |
| `type_text` | `type`, `fill` | `replace` boolean picks the verb |
| `key` | `key` (RFC-0001) | |
| `pointer` | `hover`, `dblclick`, `rclick`, `drag` (RFC-0005) | one tool, `action` argument |
| `select_option` | `select` | cascade path support |
| `scroll` | `scroll` | |
| `upload` | `upload` (RFC-0006) | path allow-listing applies |
| `wait_for` | `wait` | all conditions incl. `--request` |
| `screenshot` | `screenshot` (RFC-0008) | returns an image content block |
| `console` | `console` (RFC-0002) | |
| `network` | `net` (RFC-0003) | |
| `evaluate` | `eval` | flagged as powerful in its description |
| `batch` | `session` | run several of the above over one connection |
| `raw_cdp` | `raw` | `full` tool set only; not in `default` |

`batch` is worth calling out: it lets a client collapse a five-step interaction into one round trip, which is the same efficiency argument that makes `session` valuable at the shell.

## Result mapping

An MCP tool result carries the envelope's `result` object as structured content, plus a short human-readable text summary.

Success:

```json
{ "content": [{"type":"text","text":"clicked \"Save and Close\" (button)"}],
  "structuredContent": { "clicked": true, "waited_text": "Saved" },
  "isError": false }
```

Failure preserves the typed contract rather than flattening to prose:

```json
{ "content": [{"type":"text","text":"target_timeout: no element matching name \"Save\" after 30s"}],
  "structuredContent": { "code": "target_timeout", "exit": 4, "message": "…", "details": {"tab_hidden": true} },
  "isError": true }
```

Carrying `code` and `exit` through means an agent can branch on the same contract a shell script does, including recoverable signals like `tab_hidden`.
`screenshot` additionally returns an `image` content block.

## Design notes

- **The MCP layer is a front end, not a fork.**
  It sits beside `internal/cli`, calling the same command implementations against the same `chrome.Browser`.
  Any verb added by another RFC becomes available by registering it, with no browser-side work.
  If this ends up duplicating command logic, the design is wrong — refactor the shared part into a callable command registry that both front ends consume.
- **stdout is protocol; stderr is diagnostics.**
  Every existing human-facing write must be audited for this.
  A stray `fmt.Println` corrupts the stream and manifests as an unexplained client failure — a specific, likely, and hard-to-debug regression that deserves its own test.
- **The daemon still holds the connection.**
  MCP mode does not bypass it; a long-lived server plus the shared daemon is exactly the case the daemon was built for.
- **Tool descriptions are part of the product.**
  The addressing model (`--by name` over CSS on dynamic apps) is the CLI's main advantage on real applications, and an agent only benefits if the schema says so.
  Descriptions should be reviewed as carefully as the code.
- **Transport:** stdio only.
  HTTP/SSE is deliberately out of scope — a network-reachable server that drives the user's authenticated browser is a different security posture and needs its own RFC.
- **Library choice:** prefer an established Go MCP SDK over a hand-rolled implementation; the protocol's lifecycle and content-block details are not where this project should spend its complexity budget.
  Vendoring decision belongs in review.

## Verification scenarios

**VS-1 — Handshake** Given the server started with `mcp` When a client performs `initialize` Then the server responds with protocol version and capabilities, and nothing non-protocol has been written to stdout.

**VS-2 — Tool listing is bounded and schema-valid** When `tools/list` is called Then every tool has a name, description, and a valid JSON Schema, and the `default` set contains ≤ 18 tools.

**VS-3 — Success maps to the envelope result** Given a stub browser returning `{"clicked": true}` When the `click` tool is invoked Then `isError` is false and `structuredContent` equals that object.

**VS-4 — Failure preserves code and exit** Given a stub returning a target-timeout error When `click` is invoked Then `isError` is true and `structuredContent.code == "target_timeout"` with `exit == 4`.

**VS-5 — Usage errors do not reach the browser** Given a tool call with an unknown `by` value When it is invoked Then the result is an error with `code == "usage"` and the stub records no browser calls.

**VS-6 — stdout purity** Given a sequence of tool calls including ones whose CLI equivalents print human output When they are invoked in MCP mode Then every byte on stdout parses as protocol framing and diagnostics appear on stderr.

**VS-7 — `--read-only` hides mutating tools** When the server starts with `--read-only` and `tools/list` is called Then `click`, `type_text`, `key`, `pointer`, `select_option`, `upload`, and `evaluate` are absent, and invoking one by name returns a `usage` error.

**VS-8 — `--tools` allow-list** When started with `--tools snapshot,read` Then exactly those two are listed and no others are invocable.

**VS-9 — Allow-list enforcement (with RFC-0012)** Given an allow-list of `*.example.com` and a tab on another origin When `click` is invoked Then the call is refused with the typed policy error and the stub records no click.

**VS-10 — `batch` runs several verbs over one connection** Given a `batch` call containing three verbs When it is invoked Then three results are returned in order, the connection is acquired once, and a mid-batch failure stops the remainder with the failing index reported.

**VS-11 — Screenshot returns an image block** When `screenshot` is invoked Then the result contains an `image` content block with a base64 PNG payload.

**VS-12 — Cancellation** Given a long-running `wait_for` When the client cancels the request Then the underlying context is cancelled and no goroutine remains — verified with `-race` and a leak check.

## Test plan

**Protocol unit tests (`internal/mcp`, `chrometest.StubBrowser`, `t.Parallel()`)** Drive the server over an in-memory pipe rather than a real subprocess: initialize, `tools/list`, `tools/call`.
Covers VS-1 through VS-5, VS-7, VS-8, VS-10, VS-11.
This is the bulk of the suite and needs no Chrome, so it runs under `go test -short`.

**stdout purity test (VS-6)** Capture stdout as bytes during a scripted session and assert every frame parses; assert stderr is non-empty for a verbose run.
Worth a dedicated test because the failure is silent and the symptom is far from the cause.

**Schema conformance test (VS-2)** Validate each tool's schema against JSON Schema, and assert the argument names match the CLI flag names they wrap — a table keyed by tool name, so a renamed flag that is not mirrored fails here rather than in a user's client.

**Parity test — the important one** Table-driven over a representative verb set: run each through the CLI front end and the MCP front end against the same stub, and assert the payloads are equal.
This is what keeps US-2 true as the two front ends evolve, and it is cheap because both sides are stub-backed.

**Cancellation and leak test (VS-12)** `go test -race` with a goroutine-count assertion after a cancelled long-running call.

**Live Chrome smoke test (`testing.Short()`-guarded)** One end-to-end: start the server, drive a `data:` fixture through `navigate` → `snapshot` → `click` → `read`, assert the final state.
Deliberately thin — protocol correctness is covered by stubs; this only proves the wiring is real.

## Out of scope

- HTTP or SSE transports, and any remotely reachable server.
- MCP resources and prompts (tools only, in this RFC).
- Multi-browser or remote-browser selection.
- Replacing the existing Claude skill; the skill continues to target the CLI, and both front ends coexist.

## Open questions

1. Should `evaluate` and `raw_cdp` be in the `default` tool set?
   They are the most powerful and least constrained.
   **Recommendation:** `evaluate` in `default` with an explicit description of its power; `raw_cdp` only under `--tools full`.
2. Should the server pin one tab by default rather than accepting a `target` per call?
   Pinning is safer and simpler for agents; per-call targeting is more flexible.
   **Recommendation:** default to the sticky-tab behaviour the CLI already has, and let `--target` pin when the user wants isolation.
3. Should tool names be prefixed (`chrome_cdp_click`) to avoid collisions in clients that flatten namespaces?
   **Recommendation:** yes — most clients namespace by server, but the ones that do not produce confusing collisions with other browser servers.
4. Does the MCP layer live in this repo or a companion?
   **Recommendation:** this repo.
   A companion would have to track the `chrome.Browser` interface across releases, and the parity test above is only possible in-tree.
