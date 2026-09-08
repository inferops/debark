#!/usr/bin/env bash
#
# reproducible-check.sh - build the debark binary twice with the release
# flags and prove the two outputs are byte-identical.
#
# This is docs/dev/contract-brief.md rule 2 ("Determinism... Two builds of
# the same request must produce byte-identical manifests and repository
# metadata") applied to the release binary itself, and the reproducible-
# build promise and SECURITY.md. CI runs this on every
# push via the "determinism" job in .github/workflows/ci.yml, and
# .github/workflows/release.yml runs it as a gate before goreleaser; run it
# yourself any time with:
#
#   bash hack/reproducible-check.sh
#   bash hack/reproducible-check.sh ./cmd/debark   # same, explicit
#
# The ldflags below are .goreleaser.yaml's builds section MINUS
# -X .../core/version.Edition=official, which only the release pipeline sets
# (TRADEMARK.md, ADR-010). That omission is deliberate and harmless to what
# this script measures: Edition is a fixed string baked in at link time, so
# adding it shifts both builds identically and cannot make a reproducible
# build look irreproducible or the reverse. Everything that could — -s -w,
# -trimpath, CGO_ENABLED=0, and the Version/Commit/Date values, each pinned
# to one value reused for both builds — does match the release. If the
# release build gains a flag that varies per invocation, add it here too;
# this script exists specifically to catch the release pipeline losing
# reproducibility.
#
# Deliberately uses Go's default module mode (no -mod=mod): a reproducible-
# build check that silently rewrote go.sum on a missing entry would hide
# exactly the kind of problem it exists to catch. A "missing go.sum entry"
# failure here is a real, honest failure — fix go.sum, don't paper over it.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

pkg="${1:-./cmd/debark}"
module="github.com/inferops/debark"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    # macOS has no sha256sum by default.
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# Fix every value that could otherwise legitimately differ between two
# separate invocations (wall-clock time, above all) BEFORE either build
# runs, and reuse the exact same values for both. A real reproducibility
# bug and "this script computed two different timestamps" must never look
# the same.
commit="$(git rev-parse --short=12 HEAD 2>/dev/null || echo 000000000000)"
epoch="$(git log -1 --format=%ct 2>/dev/null || echo 0)"
date="$(date -u -d "@${epoch}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
  || date -u -r "${epoch}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
  || echo "1970-01-01T00:00:00Z")"
version="0.0.0-reproducible-check"

export CGO_ENABLED=0
export SOURCE_DATE_EPOCH="${epoch}"

ldflags="-s -w -X ${module}/core/version.Version=${version} -X ${module}/core/version.Commit=${commit} -X ${module}/core/version.Date=${date}"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

out1="$workdir/build1"
out2="$workdir/build2"

echo "== reproducible-check: building $pkg twice =="
echo "   module=$module commit=$commit date=$date SOURCE_DATE_EPOCH=$epoch"

go build -trimpath -ldflags "$ldflags" -o "$out1" "$pkg"
go build -trimpath -ldflags "$ldflags" -o "$out2" "$pkg"

sum1="$(sha256_of "$out1")"
sum2="$(sha256_of "$out2")"

echo "   build 1: $sum1"
echo "   build 2: $sum2"

if [ "$sum1" != "$sum2" ]; then
  echo "NOT REPRODUCIBLE: the two builds of $pkg differ." >&2
  if command -v cmp >/dev/null 2>&1; then
    cmp "$out1" "$out2" || true
  fi
  exit 1
fi

echo "OK: byte-identical (sha256 $sum1)"
