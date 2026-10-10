#!/usr/bin/env bash
# End-to-end check of `android-log-mcp report` on a real device/emulator.
#
#   scripts/e2e-report.sh [SERIAL] [EVIDENCE_DIR]
#
# Injects fake secrets of every kind the redactor covers with
# `adb shell log -t LeakTest …`, waits until the recorder stored them, builds a
# report, and verifies: no secret survives anywhere in the zip; every LeakTest
# line keeps its time/pid/tid/level/tag; the report loads back offline.
# Needs a recorder for SERIAL (an MCP client running android-log-mcp, or
# `sleep infinity | android-log-mcp &`); otherwise one is started for the test.
# Read-only for the device apart from the injected log lines.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERIAL="${1:-${ANDROID_SERIAL:-emulator-5556}}"
EVID="${2:-}"
ADB="${ANDROID_LOG_MCP_ADB:-$(command -v adb || echo "$HOME/android-sdk/platform-tools/adb")}"
HOME_DIR="${ANDROID_LOG_MCP_HOME:-${XDG_CACHE_HOME:-$HOME/.cache}/android-log-mcp}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"; [ -n "${REC_PID:-}" ] && kill "$REC_PID" 2>/dev/null || true' EXIT

BIN="$WORK/android-log-mcp"
(cd "$ROOT/server" && go build -o "$BIN" ./cmd/android-log-mcp)
"$ADB" -s "$SERIAL" get-state >/dev/null

RUN="LT$(date +%s)"
echo "run id $RUN, device $SERIAL, home $HOME_DIR" >&2

# (key prefixes are split with ${U}/${D}/${E} so that the fake keys do not
# look like real ones to secret scanners in this repository)
U=_ D=- E=
# <type>\t<message>\t<secret that must disappear>  (all values are fake)
cat > "$WORK/secrets.tsv" <<TSV
token	Authorization: Bearer abcDEF123456ghiJKL789xyz	abcDEF123456ghiJKL789xyz
jwt	got id_token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U	eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0
secret	github key ghp${U}aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789	ghp${U}aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789
secret	openai key sk${D}proj-Zx9Yw8Xv7Uu6Tt5Ss4Rr3Qq2Pp1Oo0	sk${D}proj-Zx9Yw8Xv7Uu6Tt5Ss4Rr3Qq2Pp1Oo0
secret	npm token npm${U}Ab12Cd34Ef56Gh78Ij90Kl12Mn34Op56Qr78	npm${U}Ab12Cd34Ef56Gh78Ij90Kl12Mn34Op56Qr78
secret	aws AK${E}IAIOSFODNN7EXAMPLE configured	AK${E}IAIOSFODNN7EXAMPLE
credential	login {"password":"Tr0ub4dor&3","user":"bob"}	Tr0ub4dor&3
credential	config api_key=k3y-9f8e7d6c5b4a client_secret=s3cr3t-Va1ue	k3y-9f8e7d6c5b4a
url_param	GET https://cdn.example.com/a.png?Expires=1791572202&Signature=Zm9vYmFyYmF6cXV4&x=1	Zm9vYmFyYmF6cXV4
password	connect postgres://bob:pa55w0rdX@db.example.com:5432/app	pa55w0rdX
cookie	Cookie: SESSIONID=8f2a9c77d1e4b3a2; theme=dark	8f2a9c77d1e4b3a2
email	signed in as leak.e2e@example.com	leak.e2e@example.com
phone	sms code sent to 13912345678	13912345678
phone	call +1 (415) 555-2671 now	555-2671
id_card	kyc id 11010519491231002X verified	11010519491231002X
bank_card	paid with card 4111 1111 1111 1111	4111 1111 1111 1111
bank_card	card=5555555555554444 charged	5555555555554444
device_id	imei=490154203237518 slot=0	490154203237518
ipv4	connecting to 203.0.113.77:443	203.0.113.77
ipv6	peer 2001:db8:85a3::8a2e:370:7334 up	2001:db8:85a3::8a2e:370:7334
mac	wlan0 hwaddr 3c:5a:b4:01:02:03	3c:5a:b4:01:02:03
domain	--> POST https://api.leak-e2e.com:8443/v1/login?page=2 (64-byte body)	api.leak-e2e.com
domain	Unable to resolve host sub.leake2e.cn: No address associated with hostname	sub.leake2e.cn
TSV
# structural negatives that must come through unchanged
cat > "$WORK/keep.txt" <<'KEEP'
keep pid 13812 tid 13813 uid 10146 at 1791572202.589379
keep fault addr 0x7ffd5b8e1a20 rcx 4111111111111111 rdx 0000000000000004
keep #01 pc 0000000013812345  /data/app/com.example.logtest-1/lib/x86_64/liblogtest.so
keep versionName=10.0.2.15 Chrome/83.0.41.10 build 2026-10-10 03:05:52.878
keep at com.example.logtest.Net.call(Net.java:42) java.lang.RuntimeException android.app.ActivityThread
keep act=android.intent.action.MAIN ro.build.fingerprint persist.sys.timezone base.apk libc.so base.odex main.xml app.json
KEEP

# make sure something records the device
if ! find "$HOME_DIR" -maxdepth 3 -name meta.json -newermt '-60 seconds' 2>/dev/null | grep -q .; then
  echo "no active recorder seen; starting one" >&2
  (sleep infinity | ANDROID_LOG_MCP_HOME="$HOME_DIR" "$BIN" >/dev/null 2>"$WORK/rec.log") &
  REC_PID=$!
  sleep 15
fi

i=0
while IFS=$'\t' read -r typ msg secret; do
  i=$((i+1))
  "$ADB" -s "$SERIAL" shell "log -t LeakTest '$RUN #$i $msg'" < /dev/null  # adb would eat the loop's stdin
done < "$WORK/secrets.tsv"
while read -r msg; do
  "$ADB" -s "$SERIAL" shell "log -t LeakTest '$RUN $msg'" < /dev/null
done < "$WORK/keep.txt"
N=$(( $(wc -l < "$WORK/secrets.tsv") + $(wc -l < "$WORK/keep.txt") ))

# the device's data directory: <home>/v1/<device-id> (0.4+; meta.json has the
# adb serial as "addr") or <home>/<serial> (older flat layout)
find_dir() {
  python3 - "$HOME_DIR" "$SERIAL" <<'PY'
import glob, json, os, sys
home, serial = sys.argv[1:3]
for m in sorted(glob.glob(os.path.join(home, "v1", "*", "meta.json"))) + glob.glob(os.path.join(home, "*", "meta.json")):
    try:
        meta = json.load(open(m))
    except Exception:
        continue
    if serial in (meta.get("serial"), meta.get("addr")) or serial in (meta.get("aliases") or []):
        print(os.path.dirname(m))
        break
PY
}
dir=""
for t in $(seq 1 90); do
  [ -n "$dir" ] || dir=$(find_dir)
  got=$(cat "$dir"/seg-*.jsonl 2>/dev/null | grep -c "$RUN" || true)
  [ "$got" -ge "$N" ] && break
  sleep 2
done
echo "recorder stored $got/$N LeakTest lines in $dir" >&2
DEVICE_ID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["serial"])' "$dir/meta.json")
[ "$got" -ge "$N" ]

OUT="$WORK/report.zip"
ANDROID_LOG_MCP_HOME="$HOME_DIR" "$BIN" report --serial "$SERIAL" --since 20m -o "$OUT" \
  --note "e2e $RUN: reporter leak.e2e@example.com, token=abcDEF123456ghiJKL789xyz" 2> "$WORK/report.stderr"
cat "$WORK/report.stderr" >&2
mkdir "$WORK/r" && (cd "$WORK/r" && unzip -q "$OUT")

# offline load of the report's store
"$BIN" --home "$WORK/r/store" --offline call logcat_search \
  "{\"device\":\"$DEVICE_ID\",\"filter\":\"tag=:LeakTest message:$RUN\",\"limit\":100}" > "$WORK/search.txt"

status=0
python3 - "$WORK" "$RUN" "$dir" > "$WORK/verify.txt" <<'PY' || status=$?
import glob, json, os, sys
work, run, store = sys.argv[1:4]
r = os.path.join(work, "r")
blob = b""
for p in glob.glob(r + "/**/*", recursive=True):
    if os.path.isfile(p):
        blob += open(p, "rb").read() + b"\n"
text = blob.decode("utf-8", "replace")
fail = []
rows = [l.rstrip("\n").split("\t") for l in open(os.path.join(work, "secrets.tsv"))]
man = json.load(open(os.path.join(r, "manifest.json")))
hits = man["redaction"]["hits"]
for typ, msg, secret in rows:
    if secret in text:
        fail.append(f"secret survived: {typ} {secret!r}")
    if not hits.get(typ):
        fail.append(f"no {typ} hits in manifest")
# LeakTest lines in the report vs the original recording: identical headers
def load(path):
    out = {}
    for f in sorted(glob.glob(path)):
        for ln in open(f, encoding="utf-8"):
            rec = json.loads(ln)
            if rec.get("g") == "LeakTest" and run in rec.get("m", ""):
                key = rec["m"].split(" ")[1] if rec["m"].split(" ")[1].startswith("#") else rec["m"]
                out[key] = rec
    return out
orig = load(os.path.join(store, "seg-*.jsonl"))
rep = load(os.path.join(r, "store", "*", "seg-*.jsonl"))
if len(rep) != len(orig) or not orig:
    fail.append(f"LeakTest records: original {len(orig)}, report {len(rep)}")
for k, o in orig.items():
    p = rep.get(k)
    if p is None:
        fail.append(f"missing in report: {k}")
        continue
    for f in "tupilgbkn":
        if o.get(f) != p.get(f):
            fail.append(f"{k}: field {f} changed {o.get(f)!r} -> {p.get(f)!r}")
    if not k.startswith("#") and o["m"] != p["m"]:
        fail.append(f"structural line altered: {o['m']!r} -> {p['m']!r}")
    if k.startswith("#") and "[REDACTED:" not in p["m"]:
        fail.append(f"{k}: no placeholder: {p['m']!r}")
import re
if not any(re.search(r"--> POST https://\[REDACTED:domain#\d+\]:8443/v1/login\?page=2 \(64-byte body\)$", p["m"]) for p in rep.values()):
    fail.append("URL host not redacted in place (scheme/port/path/query must stay)")
if not any(re.search(r"Unable to resolve host \[REDACTED:domain#\d+\]: No address", p["m"]) for p in rep.values()):
    fail.append("bare host not redacted in place")
search = open(os.path.join(work, "search.txt")).read()
if search.count(run) < len(orig):
    fail.append("offline load of the report did not return all LeakTest lines")
note = open(os.path.join(r, "note.txt")).read()
if "leak.e2e@example.com" in note or "[REDACTED:email#" not in note:
    fail.append("note not redacted")
print("== LeakTest lines as stored in the report (redacted) ==")
for k in sorted(rep, key=lambda k: rep[k]["t"]):
    p = rep[k]
    print(f'{p["t"]} uid={p["u"]} pid={p["p"]} tid={p["i"]} {p["l"]} {p["g"]}: {p["m"]}')
print("== manifest redaction hits ==")
print(json.dumps(hits, sort_keys=True))
print("== zip contents ==")
for f in man["files"]:
    print(f'{f["bytes"]:>9}  {f["path"]}')
if fail:
    print("FAIL:\n  " + "\n  ".join(fail))
    sys.exit(1)
print(f"PASS: {len(rows)} secrets of {len(set(r[0] for r in rows))} kinds redacted, {len(orig)} LeakTest headers intact, offline load OK")
PY
cat "$WORK/verify.txt"
if [ -n "$EVID" ]; then
  mkdir -p "$EVID"
  cp "$WORK/r/manifest.json" "$EVID/manifest.json"
  cp "$WORK/report.stderr" "$EVID/report.stderr.txt"
  cp "$WORK/search.txt" "$EVID/offline-search.txt"
  cut -f1 "$WORK/secrets.tsv" | sort | uniq -c > "$EVID/injected-secret-types.txt"
  cp "$WORK/verify.txt" "$EVID/verify.txt"
  grep -h "LeakTest" "$WORK"/r/devices/*/logcat/*.txt | grep "$RUN" > "$EVID/report-logcat-LeakTest.txt" || true
fi
exit $status
