#!/usr/bin/env bash
# Check actual Git trailers, and fail if the requested history cannot be read.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: bash hack/check-dco.sh BASE_SHA HEAD_SHA" >&2
  exit 2
fi

base="$1"
head="$2"
git rev-parse --verify "${base}^{commit}" >/dev/null
git rev-parse --verify "${head}^{commit}" >/dev/null
# A process substitution in a while loop hides rev-list's exit status.
# Capture it first so missing history cannot become a successful empty check.
commits="$(git rev-list --no-merges "${base}..${head}")"
missing=0
while IFS= read -r sha; do
  [ -z "$sha" ] && continue
  trailers="$(git log -1 --format='%(trailers:key=Signed-off-by,unfold=true)' "$sha")"
  if ! printf '%s\n' "$trailers" | grep -qE '^Signed-off-by: .+ <[^<>[:space:]]+@[^<>[:space:]]+>$'; then
    echo "::error::Commit $sha has no DCO Signed-off-by trailer."
    missing=$((missing + 1))
  fi
done <<< "$commits"

if [ "$missing" -gt 0 ]; then
  echo "$missing commit(s) are missing a DCO sign-off. See CONTRIBUTING.md."
  exit 1
fi
echo "All commits in this pull request are signed off."
