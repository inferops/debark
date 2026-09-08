#!/usr/bin/env bash
#
# demo-airgap.sh - the whole product, end to end, across a simulated air gap.
#
# Stage 1 (offline target):  snapshot create
# Stage 2 (online builder):  build + sign
# Stage 3 (offline target):  verify + install, with --network none, then run
#                            the installed binary
#
# The operator public key travels OUT OF BAND (copied to the target
# separately), never on the media, because a key found on the same media
# proves nothing.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
case "$repo" in /[a-z]/*) drive="${repo:1:1}"; mount="${drive}:${repo:2}" ;; *) mount="$repo" ;; esac
export MSYS_NO_PATHCONV=1

# The work directory lives inside the repo (gitignored) so that one path
# works for the Go toolchain, for bash and for docker -v on every platform.
work="${DEBARK_DEMO_OUT:-$repo/.demo}"
rm -rf "$work"; mkdir -p "$work"
case "$work" in /[a-z]/*) d="${work:1:1}"; wmount="${d}:${work:2}" ;; *) wmount="$work" ;; esac

img=debian:bookworm-slim
say() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }

say "building a static linux binary"
# Build to a path RELATIVE to the repo root. An absolute path is a trap here:
# a native Windows Go toolchain cannot read an MSYS /d/... path, and Git Bash
# rewrites a d:/... path back into /d/... on the way to the binary, so the
# build silently lands somewhere neither side expects. A relative path is
# converted by nobody.
( cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ".demo/debark" ./cmd/debark )
# $work is the same directory as $wmount, in the dialect this shell speaks;
# the Go toolchain and docker need $wmount's drive-letter form instead.
ls -la "$work/debark"

say "stage 1: capture the offline target's state"
docker run --rm -v "$wmount:/w" "$img" \
    /w/debark snapshot create --out /w/target.snapshot.tar.zst

say "stage 2: build a signed bundle on the online builder"
docker run --rm -v "$wmount:/w" "$img" sh -c '
    set -e
    /w/debark keygen --out /w/operator.key --comment "demo operator key"
    /w/debark build --snapshot /w/target.snapshot.tar.zst \
        --out /w/bundle --sign /w/operator.key jq
'

say "what crossed the gap"
find "$work/bundle" -type f | sed "s|$work/bundle|<bundle>|" | sort

say "stage 3: verify and install on the offline target, with NO NETWORK"
# The bundle goes on the "media"; the public key is handed over separately.
docker run --rm --network none -v "$wmount:/w" "$img" sh -c '
    set -e
    mkdir -p /media /keys
    cp -r /w/bundle /media/bundle
    cp /w/operator.pub /keys/                # out of band, NOT on the media
    /w/debark verify /media/bundle --key /keys/operator.pub
    /w/debark install /media/bundle --key /keys/operator.pub --yes
    echo "--- proving the installed software actually runs ---"
    jq --version
    echo "{\"air\":\"gap\"}" | jq -r .air
'

say "done"
