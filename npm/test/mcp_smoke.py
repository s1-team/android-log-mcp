#!/usr/bin/env python3
"""MCP stdio smoke test: initialize, tools/list, tools/call logcat_status.

usage: mcp_smoke.py CMD [ARGS...]   (e.g. npx -y android-log-mcp)
Checks that every stdout line is JSON-RPC (the launcher must not write to stdout)."""
import json, subprocess, sys, time

p = subprocess.Popen(sys.argv[1:], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, bufsize=1)

def send(o):
    p.stdin.write(json.dumps(o) + "\n"); p.stdin.flush()

def recv(want_id, timeout=120):
    t0 = time.time()
    while time.time() - t0 < timeout:
        line = p.stdout.readline()
        if not line:
            raise SystemExit("EOF from server; stderr:\n" + p.stderr.read())
        msg = json.loads(line)  # raises if the launcher polluted stdout
        if msg.get("id") == want_id:
            return msg
    raise SystemExit("timeout")

t = time.time()
send({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "smoke", "version": "0"}}})
r = recv(1)
print("initialize: server=%s protocol=%s (%.1fs)" % (r["result"]["serverInfo"], r["result"]["protocolVersion"], time.time() - t))
send({"jsonrpc": "2.0", "method": "notifications/initialized"})
send({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
r = recv(2)
print("tools/list:", [x["name"] for x in r["result"]["tools"]])
send({"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "logcat_status", "arguments": {}}})
r = recv(3)
print("tools/call logcat_status (isError=%s):" % r["result"].get("isError"))
print(r["result"]["content"][0]["text"])
p.stdin.close()
code = p.wait(timeout=30)
print("server exit code after stdin EOF:", code)
sys.exit(0 if code == 0 else 1)
