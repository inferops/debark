// Package lock models debark.lock/v1: exactly what the online solve chose,
// and why. The lock is the install plan (exact versions, ADR-007) and the
// provenance record for every file that crossed the gap.
package lock

// SchemaVersion is the only lock schema debark writes.
const SchemaVersion = "debark.lock/v1"

// FileName is the lock's name inside a bundle.
const FileName = "lock.json"

// Lock is debark.lock/v1.
type Lock struct {
	SchemaVersion string `json:"schema_version"`
	CreatedAt     string `json:"created_at"`
	// SnapshotDigest is the canonical digest of the snapshot this plan was
	// resolved against.
	SnapshotDigest string `json:"snapshot_digest"`
	// RequestDigest is the canonical digest of the BuildRequest, so a rerun
	// with the same inputs is recognisable.
	RequestDigest string `json:"request_digest"`
	// Target restates the target identity the plan is valid for; install
	// refuses a mismatched architecture (exit 7).
	Target Target `json:"target"`
	// Resolver records who made the decisions.
	Resolver Resolver `json:"resolver"`
	// Packages is every file in the bundle pool, sorted by (name, arch).
	Packages []Package `json:"packages"`
	// Install is the exact set install must request, sorted. It is the subset
	// of Packages that was requested or is needed by a requested package,
	// never the whole pool.
	//
	// Every entry is architecture-qualified: name:arch=version (dpkg's own
	// syntax, which apt-get install accepts). The unqualified name=version
	// form is NOT permitted, because it is ambiguous exactly where it matters:
	// a Multi-Arch: same library legitimately appears at two architectures
	// with the same name AND the same version, and an unqualified entry then
	// names both of them and resolves to neither. Validate rejects it.
	Install []string `json:"install"`
	// ClosedWorld is the result of resolving the finished bundle against
	// itself, offline, before export (ADR-007).
	ClosedWorld ClosedWorld `json:"closed_world_check"`
	// Warnings are the structured warnings this plan carries: phasing
	// fallback, solver divergence, redistribution flags, unverified URLs.
	Warnings []Warning `json:"warnings,omitempty"`
	// Unresolved names inputs apt could not satisfy (exit 3 territory).
	Unresolved []Unresolved `json:"unresolved,omitempty"`
	// Stats summarises the run against the previous bundle state.
	Stats Stats `json:"stats"`
}

// Target is the identity the plan is valid for.
type Target struct {
	DistroID     string   `json:"distro_id"`
	VersionID    string   `json:"version_id"`
	Codename     string   `json:"codename"`
	Arch         string   `json:"arch"`
	ForeignArchs []string `json:"foreign_archs,omitempty"`
	APTVersion   string   `json:"apt_version,omitempty"`
	DpkgVersion  string   `json:"dpkg_version,omitempty"`
}

// Backend names where resolution ran.
type Backend string

const (
	BackendLocal     Backend = "local"
	BackendContainer Backend = "container"
	BackendAuto      Backend = "auto" // request-time only; never recorded in a lock
)

// Resolver records the apt that made the decisions, so a divergent result is
// explainable years later.
type Resolver struct {
	Backend Backend `json:"backend"`
	// Image is the container reference used when Backend is container.
	Image string `json:"image,omitempty"`
	// ImageDigest is the resolved image digest (sha256:...).
	ImageDigest string `json:"image_digest,omitempty"`
	// APTVersion and DpkgVersion are the versions that actually ran.
	APTVersion  string `json:"apt_version"`
	DpkgVersion string `json:"dpkg_version"`
	// APTOptions is every -o option passed, sorted, so the run is reproducible.
	APTOptions []string `json:"apt_options,omitempty"`
	// PhasedUpdates is the phasing policy that applied: target-machine-id or
	// never-include.
	PhasedUpdates string `json:"phased_updates"`
	// InstallRecommends is the effective APT::Install-Recommends value.
	InstallRecommends bool `json:"install_recommends"`
	// SolverDivergence is set when the resolving apt major.minor differed
	// from the target's.
	SolverDivergence string `json:"solver_divergence,omitempty"`
}

// Reason values explain why a file is in the bundle.
const (
	ReasonRequested = "requested"
	ReasonUpgrade   = "upgrade"
	ReasonExternal  = "external"
	// ReasonDependencyOfPrefix is used as dependency-of:<pkg>.
	ReasonDependencyOfPrefix = "dependency-of:"
)

// PublisherVerification records how much the tool actually knows about where a
// file came from. HTTPS is not provenance.
type PublisherVerification string

const (
	// VerifiedAPTSigned: fetched by apt from a repository whose InRelease
	// signature verified against a captured keyring.
	VerifiedAPTSigned PublisherVerification = "apt-signed"
	// VerifiedURLUnverified: downloaded over HTTPS with no publisher signature
	// and no user-supplied digest.
	VerifiedURLUnverified PublisherVerification = "url-unverified"
	// VerifiedUserDigest: the operator supplied --digest and it matched.
	VerifiedUserDigest PublisherVerification = "user-digest"
	// VerifiedUserSignature: a vendor signature the operator supplied verified.
	VerifiedUserSignature PublisherVerification = "user-signature"
)

// Flag values recorded per package. They drive doctor output, README warnings
// and policy evaluation; none of them ever blocks a build by itself.
const (
	FlagMultiverse      = "multiverse"
	FlagRestricted      = "restricted"
	FlagNonFree         = "non-free"
	FlagNonFreeFirmware = "non-free-firmware"
	FlagNetworkPostinst = "network-postinst"
	FlagSnapShim        = "snap-shim"
	FlagDKMS            = "dkms"
	FlagUserSupplied    = "user-supplied"
)

// Package is one file in the bundle pool.
type Package struct {
	Name    string `json:"name"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
	// SourcePackage is the source name (without version), recorded for
	// redistribution and corresponding-source questions.
	SourcePackage string `json:"source_package,omitempty"`
	// Filename is the path inside the bundle, e.g.
	// pool/v/vlc/vlc_3.0.21-1build1_amd64.deb.
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	// Origin is where the file came from.
	Origin Origin `json:"origin"`
	// Reason is requested, upgrade, external or dependency-of:<pkg>.
	Reason string `json:"reason"`
	// PublisherVerification records the strength of the provenance claim.
	PublisherVerification PublisherVerification `json:"publisher_verification"`
	// Flags carry redistribution and doctor findings.
	Flags []string `json:"flags,omitempty"`
	// Essential matters to the dpkg fallback ordering.
	Essential bool `json:"essential,omitempty"`
}

// Origin is where one file came from.
type Origin struct {
	// URI is the archive URI or the vendor URL; empty for local file inputs.
	URI string `json:"uri,omitempty"`
	// LocalPath is the operator-supplied path for file inputs, recorded as
	// given.
	LocalPath string `json:"local_path,omitempty"`
	Suite     string `json:"suite,omitempty"`
	Component string `json:"component,omitempty"`
	// ReleaseDigest is the SHA-256 of the InRelease/Release that authenticated
	// the index this file was listed in.
	ReleaseDigest string `json:"release_digest,omitempty"`
	// KeyFingerprint is the archive key that signed that Release.
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
}

// ClosedWorld is the pre-export proof that the bundle resolves against itself
// with no network.
type ClosedWorld struct {
	// Result is ok, failed or skipped.
	Result string `json:"result"`
	// CommandDigest is the canonical digest of the argv that was run.
	CommandDigest string `json:"command_digest,omitempty"`
	// OutputDigest is the SHA-256 of the captured output.
	OutputDigest string `json:"output_digest,omitempty"`
	// Detail explains a failure or a skip in one line.
	Detail string `json:"detail,omitempty"`
}

// ClosedWorld result values.
const (
	ClosedWorldOK      = "ok"
	ClosedWorldFailed  = "failed"
	ClosedWorldSkipped = "skipped"
)

// Warning is a structured warning, not a log line: an auditor reads these.
type Warning struct {
	// Code is a stable machine-readable identifier, e.g.
	// phased-updates.never-include or redistribution.multiverse.
	Code string `json:"code"`
	// Message is the human sentence.
	Message string `json:"message"`
	// Packages names the packages the warning applies to, when relevant.
	Packages []string `json:"packages,omitempty"`
}

// Unresolved is one input apt could not satisfy.
type Unresolved struct {
	// Input is the operator's original input string.
	Input string `json:"input"`
	// Kind is package, url or file.
	Kind string `json:"kind"`
	// Detail is apt's own explanation, trimmed.
	Detail string `json:"detail,omitempty"`
	// Missing lists the dependency names that could not be satisfied, when
	// they could be extracted.
	Missing []string `json:"missing,omitempty"`
}

// Stats summarises a run against the previous state of the output bundle.
type Stats struct {
	Added     int   `json:"added"`
	Removed   int   `json:"removed"`
	Unchanged int   `json:"unchanged"`
	Bytes     int64 `json:"bytes"`
	// DownloadedBytes is what actually crossed the network this run; the
	// difference from Bytes is what the store already held.
	DownloadedBytes int64 `json:"downloaded_bytes"`
}
