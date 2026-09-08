#!/usr/bin/env bash
#
# e6-keyring-portability.sh — E6: can a MUCH NEWER host/container resolve for
# an OLDER target using only the exact apt keyrings captured from that
# target's own snapshot? Or does the newer host's gpg/apt reject some of the
# older target's key material (SHA-1 self-signatures, small/old key formats,
# weak-digest policy) as deprecated?
#
# Settles backend selection (local vs container) and the trust model: "Snapshot keyrings are captured
# policy, not a root of trust... never auto-import replacement archive
# keys"). Open question: "Can a modern host resolve for an older
# target's keyrings?" -> "whether container-by-default is required for older
# targets."
#
# Method (three independent stages per target release):
#   1. CAPTURE (runs inside a container of the TARGET release itself): copy
#      /etc/apt/trusted.gpg.d/* and /usr/share/keyrings/*, /etc/apt/keyrings/*
#      exactly the way snapshot-target.sh captures them on a real offline
#      machine (see snapshot-target.sh "capturing archive signing keys").
#      Then, using the TARGET's OWN gpg (never a newer one), record every
#      key's algorithm, length, creation date, and self-/binding-signature
#      digest algorithm via `gpg --with-colons --list-keys` and
#      `gpg --list-packets` — ground truth for what kind of key material is
#      actually in play, instead of assuming.
#   2. VERIFY (runs inside a container of a NEWER host release): build a
#      private apt root exactly like download-packages.sh's --state code
#      path does when resolving from a real snapshot — flatten every
#      captured keyring file into Dir::Etc::trustedparts (NOT the newer
#      host's own /etc/apt/trusted.gpg.d — that would defeat the point) and
#      drop Signed-By pinning, exactly as strip_signed_by() plus the
#      "$SNAP/keyrings -> $ROOT/etc/apt/trusted.gpg.d" copy loop in
#      download-packages.sh do. Point sources at the TARGET's real release
#      pockets and run `apt-get update` FOR REAL (genuine network fetch and
#      cryptographic verification of the target's live InRelease — this
#      cannot be answered by `-s`/simulation). Grep the transcript for
#      NO_PUBKEY / EXPKEYSIG / WEAK / SHA1 / deprecated / insecure / BADSIG.
#   3. CROSS-CHECK: independently of apt, fetch each InRelease with curl and
#      verify it with the newer host's own `gpgv --status-fd 1` (reports
#      GOODSIG/VALIDSIG plus the actual hash algorithm used for that specific
#      document signature) and cross-confirm via `gpg --list-packets` on the
#      extracted signature block. This tells apart the (frequently older,
#      sometimes SHA-1) hash used when a key's *identity was first bound* in
#      2007-2018 from the hash actually used *today* when that same key
#      signs a fresh InRelease — the thing that actually matters for
#      acceptance/rejection.
#
# Primary case: TARGET=ubuntu:22.04 (jammy), HOSTS=ubuntu:26.04 + ubuntu:24.04
# (newer container backends). A BONUS pass (default on;
# --no-bonus to skip) repeats stage 1-3 for an EOL-adjacent, genuinely older
# target (bionic/18.04) against the 26.04 host, to actually exercise
# old-key-format risk in case the primary jammy case is a non-event.
#
# Usage:
#   ./hack/experiments/e6-keyring-portability.sh
#   ./hack/experiments/e6-keyring-portability.sh --no-bonus
#   ./hack/experiments/e6-keyring-portability.sh --host-images "ubuntu:26.04"
#   ./hack/experiments/e6-keyring-portability.sh --bonus-image ubuntu:16.04 --bonus-release xenial
#
# Requires Docker (this needs a container of the TARGET release's own gpg
# AND separately a container of a NEWER release's gpg — a single machine
# can't be both, so unlike e1 there is no non-docker path). Needs real
# network access: this must really fetch archive.ubuntu.com/security.ubuntu.com.
#
# Writes raw evidence under hack/experiments/out/e6/ and a running summary to
# stdout + hack/experiments/out/e6/summary.txt.
set -uo pipefail

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF/../.." && pwd)"
REPO_REL_OUT="hack/experiments/out/e6"
OUT="$SELF/out/e6"
ARCH="amd64"
MIRROR="http://archive.ubuntu.com/ubuntu"
SEC_MIRROR="http://security.ubuntu.com/ubuntu"
COMPONENTS="main restricted universe multiverse"

TARGET_IMAGE="ubuntu:22.04"
TARGET_RELEASE="jammy"
HOST_IMAGES=("ubuntu:26.04" "ubuntu:24.04")
DO_BONUS=1
BONUS_IMAGE="ubuntu:18.04"
BONUS_RELEASE="bionic"

export MSYS_NO_PATHCONV=1

die() { echo "FATAL: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Internal sub-commands: this same script re-execs itself inside the target
# container (__capture) and inside each host container (__verify). A human
# never passes these directly.
# ---------------------------------------------------------------------------
if [ "${1:-}" = "__capture" ]; then
    RELEASE="$2"; KOUT="$REPO_ROOT/$3"
    set -e
    export DEBIAN_FRONTEND=noninteractive
    mkdir -p "$KOUT/trusted.gpg.d" "$KOUT/usr-share-keyrings" "$KOUT/etc-apt-keyrings"
    apt-get update -qq >/dev/null
    apt-get install -y -qq gnupg >/dev/null 2>&1

    GT="$KOUT/ground-truth.txt"
    {
        . /etc/os-release
        echo "=== target identity ($RELEASE) ==="
        echo "PRETTY_NAME=$PRETTY_NAME  VERSION_CODENAME=$VERSION_CODENAME  VERSION_ID=$VERSION_ID"
        apt-get -v | head -1
        dpkg -l dpkg gpgv ubuntu-keyring 2>/dev/null | grep '^ii' || true
        echo "gpg  version (TARGET's own): $(gpg --version | head -1)"
        echo "gpgv version (TARGET's own): $(gpgv --version | head -1)"
    } > "$GT"

    # Exactly what snapshot-target.sh captures ("capturing archive signing keys").
    [ -d /etc/apt/trusted.gpg.d ] && cp -a /etc/apt/trusted.gpg.d/. "$KOUT/trusted.gpg.d/" 2>/dev/null || true
    [ -d /usr/share/keyrings ] && cp -a /usr/share/keyrings/. "$KOUT/usr-share-keyrings/" 2>/dev/null || true
    [ -d /etc/apt/keyrings ] && cp -a /etc/apt/keyrings/. "$KOUT/etc-apt-keyrings/" 2>/dev/null || true

    {
        echo ""
        echo "=== captured files ==="
        find "$KOUT" -type f -name '*.gpg' -o -name '*.asc' | sort
    } >> "$GT"

    # Ground truth: ask the TARGET's OWN gpg about every key it trusts.
    for f in "$KOUT"/trusted.gpg.d/*.gpg "$KOUT"/usr-share-keyrings/*.gpg "$KOUT"/etc-apt-keyrings/*.gpg; do
        [ -f "$f" ] || continue
        sz=$(stat -c%s "$f")
        {
            echo ""
            echo "############ $f  (${sz} bytes) ############"
        } >> "$GT"
        if [ "$sz" -eq 0 ]; then echo "(empty file, skipping)" >> "$GT"; continue; fi
        {
            echo "--- with-colons: pub/sub = type:validity:LENGTH:ALGO:keyid:CREATED:expiry ---"
            gpg --no-default-keyring --keyring "$f" --with-colons --list-keys 2>/dev/null | grep -E '^(pub|sub|fpr|uid):'
            echo "--- list-packets: per-key/per-signature algo + digest algo ---"
            echo "    (pubkey algo 1=RSA 17=DSA 22=EdDSA; digest algo 2=SHA1 8=SHA256 10=SHA512)"
            gpg --list-packets "$f" 2>&1 | grep -E '^:(public key|public sub|user ID|signature) packet|version [0-9]+, algo|digest algo'
        } >> "$GT"
    done
    cat "$GT"
    exit 0
fi

if [ "${1:-}" = "__verify" ]; then
    RELEASE="$2"; KDIR="$REPO_ROOT/$3"; VOUT="$REPO_ROOT/$4"
    set -uo pipefail
    export DEBIAN_FRONTEND=noninteractive
    mkdir -p "$VOUT/inrelease"
    apt-get update -qq >/dev/null
    apt-get install -y -qq gnupg curl >/dev/null 2>&1

    {
        . /etc/os-release
        echo "HOST: $PRETTY_NAME  codename=$VERSION_CODENAME"
        echo "apt:  $(apt-get -v | head -1)"
        echo "gpgv (HOST's own, preinstalled): $(gpgv --version | head -1)"
        echo "gpg  (HOST's own, installed for this check): $(gpg --version | head -1)"
    } > "$VOUT/host-identity.txt"

    ROOT="$VOUT/aptroot"
    rm -rf "$ROOT"
    mkdir -p "$ROOT"/etc/apt/{sources.list.d,preferences.d,apt.conf.d,trusted.gpg.d} \
             "$ROOT"/var/lib/apt/lists/partial "$ROOT"/var/lib/dpkg \
             "$ROOT"/var/log/apt "$ROOT"/var/cache/apt/archives/partial
    : > "$ROOT/etc/apt/sources.list"
    : > "$ROOT/etc/apt/preferences"
    : > "$ROOT/var/lib/dpkg/status"

    # Flatten EVERY captured TARGET keyring into trustedparts. Nothing from
    # this (newer) host's own trust store is used anywhere below — this is
    # the "trusting ONLY the keys the target itself trusts" case. Mirrors
    # download-packages.sh's --state code path: it copies every file under
    # $SNAP/keyrings (our usr-share-keyrings/etc-apt-keyrings) plus
    # $SNAP/apt/trusted.gpg.d (our trusted.gpg.d) into
    # $ROOT/etc/apt/trusted.gpg.d, then strips Signed-By/signed-by from the
    # source files so trust comes from the flattened trustedparts pool, not
    # per-source pinning. We reproduce that end state directly: the sources
    # file below carries no Signed-By, and Dir::Etc::trustedparts points at
    # the flattened directory.
    cp -a "$KDIR"/trusted.gpg.d/. "$ROOT/etc/apt/trusted.gpg.d/" 2>/dev/null || true
    cp -a "$KDIR"/usr-share-keyrings/. "$ROOT/etc/apt/trusted.gpg.d/" 2>/dev/null || true
    cp -a "$KDIR"/etc-apt-keyrings/. "$ROOT/etc/apt/trusted.gpg.d/" 2>/dev/null || true
    echo "flattened trustedparts set:" >> "$VOUT/host-identity.txt"
    ls "$ROOT/etc/apt/trusted.gpg.d/" >> "$VOUT/host-identity.txt"
    cat "$VOUT/host-identity.txt"

    cat > "$ROOT/etc/apt/sources.list.d/${RELEASE}.sources" <<EOF
Types: deb
URIs: $MIRROR
Suites: $RELEASE $RELEASE-updates
Components: $COMPONENTS

Types: deb
URIs: $SEC_MIRROR
Suites: $RELEASE-security
Components: $COMPONENTS
EOF

    APT_OPTS=(
        -o Dir::Etc::sourcelist="$ROOT/etc/apt/sources.list"
        -o Dir::Etc::sourceparts="$ROOT/etc/apt/sources.list.d"
        -o Dir::Etc::preferences="$ROOT/etc/apt/preferences"
        -o Dir::Etc::preferencesparts="$ROOT/etc/apt/preferences.d"
        -o Dir::Etc::trustedparts="$ROOT/etc/apt/trusted.gpg.d"
        -o Dir::State="$ROOT/var/lib/apt"
        -o Dir::State::status="$ROOT/var/lib/dpkg/status"
        -o Dir::Cache="$ROOT/var/cache/apt"
        -o Dir::Log="$ROOT/var/log/apt"
        -o APT::Architecture="$ARCH"
        -o APT::Architectures::="$ARCH"
        -o Acquire::Languages=none
        -o APT::Sandbox::User=root
        -o Debug::NoLocking=1
    )

    echo ""
    echo "==> apt-get update (REAL network fetch+verify of live $RELEASE InRelease, keys ONLY from $KDIR)"
    apt-get "${APT_OPTS[@]}" update > "$VOUT/apt-update.log" 2>&1
    APT_RC=$?
    echo "EXIT_CODE=$APT_RC" >> "$VOUT/apt-update.log"
    cat "$VOUT/apt-update.log"

    grep -inE 'NO_PUBKEY|EXPKEYSIG|WEAK|SHA1|deprecated|insecure|BADSIG|error|warn' "$VOUT/apt-update.log" \
        > "$VOUT/apt-update-warnings.txt" || true
    echo "-- grep for NO_PUBKEY/EXPKEYSIG/WEAK/SHA1/deprecated/insecure/BADSIG/error/warn --"
    if [ -s "$VOUT/apt-update-warnings.txt" ]; then cat "$VOUT/apt-update-warnings.txt"; else echo "(none)"; fi

    # Independent cross-check: fetch InRelease ourselves and verify with the
    # HOST's own gpgv/gpg directly, bypassing apt's method wrapper entirely.
    KEYRING_ARGS=()
    for f in "$ROOT"/etc/apt/trusted.gpg.d/*.gpg; do KEYRING_ARGS+=(--keyring "$f"); done

    : > "$VOUT/gpgv-crosscheck.txt"
    : > "$VOUT/listpackets-signatures.txt"
    GPGV_FAIL=0
    for suite in "$RELEASE" "$RELEASE-updates" "$RELEASE-security"; do
        case "$suite" in
            *-security) url="$SEC_MIRROR/dists/$suite/InRelease" ;;
            *)          url="$MIRROR/dists/$suite/InRelease" ;;
        esac
        if ! curl -fsSL "$url" -o "$VOUT/inrelease/$suite.InRelease"; then
            echo "curl FAILED for $url" | tee -a "$VOUT/gpgv-crosscheck.txt"
            GPGV_FAIL=1
            continue
        fi

        {
            echo "### gpgv --status-fd 1 : $suite ###"
        } >> "$VOUT/gpgv-crosscheck.txt"
        gpgv --status-fd 1 "${KEYRING_ARGS[@]}" "$VOUT/inrelease/$suite.InRelease" >> "$VOUT/gpgv-crosscheck.txt" 2>&1
        rc=$?
        echo "gpgv exit=$rc" >> "$VOUT/gpgv-crosscheck.txt"
        echo "" >> "$VOUT/gpgv-crosscheck.txt"
        [ "$rc" -eq 0 ] || GPGV_FAIL=1

        # Cross-confirm via raw packet inspection of just the signature block
        # (gpg --list-packets on a whole clearsigned file mis-parses; the
        # extracted armored signature block parses cleanly).
        sed -n '/-----BEGIN PGP SIGNATURE-----/,/-----END PGP SIGNATURE-----/p' \
            "$VOUT/inrelease/$suite.InRelease" > "$VOUT/$suite.sig.asc"
        echo "### gpg --list-packets sig block: $suite ###" >> "$VOUT/listpackets-signatures.txt"
        gpg --list-packets "$VOUT/$suite.sig.asc" 2>&1 | grep -E 'signature packet|digest algo|keyid' \
            >> "$VOUT/listpackets-signatures.txt"
        echo "" >> "$VOUT/listpackets-signatures.txt"
    done
    echo ""
    echo "-- gpgv/gpg cross-check (independent of apt) --"
    cat "$VOUT/gpgv-crosscheck.txt"
    cat "$VOUT/listpackets-signatures.txt"

    if [ "$APT_RC" -eq 0 ] && [ "$GPGV_FAIL" -eq 0 ]; then
        echo "RESULT=PASS" > "$VOUT/result.txt"
    else
        echo "RESULT=FAIL (apt_rc=$APT_RC gpgv_fail=$GPGV_FAIL)" > "$VOUT/result.txt"
    fi
    cat "$VOUT/result.txt"
    exit 0
fi

# ---------------------------------------------------------------------------
# Top-level orchestration (what a human actually runs).
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        --target-image) TARGET_IMAGE="$2"; shift 2 ;;
        --target-release) TARGET_RELEASE="$2"; shift 2 ;;
        --host-images) IFS=' ' read -r -a HOST_IMAGES <<< "$2"; shift 2 ;;
        --no-bonus) DO_BONUS=0; shift ;;
        --bonus-image) BONUS_IMAGE="$2"; shift 2 ;;
        --bonus-release) BONUS_RELEASE="$2"; shift 2 ;;
        -h|--help) grep '^#' "$0" | sed 's/^#//'; exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 1 ;;
    esac
done

command -v docker >/dev/null || die "docker is required (need containers of both the target AND a newer host release)"

mkdir -p "$OUT"
: > "$OUT/summary.txt"
log() { printf '%s\n' "$*" | tee -a "$OUT/summary.txt"; }

host_label() { echo "$1" | sed -e 's#[:/]#-#g'; }

run_pass() {
    local target_image="$1" target_release="$2" pass_label="$3"; shift 3
    local hosts=("$@")
    local kdir_rel="$REPO_REL_OUT/keyrings-$target_release"
    local kdir_abs="$OUT/keyrings-$target_release"

    log ""
    log "=== [$pass_label] capturing $target_release keyrings from $target_image (target's own gpg) ==="
    rm -rf "$kdir_abs"; mkdir -p "$kdir_abs"
    docker run --rm -v "$SELF/../..":/src -w /src "$target_image" \
        bash hack/experiments/e6-keyring-portability.sh __capture "$target_release" "$kdir_rel" \
        > "$kdir_abs/capture-run.log" 2>&1
    CAP_RC=$?
    grep -E '^(pub:|fpr:|############|gpg  version|gpgv version|PRETTY_NAME)' "$kdir_abs/capture-run.log" | sed 's/^/    /' | tee -a "$OUT/summary.txt"
    [ "$CAP_RC" -eq 0 ] || { log "FATAL: capture failed for $target_image, see $kdir_abs/capture-run.log"; return 1; }
    N_KEYS=$(grep -c '^pub:' "$kdir_abs/capture-run.log" || true)
    N_FILES=$(find "$kdir_abs" -name '*.gpg' -size +0c 2>/dev/null | wc -l | tr -d ' ')
    log "captured $N_FILES non-empty keyring file(s), $N_KEYS public key(s) total; full ground truth: $kdir_abs/ground-truth.txt"

    local host_image host_label vout_rel vout_abs
    for host_image in "${hosts[@]}"; do
        host_label="$(host_label "$host_image")"
        vout_rel="$REPO_REL_OUT/${pass_label}-${host_label}"
        vout_abs="$OUT/${pass_label}-${host_label}"
        log ""
        log "=== [$pass_label] resolving $target_release from NEWER host $host_image, using ONLY $target_release's captured keys ==="
        rm -rf "$vout_abs"; mkdir -p "$vout_abs"
        docker run --rm -v "$SELF/../..":/src -w /src "$host_image" \
            bash hack/experiments/e6-keyring-portability.sh __verify "$target_release" "$kdir_rel" "$vout_rel" \
            > "$vout_abs/verify-run.log" 2>&1
        VER_RC=$?
        grep -E '^(HOST:|apt:|gpgv |gpg  |EXIT_CODE=|RESULT=)' "$vout_abs/verify-run.log" | sed 's/^/    /' | tee -a "$OUT/summary.txt"
        if [ "$VER_RC" -ne 0 ]; then
            log "    docker invocation itself failed (rc=$VER_RC) — see $vout_abs/verify-run.log"
        fi
        WARN_COUNT=0
        [ -s "$vout_abs/apt-update-warnings.txt" ] && WARN_COUNT=$(wc -l < "$vout_abs/apt-update-warnings.txt" | tr -d ' ')
        RESULT="$(cat "$vout_abs/result.txt" 2>/dev/null || echo 'RESULT=UNKNOWN (no result.txt written)')"
        log "    -> $RESULT   apt-warning-grep-hits=$WARN_COUNT   (raw evidence: $vout_abs/)"
    done
}

log "=== E6 keyring portability ==="
log "date (UTC): $(date -u +%Y-%m-%dT%H:%M:%SZ)"
log "primary target: $TARGET_IMAGE ($TARGET_RELEASE)"
log "newer hosts:    ${HOST_IMAGES[*]}"
log "bonus older-target pass: $( [ "$DO_BONUS" = 1 ] && echo "$BONUS_IMAGE ($BONUS_RELEASE) -> ${HOST_IMAGES[0]}" || echo "skipped (--no-bonus)" )"

run_pass "$TARGET_IMAGE" "$TARGET_RELEASE" "primary" "${HOST_IMAGES[@]}"

if [ "$DO_BONUS" = 1 ]; then
    log ""
    log "=== BONUS (labelled separately from the primary $TARGET_RELEASE result): genuinely older target ==="
    run_pass "$BONUS_IMAGE" "$BONUS_RELEASE" "bonus" "${HOST_IMAGES[0]}"
fi

log ""
log "raw evidence tree: $OUT/"
log "=== E6 done ==="
