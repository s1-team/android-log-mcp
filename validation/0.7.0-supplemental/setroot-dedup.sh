#!/bin/bash
# #56: live set-root, then a whole-buffer re-read (buffer contains clock jumps) -> no duplicates across old+new root
B=/workspace/v070-bin/android-log-mcp; export XDG_CACHE_HOME=/workspace/scratch/c7 PATH=$HOME/android-sdk/platform-tools:$PATH
D=emulator-5556; S="adb -s $D"; A=$XDG_CACHE_HOME/android-log-mcp; NEW=/workspace/scratch/c7-newroot; ts(){ date +%T; }; log(){ echo "$(ts) $*"; }
rm -rf $NEW; call(){ $B call "$@" 2>&1; }
caught(){ for i in $(seq 1 80); do call logcat_status "{\"device\":\"$D\"}" | grep -q "recording; caught up" && return 0; sleep 3; done; return 1; }
mk(){ $S shell log -t ALMDEDUP "$1"; log "marker $1"; }
call logcat_status "{\"device\":\"$D\"}" >/dev/null; caught && log "caught up (old root)" || log "NOT caught up"
for m in S1a S1b S1c; do mk $m; sleep 1; done; sleep 4
$B set-root $NEW --keep-old 2>&1 | tail -2 | cut -c1-200
mk S2a; mk S2b; sleep 4
$B service stop | head -1; sleep 2
call logcat_status "{\"device\":\"$D\"}" >/dev/null; log "service restarted; waiting for catch-up (whole-buffer re-read)"; caught && log "caught up (new root)" || log "NOT caught up"
mk S3a; sleep 6
grep -E "set-root|clock jump|whole buffer|root switched" $A/log/service.log | tail -4 | cut -c1-230
$B service stop | head -1
python3 - <<PY
import json,glob,collections
def load(root):
    L=[]
    for f in sorted(glob.glob(root+'/v1/*/seg-*.jsonl')):
        for ln in open(f):
            try: o=json.loads(ln)
            except: continue
            L.append((o['b'],o['t'],o['u'],o['p'],o['i'],o['l'],o['g'],o['m'],o['f']))
    return L
a=load("$A"); b=load("$NEW")
ka=collections.Counter(a); kb=collections.Counter(b)
print("old root lines",len(a),"distinct",len(ka),"| new root lines",len(b),"distinct",len(kb))
print("duplicate keys inside old:",sum(1 for k,v in ka.items() if v>1),"inside new:",sum(1 for k,v in kb.items() if v>1))
cross=[k for k in kb if k in ka]
print("keys in BOTH roots (re-read duplicates):",len(cross))
for k in cross[:5]: print("  ",k[1],k[6],k[7][:60])
for m in ["S1a","S1b","S1c","S2a","S2b","S3a"]:
    print(m,"old",sum(v for k,v in ka.items() if k[6]=="ALMDEDUP" and k[7]==m),"new",sum(v for k,v in kb.items() if k[6]=="ALMDEDUP" and k[7]==m))
PY
