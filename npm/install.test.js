"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const net = require("node:net");
const { spawnSync } = require("node:child_process");

const {
  assetName,
  releaseURL,
  verifyChecksum,
  validateVersion,
  stageAndInstall,
  fetchBuffer,
} = require("./install.js");

test("assetName: darwin/arm64", () => {
  assert.equal(
    assetName("0.2.2", "darwin", "arm64"),
    "chrome-cdp_0.2.2_darwin_arm64.tar.gz",
  );
});

test("assetName: linux/amd64", () => {
  assert.equal(
    assetName("0.2.2", "linux", "x64"),
    "chrome-cdp_0.2.2_linux_amd64.tar.gz",
  );
});

test("assetName: win32/x64", () => {
  assert.equal(
    assetName("0.2.2", "win32", "x64"),
    "chrome-cdp_0.2.2_windows_amd64.zip",
  );
});

test("assetName: win32/arm64", () => {
  assert.equal(
    assetName("0.2.2", "win32", "arm64"),
    "chrome-cdp_0.2.2_windows_arm64.zip",
  );
});

test("assetName: unsupported platform throws", () => {
  assert.throws(() => assetName("0.2.2", "sunos", "x64"), /unsupported/);
});

test("assetName: unsupported arch throws", () => {
  assert.throws(() => assetName("0.2.2", "linux", "ia32"), /unsupported/);
});

test("verifyChecksum: accepts a matching hash", () => {
  const buf = Buffer.from("hello world");
  const hash = crypto.createHash("sha256").update(buf).digest("hex");
  const checksumsTxt = `${hash}  chrome-cdp_0.2.2_linux_amd64.tar.gz\n`;
  assert.equal(
    verifyChecksum(buf, checksumsTxt, "chrome-cdp_0.2.2_linux_amd64.tar.gz"),
    true,
  );
});

test("verifyChecksum: rejects a mismatch", () => {
  const buf = Buffer.from("hello world");
  const wrongHash = "0".repeat(64);
  const checksumsTxt = `${wrongHash}  chrome-cdp_0.2.2_linux_amd64.tar.gz\n`;
  assert.throws(
    () => verifyChecksum(buf, checksumsTxt, "chrome-cdp_0.2.2_linux_amd64.tar.gz"),
    /checksum mismatch/,
  );
});

test("verifyChecksum: rejects a missing entry", () => {
  const buf = Buffer.from("hello world");
  const checksumsTxt = "deadbeef  chrome-cdp_0.2.2_darwin_arm64.tar.gz\n";
  assert.throws(
    () => verifyChecksum(buf, checksumsTxt, "chrome-cdp_0.2.2_linux_amd64.tar.gz"),
    /no checksum entry/,
  );
});

test("verifyChecksum: a different asset whose name ends with the same suffix does not match", () => {
  const buf = Buffer.from("hello world");
  const hash = crypto.createHash("sha256").update(buf).digest("hex");
  // "unofficial_chrome-cdp_0.2.2_linux_amd64.tar.gz" ends with the asset name
  // we're about to ask for, so an endsWith match would wrongly accept this
  // line's hash for it.
  const checksumsTxt = `${hash}  unofficial_chrome-cdp_0.2.2_linux_amd64.tar.gz\n`;
  assert.throws(
    () => verifyChecksum(buf, checksumsTxt, "chrome-cdp_0.2.2_linux_amd64.tar.gz"),
    /no checksum entry/,
  );
});

test("validateVersion: accepts a dotted-triple semver", () => {
  assert.equal(validateVersion("0.2.2"), "0.2.2");
});

test("validateVersion: accepts a semver with prerelease/build suffix", () => {
  assert.equal(validateVersion("1.2.3-beta.1"), "1.2.3-beta.1");
});

test("validateVersion: rejects the unset 0.0.0 placeholder", () => {
  assert.throws(() => validateVersion("0.0.0"), /unset/);
});

test("validateVersion: rejects a missing version", () => {
  assert.throws(() => validateVersion(undefined), /unset/);
  assert.throws(() => validateVersion(""), /unset/);
});

test("validateVersion: rejects a non-semver string", () => {
  assert.throws(() => validateVersion("latest"), /invalid version/);
  assert.throws(() => validateVersion("v1.2.3"), /invalid version/);
});

test("validateVersion: accepts a semver with a build-metadata suffix", () => {
  assert.equal(validateVersion("1.2.3+build.5"), "1.2.3+build.5");
});

test("validateVersion: accepts a semver with prerelease and build suffixes", () => {
  assert.equal(validateVersion("1.2.3-rc.1+build.5"), "1.2.3-rc.1+build.5");
});

test("validateVersion: accepts a simple prerelease suffix", () => {
  assert.equal(validateVersion("1.2.3-rc.1"), "1.2.3-rc.1");
});

test("validateVersion: rejects a path-traversal suffix smuggled after the version", () => {
  assert.throws(
    () => validateVersion("1.2.3-evil/../../x"),
    /invalid version/,
  );
});

test("validateVersion: rejects trailing garbage after a valid semver", () => {
  assert.throws(() => validateVersion("1.2.3abc"), /invalid version/);
  assert.throws(() => validateVersion("1.2.3 extra"), /invalid version/);
  assert.throws(() => validateVersion("1.2.3/../etc"), /invalid version/);
});

test("stageAndInstall: copies, chmods, and atomically renames on success", () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "chrome-cdp-test-"));
  try {
    const extractedPath = path.join(tmpDir, "source-binary");
    fs.writeFileSync(extractedPath, "binary contents");
    const destPath = path.join(tmpDir, "chrome-cdp");

    stageAndInstall(extractedPath, destPath);

    assert.equal(fs.readFileSync(destPath, "utf8"), "binary contents");
    assert.equal(fs.existsSync(`${destPath}.new`), false);
    if (process.platform !== "win32") {
      assert.equal(fs.statSync(destPath).mode & 0o777, 0o755);
    }
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test("stageAndInstall: removes the staged .new file when rename fails", () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "chrome-cdp-test-"));
  try {
    const extractedPath = path.join(tmpDir, "source-binary");
    fs.writeFileSync(extractedPath, "binary contents");

    // destPath is an existing directory, so renameSync(stagingPath, destPath)
    // fails (EISDIR/ENOTEMPTY) after the .new file has already been staged —
    // simulating a chmod/rename failure post-staging.
    const destPath = path.join(tmpDir, "chrome-cdp");
    fs.mkdirSync(destPath);

    assert.throws(() => stageAndInstall(extractedPath, destPath));

    assert.equal(fs.existsSync(`${destPath}.new`), false);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test("fetchBuffer: rejects with a clear error when the connection hangs", async () => {
  // A plain TCP server that accepts the connection but never speaks TLS
  // back: the request never gets a response, so this exercises the same
  // hang fetchBuffer's timeout is meant to bound.
  const server = net.createServer((socket) => {
    socket.on("error", () => {}); // ignore the reset once we destroy it below
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();

  try {
    await assert.rejects(
      () => fetchBuffer(`https://127.0.0.1:${port}/asset`, 0, 200),
      /timed out after 200ms/,
    );
  } finally {
    server.close();
  }
});

test("releaseURL: shape", () => {
  assert.equal(
    releaseURL("0.2.2", "chrome-cdp_0.2.2_linux_amd64.tar.gz"),
    "https://github.com/sanketsudake/chrome-cdp-cli/releases/download/v0.2.2/chrome-cdp_0.2.2_linux_amd64.tar.gz",
  );
});

// The postinstall path — `node install.js`, not `require("./install.js")` — is
// the one real users take, and it is the one the tests above cannot reach:
// requiring this file evaluates it to the end before any export is called, so a
// module-level `const` declared below the `require.main` block looks fine here
// and still explodes for every installer. 0.3.0 shipped exactly that bug
// ("Cannot access 'FETCH_TIMEOUT_MS' before initialization") with 24 green
// tests. These two run the file as a script instead.

// Guards that the file is executable as a script at all — a syntax error or a
// throw during evaluation. It cannot reach fetchBuffer (SKIP_DOWNLOAD returns
// first), so the const-ordering test below is what actually pins the 0.3.0 bug.
test("install.js runs as a script", () => {
  const res = spawnSync(process.execPath, [path.join(__dirname, "install.js")], {
    encoding: "utf8",
    env: { ...process.env, CHROME_CDP_SKIP_DOWNLOAD: "1" },
  });
  const out = `${res.stdout}${res.stderr}`;
  assert.doesNotMatch(out, /before initialization|ReferenceError/, out);
  assert.equal(res.status, 0, out);
});

test("no module-level const is declared after the require.main block", () => {
  const src = fs.readFileSync(path.join(__dirname, "install.js"), "utf8");
  const lines = src.split("\n");
  const entry = lines.findIndex((l) => l.startsWith("if (require.main === module)"));
  assert.ok(entry > 0, "the require.main entry-point block moved or was renamed");
  const late = lines
    .slice(entry)
    .map((l, i) => [entry + i + 1, l])
    .filter(([, l]) => /^const .*=/.test(l));
  assert.deepEqual(
    late,
    [],
    `module-level const(s) after the entry point are in the temporal dead zone when main() runs: ${JSON.stringify(late)}`,
  );
});
