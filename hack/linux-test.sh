#!/usr/bin/env bash
#
# linux-test.sh - run Go tests inside a Linux container that has a real apt.
#
# The builder half of debark only has to be correct where apt lives, and the
# development machine is often Windows or macOS. This runs the test binaries
# where the oracle is.
#
#   hack/linux-test.sh                       # every package
#   hack/linux-test.sh ./core/apt/...        # one package
#   DEBARK_IMAGE=golang:1.26-trixie hack/linux-test.sh ./core/apt/...
#   DEBARK_E2E=1 hack/linux-test.sh ./core/apt/...   # include apt-backed tests
#
# Module and build caches live in named volumes, so only the first run pays
# for downloads.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
image="${DEBARK_IMAGE:-golang:1.26-bookworm}"
pkgs=("$@")
[ ${#pkgs[@]} -eq 0 ] && pkgs=("./...")

# On Windows (Git Bash / MSYS) docker needs the path left alone.
export MSYS_NO_PATHCONV=1

# Turn a Windows path into something docker -v accepts.
mount_src="$repo_root"
case "$mount_src" in
    /[a-z]/*) drive="${mount_src:1:1}"; mount_src="${drive}:${mount_src:2}" ;;
esac

docker volume create debark-gomod   >/dev/null
docker volume create debark-gocache >/dev/null

exec docker run --rm \
    -v "${mount_src}:/src" \
    -v debark-gomod:/go/pkg/mod \
    -v debark-gocache:/root/.cache/go-build \
    -w /src \
    -e "DEBARK_E2E=${DEBARK_E2E:-}" \
    -e CGO_ENABLED=0 \
    -e GOFLAGS=-mod=mod \
    "$image" \
    go test "${DEBARK_TESTFLAGS:--count=1}" "${pkgs[@]}"
