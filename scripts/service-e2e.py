#!/usr/bin/env python3
"""Live check of the background service (issue #3) on a running device.

usage: service-e2e.py SERIAL NEWROOT [-- CLIENT CMD...]

CLIENT CMD defaults to the npm launcher with ANDROID_LOG_MCP_BINARY set by
the caller (see service-e2e.sh). Uses the REAL per-user control dir and
default data root (this is the manual live check, not a unit test):

 1. two MCP sessions share one service;
 2. the service is killed -> both recover with one new service; a line
    logged while it was down is recorded;
 3. set-root NEWROOT --keep-old, then back with --delete-old, with
    `log -t ALMCHECK` markers around each switch: every marker is recorded
    exactly once across the roots (no gap, no duplicate);
 4. linger: with ANDROID_LOG_MCP_LINGER=5s the service exits 5 s after the
    last client; the default is reported by logcat_status.
"""
import json, os, re, shutil, signal, subprocess, sys, time, uuid

serial, newroot = sys.argv[1], sys.argv[2]
client = sys.argv[sys.argv.index("--") + 1:] if "--" in sys.argv else ["node", "npm/android-log-mcp/bin/android-log-mcp.js"]
adb = shutil.which("adb") or "adb"
run_id = uuid.uuid4().hex[:6]
failures = []


def log(*a):
    print(time.strftime("%H:%M:%S"), *a, flush=True)


def check(cond, what):
    log(("PASS " if cond else "FAIL ") + what)
    if not cond:
        failures.append(what)


class Session:
    def __init__(self, name, env=None):
        self.name = name
        e = dict(os.environ)
        e.update(env or {})
        self.p = subprocess.Popen(client, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=open(f"/tmp/alm-e2e-{name}.stderr", "w"), text=True, bufsize=1, env=e)
        self.id = 0
        self.req("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": name, "version": "0"}})
        self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def send(self, o):
        self.p.stdin.write(json.dumps(o) + "\n")
        self.p.stdin.flush()

    def req(self, method, params):
        self.id += 1
        self.send({"jsonrpc": "2.0", "id": self.id, "method": method, "params": params})
        while True:
            line = self.p.stdout.readline()
            if not line:
                raise SystemExit(f"{self.name}: EOF")
            m = json.loads(line)
            if m.get("id") == self.id:
                return m

    def call(self, tool, args=None):
        r = self.req("tools/call", {"name": tool, "arguments": args or {}})
        return r["result"]["content"][0]["text"]

    def service_pid(self):
        t = self.call("logcat_status")
        m = re.search(r"service: [a-z -]+(?:\([^)]*\) )?pid (\d+), version (\S+), protocol (\d+)", t)
        return (int(m.group(1)) if m else 0), t

    def close(self):
        self.p.stdin.close()
        self.p.wait(timeout=30)


def cli(*args, env=None):
    e = dict(os.environ)
    e.update(env or {})
    r = subprocess.run(client + list(args), capture_output=True, text=True, env=e)
    return r.returncode, r.stdout, r.stderr


def marker(n):
    m = f"alm-{run_id}-{n}"
    subprocess.run([adb, "-s", serial, "shell", "log", "-t", "ALMCHECK", m], check=True)
    log("logged marker", m)
    return m


def count_in(text, markers):
    return {m: len(re.findall(re.escape(m) + r"\b", text)) for m in markers}


def search_offline(root):
    code, out, err = cli("--offline", root, "call", "logcat_search", json.dumps({"tags": ["ALMCHECK"], "from": "2h", "limit": 500}))
    return out


def alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def wait_gone(pid, t=30):
    end = time.time() + t
    while time.time() < end:
        if not alive(pid):
            return True
        time.sleep(0.2)
    return False


code, out, _ = cli("root", "--json")
info = json.loads(out)
default_root = info["root"]
log("default root", default_root, "control", info["control"])
check(info["root"] == info["default_root"], "starts at the default root")

def search_live(s):
    return s.call("logcat_search", {"device": serial, "tags": ["ALMCHECK"], "from": "2h", "limit": 500})


def wait_recording(s, root=None, t=60):
    end = time.time() + t
    while time.time() < end:
        txt = s.call("logcat_status")
        if re.search(r"^    recording", txt, re.M) and (root is None or f"root: {root}" in txt):
            return True
        time.sleep(1)
    return False


def wait_marker(s, m, t=40):
    end = time.time() + t
    while time.time() < end:
        txt = search_live(s)
        if m in txt:
            return txt
        time.sleep(0.5)
    return search_live(s)


# 1. two sessions share one service
a, b = Session("A"), Session("B")
pa, ta = a.service_pid()
pb, _ = b.service_pid()
log("service pid seen by A/B:", pa, pb, "clients A/B:", a.p.pid, b.p.pid)
check(pa == pb and pa not in (a.p.pid, b.p.pid) and pa > 0, "two MCP clients share one background service")
log(ta.split("\nadb:")[0])
code, out, _ = cli("service", "status", "--json")
st = json.loads(out)
check(st.get("clients", 0) >= 2, f"service status reports {st.get('clients')} connections (2 MCP clients + this status call)")
check(wait_recording(a), "recording")
m1 = marker(1)
check(m1 in wait_marker(a, m1), "marker 1 recorded")

# 2. kill the service; a line logged while it is down; both recover
os.kill(pa, signal.SIGKILL)
check(wait_gone(pa), f"service {pa} killed (SIGKILL)")
m2 = marker(2)
pa2, _ = a.service_pid()
pb2, _ = b.service_pid()
log("new service pid seen by A/B:", pa2, pb2)
check(pa2 == pb2 and pa2 not in (0, pa), "both clients recovered with one new service")
txt = wait_marker(b, m2)
c = count_in(txt, [m1, m2])
check(c == {m1: 1, m2: 1}, f"line logged while the service was down recorded, nothing twice: {c}")

# 3. set-root keep-old, then back with delete-old
if os.path.exists(newroot):
    shutil.rmtree(newroot)
m3 = marker(3)  # recorded before the switch: the newest line, which logcat -T repeats
check(m3 in wait_marker(a, m3), "marker 3 recorded before the switch")
time.sleep(1)
m3b = marker("3b")  # logged right before the switch: lands in exactly one root
code, out, err = cli("set-root", newroot, "--keep-old", "--json")
check(code == 0, f"set-root {newroot} --keep-old (exit {code}) {err.strip()[-300:]}")
r = json.loads(out) if code == 0 else {}
log("set-root:", json.dumps(r))
check(r.get("service_pid") == pa2, "switched by the running service (same pid)")
check(wait_recording(a, newroot), "service records in the new root (status)")
m4 = marker(4)
txt_new = wait_marker(a, m4)
m5 = marker(5)
txt_new = wait_marker(a, m5)
txt_old = search_offline(default_root)
markers = [m1, m2, m3, m3b, m4, m5]
c_old, c_new = count_in(txt_old, markers), count_in(txt_new, markers)
log("default (old) root:", c_old)
log("new root:          ", c_new)
check(c_old[m1] == c_old[m2] == c_old[m3] == 1 and c_new[m3] == 0, "boundary line repeated by logcat -T is not stored again in the new root")
check(c_old[m3b] + c_new[m3b] == 1, f"line logged right before the switch recorded exactly once: old {c_old[m3b]}, new {c_new[m3b]}")
check(c_new[m4] == 1 and c_new[m5] == 1 and c_old[m4] == 0, "after the switch, lines go to the new root")
code, out, err = cli("set-root", default_root, "--delete-old", "--json")
check(code == 0, f"set-root back {default_root} --delete-old (exit {code}) {err.strip()[-300:]}")
r = json.loads(out) if code == 0 else {}
log("set-root back:", json.dumps(r))
check(r.get("old_root_removed") is True and not os.path.exists(newroot), "old root deleted")
check(wait_recording(b, default_root), "service records in the default root again")
m6 = marker(6)
txt_back = wait_marker(b, m6)
c_back = count_in(txt_back, markers + [m6])
log("default root after switching back:", c_back)
check(c_back[m5] == 0, "boundary line of the new root not stored again after switching back")
check(c_back[m1] == c_back[m2] == c_back[m3] == c_back[m6] == 1 and c_back[m4] == 0, "default root keeps its history; new-root history is not in it (as documented)")

# 4. linger
a.close()
b.close()
code, out, _ = cli("service", "stop")
log("service stop:", out.strip())
s = Session("L", {"ANDROID_LOG_MCP_LINGER": "5s"})
pl, tl = s.service_pid()
lingerline = [l for l in tl.splitlines() if l.startswith("linger:")]
log("linger line:", lingerline)
s.close()
t0 = time.time()
gone = wait_gone(pl, 30)
check(gone and 4 <= time.time() - t0 <= 15, f"service {pl} exited {time.time() - t0:.1f}s after the last client (linger 5s)")
d = Session("D")
pd, td = d.service_pid()
dl = [l for l in td.splitlines() if l.startswith("linger:")]
log("default linger line:", dl)
check(bool(dl) and dl[0].startswith("linger: ") and "default: max(10m0s, 2 × device log buffer span" in dl[0], "default linger reported")
d.close()
code, out, _ = cli("service", "stop")
log("service stop:", out.strip())

print()
print("RESULT:", "OK" if not failures else "FAILED: " + "; ".join(failures))
sys.exit(1 if failures else 0)
