// Frozen public API of the lock package.

package lock

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
)

// Save writes the lock into a bundle directory as FileName, indented, and
// returns its canonical digest.
//
// It refuses anything Load will refuse to read back, so this package can
// never write a lock.json it cannot itself open: the one supported
// schema_version, and the same filename safety subset Load enforces
// (checkLoadSafety). Both gaps were measured: Save wrote a lock naming
// "../../../../etc/shadow" as a package filename, and a lock stamped
// debark.lock/v2, and Load then rejected each of them. A build that appears
// to succeed and leaves a bundle no debark can open is the worst shape for
// this failure, because the diagnostic arrives on the far side of the air gap
// from whoever could fix it.
//
// Save is deliberately NOT the whole of Validate: an incomplete lock (empty
// reason, missing target metadata) is still a lock worth writing and worth
// inspecting, which is the same split Load's own doc comment describes.
func Save(dir string, l *Lock) (digest string, err error) {
	if l == nil {
		return "", dferr.New(dferr.Usage, "lock: nil lock")
	}
	// Load refuses any schema_version but this one, so writing a different
	// one produces a file this package cannot read back — the same
	// write-but-not-readable shape as the filename rule below, measured the
	// same way.
	if l.SchemaVersion != SchemaVersion {
		return "", dferr.New(dferr.Usage,
			"lock: refusing to write schema version %q; %s is the only lock schema debark writes, and Load will not read anything else",
			l.SchemaVersion, SchemaVersion)
	}
	if serr := checkLoadSafety(l); serr != nil {
		return "", serr
	}
	d, derr := Digest(l)
	if derr != nil {
		return "", derr
	}
	out, merr := canonical.MarshalIndent(l)
	if merr != nil {
		return "", dferr.Wrap(dferr.Usage, merr, "lock: marshal")
	}
	path := filepath.Join(dir, FileName)
	// dferr.Environment, not Usage: a failed write here is a full disk, a
	// read-only medium or a missing permission — ADR-012's environment class
	// (exit 2), not "the operator passed something malformed" (exit 1). The
	// lock document itself was already accepted by everything above.
	if werr := os.WriteFile(path, out, 0o644); werr != nil {
		return "", dferr.Wrap(dferr.Environment, werr, "lock: write %s", path)
	}
	return d, nil
}

// Load reads a lock from a bundle directory, validates its schema version,
// and refuses a document that is unsafe to act on at all: one larger than
// maxLockBytes, one whose keys are not exactly the ones debark.lock/v1
// declares (see checkDocumentShape — that covers an undeclared field, a field
// name differing only in case, a repeated key, and anything appended after
// the document), or one naming a file outside the bundle.
//
// It deliberately does NOT run the whole of Validate. Load is what `debark
// inspect` and `debark doctor` reach for, and those exist to examine a
// bundle that may well be wrong; refusing to open one because a reason string
// is empty would take the diagnostic tool away exactly when it is needed.
// Validate is the gate for acting on a lock (core/engine at build time,
// core/install once verify has passed), and it stays that.
//
// The unknown-field refusal is the same concern core/verify already documents
// when it re-derives lock.json's digest from the raw bytes rather than from a
// re-marshalled struct: "it would let an unknown field appended to lock.json
// survive an unmarshal/remarshal round trip undetected". Refusing to decode
// one at all closes that from the other side.
func Load(dir string) (*Lock, error) {
	path := filepath.Join(dir, FileName)
	fi, err := os.Stat(path)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "lock: read %s", path)
	}
	if fi.Size() > maxLockBytes {
		return nil, dferr.New(dferr.Usage,
			"lock: %s is %d bytes, more than the %d-byte maximum; refusing to read it",
			path, fi.Size(), maxLockBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "lock: read %s", path)
	}
	if len(raw) > maxLockBytes { // the file grew between Stat and ReadFile
		return nil, dferr.New(dferr.Usage,
			"lock: %s is %d bytes, more than the %d-byte maximum; refusing to read it",
			path, len(raw), maxLockBytes)
	}
	// The key check runs on the raw bytes, before the decode, because two of
	// the three things it catches are invisible to encoding/json: a field
	// name that differs only in case (which it matches anyway) and a repeated
	// key (which it silently resolves to the last one). See
	// checkDocumentShape.
	if err := checkDocumentShape(raw); err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "lock: parse %s", path)
	}
	var l Lock
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "lock: parse %s", path)
	}
	if l.SchemaVersion != SchemaVersion {
		return nil, dferr.New(dferr.Usage, "lock: unsupported schema version %q (want %q)", l.SchemaVersion, SchemaVersion)
	}
	if err := checkLoadSafety(&l); err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "lock: %s", path)
	}
	return &l, nil
}

// Digest returns the canonical digest of a lock document.
func Digest(l *Lock) (string, error) {
	d, err := canonical.Digest(l)
	if err != nil {
		return "", dferr.Wrap(dferr.Usage, err, "lock: digest")
	}
	return d, nil
}

// Validate checks required fields and internal consistency: every entry in
// Install names a package that exists in Packages at that exact version.
func Validate(l *Lock) error {
	if l == nil {
		return dferr.New(dferr.Usage, "lock: nil lock")
	}
	if l.SchemaVersion != SchemaVersion {
		return dferr.New(dferr.Usage, "lock: unsupported schema version %q (want %q)", l.SchemaVersion, SchemaVersion)
	}
	// Only BackendAuto is refused here, which leaves this check's own sentence
	// ("must be resolved to local or container") unenforced for every other
	// value: "", "totally-fake" and any future backend name all pass. That is
	// a real gap against lock.v1.schema.json's [local, container] enum, and it
	// is deliberately still open: core/install's test fixtures build locks
	// with no resolver at all, so closing it here is a change to that
	// package's fixtures, not to this one. Recorded rather than fixed —
	// backend is an audit field ("where were these decisions made"), not a
	// control, so nothing acts differently on a wrong value.
	if l.Resolver.Backend == BackendAuto {
		return dferr.New(dferr.Usage, "lock: resolver.backend must be resolved to local or container, never auto")
	}

	// lock.v1.schema.json makes target.arch required, and install leans on it
	// harder than on anything else in Target: core/install guards its whole
	// architecture check with `if lk.Target.Arch != ""`, so a lock that
	// simply omits the field is a lock that installs on ANY architecture with
	// no exit-7 refusal at all. An absent value must not be able to switch
	// off the check that exists to catch it.
	if l.Target.Arch == "" {
		return dferr.New(dferr.Usage, "lock: target.arch is empty; install has no architecture to refuse a mismatch against")
	}
	if !containsString(validClosedWorldResults, l.ClosedWorld.Result) {
		return dferr.New(dferr.Usage, "lock: closed_world_check.result %q is not one of %s, %s or %s",
			l.ClosedWorld.Result, ClosedWorldOK, ClosedWorldFailed, ClosedWorldSkipped)
	}

	// Filename safety and pool-path uniqueness live in checkLoadSafety, which
	// Load runs too, so a consumer that never reaches Validate still gets a
	// document whose work is bounded by the files actually on the medium. See
	// checkLoadSafety for why that placement is load-bearing rather than tidy,
	// and for why the (name, arch) rule below deliberately did NOT move with
	// it. Validate calls it rather than repeating it, so the two entry points
	// cannot drift into disagreeing about what a duplicate file is.
	if err := checkLoadSafety(l); err != nil {
		return err
	}

	type key struct{ name, arch, version string }
	type ident struct{ name, arch string }
	seen := make(map[key]bool, len(l.Packages))
	// A lock must name one file per (name, arch), not one per (name, arch,
	// version). The uniqueness key used to include the version, which let a
	// lock carry two versions of the same package and still validate — and
	// then mean different things to different readers, all of them silent:
	// Find returns whichever entry comes first, so InstallSet drops an entry
	// naming the other one, core/install's own selectInstallSet refuses the
	// whole lock, and `install --all` asks apt for two conflicting version
	// pins of one package. ADR-007 says the lock is the plan; a plan that
	// says two things about one package is not one.
	byIdent := make(map[ident]string, len(l.Packages))
	for i, p := range l.Packages {
		if err := checkPackageFields(i, p); err != nil {
			return err
		}
		if !digest.Valid(p.SHA256) {
			return dferr.New(dferr.Usage, "lock: package %s %s/%s: malformed sha256 %q", p.Name, p.Arch, p.Version, p.SHA256)
		}
		id := ident{p.Name, p.Arch}
		if prior, dup := byIdent[id]; dup {
			return dferr.New(dferr.Usage,
				"lock: %s:%s appears twice, at versions %s and %s; a lock names exactly one version of each package",
				p.Name, p.Arch, prior, p.Version)
		}
		byIdent[id] = p.Version
		seen[key{p.Name, p.Arch, p.Version}] = true
	}

	installed := make(map[ident]string, len(l.Install))
	for _, entry := range l.Install {
		name, arch, version, ok := parseInstallEntry(entry)
		if !ok {
			return dferr.New(dferr.Usage, "lock: malformed install entry %q, want the architecture-qualified name:arch=version", entry)
		}
		if !seen[key{name, arch, version}] {
			return dferr.New(dferr.Usage, "lock: install entry %q names a package not present in packages at that version", entry)
		}
		// Install is handed to apt as an argument list. A repeated entry, or
		// two entries pinning one package to different versions, is a request
		// apt cannot satisfy as written — better to say so here than to let
		// the target discover it after the medium has crossed the gap.
		if prior, dup := installed[ident{name, arch}]; dup {
			return dferr.New(dferr.Usage,
				"lock: install names %s:%s more than once (versions %s and %s)", name, arch, prior, version)
		}
		installed[ident{name, arch}] = version
	}
	return nil
}

// parseInstallEntry splits an Install entry of the required, always
// architecture-qualified "name:arch=version" form (dpkg's own syntax). The
// unqualified "name=version" form is rejected: Multi-Arch: same packages
// legitimately appear at the same name and version across two architectures,
// so leaving arch out is ambiguous exactly where precision matters most.
func parseInstallEntry(s string) (name, arch, version string, ok bool) {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 || eq == len(s)-1 {
		return "", "", "", false
	}
	left, version := s[:eq], s[eq+1:]
	colon := strings.IndexByte(left, ':')
	if colon <= 0 || colon == len(left)-1 {
		return "", "", "", false
	}
	name, arch = left[:colon], left[colon+1:]
	return name, arch, version, true
}

// InstallSet returns the packages named by Install, resolved to their entries.
func (l *Lock) InstallSet() []Package {
	if l == nil {
		return nil
	}
	out := make([]Package, 0, len(l.Install))
	for _, entry := range l.Install {
		name, arch, version, ok := parseInstallEntry(entry)
		if !ok {
			continue
		}
		if p, found := l.Find(name, arch); found && p.Version == version {
			out = append(out, p)
		}
	}
	return out
}

// Find returns the package with this name and architecture, if present.
func (l *Lock) Find(name, arch string) (Package, bool) {
	if l == nil {
		return Package{}, false
	}
	for _, p := range l.Packages {
		if p.Name == name && p.Arch == arch {
			return p, true
		}
	}
	return Package{}, false
}
