#!/usr/bin/env bash
#
# verify-deb.sh — run the twenty rows of docs/security-review.md §5 against a
# built .deb and print, for each row, the command and its actual output.
#
# The §5 checklist was written before the package existed, by the security
# review, and it is this package's acceptance test. This script is the
# executable form of it. It does not summarise: every row prints the command
# it ran and what that command actually said, because a checklist that prints
# ticks is a checklist nobody can audit.
#
# §5.2 says the two rows that prove anything are 19 and 20 — rows 1 to 18 are
# each defeatable by a package that does the privileged thing somewhere the
# row did not look, and a before/after diff of a real install is not. So this
# script runs 19 and 20 too, rather than leaving them to a human:
#
#   Row 19 needs root and a throwaway machine. It installs the package,
#          removes it, and diffs `getent group`, `systemctl list-unit-files`
#          and `find / -perm /6000` across both. Run it in a container.
#   Row 20 needs an X server and a non-root user with no sudo. It drops to
#          that user, starts a session bus and the installed binary under
#          Xvfb, waits for the window to appear, and photographs it if
#          ImageMagick is present. Everything it starts is bounded by a
#          timeout and torn down by pid.
#
# Both are skipped, loudly and as SKIP rather than PASS, when their
# preconditions are absent. A skipped row is reported as skipped.
#
# Row 16's corroboration is built to survive a STRIPPED binary, because the
# shipped one is stripped: .goreleaser.yaml links with `-s -w`. See the long
# comment above that row.
#
# Usage:
#   packaging/verify-deb.sh PACKAGE.deb [--display :101] [--user debark]
#
# Environment:
#   SHOT_DIR       where row 20 writes its screenshot (default: a temp dir
#                  that is deleted on exit)
#   SHOT_NAME      a suffix for that filename, so a run per release does not
#                  overwrite the previous release's photograph
#   DISPLAY_NUM    X display for row 20 (default :101; give each concurrent
#                  run its own number)
#   VERIFY_USER    the unprivileged user row 20 drops to (default debark)
#   START_WAIT     seconds to wait for the window (default 15)
#   ROW20_TIMEOUT  hard bound on the application process (default 90)
#
# Exit status is 0 only if every row that ran passed. A skipped row does not
# make the run fail, but the summary says so and the count is printed.

set -uo pipefail

deb=
display=${DISPLAY_NUM:-:101}
runuser=${VERIFY_USER:-debark}
startwait=${START_WAIT:-15}
row20timeout=${ROW20_TIMEOUT:-90}

while [ $# -gt 0 ]; do
	case $1 in
	--display) display=${2:?}; shift 2 ;;
	--user) runuser=${2:?}; shift 2 ;;
	--wait) startwait=${2:?}; shift 2 ;;
	-h | --help) sed -n '2,48p' "$0"; exit 0 ;;
	*) deb=$1; shift ;;
	esac
done

[ -n "$deb" ] && [ -f "$deb" ] || { echo "usage: verify-deb.sh PACKAGE.deb" >&2; exit 2; }
deb=$(readlink -f "$deb")

PKG=debark-gui
work=$(mktemp -d)
ROOT=$work/root

# Row 20 starts an X server, a window manager, a session bus and the
# application. Every one of them is tracked BY PID and torn down by pid on the
# way out — never by name. This runs on machines with other containers and
# other people's Xvfb processes on them, and `pkill Xvfb` has cost this project
# time before. `reap` is idempotent, so the row can call it early and the trap
# can call it again.
TRACKED_PIDS=
track_pid() { [ -n "${1:-}" ] && TRACKED_PIDS="$TRACKED_PIDS $1"; }
reap() {
	local p
	for p in $TRACKED_PIDS; do kill -TERM "$p" 2>/dev/null; done
	[ -n "$TRACKED_PIDS" ] && sleep 1
	for p in $TRACKED_PIDS; do kill -KILL "$p" 2>/dev/null; done
	TRACKED_PIDS=
	return 0
}
trap 'reap; rm -rf "$work"' EXIT

pass=0; fail=0; skip=0
declare -a RESULTS

hdr() { printf '\n=== Row %-2s %s\n' "$1" "$2"; }
cmd() { printf '$ %s\n' "$*"; }
out() { if [ -s "$1" ]; then sed 's/^/| /' "$1"; else printf '| (no output)\n'; fi; }
ok() { pass=$((pass+1)); RESULTS+=("$1 PASS $2"); printf '=> PASS\n'; }
no() { fail=$((fail+1)); RESULTS+=("$1 FAIL $2"); printf '=> FAIL: %s\n' "$3"; }
sk() { skip=$((skip+1)); RESULTS+=("$1 SKIP $2"); printf '=> SKIP: %s\n' "$3"; }

printf 'verify-deb.sh — docs/security-review.md §5, twenty rows\n'
printf 'package: %s\n' "$deb"
printf 'host:    %s\n' "$(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME" || uname -a)"
printf 'date:    %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"

cmd "dpkg-deb -R $deb \$ROOT"
dpkg-deb -R "$deb" "$ROOT" || { echo "cannot unpack"; exit 2; }

# ---------------------------------------------------------------------- 1
hdr 1 "No setuid or setgid file"
cmd "find \$ROOT -perm /6000"
find "$ROOT" -perm /6000 >"$work/r1" 2>&1; out "$work/r1"
[ -s "$work/r1" ] && no 1 "setuid/setgid" "$(cat "$work/r1")" || ok 1 "setuid/setgid"

# ---------------------------------------------------------------------- 2
hdr 2 "No file capabilities"
if command -v getcap >/dev/null 2>&1; then
	cmd "find \$ROOT -type f -exec getcap {} +"
	find "$ROOT" -type f -exec getcap {} + >"$work/r2" 2>&1; out "$work/r2"
	[ -s "$work/r2" ] && no 2 "file caps" "$(cat "$work/r2")" || ok 2 "file caps"
else
	sk 2 "file caps" "getcap not installed (libcap2-bin)"
fi

# ---------------------------------------------------------------------- 3
hdr 3 "No maintainer script at all"
cmd "ls -A \$ROOT/DEBIAN/"
ls -A "$ROOT/DEBIAN/" >"$work/r3" 2>&1; out "$work/r3"
got=$(sort "$work/r3" | tr '\n' ' ' | sed 's/ *$//')
if [ "$got" = "control md5sums" ]; then ok 3 "DEBIAN contents"
else no 3 "DEBIAN contents" "expected exactly 'control md5sums', got '$got'"; fi

# ---------------------------------------------------------------------- 4
hdr 4 "If a postinst is unavoidable it does exactly one permitted thing"
if [ -e "$ROOT/DEBIAN/postinst" ]; then
	cmd "cat \$ROOT/DEBIAN/postinst"
	cat "$ROOT/DEBIAN/postinst" >"$work/r4"; out "$work/r4"
	no 4 "postinst" "a postinst exists; row 3 says it should not, and this row must then be read by a human"
else
	printf '| there is no postinst, so this row has nothing to constrain\n'
	ok 4 "postinst"
fi

# ---------------------------------------------------------------------- 5
hdr 5 "No preinst, prerm or postrm"
cmd "ls \$ROOT/DEBIAN/{preinst,prerm,postrm}"
: >"$work/r5"
for s in preinst prerm postrm; do
	[ -e "$ROOT/DEBIAN/$s" ] && echo "$s present" >>"$work/r5"
done
out "$work/r5"
[ -s "$work/r5" ] && no 5 "pre/post scripts" "$(cat "$work/r5")" || ok 5 "pre/post scripts"

# ---------------------------------------------------------------------- 6
hdr 6 "No systemd unit, init script or user unit"
cmd "find \$ROOT -path '*systemd*' -o -path '*init.d*'"
find "$ROOT" \( -path '*systemd*' -o -path '*init.d*' \) -print >"$work/r6" 2>&1; out "$work/r6"
[ -s "$work/r6" ] && no 6 "units" "$(cat "$work/r6")" || ok 6 "units"

# ---------------------------------------------------------------------- 7..10
for spec in "7:polkit action or rule:*polkit-1*" \
	"8:udev rule:*udev*" \
	"9:D-Bus system-bus service or policy:*dbus-1/system*" \
	"10:sudoers fragment:*sudoers*"; do
	n=${spec%%:*}; rest=${spec#*:}; what=${rest%%:*}; pat=${rest#*:}
	hdr "$n" "No $what"
	cmd "find \$ROOT -path '$pat'"
	find "$ROOT" -path "$pat" -print >"$work/r$n" 2>&1; out "$work/r$n"
	if [ -s "$work/r$n" ]; then no "$n" "$what" "$(cat "$work/r$n")"; else ok "$n" "$what"; fi
done

# ---------------------------------------------------------------------- 11
hdr 11 "No PAM, /etc/cron*, /etc/profile.d, /etc/ld.so.conf.d"
cmd "find \$ROOT/etc -type f"
if [ -d "$ROOT/etc" ]; then find "$ROOT/etc" -type f -print >"$work/r11" 2>&1; else : >"$work/r11"; fi
if [ -d "$ROOT/etc" ]; then printf '| $ROOT/etc exists\n'; else printf '| $ROOT/etc does not exist at all\n'; fi
out "$work/r11"
[ -s "$work/r11" ] && no 11 "/etc" "$(cat "$work/r11")" || ok 11 "/etc"

# ---------------------------------------------------------------------- 12
hdr 12 "No file outside the six permitted prefixes"
cmd "dpkg-deb -c $deb"
dpkg-deb -c "$deb" >"$work/r12.full" 2>&1; out "$work/r12.full"
grep -E '^-|^l' "$work/r12.full" | awk '{print $NF}' | sed 's|^\./|/|' >"$work/r12.files"
: >"$work/r12"
while IFS= read -r f; do
	case $f in
	/usr/bin/*|/usr/share/applications/*|/usr/share/icons/*|/usr/share/doc/debark-gui/*|/usr/share/man/*|/usr/share/metainfo/*) : ;;
	*) echo "$f" >>"$work/r12" ;;
	esac
done <"$work/r12.files"
if [ -s "$work/r12" ]; then
	printf 'outside the permitted prefixes:\n'; out "$work/r12"; no 12 "install prefixes" "$(cat "$work/r12")"
else
	printf '| every regular file is under one of the six permitted prefixes\n'; ok 12 "install prefixes"
fi

# ---------------------------------------------------------------------- 13
hdr 13 "0644 files, 0755 dirs, 0755 binary, all root:root"
cmd "dpkg-deb -c $deb   # modes and ownership"
: >"$work/r13"
while IFS= read -r line; do
	mode=$(echo "$line" | awk '{print $1}')
	owner=$(echo "$line" | awk '{print $2}')
	path=$(echo "$line" | awk '{print $NF}')
	[ "$owner" = "root/root" ] || echo "not root:root: $owner $path" >>"$work/r13"
	case $mode in
	d*) [ "$mode" = "drwxr-xr-x" ] || echo "dir not 0755: $mode $path" >>"$work/r13" ;;
	-*)
		case $path in
		./usr/bin/*) [ "$mode" = "-rwxr-xr-x" ] || echo "binary not 0755: $mode $path" >>"$work/r13" ;;
		*) [ "$mode" = "-rw-r--r--" ] || echo "file not 0644: $mode $path" >>"$work/r13" ;;
		esac ;;
	*) echo "unexpected entry type: $mode $path" >>"$work/r13" ;;
	esac
done <"$work/r12.full"
if [ -s "$work/r13" ]; then out "$work/r13"; no 13 "modes/ownership" "$(head -5 "$work/r13")"
else printf '| every entry is root/root; dirs 0755, data 0644, /usr/bin/%s 0755\n' "$PKG"; ok 13 "modes/ownership"; fi

# ---------------------------------------------------------------------- 14
hdr 14 "No Depends/Recommends/Suggests that grants privilege"
cmd "dpkg-deb -f $deb Depends Recommends Suggests"
dpkg-deb -f "$deb" Depends Recommends Suggests >"$work/r14" 2>&1; out "$work/r14"
if grep -Eqi '(^|[ ,:])(policykit-1|pkexec|sudo|gksu|kdesudo|libpolkit-gobject[^ ]*|libcap-ng[^ ]*|libcap2-bin|udisks2)( |,|$|\()' "$work/r14"; then
	no 14 "privileged deps" "a privilege-granting package is listed above"
else
	printf '| no policykit, pkexec, sudo, gksu, udisks or libcap in any of the three fields\n'
	ok 14 "privileged deps"
fi

# ---------------------------------------------------------------------- 15
hdr 15 ".desktop Exec= is the plain binary"
cmd "grep -E '^(Exec|Terminal|TryExec)=' \$ROOT/usr/share/applications/$PKG.desktop"
grep -E '^(Exec|Terminal|TryExec)=' "$ROOT/usr/share/applications/$PKG.desktop" >"$work/r15" 2>&1; out "$work/r15"
execline=$(grep -m1 '^Exec=' "$work/r15" | cut -d= -f2-)
term=$(grep -m1 '^Terminal=' "$work/r15" | cut -d= -f2-)
if [ "$execline" = "/usr/bin/$PKG" ] && [ "$term" = "false" ]; then
	ok 15 "desktop Exec"
else
	no 15 "desktop Exec" "Exec='$execline' Terminal='$term'"
fi

# ---------------------------------------------------------------------- 16
#
# Row 16 as written is "the binary is not linked against libcap or libpolkit
# and makes no setuid/setgid/seteuid/capset call", checked with objdump/nm
# plus `go list -deps .`.
#
# The nm half needs care, and getting it wrong is how this row produces a
# false failure. EVERY cgo-enabled Go binary on Linux imports setuid,
# setgid, seteuid, setegid, setreuid, setregid, setresuid, setresgid and
# setgroups from glibc, because runtime/cgo/linux_syscall.c compiles nine
# _cgo_libc_set* wrappers that the syscall package calls when it has to
# change credentials on every thread. A two-line cgo hello-world imports the
# identical nine; the measurement is in docs/packaging.md. The GUI links GTK
# through cgo and cannot be CGO_ENABLED=0, so it has them too, and their
# presence says nothing about whether this application ever calls one.
#
# THE SHIPPED BINARY IS STRIPPED, and that is what this row's corroboration
# has to survive. .goreleaser.yaml links with `-s -w` (docs/release.md §4.7:
# it is what keeps `lintian` clean of `E: unstripped-binary-or-object`, and
# the decision was to keep it). `-s` removes the STATIC symbol table, so the
# nine `_cgo_libc_set*` wrappers this row used to match each import against
# are not there to match: 9 of them in an unstripped build, 0 in the shipped
# one. The property held; the evidence path did not, and the package scored
# 19/20 on its own checklist for want of it. So the corroboration below is
# built out of things `-s` cannot remove.
#
# Four checks, and each of them can fail:
#
#   16a  no libcap, libcap-ng or libpolkit in DT_NEEDED — a real link, and one
#        no cgo runtime introduces. Dynamic, survives stripping.
#   16b  no capability-manipulating symbol at all (capset, capget, cap_*,
#        polkit_*, pkexec). prctl is deliberately not checked: it is not a
#        privilege-granting call, and legitimate libraries in this link use
#        it. Dynamic, survives stripping.
#   16c  the binary's credential- and privilege-related dynamic imports are
#        EXACTLY the nine runtime/cgo names — no more and no fewer. The watch
#        list is wider than those nine (it also holds setfsuid, setfsgid,
#        initgroups and every capability and polkit name), so a tenth
#        privilege import fails the row on a stripped binary. And a MISSING
#        one fails too, because the whole argument here is "these nine are the
#        cgo runtime's, present whether or not anything calls them"; a binary
#        carrying eight of them is not the shape that argument describes and a
#        person should look at it. Dynamic, survives stripping.
#   16d  each of the nine is reached from EXACTLY ONE direct call site in the
#        disassembly — the single `_cgo_libc_<name>` wrapper. objdump names a
#        PLT stub from the dynamic relocations, so `call <addr> <setuid@plt>`
#        is still labelled on a stripped binary. A SECOND caller means
#        something other than the runtime wrapper calls it, and fails the row.
#        This is the check that catches C code in the binary calling setuid()
#        directly — the case 16c cannot see, because the import is already
#        there for the runtime's sake.
#        It fails only on evidence of an extra caller, never on absence of
#        evidence: a count of zero means this toolchain annotates the
#        disassembly in a shape this check does not recognise, and it is
#        reported as uncorroborated rather than treated as a finding. A row
#        that fails on a binutils change is a row that gets waived.
#   16e  when the static symbol table is present — a hand build without
#        `-s -w` — the original wrapper match is required as well. It is extra
#        evidence on top of 16c and 16d, not a substitute for them.
#
# What this row still cannot see, stated so nobody reads it as more than it
# is: a call to one of the nine from C code inside the binary, if that call
# were inlined or otherwise left no distinguishable call site. The
# discriminators for that are the `go list -deps .` half of the row and rows
# 19 and 20 — §5.2 says rows 1 to 18 are each individually defeatable, and
# this row is one of them.
#
# The `go list -deps .` half of the row needs the source tree and a Go
# toolchain, which a .deb does not carry. It is run at build time and its
# output is recorded in docs/packaging.md; this script says so rather than
# pretending it covered it.
hdr 16 "Binary is not linked against libcap or libpolkit and makes no setuid call"
bin=$ROOT/usr/bin/$PKG

# The nine runtime/cgo credential wrappers, in LC_ALL=C order. This list is
# the row's allow-list; adding to it is a design decision, not a test fix.
LC_ALL=C sort >"$work/r16.expected" <<'EOF'
setegid
seteuid
setgid
setgroups
setregid
setresgid
setresuid
setreuid
setuid
EOF

cmd "objdump -p $bin | grep NEEDED"
objdump -p "$bin" 2>/dev/null | awk '$1=="NEEDED"{print "  NEEDED  " $2}' >"$work/r16.needed"; out "$work/r16.needed"

# Every undefined dynamic symbol, then filtered to the privilege watch list.
# nm's format is "<spaces>U name@VERSION"; objdump -T's is the fallback for a
# machine with binutils' nm absent.
cmd "nm -D --undefined-only $bin   # filtered to the privilege watch list"
if nm -D --undefined-only "$bin" >"$work/r16.nm" 2>/dev/null && [ -s "$work/r16.nm" ]; then
	sed -n 's/^[[:space:]]*[UwW][[:space:]]\{1,\}\([A-Za-z_][A-Za-z0-9_]*\).*$/\1/p' "$work/r16.nm"
else
	objdump -T "$bin" 2>/dev/null | awk '/\*UND\*/{print $NF}' | sed 's/@.*//'
fi | LC_ALL=C sort -u >"$work/r16.undef"
grep -Ex '(set(uid|gid|euid|egid|reuid|regid|resuid|resgid|fsuid|fsgid|groups)|initgroups|cap(set|get)|cap_[a-z0-9_]+|polkit_[a-z0-9_]+|pkexec)' \
	"$work/r16.undef" >"$work/r16.syms" 2>/dev/null
sed 's/^/  U /' "$work/r16.syms" >"$work/r16.symsp"; out "$work/r16.symsp"

cmd "nm $bin | grep _cgo_libc_set   # present only if the binary is unstripped"
nm "$bin" 2>/dev/null | grep -E '_cgo_libc_set' >"$work/r16.cgo" 2>&1 || true
out "$work/r16.cgo"

# 16d. One objdump -d pass; count direct call/jmp references to each PLT stub.
# The stub's own indirect `jmp *0x..(%rip)` is annotated with the GLIBC symbol
# rather than with <name@plt>, so it is not counted as a caller.
cmd "objdump -d $bin | count direct call/jmp sites per <name@plt>"
: >"$work/r16.calls"
if command -v objdump >/dev/null 2>&1; then
	timeout 600 objdump -d "$bin" 2>/dev/null |
		grep -oE '(call|jmp)q?[[:space:]]+[0-9a-f]+[[:space:]]+<[A-Za-z_][A-Za-z0-9_]*@plt>' |
		sed 's/.*<//; s/@plt>$//' | LC_ALL=C sort | uniq -c |
		awk '{print $2, $1}' >"$work/r16.callsall" 2>/dev/null
	while IFS= read -r s; do
		[ -n "$s" ] || continue
		n=$(awk -v s="$s" '$1==s{print $2}' "$work/r16.callsall")
		printf '%s %s\n' "$s" "${n:-0}" >>"$work/r16.calls"
	done <"$work/r16.expected"
	sed 's/^/  /' "$work/r16.calls" >"$work/r16.callsp"; out "$work/r16.callsp"
else
	printf '| objdump not installed; 16d not run\n'
fi

r16bad=
r16note=

# 16a
grep -Eqi 'libcap[-.]|libcap-ng|libpolkit' "$work/r16.needed" && r16bad="DT_NEEDED includes libcap, libcap-ng or libpolkit"

# 16b
if grep -Eqx '(cap(set|get)|cap_[a-z0-9_]+|polkit_[a-z0-9_]+|pkexec)' "$work/r16.syms"; then
	r16bad="${r16bad:+$r16bad; }imports a capability- or polkit-manipulating symbol"
fi

# 16c — exactly the nine, no more and no fewer.
extra=$(LC_ALL=C comm -13 "$work/r16.expected" "$work/r16.syms" | tr '\n' ' ' | sed 's/ *$//')
missing=$(LC_ALL=C comm -23 "$work/r16.expected" "$work/r16.syms" | tr '\n' ' ' | sed 's/ *$//')
if [ -n "$extra" ]; then
	r16bad="${r16bad:+$r16bad; }privilege import(s) beyond the nine runtime/cgo wrappers: $extra"
fi
if [ -n "$missing" ]; then
	r16bad="${r16bad:+$r16bad; }expected runtime/cgo import(s) absent, so this is not the cgo-linked binary the row reasons about: $missing"
fi

# 16d — more than one caller into a credential PLT stub is a finding.
if [ -s "$work/r16.calls" ]; then
	uncorroborated=
	while read -r s n; do
		if [ "$n" -gt 1 ]; then
			r16bad="${r16bad:+$r16bad; }$s is called from $n places, not just the runtime/cgo wrapper"
		elif [ "$n" -eq 0 ]; then
			uncorroborated="${uncorroborated:+$uncorroborated }$s"
		fi
	done <"$work/r16.calls"
	[ -n "$uncorroborated" ] &&
		r16note="16d found no annotated call site for: $uncorroborated (disassembly shape not recognised; 16c carries the row)"
fi

# 16e — the original wrapper match, when the static symbol table survived.
if [ -s "$work/r16.cgo" ]; then
	while IFS= read -r s; do
		[ -n "$s" ] || continue
		grep -q "_cgo_libc_$s\$" "$work/r16.cgo" ||
			r16bad="${r16bad:+$r16bad; }$s is imported with no runtime/cgo wrapper behind it"
	done <"$work/r16.syms"
fi

if [ -n "$r16bad" ]; then
	no 16 "privilege symbols" "$r16bad"
else
	printf '| 16a no libcap/libcap-ng/libpolkit in DT_NEEDED.\n'
	printf '| 16b no capability or polkit symbol of any kind.\n'
	printf '| 16c the privilege-related dynamic imports are exactly the nine runtime/cgo names,\n'
	printf '|     no more and no fewer — so a tenth one would fail this row on a stripped binary.\n'
	if [ -s "$work/r16.calls" ]; then
		printf '| 16d each of the nine is reached from exactly one direct call site: the single\n'
		printf '|     _cgo_libc_<name> wrapper. A second caller would fail this row.\n'
	fi
	if [ -s "$work/r16.cgo" ]; then
		printf '| 16e this binary is unstripped, so the wrapper match ran too and passed. The\n'
		printf '|     shipped binary is stripped (-s -w) and 16c/16d carry the row there.\n'
	else
		printf '| 16e this binary is stripped, as the shipped one is, so there is no static symbol\n'
		printf '|     table and no wrapper match. 16c and 16d do not need one.\n'
	fi
	[ -n "$r16note" ] && printf '| NOTE: %s\n' "$r16note"
	printf '| See docs/packaging.md for the control experiment, the negative controls that make\n'
	printf '| this row fail, and the `go list -deps` half of the row.\n'
	ok 16 "privilege symbols"
fi

# ---------------------------------------------------------------------- 17
hdr 17 "Nothing installed into the operator's home"
cmd "dpkg-deb -c $deb | grep -E '/home|/root'"
grep -E '(^| )\./(home|root)(/|$)' "$work/r12.full" >"$work/r17" 2>&1
out "$work/r17"
[ -s "$work/r17" ] && no 17 "home" "$(cat "$work/r17")" || ok 17 "home"

# ---------------------------------------------------------------------- 18
hdr 18 "lintian reports no privilege-class tag"
if command -v lintian >/dev/null 2>&1; then
	cmd "lintian --no-cfg --display-info --display-experimental --pedantic $deb"
	lintian --no-cfg --display-info --display-experimental --pedantic "$deb" >"$work/r18" 2>&1
	out "$work/r18"
	grep -Ei 'setuid-binary|setgid-binary|elevated-privileges|maintainer-script-should-not|privileged-|setuid-gid-binary' "$work/r18" >"$work/r18.bad" 2>&1
	if [ -s "$work/r18.bad" ]; then out "$work/r18.bad"; no 18 "lintian" "$(cat "$work/r18.bad")"
	else printf '| no setuid-binary, setgid-binary, elevated-privileges, maintainer-script-should-not-* or privileged-* tag\n'; ok 18 "lintian"; fi
else
	sk 18 "lintian" "lintian not installed"
fi

# ---------------------------------------------------------------------- 19
hdr 19 "Install and remove change no permission, group or service state"
if [ "$(id -u)" != 0 ]; then
	sk 19 "install/remove diff" "needs root; run this script in a throwaway container"
else
	snap() {
		local d=$1; mkdir -p "$d"
		getent group  >"$d/group"
		getent passwd >"$d/passwd"
		# The row names `systemctl list-unit-files`. That command reads unit
		# files off disk and needs no running init, but it does need the
		# systemd package present at all — in a container with none it is
		# vacuous. So the on-disk unit and autostart directories are diffed
		# beside it, and the output below says which of the two carried
		# weight on this machine.
		( systemctl list-unit-files --no-pager --no-legend 2>/dev/null || echo '(systemctl unavailable on this machine)' ) >"$d/units"
		find /etc/systemd /lib/systemd /usr/lib/systemd /etc/init.d /etc/xdg/autostart \
			-type f 2>/dev/null | sort >"$d/units_fs"
		find / -xdev -perm /6000 -type f 2>/dev/null | sort >"$d/suid"
		( ls -la /etc/sudoers.d 2>/dev/null; cat /etc/sudoers 2>/dev/null ) >"$d/sudoers"
		find /etc/polkit-1 /usr/share/polkit-1 /etc/udev/rules.d /lib/udev/rules.d \
			/etc/dbus-1 /usr/share/dbus-1 -type f 2>/dev/null | sort >"$d/rules"
	}
	cmd "snapshot before  (getent group+passwd / systemctl list-unit-files / on-disk unit+autostart dirs / find / -xdev -perm /6000 / polkit+udev+dbus rules / sudoers)"
	snap "$work/before"
	printf '| lines: group=%s passwd=%s units(systemctl)=%s units(on disk)=%s suid=%s rules=%s\n' \
		"$(wc -l <"$work/before/group")" "$(wc -l <"$work/before/passwd")" \
		"$(wc -l <"$work/before/units")" "$(wc -l <"$work/before/units_fs")" \
		"$(wc -l <"$work/before/suid")" "$(wc -l <"$work/before/rules")"
	printf '| systemctl reported: %s\n' "$(head -1 "$work/before/units")"

	cmd "dpkg -i $deb   (apt-get -f install -y to satisfy Depends)"
	dpkg -i "$deb" >"$work/r19.install" 2>&1
	if grep -q 'dependency problems' "$work/r19.install"; then
		apt-get -o Debug::pkgProblemResolver=0 -y -f install >>"$work/r19.install" 2>&1
	fi
	out "$work/r19.install"

	cmd "snapshot during"
	snap "$work/during"
	printf '| installed: %s\n' "$(dpkg-query -W -f='${Package} ${Version} ${Status}' "$PKG" 2>&1)"

	cmd "dpkg -r $PKG"
	dpkg -r "$PKG" >"$work/r19.remove" 2>&1; out "$work/r19.remove"

	cmd "snapshot after"
	snap "$work/after"

	: >"$work/r19.diff"
	for f in group passwd units units_fs suid sudoers rules; do
		if ! diff -u "$work/before/$f" "$work/after/$f" >"$work/d.$f" 2>&1; then
			echo "--- $f changed across install+remove ---" >>"$work/r19.diff"
			cat "$work/d.$f" >>"$work/r19.diff"
		fi
	done
	cmd "diff before/ after/   for group, passwd, units(systemctl), units(on disk), suid, sudoers, polkit+udev+dbus rules"
	out "$work/r19.diff"

	# The stronger statement: nothing changed even while the package was
	# installed. If that also holds, say so — it is a better result than the
	# row asks for.
	: >"$work/r19.during"
	for f in group passwd units units_fs suid sudoers rules; do
		diff -u "$work/before/$f" "$work/during/$f" >>"$work/r19.during" 2>&1 || true
	done
	if [ -s "$work/r19.diff" ]; then
		no 19 "install/remove diff" "state changed across install+remove"
	else
		if [ -s "$work/r19.during" ]; then
			printf '| no diff across install+remove. While installed, these differed:\n'
			sed 's/^/| /' "$work/r19.during"
		else
			printf '| no diff across install+remove, and none while the package was installed either\n'
		fi
		ok 19 "install/remove diff"
	fi
fi

# ---------------------------------------------------------------------- 20
#
# TWO THINGS THIS ROW LEARNED THE HARD WAY, both recorded because the failure
# mode of each is a run that never ends rather than a run that fails.
#
#  * A container has no session bus. GTK then falls back to
#    `dbus-launch --autolaunch`, which blocks — observed at over ten minutes
#    with no timeout at all (docs/release.md §4.7). So this row starts its own
#    `dbus-daemon --session` and exports DBUS_SESSION_BUS_ADDRESS, and it
#    starts it AS THE UNPRIVILEGED USER, because a session bus started by root
#    refuses a connection from uid 1001 and the fallback would be back.
#    Providing a session bus is not making the row easier: a real desktop has
#    one, and this row is about privilege, not about D-Bus.
#
#  * Everything here is bounded. The app is started under `timeout`, every
#    X client call is under `timeout`, and every process this row starts is
#    tracked BY PID and torn down by pid — never by name, because this runs on
#    machines with other containers on them. The next person to hit a stuck
#    start gets a FAIL with a transcript, which is the point.
#
# ROW20_TIMEOUT bounds the application itself (default 90s). START_WAIT is how
# long to wait before looking for the window (default 15s).
hdr 20 "Starts and reaches its first screen as an unprivileged user with no sudo"
if ! command -v Xvfb >/dev/null 2>&1; then
	sk 20 "unprivileged start" "no Xvfb; run this in the WebKit2GTK container"
elif [ ! -x "/usr/bin/$PKG" ] && [ "$(id -u)" != 0 ]; then
	sk 20 "unprivileged start" "the package is not installed and this process cannot install it; run this script as root in a throwaway container"
else
	# Row 19 deliberately leaves the machine as it found it, which means it
	# removed the package. Put it back for row 20 — row 20 is about the
	# installed application starting, not about installing.
	if [ ! -x "/usr/bin/$PKG" ]; then
		cmd "dpkg -i $deb   # reinstall: row 19 removed it again on purpose"
		dpkg -i "$deb" >"$work/r20.install" 2>&1 || apt-get -y -f install >>"$work/r20.install" 2>&1
		out "$work/r20.install"
	fi
	cmd "id $runuser  &&  command -v sudo"
	id "$runuser" >"$work/r20.id" 2>&1 || useradd -m -s /bin/bash "$runuser" >>"$work/r20.id" 2>&1
	id "$runuser" >>"$work/r20.id" 2>&1
	if command -v sudo pkexec >/dev/null 2>&1; then
		command -v sudo pkexec >>"$work/r20.id" 2>&1
		echo "WARNING: a privilege-escalation tool is on PATH; row 20 wants it absent" >>"$work/r20.id"
	else
		echo "neither sudo nor pkexec is on PATH" >>"$work/r20.id"
		dpkg-query -W -f='dpkg-query: ${Package} ${Status}
' sudo policykit-1 pkexec >>"$work/r20.id" 2>&1 || true
	fi
	out "$work/r20.id"

	cmd "runuser -u $runuser -- $PKG   on Xvfb $display, WEBKIT_DISABLE_COMPOSITING_MODE=1"
	# The container's X stack has no GL; with compositing on, WebKit crashes it.
	Xvfb "$display" -screen 0 1400x900x24 -nolisten tcp >"$work/xvfb.log" 2>&1 &
	track_pid $!
	for _ in $(seq 1 60); do DISPLAY=$display timeout 5 xdpyinfo >/dev/null 2>&1 && break; sleep 0.2; done
	if command -v openbox >/dev/null 2>&1; then
		DISPLAY=$display openbox --sm-disable >"$work/openbox.log" 2>&1 &
		track_pid $!
	fi
	sleep 1
	# mktemp -d gives 0700, which the unprivileged user cannot traverse — so
	# XDG_RUNTIME_DIR below would be unreachable and the session-bus socket
	# unopenable. 0711 lets that user reach the two paths it is given without
	# letting it list the unpacked package.
	chmod 0711 "$work"
	install -d -m 0777 "$work/xdg"

	uid=$(id -u "$runuser"); gid=$(id -g "$runuser")
	runhome=$(getent passwd "$runuser" | cut -d: -f6)
	priv="setpriv --reuid=$uid --regid=$gid --clear-groups"

	# The session bus, started as the unprivileged user so that user's process
	# can connect to it. Without one, GTK falls back to `dbus-launch
	# --autolaunch` and blocks; with one, this row finishes in seconds.
	dbusaddr=
	if command -v dbus-daemon >/dev/null 2>&1; then
		: >"$work/dbus.addr"; : >"$work/dbus.pid"
		chmod 0666 "$work/dbus.addr" "$work/dbus.pid"
		# --print-address/--print-pid take a file descriptor, so which value
		# lands where is not a guess about output ordering.
		( exec 8>"$work/dbus.addr" 9>"$work/dbus.pid"
		  $priv env HOME="$runhome" XDG_RUNTIME_DIR="$work/xdg" \
			dbus-daemon --session --fork --print-address=8 --print-pid=9 ) >/dev/null 2>&1
		dbusaddr=$(cat "$work/dbus.addr" 2>/dev/null)
		dbuspid=$(cat "$work/dbus.pid" 2>/dev/null)
		[ -n "${dbuspid:-}" ] && track_pid "$dbuspid"
		printf '| session bus: %s (dbus-daemon pid %s, running as %s)\n' \
			"${dbusaddr:-<none>}" "${dbuspid:-<none>}" "$runuser"
	else
		printf '| dbus-daemon is not installed; GTK will try dbus-launch --autolaunch, which\n'
		printf '| blocks in a container with no session bus. The timeout below is what turns\n'
		printf '| that into a failure rather than a hang.\n'
	fi

	dbusenv=
	[ -n "$dbusaddr" ] && dbusenv="DBUS_SESSION_BUS_ADDRESS=$dbusaddr"

	# Bounded. A start that blocks becomes a FAIL with a transcript.
	timeout -k 5 "$row20timeout" \
		$priv env DISPLAY="$display" HOME="$runhome" \
		XDG_RUNTIME_DIR="$work/xdg" WEBKIT_DISABLE_COMPOSITING_MODE=1 \
		$dbusenv \
		"/usr/bin/$PKG" >"$work/r20.app" 2>&1 </dev/null &
	apppid=$!
	track_pid "$apppid"
	sleep "$startwait"
	winid=$(DISPLAY=$display timeout 20 xdotool search --name '^Debark$' 2>/dev/null | head -1)
	printf '| app pid %s, alive: %s\n' "$apppid" "$(kill -0 "$apppid" 2>/dev/null && echo yes || echo no)"
	printf '| window id: %s\n' "${winid:-<none>}"
	if [ -n "$winid" ]; then
		DISPLAY=$display timeout 20 xdotool windowmove "$winid" 0 0 >/dev/null 2>&1
		DISPLAY=$display timeout 20 xdotool windowsize "$winid" 1400 900 >/dev/null 2>&1
		DISPLAY=$display timeout 20 xdotool windowactivate "$winid" >/dev/null 2>&1
		sleep 2
		if command -v import >/dev/null 2>&1; then
			shot=${SHOT_DIR:-$work}/row20-unprivileged-start${SHOT_NAME:+-$SHOT_NAME}.png
			mkdir -p "$(dirname "$shot")"
			DISPLAY=$display timeout 60 import -window "$winid" "$shot" 2>/dev/null &&
				printf '| screenshot: %s (%s bytes)\n' "$shot" "$(stat -c%s "$shot")"
			# A window that is mapped but never painted photographs as a
			# single flat colour. Count distinct colours so "it started" is
			# not confused with "it is a black rectangle".
			command -v identify >/dev/null 2>&1 &&
				printf '| distinct colours in the capture: %s\n' "$(timeout 60 identify -format '%k' "$shot" 2>/dev/null)"
		fi
		printf '| WM_CLASS: %s\n' "$(DISPLAY=$display timeout 20 xprop -id "$winid" WM_CLASS 2>/dev/null | tr -d '\n')"
	fi
	printf -- '--- app stderr/stdout ---\n'; out "$work/r20.app"
	reap
	if [ -n "$winid" ]; then
		ok 20 "unprivileged start"
	elif [ -z "$dbusaddr" ] && ! command -v dbus-daemon >/dev/null 2>&1; then
		no 20 "unprivileged start" "no window titled Debark within ${startwait}s, and this machine has no dbus-daemon, so GTK had no session bus. Install dbus and re-run before reading this as a defect in the package."
	else
		no 20 "unprivileged start" "no window titled Debark within ${startwait}s (the application was bounded at ${row20timeout}s; see the transcript above)"
	fi
fi

# ---------------------------------------------------------------------------
printf '\n=== Summary — docs/security-review.md §5\n'
for r in "${RESULTS[@]}"; do printf '  row %-3s %-5s %s\n' ${r%% *} $(echo "$r" | cut -d' ' -f2) "$(echo "$r" | cut -d' ' -f3-)"; done
printf '\n  %d passed, %d failed, %d skipped, of 20\n' "$pass" "$fail" "$skip"
[ "$fail" -eq 0 ] || exit 1
