#!/usr/bin/env bash
#
# e5-container-io.sh — E5: bind mount vs named volume I/O for a store
# directory on Docker Desktop for Windows.
#
# Settles the open question:
# "Container I/O performance on shared volumes is an open experiment (E5);
# the store lives in a named volume if a bind mount proves slow."
#
# Method: write, then read back, a few hundred MB spread across a few
# hundred small-to-medium files (like a store/pool of .deb files) inside a
# container, once with the target directory bind-mounted from a native
# Windows path (D:\...) and once with it backed by a Docker named volume.
# Same file-size manifest both times (deterministic, not random per run) so
# the comparison is apples to apples. Repeats each N times and reports
# min/median, since the first run of any mount type is usually slower (cold
# cache / lazy allocation).
#
# Run from Git Bash on Windows (uses MSYS_NO_PATHCONV-style d:/ mounts):
#   ./hack/experiments/e5-container-io.sh
#
# Writes raw evidence to hack/experiments/out/e5/ and a summary to stdout.
set -uo pipefail

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="ubuntu:24.04"
N_FILES=300
MIN_KB=200
MAX_KB=2200
REPEATS=3
OUT="$SELF/out/e5"
VOLUME="debark-e5-store"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --image) IMAGE="$2"; shift 2 ;;
        --files) N_FILES="$2"; shift 2 ;;
        --repeats) REPEATS="$2"; shift 2 ;;
        -h|--help) grep '^#' "$0" | sed 's/^#//'; exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 1 ;;
    esac
done

command -v docker >/dev/null || { echo "docker not found on PATH" >&2; exit 1; }

mkdir -p "$OUT"
: > "$OUT/summary.txt"
log() { printf '%s\n' "$*" | tee -a "$OUT/summary.txt"; }

log "=== E5 container backend I/O: bind mount vs named volume ==="
log "date (local):   $(date)"
log "docker version: $(docker version --format '{{.Server.Version}} ({{.Server.Os}}/{{.Server.Arch}})' 2>/dev/null || docker --version)"
log "host:           $(uname -s 2>/dev/null || echo Windows) via Git Bash / MSYS"
log "image:          $IMAGE"
log "files:          $N_FILES, sizes ${MIN_KB}KB-${MAX_KB}KB, repeats=$REPEATS"

# Deterministic size manifest (not random — reproducible across runs and
# between the two mount types).
SIZES_FILE="$OUT/sizes.txt"
: > "$SIZES_FILE"
for i in $(seq 1 "$N_FILES"); do
    echo $(( (i * 37 + i * i % 53) % (MAX_KB - MIN_KB) + MIN_KB )) >> "$SIZES_FILE"
done
TOTAL_KB=$(awk '{s+=$1} END{print s}' "$SIZES_FILE")
log "planned total payload: ${TOTAL_KB} KB (~$(awk -v k="$TOTAL_KB" 'BEGIN{printf "%.1f", k/1024}') MB)"

# The benchmark script that runs INSIDE the container. Reads /sizes.txt,
# writes that many files of those sizes into $1, times write and read.
cat > "$OUT/bench-inner.sh" <<'INNER'
#!/bin/sh
set -eu
dir="$1"
rm -rf "$dir"/e5-* 2>/dev/null || true
mkdir -p "$dir"

t0=$(date +%s%N)
i=0
while read -r kb; do
    i=$((i+1))
    dd if=/dev/zero of="$dir/e5-pkg_$i.deb" bs=1024 count="$kb" 2>/dev/null
done < /sizes.txt
sync
t1=$(date +%s%N)
write_ns=$((t1 - t0))

t0=$(date +%s%N)
find "$dir" -maxdepth 1 -name 'e5-pkg_*.deb' -exec cat {} \; > /dev/null
t1=$(date +%s%N)
read_ns=$((t1 - t0))

bytes=$(du -sb "$dir" 2>/dev/null | cut -f1 || echo 0)
rm -f "$dir"/e5-*.deb
echo "WRITE_NS=$write_ns READ_NS=$read_ns BYTES=$bytes"
INNER
chmod +x "$OUT/bench-inner.sh"

fmt_result() {
    # $1 = "WRITE_NS=... READ_NS=... BYTES=..."
    local WRITE_NS READ_NS BYTES
    eval "$1"
    awk -v w="$WRITE_NS" -v r="$READ_NS" -v b="$BYTES" 'BEGIN {
        printf "write: %.2fs (%.1f MB/s)   read: %.2fs (%.1f MB/s)   bytes=%d\n", \
            w/1e9, (b/1048576)/(w/1e9), r/1e9, (b/1048576)/(r/1e9), b
    }'
}

run_case() {
    local label="$1" tag="$2" mount_args="$3" n
    log ""
    log "-- $label --"
    for n in $(seq 1 "$REPEATS"); do
        local out
        out=$(docker run --rm \
            -v "$SELF/out/e5/bench-inner.sh":/bench-inner.sh \
            -v "$SELF/out/e5/sizes.txt":/sizes.txt \
            $mount_args \
            "$IMAGE" sh /bench-inner.sh /store 2>>"$OUT/errors.log")
        echo "$out" > "$OUT/raw-${tag}-run$n.txt"
        printf '  run %d: %s -> %s\n' "$n" "$out" "$(fmt_result "$out")" | tee -a "$OUT/summary.txt"
    done
}

# --- Case A: bind mount from a native Windows path (D:\...) ---
BIND_DIR="$OUT/bind-store"
rm -rf "$BIND_DIR"; mkdir -p "$BIND_DIR"
MSYS_NO_PATHCONV=1 run_case "bind mount (Windows D:\\ path)" "bind" "-v $SELF/out/e5/bind-store:/store"

# --- Case B: Docker named volume ---
docker volume rm "$VOLUME" >/dev/null 2>&1 || true
docker volume create "$VOLUME" >/dev/null
MSYS_NO_PATHCONV=1 run_case "named volume" "volume" "-v $VOLUME:/store"
docker volume rm "$VOLUME" >/dev/null 2>&1 || true

log ""
log "raw per-run output: $OUT/raw-*.txt   (WRITE_NS/READ_NS/BYTES, converted above)"
log "=== E5 done — see docs/experiments/E5-container-io.md for the summary table and recommendation ==="
