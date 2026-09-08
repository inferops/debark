#!/usr/bin/env bash
#
# e8-base-fidelity.sh — E8: how close is the resolved closure of a base
# definition's seed metapackages to what a real install of that release
# actually has?
#
# Settles the question `debark snapshot from-base` turns on (ADR-014, a
# scope addition made 2026-09-06). A
# synthesized snapshot is an ASSUMPTION about a machine that may not exist
# yet; a bundle built from it is complete only if the target really is a stock
# install of that release. Nobody had measured how good that assumption is.
#
# The two directions are NOT equally dangerous, and the whole point of this
# experiment is to report them separately:
#
#   ASSUMED-BUT-ABSENT  the base claims a package the real install does not
#                       have. The bundle omits it, the target's apt cannot
#                       satisfy the dependency, and the install fails at the
#                       far side of the air gap. This is the ONLY direction
#                       that can hurt anyone, and core/base's rule is that it
#                       must be empty. If it is not, the base definition
#                       shrinks until it is.
#
#   ABSENT-BUT-PRESENT  the real install has a package the base did not claim.
#                       The bundle carries it needlessly and is bigger than it
#                       had to be. Harmless; the Bash prototype shipped this
#                       behaviour deliberately ("the bundle stays correct — it
#                       is a superset — but it is much larger").
#
# Method (three stages):
#
#   1. CLOSURE. Run `debark snapshot from-base <base> --backend local` for
#      each variant and read the resulting snapshot's origin.assumed_installed
#      back out with `snapshot inspect --json`. That is the real product code
#      path, not a re-implementation of it: apt resolves the seeds in a
#      private root where nothing is installed, exactly as a from-base run
#      does for an operator.
#
#   2. GROUND TRUTH. Two independent sources, because neither covers every
#      variant:
#        - Canonical publishes a .manifest beside each ISO on
#          releases.ubuntu.com listing every package that image installs.
#          That is the authoritative answer for the server and desktop
#          variants, and it is a fact about a real image rather than about
#          anything debark did.
#        - For minimal there is no ISO, so the ground truth is the dpkg
#          database of a REAL INSTALLED SYSTEM of that release — whatever
#          machine this script runs on, when it matches. `dpkg-query -W` on a
#          live system is as real as evidence gets.
#
#   3. COMPARE, by package NAME. Versions are deliberately ignored: a target
#      running a different version of an assumed package still HAS it, so the
#      bundle is not short, and counting that as divergence would bury the one
#      direction that matters under ordinary patching noise.
#
# Usage:
#   ./hack/experiments/e8-base-fidelity.sh
#   ./hack/experiments/e8-base-fidelity.sh --release 24.04
#   ./hack/experiments/e8-base-fidelity.sh --binary /path/to/debark
#   ./hack/experiments/e8-base-fidelity.sh --no-network   # closure only
#
# Requires a Linux host with apt whose distro AND version match the release
# being measured — the local backend is refused otherwise, and correctly so
# (core/base's gateTarget explains why). Needs real network access to the
# distro archive and, unless --no-network, to releases.ubuntu.com.
#
# Writes raw evidence under hack/experiments/out/e8/ and a running summary to
# stdout + hack/experiments/out/e8/summary.txt.

set -uo pipefail

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF/../.." && pwd)"
REPO_REL_OUT="hack/experiments/out/e8"
OUT="$SELF/out/e8"

RELEASE=""
BINARY=""
DO_NETWORK=1
VARIANTS=(minimal server desktop)

export MSYS_NO_PATHCONV=1
die() { echo "FATAL: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --release) RELEASE="$2"; shift 2 ;;
        --binary) BINARY="$2"; shift 2 ;;
        --no-network) DO_NETWORK=0; shift ;;
        -h|--help) grep '^#' "$0" | sed 's/^#//'; exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 1 ;;
    esac
done

command -v dpkg-query >/dev/null || die "dpkg-query is required (this must run on a Debian/Ubuntu system)"
command -v apt-get >/dev/null || die "apt-get is required; the local backend resolves the seeds"

# The host must BE the release under test. This is not a convenience: the
# backend gate refuses the local backend when it cannot confirm the host apt's
# solver matches the target's, and for a base -- a machine that does not exist
# -- the only case where it can is when this host is that same release
# (core/base's gateTarget). Measuring 22.04 from a 24.04 host would measure
# the wrong apt.
HOST_ID="$(. /etc/os-release && echo "$ID")"
HOST_VERSION="$(. /etc/os-release && echo "$VERSION_ID")"
HOST_CODENAME="$(. /etc/os-release && echo "${VERSION_CODENAME:-}")"
[ -n "$RELEASE" ] || RELEASE="$HOST_VERSION"
if [ "$RELEASE" != "$HOST_VERSION" ]; then
    die "this host is $HOST_ID $HOST_VERSION but --release $RELEASE was asked for; run this on a $RELEASE host (see the header)"
fi
[ "$HOST_ID" = "ubuntu" ] || die "only the Ubuntu bases have a published ISO manifest to compare against; this host is $HOST_ID"

if [ -z "$BINARY" ]; then
    BINARY="$OUT/debark"
    mkdir -p "$OUT"
    (cd "$REPO_ROOT" && go build -o "$BINARY" ./cmd/debark) || die "go build failed"
fi
[ -x "$BINARY" ] || die "$BINARY is not executable"

mkdir -p "$OUT"
: > "$OUT/summary.txt"
log() { printf '%s\n' "$*" | tee -a "$OUT/summary.txt"; }

log "=== E8 base fidelity ==="
log "date (UTC): $(date -u +%Y-%m-%dT%H:%M:%SZ)"
log "host:       $HOST_ID $HOST_VERSION ($HOST_CODENAME) $(dpkg --print-architecture)"
log "apt:        $(apt-get -v | head -1)"
log "debark:     $("$BINARY" version --json | tr -d '\n' | sed 's/  */ /g' | cut -c1-160)"
log ""

# --- stage 1: the closure of each base's seeds ----------------------------

closure_file() { echo "$OUT/closure-$1.txt"; }

for v in "${VARIANTS[@]}"; do
    base="ubuntu:$RELEASE/$v"
    snap="$OUT/$v.snapshot.tar.zst"
    rm -f "$snap"
    log "--- closure: $base ---"
    if ! "$BINARY" snapshot from-base "$base" --backend local --out "$snap" \
            > "$OUT/from-base-$v.log" 2>&1; then
        log "  FAILED (see $REPO_REL_OUT/from-base-$v.log)"
        tail -3 "$OUT/from-base-$v.log" | sed 's/^/    /' | tee -a "$OUT/summary.txt"
        : > "$(closure_file "$v")"
        continue
    fi
    # origin.assumed_installed is name:arch; the comparison is by name.
    "$BINARY" snapshot inspect "$snap" --json > "$OUT/inspect-$v.json" 2>/dev/null
    python3 - "$OUT/inspect-$v.json" > "$(closure_file "$v")" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
names = {e.split(':', 1)[0] for e in doc.get('origin', {}).get('assumed_installed', [])}
print('\n'.join(sorted(names)))
PY
    log "  $(wc -l < "$(closure_file "$v")") packages assumed"
done
log ""

# --- stage 2: ground truth -------------------------------------------------

truth_file() { echo "$OUT/truth-$1.txt"; }

# minimal: this machine's own dpkg database. A real installed system.
dpkg-query -W -f '${Package}\n' 2>/dev/null | sort -u > "$(truth_file minimal)"
log "--- ground truth: minimal = this machine's dpkg database ---"
log "  $(wc -l < "$(truth_file minimal)") packages installed on this host"

if [ "$DO_NETWORK" = 1 ]; then
    command -v curl >/dev/null || die "curl is required for the ISO manifests (or pass --no-network)"
    # releases.ubuntu.com publishes a .manifest beside each image. The point
    # release in the file name moves, so the index is scraped rather than a
    # version guessed -- a guessed URL that 404s would silently produce an
    # empty ground truth and a flattering result.
    idx="$OUT/releases-index.html"
    curl -fsSL "https://releases.ubuntu.com/$RELEASE/" -o "$idx" || log "  WARNING: could not fetch the release index"
    for v in server desktop; do
        pat="live-$v-amd64.manifest"
        [ "$v" = "desktop" ] && pat="desktop-amd64.manifest"
        name="$(grep -o "ubuntu-[0-9.]*-$pat" "$idx" 2>/dev/null | sort -u | tail -1)"
        if [ -z "$name" ]; then
            log "--- ground truth: $v = NOT AVAILABLE (no $pat in the release index) ---"
            : > "$(truth_file "$v")"
            continue
        fi
        url="https://releases.ubuntu.com/$RELEASE/$name"
        if curl -fsSL "$url" -o "$OUT/$v.manifest"; then
            awk '{print $1}' "$OUT/$v.manifest" | sed 's/:.*//' | sort -u > "$(truth_file "$v")"
            log "--- ground truth: $v = $name ---"
            log "  $(wc -l < "$(truth_file "$v")") packages in the published image manifest"
        else
            log "--- ground truth: $v = FETCH FAILED ($url) ---"
            : > "$(truth_file "$v")"
        fi
    done
else
    for v in server desktop; do : > "$(truth_file "$v")"; done
    log "--- ground truth: server/desktop skipped (--no-network) ---"
fi
log ""

# --- stage 3: compare ------------------------------------------------------

log "--- results ---"
overall=PASS
for v in "${VARIANTS[@]}"; do
    c="$(closure_file "$v")"; t="$(truth_file "$v")"
    if [ ! -s "$c" ]; then log "$v: SKIPPED (no closure)"; overall=INCOMPLETE; continue; fi
    if [ ! -s "$t" ]; then log "$v: SKIPPED (no ground truth)"; overall=INCOMPLETE; continue; fi

    comm -23 "$c" "$t" > "$OUT/assumed-but-absent-$v.txt"
    comm -13 "$c" "$t" > "$OUT/absent-but-present-$v.txt"
    dangerous=$(wc -l < "$OUT/assumed-but-absent-$v.txt")
    harmless=$(wc -l < "$OUT/absent-but-present-$v.txt")
    nclosure=$(wc -l < "$c"); ntruth=$(wc -l < "$t")

    verdict=PASS
    [ "$dangerous" -eq 0 ] || { verdict=FAIL; overall=FAIL; }
    log "$v: $verdict  closure=$nclosure  real=$ntruth  ASSUMED-BUT-ABSENT=$dangerous (dangerous)  ABSENT-BUT-PRESENT=$harmless (harmless)"
    if [ "$dangerous" -gt 0 ]; then
        log "    first 20 assumed-but-absent:"
        head -20 "$OUT/assumed-but-absent-$v.txt" | sed 's/^/      /' | tee -a "$OUT/summary.txt" >/dev/null
        head -20 "$OUT/assumed-but-absent-$v.txt" | sed 's/^/      /'
    fi
    printf 'RESULT=%s dangerous=%s harmless=%s closure=%s real=%s\n' \
        "$verdict" "$dangerous" "$harmless" "$nclosure" "$ntruth" > "$OUT/result-$v.txt"
done

log ""
log "overall: $overall"
log "raw evidence tree: $REPO_REL_OUT/"
log "=== E8 done ==="
