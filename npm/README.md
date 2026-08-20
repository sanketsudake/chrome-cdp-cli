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

## Trust model

The installer downloads the release archive and `checksums.txt` from the same GitHub release, over the same HTTPS connection, and verifies the archive's SHA-256 against the entry in `checksums.txt` before it extracts anything.
This check confirms integrity: the bytes you got are the bytes that release published, and the download was not corrupted or altered in transit.
It does not add authenticity beyond what GitHub's own account and release security already provide, because `checksums.txt` comes from that same release and is not signed or verified against an independent, out-of-band source.
If you need a stronger guarantee than "GitHub's release for this repo was not tampered with," build from source or verify a signed artifact instead.
