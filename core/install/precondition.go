package install

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// effectiveRoot is the filesystem root preconditions read from: Deps.Root
// when a test overrides it, otherwise the real root. Used for
// /etc/os-release, the --keep-source destination and the disk-space check —
// never for the temporary private apt root, which always lives in the real
// OS temp directory regardless of this override.
func effectiveRoot(deps Deps) string {
	if deps.Root != "" {
		return deps.Root
	}
	return "/"
}

// osRelease is the subset of /etc/os-release install cares about.
type osRelease struct {
	ID              string
	VersionID       string
	VersionCodename string
}

// readOSRelease reads and parses /etc/os-release under root. It tolerates a
// missing or unparseable file (returns a zero osRelease, no error, so a
// caller can degrade to "could not determine" rather than fail preconditions
// outright over a file that a stripped-down system might lack).
func readOSRelease(root string) osRelease {
	data, err := os.ReadFile(filepath.Join(root, "etc", "os-release"))
	if err != nil {
		return osRelease{}
	}
	var rel osRelease
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val = unquoteShellValue(strings.TrimSpace(val))
		switch strings.TrimSpace(key) {
		case "ID":
			rel.ID = val
		case "VERSION_ID":
			rel.VersionID = val
		case "VERSION_CODENAME":
			rel.VersionCodename = val
		}
	}
	return rel
}

// unquoteShellValue strips one layer of matching double or single quotes, as
// found in os-release values (PRETTY_NAME="Debian GNU/Linux 12 (bookworm)").
// It does not interpret backslash escapes: os-release's own spec limits
// quoted values to plain text that needs none.
func unquoteShellValue(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// diskShortfall reports how many bytes short freeBytes is of requiredBytes.
// A pure function so the "enough free disk space for the pool" precondition
// (step 2) is testable without touching a filesystem at all; short is
// false whenever there is enough room (including when requiredBytes <= 0,
// i.e. nothing was actually selected to install).
func diskShortfall(freeBytes uint64, requiredBytes int64) (shortfall int64, short bool) {
	if requiredBytes <= 0 {
		return 0, false
	}
	// The clamp comes BEFORE the conversion, not after it. Both orders
	// produce the same answer on every platform Go supports - int64(x) for
	// x > MaxInt64 is a defined, wrapping conversion, and the line below
	// used to overwrite the wrapped value immediately - but "correct because
	// the very next statement repairs it" is a property a reader has to
	// verify and a later edit can silently break. In this shape the
	// conversion is only reachable for a value that provably fits, so there
	// is no moment, not even a transient one, at which an out-of-range
	// uint64 has been converted (gosec G115).
	const maxInt64 uint64 = 1<<63 - 1
	var freeSigned int64
	if freeBytes > maxInt64 {
		freeSigned = 1<<63 - 1
	} else {
		freeSigned = int64(freeBytes)
	}
	if freeSigned >= requiredBytes {
		return 0, false
	}
	return requiredBytes - freeSigned, true
}

// --- foreign architectures (dpkg --add-architecture) -----------------------

// parseForeignArchs reads `dpkg --print-foreign-architectures` output: one
// architecture per line, and no output at all on a machine that has never
// run `dpkg --add-architecture`. Fields rather than a line split, so a
// space-separated answer parses identically here and in core/snapshot's
// capture of the same command (archFromDpkgCommand) - the two sides of the
// gap must agree on what "i386 armhf" means or the comparison below is
// comparing different things. Sorted, like snapshot's, so the comparison and
// anything that reports the list are order-independent.
func parseForeignArchs(out []byte) []string {
	archs := strings.Fields(string(out))
	sort.Strings(archs)
	return archs
}

// missingForeignArchs returns the architectures a bundle needs dpkg to
// accept that this machine has not been told about: required minus enabled,
// sorted and deduplicated.
//
// WHY this check exists, measured 2026-09-06 in debian:12-slim: a real
// bundle whose lock records foreign_archs ["i386"], installed on an amd64
// target that had never run `dpkg --add-architecture i386`, failed with
// rc=100, dpkg refusing every archive with "package architecture (i386) does
// not match system (amd64)". The SAME bundle on the SAME image installed
// cleanly (rc=0) after a single `dpkg --add-architecture i386`.
//
// It has to be its own precondition because nothing else in install can see
// it. `apt-get -s install` SUCCEEDS in both cases - apt's simulation resolves
// against its own package lists and never consults dpkg's
// foreign-architecture list - so the simulate-based plan check reports a
// clean, problem-free plan for a bundle that cannot be unpacked, and the
// operator learns otherwise only from an opaque exit 100 halfway through an
// apply, with some packages possibly already on the disk. The native-arch
// check immediately above it in runner.go does not fire either: the bundle's
// primary arch matches perfectly. This is the gap between them.
//
// nativeArch is excluded from "missing" deliberately: dpkg refuses to add its
// own native architecture ("cannot add architecture 'amd64': it is the native
// architecture"), so a lock that lists the target's own architecture among
// its foreign ones must not produce a refusal whose only stated remedy is a
// command that cannot succeed.
func missingForeignArchs(required, enabled []string, nativeArch string) []string {
	if len(required) == 0 {
		return nil
	}
	have := make(map[string]bool, len(enabled)+1)
	for _, a := range enabled {
		if a = strings.TrimSpace(a); a != "" {
			have[a] = true
		}
	}
	if a := strings.TrimSpace(nativeArch); a != "" {
		have[a] = true
	}
	var missing []string
	seen := map[string]bool{}
	for _, a := range required {
		a = strings.TrimSpace(a)
		if a == "" || have[a] || seen[a] {
			continue
		}
		seen[a] = true
		missing = append(missing, a)
	}
	sort.Strings(missing)
	return missing
}

// addArchitectureCommand renders the exact command an operator has to run to
// clear a foreign-architecture refusal. It is spelled out in full - not
// described as "add the architecture with dpkg" - because the measurement
// that produced this check is precisely that running this command, and
// nothing else, turned rc=100 into rc=0 for the same bundle on the same
// machine. Several architectures are joined with && so the whole line can be
// pasted once.
func addArchitectureCommand(archs []string) string {
	parts := make([]string, 0, len(archs))
	for _, a := range archs {
		parts = append(parts, "dpkg --add-architecture "+a)
	}
	return strings.Join(parts, " && ")
}
