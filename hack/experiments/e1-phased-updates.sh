#!/usr/bin/env bash
#
# e1-phased-updates.sh — E1: does APT::Machine-ID in a private apt root
# reproduce the phasing decision the TARGET's apt would make?
#
# Settles ADR-006.
#
# Method:
#   1. Find a package currently phasing in an Ubuntu -updates pocket by
#      searching the *real* Packages index for "Phased-Update-Percentage"
#      (never guess a package name — the phasing set changes weekly).
#   2. Really `apt-get install` the package's older, non-phased fallback
#      version into the container's actual root, so we get a dependency-
#      -consistent dpkg status (a synthetic one-line status makes `apt-get
#      upgrade` refuse to touch the package for unrelated reasons and gives
#      a false reading — see the E1 findings doc, "methodology pitfall").
#   3. Copy that status into a private apt root built exactly as debark
#      builds one (explicit Dir::* options, never Dir= wholesale).
#   4. Resolve with apt-get -s under a matrix of:
#        - command: `upgrade` / `full-upgrade` (automatic, no package named)
#                    vs `install <pkg>` (package named explicitly, as
#                    debark's own "install --download-only requested
#                    packages" step does — see core/apt/iface.go ResolveInput)
#        - policy:  APT::Machine-ID=<id-A>, =<id-B> (different ids), and
#                   APT::Get::Never-Include-Phased-Updates=true
#      recording which candidate version apt selects each time, plus a
#      27-way machine-id scan under `upgrade -s` to show the split directly.
#
# Run on a Debian/Ubuntu host with apt-get, or let it re-exec itself in a
# container:
#
#   MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd)":/src -w /src \
#       ubuntu:24.04 bash hack/experiments/e1-phased-updates.sh
#
# or simply:
#
#   ./hack/experiments/e1-phased-updates.sh --docker [--image ubuntu:24.04]
#
# NOTE: step 2 runs a REAL (non-simulated) `apt-get install` — inside the
# container's own root, which is discarded with --rm. It installs one small
# package (and its deps) for real so its dpkg Status: stanza is realistic.
#
# Writes raw evidence to hack/experiments/out/e1/ and a summary to stdout.
set -uo pipefail

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="ubuntu:24.04"
USE_DOCKER=0
RELEASE="noble"
MIRROR="http://archive.ubuntu.com/ubuntu"
SEC_MIRROR="http://security.ubuntu.com/ubuntu"
COMPONENTS="main restricted universe multiverse"
ARCH="amd64"
OUT="$SELF/out/e1"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --docker) USE_DOCKER=1; shift ;;
        --image) IMAGE="$2"; USE_DOCKER=1; shift 2 ;;
        --release) RELEASE="$2"; shift 2 ;;
        -h|--help) grep '^#' "$0" | sed 's/^#//'; exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 1 ;;
    esac
done

if [ "$USE_DOCKER" = 1 ]; then
    echo "==> re-running inside $IMAGE"
    exec docker run --rm -v "$SELF/../..":/src -w /src "$IMAGE" \
        bash hack/experiments/e1-phased-updates.sh --release "$RELEASE"
fi

command -v apt-get >/dev/null || { echo "need apt-get; use --docker" >&2; exit 1; }

mkdir -p "$OUT"
: > "$OUT/summary.txt"
log() { printf '%s\n' "$*" | tee -a "$OUT/summary.txt"; }

log "=== E1 phased updates ==="
log "date (UTC):    $(date -u +%Y-%m-%dT%H:%M:%SZ)"
log "apt version:   $(apt-get -v | head -1)"
log "os-release:    $(. /etc/os-release; echo "$PRETTY_NAME")"
log "release used:  $RELEASE"

# ---------------------------------------------------------------------------
# 1. Populate the AMBIENT container root's lists (needed later for the real
#    "install the fallback version" step) with a plain, ordinary update.
# ---------------------------------------------------------------------------
apt-get update -qq > "$OUT/host-update.log" 2>&1

# ---------------------------------------------------------------------------
# 2. Build the private apt root and update IT, asking for plain-text
#    indexes from the very first fetch (apt will happily keep serving a
#    cached .lz4 copy on a *second* update against the same sources even if
#    you ask for gz/plain afterwards, because the content hash already
#    matches — so this option must be present on the FIRST update or it is a
#    no-op; ask for it from the start rather than re-updating).
# ---------------------------------------------------------------------------
ROOT="$OUT/aptroot"
rm -rf "$ROOT"
mkdir -p "$ROOT"/etc/apt/{sources.list.d,preferences.d,apt.conf.d,trusted.gpg.d} \
         "$ROOT"/var/lib/apt/lists/partial "$ROOT"/var/lib/dpkg \
         "$ROOT"/var/log/apt "$ROOT"/var/cache/apt/archives/partial
: > "$ROOT/etc/apt/sources.list"
: > "$ROOT/etc/apt/preferences"

cat > "$ROOT/etc/apt/sources.list.d/ubuntu.sources" <<EOF
Types: deb
URIs: $MIRROR
Suites: $RELEASE $RELEASE-updates $RELEASE-backports
Components: $COMPONENTS
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg

Types: deb
URIs: $SEC_MIRROR
Suites: $RELEASE-security
Components: $COMPONENTS
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
EOF
cp -a /etc/apt/trusted.gpg.d/. "$ROOT/etc/apt/trusted.gpg.d/" 2>/dev/null || true

APT_BASE=(
    -o "Dir::Etc::sourcelist=$ROOT/etc/apt/sources.list"
    -o "Dir::Etc::sourceparts=$ROOT/etc/apt/sources.list.d"
    -o "Dir::Etc::preferences=$ROOT/etc/apt/preferences"
    -o "Dir::Etc::preferencesparts=$ROOT/etc/apt/preferences.d"
    -o "Dir::Etc::trustedparts=$ROOT/etc/apt/trusted.gpg.d"
    -o "Dir::State=$ROOT/var/lib/apt"
    -o "Dir::State::status=$ROOT/var/lib/dpkg/status"
    -o "Dir::Cache=$ROOT/var/cache/apt"
    -o "Dir::Log=$ROOT/var/log/apt"
    -o "APT::Architecture=$ARCH"
    -o "APT::Architectures::=$ARCH"
    -o "Acquire::Languages=none"
    -o "APT::Sandbox::User=root"
    -o "Debug::NoLocking=1"
    # Plain-text indexes so this script can grep them directly.
    -o "Acquire::GzipIndexes=false"
    -o "Acquire::CompressionTypes::Order::=gz"
)
: > "$ROOT/var/lib/dpkg/status"
apt-get "${APT_BASE[@]}" update -qq > "$OUT/root-update.log" 2>&1
if [ $? -ne 0 ]; then
    log "FATAL: apt-get update failed in the private root:"
    tail -20 "$OUT/root-update.log" | tee -a "$OUT/summary.txt"
    exit 1
fi

IDXFILE=$(find "$ROOT/var/lib/apt/lists" -name "*${RELEASE}-updates_main_binary-${ARCH}_Packages" | head -1)
[ -n "$IDXFILE" ] && [ -f "$IDXFILE" ] || { log "FATAL: plain-text index not found under $ROOT/var/lib/apt/lists"; ls "$ROOT/var/lib/apt/lists" | tee -a "$OUT/summary.txt"; exit 1; }

PHASED_LIST="$OUT/phased-candidates.txt"
awk 'BEGIN{RS=""; FS="\n"} /Phased-Update-Percentage:/ {
    pkg=""; ver=""; pct="";
    for (i=1;i<=NF;i++) {
        if ($i ~ /^Package: /)  { pkg=$i; sub(/^Package: /,"",pkg) }
        if ($i ~ /^Version: /)  { ver=$i; sub(/^Version: /,"",ver) }
        if ($i ~ /^Phased-Update-Percentage: /) { pct=$i; sub(/^Phased-Update-Percentage: /,"",pct) }
    }
    if (pkg != "" && pct != "" && pct+0 > 0 && pct+0 < 100) print pkg"\t"ver"\t"pct
}' "$IDXFILE" | sort > "$PHASED_LIST"

N_PHASED=$(wc -l < "$PHASED_LIST" | tr -d ' ')
log "found $N_PHASED partially-phased package(s) in $RELEASE-updates/main; full list in phased-candidates.txt"
[ "$N_PHASED" -gt 0 ] || { log "FATAL: no partially-phased package found right now — rerun later"; exit 1; }

PKG=$(cut -f1 "$PHASED_LIST" | head -1)
PHASED_VER=$(awk -F'\t' -v p="$PKG" '$1==p{print $2}' "$PHASED_LIST")
PCT=$(awk -F'\t' -v p="$PKG" '$1==p{print $3}' "$PHASED_LIST")
log "chosen package: $PKG  phased_version=$PHASED_VER  percentage=$PCT%"

apt-cache madison "$PKG" > "$OUT/madison.txt" 2>&1
log "" ; log "-- apt-cache madison $PKG --"; cat "$OUT/madison.txt" | tee -a "$OUT/summary.txt"
FALLBACK_VER=$(awk -F'|' 'NR==2{gsub(/^[ \t]+|[ \t]+$/,"",$2); print $2}' "$OUT/madison.txt")
log "fallback (non-phased) version: ${FALLBACK_VER:-<none found — aborting>}"
[ -n "$FALLBACK_VER" ] || exit 1

# ---------------------------------------------------------------------------
# 3. Get a REALISTIC, dependency-consistent dpkg status: really install the
#    fallback version into the container's own (throwaway, ambient) root,
#    then drop that status file into the private ROOT. (Changing the dpkg
#    status does not require re-running `apt-get update` — that only
#    refreshes package indexes from remote sources — so ROOT keeps the
#    indexes it already fetched in step 2.)
# ---------------------------------------------------------------------------
log ""
log "==> apt-get install -y $PKG=$FALLBACK_VER (REAL install, ambient container root, for a consistent status file)"
apt-get install -y -qq "$PKG=$FALLBACK_VER" > "$OUT/real-install.log" 2>&1
REALSTATUS="$OUT/real-status"
cp -a /var/lib/dpkg/status "$REALSTATUS"
log "    dpkg status now has $(grep -c '^Package:' "$REALSTATUS") installed package stanzas"
cp -a "$REALSTATUS" "$ROOT/var/lib/dpkg/status"

# Two fixed, deterministic machine-ids (sha256 of fixed seeds truncated to 32
# hex chars, like a real /etc/machine-id) that this script has already found
# land on opposite sides of the phasing cut for the FIRST package this finds
# at ~40%; if today's chosen package/percentage differs, the later scan (step
# 5) is what actually proves the split either way.
MID_A="$(printf 'debark-e1-alpha' | sha256sum | cut -c1-32)"
MID_B="$(printf 'debark-e1-delta' | sha256sum | cut -c1-32)"

# Classify one apt-get -s transcript for $PKG.
classify() {
    local logf="$1"
    if grep -q "Inst $PKG .*$PHASED_VER" "$logf"; then
        echo "SELECTED (phased $PHASED_VER)"
    elif grep -qE "^Conf $PKG \($PHASED_VER" "$logf"; then
        echo "SELECTED (phased $PHASED_VER)"
    elif grep -q "$PKG$" "$logf" && grep -qE "[0-9]+ not upgraded\." "$logf"; then
        echo "kept-back ($FALLBACK_VER)"
    elif grep -q "^0 upgraded, 0 newly installed" "$logf"; then
        echo "no-op ($FALLBACK_VER, already newest offered)"
    else
        echo "unclear — inspect $logf"
    fi
}

run() {
    local label="$1" logf="$2"; shift 2
    apt-get "${APT_BASE[@]}" "$@" > "$logf" 2>&1
    printf '%-42s -> %s\n' "$label" "$(classify "$logf")" | tee -a "$OUT/summary.txt"
}

# ---------------------------------------------------------------------------
# 4. The command x policy matrix.
# ---------------------------------------------------------------------------
log ""
log "=== matrix: command x phasing policy, for $PKG (installed $FALLBACK_VER -> candidate $PHASED_VER, ${PCT}% phased) ==="
log "machine-id A = $MID_A   machine-id B = $MID_B"
log ""
log "-- apt-get upgrade -s (automatic, no package named) --"
run "  machine-id A"                   "$OUT/m-upgrade-A.log"          -o "APT::Machine-ID=$MID_A" upgrade -s
run "  machine-id B"                   "$OUT/m-upgrade-B.log"          -o "APT::Machine-ID=$MID_B" upgrade -s
run "  Never-Include-Phased-Updates"   "$OUT/m-upgrade-never.log"      -o "APT::Get::Never-Include-Phased-Updates=true" upgrade -s

log ""
log "-- apt-get full-upgrade -s (automatic, no package named — what the --upgrades pass runs) --"
run "  machine-id A"                   "$OUT/m-fullupgrade-A.log"      -o "APT::Machine-ID=$MID_A" full-upgrade -s
run "  machine-id B"                   "$OUT/m-fullupgrade-B.log"      -o "APT::Machine-ID=$MID_B" full-upgrade -s
run "  Never-Include-Phased-Updates"   "$OUT/m-fullupgrade-never.log"  -o "APT::Get::Never-Include-Phased-Updates=true" full-upgrade -s

log ""
log "-- apt-get install -s $PKG (EXPLICIT name — what the requested-packages pass runs) --"
run "  machine-id A"                   "$OUT/m-install-A.log"          -o "APT::Machine-ID=$MID_A" install -s "$PKG"
run "  machine-id B"                   "$OUT/m-install-B.log"          -o "APT::Machine-ID=$MID_B" install -s "$PKG"
run "  Never-Include-Phased-Updates"   "$OUT/m-install-never.log"      -o "APT::Get::Never-Include-Phased-Updates=true" install -s "$PKG"

# ---------------------------------------------------------------------------
# 5. Machine-id scan under `upgrade -s`: direct proof of a split (or not).
# ---------------------------------------------------------------------------
log ""
log "=== scanning 27 deterministic machine-ids under 'apt-get upgrade -s' ==="
SEEDS=(alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu omega)
N_SEL=0; N_KEPT=0
for s in "${SEEDS[@]}"; do
    mid="$(printf 'debark-e1-%s' "$s" | sha256sum | cut -c1-32)"
    logf="$OUT/scan-$s.log"
    apt-get "${APT_BASE[@]}" -o "APT::Machine-ID=$mid" upgrade -s > "$logf" 2>&1
    v="$(classify "$logf")"
    case "$v" in
        SELECTED*) N_SEL=$((N_SEL+1)) ;;
        kept-back*) N_KEPT=$((N_KEPT+1)) ;;
    esac
    printf '  %-10s %s -> %s\n' "$s" "$mid" "$v" | tee -a "$OUT/summary.txt"
done
log ""
log "scan result: $N_SEL/${#SEEDS[@]} selected the phased version, $N_KEPT/${#SEEDS[@]} kept it back (index says ${PCT}% should select it)"
if [ "$N_SEL" -gt 0 ] && [ "$N_KEPT" -gt 0 ]; then
    log "-> CONFIRMED: APT::Machine-ID changes the automatic-upgrade phasing outcome for $PKG."
else
    log "-> NOT CONFIRMED this run: all $((N_SEL+N_KEPT)) sampled ids landed on the same side. Rerun with more seeds or a higher-percentage package."
fi

log ""
log "raw logs and dpkg status: $OUT/   phased candidates: $PHASED_LIST"
log "=== E1 done ==="
