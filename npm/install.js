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

// Every module-level `const` MUST be declared above the `require.main` block
// below, which starts main() while this file is still evaluating. A const
// declared after it is in the temporal dead zone when main() reaches it: 0.3.0
// shipped with FETCH_TIMEOUT_MS down beside fetchBuffer and every npm install
// died with "Cannot access 'FETCH_TIMEOUT_MS' before initialization" — invisible
// to the tests, which `require` this file and so finish evaluating it first.
const FETCH_TIMEOUT_MS = 60_000;

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
 * Reject anything that isn't a resolved, dotted-triple semver (and the
 * "0.0.0" placeholder the repo ships before a release job sets it). Throws
 * with a message suitable for the fallback-lines catch handler.
 */
function validateVersion(version) {
  if (!version || version === "0.0.0") {
    throw new Error(
      `package.json version is unset (${version}); cannot resolve a release`,
    );
  }
  if (!/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/.test(version)) {
    throw new Error(`invalid version: ${version}`);
  }
  return version;
}

/**
 * Verify buffer's SHA-256 against the "<hash>  <name>" lines in
 * checksums.txt. Throws on missing entry or mismatch.
 */
function verifyChecksum(buffer, checksumsTxt, asset) {
  const line = checksumsTxt
    .split("\n")
    .map((l) => l.trim())
    .find((l) => l.split(/\s+/)[1] === asset);
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

module.exports = {
  assetName,
  releaseURL,
  verifyChecksum,
  validateVersion,
  stageAndInstall,
  fetchBuffer,
};

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
  const version = validateVersion(pkg.version);

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
  fs.mkdirSync(binDir, { recursive: true });

  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "chrome-cdp-install-"));
  try {
    const archivePath = path.join(
      tmpDir,
      asset.endsWith(".zip") ? "archive.zip" : "archive.tar.gz",
    );
    fs.writeFileSync(archivePath, archiveBuf);

    extractArchive(archivePath, tmpDir);

    const binaryName =
      process.platform === "win32" ? "chrome-cdp.exe" : "chrome-cdp";
    const extractedPath = findBinary(tmpDir, binaryName);
    if (!extractedPath) {
      throw new Error(`${binaryName} not found in downloaded archive`);
    }

    const destPath = path.join(binDir, binaryName);
    stageAndInstall(extractedPath, destPath);

    console.log(`Installed ${binaryName} (${version}) to ${destPath}`);
  } finally {
    // Always clean up the temp dir, whether extraction/copy succeeded,
    // threw, or the binary was simply missing from the archive.
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
}

/**
 * Extract a .tar.gz or .zip archive using the system `tar` binary.
 *
 * This extracts the whole archive into a disposable temp dir rather than
 * selecting just the `chrome-cdp`/`chrome-cdp.exe` member: the archive's
 * bytes are already checksum-pinned before extraction, so a full extract
 * is no less trusted than a partial one, and single-member selection isn't
 * available uniformly across `tar`'s zip mode on Windows vs. its tar.gz
 * mode on macOS/Linux without adding a second extraction path to maintain.
 */
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

/**
 * Stage `extractedPath` next to `destPath` as `<destPath>.new` (same
 * filesystem as the destination), make it executable, then rename it into
 * place last. rename() is atomic within a filesystem, so a failure at any
 * earlier step — bad extraction, missing binary — leaves the previously
 * installed binary (if any) untouched rather than a half-copied file.
 *
 * If chmod or rename fails after the .new file is staged, it would
 * otherwise be orphaned in binDir (outside tmpDir, so the caller's
 * tmpDir cleanup won't touch it); this removes it before rethrowing.
 */
function stageAndInstall(extractedPath, destPath) {
  const stagingPath = `${destPath}.new`;
  fs.copyFileSync(extractedPath, stagingPath);
  try {
    fs.chmodSync(stagingPath, 0o755);
    fs.renameSync(stagingPath, destPath);
  } catch (err) {
    fs.rmSync(stagingPath, { force: true });
    throw err;
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

/**
 * Fetch a URL into a Buffer, following up to 5 redirects. Aborts and
 * rejects with a clear error if no response completes within timeoutMs
 * (default FETCH_TIMEOUT_MS), so a hung connection can't hang
 * `npm install` forever. timeoutMs is only overridden by tests.
 */
function fetchBuffer(url, redirects = 5, timeoutMs = FETCH_TIMEOUT_MS) {
  return new Promise((resolve, reject) => {
    const controller = new AbortController();
    const timer = setTimeout(() => {
      controller.abort();
    }, timeoutMs);

    const req = https
      .get(
        url,
        {
          headers: { "User-Agent": "chrome-cdp-npm-installer" },
          signal: controller.signal,
        },
        (res) => {
          if (
            res.statusCode >= 300 &&
            res.statusCode < 400 &&
            res.headers.location &&
            redirects > 0
          ) {
            res.resume();
            clearTimeout(timer);
            resolve(fetchBuffer(res.headers.location, redirects - 1, timeoutMs));
            return;
          }
          if (res.statusCode !== 200) {
            res.resume();
            clearTimeout(timer);
            reject(new Error(`GET ${url} failed: ${res.statusCode}`));
            return;
          }
          const chunks = [];
          res.on("data", (c) => chunks.push(c));
          res.on("end", () => {
            clearTimeout(timer);
            resolve(Buffer.concat(chunks));
          });
          res.on("error", (err) => {
            clearTimeout(timer);
            if (controller.signal.aborted) {
              reject(
                new Error(`GET ${url} timed out after ${timeoutMs}ms`),
              );
              return;
            }
            reject(err);
          });
        },
      )
      .on("error", (err) => {
        clearTimeout(timer);
        if (controller.signal.aborted) {
          reject(
            new Error(`GET ${url} timed out after ${timeoutMs}ms`),
          );
          return;
        }
        reject(err);
      });
    req.on("close", () => clearTimeout(timer));
  });
}

/** Fetch a URL as UTF-8 text, following redirects. */
async function fetchText(url) {
  const buf = await fetchBuffer(url);
  return buf.toString("utf8");
}
