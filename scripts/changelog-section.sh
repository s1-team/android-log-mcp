#!/usr/bin/env bash
# Print the CHANGELOG.md section for one version (without its "## [X.Y.Z]"
# heading), followed by a "Full changelog" compare link. Used by release.yml
# for the GitHub Release notes. Fails if the section is missing or empty.
#   scripts/changelog-section.sh 0.4.0 [CHANGELOG.md]
set -euo pipefail
ver="${1:?usage: changelog-section.sh VERSION [CHANGELOG.md]}"
ver="${ver#v}"
file="${2:-$(dirname "$0")/../CHANGELOG.md}"
body="$(awk -v v="$ver" '
  /^## \[/ { if (on) exit; if (index($0, "## [" v "]") == 1) { on = 1; next } }
  /^\[[^]]+\]: / { if (on) exit }
  on { print }
' "$file" | sed -e '/./,$!d' | sed -e ':a' -e '/^\n*$/{$d;N;ba' -e '}')"
if [ -z "$(printf '%s' "$body" | tr -d '[:space:]')" ]; then
  echo "changelog-section: no entries for $ver in $file" >&2
  exit 1
fi
printf '%s\n' "$body"
link="$(awk -v v="$ver" 'index($0, "[" v "]: ") == 1 { print substr($0, length(v) + 5) }' "$file")"
if [ -n "$link" ]; then printf '\n**Full changelog:** %s\n' "$link"; fi
