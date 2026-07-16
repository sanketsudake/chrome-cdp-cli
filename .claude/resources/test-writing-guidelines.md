# Test-writing guidelines

Adapted from the [Fission test-writing conventions](https://github.com/fission/fission/blob/main/.claude/resources/test-writing-guidelines.md), keeping only what applies to this Go CLI and reconciling with the conventions already in the tree.
When a rule below disagrees with an existing test, follow the existing pattern in that package — consistency wins — and raise the discrepancy rather than silently rewriting.

## Structure (adopted)

- **Table-driven** with a `map[...]` or `[]struct` of cases; use `t.Run(name, …)` with descriptive case names for anything with more than a couple of cases (see `internal/result/result_test.go`).
- **`t.Parallel()`** on independent tests and non-state-sharing subtests. Do **not** parallelize the live-Chrome tests — they share a spawned browser.
- **`t.TempDir()`** for any filesystem fixture; never write into the repo or a hardcoded path.
- **`t.Cleanup()`** for teardown instead of bare `defer` when the resource is set up mid-test.
- Name tests `TestXxx` and test through the **exported API** of the package (the envelope, the `chrome.Browser` interface, `target.Resolve`) — not unexported internals, unless the internal is the unit.

## The `chrome.Browser` seam (project-specific)

- Unit-test CLI and daemon behavior against **`chrometest.StubBrowser`**, not a real browser. Embed it and override only the methods the test asserts on.
- When you add a method to the `chrome.Browser` interface, give it a permissive default in `chrometest.StubBrowser` — that's the single place a new method gets its test default.
- Assert on the **envelope and exit code** (the contract), not on incidental human-rendered text. A test that pins `error.code` → exit code is more valuable than one that pins a `✗ …` string.

## Live-Chrome tests (project-specific)

- Guard every test that drives a real Chrome with `if testing.Short() { t.Skip(...) }`, and skip gracefully when no Chrome binary is found. This project uses `testing.Short()` for this, **not** `//go:build integration` tags.
- These tests legitimately use `context.Background()` (the driver context is tied to the browser lifecycle, not the test's). Elsewhere, prefer a scoped context.
- Keep them hermetic: spawn the browser the test needs, drive a `data:`/local fixture page where possible, and tear it down in `t.Cleanup`.

## HTTP handlers (adopted, where relevant)

Use `net/http/httptest` (`NewRecorder` / `NewServer`) for any HTTP-facing code rather than a real network listener.

## Property-based tests & fuzzing (adopted, where valuable)

- The parser-shaped surfaces — the `target` grammar and the `--by` selector syntax — are good fuzz/property targets: round-trip stability, and "only exactly-valid input parses". Prefer `pgregory.net/rapid` over the frozen `testing/quick` if you add property tests; direct `go test -fuzz` at the parser boundary and check the corpus into `testdata/`.
- Good properties here: `Resolve` is deterministic for a fixed tab list; a selector that round-trips through parse→format is unchanged; unknown `error.code` always maps to `ExitGeneric`.

## Assertions (project deviation — note before changing)

The existing suite uses the **standard library** (`t.Errorf` / `t.Fatalf`), not `testify`.
Fission mandates `testify require`/`assert`; this repo does not depend on it.
Match the standard-library style in existing files; do not introduce `testify` as a new dependency for a single test without agreement.

## Not applicable

These Fission rules target infrastructure this project doesn't have — ignore them: Kubernetes fake clientsets / `envtest`, `go-snaps` snapshots, `porcupine`/TLA+ linearizability, `framework.Connect(t)` integration harness, SPDX-header `make license`, and the Fission-specific builders.
