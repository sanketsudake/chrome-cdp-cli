# Development

## Build, test, lint

```sh
go build -o chrome-cdp ./cmd/chrome-cdp   # build the binary (the checked-in ./chrome-cdp is gitignored)
go test ./...                             # full suite — spawns a headless Chrome for the live-driver tests
go test -short ./...                      # skip every live-Chrome test (no Chrome needed; what to run without a browser)
go test -race ./...                       # what CI runs
go test -run TestExitCodeFor ./internal/result   # a single test
go test ./internal/chrome/ -run TestFill  # a single package + test
gofmt -l .                                # CI fails if this lists any file
go vet ./...
```

There is no Makefile and no golangci config — CI is exactly the four steps in `.github/workflows/ci.yml`: `gofmt -l`, `go vet`, `go test -race ./...`, `go build ./...`, on ubuntu + macOS.
Before pushing, run `gofmt -l .` (must be empty), `go vet ./...`, and `go test -race ./...` locally to match it.

## The live-Chrome tests

Tests in `internal/chrome/*_test.go` drive a **real** Chrome the test spawns.
They guard themselves with `if testing.Short() { t.Skip(...) }` and also skip gracefully when no Chrome binary is present (so `macos-latest` in CI, which has no Chrome, is green without running them).
CI runs the live path on `ubuntu-latest` (it ships `google-chrome-stable`).
When iterating on non-driver code, prefer `go test -short ./...` — it's fast and needs no browser.

## Release

Tag-driven via GoReleaser (`.goreleaser.yaml`): pushing a `vX.Y.Z` tag runs `goreleaser release` (see `.github/workflows/release.yml`).
`Version` in `internal/cli/commands.go` is set at build time via `-ldflags`; leave it `"dev"` in source.
Validate config changes with `goreleaser check`.

## Conventions

- Follow the repo's markdown style for docs: **one sentence per line** (keeps `git diff` surgical; renders identically).
- The user opens PRs — push the branch, don't open the PR, unless asked.
- Never commit the built `./chrome-cdp` binary, `*.png`/`*.pdf` screenshots, or `.scratch/` (all gitignored).
