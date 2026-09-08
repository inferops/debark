package apt

import (
	"fmt"
	"strings"

	"github.com/inferops/debark/core/snapshot"
)

// This file implements the rule: "Holds in dpkg_status are respected
// (--allow-change-held-packages only with --upgrade and an explicit flag)".
// A hold is the operator's explicit instruction that dpkg must not change a
// package's version; local.go's Resolve reads the held set once, up front,
// keyed by each held package's own CURRENT version, and uses it to replace
// whatever version apt proposed for it with that current one (record's
// doc), then fetches it via apt-get download rather than through the normal
// install --download-only pass (downloadHeldPackages's doc explains why:
// measured directly, even a same-version "install --reinstall" is refused
// by apt for a held package once -y is in effect, so no install-transaction
// path, at any pinned version, ever gets its bytes fetched). The package
// still ends up in the bundle, just never at a version other than the one
// already on the target.
//
// core/doctor already has a held-package check (checkHeldPackage /
// parseDpkgStatus in core/doctor/checks.go + deb822.go) reading the exact
// same Status convention this file does, so the two packages agree on what
// "held" means. That reader is unexported in a different package — core/apt
// cannot import it, and core/doctor's file is not this package's to add an
// export to — so the same small, narrow scan is duplicated here rather than
// shared, matching core/doctor's own precedent of writing a purpose-built
// ~60-line deb822 reader instead of reaching for a general-purpose one.

// heldPackageVersions reads the snapshot's captured dpkg status and returns
// every package dpkg would refuse to change without
// --allow-change-held-packages (Status field exactly "hold ok installed"),
// mapped to its own current version. Checking a name's presence as a map key
// is the "is this held" test; the value is what record (in local.go) pins it
// at instead of whatever version apt proposed. This is byte-for-byte the
// same test core/doctor's parseDpkgStatus applies for "held", so an operator
// never sees doctor and the resolver disagree about which packages are held.
//
// A held stanza with no parseable Version (malformed input; every real dpkg
// status always has one for an installed package) still counts as held, with
// an empty version string — record's caller must treat that as "cannot be
// safely re-pinned, exclude entirely" rather than fetch nothing under an
// empty version string.
//
// A snapshot with no captured dpkg status at all (DpkgStatus.ArchivePath
// empty) returns an empty set and no error: BuildPrivateRoot already treats
// that case as "resolve as if nothing were installed" (the
// private-root.no-dpkg-status warning), and nothing can be on hold on a
// target with no recorded installed state. A status file that IS captured
// but cannot be read back is a hard error rather than a silent empty set —
// holds are a safety property (states it as a MUST), so failing to
// determine them must not silently resolve as if none existed.
func heldPackageVersions(snap *snapshot.Snapshot, filesDir string) (map[string]string, error) {
	held := map[string]string{}
	if snap == nil || snap.DpkgStatus.ArchivePath == "" {
		return held, nil
	}
	data, err := readSnapshotFile(filesDir, snap.DpkgStatus.ArchivePath)
	if err != nil {
		return nil, fmt.Errorf("apt: read dpkg status: %w", err)
	}
	for _, stanza := range splitDpkgStatusStanzas(data) {
		name := stanza["Package"]
		if name == "" {
			continue
		}
		// dpkg's Status field is "<want> <flag> <status>", e.g.
		// "install ok installed" or "hold ok installed". A package is held
		// exactly when want is "hold" and dpkg still considers it installed
		// (matching core/doctor's parseDpkgStatus precisely, field for
		// field, rather than a looser "contains hold" test that could
		// false-positive on some other field's free-text value).
		fields := strings.Fields(stanza["Status"])
		if len(fields) == 3 && fields[0] == "hold" && fields[2] == "installed" {
			held[name] = strings.TrimSpace(stanza["Version"])
		}
	}
	return held, nil
}

// splitDpkgStatusStanzas is a minimal, narrow deb822 reader that returns
// every single-line field of each stanza in a dpkg status file (the only
// ones this file needs are Package, Status and Version). It is not a
// general deb822 parser: continuation lines
// (folded Description, Conffiles, ...) are recognised only so they don't get
// misread as new fields, never joined into a value, because nothing here
// ever reads a field that spans more than one line.
func splitDpkgStatusStanzas(data []byte) []map[string]string {
	var stanzas []map[string]string
	cur := map[string]string{}
	sawField := false
	flush := func() {
		if sawField {
			stanzas = append(stanzas, cur)
		}
		cur = map[string]string{}
		sawField = false
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue // continuation of a multi-line field; not needed here
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue // not a field line; ignore rather than fail
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		cur[key] = strings.TrimPrefix(val, " ")
		sawField = true
	}
	flush()
	return stanzas
}
