#!/usr/bin/env bash
# Live check of the background service (issue #3) on a running device,
# through the npm launcher (as `npx android-log-mcp` runs it).
# usage: scripts/service-e2e.sh SERIAL [NEWROOT]   (adb on PATH)
#   env ANDROID_LOG_MCP_BIN: the binary (default server/android-log-mcp)
# Uses the real per-user control dir and default data root; NEWROOT
# (default /tmp/alm-newroot) is created and deleted again.
set -euo pipefail
cd "$(dirname "$0")/.."
serial=${1:?usage: service-e2e.sh SERIAL [NEWROOT]}
newroot=${2:-/tmp/alm-newroot}
export ANDROID_LOG_MCP_BINARY=${ANDROID_LOG_MCP_BIN:-$PWD/server/android-log-mcp}
exec python3 scripts/service-e2e.py "$serial" "$newroot" -- node npm/android-log-mcp/bin/android-log-mcp.js
