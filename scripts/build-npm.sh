#!/usr/bin/env bash
# Cross-compile android-log-mcp and assemble the npm packages.
#
#   scripts/build-npm.sh [VERSION] [--pack]
#
# VERSION defaults to the "version" in npm/VERSION. Output:
#   npm/dist/android-log-mcp/                main package (launcher, optionalDependencies)
#   npm/dist/android-log-mcp-<os>-<cpu>/     one package per platform with bin/android-log-mcp[.exe]
#   npm/dist/tarballs/*.tgz                  with --pack (npm pack of every package)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION=""
PACK=0
for a in "$@"; do
  case "$a" in
    --pack) PACK=1 ;;
    v*|[0-9]*) VERSION="${a#v}" ;;
    *) echo "usage: $0 [VERSION] [--pack]" >&2; exit 2 ;;
  esac
done
[ -n "$VERSION" ] || VERSION="$(tr -d ' \n' < "$ROOT/npm/VERSION")"
echo "version $VERSION" >&2

DIST="$ROOT/npm/dist"
rm -rf "$DIST"
mkdir -p "$DIST/tarballs"

# npm platform name (process.platform/process.arch)  →  GOOS/GOARCH
TARGETS="linux-x64:linux/amd64 linux-arm64:linux/arm64 darwin-x64:darwin/amd64 darwin-arm64:darwin/arm64 win32-x64:windows/amd64 win32-arm64:windows/arm64"

# Build the targets in parallel (issue #25); each one logs to its own file,
# printed when it fails.
LOGS="$DIST/.build-logs"
mkdir -p "$LOGS"
(cd "$ROOT/server" && go mod download)
PIDS=()
for t in $TARGETS; do
  plat="${t%%:*}"; goos="${t#*:}"; goos="${goos%/*}"; goarch="${t#*/}"
  exe="android-log-mcp"; [ "$goos" = windows ] && exe="android-log-mcp.exe"
  out="$DIST/android-log-mcp-$plat/bin/$exe"
  mkdir -p "$(dirname "$out")"
  echo "build android-log-mcp-$plat ($goos/$goarch)" >&2
  (cd "$ROOT/server" && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out" ./cmd/android-log-mcp) >"$LOGS/$plat.log" 2>&1 &
  PIDS+=("$!:$plat")
done
FAILED=""
for p in "${PIDS[@]}"; do
  if ! wait "${p%%:*}"; then
    FAILED="$FAILED ${p#*:}"
    echo "--- build failed: android-log-mcp-${p#*:} ---" >&2
    cat "$LOGS/${p#*:}.log" >&2
  fi
done
[ -z "$FAILED" ] || { echo "build failed:$FAILED" >&2; exit 1; }
rm -rf "$LOGS"

for t in $TARGETS; do
  plat="${t%%:*}"; goos="${t#*:}"; goos="${goos%/*}"
  os="${plat%-*}"; cpu="${plat#*-}"
  pkg="android-log-mcp-$plat"
  dir="$DIST/$pkg"
  exe="android-log-mcp"; [ "$goos" = windows ] && exe="android-log-mcp.exe"
  chmod 755 "$dir/bin/$exe"
  sed -e "s/@PLATFORM@/$plat/g" -e "s/@OS@/$os/g" -e "s/@CPU@/$cpu/g" "$ROOT/npm/platform.template.json" |
    node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const p=JSON.parse(s);p.version=process.argv[1];process.stdout.write(JSON.stringify(p,null,2)+"\n")})' "$VERSION" > "$dir/package.json"
  cp "$ROOT/LICENSE" "$dir/"
  printf '# %s\n\nThe %s binary of [android-log-mcp](https://www.npmjs.com/package/android-log-mcp). Do not install directly; it is pulled in as an optional dependency.\n' "$pkg" "$plat" > "$dir/README.md"
done

main="$DIST/android-log-mcp"
mkdir -p "$main/bin"
cp "$ROOT/npm/android-log-mcp/bin/android-log-mcp.js" "$main/bin/"
chmod 755 "$main/bin/android-log-mcp.js"
cp "$ROOT/LICENSE" "$ROOT/README.md" "$main/"
node -e '
const fs=require("fs");const [tpl,out,ver,...plats]=process.argv.slice(1);
const p=JSON.parse(fs.readFileSync(tpl,"utf8"));p.version=ver;
p.optionalDependencies={};for(const pl of plats)p.optionalDependencies["android-log-mcp-"+pl]=ver;
fs.writeFileSync(out,JSON.stringify(p,null,2)+"\n");' \
  "$ROOT/npm/android-log-mcp/package.template.json" "$main/package.json" "$VERSION" \
  $(for t in $TARGETS; do printf '%s ' "${t%%:*}"; done)

if [ "$PACK" = 1 ]; then
  for d in "$DIST"/android-log-mcp*/; do
    (cd "$DIST/tarballs" && npm pack "$d" --silent >/dev/null)
  done
  ls -la "$DIST/tarballs" >&2
fi
echo "done: $DIST" >&2
