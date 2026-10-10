#!/bin/bash
# usage: call2.sh <outname> <tool> '<json>'  → validation/mcp-v2/<outname>.txt
. /workspace/android-log-mcp/validation/scripts/env.sh
out=/workspace/android-log-mcp/validation/mcp-v2/$1.txt
{ echo "### host $(date '+%F %T %z')  call $2 $3"; /workspace/android-log-mcp/server/android-log-mcp call "$2" "$3"; echo "[exit $?]"; } > "$out" 2>&1
cat "$out"
