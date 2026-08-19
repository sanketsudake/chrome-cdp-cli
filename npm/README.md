# @sanketsudake/chrome-cdp

This package is a thin shim, not the CLI itself.
On install it downloads the matching prebuilt `chrome-cdp` binary from the GitHub release for your platform and verifies its checksum.

```sh
npm i -g @sanketsudake/chrome-cdp
# or:
npx @sanketsudake/chrome-cdp doctor
```

Supported platforms: macOS (arm64, amd64), Linux (amd64, arm64), Windows (amd64, arm64).
Extraction shells out to the system `tar` binary, which ships with macOS, Linux, and Windows 10+.

If the download fails, install another way instead:

```sh
brew install sanketsudake/tap/chrome-cdp
# or:
go install github.com/sanketsudake/chrome-cdp-cli/cmd/chrome-cdp@latest
```

See the [main README](https://github.com/sanketsudake/chrome-cdp-cli#readme) for full usage.
