#!/usr/bin/env node
// Thin exec shim: forwards argv/stdio/exit code to the downloaded binary.

"use strict";

const fs = require("node:fs");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const binaryName = process.platform === "win32" ? "chrome-cdp.exe" : "chrome-cdp";
const binaryPath = path.join(__dirname, ".bin", binaryName);

if (!fs.existsSync(binaryPath)) {
  console.error(
    `chrome-cdp binary not found at ${binaryPath}. The postinstall download may have failed.`,
  );
  console.error("Try one of these instead:");
  console.error("  brew install sanketsudake/tap/chrome-cdp");
  console.error(
    "  go install github.com/sanketsudake/chrome-cdp-cli/cmd/chrome-cdp@latest",
  );
  process.exit(1);
}

const result = spawnSync(binaryPath, process.argv.slice(2), {
  stdio: "inherit",
});

if (result.error) {
  console.error(String(result.error));
  process.exit(1);
}

if (result.signal) {
  // Re-raise the signal so the parent shell sees the same termination.
  process.kill(process.pid, result.signal);
} else {
  process.exit(result.status === null ? 1 : result.status);
}
