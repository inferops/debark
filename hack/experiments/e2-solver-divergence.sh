#!/usr/bin/env bash
#
# e2-solver-divergence.sh — E2: does apt's classic ("internal") dependency
# resolver and apt 3.x's `solver3` choose the same closure for the same
# request? And is any divergence gated by apt MAJOR.MINOR (what backend
# auto-selection checks for) or by the SOLVER ALGORITHM specifically?
#
# Settles the backend auto-selection threshold and solver-version
# divergence recording.
# Full writeup: docs/experiments/E2-solver-divergence.md
#
# ---------------------------------------------------------------------------
# METHOD
# ---------------------------------------------------------------------------
# 1. Build a handful of small, controlled fixtures as a local flat apt
#    repository (same pattern as download-packages.sh section 6: an
#    `apt-ftparchive packages`/`release` index over a folder of .deb files).
#    Most fixtures use `equivs-build` from a hand-written control stanza —
#    a placeholder .deb with only control metadata, no payload — so the
#    dependency SHAPE is fully under our control and building it is far
#    less error-prone than hand-rolling .deb directory trees. One fixture
#    is deliberately a REAL Debian/Ubuntu package relationship instead of a
#    synthetic one, so at least one result is not just a synthetic curiosity:
#
#      A   zz-e2-app-alt-{fwd,rev}
#          Depends: liba | libb  (both installable, order flipped between
#          the two packages) — does solver order preference match Depends
#          order, and does it agree between solvers?
#
#      B   zz-e2-needs-mta
#          Depends: mail-transport-agent — a REAL virtual package with 9-10
#          REAL providers (postfix, exim4-daemon-{light,heavy}, ssmtp,
#          sendmail-bin, opensmtpd, nullmailer, msmtp-mta, esmtp-run, dma,
#          and (25.10+) courier-mta) resolved against each release's own
#          real archive. Zero synthetic package authoring needed for the
#          ambiguity itself.
#
#      B2  zz-e2-app-virt
#          Depends: zz-e2-virt — a synthetic virtual package with THREE
#          providers (zz-e2-pv-alpha/mike/zulu) whose apt-ftparchive SCAN
#          order is deliberately the reverse of their PACKAGE-NAME
#          alphabetical order (forced via a numeric filename prefix, since
#          the .deb filename — not the control Package: field — is what
#          apt-ftparchive's directory walk sorts on). This separates two
#          possible tie-break rules that fixture B's real-world result
#          cannot separate on its own: "first provider apt's index-builder
#          happened to encounter" vs "alphabetically-first provider name".
#
#      B3  zz-e2-app-virt-nat
#          Depends: zz-e2-virt-nat — the SAME idea as B2 (three otherwise-
#          identical providers, zz-e2-pvn-alpha/mike/zulu), but this time
#          left under their natural equivs-generated filenames, so scan
#          order and alphabetical-name order AGREE (both: alpha, mike,
#          zulu) instead of being deliberately opposed as in B2. Paired
#          with B2, this turns a single confounded observation into a
#          proper two-point comparison: if a solver's pick tracks
#          "scanned last" it should choose zulu here but alpha in B2; if it
#          tracks "alphabetically first" it should choose alpha in BOTH.
#
#      C1  zz-e2-app-multiclean
#          Depends: zz-e2-multi-clean (>= 1.0), which has 3 fully-installable
#          versions (1.0/1.1/2.0) — the trivial "pick the highest
#          version" case. Expected to be solver-invariant; included as a
#          control/baseline for C2.
#
#      C2  zz-e2-app-multibroken
#          Depends: zz-e2-multi-broken (>= 1.0), which also has versions
#          1.0/1.1/2.0 — but 2.0 (the version both resolvers try FIRST)
#          Depends on a package that does not exist anywhere in the
#          scenario. 1.1 and 1.0 both have no deps and would satisfy the
#          request cleanly. Does either resolver back off to 1.1, or do
#          both just fail the whole request?
#
# 2. Resolve every fixture with `apt-get install -s` (simulate only — this
#    script never downloads or installs a real package) inside a private
#    apt root built exactly (explicit Dir::* options,
#    never `Dir=` wholesale), under:
#      - apt 2.8.3        (ubuntu:24.04, codename noble)    — no solver3 at all
#      - apt 3.1.6ubuntu2  (ubuntu:25.10, codename questing) — solver3 default
#      - apt 3.2.0        (ubuntu:26.04, codename resolute) — solver3 default
#    and, on the two solver3-capable images, a SECOND pass of every fixture
#    with `-o APT::Solver=internal` — the SAME apt binary, forced onto the
#    classic resolver. `internal` is accepted (a no-op) even on apt 2.8.3,
#    which predates the `APT::Solver` key entirely — but `-o
#    APT::Solver=3.0` on apt 2.8.3 is a hard error ("Can't call external
#    solver '3.0'"), not a silently-ignored unknown key. That matters: you
#    cannot detect solver3 support by just trying the flag and seeing if
#    apt complains about an unknown key — apt 2.8.3 DOES recognise
#    `APT::Solver`, it just doesn't have a "3.0" to dispatch to.
#
#    This gives four isolating comparisons per fixture:
#      version-and-solver : 24.04-default   vs 26.04-default   (both vary)
#      version-only       : 25.10-default   vs 26.04-default   (same solver3)
#      solver-only        : 26.04-default   vs 26.04-internal  (same apt binary)
#      solver-only (again): 25.10-default   vs 25.10-internal  (same apt binary,
#                                                                independent instance)
#
# 3. The base dpkg status for each private root is that container's own REAL
#    /var/lib/dpkg/status. Unlike E1 (which needed a specific package/version
#    state and so had to do a real install first — see e1's "methodology
#    pitfall" note on why a hand-written one-line status file gives a false
#    reading), a stock ubuntu:*.* image's OWN status file already is a
#    realistic, dependency-consistent installed base — nothing needs
#    installing first, it's just copied as-is.
#
# 4. Diff the resulting `Inst` lines across rows, per fixture, and print a
#    verdict for each of the three comparisons above.
#
# ---------------------------------------------------------------------------
# Requires Docker Desktop (or another `docker`). The experiment inherently
# needs THREE different Ubuntu releases' apt; the WSL Ubuntu 24.04 fallback
# used by e1 only ever gives you one apt version, so it cannot reproduce
# this experiment by itself.
#
#   ./hack/experiments/e2-solver-divergence.sh
#
# Writes raw evidence (fixture repo, per-row apt-get logs, dpkg status
# snapshots) to hack/experiments/out/e2/ and a summary to stdout, also saved
# to out/e2/summary.txt.
set -uo pipefail

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$SELF/out/e2"
REPO_DIR="$OUT/repo"
# Container-side spelling of $OUT. Git Bash resolves $SELF to a host path
# like /d/projects/debark/hack/experiments (or, under `docker run -v`, gets
# passed through as-is with MSYS_NO_PATHCONV=1) — but every container mounts
# the repo root at /src, so any path an *inside-the-container* command needs
# to open must be spelled relative to /src, never as $OUT itself. Anything
# the OUTER (host) bash reads/writes directly still uses $OUT.
OUT_C="/src/hack/experiments/out/e2"

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 1 ;;
    esac
done

command -v docker >/dev/null 2>&1 || { echo "need docker — this experiment requires 3 distinct Ubuntu apt versions" >&2; exit 1; }

mkdir -p "$OUT"
: > "$OUT/summary.txt"
log()  { printf '%s\n' "$*" | tee -a "$OUT/summary.txt"; }
run_docker() { MSYS_NO_PATHCONV=1 docker run --rm -v "$SELF/../..":/src -w /src "$@"; }

log "=== E2 solver divergence ==="
log "host date (UTC): $(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ---------------------------------------------------------------------------
# 0. Re-confirm image digests + apt/solver identity ourselves (never trust a
#    stale note — pull fresh, then ask docker/apt directly).
# ---------------------------------------------------------------------------
declare -A DIGEST APTVER CODENAME
IMAGES=(ubuntu:24.04 ubuntu:25.10 ubuntu:26.04)
CODENAME[ubuntu:24.04]=noble
CODENAME[ubuntu:25.10]=questing
CODENAME[ubuntu:26.04]=resolute

log ""
log "=== 0. image identity (re-pulled and re-inspected this run) ==="
for img in "${IMAGES[@]}"; do
    docker pull "$img" > "$OUT/pull-${img/:/_}.log" 2>&1
    DIGEST[$img]="$(docker inspect --format '{{index .RepoDigests 0}}' "$img" 2>/dev/null)"
    probe="$(run_docker "$img" bash -c 'apt-get -v | head -1; apt-config dump | grep -i solv; date -u +%Y-%m-%dT%H:%M:%SZ')"
    APTVER[$img]="$(printf '%s\n' "$probe" | head -1)"
    log "$img  digest=${DIGEST[$img]}"
    log "    ${APTVER[$img]}  (codename ${CODENAME[$img]})"
    printf '%s\n' "$probe" | tail -n +2 | sed 's/^/    /' | tee -a "$OUT/summary.txt" >/dev/null
    printf '%s\n' "$probe" | tail -n +2 | sed 's/^/    /'
done

# ---------------------------------------------------------------------------
# 0b. Solver-activation probe: does an unrecognised APT::Solver value error
#     or get silently ignored on the apt that predates the key? Does
#     `internal` work everywhere? This determines whether `APT::Solver`
#     itself could ever be used as a version-detection signal.
# ---------------------------------------------------------------------------
log ""
log "=== 0b. APT::Solver activation probe ==="
p1="$(run_docker ubuntu:24.04 bash -c 'apt-get update -qq >/dev/null 2>&1; apt-get -o APT::Solver=3.0 install -s -y bash 2>&1 | tail -3')"
log "24.04 (apt 2.8.3) with -o APT::Solver=3.0 :"
log "$(printf '%s\n' "$p1" | sed 's/^/    /')"
p2="$(run_docker ubuntu:24.04 bash -c 'apt-get -o APT::Solver=internal install -s -y bash 2>&1 | tail -3')"
log "24.04 (apt 2.8.3) with -o APT::Solver=internal :"
log "$(printf '%s\n' "$p2" | sed 's/^/    /')"
if printf '%s' "$p1" | grep -q "Can't call external solver"; then
    log "-> CONFIRMED: apt 2.8.3 recognises the APT::Solver key (does NOT silently ignore"
    log "   an unrecognised value) but hard-errors trying to exec solver \"3.0\" as an"
    log "   external binary. 'internal' is accepted everywhere as a portable sentinel for"
    log "   \"use the classic resolver\", but the string \"3.0\" is not a safe universal probe."
else
    log "-> NOT CONFIRMED this run: apt 2.8.3 did not error as expected on APT::Solver=3.0 — inspect $OUT/summary.txt"
fi

# ---------------------------------------------------------------------------
# 1. Build the fixture repository once (equivs .debs are arch=all and carry
#    no real code, so one build is reused unmodified across all 3 apt
#    versions — only the REAL mail-transport-agent fixture (B) resolves
#    against each release's own real archive instead of this repo).
# ---------------------------------------------------------------------------
log ""
log "=== 1. building fixture repository ==="
rm -rf "$REPO_DIR"; mkdir -p "$REPO_DIR"
cat > "$OUT/_build-fixtures.sh" <<'BUILDEOF'
set -euo pipefail
REPO=/src/hack/experiments/out/e2/repo
WORK=/tmp/e2build
rm -rf "$WORK"; mkdir -p "$WORK"; cd "$WORK"

mkctl() { # mkctl <outfile> <package> <version> [depends] [provides]
    local f="$1" pkg="$2" ver="$3" dep="${4:-}" prov="${5:-}"
    { echo "Section: misc"
      echo "Priority: optional"
      echo "Standards-Version: 3.9.2"
      echo "Package: $pkg"
      echo "Version: $ver"
      [ -n "$dep" ]  && echo "Depends: $dep"
      [ -n "$prov" ] && echo "Provides: $prov"
      echo "Maintainer: debark E2 experiment <noreply@example.invalid>"
      echo "Architecture: all"
      echo "Description: debark E2 fixture ($pkg $ver)"
      echo " Synthetic placeholder package built by hack/experiments/e2-solver-divergence.sh"
      echo " for the apt solver-divergence experiment. Carries no files."
    } > "$f"
}
build() { equivs-build "$1" >/tmp/equivs-build.log 2>&1 || { echo "FAILED building $1"; cat /tmp/equivs-build.log; exit 1; }; }

# --- A: alternatives, both orders ---
mkctl liba.ctl   zz-e2-liba 1.0
mkctl libb.ctl   zz-e2-libb 1.0
mkctl appfwd.ctl zz-e2-app-alt-fwd 1.0 "zz-e2-liba (>= 1.0) | zz-e2-libb (>= 1.0)"
mkctl apprev.ctl zz-e2-app-alt-rev 1.0 "zz-e2-libb (>= 1.0) | zz-e2-liba (>= 1.0)"

# --- B: real virtual package wrapper ---
mkctl needsmta.ctl zz-e2-needs-mta 1.0 "mail-transport-agent"

# --- B2: synthetic virtual package, 3 providers, scan order SCRAMBLED ---
mkctl pva.ctl zz-e2-pv-alpha 1.0 "" "zz-e2-virt"
mkctl pvm.ctl zz-e2-pv-mike  1.0 "" "zz-e2-virt"
mkctl pvz.ctl zz-e2-pv-zulu  1.0 "" "zz-e2-virt"
mkctl appvirt.ctl zz-e2-app-virt 1.0 "zz-e2-virt"

# --- B3: same idea, 3 providers, but scan order left NATURAL (== alphabetical) ---
mkctl pvna.ctl zz-e2-pvn-alpha 1.0 "" "zz-e2-virt-nat"
mkctl pvnm.ctl zz-e2-pvn-mike  1.0 "" "zz-e2-virt-nat"
mkctl pvnz.ctl zz-e2-pvn-zulu  1.0 "" "zz-e2-virt-nat"
mkctl appvirtnat.ctl zz-e2-app-virt-nat 1.0 "zz-e2-virt-nat"

# --- C1: multiple satisfiable versions, all clean ---
mkctl mclean10.ctl zz-e2-multi-clean 1.0
mkctl mclean11.ctl zz-e2-multi-clean 1.1
mkctl mclean20.ctl zz-e2-multi-clean 2.0
mkctl appmclean.ctl zz-e2-app-multiclean 1.0 "zz-e2-multi-clean (>= 1.0)"

# --- C2: multiple satisfiable versions, highest is broken (forced choice) ---
mkctl mbrok10.ctl zz-e2-multi-broken 1.0
mkctl mbrok11.ctl zz-e2-multi-broken 1.1
mkctl mbrok20.ctl zz-e2-multi-broken 2.0 "zz-e2-phantom-missing"
mkctl appmbrok.ctl zz-e2-app-multibroken 1.0 "zz-e2-multi-broken (>= 1.0)"

for c in liba.ctl libb.ctl appfwd.ctl apprev.ctl needsmta.ctl pva.ctl pvm.ctl pvz.ctl \
         appvirt.ctl pvna.ctl pvnm.ctl pvnz.ctl appvirtnat.ctl \
         mclean10.ctl mclean11.ctl mclean20.ctl appmclean.ctl \
         mbrok10.ctl mbrok11.ctl mbrok20.ctl appmbrok.ctl; do
    build "$c"
done

# B2's whole point: make apt-ftparchive's directory-scan order (which
# follows the .deb FILENAME, not the control Package: field) the REVERSE of
# the providers' alphabetical PACKAGE NAME order, so a later divergence in
# which provider gets chosen can be attributed to one or the other.
cp zz-e2-pv-zulu_1.0_all.deb  "$REPO/01-scanned-first_zz-e2-pv-zulu.deb"
cp zz-e2-pv-mike_1.0_all.deb  "$REPO/02-scanned-second_zz-e2-pv-mike.deb"
cp zz-e2-pv-alpha_1.0_all.deb "$REPO/03-scanned-third_zz-e2-pv-alpha.deb"
# Remove the naturally-named originals so the generic move below doesn't
# ALSO add them under their natural (alphabetically-sortable) filenames.
rm -f zz-e2-pv-zulu_1.0_all.deb zz-e2-pv-mike_1.0_all.deb zz-e2-pv-alpha_1.0_all.deb

# Everything else — INCLUDING B3's zz-e2-pvn-* providers, deliberately left
# untouched so their scan order stays natural/alphabetical — moves over
# under its natural equivs-generated filename.
mv "$WORK"/*.deb "$REPO"/

cd "$REPO"
rm -f Packages Packages.gz Release
apt-ftparchive packages . > Packages
gzip -9kf Packages
apt-ftparchive -o APT::FTPArchive::Release::Origin=debark-e2 \
    -o APT::FTPArchive::Release::Label=debark-e2 \
    -o APT::FTPArchive::Release::Suite=./ \
    -o APT::FTPArchive::Release::Architectures=all \
    release . > Release
echo "-- fixture repo built: $(ls *.deb | wc -l) .deb files --"
grep -E "^Package:|^Version:|^Depends:|^Provides:" Packages
BUILDEOF
run_docker ubuntu:24.04 bash -c '
    apt-get update -qq
    apt-get install -y -qq equivs apt-utils >/tmp/eq.log 2>&1 || { cat /tmp/eq.log; exit 1; }
    bash /src/hack/experiments/out/e2/_build-fixtures.sh
' > "$OUT/build-fixtures.log" 2>&1
if [ $? -ne 0 ]; then log "FATAL: fixture build failed — see $OUT/build-fixtures.log"; exit 1; fi
N_DEB="$(ls "$REPO_DIR"/*.deb 2>/dev/null | wc -l | tr -d ' ')"
log "built $N_DEB fixture .deb files in $REPO_DIR (log: build-fixtures.log)"
[ "$N_DEB" -ge 18 ] || { log "FATAL: expected >=18 fixture .deb files, found $N_DEB"; exit 1; }

# ---------------------------------------------------------------------------
# 2. Per-image resolution matrix.
# ---------------------------------------------------------------------------
cat > "$OUT/_run-row.sh" <<'ROWEOF'
set -uo pipefail
IMG_LABEL="$1"; CODENAME="$2"; SOLVER_NAME="$3"; OUTDIR="$4"; shift 4
SOLVER_OPTS=("$@")
REPO=/src/hack/experiments/out/e2/repo
ROOT="$OUTDIR/aptroot"
rm -rf "$ROOT"
mkdir -p "$ROOT"/etc/apt/{sources.list.d,preferences.d,apt.conf.d,trusted.gpg.d} \
         "$ROOT"/var/lib/apt/lists/partial "$ROOT"/var/lib/dpkg \
         "$ROOT"/var/log/apt "$ROOT"/var/cache/apt/archives/partial
: > "$ROOT/etc/apt/sources.list"
: > "$ROOT/etc/apt/preferences"
cp -a /var/lib/dpkg/status "$ROOT/var/lib/dpkg/status"

cat > "$ROOT/etc/apt/sources.list.d/zz-e2-local.sources" <<EOF
Types: deb
URIs: file://$REPO
Suites: ./
Trusted: yes
EOF
cat > "$ROOT/etc/apt/sources.list.d/ubuntu.sources" <<EOF
Types: deb
URIs: http://archive.ubuntu.com/ubuntu
Suites: $CODENAME $CODENAME-updates
Components: main restricted universe multiverse
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
    -o "APT::Architecture=amd64"
    -o "APT::Architectures::=amd64"
    -o "Acquire::Languages=none"
    -o "APT::Sandbox::User=root"
    -o "Debug::NoLocking=1"
    "${SOLVER_OPTS[@]}"
)
echo "row: $IMG_LABEL / $SOLVER_NAME   date(UTC): $(date -u +%Y-%m-%dT%H:%M:%SZ)"
apt-get "${APT_BASE[@]}" update -qq > "$OUTDIR/update.$SOLVER_NAME.log" 2>&1
echo "  update rc=$?"

PROVIDER_RE='zz-e2-|postfix|exim4|ssmtp|sendmail|opensmtpd|nullmailer|msmtp|esmtp-run|dma|courier'
select_of() {
    # NOTE: these must be separate `local` statements, not one chained
    # `local fixture=$1 logf=...$fixture...` — under `set -u`, bash
    # evaluates every RHS in a single multi-assignment `local` command
    # before any of that command's own names become bound, so referencing
    # an earlier name (fixture) in a later value (logf) on the SAME `local`
    # line is itself an unbound-variable error. (Confirmed by minimal
    # repro; this is not apt/docker-specific, just a bash `set -u` gotcha.)
    local fixture="$1" pkg="$2"
    local logf="$OUTDIR/$fixture.$SOLVER_NAME.log"
    apt-get "${APT_BASE[@]}" install -s -y "$pkg" > "$logf" 2>&1
    local sel
    sel="$(grep -E '^Inst ' "$logf" | grep -E "$PROVIDER_RE" | awk '{print $2, $3}' | sort)"
    if [ -z "$sel" ] && grep -qE "^E: |unmet dependencies" "$logf"; then
        sel="(FAILED: $(grep -m1 -E '^E: ' "$logf" | cut -c1-120))"
    fi
    printf '  %-16s -> %s\n' "$fixture" "$(printf '%s' "$sel" | tr '\n' '|')"
}

select_of alt-fwd      zz-e2-app-alt-fwd
select_of alt-rev      zz-e2-app-alt-rev
select_of mta          zz-e2-needs-mta
select_of virt         zz-e2-app-virt
select_of virt-nat     zz-e2-app-virt-nat
select_of multiclean   zz-e2-app-multiclean
select_of multibroken  zz-e2-app-multibroken
ROWEOF

declare -A RESULT   # RESULT["<fixture>|<row>"] = selection string

run_row() {
    local label="$1" image="$2" codename="$3" solver_name="$4"; shift 4
    local outdir="$OUT/runs/${label}"        # host-side, for our own mkdir
    local outdir_c="$OUT_C/runs/${label}"    # container-side, passed in
    mkdir -p "$outdir"
    log ""
    log "--- row: $label (image=$image, solver=$solver_name) ---"
    local out
    out="$(run_docker "$image" bash "$OUT_C/_run-row.sh" "$label" "$codename" "$solver_name" "$outdir_c" "$@" 2>&1)"
    printf '%s\n' "$out" | tee -a "$OUT/summary.txt"
    local fixture sel
    while IFS= read -r fline; do
        fixture="$(printf '%s' "$fline" | awk '{print $1}')"
        sel="$(printf '%s' "$fline" | sed -E 's/^.*-> //')"
        [ -n "$fixture" ] && RESULT["$fixture|$label/$solver_name"]="$sel"
    done < <(printf '%s\n' "$out" | grep -E '^  (alt-fwd|alt-rev|mta|virt-nat|virt|multiclean|multibroken) ')
}

log ""
log "=== 2. resolution matrix ==="
run_row "2404" ubuntu:24.04 noble    default  # apt 2.8.3 — no solver3 exists
run_row "2404" ubuntu:24.04 noble    internal -o APT::Solver=internal  # sanity check: must equal default
run_row "2510" ubuntu:25.10 questing default
run_row "2510" ubuntu:25.10 questing internal -o APT::Solver=internal
run_row "2604" ubuntu:26.04 resolute default
run_row "2604" ubuntu:26.04 resolute internal -o APT::Solver=internal

# ---------------------------------------------------------------------------
# 3. Diff matrix: for each fixture, the 3 isolating comparisons.
# ---------------------------------------------------------------------------
log ""
log "=== 3. divergence verdicts ==="
FIXTURES=(alt-fwd alt-rev mta virt virt-nat multiclean multibroken)
N_DIVERGED=0; N_TOTAL=0
for f in "${FIXTURES[@]}"; do
    log ""
    log "-- fixture: $f --"
    for row in 2404/default 2404/internal 2510/default 2510/internal 2604/default 2604/internal; do
        log "    [$row]  ${RESULT[$f|$row]:-<no data>}"
    done
    a="${RESULT[$f|2404/default]:-}"
    b="${RESULT[$f|2510/default]:-}"
    c="${RESULT[$f|2604/default]:-}"
    d="${RESULT[$f|2604/internal]:-}"
    e="${RESULT[$f|2510/internal]:-}"
    verdict_line() {
        local name="$1" x="$2" y="$3"
        N_TOTAL=$((N_TOTAL+1))
        if [ "$x" = "$y" ]; then
            log "    $name: SAME"
        else
            log "    $name: DIVERGED  ( ${x} )  vs  ( ${y} )"
            N_DIVERGED=$((N_DIVERGED+1))
        fi
    }
    verdict_line "version+solver together (24.04-default vs 26.04-default)" "$a" "$c"
    verdict_line "version-only, same solver3   (25.10-default vs 26.04-default)" "$b" "$c"
    verdict_line "solver-only, same apt binary (26.04-default vs 26.04-internal)" "$c" "$d"
    verdict_line "solver-only, same apt binary (25.10-default vs 25.10-internal)" "$b" "$e"
done

log ""
log "=== summary: $N_DIVERGED / $N_TOTAL pairwise comparisons diverged ==="
log "raw logs, fixture repo, and per-row apt roots: $OUT/"
log "=== E2 done ==="
