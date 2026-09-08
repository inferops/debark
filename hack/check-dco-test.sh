#!/usr/bin/env bash
# Exercise the DCO check against real temporary Git history.
set -euo pipefail
checker="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-dco.sh"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
cd "$work"
git init -q
git config user.name 'DCO Test'
git config user.email 'dco-test@example.org'
git config commit.gpgsign false
git -c core.hooksPath=/dev/null commit -q --allow-empty -m 'base'
base="$(git rev-parse HEAD)"

git -c core.hooksPath=/dev/null commit -q --allow-empty -s -m 'signed'
bash "$checker" "$base" HEAD
if bash "$checker" missing-base HEAD >failure.log 2>&1; then
  echo 'DCO check accepted missing history' >&2
  exit 1
fi

git -c core.hooksPath=/dev/null commit -q --allow-empty -m 'unsigned'
if bash "$checker" "$base" HEAD >failure.log 2>&1; then
  echo 'DCO check accepted an unsigned commit' >&2
  exit 1
fi

# A sign-off quoted in the message body is not a trailer.
git -c core.hooksPath=/dev/null commit -q --allow-empty -F - <<'MESSAGE'
quoted example

Signed-off-by: DCO Test <dco-test@example.org>

This paragraph is the actual end of the message.
MESSAGE
if bash "$checker" HEAD^ HEAD >failure.log 2>&1; then
  echo 'DCO check accepted a sign-off outside the trailers' >&2
  exit 1
fi
echo 'DCO regression checks passed.'
