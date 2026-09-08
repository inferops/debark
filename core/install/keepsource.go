package install

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// This file is about the one thing install does that outlives the run.
//
// Everything else install touches is inside a temporary directory that
// Close() removes: the private apt root exists only for the length of one
// apt-get invocation, and every byte apt reads through it was covered by the
// signed manifest that verify checked moments earlier (ADR-008). --keep-source
// breaks that shape deliberately: PersistSource copies the same deb822 stanza,
// "Trusted: yes" and all, into /etc/apt/sources.list.d/, where it stays.
//
// What that means afterwards is worth stating plainly, because the flag's name
// does not say it. From then on, this machine's apt has a permanently trusted
// repository whose contents nothing ever verifies again. "Trusted: yes" tells
// apt to skip the Release signature check that would normally be the reason to
// believe an archive; there is no InRelease over the bundle's pool, and no
// digest check outside the one apt does against repo/Packages - a file sitting
// in the same directory, writable by whoever can write the directory. So every
// future `apt install` and `apt upgrade` on this machine will install, as root,
// whatever is in that directory at the time. verify never runs again. The
// operator's next install is not covered by the signature that made this one
// safe.
//
// That is a legitimate thing to ask for - install-offline.sh offers the same
// flag - but it is a standing grant of root-level package installation over a
// directory, and it deserves to be said out loud on success (not only when
// something fails), and refused outright when the directory in question is one
// that other users on the machine can write.

// pathPermissions reports the permission bits of path, and whether this
// platform's os.Stat returns bits that mean anything at all.
//
// It is a package variable, like execCommandContext and stdinIsTerminal, so
// tests can pin a permission pattern rather than depend on a filesystem that
// may be unable to express one. Production code never reassigns it.
var pathPermissions = func(path string) (mode fs.FileMode, known bool, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false, err
	}
	// Windows' os.Stat synthesises 0666, or 0444 for a read-only file, out of
	// the read-only attribute alone: there are no group or other bits behind
	// it to read, so every directory there would look world-writable and this
	// check would refuse every bundle for a reason that does not exist.
	// install never runs for real on Windows - only this suite does -
	// so the honest answer on that platform is "unknown", not a guess in
	// either direction.
	return fi.Mode(), runtime.GOOS != "windows", nil
}

// keepSourceTrustProblem reports why repoDir must not be handed standing,
// unauthenticated, root-level apt trust, or "" when it found no reason.
//
// Two shapes are refused:
//
//   - repoDir itself group- or world-writable. Anyone who can write the
//     directory can drop a .deb into it and have root install it on this
//     machine's next apt run. There is no ambiguity about that one: it is a
//     local privilege escalation with a waiting period, and the operator who
//     typed --keep-source asked to keep trusting THIS bundle, not to grant
//     every local account a permanent root-install channel.
//
//   - an ancestor of repoDir world-writable WITHOUT the sticky bit. That
//     reaches the same end through a different door: replace or rename the
//     repository directory wholesale rather than write inside it. The sticky
//     exception is what keeps /tmp and /var/tmp usable - sticky is precisely
//     the bit that stops a non-owner renaming or removing an entry they do
//     not own, so a world-writable sticky ancestor cannot be used to swap the
//     bundle out. Group-writable ancestors are NOT refused: a shared,
//     group-owned staging directory (2775) is a normal way to run this, and
//     the group already has to be trusted to have put the bundle there.
//
// This deliberately checks the directory apt will TRUST - the bundle's own
// repo/ - and not /etc/apt/sources.list.d, the directory the stanza is written
// into. A world-writable /etc/apt/sources.list.d is a machine that is already
// lost by other means; a world-writable bundle directory is the ordinary
// result of extracting onto removable media, and is the case this exists for.
//
// A stat failure anywhere in the chain is itself a refusal. It cannot happen
// on a path whose bundle was just read successfully - stat'ing a directory
// needs only traverse permission on its parent, which reading the bundle
// already proved - so "could not tell" here means something unusual is true of
// the filesystem, and guessing in favour of a permanent root-install grant is
// not the direction to guess in.
func keepSourceTrustProblem(repoDir string) string {
	mode, known, err := pathPermissions(repoDir)
	if err != nil {
		return fmt.Sprintf("cannot check the permissions of %s: %v", repoDir, err)
	}
	if !known {
		return ""
	}
	if who := writableByOthers(mode); who != "" {
		return fmt.Sprintf("%s is %s (mode %04o), so anyone who can write it could have root install a package of their choosing on the next apt run",
			repoDir, who, mode.Perm())
	}

	// Walk to the filesystem root. The bound is belt and braces against a
	// filepath.Dir that never reaches a fixed point on some path shape.
	dir := filepath.Dir(repoDir)
	for i := 0; i < 64; i++ {
		mode, known, err := pathPermissions(dir)
		if err != nil {
			return fmt.Sprintf("cannot check the permissions of %s, an ancestor of %s: %v", dir, repoDir, err)
		}
		if known && mode.Perm()&0o002 != 0 && mode&fs.ModeSticky == 0 {
			return fmt.Sprintf("%s, an ancestor of %s, is world-writable and not sticky (mode %04o), so anyone could replace the repository directory itself",
				dir, repoDir, mode.Perm())
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// writableByOthers names who, other than the owner, may write something with
// this mode: "" when nobody can.
func writableByOthers(mode fs.FileMode) string {
	switch perm := mode.Perm(); {
	case perm&0o022 == 0o022:
		return "group- and world-writable"
	case perm&0o020 != 0:
		return "group-writable"
	case perm&0o002 != 0:
		return "world-writable"
	default:
		return ""
	}
}

// keepSourceStandingTrustWarning is the sentence a SUCCESSFUL --keep-source
// install has to end with. It is a Warning rather than a Problem on purpose:
// the operator asked for this, it worked, and the install is OK - but the
// machine is in a materially different state afterwards from every other
// install this tool performs, and a report that did not say so would be
// describing only half of what happened.
func keepSourceStandingTrustWarning(destPath, repoDir string) string {
	return "--keep-source left a permanent apt source at " + destPath +
		": this machine's apt will now install anything found in " + repoDir +
		" as root, with no signature and no digest check, on every future 'apt install' or 'apt upgrade'" +
		" - the bundle is never verified again. Delete that file to end the trust."
}
