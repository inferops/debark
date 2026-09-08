#!/usr/bin/env bash
#
# demo-gui-e2e.sh - the desktop GUI, end to end, across a simulated air gap.
#
# This is ../hack/demo-airgap.sh's sibling, and it was written by
# reading that script rather than inventing a second approach. The difference
# is the one that matters: the online builder here is not the debark CLI, it
# is the GUI's own Go layer. hack/e2e drives internal/app on top of
# internal/cliadapter — the readiness screen, the target screen, the tray, the
# build screen, the verify and export screens, through the same bound methods
# the frontend calls. If a flag is translated wrongly, an event never arrives
# or a path is handed across in the wrong dialect, this is where it shows.
#
# Stage 1 (online builder):  readiness -> keygen -> base -> tray -> build ->
#                            verify -> export, all through internal/app
# Stage 2 (offline target):  verify + install with --network none, then run
#                            the installed software
#
# The operator public key travels OUT OF BAND — it is copied to the target
# separately and is never on the media — because a key found beside the thing
# it signs proves nothing. The script asserts that.
#
# Why stage 1 also runs in a container: the GUI is a desktop application and
# its shipping platform is Linux, but a build needs an apt whose release
# matches the target. Running the builder half inside an image of the base's
# own release is the one arrangement that behaves identically on a Linux
# workstation, a Windows laptop and a CI runner, and it is what makes this
# script repeatable rather than a description of one machine. The Go layer
# under test is the same code either way; only apt's surroundings change.
#
# Usage:
#
#   hack/demo-gui-e2e.sh
#   DEBARK_GUI_E2E_PACKAGES=jq,curl hack/demo-gui-e2e.sh
#
# Environment:
#
#   DEBARK_GUI_E2E_OUT        work directory  (default: $TMPDIR/debark-gui-e2e)
#   DEBARK_CORE               the debark checkout (default: ..)
#   DEBARK_GUI_E2E_BASE       stock base id   (default: debian:12/minimal)
#   DEBARK_GUI_E2E_IMAGE      container image (default: debian:bookworm-slim)
#   DEBARK_GUI_E2E_PACKAGES   packages to bundle, comma separated (default: jq)
#   DEBARK_GUI_E2E_URLS       vendor .deb downloads, comma separated, each
#                               URL or URL=SHA256 (default: none). Setting this
#                               also installs ca-certificates in the builder
#                               container -- see the comment at stage 1.
#   DEBARK_GUI_E2E_PROVE      command run on the target to prove the software
#                               works (default: jq --version)
#
# The image must be the release the base describes. debark refuses the local
# apt backend when the host is not demonstrably the target's own release
# (core/base/synthesize.go's gate), so a mismatched pair does not quietly
# produce a wrong bundle — it fails.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
core="${DEBARK_CORE:-$repo/..}"
work="${DEBARK_GUI_E2E_OUT:-${TMPDIR:-/tmp}/debark-gui-e2e}"
base="${DEBARK_GUI_E2E_BASE:-debian:12/minimal}"
image="${DEBARK_GUI_E2E_IMAGE:-debian:bookworm-slim}"
packages="${DEBARK_GUI_E2E_PACKAGES:-jq}"
prove="${DEBARK_GUI_E2E_PROVE:-jq --version}"

# Git Bash rewrites anything that looks like a path when it crosses into a
# native Windows program, which is what breaks `docker -v` and `go build -o`
# there.
export MSYS_NO_PATHCONV=1

say()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31m!!! %s\033[0m\n' "$*" >&2; exit 1; }

# hostpath renders a shell path in the dialect docker and a native Windows Go
# toolchain understand. On Linux and macOS it is the identity.
#
# ../hack/demo-airgap.sh does this with an inline `case /[a-z]/*`,
# which is right for the paths it uses and wrong for two this script needs: it
# cannot convert /tmp (not a drive-letter path at all, though it is a real
# Windows directory), and on Linux it would happily turn a perfectly good
# /e/projects into "e:/projects". cygpath knows the mapping for real and
# exists in Git Bash, MSYS2 and Cygwin alike, so the platform test is on the
# shell rather than on the shape of the string.
hostpath() {
    case "$(uname -s)" in
        MINGW*|MSYS*|CYGWIN*) cygpath -m "$1" ;;
        *)                    printf '%s\n' "$1" ;;
    esac
}

command -v docker >/dev/null 2>&1 || fail "docker is not on PATH; this script needs a container runtime for the offline target"
command -v go     >/dev/null 2>&1 || fail "go is not on PATH"
[ -d "$core/cmd/debark" ] || fail "no debark checkout at $core (set DEBARK_CORE)"

rm -rf "$work"; mkdir -p "$work"
mount="$(hostpath "$work")"

say "work directory"
printf '  shell:  %s\n  mount:  %s\n' "$work" "$mount"

# ---------------------------------------------------------------------------
# Build the two static linux binaries the run needs.
#
# Both are built OUTSIDE either repository, into the work directory, so a run
# never leaves an artefact in a tree someone is about to commit.
# ---------------------------------------------------------------------------

say "building a static linux debark from $core"
( cd "$core" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$mount/debark" ./cmd/debark )

# CGO_ENABLED=0 is not only for portability. internal/app imports Wails'
# runtime package, and a cgo-enabled Linux build of it wants the WebKitGTK
# headers; the harness needs none of that, because it never opens a window.
say "building the GUI's end-to-end harness from $repo"
( cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$mount/gui-e2e" ./hack/e2e )

ls -la "$work/debark" "$work/gui-e2e"

# ---------------------------------------------------------------------------
# Stage 1 - the online builder. Everything here is the GUI's own Go layer.
# ---------------------------------------------------------------------------

say "stage 1: the GUI builds and signs a bundle (online)"
# A vendor .deb is fetched over https by core/fetch, and the -slim images carry
# no CA certificates, so without this the download fails with "certificate
# signed by unknown authority" and the build ends incomplete. apt itself does
# not need them -- its archive transport is plain http with signed indexes --
# which is why the default run does not pay for this step. Adding it silently
# to every run would also hide the interesting thing: how the application
# reports a vendor URL it could not fetch.
prep=""
if [ -n "${DEBARK_GUI_E2E_URLS:-}" ]; then
    say "installing ca-certificates for the vendor download"
    prep='apt-get update -qq >/dev/null && apt-get install -y -qq --no-install-recommends ca-certificates >/dev/null && '
fi

docker run --rm -v "$mount:/w" "$image" sh -c "${prep}exec /w/gui-e2e \
    -debark /w/debark \
    -work /w/builder \
    -base '$base' \
    -packages '$packages' \
    -urls '${DEBARK_GUI_E2E_URLS:-}'"

report="$work/builder/gui-e2e-report.json"
[ -f "$report" ] || fail "the harness wrote no report at $report"

say "what crossed the gap"
media="$work/builder/media/bundle"
[ -d "$media" ] || fail "the export produced no bundle at $media"
find "$media" -type f | sed "s|$media|<media>|" | sort

# The public key must reach the target separately. A bundle that carries the
# key it was signed with is self-certifying, which is to say worthless, so
# this is an assertion rather than a comment.
if find "$media" -type f \( -name '*.key' -o -name '*.pub' \) | grep -q .; then
    fail "key material is on the media; the operator key must travel out of band"
fi

# ---------------------------------------------------------------------------
# Stage 2 - the offline target. --network none is the whole point: it is the
# only step that proves the bundle is actually closed.
# ---------------------------------------------------------------------------

say "stage 2: verify and install on the offline target, with NO NETWORK"
docker run --rm --network none -v "$mount:/w" "$image" sh -c '
    set -e
    mkdir -p /media /keys
    cp -r /w/builder/media/bundle /media/bundle
    cp /w/builder/operator.pub /keys/          # out of band, NOT on the media

    echo "--- proving this container has no network ---"
    if getent hosts deb.debian.org >/dev/null 2>&1; then
        echo "a name resolved: this container is NOT isolated" >&2
        exit 1
    fi
    echo "deb.debian.org does not resolve"

    /w/debark verify /media/bundle --key /keys/operator.pub
    echo "--- install ---"
    /w/debark install /media/bundle --key /keys/operator.pub --yes
    echo "--- proving the installed software actually runs ---"
    '"$prove"'
'

say "the harness report"
# python3 and jq are not assumed on the machine running this script. The
# harness writes the report with json.MarshalIndent, so a field's nesting depth
# is its indentation, and anchoring each pattern to that depth picks out the
# top-level verdict without dragging in the same key from every nested object.
grep -E '^  "(ok|debark_version|base_id|arch|media_path)"' "$report" | sed 's/^ */  /'
grep -E '^    "(bundle_id|signed)"'  "$report" | sed 's/^ */  build. /'
grep -E '^      "package_count"'     "$report" | sed 's/^ */  build. /'

say "done - a GUI-built bundle verified and installed with --network none"
printf '  report: %s\n' "$report"
