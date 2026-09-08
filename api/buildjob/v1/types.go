// Package v1 is debark.buildjob/v1: the job model that keeps the
// engine callable by something other than this CLI.
//
// The CLI constructs a BuildRequest and calls the engine; a future scheduler
// would store requests and call the same entry point. Nothing in this package
// may reference a terminal, a filesystem layout convention or global config
// (principle 3).
package v1

// SchemaVersion is the job schema version.
const SchemaVersion = "debark.buildjob/v1"

// BuildRequest is everything the engine needs to produce a bundle.
type BuildRequest struct {
	SchemaVersion string `json:"schema_version"`
	// SnapshotRef is a path to a snapshot archive or an already-extracted
	// snapshot directory. It is resolved by the caller, not by the engine.
	SnapshotRef string  `json:"snapshot_ref"`
	Inputs      Inputs  `json:"inputs"`
	Options     Options `json:"options"`
	Output      Output  `json:"output"`
}

// Inputs is what the operator asked for. Every list is order-preserving; the
// engine sorts before hashing so a reordering does not change the request
// digest.
type Inputs struct {
	// Packages are apt package names, optionally name=version.
	Packages []string `json:"packages,omitempty"`
	// URLs are https:// URLs to .deb files.
	URLs []URLInput `json:"urls,omitempty"`
	// Files are local .deb paths.
	Files []string `json:"files,omitempty"`
	// LocalDirs are directories scanned for .deb files.
	LocalDirs []string `json:"local_dirs,omitempty"`
	// ListFiles are packages.txt-style lists that were expanded into the
	// fields above; recorded for provenance.
	ListFiles []string `json:"list_files,omitempty"`
}

// URLInput is a vendor .deb to download, with an optional operator-supplied
// digest that upgrades its provenance from url-unverified to user-digest.
type URLInput struct {
	URL string `json:"url"`
	// SHA256 is the operator's expected digest, lowercase hex. Optional.
	SHA256 string `json:"sha256,omitempty"`
}

// UpdateMode controls how an existing output bundle is refreshed.
type UpdateMode string

const (
	// UpdateAdditive is the default: add what is missing, touch nothing else.
	UpdateAdditive UpdateMode = "additive"
	// UpdateRefresh is --update: refresh indexes, re-resolve, fetch newer
	// versions, then prune superseded files unless Prune is false.
	UpdateRefresh UpdateMode = "refresh"
)

// Options are the knobs that change what apt is asked and what is written.
type Options struct {
	// Recommends follows the target's apt.conf when nil; true or false
	// overrides it.
	Recommends *bool `json:"recommends,omitempty"`
	// Upgrades adds a full-upgrade pass for packages already installed on the
	// target.
	Upgrades bool `json:"upgrades,omitempty"`
	// UpdateMode is additive or refresh.
	UpdateMode UpdateMode `json:"update_mode,omitempty"`
	// Prune removes superseded files in refresh mode. User-supplied files are
	// never pruned regardless of this value.
	Prune bool `json:"prune,omitempty"`
	// ArchOverride resolves for a different architecture than the snapshot's.
	ArchOverride string `json:"arch_override,omitempty"`
	// MirrorOverrides maps an original URI prefix to a replacement, for shops
	// with an internal mirror. Recorded in the lock.
	MirrorOverrides map[string]string `json:"mirror_overrides,omitempty"`
	// Backend is auto, local or container.
	Backend string `json:"backend,omitempty"`
	// Image overrides the container image for the container backend.
	Image string `json:"image,omitempty"`
	// PolicyRef is a path to a local policy file.
	PolicyRef string `json:"policy_ref,omitempty"`
	// ApprovedKeysRef is a path to an organisation-approved fingerprint list
	// that resolution must satisfy.
	ApprovedKeysRef string `json:"approved_keys_ref,omitempty"`
	// AcknowledgeRedistribution suppresses the interactive redistribution
	// prompt; the warnings are still recorded.
	AcknowledgeRedistribution bool `json:"acknowledge_redistribution,omitempty"`
	// EmbedBinary copies a debark target binary into the bundle.
	EmbedBinary string `json:"embed_binary,omitempty"`
	// SBOM writes sbom.cdx.json.
	SBOM bool `json:"sbom,omitempty"`
	// StoreDir overrides the content-addressed store location.
	StoreDir string `json:"store_dir,omitempty"`
	// ClosedWorldCheck runs the pre-export offline resolution proof. Default
	// true; only a debugging flag turns it off.
	ClosedWorldCheck *bool `json:"closed_world_check,omitempty"`
}

// OutputFormat is the shape of the produced bundle.
type OutputFormat string

const (
	FormatDir OutputFormat = "dir"
	FormatTar OutputFormat = "tar"
)

// Output says where the bundle goes and how it is signed.
type Output struct {
	Path   string       `json:"path"`
	Format OutputFormat `json:"format,omitempty"`
	Sign   SignOptions  `json:"sign,omitempty"`
}

// SignOptions selects the signer.
type SignOptions struct {
	// SignerRef names the key: a path to an ed25519 private key, gpg:<keyid>,
	// or plugin:<name>.
	SignerRef string `json:"signer_ref,omitempty"`
	// Required fails the build when signing is impossible. When false and no
	// signer is configured, the bundle is written unsigned and the fact is
	// recorded in the README and the result.
	Required bool `json:"required,omitempty"`
	// RepoSignerRef optionally signs the apt Release with a GPG key the target
	// already trusts, producing InRelease/Release.gpg.
	RepoSignerRef string `json:"repo_signer_ref,omitempty"`
}

// ExitClass mirrors dferr.Class as a stable string in the result, so a caller
// that never sees the process exit code still learns the outcome.
type ExitClass string

const (
	ExitSuccess      ExitClass = "success"
	ExitUsage        ExitClass = "usage"
	ExitEnvironment  ExitClass = "environment"
	ExitIncomplete   ExitClass = "incomplete"
	ExitVerification ExitClass = "verification"
	ExitResolution   ExitClass = "resolution"
	ExitPolicy       ExitClass = "policy"
	ExitTarget       ExitClass = "target-mismatch"
)

// BuildResult is what the engine returns. It is a value, not a stream: events
// go to the evidence sink.
type BuildResult struct {
	SchemaVersion string `json:"schema_version"`
	// LockRef and ManifestRef are paths inside BundlePath.
	LockRef     string `json:"lock_ref,omitempty"`
	ManifestRef string `json:"manifest_ref,omitempty"`
	BundlePath  string `json:"bundle_path,omitempty"`
	// BundleID copies the manifest's bundle id.
	BundleID string `json:"bundle_id,omitempty"`
	// Signed is true when at least one signature block was written.
	Signed bool  `json:"signed"`
	Stats  Stats `json:"stats"`
	// Warnings are the human-facing warning sentences, already deduplicated.
	Warnings []string `json:"warnings,omitempty"`
	// Unresolved names inputs apt could not satisfy.
	Unresolved []string `json:"unresolved,omitempty"`
	// FetchFailed names the external inputs that did not make it into the
	// bundle: a URL that could not be downloaded, or a local .deb that could
	// not be read. Each entry is the operator's literal input string.
	//
	// It says WHICH inputs failed and never why. FetchFailures says why, and
	// carries one entry per entry here, in the same order. This field is not
	// widened in place and not deprecated: docs/formats.md 0 fixes a
	// published schema version as immutable, and changing an array of string
	// into an array of object is a meaning change that would force
	// debark.buildjob/v2 on every existing consumer for the sake of one
	// added attribute.
	FetchFailed []string `json:"fetch_failed,omitempty"`
	// FetchFailures is FetchFailed with the reason attached.
	//
	// Added under the additive allowance in docs/formats.md 3.6 ("both
	// shapes may gain new optional fields across minor versions of the tool
	// without a schema bump"). A reader that does not know the field is
	// unaffected; a reader that does gets, for the first time, the
	// difference between an unreachable host, a certificate this machine
	// does not trust and a SHA-256 that did not match. Before this, those
	// three were one indistinguishable line in the result document and the
	// reason existed only in a warn-level input.external event, so anything
	// consuming the result alone had to say "a download failed" and stop.
	FetchFailures []FetchFailure `json:"fetch_failures,omitempty"`
	// ExitClass is the outcome class.
	ExitClass ExitClass `json:"exit_class"`
}

// FetchFailure is one external input that did not make it into the bundle,
// with the reason it did not.
type FetchFailure struct {
	// Input is the operator's literal input string: a URL, or a local .deb
	// path. It matches the corresponding entry in BuildResult.FetchFailed
	// exactly, so a consumer can join the two, and it is deliberately NOT
	// redacted, for the reason FetchFailed is not: this is operator-facing
	// summary data returned from Build, not an artefact that ships inside
	// the bundle, and the operator typed this string themselves and needs
	// to see exactly what failed in order to retry it. Detail, which CAN
	// carry a cause this process did not compose, is redacted.
	Input string `json:"input"`
	// Reason is the machine-readable classification. A consumer that meets
	// a reason it does not know should treat it as ReasonOther.
	Reason FetchFailureReason `json:"reason"`
	// Detail is the one-line human explanation, already passed through
	// fetch.RedactMessage so it is safe to print, log and store. Empty when
	// the reason says everything there is to say.
	Detail string `json:"detail,omitempty"`
}

// FetchFailureReason classifies why one external input failed. The set is
// closed and each member maps to a distinct operator action -- that is the
// test for whether a new one is worth adding, not whether the code has a
// distinct branch. Reasons are additive: a new member may appear without a
// schema bump, which is why ReasonOther exists and why a consumer must have
// a default.
type FetchFailureReason string

const (
	// ReasonUnreachable: the host could not be contacted at all -- DNS
	// failure, refused connection, reset, timeout, a connection that died
	// mid-transfer. Act on the network, the proxy or the address.
	ReasonUnreachable FetchFailureReason = "unreachable"
	// ReasonTLSUntrusted: the server was reached and its certificate could
	// not be verified. Act on the machine's trust store -- typically a
	// corporate root that is not installed, or an intercepting proxy.
	// Distinct from unreachable because retrying cannot help and the fix is
	// on this machine, not the far end.
	ReasonTLSUntrusted FetchFailureReason = "tls-untrusted"
	// ReasonHTTPStatus: the server answered with something other than 200.
	// Act on the URL: it is usually wrong, moved, or needs credentials.
	ReasonHTTPStatus FetchFailureReason = "http-status"
	// ReasonDigestMismatch: the bytes arrived intact and hashed to
	// something other than the --digest the operator supplied. The one
	// reason where retrying is the wrong instinct: either the digest is
	// wrong or the file is not the one that was expected, and both need a
	// human.
	ReasonDigestMismatch FetchFailureReason = "digest-mismatch"
	// ReasonNotADeb: the bytes arrived and are not a Debian package. Act on
	// the URL -- an HTML error page served with status 200 is the usual
	// cause.
	ReasonNotADeb FetchFailureReason = "not-a-deb"
	// ReasonTooLarge: the response was bigger than the download limit.
	ReasonTooLarge FetchFailureReason = "too-large"
	// ReasonRefusedRedirect: the server redirected somewhere this tool will
	// not follow -- an https chain continuing in plaintext, or a scheme
	// that is not http(s).
	ReasonRefusedRedirect FetchFailureReason = "refused-redirect"
	// ReasonUnreadable: a local .deb input could not be read -- missing,
	// a directory, or no permission. No network was involved.
	ReasonUnreadable FetchFailureReason = "unreadable"
	// ReasonCancelled: the build was cancelled or the request timed out
	// before the download finished. Not a fault in the input.
	ReasonCancelled FetchFailureReason = "cancelled"
	// ReasonBadInput: the input itself is malformed -- an unparseable URL,
	// a scheme that is not fetched, a --digest that is not 64 hex
	// characters.
	ReasonBadInput FetchFailureReason = "bad-input"
	// ReasonLocalStorage: the builder's own disk or store failed while
	// handling the download. Nothing is wrong with the input.
	ReasonLocalStorage FetchFailureReason = "local-storage"
	// ReasonOther: no classification applied. Present so the set stays
	// closed and a consumer's switch always has an arm to land in, rather
	// than one of the specific reasons quietly absorbing the unknown case.
	ReasonOther FetchFailureReason = "other"
)

// FetchFailureReasons lists every reason, in the order they are documented,
// for schema generation and for tests that must fail when a member is added
// without being written down.
func FetchFailureReasons() []FetchFailureReason {
	return []FetchFailureReason{
		ReasonUnreachable,
		ReasonTLSUntrusted,
		ReasonHTTPStatus,
		ReasonDigestMismatch,
		ReasonNotADeb,
		ReasonTooLarge,
		ReasonRefusedRedirect,
		ReasonUnreadable,
		ReasonCancelled,
		ReasonBadInput,
		ReasonLocalStorage,
		ReasonOther,
	}
}

// Stats is the summary printed after a build and recorded in the lock.
type Stats struct {
	Added           int   `json:"added"`
	Removed         int   `json:"removed"`
	Unchanged       int   `json:"unchanged"`
	Bytes           int64 `json:"bytes"`
	DownloadedBytes int64 `json:"downloaded_bytes"`
	PackageCount    int   `json:"package_count"`
	DurationSeconds int   `json:"duration_seconds,omitempty"`
}
