#!/usr/bin/env bash
#
# reproducible-check.sh - build the Debark desktop binary twice with the
# release flags and prove the two outputs are byte-identical.
#
# This is the sibling of ../hack/reproducible-check.sh, and it is
# deliberately not a copy of it, because the claim that script proves does not
# transfer to this repository:
#
#     * debark is CGO_ENABLED=0 on every target. Its binary is a pure function
#     of (source, Go toolchain, flags).
#     * debark-gui LINKS GTK3 AND WEBKITGTK THROUGH CGO on Linux. The Linux
#     binary is a function of (source, Go toolchain, flags, the C compiler, the
#     system linker, and the exact GTK/WebKitGTK/glibc headers and .so files
#     present on the build machine). None of those are pinned by anything in
#     this repository; apt resolves them at build time.
#
# So this script does not claim "anyone can rebuild this byte-for-byte". It
# proves a narrower, checkable property, per target:
#
#   Linux/amd64 (cgo):  same source + same toolchain + same C toolchain and
#                       system libraries => byte-identical. In practice that
#                       means "same container image", and the release pins the
#                       runner image rather than the libraries. This target is
#                       built -buildmode=pie, which was measured to change
#                       neither half of that sentence before it was adopted.
#   Windows/amd64:      CGO_ENABLED=0, so the debark-grade claim does hold:
#                       same source + same Go toolchain => byte-identical,
#                       cross-built from anywhere.
#
# docs/release.md records what was actually measured, with the commands and
# the digests. Read it before quoting a reproducibility property at anyone.
#
# Usage:
#   bash hack/reproducible-check.sh                 # the host GOOS
#   bash hack/reproducible-check.sh linux           # native cgo build
#   bash hack/reproducible-check.sh windows         # CGO_ENABLED=0, cross-buildable
#   bash hack/reproducible-check.sh linux windows   # both
#
# The flags below intentionally mirror .goreleaser.yaml's builds section. If
# one changes (a new -X target, a new tag, a new flag) change the other to
# match: this script exists specifically to catch the release pipeline losing
# reproducibility, so it has to build the way the release builds.
#
# Deliberately uses Go's default module mode (no -mod=mod) for the same reason
# the core repository's version does: a reproducible-build check that silently
# rewrote go.sum would hide exactly the kind of problem it exists to catch.
# And it never runs `go mod tidy` - docs/dev/contract-brief.md rule 6.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

targets=("$@")
if [ "${#targets[@]}" -eq 0 ]; then
  targets=("$(go env GOOS)")
fi

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    # macOS, and Git Bash without coreutils, have no sha256sum.
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# A stable digest of a whole directory tree: every file's path and content,
# in sorted order. Used for frontend/dist, which is not a file but is embedded
# into the binary wholesale by main.go's //go:embed all:frontend/dist.
tree_digest() {
  local dir="$1"
  (
    cd "$dir"
    find . -type f | LC_ALL=C sort | while IFS= read -r f; do
      printf '%s  %s\n' "$(sha256_of "$f")" "$f"
    done
  ) | {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum; else shasum -a 256; fi
  } | awk '{print $1}'
}

# ---------------------------------------------------------------------------
# Fix every value that could otherwise legitimately differ between two
# invocations (wall-clock time above all) BEFORE either build runs, and reuse
# the identical values for both. A real reproducibility bug and "this script
# computed two different timestamps" must never look the same.
# ---------------------------------------------------------------------------
commit="$(git rev-parse --short=12 HEAD 2>/dev/null || echo 000000000000)"
epoch="$(git log -1 --format=%ct 2>/dev/null || echo 0)"
version="0.0.0-reproducible-check"

# Go itself ignores SOURCE_DATE_EPOCH; it is exported for two other readers.
# goreleaser uses the same value for archive member timestamps (mod_timestamp),
# and GCC honours it for __DATE__/__TIME__ - which matters here and does not
# matter in the core repository, because this is the repository with a C
# compiler in its Linux build.
export SOURCE_DATE_EPOCH="${epoch}"

# ---------------------------------------------------------------------------
# The engine half of this application arrives through
# `replace github.com/inferops/debark => ..`, off local disk, with
# no version and no go.sum entry (docs/dependency-review.md, "The `replace`
# directive"). A digest produced here is only attributable to a source state if
# that engine module's state is recorded too - and is attributable to
# nothing at all if that tree is dirty. Say so rather than printing a digest
# that looks authoritative.
# ---------------------------------------------------------------------------
core_dir="$(cd "$repo_root/.." 2>/dev/null && pwd || true)"
core_state="ABSENT"
if [ -n "$core_dir" ]; then
  core_commit="$(git -C "$core_dir" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
  if [ -n "$(git -C "$core_dir" status --porcelain 2>/dev/null)" ]; then
    core_state="${core_commit} +UNCOMMITTED-CHANGES"
  else
    core_state="${core_commit} clean"
  fi
fi

echo "== reproducible-check =================================================="
echo "   debark-gui commit : ${commit}"
echo "   engine tree       : ${core_state}   (${core_dir:-<not found>})"
echo "   SOURCE_DATE_EPOCH   : ${epoch}"
echo "   go                  : $(go version)"
if [ "$core_state" != "${core_state%UNCOMMITTED-CHANGES}" ]; then
  echo
  echo "   WARNING: the engine tree has uncommitted changes. The digests below are"
  echo "            still a valid determinism check, but they are NOT"
  echo "            attributable to any commit of the engine, so do not quote"
  echo "            them as a release digest."
fi
echo

# ---------------------------------------------------------------------------
# The frontend "build" is a recursive copy (hack/copyfrontend; contract rule
# 5 - there is no bundler). It is embedded wholesale, so if it were not a
# deterministic function of the source the binary could not be either. Run it
# twice and compare the trees before touching the compiler, so a copy-step
# defect is reported as a copy-step defect rather than as a mysterious binary
# diff.
# ---------------------------------------------------------------------------
echo "-- frontend bundle (hack/copyfrontend, run twice) ----------------------"
go run ./hack/copyfrontend
fe1="$(tree_digest frontend/dist)"
go run ./hack/copyfrontend
fe2="$(tree_digest frontend/dist)"
echo "   copy 1: ${fe1}"
echo "   copy 2: ${fe2}"
if [ "$fe1" != "$fe2" ]; then
  echo "NOT REPRODUCIBLE: frontend/dist differs between two copy runs." >&2
  exit 1
fi
echo "   OK: frontend/dist is identical across runs ($(find frontend/dist -type f | wc -l | tr -d ' ') files)"
echo

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

status=0

for goos in "${targets[@]}"; do
  case "$goos" in
    linux)
      # desktop,production are mandatory on every target: without them the
      # binary compiles and then refuses to run, printing "Wails applications
      # will not build without the correct build tags". webkit2_41 selects the
      # WebKitGTK 4.1 ABI, the only one Ubuntu 24.04 and Debian 13 carry.
      tags="desktop,production,webkit2_41"
      cgo=1
      ext=""
      # -buildmode=pie, matching .goreleaser.yaml's linux build. It is here
      # for the reason this whole script exists: pie changes the link, and a
      # determinism check that built the release's flags minus one of them
      # would prove a property of a binary nobody ships. Measured before it
      # landed and it does not cost the property - docs/release.md section 4.7.
      buildmode="-buildmode=pie"
      extra_ldflags=""
      ;;
    windows)
      tags="desktop,production"
      cgo=0
      ext=".exe"
      # No -buildmode=pie on windows: .goreleaser.yaml does not use it there
      # and the comment in that file says why. -H windowsgui suppresses the
      # console window and IS in .goreleaser.yaml, so it belongs here too.
      buildmode=""
      extra_ldflags=" -H windowsgui"
      ;;
    *)
      echo "unsupported target '${goos}'. This project ships linux and windows only" >&2
      echo "(macOS is out of scope; see .github/workflows/ci.yml)." >&2
      exit 2
      ;;
  esac

  if [ "$goos" = "linux" ] && [ "$(go env GOHOSTOS)" != "linux" ]; then
    echo "-- ${goos}: SKIPPED ----------------------------------------------------"
    echo "   The Linux build is cgo (GTK3 + WebKitGTK headers) and cannot be"
    echo "   cross-compiled from $(go env GOHOSTOS). Run this inside the Linux"
    echo "   build container - see docs/release.md."
    echo
    continue
  fi

  ldflags="-s -w${extra_ldflags} -X main.version=${version}"
  out1="${workdir}/${goos}-1${ext}"
  out2="${workdir}/${goos}-2${ext}"

  echo "-- ${goos}: building twice --------------------------------------------"
  echo "   GOOS=${goos} CGO_ENABLED=${cgo} -trimpath ${buildmode} -tags ${tags}"
  echo "   ldflags: ${ldflags}"

  # shellcheck disable=SC2086 # buildmode is one word or empty, deliberately
  GOOS="$goos" CGO_ENABLED="$cgo" go build -trimpath ${buildmode} -tags "$tags" -ldflags "$ldflags" -o "$out1" .
  # shellcheck disable=SC2086
  GOOS="$goos" CGO_ENABLED="$cgo" go build -trimpath ${buildmode} -tags "$tags" -ldflags "$ldflags" -o "$out2" .

  sum1="$(sha256_of "$out1")"
  sum2="$(sha256_of "$out2")"
  size="$(wc -c <"$out1" | tr -d ' ')"

  echo "   build 1: ${sum1}"
  echo "   build 2: ${sum2}"
  echo "   size   : ${size} bytes"

  if [ "$sum1" != "$sum2" ]; then
    echo "NOT REPRODUCIBLE: the two ${goos} builds differ." >&2
    if command -v cmp >/dev/null 2>&1; then
      cmp "$out1" "$out2" || true
    fi
    status=1
  else
    echo "   OK: byte-identical (sha256 ${sum1})"
  fi
  echo
done

if [ "$status" -ne 0 ]; then
  echo "FAILED" >&2
  exit "$status"
fi

echo "OK: every requested target is byte-identical across two builds."
echo
echo "Reminder about what that does and does not mean: on linux this is a"
echo "same-machine, same-C-toolchain property, because the binary links GTK3"
echo "and WebKitGTK. On windows it is CGO_ENABLED=0 and the claim is the"
echo "strong one. docs/release.md has the measurements."
