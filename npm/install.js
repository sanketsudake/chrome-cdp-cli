#!/usr/bin/env node
// Postinstall shim: downloads the matching chrome-cdp release binary for
// this platform/arch, verifies its SHA-256 against checksums.txt, and
// extracts it into npm/bin/.bin/. Never leaves a partial install: any
// failure prints the brew/go-install fallback lines and exits 1.

"use strict";

const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const crypto = require("node:crypto");
const https = require("node:https");
const { execFileSync } = require("node:child_process");

const REPO = "sanketsudake/chrome-cdp-cli";

const OS_MAP = { darwin: "darwin", linux: "linux", win32: "windows" };
const ARCH_MAP = { x64: "amd64", arm64: "arm64" };

const FALLBACK_LINES = [
  "Install failed. Try one of these instead:",
  "  brew install sanketsudake/tap/chrome-cdp",
  "  go install github.com/sanketsudake/chrome-cdp-cli/cmd/chrome-cdp@latest",
];

/**
 * Map a Node platform/arch pair to the goreleaser archive name for a
 * version (without the leading "v").
 */
function assetName(version, platform, arch) {
  const goos = OS_MAP[platform];
  const goarch = ARCH_MAP[arch];
  if (!goos || !goarch) {
    throw new Error(`unsupported platform/arch: ${platform}/${arch}`);
  }
  const ext = goos === "windows" ? "zip" : "tar.gz";
  return `chrome-cdp_${version}_${goos}_${goarch}.${ext}`;
}

/** Build the GitHub release download URL for a version + asset name. */
function releaseURL(version, asset) {
  return `https://github.com/${REPO}/releases/download/v${version}/${asset}`;
}

/**
 * Verify buffer's SHA-256 against the "<hash>  <name>" lines in
 * checksums.txt. Throws on missing entry or mismatch.
 */
function verifyChecksum(buffer, checksumsTxt, asset) {
  const line = checksumsTxt
    .split("\n")
    .map((l) => l.trim())
    .find((l) => l.endsWith(asset));
  if (!line) {
    throw new Error(`no checksum entry for ${asset}`);
  }
  const expected = line.split(/\s+/)[0].toLowerCase();
  const actual = crypto.createHash("sha256").update(buffer).digest("hex");
  if (actual !== expected) {
    throw new Error(
      `checksum mismatch for ${asset}: expected ${expected}, got ${actual}`,
    );
  }
  return true;
}

module.exports = { assetName, releaseURL, verifyChecksum };

// Everything below only runs when this file is executed directly
// (postinstall), never when required as a test dependency.
if (require.main === module) {
  main().catch((err) => {
    console.error(String((err && err.message) || err));
    console.error(FALLBACK_LINES.join("\n"));
    process.exit(1);
  });
}

async function main() {
  if (process.env.CHROME_CDP_SKIP_DOWNLOAD === "1") {
    console.log("CHROME_CDP_SKIP_DOWNLOAD=1 set; skipping binary download.");
    return;
  }

  const pkg = JSON.parse(
    fs.readFileSync(path.join(__dirname, "package.json"), "utf8"),
  );
  const version = pkg.version;
  if (!version || version === "0.0.0") {
    throw new Error(
      "package.json version is unset (0.0.0); cannot resolve a release",
    );
  }

  const asset = assetName(version, process.platform, process.arch);
  const url = releaseURL(version, asset);
  const checksumsURL = releaseURL(version, "checksums.txt");

  console.log(`Downloading ${asset}...`);
  const [archiveBuf, checksumsTxt] = await Promise.all([
    fetchBuffer(url),
    fetchText(checksumsURL),
  ]);

  verifyChecksum(archiveBuf, checksumsTxt, asset);

  const binDir = path.join(__dirname, "bin", ".bin");
  fs.rmSync(binDir, { recursive: true, force: true });
  fs.mkdirSync(binDir, { recursive: true });

  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "chrome-cdp-install-"));
  const archivePath = path.join(
    tmpDir,
    asset.endsWith(".zip") ? "archive.zip" : "archive.tar.gz",
  );
  fs.writeFileSync(archivePath, archiveBuf);

  extractArchive(archivePath, tmpDir);

  const binaryName = process.platform === "win32" ? "chrome-cdp.exe" : "chrome-cdp";
  const extractedPath = findBinary(tmpDir, binaryName);
  if (!extractedPath) {
    throw new Error(`${binaryName} not found in downloaded archive`);
  }

  const destPath = path.join(binDir, binaryName);
  fs.copyFileSync(extractedPath, destPath);
  fs.chmodSync(destPath, 0o755);
  fs.rmSync(tmpDir, { recursive: true, force: true });

  console.log(`Installed ${binaryName} (${version}) to ${destPath}`);
}

/** Extract a .tar.gz or .zip archive using the system `tar` binary. */
function extractArchive(archivePath, destDir) {
  try {
    if (archivePath.endsWith(".zip")) {
      // tar on Windows 10+ (bsdtar) extracts zip archives too.
      execFileSync("tar", ["-xf", archivePath, "-C", destDir], {
        stdio: "pipe",
      });
    } else {
      execFileSync("tar", ["-xzf", archivePath, "-C", destDir], {
        stdio: "pipe",
      });
    }
  } catch (err) {
    throw new Error(
      `extraction failed (requires a \`tar\` binary on PATH): ${
        (err && err.message) || err
      }`,
    );
  }
}

/** Depth-first search for a file named `name` under `dir`. */
function findBinary(dir, name) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      const found = findBinary(full, name);
      if (found) return found;
    } else if (entry.name === name) {
      return full;
    }
  }
  return null;
}

/** Fetch a URL into a Buffer, following up to 5 redirects. */
function fetchBuffer(url, redirects = 5) {
  return new Promise((resolve, reject) => {
    https
      .get(url, { headers: { "User-Agent": "chrome-cdp-npm-installer" } }, (res) => {
        if (
          res.statusCode >= 300 &&
          res.statusCode < 400 &&
          res.headers.location &&
          redirects > 0
        ) {
          res.resume();
          resolve(fetchBuffer(res.headers.location, redirects - 1));
          return;
        }
        if (res.statusCode !== 200) {
          res.resume();
          reject(new Error(`GET ${url} failed: ${res.statusCode}`));
          return;
        }
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => resolve(Buffer.concat(chunks)));
        res.on("error", reject);
      })
      .on("error", reject);
  });
}

/** Fetch a URL as UTF-8 text, following redirects. */
async function fetchText(url) {
  const buf = await fetchBuffer(url);
  return buf.toString("utf8");
}
