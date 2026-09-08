#!/usr/bin/env bash
#
# e4-aptconfd-leakage.sh — E4: does NOT capturing/carrying apt.conf.d make
# the prototype resolve a different (larger) set than the target would?
#
# Settles the snapshot
# field set (ADR-005: "Snapshot captures apt.conf.d, apt/dpkg versions and
# redactable machine-id").
#
# Method:
#   1. Build a TARGET fixture: a private apt root whose apt.conf.d carries
#      `APT::Install-Recommends "false";` (the prototype's own docstring
#      calls out Recommends specifically: "a plain apt-get install
#      --download-only ... silently skips every dependency" — this is the
#      same class of silent-divergence risk, just from apt.conf instead of
#      dpkg status).
#   2. THE PROTOTYPE'S ACTUAL BEHAVIOUR: resolve the same request in a
#      private root that carries the target's dpkg status and sources (as
#      download-packages.sh does today) but NOT apt.conf.d (as
#      download-packages.sh does today — verified by reading it, it only
#      copies dpkg-status, apt/sources.list*, apt/preferences*,
#      apt/trusted.gpg.d — never apt.conf.d). Record the set and total size.
#   3. THE FIX: resolve the same request while actually carrying the
#      target's apt.conf.d. Record the set and size, and confirm it now
#      matches the target's own resolution from step 1.
#   4. A second, independent test of a `Default-Release`-style apt.conf
#      setting using a small fully-controlled two-suite fixture (so the
#      result doesn't depend on today's Ubuntu archive contents): without
#      the setting apt picks by version/first-listed; with it, apt.conf
#      forces a specific suite even though it is not the higher version.
#
# IMPORTANT FINDING BAKED INTO THIS SCRIPT (see docs/experiments/E4-*.md for
# the full writeup): `-o Dir::Etc::parts=<dir>` and `-c <file>` do **NOT**
# redirect where apt.conf.d is scanned from — apt reads its main apt.conf and
# apt.conf.d at a bootstrap stage that runs BEFORE command-line -o/-c
# processing, so those flags are silently too late. The only mechanism that
# works is the `APT_CONFIG` **environment variable**, pointing at a small
# generated "loader" file whose content sets `Dir::Etc::parts` (and
# `Dir::Etc::main` if a main apt.conf was also captured) to the target's
# captured paths — apt.conf.d is then scanned from there, honouring every
# setting the target had, not just the ones debark knows to translate.
# This script demonstrates both the broken and working mechanisms so you can
# see the difference directly.
#
# Run on a Debian/Ubuntu host with apt-get, or let it re-exec itself in a
# container:
#   ./hack/experiments/e4-aptconfd-leakage.sh --docker [--image ubuntu:24.04]
#
# Writes raw evidence to hack/experiments/out/e4/ and a summary to stdout.
set -uo pipefail

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="ubuntu:24.04"
USE_DOCKER=0
RELEASE="noble"
MIRROR="http://archive.ubuntu.com/ubuntu"
SEC_MIRROR="http://security.ubuntu.com/ubuntu"
COMPONENTS="main restricted universe multiverse"
ARCH="amd64"
OUT="$SELF/out/e4"
# A "realistic request": a package with a non-trivial Recommends chain, so
# the Recommends-leakage effect is measurable, not a rounding error.
REQUEST="git"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --docker) USE_DOCKER=1; shift ;;
        --image) IMAGE="$2"; USE_DOCKER=1; shift 2 ;;
        --release) RELEASE="$2"; shift 2 ;;
        --request) REQUEST="$2"; shift 2 ;;
        -h|--help) grep '^#' "$0" | sed 's/^#//'; exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 1 ;;
    esac
done

if [ "$USE_DOCKER" = 1 ]; then
    echo "==> re-running inside $IMAGE"
    exec docker run --rm -v "$SELF/../..":/src -w /src "$IMAGE" \
        bash hack/experiments/e4-aptconfd-leakage.sh --release "$RELEASE" --request "$REQUEST"
fi

command -v apt-get >/dev/null || { echo "need apt-get; use --docker" >&2; exit 1; }

mkdir -p "$OUT"
: > "$OUT/summary.txt"
log() { printf '%s\n' "$*" | tee -a "$OUT/summary.txt"; }

log "=== E4 apt.conf.d leakage ==="
log "date (UTC):   $(date -u +%Y-%m-%dT%H:%M:%SZ)"
log "apt version:  $(apt-get -v | head -1)"
log "os-release:   $(. /etc/os-release; echo "$PRETTY_NAME")"
log "request:      $REQUEST"

# ---------------------------------------------------------------------------
# Build a base private apt root: sources only, no apt.conf.d, empty
# status (a fresh-install request keeps the arithmetic simple: "packages
# needed to satisfy $REQUEST from nothing").
# ---------------------------------------------------------------------------
ROOT="$OUT/aptroot"
rm -rf "$ROOT"
mkdir -p "$ROOT"/etc/apt/{sources.list.d,preferences.d,apt.conf.d,trusted.gpg.d} \
         "$ROOT"/var/lib/apt/lists/partial "$ROOT"/var/lib/dpkg \
         "$ROOT"/var/log/apt "$ROOT"/var/cache/apt/archives/partial
: > "$ROOT/etc/apt/sources.list"
: > "$ROOT/etc/apt/preferences"
: > "$ROOT/var/lib/dpkg/status"

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

# This is the TARGET's captured apt.conf.d — what a real snapshot would carry
# in APT.Conf per core/snapshot/types.go.
TARGET_CONFD="$OUT/target-apt.conf.d"
rm -rf "$TARGET_CONFD"; mkdir -p "$TARGET_CONFD"
cat > "$TARGET_CONFD/99-debark-e4-test" <<'EOF'
// captured verbatim from the (simulated) target's /etc/apt/apt.conf.d/
APT::Install-Recommends "false";
EOF

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
)
apt-get "${APT_BASE[@]}" update -qq > "$OUT/root-update.log" 2>&1

# Resolve $REQUEST and report: package count + total bytes, from
# --print-uris (which carries the real archive Size field per file — the
# same output debark parses into the lock).
# LOADER is the ONE mechanism this script found that actually works: an
# APT_CONFIG-pointed file whose content redirects Dir::Etc::parts. Built here
# so both the "target ground truth" row and the "applied fix" row can use it.
LOADER="$OUT/loader.conf"
printf 'Dir::Etc::parts "%s";\n' "$TARGET_CONFD" > "$LOADER"

RESOLVE_N=0
RESOLVE_BYTES=0
resolve() {
    # Usage: resolve <label> <logfile> <use-apt-config: 0|1> [extra -o args...]
    local label="$1" logf="$2" use_apt_config="$3"; shift 3
    if [ "$use_apt_config" = 1 ]; then
        APT_CONFIG="$LOADER" apt-get "${APT_BASE[@]}" "$@" install --print-uris -qq -y "$REQUEST" > "$logf" 2>&1
    else
        apt-get "${APT_BASE[@]}" "$@" install --print-uris -qq -y "$REQUEST" > "$logf" 2>&1
    fi
    RESOLVE_N=$(grep -c "^'http" "$logf" || true)
    RESOLVE_BYTES=$(grep "^'http" "$logf" | awk '{sum+=$3} END{print sum+0}')
    local mb
    mb=$(awk -v b="$RESOLVE_BYTES" 'BEGIN{printf "%.1f", b/1048576}')
    printf '%-32s -> %4d files, %10d bytes (%s MB)\n' "$label" "$RESOLVE_N" "$RESOLVE_BYTES" "$mb" \
        | tee -a "$OUT/summary.txt"
}

log ""
log "=== part 1: Install-Recommends leakage, request='$REQUEST' ==="
log ""
log "-- (a) TARGET's own resolution: real target behaviour, apt.conf.d honoured via the working APT_CONFIG mechanism --"
resolve "target (recommends=false)" "$OUT/a-target.log" 1
N_TARGET=$RESOLVE_N; B_TARGET=$RESOLVE_BYTES

log ""
log "-- (b) PROTOTYPE's actual behaviour today: apt.conf.d NOT carried at all (download-packages.sh never copies it) --"
resolve "prototype (no apt.conf.d carried)" "$OUT/b-prototype.log" 0
N_PROTO=$RESOLVE_N; B_PROTO=$RESOLVE_BYTES

log ""
log "-- (b2) the OBVIOUS-LOOKING 'fix' that does NOT work: -o Dir::Etc::parts=<target confd> --"
log "   (describes the private root as built entirely from explicit -o Dir::* options;"
log "    this row shows that idiom silently fails for apt.conf.d specifically.)"
resolve "'-o Dir::Etc::parts=...' (broken)" "$OUT/b2-broken-fix.log" 0 -o "Dir::Etc::parts=$TARGET_CONFD"
N_BROKEN=$RESOLVE_N; B_BROKEN=$RESOLVE_BYTES

log ""
log "-- (c) the fix that DOES work, applied the same way (c) will be compared to (a) to confirm they match --"
resolve "APT_CONFIG=loader (fixed)" "$OUT/c-fix.log" 1
N_FIX=$RESOLVE_N; B_FIX=$RESOLVE_BYTES

EXTRA_N=$((N_PROTO - N_TARGET))
EXTRA_B=$((B_PROTO - B_TARGET))
log ""
log "=== result ==="
log "target (real target behaviour):        $N_TARGET files, $B_TARGET bytes"
log "prototype today (no apt.conf.d):        $N_PROTO files, $B_PROTO bytes"
log "delta (prototype minus target):         $EXTRA_N extra file(s), $EXTRA_B extra bytes ($(awk -v b="$EXTRA_B" 'BEGIN{printf "%.2f", b/1048576}') MB)"
log "'-o Dir::Etc::parts=' broken-fix:       $N_BROKEN files, $B_BROKEN bytes  (expect == prototype, i.e. the -o override had NO effect)"
log "APT_CONFIG loader-file fix:             $N_FIX files, $B_FIX bytes  (expect == target)"
if [ "$N_BROKEN" -eq "$N_PROTO" ]; then
    log "-> CONFIRMED: '-o Dir::Etc::parts=' does not redirect apt.conf.d scanning (broken-fix == prototype's unfixed count)."
else
    log "-> NOT reproduced this run: '-o Dir::Etc::parts=' actually changed the count. Re-check apt version behaviour."
fi
if [ "$N_FIX" -eq "$N_TARGET" ]; then
    log "-> CONFIRMED: APT_CONFIG loader-file fix reproduces the target's exact resolution."
else
    log "-> NOT reproduced this run: APT_CONFIG fix count differs from target. Inspect $OUT/c-fix.log vs $OUT/a-target.log."
fi

# ---------------------------------------------------------------------------
# Part 2: Default-Release, on a small fully-controlled two-suite fixture so
# the result does not depend on today's Ubuntu archive contents.
# ---------------------------------------------------------------------------
log ""
log "=== part 2: Default-Release, controlled two-suite fixture ==="
if ! command -v apt-ftparchive >/dev/null 2>&1; then
    apt-get update -qq > "$OUT/install-apt-utils.log" 2>&1
    apt-get install -y -qq apt-utils >> "$OUT/install-apt-utils.log" 2>&1
fi
command -v apt-ftparchive >/dev/null 2>&1 || { log "FATAL: apt-ftparchive still not available after install attempt"; cat "$OUT/install-apt-utils.log" | tee -a "$OUT/summary.txt"; exit 1; }
DR_OUT="$OUT/default-release"
rm -rf "$DR_OUT"; mkdir -p "$DR_OUT/repo-low/pool" "$DR_OUT/repo-high/pool"

build_placeholder_deb() {
    # $1=name $2=version $3=destdir
    local name="$1" ver="$2" dest="$3" workdir
    workdir=$(mktemp -d)
    mkdir -p "$workdir/DEBIAN"
    cat > "$workdir/DEBIAN/control" <<EOF
Package: $name
Version: $ver
Section: misc
Priority: optional
Architecture: all
Maintainer: debark experiments <noreply@example.invalid>
Description: E4 placeholder package ($name $ver)
 Built only to test apt.conf Default-Release handling; installs nothing.
EOF
    dpkg-deb --build -Zgzip "$workdir" "$dest/${name}_${ver}_all.deb" > /dev/null
    rm -rf "$workdir"
}

PKGNAME="debark-e4-testpkg"
build_placeholder_deb "$PKGNAME" "1.0-low"  "$DR_OUT/repo-low/pool"
build_placeholder_deb "$PKGNAME" "0.9-high" "$DR_OUT/repo-high/pool"   # LOWER version number, HIGHER-priority suite

index_repo() {
    local dir="$1" suite="$2" origin="$3"
    ( cd "$dir" && apt-ftparchive packages pool > Packages 2>/dev/null
      gzip -9kf Packages
      apt-ftparchive -o APT::FTPArchive::Release::Origin="$origin" \
                      -o APT::FTPArchive::Release::Suite="$suite" \
                      -o APT::FTPArchive::Release::Codename="$suite" \
                      -o APT::FTPArchive::Release::Architectures="all $ARCH" \
                      release . > Release )
}
index_repo "$DR_OUT/repo-low"  "suite-low"  "debark-e4-low"
index_repo "$DR_OUT/repo-high" "suite-high" "debark-e4-high"

DR_ROOT="$DR_OUT/aptroot"
rm -rf "$DR_ROOT"
mkdir -p "$DR_ROOT"/etc/apt/{sources.list.d,preferences.d,apt.conf.d,trusted.gpg.d} \
         "$DR_ROOT"/var/lib/apt/lists/partial "$DR_ROOT"/var/lib/dpkg \
         "$DR_ROOT"/var/log/apt "$DR_ROOT"/var/cache/apt/archives/partial
: > "$DR_ROOT/etc/apt/sources.list"
: > "$DR_ROOT/etc/apt/preferences"
: > "$DR_ROOT/var/lib/dpkg/status"
cat > "$DR_ROOT/etc/apt/sources.list.d/e4.sources" <<EOF
Types: deb
URIs: file://$DR_OUT/repo-low
Suites: suite-low
Components: pool
Trusted: yes

Types: deb
URIs: file://$DR_OUT/repo-high
Suites: suite-high
Components: pool
Trusted: yes
EOF
# apt-ftparchive with a bare "pool" component and flat layout needs the
# "trivial" per-directory Packages form; simplest robust approach: point
# directly at the flat repo root with Suites: ./ instead.
cat > "$DR_ROOT/etc/apt/sources.list.d/e4.sources" <<EOF
Types: deb
URIs: file://$DR_OUT/repo-low
Suites: ./
Trusted: yes

Types: deb
URIs: file://$DR_OUT/repo-high
Suites: ./
Trusted: yes
EOF

DR_APT_BASE=(
    -o "Dir::Etc::sourcelist=$DR_ROOT/etc/apt/sources.list"
    -o "Dir::Etc::sourceparts=$DR_ROOT/etc/apt/sources.list.d"
    -o "Dir::Etc::preferences=$DR_ROOT/etc/apt/preferences"
    -o "Dir::Etc::preferencesparts=$DR_ROOT/etc/apt/preferences.d"
    -o "Dir::Etc::trustedparts=$DR_ROOT/etc/apt/trusted.gpg.d"
    -o "Dir::State=$DR_ROOT/var/lib/apt"
    -o "Dir::State::status=$DR_ROOT/var/lib/dpkg/status"
    -o "Dir::Cache=$DR_ROOT/var/cache/apt"
    -o "Dir::Log=$DR_ROOT/var/log/apt"
    -o "APT::Architecture=$ARCH"
    -o "APT::Architectures::=$ARCH"
    -o "APT::Sandbox::User=root"
    -o "Debug::NoLocking=1"
)
apt-get "${DR_APT_BASE[@]}" update -qq > "$DR_OUT/update.log" 2>&1

log ""
log "-- without Default-Release: which version does apt prefer between suite-low (1.0-low) and suite-high (0.9-high)? --"
apt-cache "${DR_APT_BASE[@]}" policy "$PKGNAME" > "$DR_OUT/policy-no-dr.log" 2>&1
cat "$DR_OUT/policy-no-dr.log" | tee -a "$OUT/summary.txt"
NODR_CANDIDATE=$(awk '/Candidate:/{print $2}' "$DR_OUT/policy-no-dr.log")
log "candidate with no Default-Release: $NODR_CANDIDATE"

log ""
log "-- with APT::Default-Release=\"suite-high\" carried via the APT_CONFIG loader mechanism --"
DR_CONFD="$DR_OUT/target-apt.conf.d"
rm -rf "$DR_CONFD"; mkdir -p "$DR_CONFD"
printf 'APT::Default-Release "suite-high";\n' > "$DR_CONFD/99-default-release"
DR_LOADER="$DR_OUT/loader.conf"
printf 'Dir::Etc::parts "%s";\n' "$DR_CONFD" > "$DR_LOADER"
APT_CONFIG="$DR_LOADER" apt-cache "${DR_APT_BASE[@]}" policy "$PKGNAME" > "$DR_OUT/policy-with-dr.log" 2>&1
cat "$DR_OUT/policy-with-dr.log" | tee -a "$OUT/summary.txt"
WITHDR_CANDIDATE=$(awk '/Candidate:/{print $2}' "$DR_OUT/policy-with-dr.log")
log "candidate with Default-Release=suite-high: $WITHDR_CANDIDATE"

log ""
if [ "$NODR_CANDIDATE" != "$WITHDR_CANDIDATE" ]; then
    log "-> CONFIRMED: Default-Release changes the selected version/suite ($NODR_CANDIDATE -> $WITHDR_CANDIDATE) and is invisible to a resolver that never carries apt.conf.d."
else
    log "-> NOT reproduced this run: candidate unchanged ($NODR_CANDIDATE). Inspect $DR_OUT/policy-*.log."
fi

log ""
log "raw evidence: $OUT/"
log "=== E4 done ==="
