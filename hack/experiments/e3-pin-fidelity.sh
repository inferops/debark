#!/usr/bin/env bash
#
# e3-pin-fidelity.sh — E3: does apt_preferences pinning survive the trip
# through a flat bundle repository whose Release no longer carries the
# original origins?
#
# Settles pinning versus the flat repository, and whether per-origin
# partitioning is needed in v1.
#
# Method: three small local flat repos stand in for "the normal archive",
# "-backports" and "a trusted third-party vendor" (fully controlled —
# doesn't depend on today's Ubuntu archive contents, same technique as
# E4 part 2's Default-Release fixture). A target apt_preferences pins the
# backports-like repo LOW and the vendor-like repo HIGH. Resolve online
# (bare package name): the pin should make apt pick the vendor's version
# even though it is not the highest version number available.
#
# Then build the flat bundle repo (Origin: debark,
# Suite: bundle) containing BOTH the version that was actually picked AND a
# second version of the same package (representing a realistic case: the
# bundle legitimately contains more than one version of a package — e.g.
# from a previous incremental run, or pulled in to satisfy a different
# requester's version constraint). Run the closed-world check
# defence 2 (exact-version install — must succeed) AND a bare-name
# re-resolution against the bundle-only world with the SAME preferences file
# carried over, to see whether losing the original origins changes the
# outcome versus the online resolution.
#
# Run: ./hack/experiments/e3-pin-fidelity.sh [--docker] [--image ubuntu:24.04]
#
# Writes raw evidence to hack/experiments/out/e3/ and a summary to stdout.
set -uo pipefail

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="ubuntu:24.04"
USE_DOCKER=0
CODENAME="noble"
ARCH="amd64"
OUT="$SELF/out/e3"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --docker) USE_DOCKER=1; shift ;;
        --image) IMAGE="$2"; USE_DOCKER=1; shift 2 ;;
        -h|--help) grep '^#' "$0" | sed 's/^#//'; exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 1 ;;
    esac
done

if [ "$USE_DOCKER" = 1 ]; then
    echo "==> re-running inside $IMAGE"
    exec docker run --rm -v "$SELF/../..":/src -w /src "$IMAGE" bash hack/experiments/e3-pin-fidelity.sh
fi

command -v dpkg-deb >/dev/null || { echo "need dpkg-deb; use --docker" >&2; exit 1; }

rm -rf "$OUT"
mkdir -p "$OUT"
: > "$OUT/summary.txt"
log() { printf '%s\n' "$*" | tee -a "$OUT/summary.txt"; }

log "=== E3 pin fidelity ==="
log "date (UTC):  $(date -u +%Y-%m-%dT%H:%M:%SZ)"
command -v apt-get >/dev/null && log "apt version: $(apt-get -v | head -1)"
command -v apt-ftparchive >/dev/null 2>&1 || {
    apt-get update -qq > "$OUT/install-tools.log" 2>&1
    apt-get install -y -qq apt-utils >> "$OUT/install-tools.log" 2>&1
}

PKG="debark-e3-testpkg"
build_placeholder_deb() {
    local ver="$1" dest="$2" workdir
    workdir=$(mktemp -d)
    mkdir -p "$workdir/DEBIAN"
    cat > "$workdir/DEBIAN/control" <<EOF
Package: $PKG
Version: $ver
Section: misc
Priority: optional
Architecture: all
Maintainer: debark experiments <noreply@example.invalid>
Description: E3 placeholder package ($ver)
 Built only to test apt_preferences pin fidelity through a flat bundle repo.
EOF
    dpkg-deb --build -Zgzip "$workdir" "$dest/${PKG}_${ver}_all.deb" > /dev/null
    rm -rf "$workdir"
}

index_repo() {
    local dir="$1" suite="$2" origin="$3"
    ( cd "$dir" && apt-ftparchive packages . > Packages 2>/dev/null
      gzip -9kf Packages
      apt-ftparchive -o APT::FTPArchive::Release::Origin="$origin" \
                      -o APT::FTPArchive::Release::Suite="$suite" \
                      -o APT::FTPArchive::Release::Codename="$suite" \
                      -o APT::FTPArchive::Release::Architectures="all $ARCH" \
                      release . > Release )
}

# ---------------------------------------------------------------------------
# 1. Three controlled repos: main (default), backports-like (pinned LOW,
#    higher version number), vendor-like (pinned HIGH, middling version).
# ---------------------------------------------------------------------------
mkdir -p "$OUT/repo-main" "$OUT/repo-backports" "$OUT/repo-vendor"
build_placeholder_deb "1.0-main"       "$OUT/repo-main"
build_placeholder_deb "2.0-backports"  "$OUT/repo-backports"
build_placeholder_deb "1.5-vendor"     "$OUT/repo-vendor"
index_repo "$OUT/repo-main"       "repo-main"       "debark-e3-main"
index_repo "$OUT/repo-backports"  "repo-backports"  "debark-e3-backports"
index_repo "$OUT/repo-vendor"     "repo-vendor"      "debark-e3-vendor"

log ""
log "fixture: $PKG available as 1.0-main (default archive), 2.0-backports"
log "(higher version, meant to be pinned LOW like real -backports), and"
log "1.5-vendor (middling version, meant to be pinned HIGH like a trusted"
log "third-party origin)."

# ---------------------------------------------------------------------------
# 2. Target apt_preferences: pin backports-like LOW, vendor-like HIGH.
# ---------------------------------------------------------------------------
PREFS="$OUT/target.preferences"
cat > "$PREFS" <<EOF
Package: $PKG
Pin: release a=repo-backports
Pin-Priority: 100

Package: $PKG
Pin: origin debark-e3-vendor
Pin-Priority: 900
EOF
log ""
log "-- target apt_preferences --"
cat "$PREFS" | tee -a "$OUT/summary.txt"

mk_root() {
    local root="$1"
    rm -rf "$root"
    mkdir -p "$root"/etc/apt/{sources.list.d,preferences.d,apt.conf.d,trusted.gpg.d} \
             "$root"/var/lib/apt/lists/partial "$root"/var/lib/dpkg \
             "$root"/var/log/apt "$root"/var/cache/apt/archives/partial
    : > "$root/etc/apt/sources.list"
    : > "$root/var/lib/dpkg/status"
}
root_apt_args() {
    local root="$1"
    echo -o "Dir::Etc::sourcelist=$root/etc/apt/sources.list" \
       -o "Dir::Etc::sourceparts=$root/etc/apt/sources.list.d" \
       -o "Dir::Etc::preferences=$root/etc/apt/preferences" \
       -o "Dir::Etc::preferencesparts=$root/etc/apt/preferences.d" \
       -o "Dir::Etc::trustedparts=$root/etc/apt/trusted.gpg.d" \
       -o "Dir::State=$root/var/lib/apt" \
       -o "Dir::State::status=$root/var/lib/dpkg/status" \
       -o "Dir::Cache=$root/var/cache/apt" \
       -o "Dir::Log=$root/var/log/apt" \
       -o "APT::Architecture=$ARCH" -o "APT::Architectures::=$ARCH" \
       -o "APT::Sandbox::User=root" -o "Debug::NoLocking=1"
}

# ---------------------------------------------------------------------------
# 3. Resolve ONLINE (bare package name — no version) with the pins in place.
# ---------------------------------------------------------------------------
ONLINE="$OUT/root-online"
mk_root "$ONLINE"
cp -a "$PREFS" "$ONLINE/etc/apt/preferences"
cat > "$ONLINE/etc/apt/sources.list.d/e3.sources" <<EOF
Types: deb
URIs: file://$OUT/repo-main
Suites: ./
Trusted: yes

Types: deb
URIs: file://$OUT/repo-backports
Suites: ./
Trusted: yes

Types: deb
URIs: file://$OUT/repo-vendor
Suites: ./
Trusted: yes
EOF
ONLINE_ARGS=($(root_apt_args "$ONLINE"))
apt-get "${ONLINE_ARGS[@]}" update -qq > "$OUT/online-update.log" 2>&1

log ""
log "-- online: apt-cache policy $PKG (with pins) --"
apt-cache "${ONLINE_ARGS[@]}" policy "$PKG" > "$OUT/online-policy.log" 2>&1
cat "$OUT/online-policy.log" | tee -a "$OUT/summary.txt"
ONLINE_CANDIDATE=$(awk '/Candidate:/{print $2}' "$OUT/online-policy.log")

apt-get "${ONLINE_ARGS[@]}" install --print-uris -qq -y "$PKG" > "$OUT/online-install.log" 2>&1
ONLINE_PICK_URI=$(grep "^'file" "$OUT/online-install.log" | head -1)
log ""
log "online resolution (apt-get install --print-uris $PKG, bare name):"
log "  $ONLINE_PICK_URI"
log "online candidate per apt-cache policy: $ONLINE_CANDIDATE"
if [ "$ONLINE_CANDIDATE" = "1.5-vendor" ]; then
    log "-> as expected: the HIGH-pinned vendor origin wins even though 2.0-backports is a higher version number."
else
    log "-> UNEXPECTED: online pick was not 1.5-vendor; inspect $OUT/online-policy.log"
fi

# ---------------------------------------------------------------------------
# 4. Build the flat BUNDLE repo (Origin: debark, Suite: bundle),
#    containing BOTH the online pick (1.5-vendor) and a second version
#    (2.0-backports) — a realistic case (e.g. left over from a prior
#    incremental run, or pulled in for another requester's version pin).
# ---------------------------------------------------------------------------
BUNDLE="$OUT/bundle/repo"
mkdir -p "$BUNDLE/pool"
cp "$OUT/repo-vendor/${PKG}_1.5-vendor_all.deb"       "$BUNDLE/pool/"
cp "$OUT/repo-backports/${PKG}_2.0-backports_all.deb" "$BUNDLE/pool/"
( cd "$BUNDLE" && apt-ftparchive packages . > Packages 2>/dev/null
  gzip -9kf Packages
  apt-ftparchive -o APT::FTPArchive::Release::Origin=debark \
                  -o APT::FTPArchive::Release::Label=debark \
                  -o APT::FTPArchive::Release::Suite=bundle \
                  -o APT::FTPArchive::Release::Codename="$CODENAME" \
                  -o APT::FTPArchive::Release::Architectures="all $ARCH" \
                  release . > Release )
log ""
log "-- bundle Release (fields) --"
cat "$BUNDLE/Release" | tee -a "$OUT/summary.txt"

# ---------------------------------------------------------------------------
# 5. Closed-world check: SAME dpkg status, SAME preferences, sources
#    replaced with ONLY the bundle.
# ---------------------------------------------------------------------------
CLOSED="$OUT/root-closedworld"
mk_root "$CLOSED"
cp -a "$PREFS" "$CLOSED/etc/apt/preferences"
cat > "$CLOSED/etc/apt/sources.list.d/bundle.sources" <<EOF
Types: deb
URIs: file://$BUNDLE
Suites: bundle
Components: main
Trusted: yes
EOF
CLOSED_ARGS=($(root_apt_args "$CLOSED"))
apt-get "${CLOSED_ARGS[@]}" update -qq > "$OUT/closedworld-update.log" 2>&1

log ""
log "-- closed-world: apt-cache policy $PKG (bundle-only source, SAME preferences file) --"
apt-cache "${CLOSED_ARGS[@]}" policy "$PKG" > "$OUT/closedworld-policy.log" 2>&1
cat "$OUT/closedworld-policy.log" | tee -a "$OUT/summary.txt"
CLOSED_CANDIDATE=$(awk '/Candidate:/{print $2}' "$OUT/closedworld-policy.log")

log ""
log "-- closed-world defence 1: apt-get -s install $PKG=1.5-vendor (EXACT locked version) --"
apt-get "${CLOSED_ARGS[@]}" install -s "$PKG=1.5-vendor" > "$OUT/closedworld-exact.log" 2>&1
EXACT_RC=$?
if grep -qE "^(Inst|Conf) $PKG \(1\.5-vendor" "$OUT/closedworld-exact.log"; then
    EXACT_RESULT="OK — installs the exact locked version"
else
    EXACT_RESULT="FAILED — see $OUT/closedworld-exact.log"
fi
log "  exit=$EXACT_RC -> $EXACT_RESULT"
grep -E "^(Inst|Conf) $PKG|^0 upgraded|Unable to locate" "$OUT/closedworld-exact.log" | sed 's/^/    /' | tee -a "$OUT/summary.txt"

log ""
log "-- the interesting test: apt-get -s install $PKG (BARE name, no version) against the bundle-only world --"
apt-get "${CLOSED_ARGS[@]}" install -s "$PKG" > "$OUT/closedworld-barename.log" 2>&1
BARENAME_PICK=$(grep -E "^(Inst|Conf) $PKG" "$OUT/closedworld-barename.log" | head -1)
log "  $BARENAME_PICK"

log ""
log "=== result ==="
log "online resolution picked:        $ONLINE_CANDIDATE  (pin-preferred, priority 900, NOT the highest version)"
log "closed-world candidate (bundle): $CLOSED_CANDIDATE"
log "exact-version install (defence 1): $EXACT_RESULT"
if [ "$CLOSED_CANDIDATE" != "$ONLINE_CANDIDATE" ]; then
    log "-> CONFIRMED DIVERGENCE: a bare-name re-resolution against the flat bundle picks"
    log "   '$CLOSED_CANDIDATE', NOT the online pin-preferred '$ONLINE_CANDIDATE' — because the"
    log "   bundle's Release (Origin: debark, Suite: bundle) matches neither pin rule"
    log "   ('release a=repo-backports' / 'origin debark-e3-vendor'), so apt falls back to"
    log "   plain highest-version-wins, exactly the risk pinning is meant to close."
else
    log "-> NOT reproduced this run: closed-world candidate matches the online pick regardless."
fi

log ""
log "raw evidence: $OUT/"
log "=== E3 done ==="
