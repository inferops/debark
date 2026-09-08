#!/usr/bin/env bash
#
# build-deb.sh — build the Debark .deb from an already-built binary.
#
# This script is deliberately standalone. It needs dpkg-deb, dpkg-shlibdeps
# (dpkg-dev), gzip and coreutils, and nothing else — no goreleaser, no nfpm,
# no debhelper, no dpkg-buildpackage. So the package can be built and proven
# by anyone with a checkout and a Debian-family machine, which is what makes
# docs/security-review.md §5 checkable rather than assertable.
#
# It is also the ONLY producer of the .deb. .goreleaser.yaml has no nfpms:
# block and calls this script from the linux build's post hook, so the package
# that lands under the release signature is the same package verify-deb.sh ran
# the twenty rows against. Two producers would mean §5 describes an artefact
# nobody shipped.
#
# It does NOT build the binary. Build that first, with EXACTLY the flags
# .goreleaser.yaml uses — this command and that file must agree, and the
# reason is in the next paragraph:
#
#   go run ./hack/copyfrontend
#   CGO_ENABLED=1 go build -tags "desktop,production,webkit2_41" \
#       -trimpath -buildmode=pie \
#       -ldflags "-s -w -X main.version=$VERSION" -o /tmp/debark-gui .
#
# Every flag above is load-bearing, and three of them are here because a
# package built without them is a package the release never produces:
#
#   * desktop,production — without them the binary compiles and then refuses
#     to run, printing "Wails applications will not build without the correct
#     build tags."
#   * webkit2_41 — without it the binary links webkit2gtk-4.0, which no
#     supported release ships.
#   * -buildmode=pie — Go with cgo defaults to buildmode=exe. Without pie,
#     lintian reports `W: hardening-no-pie` (and `hardening-no-bindnow`, which
#     pie clears too: Go's pie link is -z now).
#   * -s -w — Go does not strip. Without them, lintian reports
#     `E: unstripped-binary-or-object`.
#
# THIS HEADER USED TO OMIT -s -w AND -buildmode=pie, AND THAT IS THE WHOLE
# ORIGIN OF A lintian ERROR REPORTED AGAINST A PACKAGE THE RELEASE NEVER
# PRODUCES. Someone followed the command above, built an unstripped non-PIE
# binary, packaged it, and found `E: unstripped-binary-or-object` — a property
# of that build, not of the released artefact, which has carried `-s -w` from
# the start and gained pie with the reproducibility work. `lintian` on a
# package built with the command above is clean: no E, no W. See
# docs/packaging.md §4 for the transcript and the four-build attribution
# table, and docs/release.md §4.7 for the measurements behind both flags.
#
# One consequence of `-s -w` is owed to the reader here rather than left to be
# rediscovered: docs/security-review.md §5 row 16 used to corroborate the nine
# glibc setuid-family imports against the `_cgo_libc_set*` wrappers in the
# STATIC symbol table, which `-s` removes. packaging/verify-deb.sh now
# corroborates them a way that survives stripping; see its row 16 comment.
#
# The .deb is only as correct as the binary handed to it, so this script checks
# the ELF it is given rather than trusting the caller.
#
# Usage:
#   packaging/build-deb.sh --binary PATH [--version V] [--arch A] [--out DIR]
#
# or, equivalently and as the release pipeline calls it:
#
#   DEBARK_GUI_BINARY=... DEBARK_GUI_VERSION=... DEBARK_GUI_ARCH=...
#   DEBARK_GUI_OUTDIR=dist SOURCE_DATE_EPOCH=... packaging/build-deb.sh
#
# It writes exactly one *.deb into the output directory and prints its path,
# and it fails if a second one is there — .goreleaser.yaml identifies the
# package by glob for the checksum file, and therefore for the signature.
#
# Everything about the package that could be a lie is derived rather than
# written down:
#
#   * Depends comes from dpkg-shlibdeps against the real ELF. There is no
#     hand-written dependency list anywhere in this repository, on purpose:
#     the GUI links GTK through cgo and cannot be CGO_ENABLED=0, so its
#     shared-library surface is a property of the build, not of a document.
#   * Installed-Size comes from du.
#   * The changelog version comes from the package version.
#
# And two things about the package are enforced rather than intended:
#
#   * There are no maintainer scripts. Not a postinst, not a postrm, not
#     anything. Updating the desktop database and the icon cache is done by
#     dpkg triggers that desktop-file-utils and hicolor-icon-theme already
#     own; a package that runs its own update commands is a package running
#     code as root at install time for no reason. See docs/packaging.md.
#   * Nothing is installed outside /usr/bin, /usr/share/applications,
#     /usr/share/icons, /usr/share/man, /usr/share/metainfo and
#     /usr/share/doc/debark-gui.
#
# Reproducibility: every file's mtime is clamped to SOURCE_DATE_EPOCH (which
# defaults to the last commit touching packaging/) before dpkg-deb runs, and
# dpkg-deb honours the same variable for the archive members. Running this
# script twice on the same binary produces byte-identical .deb files.

set -euo pipefail

die() { printf 'build-deb.sh: %s\n' "$*" >&2; exit 1; }
note() { printf '  %s\n' "$*" >&2; }

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH= cd -- "$here/.." && pwd)

PKG=debark-gui
APPID=io.github.inferops.Debark
MAINTAINER=${DEBARK_GUI_MAINTAINER:-"The debark Authors <noreply@users.noreply.github.com>"}
HOMEPAGE=https://debark.dev

# ---------------------------------------------------------------------------
# Inputs. Every one can be given as a flag or as an environment variable, and
# the flag wins.
#
#   --binary   DEBARK_GUI_BINARY    the already-built desktop binary
#   --version  DEBARK_GUI_VERSION   default: derived from git, see below
#   --arch     DEBARK_GUI_ARCH      default: dpkg --print-architecture
#   --out      DEBARK_GUI_OUTDIR    default: build/deb
#              SOURCE_DATE_EPOCH      default: derived from git, see below
#
# The environment form exists because the release pipeline calls this script
# from a build hook, where passing arguments through is awkward and passing
# environment through is not. `.goreleaser.yaml` is the release work's file and
# calls this script with exactly those five variables, so they are part of
# this script's contract now: renaming one breaks the release.
#
# The other half of that contract is at the bottom of this file — this script
# writes EXACTLY ONE *.deb into the output directory, and checks that it did,
# because the release picks the package up by glob for the checksum file and
# the signature.
# ---------------------------------------------------------------------------
binary=${DEBARK_GUI_BINARY:-}
version=${DEBARK_GUI_VERSION:-}
arch=${DEBARK_GUI_ARCH:-}
outdir=${DEBARK_GUI_OUTDIR:-$repo/build/deb}
distribution=${DEBARK_GUI_DISTRIBUTION:-unstable}

while [ $# -gt 0 ]; do
	case $1 in
	--binary) binary=${2:?--binary needs a path}; shift 2 ;;
	--version) version=${2:?--version needs a value}; shift 2 ;;
	--arch) arch=${2:?--arch needs a value}; shift 2 ;;
	--out) outdir=${2:?--out needs a path}; shift 2 ;;
	-h | --help) sed -n '2,95p' "$0"; exit 0 ;;
	*) die "unknown argument: $1" ;;
	esac
done

[ -n "$binary" ] || die "no binary given: pass --binary or set DEBARK_GUI_BINARY (build it first; see the header of this file)"
[ -f "$binary" ] || die "no such file: $binary"

for tool in dpkg-deb dpkg-shlibdeps gzip du md5sum; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool not found on PATH (dpkg-shlibdeps is in dpkg-dev)"
done

# ---------------------------------------------------------------------------
# Version.
#
# The scheme matches the CLI's: the upstream version, with no Debian
# revision, because this is a native package with no separate Debian
# packaging branch. An untagged build gets 0.0.0~git<N>.g<sha>, where the
# tilde sorts it *below* every real release — so a developer build can never
# shadow a released one, which is the failure mode a plain "0.0.0-dev" has.
# ---------------------------------------------------------------------------
if [ -z "$version" ]; then
	if [ -n "${VERSION:-}" ]; then
		version=$VERSION
	elif tag=$(git -C "$repo" describe --tags --abbrev=0 2>/dev/null) && [ -n "$tag" ]; then
		version=${tag#v}
	elif sha=$(git -C "$repo" rev-parse --short=7 HEAD 2>/dev/null); then
		version="0.0.0~git$(git -C "$repo" rev-list --count HEAD).g$sha"
	else
		version=0.0.0~unknown
	fi
fi
case $version in
v*) version=${version#v} ;;
esac
# A Debian version must start with a digit and may not contain characters
# dpkg refuses to parse. Catch it here rather than three minutes later in
# dpkg-deb's error message.
case $version in
[0-9]*) : ;;
*) die "version must start with a digit: $version" ;;
esac

if [ -z "$arch" ]; then
	arch=$(dpkg --print-architecture)
fi

# ---------------------------------------------------------------------------
# Check the binary is the one this package is allowed to ship.
#
# A Wails desktop binary built without the `desktop,production` tags compiles
# cleanly and then refuses to run. That failure is invisible to every static
# check in the §5 checklist — the package would pass rows 1 to 19 and fail
# row 20. So look for the string the wrong binary prints, and for the
# WebKitGTK 4.1 SONAME the right one links.
# ---------------------------------------------------------------------------
if grep -qa 'Wails applications will not build without the correct build tags' "$binary"; then
	die "$binary was built without -tags desktop,production; it will not run. Rebuild (see this file's header)."
fi
if command -v objdump >/dev/null 2>&1; then
	sonames=$(objdump -p "$binary" 2>/dev/null | awk '$1=="NEEDED"{print $2}' || true)
	case $sonames in
	*libwebkit2gtk-4.1.so*) : ;;
	*libwebkit2gtk-4.0.so*) die "$binary links webkit2gtk-4.0; no supported release ships that ABI. Rebuild with -tags webkit2_41." ;;
	*) die "$binary does not link libwebkit2gtk at all. It is not the desktop app." ;;
	esac
fi

# ---------------------------------------------------------------------------
# SOURCE_DATE_EPOCH: the commit date of the last change to packaging/, so the
# package's timestamps move when the packaging moves and not otherwise.
# ---------------------------------------------------------------------------
if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	SOURCE_DATE_EPOCH=$(git -C "$repo" log -1 --format=%ct -- packaging 2>/dev/null || true)
	[ -n "$SOURCE_DATE_EPOCH" ] || SOURCE_DATE_EPOCH=$(date -u +%s)
fi
export SOURCE_DATE_EPOCH
rfc2822=$(LC_ALL=C date -u -d "@$SOURCE_DATE_EPOCH" '+%a, %d %b %Y %H:%M:%S +0000')

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
root=$stage/root

note "package     $PKG"
note "version     $version"
note "arch        $arch"
note "app id      $APPID"
note "binary      $binary"
note "epoch       $SOURCE_DATE_EPOCH ($rfc2822)"

# ---------------------------------------------------------------------------
# Lay out the filesystem. install(1) sets the mode at copy time, so no chmod
# pass is needed and no file can be missed by one.
# ---------------------------------------------------------------------------
install -d -m 0755 "$root/usr/bin"
install -m 0755 "$binary" "$root/usr/bin/$PKG"

install -d -m 0755 "$root/usr/share/applications"
install -m 0644 "$here/$PKG.desktop" "$root/usr/share/applications/$PKG.desktop"

icons=0
while IFS= read -r src; do
	rel=${src#"$here/icons/"}
	install -d -m 0755 "$root/usr/share/icons/$(dirname "$rel")"
	install -m 0644 "$src" "$root/usr/share/icons/$rel"
	icons=$((icons + 1))
done < <(find "$here/icons" -type f \( -name '*.png' -o -name '*.svg' \) | sort)
[ "$icons" -gt 0 ] || die "no icons found under $here/icons"
note "icons       $icons"

install -d -m 0755 "$root/usr/share/metainfo"
# The <releases> element is injected here rather than committed, because the
# only honest value for it is the version being built. See the comment in the
# metainfo file itself.
sed "s|^</component>$|  <releases>\n    <release version=\"$version\" date=\"$(LC_ALL=C date -u -d "@$SOURCE_DATE_EPOCH" '+%Y-%m-%d')\"/>\n  </releases>\n</component>|" \
	"$here/metainfo/$APPID.metainfo.xml" >"$root/usr/share/metainfo/$APPID.metainfo.xml"
chmod 0644 "$root/usr/share/metainfo/$APPID.metainfo.xml"

install -d -m 0755 "$root/usr/share/doc/$PKG"
install -m 0644 "$here/copyright" "$root/usr/share/doc/$PKG/copyright"

# The changelog's NAME is decided by the version, not by preference, and
# getting it wrong is an error rather than a style point.
#
# dpkg splits a version at its LAST hyphen: everything before is upstream,
# everything after is the Debian revision. A version with no hyphen is a
# NATIVE package and its changelog must be changelog.gz; a version with one is
# NON-native and its changelog must be changelog.Debian.gz. Name it wrongly
# and lintian reports
#     E: debark-gui: debian-changelog-file-missing-or-wrong-name
#
# This is not hypothetical and it is not only about hand-built packages.
# `.github/workflows/release.yml` fires on `v[0-9]+.[0-9]+.[0-9]+-*` as well as
# on `v[0-9]+.[0-9]+.[0-9]+`, so `v1.2.3-rc1` is a releasable tag; goreleaser
# passes `1.2.3-rc1` through as DEBARK_GUI_VERSION, and a snapshot build
# passes `1.2.3-SNAPSHOT-<sha>`. Both were built from one binary and both drew
# the E above while the plain `1.2.3` package was clean. See docs/packaging.md
# §9.
#
# ONE THING THIS DOES NOT FIX, deliberately, because it changes what the
# release publishes rather than how it is packaged: a Debian revision sorts
# ABOVE the bare upstream version, so `1.2.3-rc1` is NEWER than `1.2.3` to
# dpkg, and a release candidate would shadow the release it precedes. That is
# the exact failure the `0.0.0~git<N>.g<sha>` scheme above uses `~` to avoid.
# Mapping a semver pre-release's `-` to Debian's `~` would fix the ordering and
# keep the package native, and it is recommended in docs/packaging.md §9 for
# whoever owns the release's version string. It is not done here silently.
#
# gzip -n drops the timestamp and the original filename from the member header,
# which is what makes the result reproducible.
case $version in
*-*)
	changelog_name=changelog.Debian.gz
	note "version     $version has a Debian revision ('${version##*-}'), so this is a NON-native package"
	note "            changelog is ${changelog_name}; note that dpkg sorts it ABOVE plain ${version%-*}"
	;;
*)
	changelog_name=changelog.gz
	;;
esac
note "changelog   /usr/share/doc/$PKG/$changelog_name"
sed -e "s|@VERSION@|$version|g" \
	-e "s|@DISTRIBUTION@|$distribution|g" \
	-e "s|@MAINTAINER@|$MAINTAINER|g" \
	-e "s|@DATE@|$rfc2822|g" \
	"$here/changelog.in" | gzip -9n >"$root/usr/share/doc/$PKG/$changelog_name"
chmod 0644 "$root/usr/share/doc/$PKG/$changelog_name"

# The man page. gzip -9n for the same reason as the changelog: maximum
# compression, and -n so no timestamp and no original filename reach the
# member header, which is what keeps the .deb byte-reproducible.
install -d -m 0755 "$root/usr/share/man/man1"
gzip -9nc "$here/$PKG.1" >"$root/usr/share/man/man1/$PKG.1.gz"
chmod 0644 "$root/usr/share/man/man1/$PKG.1.gz"

# ---------------------------------------------------------------------------
# Depends, derived.
#
# dpkg-shlibdeps reads the ELF's DT_NEEDED entries, maps each SONAME to the
# package that owns it through that package's shlibs/symbols file, and emits
# a minimum version derived from the symbols actually referenced. That last
# part is why this is derived and not written: the answer depends on which
# symbols this build of this binary uses, and it changes when the code does.
#
# It needs a debian/control to exist and a package name to attribute the
# dependency to; -O sends the result to stdout instead of debian/substvars.
# --ignore-missing-info downgrades "this library has no dependency
# information" from fatal to a warning, which matters for out-of-archive
# libraries; the warnings are printed, not swallowed.
# ---------------------------------------------------------------------------
shlibdir=$stage/shlibdeps
install -d "$shlibdir/debian"
cat >"$shlibdir/debian/control" <<EOF
Source: $PKG

Package: $PKG
Architecture: any
EOF
note "running dpkg-shlibdeps against $binary"
depends=$(cd "$shlibdir" && dpkg-shlibdeps -O --ignore-missing-info -pshlibs "$root/usr/bin/$PKG" 2>"$stage/shlibdeps.err" | sed 's/^shlibs:Depends=//')
if [ -s "$stage/shlibdeps.err" ]; then
	sed 's/^/  dpkg-shlibdeps: /' "$stage/shlibdeps.err" >&2
fi
[ -n "$depends" ] || die "dpkg-shlibdeps produced no dependencies; that cannot be right for a cgo GTK binary"
printf '%s\n' "$depends" | tr ',' '\n' | sed 's/^ */  dep         /' >&2

installed_size=$(du -k -s --apparent-size "$root" | cut -f1)

install -d -m 0755 "$root/DEBIAN"
cat >"$root/DEBIAN/control" <<EOF
Package: $PKG
Version: $version
Architecture: $arch
Maintainer: $MAINTAINER
Installed-Size: $installed_size
Depends: $depends
Section: admin
Priority: optional
Homepage: $HOMEPAGE
Description: prepare Debian and Ubuntu software transfers for air-gapped machines
 Debark is the desktop front end for debark. It runs on an online builder
 machine: pick a target operating system, pick packages from a catalogue
 built out of that target's own apt indexes, add vendor .deb URLs or local
 files, and build a signed bundle to carry to a machine with no network.
 .
 Nothing changes on the offline machine. Over there it is still the debark
 command-line tool, or still plain apt.
 .
 Debark decides nothing itself: it contains no dependency resolution, no
 version comparison and no dependency reasoning. Every such decision is made
 by debark, by asking the target release's own apt.
 .
 It needs no root or other elevated privilege at any point, installs no
 service and no system configuration, collects no telemetry, and contacts no
 server operated by this project.
EOF
chmod 0644 "$root/DEBIAN/control"

# md5sums, in dpkg's format: hash, two spaces, path relative to / with no
# leading slash. dpkg-deb does not generate this; without it `dpkg -V` and
# `debsums` have nothing to check.
(cd "$root" && find . -path ./DEBIAN -prune -o -type f -print0 |
	sort -z | xargs -0 md5sum | sed 's| \./| |' >DEBIAN/md5sums)
chmod 0644 "$root/DEBIAN/md5sums"

# Clamp every timestamp. dpkg-deb honours SOURCE_DATE_EPOCH for the ar
# member headers but takes the tar entries' mtimes from the filesystem.
find "$root" -exec touch --no-dereference --date="@$SOURCE_DATE_EPOCH" {} +

mkdir -p "$outdir"
deb="$outdir/${PKG}_${version}_${arch}.deb"

# Any package this script produced on an earlier run is ours to clear, and
# clearing it is what makes the "exactly one" check below meaningful rather
# than a trap for whoever forgot --clean. Only our own name is touched;
# anything else in the output directory is left alone, because on the release
# path that directory is goreleaser's `dist` and is full of other artefacts.
rm -f "$outdir/${PKG}"_*.deb

# --root-owner-group makes every entry root:root without needing fakeroot or
# actual root, which is what lets this script run unprivileged. -Zxz because
# every dpkg that can read a 4.1-era package can read xz; zstd is newer and
# buys nothing here.
dpkg-deb --root-owner-group -Zxz -z9 --build "$root" "$deb" >/dev/null

# The release picks the package up by glob, for the checksum file and hence
# for the signature, so "exactly one .deb in the output directory" is part of
# this script's contract with .goreleaser.yaml. Check it rather than assume
# it: a second .deb here would be signed-adjacent and unnoticed.
count=0
for f in "$outdir"/*.deb; do
	[ -e "$f" ] || continue
	count=$((count + 1))
done
if [ "$count" -ne 1 ]; then
	printf 'build-deb.sh: expected exactly one .deb in %s, found %d:\n' "$outdir" "$count" >&2
	ls -1 "$outdir"/*.deb >&2 2>/dev/null || true
	die "the release identifies the package by glob; more than one is ambiguous"
fi

printf '%s\n' "$deb"
