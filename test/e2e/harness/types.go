package harness

// This file is the fixture data model ("a fixture
// is a declarative description of a target and a request"). It lives in
// harness, not in package e2e, so both the `go test` driver
// (test/e2e/e2e_test.go) and the standalone matrix runner (hack/matrix) work
// from one definition with no import cycle; test/e2e/fixture.go is a thin
// JSON envelope (schema_version, name, protects, tags) around a Scenario.
//
// Every field here corresponds directly to something a fixture file states,
// per the task's declarative-fixture template:
//
//	name, distro, version, arch
//	installed set (how to construct the target container's dpkg state)
//	sources / preferences / apt.conf.d overrides
//	the request (packages, vendor .debs, flags)
//	expected outcome (exit class, packages that must be present, binaries that must run)

// Scenario is everything RunFixture needs to execute one row.
type Scenario struct {
	Name        string      `json:"name"`
	Protects    string      `json:"protects"`
	Tags        []string    `json:"tags,omitempty"`
	Target      TargetSpec  `json:"target"`
	Request     RequestSpec `json:"request"`
	Expect      ExpectSpec  `json:"expect"`
	Tamper      *TamperSpec `json:"tamper,omitempty"`
	Determinism bool        `json:"determinism,omitempty"`
}

// TargetSpec describes the release and the pre-snapshot installed state.
//
// Everything declared here reaches BOTH ends of the pipeline, and that is a
// property fixtures are entitled to rely on. It is applied once, to the state
// container the snapshot is taken from, and that container is then committed
// to an image the fresh target is started from (scenario.go's
// commitTargetImage) — so the machine debark solved a bundle FOR is the
// machine the bundle is installed ON, down to the dpkg status, the foreign
// architectures, the holds and the apt configuration.
//
// Before 2026-09-05 it reached only the snapshot end, and the install ran on a
// stock release image. Fixtures whose claim is about installed state — a hold
// that must be respected, an already-installed older version, an enabled
// foreign architecture — were therefore either failed for a state the target
// never had (foreign-arch-i386, multiarch-coexist) or passed as plain fresh
// installs that could not have failed (held-package, stale-installed-version).
// A fixture written against this type may now state a premise about the target
// machine and have it be true at install time.
type TargetSpec struct {
	Distro  string `json:"distro"`  // "debian" | "ubuntu" (core/distro.Debian / .Ubuntu)
	Version string `json:"version"` // e.g. "12", "24.04" (core/distro VersionID)
	Arch    string `json:"arch"`    // e.g. "amd64"

	// Matrix, when true, tells the matrix runner to expand this fixture
	// across every core/distro.Supported() release (and MatrixArches, or
	// just Arch when MatrixArches is empty) instead of only Distro/Version/
	// Arch above, which remain its default/debugging target. This is how a
	// handful of fixtures become "the integration matrix"
	// while most edge fixtures stay pinned to the one release whose
	// mechanism they test (task: "each row is minutes of container time").
	Matrix       bool     `json:"matrix,omitempty"`
	MatrixArches []string `json:"matrix_arches,omitempty"`

	// ForeignArchs are added with `dpkg --add-architecture` before any
	// package installation, so packages of a second architecture
	// can be requested.
	ForeignArchs []string `json:"foreign_archs,omitempty"`

	Installed   InstalledSet      `json:"installed"`
	Sources     []NamedFile       `json:"sources,omitempty"`     // written under /etc/apt/sources.list.d/
	Preferences []NamedFile       `json:"preferences,omitempty"` // written under /etc/apt/preferences.d/
	AptConf     map[string]string `json:"apt_conf,omitempty"`    // key -> raw apt.conf.d value, e.g. {"APT::Install-Recommends": "\"false\";"}
}

// InstalledSet is how the target container's dpkg state is constructed
// before the snapshot is taken, entirely with the container's own real apt
// (never a private root — a real target's dpkg status is exactly what a real
// apt-get install leaves behind).
type InstalledSet struct {
	// Packages are real archive package names installed with
	// `apt-get install -y --no-install-recommends` before any synthetic
	// repo is added (task: prefer small real packages — jq, tree, libonig5).
	Packages []string `json:"packages,omitempty"`
	// Holds are apt-mark held after installation.
	Holds []string `json:"holds,omitempty"`
	// Repos are synthetic local repositories (see SyntheticRepo,
	// syntheticdeb.go) added to the target's real apt sources before
	// Packages/FromRepos are installed, so a FromRepos entry can request a
	// synthetic package. Built once on the host and served over HTTP
	// (httprepo.go) so the separately-started builder container sees the
	// same repository the state container installed from.
	Repos []SyntheticRepo `json:"repos,omitempty"`
	// FromRepos are synthetic package names (optionally name=version) to
	// `apt-get install -y` after Repos are added, e.g. to seed a "stale
	// installed version" or a third-party-repo package onto the target.
	FromRepos []string `json:"from_repos,omitempty"`
}

// NamedFile is one file written verbatim under a well-known apt config
// directory (sources.list.d or preferences.d).
type NamedFile struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

// RequestSpec is the build request a fixture drives, plus the flags used at
// each later stage.
type RequestSpec struct {
	Packages []string `json:"packages,omitempty"`
	URLs     []string `json:"urls,omitempty"`
	// VendorDebs are built fresh on the builder container and fed to
	// `debark build` via a --list file plus local-debs/ dir,
	// so the external-.deb-closure and external-.deb-missing-deps fixtures
	// never need a real vendor download.
	VendorDebs []SyntheticDebSpec `json:"vendor_debs,omitempty"`

	// VendorURLs are built on the host exactly like VendorDebs, but are
	// SERVED OVER HTTP by this run's RepoServer and handed to `debark
	// build` as http:// URLs rather than as local paths. That difference is
	// the whole point of the field, and it is not a stylistic one.
	//
	// Every other vendor input in this suite is staged as a local path, so
	// core/fetch's HTTP path — the retry loop, the progress writer, URL
	// redaction, and the fetch-failure reason classification — had no
	// covering row at all. Measured on the 2026-09-07 run before this
	// existed: of the 35 evidence.json files the run produced, ZERO
	// contained a progress event. A defect that made every build with a
	// vendor URL unreproducible (core/engine's liveOnlyTypes) therefore had
	// to be found by reading core/fetch rather than by running this suite.
	//
	// The URL is built from the RepoServer's BaseURL, so a fixture cannot
	// spell it out itself — it does not know the OS-assigned port. Use
	// VendorURLQuery to attach a credential-shaped query string.
	VendorURLs []SyntheticDebSpec `json:"vendor_urls,omitempty"`

	// VendorURLQuery is a raw query string (no leading "?") appended to
	// every VendorURLs URL. It exists so a fixture can put something
	// secret-shaped into the operator's literal input and then assert it
	// does not appear in the bundle: core/fetch.RedactURL drops a query
	// string WHOLE rather than scrubbing it parameter by parameter, because
	// a presigned credential (an AWS X-Amz-Signature, an Azure SAS sig, a
	// bare vendor token=) lives there under a name that cannot be told from
	// a harmless one.
	//
	// The server ignores it — http.FileServer keys on the path — so the
	// download succeeds and the assertion is about the artefact, not about
	// whether the fetch worked.
	VendorURLQuery string `json:"vendor_url_query,omitempty"`

	BuildFlags   []string `json:"build_flags,omitempty"`
	InstallFlags []string `json:"install_flags,omitempty"`
	VerifyFlags  []string `json:"verify_flags,omitempty"`

	// PriorBuild, when set, runs one earlier `debark build` into the SAME
	// output directory before the fixture's own request, so a fixture can
	// state a claim about a bundle directory that already has a history.
	// A run is additive, so what the earlier request pulled into
	// repo/pool is still there when the later one runs — which is the whole
	// mechanism behind pool-unreferenced.json.
	//
	// It is a separate field rather than a second Scenario because
	// everything else about the row — the target, the snapshot, the keys,
	// the builder container — is shared. Only the request differs, which is
	// exactly the real-world shape being modelled: the same operator,
	// building the same media again next month, asking for less.
	PriorBuild *PriorBuild `json:"prior_build,omitempty"`

	// Sign builds and installs with a generated operator key when true
	// (required for every tamper fixture; also exercised by the
	// determinism fixture to prove signing doesn't introduce
	// nondeterminism). Unsigned when false, matching `build --no-sign`.
	Sign bool `json:"sign,omitempty"`
}

// PriorBuild is one earlier build of the same bundle directory. It runs with
// the same snapshot, backend, signing key and output directory as the
// fixture's own build; only the requested packages and build flags differ.
//
// It must exit 0. A fixture whose premise ("this bundle directory already
// holds last month's packages") never came true has not tested anything, so
// a non-zero prior build ends the row rather than letting the real build run
// against a directory in an unknown state.
type PriorBuild struct {
	Packages   []string `json:"packages,omitempty"`
	BuildFlags []string `json:"build_flags,omitempty"`
}

// ExpectSpec is the expected outcome: exit classes, and post-hoc assertions
// that run only when every stage reached the exit class it expected.
type ExpectSpec struct {
	Build   StageExpect  `json:"build"`
	Verify  *StageExpect `json:"verify,omitempty"`  // nil = default StageExpect{ExitClass: "success"}
	Install *StageExpect `json:"install,omitempty"` // nil = default StageExpect{ExitClass: "success"}

	PackagesPresent []string `json:"packages_present,omitempty"`
	// PackagesAbsent must NOT be dpkg-installed on the fresh target — the
	// other half of a Recommends/redistribution/policy fixture's claim,
	// where proving something was correctly left out matters as much as
	// proving what was included (e.g. no-recommends-target.json).
	PackagesAbsent []string      `json:"packages_absent,omitempty"`
	Binaries       []BinaryCheck `json:"binaries,omitempty"`
	ElfChecks      []ElfCheck    `json:"elf_checks,omitempty"`

	// UnresolvedContains are substrings expected somewhere in the build
	// stage's stdout/stderr (BuildResult.Unresolved, rendered by the CLI),
	// for the exit-3 "incomplete" fixtures.
	UnresolvedContains []string `json:"unresolved_contains,omitempty"`

	// BundleFiles assert on files inside the built bundle, as copied out to
	// the host — lock.json, README.txt, last-run-unreferenced.txt. This is
	// the operator-facing surface for everything the build only *reports*:
	// a warning that never reaches lock.json is a warning nobody sees, and
	// no exit code or installed package can stand in for it.
	BundleFiles []BundleFileCheck `json:"bundle_files,omitempty"`

	// ContainerPaths assert that a path does, or does not, exist inside one
	// of the pipeline's containers after the build. The reason this is not
	// merely another file check: some guarantees are about what did NOT
	// happen, and the only honest evidence for "this command never ran" is
	// that the file it would have created is not there.
	ContainerPaths []ContainerPathCheck `json:"container_paths,omitempty"`

	// DoctorContains are substrings expected in
	// `debark doctor BUNDLE --json` output, for the snap-shim and DKMS
	// fixtures (design's doctor flags: FlagSnapShim, FlagDKMS in
	// core/lock/types.go, surfaced through core/doctor's findings).
	DoctorContains []string `json:"doctor_contains,omitempty"`
}

// BundleFileCheck asserts that one file in the built bundle exists and
// contains every listed substring. Path is bundle-relative and always uses
// forward slashes ("lock.json", "repo/Packages", "last-run-unreferenced.txt")
// — the same spelling the manifest and the last-run-*.txt reports use.
//
// It deliberately asserts on the file as an operator reads it rather than on
// a parsed structure: a warning code that has been renamed, moved into a
// nested object, or dropped from the rendering is a change an operator would
// notice, and a substring check notices it too. Where the exact JSON shape
// matters it is core/lock's own tests that own it, not this harness.
//
// The build writes three such reports at the bundle root (core/bundle/api.go
// names them): last-run-added.txt and last-run-removed.txt say what this run
// DID, and last-run-unreferenced.txt says what the bundle now IS — every pool
// file repo/Packages does not index, one repo-relative path per line, sorted.
// The third is the one worth naming here, because it is the only signal in
// the bundle for a file that no single run ever touched: present on the
// media, hashed by the manifest and covered by the signature, and invisible
// to the target's apt. It therefore appears in neither of its two siblings,
// and the same state is raised into lock.json as the "pool.unreferenced"
// warning (core/bundle.WarnUnreferenced). Its usual cause is an incremental
// build — a bundle directory that already held an earlier run's packages,
// which is what Request.PriorBuild exists to set up.
//
// No new mechanism is needed to assert on it: it is an ordinary file at the
// bundle root, so
//
//	{"path": "last-run-unreferenced.txt", "contains": ["pool/main/h/hello/"]}
//
// works today, and the natural companion check is
//
//	{"path": "lock.json", "contains": ["pool.unreferenced"]}
//
// which proves the same finding reached the document an operator actually
// reads. AssertBundleFiles runs against the host copy taken straight off the
// builder, before any tamper, so both are claims about what the BUILD wrote.
//
// One asymmetry to know before writing such a fixture: a substring check can
// only prove a path IS listed, never that the file is empty of everything
// else, and an absent file is reported as a product finding (see
// AssertBundleFiles) — a build that stopped writing the report at all fails
// the check rather than passing it vacuously.
// Absent is the other direction, and it is the one that needs care. A
// substring that is not there is the normal state of almost every string, so
// an Absent check passes for a thousand reasons that have nothing to do with
// the property being claimed — the file could be empty, the feature could
// never have run, the fixture could have a typo in the value it planted.
// That is the vacuous-pass shape this tree has paid for repeatedly.
//
// So Absent is only meaningful PAIRED with a Contains on the same file that
// proves the code under test ran at all. The redaction fixture asserts that
// evidence.json contains "?REDACTED" (the redaction happened) AND does not
// contain the token (it happened correctly); either half alone proves
// nothing. AssertBundleFiles enforces the file being readable, so a build
// that stopped writing evidence.json fails rather than passing both halves.
type BundleFileCheck struct {
	Path     string   `json:"path"`
	Contains []string `json:"contains,omitempty"`
	Absent   []string `json:"absent,omitempty"`
}

// ContainerPathCheck asserts the presence or absence of one path inside one
// of the pipeline's containers, named by role: "state" (the container the
// snapshot was taken from) or "builder" (the container the bundle was built
// on).
//
// The two directions are NOT symmetric, and the asymmetry is the point.
//
//   - present:false is a claim about the product. The path is what some
//     captured, untrusted input would have created had debark honoured it;
//     finding it there means the input executed, and that is a product
//     failure of the most serious kind (see apt-conf-hook-dropped.json).
//   - present:true is a claim about the FIXTURE's own instrumentation — that
//     the payload it plants really does fire when something honours it. A
//     mistyped path or a hook apt never invokes would leave the negative
//     check passing for a reason that has nothing to do with debark, and a
//     green row nobody can trust is what this whole harness exists to avoid.
//     So a failed present:true check is reported as a row that could not be
//     run (blocked), never as a product failure: it is evidence about the
//     fixture, not about debark.
type ContainerPathCheck struct {
	// Container is "state" or "builder".
	Container string `json:"container"`
	// Path is an absolute path inside that container.
	Path string `json:"path"`
	// Present is what must be true of it.
	Present bool `json:"present,omitempty"`
}

// Container role names usable in a ContainerPathCheck. The fresh target is
// deliberately absent, and the reason changed on 2026-09-05 without changing
// the conclusion, so it is worth stating precisely.
//
// It used to be "the fresh target is a clean release image, so nothing a
// snapshot carried could have run there". That is no longer true: the fresh
// target is now started from a `docker commit` of the state container
// (scenario.go's commitTargetImage), because a debark bundle is a
// closed-world plan for the one machine the snapshot describes and installing
// it anywhere else measures nothing. The fresh target therefore INHERITS
// whatever the target state built — including, for apt-conf-hook-dropped, the
// captured apt.conf.d hook and the /tmp marker it already fired on the state
// container.
//
// A present:false check against it would consequently be satisfied by neither
// of the two things such a check is ever written to mean. It cannot show that
// debark declined to honour a captured hook (the file is simply inherited
// from the image), and it cannot show the target ran one (the marker is there
// either way). The claim about the product lives on the builder — the host
// that holds a signing key — and it is checked there; the target honouring
// its OWN apt configuration on its OWN machine is not a finding at all.
const (
	RoleState   = "state"
	RoleBuilder = "builder"
)

// StageExpect names the expected dferr.Class (by its stable String(), e.g.
// "success", "incomplete", "resolution", "verification") for one CLI
// invocation's exit code.
type StageExpect struct {
	ExitClass string `json:"exit_class"`
}

// BinaryCheck runs one binary inside the freshly-installed, --network-none
// target and checks its exit code — the assertion that matters more than
// dpkg's own opinion (task point 2: "a package can install and still be
// unusable if its dependencies were missed").
type BinaryCheck struct {
	Path           string   `json:"path"`
	Args           []string `json:"args,omitempty"`
	ExitCode       int      `json:"exit_code"`
	StdoutContains string   `json:"stdout_contains,omitempty"`
}

// ElfCheck is the library-only equivalent of BinaryCheck, for foreign-arch
// packages that ship no executable of their own (e.g. a plain :i386
// library): it confirms an installed file is genuinely the declared
// architecture's machine code, not merely that dpkg believes it installed.
// Package, not Path: `readelf`/`file` are not guaranteed present in a slim
// image (binutils is not installed by default — confirmed directly against
// debian:bookworm-slim during development), so the harness discovers a
// candidate file from `dpkg -L Package` itself and inspects it with Go's
// stdlib debug/elf after copying it to the host, rather than depending on
// exact package-internal paths that drift across releases.
type ElfCheck struct {
	Package string `json:"package"`
	// Class is "ELF32" or "ELF64".
	Class string `json:"class"`
}

// TamperKind enumerates the corrupted-bundle cases (the tamper
// matrix): "each tamper case must fail before apt runs".
type TamperKind string

const (
	TamperModifiedDeb      TamperKind = "modified-deb"
	TamperAddedFile        TamperKind = "added-file"
	TamperRemovedFile      TamperKind = "removed-file"
	TamperEditedManifest   TamperKind = "edited-manifest"
	TamperSwappedSignature TamperKind = "swapped-signature"
	TamperWrongKey         TamperKind = "wrong-key"
	// TamperSymlinkedFile is the one tamper applied INSIDE the fresh target
	// rather than on the host: see tamperSymlinkedFileScript.
	TamperSymlinkedFile TamperKind = "symlinked-file"
)

// AppliedInTarget reports whether a tamper kind is applied inside the fresh
// target container instead of on the host copy of the bundle.
func (k TamperKind) AppliedInTarget() bool { return k == TamperSymlinkedFile }

// TamperSpec selects and, where needed, parameterises one corruption applied
// to a freshly built, signed bundle before it is copied to the fresh target
// container. Applying it on the host, directly to the copied-out bundle
// directory, keeps every tamper deterministic and fast (no container needed
// to flip a byte).
type TamperSpec struct {
	Kind TamperKind `json:"kind"`
}
