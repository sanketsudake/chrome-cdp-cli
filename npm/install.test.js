"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const crypto = require("node:crypto");

const { assetName, releaseURL, verifyChecksum } = require("./install.js");

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

test("releaseURL: shape", () => {
  assert.equal(
    releaseURL("0.2.2", "chrome-cdp_0.2.2_linux_amd64.tar.gz"),
    "https://github.com/sanketsudake/chrome-cdp-cli/releases/download/v0.2.2/chrome-cdp_0.2.2_linux_amd64.tar.gz",
  );
});
