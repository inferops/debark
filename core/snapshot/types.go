// Package snapshot captures, carries and validates the state of an offline
// target: everything the target's own apt would need to resolve a request as
// the target would, and nothing more by default.
//
// The on-disk form is snapshot.tar.zst containing:
//
//	snapshot.json          the Snapshot document below
//	files/<recorded path>  every captured file, at the path recorded in the document
//
// This file is the frozen schema (ADR-005). Implementation lives beside it.
package snapshot

// SchemaVersion is the only snapshot schema debark writes.
const SchemaVersion = "debark.snapshot/v1"

// DocumentName and FilesDir are the fixed member names inside the archive.
const (
	DocumentName = "snapshot.json"
	FilesDir     = "files"
)

// Snapshot is debark.snapshot/v1.
type Snapshot struct {
	// SchemaVersion is always SchemaVersion. It is the first field on the wire.
	SchemaVersion string `json:"schema_version"`
	// CreatedAt is when the capture ran, RFC 3339 UTC.
	CreatedAt string `json:"created_at"`
	// Tool records what captured this snapshot.
	Tool Tool `json:"tool"`
	// Target describes the machine the bundle is being built for.
	Target Target `json:"target"`
	// APT is the target's apt configuration, captured verbatim.
	APT APT `json:"apt"`
	// DpkgStatus points at the captured /var/lib/dpkg/status.
	DpkgStatus File `json:"dpkg_status"`
	// Origin records how this snapshot came to exist: measured from a real
	// machine, or synthesized from a base definition (ADR-014).
	//
	// Required, and deliberately not omitempty. The two kinds carry different
	// correctness guarantees, and absent-means-captured would make the
	// dangerous one the silent default.
	Origin Origin `json:"origin"`
	// InstalledCount is the number of installed packages in DpkgStatus, for
	// human output; it is derived and never authoritative.
	InstalledCount int `json:"installed_count,omitempty"`
	// KeyringFingerprints lists every OpenPGP fingerprint found in the
	// captured keyrings, uppercase hex, sorted. Recorded as policy, never as
	// an independent root of trust (ADR-008).
	KeyringFingerprints []KeyFingerprint `json:"keyring_fingerprints,omitempty"`
	// Labels is optional operator-supplied metadata (hostname, site, ticket).
	// Off by default and removed by --redact.
	Labels map[string]string `json:"labels,omitempty"`
	// Redactions names every field class removed from this snapshot.
	// See RedactionKind.
	Redactions []string `json:"redactions,omitempty"`
	// Warnings records what the capture could not do (unreadable keyring,
	// missing preferences file) without failing the capture.
	Warnings []string `json:"warnings,omitempty"`
}

// Origin is the provenance of a snapshot, and with it the guarantee any
// bundle built from it can carry (ADR-014).
//
// A captured snapshot is a measurement of one real machine: the resolved
// closure is *proven* complete for that machine. A synthesized snapshot is an
// assumption about a machine that may not exist yet: the closure is complete
// only if the target really is what the base says it is. The asymmetry is the
// whole point of recording this. If the real machine has *more* packages than
// the base assumed, the bundle is merely larger than it needed to be. If it
// has *fewer*, the bundle is short and fails at the gap — which is precisely
// the failure debark exists to prevent — so verify and install must be able
// to say which kind the operator is holding.
type Origin struct {
	// Kind is OriginCaptured or OriginSynthesized.
	Kind string `json:"kind"`
	// BaseID identifies the base definition a synthesized snapshot came from,
	// e.g. "ubuntu:26.04/desktop". Empty when Kind is OriginCaptured.
	BaseID string `json:"base_id,omitempty"`
	// Source is where that definition came from: OriginSourceBuiltin for a
	// definition compiled into the binary, or the base name of the
	// operator-supplied file it was read from.
	//
	// Never an absolute path: a snapshot travels to the builder and then into
	// the bundle, and the layout of the machine that ran from-base is not the
	// target's business.
	Source string `json:"source,omitempty"`
	// SourceDigest is the lowercase hex SHA-256 of the canonicalised base
	// definition, so two snapshots claiming the same BaseID can be told apart
	// when the definition itself changed underneath them.
	SourceDigest string `json:"source_digest,omitempty"`
	// AssumedInstalled is every package the base claims a stock install
	// already has, as name:arch, sorted. Empty when Kind is OriginCaptured.
	//
	// It restates what the synthesized DpkgStatus file already says, and that
	// duplication is the point rather than an oversight. A bundle carries
	// snapshot.json but not the snapshot's files (docs/formats.md §2), so on
	// the target — the one place where the assumption can finally be checked
	// against a real dpkg status — this list is the only copy there is. It is
	// what turns install's warning from "this bundle was built against an
	// assumption" into "12 of the 1,847 assumed packages are not present
	// here", which is the difference between a caveat and a finding.
	//
	// Carrying it is safe here for a reason that does not generalise: a
	// synthesized installed-set is a claim about a public stock release. The
	// installed-package list of a *real* machine is the sensitive inventory
	// D8 exists to keep out of artefacts, which is exactly why a captured
	// snapshot's dpkg status never reaches a bundle and why this field must
	// stay empty for one.
	AssumedInstalled []string `json:"assumed_installed,omitempty"`
}

// Origin.Kind values.
const (
	// OriginCaptured means the snapshot was read from a real target by
	// `debark snapshot create`.
	OriginCaptured = "captured"
	// OriginSynthesized means the snapshot was constructed by
	// `debark snapshot from-base` by resolving a base definition's seed
	// packages with apt.
	OriginSynthesized = "synthesized"
)

// OriginSourceBuiltin is the Origin.Source value for a base definition
// compiled into the binary, as opposed to one read from an operator's file.
const OriginSourceBuiltin = "builtin"

// Synthesized reports whether this snapshot was synthesized from a base
// definition rather than captured from a real machine. Callers that must warn
// about an assumed base use this rather than comparing strings.
func (s *Snapshot) Synthesized() bool {
	return s.Origin.Kind == OriginSynthesized
}

// Tool identifies the binary that produced a snapshot.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Edition is community, official or enterprise.
	Edition string `json:"edition,omitempty"`
}

// Target is the identity of the offline machine.
type Target struct {
	// DistroID is ID from /etc/os-release: "debian", "ubuntu", …
	DistroID string `json:"distro_id"`
	// VersionID is VERSION_ID: "12", "24.04", …
	VersionID string `json:"version_id"`
	// Codename is VERSION_CODENAME: "bookworm", "noble", …
	Codename string `json:"codename"`
	// PrettyName is the human string, for reports only.
	PrettyName string `json:"pretty_name,omitempty"`
	// Arch is dpkg --print-architecture.
	Arch string `json:"arch"`
	// ForeignArchs is dpkg --print-foreign-architectures, sorted.
	ForeignArchs []string `json:"foreign_archs,omitempty"`
	// APTVersion is the version reported by apt-get -v, e.g. "2.8.3".
	APTVersion string `json:"apt_version"`
	// DpkgVersion is the version reported by dpkg --version, e.g. "1.22.6".
	DpkgVersion string `json:"dpkg_version"`
	// MachineID is /etc/machine-id, needed to reproduce Ubuntu phased-update
	// selection exactly (ADR-006). Redactable: empty when --redact was used,
	// in which case resolution falls back to never-include-phased-updates.
	MachineID string `json:"machine_id,omitempty"`
	// OSRelease is the captured /etc/os-release file.
	OSRelease *File `json:"os_release,omitempty"`
}

// APT is the target's captured apt configuration. Every list is sorted by
// RecordedPath so the snapshot digest is stable.
type APT struct {
	// Sources are sources.list, sources.list.d/*.list and *.sources.
	Sources []File `json:"sources,omitempty"`
	// Preferences are preferences and preferences.d/*.
	Preferences []File `json:"preferences,omitempty"`
	// Conf is apt.conf and apt.conf.d/* — Install-Recommends, Default-Release,
	// phasing settings and proxies (proxies are redacted).
	Conf []File `json:"conf,omitempty"`
	// Trusted are trusted.gpg.d/* keyrings.
	Trusted []File `json:"trusted,omitempty"`
	// Keyrings are the files referenced by Signed-By in Sources, at their
	// original absolute paths.
	Keyrings []File `json:"keyrings,omitempty"`
}

// File is one captured file: where it lived on the target, where it lives in
// the archive, and what it hashes to.
type File struct {
	// Path is the absolute path on the target, e.g. /etc/apt/sources.list.
	Path string `json:"path"`
	// ArchivePath is the member name inside the archive, relative to files/:
	// the target path with its leading separator removed.
	//
	// SECURITY INVARIANT. A snapshot is untrusted input: it was captured on a
	// machine the builder does not control, and the builder is the one host in
	// the pipeline that holds a signing key. ArchivePath must therefore be a
	// local relative path - no leading separator, no drive letter, no parent
	// (dot-dot) component, no backslash - so extracting it can never write
	// outside the extraction directory. Validate rejects any path that fails
	// this test, and every extractor must re-check rather than trusting that
	// Validate ran.
	ArchivePath string `json:"archive_path"`
	// Size in bytes.
	Size int64 `json:"size"`
	// SHA256 is the lowercase hex digest of the file's bytes.
	SHA256 string `json:"sha256"`
	// Mode is the file's permission bits, octal, e.g. "0644".
	Mode string `json:"mode,omitempty"`
	// Redacted is true when the stored bytes differ from the target's because
	// secrets (proxy credentials) were removed.
	Redacted bool `json:"redacted,omitempty"`
}

// KeyFingerprint is one OpenPGP key found in a captured keyring.
type KeyFingerprint struct {
	// Fingerprint is uppercase hex with no spaces.
	Fingerprint string `json:"fingerprint"`
	// KeyID is the long key id (last 16 hex characters).
	KeyID string `json:"key_id,omitempty"`
	// UserIDs are the primary user ids, for human output.
	UserIDs []string `json:"user_ids,omitempty"`
	// Keyring is the RecordedPath of the file the key came from.
	Keyring string `json:"keyring"`
	// SignedBy lists the source files that reference this keyring with
	// Signed-By, recording the captured trust policy.
	SignedBy []string `json:"signed_by,omitempty"`
}

// RedactionKind values are the strings that may appear in Snapshot.Redactions.
const (
	RedactMachineID = "machine-id"
	RedactProxies   = "proxies"
	RedactLabels    = "labels"
)

// PhasedPolicy is how resolution must treat Ubuntu phased updates for this
// snapshot (ADR-006). It is derived, not stored: a snapshot with a machine id
// gets PhasedTargetMachineID, one without gets PhasedNeverInclude.
type PhasedPolicy string

const (
	// PhasedTargetMachineID sets APT::Machine-ID to the target's machine id so
	// apt makes the same phasing decision the target would.
	PhasedTargetMachineID PhasedPolicy = "target-machine-id"
	// PhasedNeverInclude selects only fully-phased versions, which the target
	// accepts too. Conservative fallback for redacted snapshots.
	PhasedNeverInclude PhasedPolicy = "never-include"
)

// PhasedPolicyFor returns the policy this snapshot requires.
func (s *Snapshot) PhasedPolicyFor() PhasedPolicy {
	if s.Target.MachineID == "" {
		return PhasedNeverInclude
	}
	return PhasedTargetMachineID
}

// Files returns every captured file in the snapshot, in a stable order.
func (s *Snapshot) Files() []File {
	out := make([]File, 0, len(s.APT.Sources)+len(s.APT.Preferences)+len(s.APT.Conf)+
		len(s.APT.Trusted)+len(s.APT.Keyrings)+2)
	out = append(out, s.DpkgStatus)
	if s.Target.OSRelease != nil {
		out = append(out, *s.Target.OSRelease)
	}
	out = append(out, s.APT.Sources...)
	out = append(out, s.APT.Preferences...)
	out = append(out, s.APT.Conf...)
	out = append(out, s.APT.Trusted...)
	out = append(out, s.APT.Keyrings...)
	return out
}
