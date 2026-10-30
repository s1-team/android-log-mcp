#!/usr/bin/env node
// Launcher for the android-log-mcp Go binary (esbuild/biome pattern).
// It resolves the platform package installed as an optionalDependency and
// runs its binary with stdio inherited. It must never write to stdout:
// stdout is the MCP JSON-RPC channel.
"use strict";

const { spawn } = require("child_process");
const path = require("path");
const fs = require("fs");

const PLATFORMS = {
  "linux x64": "android-log-mcp-linux-x64",
  "linux arm64": "android-log-mcp-linux-arm64",
  "darwin x64": "android-log-mcp-darwin-x64",
  "darwin arm64": "android-log-mcp-darwin-arm64",
  "win32 x64": "android-log-mcp-win32-x64",
  "win32 arm64": "android-log-mcp-win32-arm64",
};

function fail(msg) {
  process.stderr.write("android-log-mcp: " + msg + "\n");
  process.exit(1);
}

function binaryPath() {
  const override = process.env.ANDROID_LOG_MCP_BINARY;
  if (override) return override;
  const key = process.platform + " " + process.arch;
  const pkg = PLATFORMS[key];
  if (!pkg) {
    fail(
      "unsupported platform " + key + ". Supported: " + Object.keys(PLATFORMS).join(", ") +
        ". Build from source: https://github.com/s1-team/android-log-mcp (go build ./server/cmd/android-log-mcp) " +
        "and set ANDROID_LOG_MCP_BINARY to the binary path."
    );
  }
  const exe = process.platform === "win32" ? "android-log-mcp.exe" : "android-log-mcp";
  try {
    return require.resolve(pkg + "/bin/" + exe);
  } catch (e) {
    // fallback: sibling directory (e.g. unusual node_modules layouts)
    const guess = path.join(__dirname, "..", "..", pkg, "bin", exe);
    if (fs.existsSync(guess)) return guess;
    fail(
      "the platform package \"" + pkg + "\" is not installed. It is an optionalDependency of android-log-mcp; " +
        "it may have been skipped (e.g. --no-optional / --omit=optional, or a lockfile made on another OS). " +
        "Reinstall with optional dependencies, e.g. `npm i -g android-log-mcp` or `npx -y android-log-mcp`, " +
        "or set ANDROID_LOG_MCP_BINARY to a binary built from source."
    );
  }
}

const bin = binaryPath();
// The binary is started directly (no shell, so no cmd.exe on Windows) and
// without a console window of its own.
const child = spawn(bin, process.argv.slice(2), { stdio: "inherit", windowsHide: true, shell: false });

// Forward termination signals to the child; exit when it exits.
const signals = ["SIGINT", "SIGTERM", "SIGHUP", "SIGQUIT", "SIGBREAK"];
for (const sig of signals) {
  try {
    process.on(sig, () => {
      if (child.exitCode === null && child.signalCode === null) {
        try { child.kill(sig); } catch (_) {}
      }
    });
  } catch (_) {
    // signal not supported on this platform
  }
}

child.on("error", (err) => fail("failed to start " + bin + ": " + err.message));
child.on("exit", (code, signal) => {
  if (signal) {
    // re-raise the same signal so our parent sees the real cause
    for (const sig of signals) process.removeAllListeners(sig);
    try { process.kill(process.pid, signal); } catch (_) {}
    process.exit(128 + (require("os").constants.signals[signal] || 1));
  }
  process.exit(code === null ? 1 : code);
});
