#!/usr/bin/env node
// MCP stdio smoke test (Node ≥ 16, no dependencies): initialize, tools/list,
// tools/call logcat_status, then stdin EOF must end the server with code 0.
// Every stdout line must be JSON-RPC (the launcher must not write to stdout).
//
//   node npm/test/mcp_smoke.js CMD [ARGS...]     e.g. npx android-log-mcp --offline /tmp/empty
"use strict";
const { spawn } = require("child_process");
const readline = require("readline");

const [cmd, ...args] = process.argv.slice(2);
if (!cmd) {
  console.error("usage: mcp_smoke.js CMD [ARGS...]");
  process.exit(2);
}
// npx/npm are .cmd shims on Windows; they need a shell there
const child = spawn(cmd, args, { stdio: ["pipe", "pipe", "inherit"], shell: process.platform === "win32" });
const waiting = new Map();
let polluted = null;
readline.createInterface({ input: child.stdout }).on("line", (line) => {
  let msg;
  try {
    msg = JSON.parse(line);
  } catch (e) {
    polluted = line;
    return;
  }
  const w = waiting.get(msg.id);
  if (w) {
    waiting.delete(msg.id);
    w(msg);
  }
});
const timer = setTimeout(() => fail("timeout after 120 s"), 120000);
function fail(m) {
  console.error("FAIL: " + m);
  try { child.kill(); } catch (_) {}
  process.exit(1);
}
function send(o) { child.stdin.write(JSON.stringify(o) + "\n"); }
function call(id, method, params) {
  return new Promise((resolve) => {
    waiting.set(id, resolve);
    send({ jsonrpc: "2.0", id, method, params });
  });
}
child.on("error", (e) => fail("spawn: " + e.message));

(async () => {
  const t0 = Date.now();
  const init = await call(1, "initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "smoke", version: "0" } });
  if (!init.result) fail("initialize: " + JSON.stringify(init));
  console.log(`initialize: ${JSON.stringify(init.result.serverInfo)} protocol=${init.result.protocolVersion} (${Date.now() - t0} ms)`);
  send({ jsonrpc: "2.0", method: "notifications/initialized" });
  const list = await call(2, "tools/list", {});
  const names = list.result.tools.map((t) => t.name).sort();
  console.log("tools/list: " + names.join(", "));
  for (const n of ["logcat_app_launches", "logcat_crashes", "logcat_search", "logcat_status"]) {
    if (!names.includes(n)) fail("missing tool " + n);
  }
  const st = await call(3, "tools/call", { name: "logcat_status", arguments: {} });
  if (!st.result || st.result.isError) fail("logcat_status: " + JSON.stringify(st));
  console.log("logcat_status:\n" + st.result.content[0].text);
  child.stdin.end();
  child.on("exit", (code) => {
    clearTimeout(timer);
    if (polluted !== null) fail("non-JSON line on stdout: " + polluted);
    if (code !== 0) fail("server exit code " + code + " after stdin EOF");
    console.log("OK: server exited 0 after stdin EOF");
  });
})().catch((e) => fail(e.stack || String(e)));
