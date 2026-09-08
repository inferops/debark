// Package cliadapter is the single seam through which every debark
// invocation passes.
//
// Nothing outside this package may call exec.Command with "debark" in it
// (contract-brief.md, frozen contract 1). Every other packages mocks this
// package; internal/cliadapter/fake is the mock they build against.
//
// This file is frozen. It is owned by a frozen contract (W0-B) and declares the interface,
// the request shape, the error type and the argv builder. The three packages
// that implement it own separate files and must not redeclare anything here:
//
//	invoke.go  process execution, --json decoding, temp-file event tail
//	events.go  NDJSON decoding and delivery to an EventSink
//	errors.go  enrichment of *Error (hint catalogue, non-exec failures)
//
// # Three facts about the CLI that shape everything below
//
// They are recorded in full in docs/dev/cli-surface.md, which — with the real
// binary — outranks the contract brief where they disagree.
//
//  1. Errors are never JSON. --json does not change failure output. A failure
//     goes to stderr as "debark: <message>" plus an optional hint line, plus
//     cobra's usage block for usage-class (exit 1) errors. dferr.Error has no
//     JSON tags and is never serialised. The exit code is the only
//     machine-readable channel, which is why Error below is built from exactly
//     the three things a finished process leaves behind: argv, exit code and
//     captured stderr.
//
//  2. --json-events "-" shares stdout with --json. The NDJSON lines and the
//     final MarshalIndent'ed (multi-line) result object go to the same stream,
//     events first, so naive line-by-line parsing mangles the result. This
//     adapter therefore never passes "-": it passes a private temporary file
//     and tails it, leaving stdout clean for a plain json.Decoder. See
//     EventsDestination for the full argument.
//
//  3. Progress is thinner than it looks. There are exactly two progress
//     shapes, both type "progress", and neither carries a cumulative counter:
//     files-done and bytes-done must be accumulated by the consumer from
//     fetch.file events. total_bytes is -1 when the server sent no
//     Content-Length. See ProgressOf and FetchFileOf.
package cliadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/base"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/verify"
	"github.com/inferops/debark/core/version"
)

// ProgramName is argv[0] in everything Command returns. The real adapter
// execs Probe.Path and passes Command(spec)[1:]; the name is what a person
// sees, and what they could paste into a shell themselves.
const ProgramName = "debark"

// Adapter is the only way this application runs debark.
//
// Every method returns *Error on failure — never a bare *exec.ExitError, never
// a raw exit status. See Error.
type Adapter interface {
	// Probe locates the binary and reports its version and capabilities.
	Probe(ctx context.Context) (Probe, error)

	// ListBases returns the base definitions compiled into the binary, for
	// one architecture. Wraps `snapshot list-bases --json`.
	//
	// An empty arch means "the architecture debark would default to", which
	// is the host's; the returned List.Arch says which one that turned out to
	// be, and callers should read it rather than assume.
	ListBases(ctx context.Context, arch string) (*base.List, error)

	// InspectSnapshot reads a snapshot the operator chose from disk. Wraps
	// `snapshot inspect --json`. Nothing is uploaded anywhere.
	InspectSnapshot(ctx context.Context, path string) (SnapshotInfo, error)

	// Build runs a build to completion, delivering every NDJSON event to
	// sink as it arrives. Cancelling ctx cancels the build. Wraps `build`
	// with a private --json-events destination (see EventsDestination) —
	// unless the located binary has no such flag, in which case the build
	// runs without one and sink is never called. See
	// Capabilities.JSONEvents.
	//
	// The result and the error are NOT mutually exclusive. `build` is the one
	// command that prints its --json object and THEN exits with a non-zero
	// class: an incomplete build (exit 3) returns a fully populated
	// *buildjob.BuildResult alongside a *Error whose class is dferr.Incomplete.
	// Callers must check the result even when err != nil — Unresolved and
	// FetchFailed are the only place that detail exists.
	//
	// sink may be nil, in which case events are decoded and discarded.
	Build(ctx context.Context, spec BuildSpec, sink EventSink) (*buildjob.BuildResult, error)

	// Keygen creates an ed25519 operator signing key. Wraps `keygen`.
	// outPath is --out, which the CLI marks required.
	Keygen(ctx context.Context, outPath string) (KeyInfo, error)

	// Verify checks a bundle. Wraps `verify --json`.
	//
	// A bundle that fails verification is reported through the returned
	// *Error (class dferr.Verification) AND through the report's Problems.
	// As with Build, read both.
	Verify(ctx context.Context, bundlePath string) (VerifyReport, error)

	// Command returns the exact argv a spec would run, without running it,
	// so the UI can show the operator the command it is about to execute.
	// Rule 8 depends on this. Implementations return BuildArgv(spec) and
	// nothing else, so the fake and the real adapter cannot disagree.
	Command(spec BuildSpec) []string
}

// KeyedVerifier is the optional half of Adapter: an adapter that can be told
// which public keys a bundle's signature must verify against.
//
// It exists because `verify BUNDLE` with no --key and no configured
// verify_keys trusts nothing, so a bundle this application had just built and
// signed with the operator's own key failed its own verification screen with
// "no signature verifies against a trusted key". Adapter.Verify has nowhere to
// say which key, and the application does know: the one the readiness screen
// created and the build screen signed with.
//
// An extension interface rather than a third parameter on Adapter.Verify,
// deliberately. Adapter is frozen contract 1 and is implemented outside this
// package (internal/cliadapter/fake); widening the method would stop that
// compiling for every package at once, which the contract brief's "keep the tree
// compiling" rule forbids. A caller therefore type-asserts, and an
// implementation that cannot supply keys keeps working through Verify. When
// the fake can be updated alongside, this belongs on Adapter itself.
//
// Note for callers: --key REPLACES debark's configured verify_keys rather
// than adding to them (internal/cli/cmd_verify.go's firstNonEmptySlice), so
// passing a key narrows the trust set to exactly what is passed. Pass none to
// leave the operator's own debark configuration in charge.
type KeyedVerifier interface {
	// VerifyWithKeys checks a bundle against the given public key files.
	// An empty list behaves exactly like Adapter.Verify.
	VerifyWithKeys(ctx context.Context, bundlePath string, keys []string) (VerifyReport, error)
}

// Event is one decoded debark.events/v1 record.
//
// An alias, not a copy: hand-written copies of debark's structs drift and
// typed imports cannot. Everything the GUI knows about an event it learns
// from core/evidence.
type Event = evidence.Event

// EventSink receives one decoded debark.events/v1 event per call, in
// emission order, from the goroutine reading the stream. Implementations must
// not block: the build's output pipe is behind this call.
//
// "Must not block" means: do not do I/O, do not take a lock another goroutine
// holds across work, do not send on an unbuffered channel. Push onto a
// buffered channel or a mutex-guarded slice and return. A sink that stalls
// stalls the tail of the event file, and — once the OS pipe buffer behind
// stdout fills — the build process itself.
type EventSink func(Event)

// ---------------------------------------------------------------------------
// Probe
// ---------------------------------------------------------------------------

// Probe is what a successful `version --json` plus one capability check told
// the app about the binary it found.
type Probe struct {
	// Path is the absolute path to the debark binary this adapter will
	// exec. It is shown in the about box and in the details drawer of any
	// error, because "which debark" is the first question when a build
	// behaves unexpectedly.
	Path string

	// Info is the whole `version --json` document, imported verbatim from
	// core/version. It is the authoritative source for version, edition and
	// platform; the accessors below exist so callers need not remember which
	// field is which, not as a second copy of the data.
	Info version.Info

	// HostArch is the dpkg architecture debark defaults to when --arch is
	// not given, learned from `snapshot list-bases --json` (whose List.Arch
	// records what the listing was rendered for). The base picker needs a
	// default selection and must not guess it from runtime.GOARCH: the GUI's
	// own GOARCH is a fact about the GUI, not about what debark would do.
	HostArch string

	// Capabilities are the facts the app branches on. Keep this list short
	// and each entry load-bearing: a capability nothing reads is a lie
	// waiting to happen.
	Capabilities Capabilities
}

// Version is Info.Version, e.g. "1.2.0" or "dev".
func (p Probe) Version() string { return p.Info.Version }

// Edition is Info.Edition: community, official or enterprise. It is branding
// and audit metadata, never a feature gate (ADR-010) — no code in this
// repository may behave differently because of its value.
func (p Probe) Edition() string { return p.Info.Edition }

// Platform is Info.Platform, e.g. "linux/amd64".
func (p Probe) Platform() string { return p.Info.Platform }

// Capabilities are the yes/no facts about a located binary that change what
// the UI offers.
type Capabilities struct {
	// Bases is true when `snapshot list-bases` exists, i.e. the binary
	// supports building against a stock release with `build --base`. When
	// false the target screen must offer only "choose a snapshot file".
	Bases bool

	// JSONEvents is true when the root command carries --json-events. When
	// false there is no progress stream at all and the build screen must show
	// an indeterminate spinner rather than a bar.
	//
	// Build honours it: against a binary that lacks the flag it leaves the
	// flag off, reserves no events file and starts no tailer, so the build
	// runs and the caller's EventSink is simply never called. That is not a
	// courtesy — cobra answers an unknown flag with an error and its usage
	// block, so passing it unconditionally made every such build die on exit
	// 1. Build asks the binary itself rather than reading this field, because
	// nothing obliges a caller to have probed first; the answer is memoised
	// per adapter, and Probe fills the memo for free.
	JSONEvents bool

	// Keygen is true when `keygen` exists, i.e. the app may offer to create
	// an operator signing key rather than only accepting an existing one.
	Keygen bool

	// SBOM is true when `build --sbom` exists.
	SBOM bool
}

// ---------------------------------------------------------------------------
// SnapshotInfo, KeyInfo, VerifyReport
// ---------------------------------------------------------------------------

// SnapshotInfo is what the operator's chosen snapshot file turned out to be.
//
// WRAPPED, not aliased, and for one specific reason: `snapshot inspect --json`
// prints the bare debark.snapshot/v1 document, which does not contain the
// path it was read from (unlike `inspect --json`, whose envelope does carry
// bundle_path). The GUI shows the operator which file they picked next to
// what is in it, so the path has to be carried alongside. Everything else is
// core/snapshot's own type, imported verbatim.
type SnapshotInfo struct {
	// Path is the file the operator chose, as this process sees it. Not from
	// the CLI's JSON — the adapter fills it in from the argv it ran.
	Path string
	// Snapshot is the debark.snapshot/v1 document, unmodified.
	Snapshot snapshot.Snapshot
}

// Synthesized reports whether this snapshot was assumed from a base rather
// than measured on a real machine. The distinction changes what a bundle
// built from it can promise, so the UI must show it (ADR-014).
func (s SnapshotInfo) Synthesized() bool { return s.Snapshot.Synthesized() }

// Target is the identity of the machine this snapshot describes.
func (s SnapshotInfo) Target() snapshot.Target { return s.Snapshot.Target }

// KeyInfo is what `keygen --json` reported.
//
// WRAPPED, and here there was no choice: keygen emits an ad-hoc
// map[string]string with keys key_id, private_key and public_key. There is no
// importable Go type and no schema_version. This struct IS that map, given a
// name and JSON tags that match it exactly, so the decode is one step and the
// field names are checkable against the CLI.
type KeyInfo struct {
	// KeyID is the generated key's id, as the CLI computed it.
	KeyID string `json:"key_id"`
	// PrivateKeyPath is the --out path, echoed back. Mode 0600, unencrypted.
	PrivateKeyPath string `json:"private_key"`
	// PublicKeyPath is the private path with its .key suffix replaced by
	// .pub (or .pub appended when there was no .key suffix). The target must
	// receive this file out of band, never on the same media as the bundle.
	PublicKeyPath string `json:"public_key"`
}

// VerifyReport is `verify --json`, debark.verifyreport/v1.
//
// ALIASED, not wrapped: core/verify.Report is a published, versioned document
// that already carries BundlePath, so there is nothing for a wrapper to add.
// The alias exists so that if the CLI ever grows an envelope around it, this
// name becomes a struct and no call site changes.
type VerifyReport = verify.Report

// ---------------------------------------------------------------------------
// BuildSpec
// ---------------------------------------------------------------------------

// BuildSpec is the GUI's own build request.
//
// It is translated into FLAGS, and deliberately does not reuse
// buildjob.BuildRequest, because the CLI takes flags and not a job document.
// Every field maps to exactly one documented flag or to one class of
// positional argument, and each field's comment names it. BuildArgv is the
// single translation, and it is the only one: the fake and the real adapter
// both call it, so Command cannot drift from what actually runs.
//
// There is deliberately NO field for --interactive. The GUI must never pass
// it: it makes debark ask questions on a terminal that is not there.
type BuildSpec struct {
	// --- Target. Exactly one of SnapshotPath and BaseID. ---

	// SnapshotPath is --snapshot: a snapshot archive or extracted directory
	// captured from the real target. Prefer it: it proves the bundle is
	// complete for that machine, where a base only assumes it.
	SnapshotPath string
	// BaseID is --base: a builtin id such as "ubuntu:26.04/desktop", or a
	// path to an operator's base definition file.
	BaseID string
	// Arch is --arch: the dpkg architecture the --base describes. It is
	// meaningful ONLY with BaseID; the CLI rejects it alongside --snapshot,
	// because a snapshot already records the target's own architecture.
	// Empty means the host's.
	Arch string

	// --- Inputs. Positional arguments, plus two repeatable flags. ---

	// Packages are apt package names, optionally "name=version". Emitted as
	// positional "apt:NAME" arguments — the explicit prefix, so a package
	// name that happens to end in .deb or look like a URL cannot be
	// misclassified by the CLI's argument grammar.
	Packages []string
	// URLs are https:// .deb downloads with an optional operator-supplied
	// SHA-256. Emitted as positional "url:URL" arguments; a non-empty SHA256
	// additionally emits "--digest URL=SHA256", which upgrades the input's
	// provenance from url-unverified to user-digest.
	//
	// buildjob.URLInput is imported rather than mirrored.
	URLs []buildjob.URLInput
	// LocalDebs are local .deb file paths. Emitted as positional "file:PATH"
	// arguments.
	LocalDebs []string
	// LocalDirs is --local-dir, repeatable: a directory of vendor .deb files,
	// scanned but not recursively.
	LocalDirs []string
	// ListFiles is --list, repeatable: a packages.txt-style input list. The
	// GUI exists to remove the blank-packages.txt problem, but an operator
	// who already has one should not have to retype it.
	ListFiles []string

	// --- Output. ---

	// OutPath is --out when Format is dir (or empty) and --tar when Format is
	// tar. The two flags are mutually exclusive in the CLI, which is why this
	// is one field and not two: a spec cannot express the contradiction.
	// Empty means the CLI's default, ./bundle.
	OutPath string
	// Format selects which flag OutPath becomes. buildjob.OutputFormat is
	// imported, not mirrored; the zero value ("") means FormatDir.
	Format buildjob.OutputFormat

	// --- Signing. --sign and --no-sign are mutually exclusive. ---

	// SignerRef is --sign: a private key file, "gpg:<keyid>" or
	// "plugin:<name>". Setting it makes signing REQUIRED — the build fails
	// rather than writing an unsigned bundle.
	SignerRef string
	// NoSign is --no-sign: write an unsigned bundle, explicitly. With neither
	// this nor SignerRef set, debark uses a configured default key if there
	// is one and writes an unsigned bundle if there is not, recording the
	// fact in BuildResult.Signed.
	NoSign bool

	// --- Options. ---

	// SBOM is --sbom: also write sbom.cdx.json (CycloneDX).
	SBOM bool
	// Recommends overrides the target's own Install-Recommends, and has three
	// states because the CLI now has three:
	//
	//	nil    follow the target's apt.conf — no flag
	//	false  --no-recommends, a smaller bundle
	//	true   --recommends, pull recommended packages in
	//
	// A *bool rather than two booleans, mirroring debark's own
	// Options.Recommends. Two booleans can express "both", which the CLI
	// rejects as mutually exclusive, and a spec that can hold a contradiction
	// makes Validate responsible for a state this shape simply cannot reach.
	//
	// It used to be a one-directional NoRecommends, which matched a CLI that
	// only had --no-recommends. That is no longer true, and a bool is now a
	// lossy model: "follow the target" and "definitely exclude" were the same
	// value.
	Recommends *bool
	// Upgrades is --upgrades: add a full-upgrade pass for packages already
	// installed on the target. Note that Update implies it in the engine, so
	// setting both is harmless and setting neither is not the same as
	// setting Update alone.
	Upgrades bool
	// Update is --update: refresh indexes, re-resolve, fetch newer versions,
	// then prune superseded files. Without it a build is additive — it adds
	// what is missing and touches nothing else.
	Update bool
	// NoPrune is --no-prune: with Update, keep superseded files instead of
	// pruning them. It has no effect without Update.
	NoPrune bool
	// Backend is --backend: "auto", "local" or "container". Empty means the
	// config's backend, else auto.
	Backend string
	// Image is --image: override the container image for the container
	// backend.
	Image string
	// PolicyPath is --policy: a local policy file evaluated against the plan.
	// A deny finding fails the build with dferr.Policy (exit 6).
	PolicyPath string
	// ApprovedKeysPath is --approved-keys: archive key fingerprints
	// resolution must satisfy.
	ApprovedKeysPath string
	// AcknowledgeRedistribution is --acknowledge-redistribution: suppress the
	// interactive redistribution prompt. The warnings are still recorded in
	// the bundle and in BuildResult.Warnings.
	//
	// The GUI must set this only after the operator has actually seen and
	// accepted the redistribution notice in the UI. It is not a convenience
	// flag; it is the record that a person was asked.
	AcknowledgeRedistribution bool
	// EmbedBinary is --embed-binary: copy a debark binary into the bundle
	// at bin/debark-linux-<arch>, so the offline side has the tool without
	// a chicken-and-egg problem.
	EmbedBinary string
}

// Validate reports whether spec can be turned into a legal command line.
//
// It exists so the UI fails fast with a sentence a person can act on, instead
// of shelling out and rendering cobra's usage block. It enforces exactly the
// constraints the CLI enforces and no more — it is not a second engine, and
// it never inspects the filesystem. Every failure is dferr.Usage.
func (s BuildSpec) Validate() error {
	argv := []string{ProgramName, "build"}
	fail := func(summary, hint string) error {
		return newError(argv, int(dferr.Usage), "", summary, hint)
	}
	switch {
	case s.SnapshotPath == "" && s.BaseID == "":
		return fail("no target: a build needs either a snapshot of the real machine or a stock base to assume",
			"pick a snapshot file captured with `debark snapshot create`, or choose a base from `debark snapshot list-bases`")
	case s.SnapshotPath != "" && s.BaseID != "":
		return fail("both a snapshot and a base were given; they name the same thing two different ways",
			"clear one of them: --snapshot proves the bundle is complete for that machine, --base only assumes it")
	}
	if s.Arch != "" && s.SnapshotPath != "" {
		return fail("an architecture was given alongside a snapshot",
			"a snapshot records the target's own architecture; clear the architecture, or switch the target to a base")
	}
	if s.SignerRef != "" && s.NoSign {
		return fail(`both a signing key and "write it unsigned" were given`,
			"clear one: a key makes signing required, and unsigned is an explicit choice on its own")
	}
	if len(s.Packages) == 0 && len(s.URLs) == 0 && len(s.LocalDebs) == 0 &&
		len(s.LocalDirs) == 0 && len(s.ListFiles) == 0 {
		return fail("nothing was selected to put in the bundle",
			"choose at least one package, vendor .deb URL, local .deb file, directory or list file")
	}
	for _, u := range s.URLs {
		if strings.TrimSpace(u.URL) == "" {
			return fail("a vendor download was listed with no URL",
				"remove the empty row, or type its https:// URL")
		}
		if u.SHA256 != "" && !isHex64(u.SHA256) {
			return fail(fmt.Sprintf("the expected digest for %s is not a SHA-256", u.URL),
				`a SHA-256 is 64 hexadecimal characters, with no "sha256:" prefix`)
		}
		if u.SHA256 != "" && !digestPairIsExpressible(u.URL) {
			return fail(fmt.Sprintf(`the expected digest for %s cannot be passed to this debark, because the URL contains "="`, u.URL),
				"`build --digest` splits the pair at the first \"=\", so a URL with a query string loses its digest "+
					"without saying so. Either clear the expected SHA-256 for this one URL and accept an unverified "+
					"download — which is what would silently have happened — or use a URL with no query string, or "+
					"download the file yourself and add it as a local .deb, where the digest is checked on disk. A "+
					"newer debark whose `build --help` says --digest is split at the LAST '=' does not have this limit")
		}
	}
	if s.Format != "" && s.Format != buildjob.FormatDir && s.Format != buildjob.FormatTar {
		return fail(fmt.Sprintf("unknown output format %q", string(s.Format)),
			"the output is either a directory (dir) or a .debark.tar.zst archive (tar)")
	}
	return nil
}

// digestPairIsExpressible reports whether "URL=SHA256" survives the parser on
// the other end and binds the digest to the URL the operator typed.
//
// --digest is a pflag stringToString, which cannot express this flag. It goes
// wrong in two different ways depending on how many "=" the pair holds, and
// three of the four resulting shapes are silent.
//
// With exactly one "=" in the URL half, Set splits the pair at the FIRST "=".
// `https://vendor.example/download?file=agent_2.1.0_amd64.deb` is an ordinary
// download URL, not a contrived one, and it binds the digest to the key
// `https://vendor.example/download?file`.
//
// With two or more, Set first runs the whole value through encoding/csv, where
// a comma is a field separator and a quote starts a quoted field. So
// `…/a.deb?ids=1,2=<sha>` becomes TWO map entries and puts the digest under the
// key "2"; and `…/a.deb?q="x"=<sha>` fails outright with `bare " in
// non-quoted-field`. The last of those is at least loud.
//
// In every one of them the digest never reaches the URL the operator typed.
// `internal/cli/cmd_build.go`'s buildInputs looks it up as `digests[val]` with
// val the exact positional URL, so the miss costs nothing and says nothing: the
// download proceeds with provenance url-unverified while the UI still shows the
// digest as recorded. They asked for user-digest, the strongest provenance
// claim debark makes about a file, and quietly did not get it.
//
// That is not reasoning. It was driven against a real binary: two builds
// differing only in whether the vendor URL carried a query string, both given
// the same deliberately wrong --digest. The plain URL failed with "sha256
// mismatch: expected 1111…, got bcfb…"; the query-string URL was accepted with
// publisher_verification "url-unverified". docs/security-review.md §6.5a has
// the transcript.
//
// No escaping on this side helps, which is why this refuses rather than fixes.
// Percent-encoding the "=" in the key half — the first of the two remedies
// §6.5 suggested — makes the key differ from the positional URL, so
// `digests[val]` misses in exactly the same silent way; and the positional URL
// cannot be encoded to match, because that is the URL debark fetches.
// Quoting does not help either: pflag reaches for encoding/csv only when the
// pair contains two or more "=", and csv does not protect the SplitN that
// follows it.
//
// # The core repository has fixed this, and `build` has not got the fix yet
//
// debark commit a9fa119's sibling, 92ebdc2, added `registerDigestFlag`
// (`internal/cli/digestflag.go`): a flag type that splits the pair at the LAST
// "=" and validates the digest at parse time. Splitting last is exact rather
// than a better guess — a SHA-256 is 64 hexadecimal characters and can never
// contain "=" — and the parse-time validation is what makes it safe, because a
// genuinely malformed pair becomes a loud error naming where the split landed
// instead of a quiet misbinding.
//
// It was adopted for `fetch` only. `build`'s registration is in
// `internal/cli/cmd_build.go`, which carried uncommitted work belonging to
// nobody on that side, so the adoption was left as a documented one-line
// change: `StringToStringVar(&digests, "digest", …)` becomes
// `registerDigestFlag(flags, &digests)`, with `digests` and `buildInputs`
// untouched.
//
// `build` is the command this application runs. So the defect is still live
// here, and this refusal is still the right behaviour — not a workaround for a
// bug nobody has fixed, but the correct handling of a binary that has not got
// the fix.
//
// # What to do the day `build` adopts it
//
// Delete this function, its call in Validate, and the "=" rows in
// digest_test.go. Two things have to be true first, and only the first is
// about the core repository:
//
//  1. `build` registers --digest through registerDigestFlag. Nothing else
//     changes: the argv this package emits is already correct for a
//     last-"=" split, including the presigned-URL shape whose signature ends
//     in base64 padding ("…&sig=abcd==<sha>"), which splits correctly because
//     the digest half cannot contain "=".
//  2. Every debark this application will run has it. That is the part this
//     package cannot assume, because it runs whatever binary it finds. If the
//     project cannot simply require a minimum version, the detection is
//     cheap and already half-built: `registerDigestFlag` gives both commands
//     one shared usage string containing "split at the LAST '='", and Probe
//     already reads `build --help` for the --sbom capability. A capability
//     read from the same text would let this refusal apply only to binaries
//     that need it. Do NOT gate it on a version comparison — this repository
//     compares no version strings, for anything (rule 1).
//
// # The rule is only as wide as the defect
//
// A comma is fine: with exactly one "=" in the pair, pflag takes the non-csv
// branch and the comma is just a character in the key.
func digestPairIsExpressible(rawURL string) bool {
	return !strings.Contains(rawURL, "=")
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// BuildArgv is the single translation from a BuildSpec to a debark command
// line. Both the real adapter and the fake return it from Command, so a test
// that pins flag construction pins the thing that actually runs.
//
// It does NOT include --json-events. That destination is this adapter's own
// temporary file (see EventsDestination); a path under the system temp
// directory is meaningless to an operator who wants to paste the command, and
// it is not part of what the GUI is asking debark to do. Everything else the
// adapter passes is here, --json included, because the GUI really does run
// with --json and showing a command that would behave differently is a lie.
//
// Order is fixed so the output is stable and diffable: target, input flags,
// output, options, digests, --json, then positional arguments after a "--"
// separator.
func BuildArgv(spec BuildSpec) []string {
	argv := []string{ProgramName, "build"}
	add := func(flag, value string) {
		if value != "" {
			argv = append(argv, flag, value)
		}
	}
	addBool := func(flag string, on bool) {
		if on {
			argv = append(argv, flag)
		}
	}

	add("--snapshot", spec.SnapshotPath)
	add("--base", spec.BaseID)
	if spec.BaseID != "" {
		add("--arch", spec.Arch)
	}

	for _, l := range spec.ListFiles {
		add("--list", l)
	}
	for _, d := range spec.LocalDirs {
		add("--local-dir", d)
	}

	if spec.Format == buildjob.FormatTar {
		add("--tar", spec.OutPath)
	} else {
		add("--out", spec.OutPath)
	}

	addBool("--update", spec.Update)
	addBool("--no-prune", spec.NoPrune)
	addBool("--upgrades", spec.Upgrades)
	// Three states, one of which emits nothing. The CLI marks the two flags
	// mutually exclusive, and this shape cannot emit both.
	if spec.Recommends != nil {
		if *spec.Recommends {
			argv = append(argv, "--recommends")
		} else {
			argv = append(argv, "--no-recommends")
		}
	}
	add("--backend", spec.Backend)
	add("--image", spec.Image)
	add("--sign", spec.SignerRef)
	addBool("--no-sign", spec.NoSign)
	add("--approved-keys", spec.ApprovedKeysPath)
	add("--policy", spec.PolicyPath)
	addBool("--acknowledge-redistribution", spec.AcknowledgeRedistribution)
	add("--embed-binary", spec.EmbedBinary)
	addBool("--sbom", spec.SBOM)

	// --digest is stringToString and repeatable. Emitted in URL order rather
	// than sorted, so the command line reads in the same order as the rows
	// the operator typed.
	//
	// This stays a plain translation, including for a URL containing "=",
	// which `build`'s pflag registration still mis-splits — see
	// digestPairIsExpressible. Refusing such a spec is Validate's job and
	// Validate does refuse it, so a build never runs one; making the
	// translation silently drop the pair as well would put the decision in two
	// places and would hide, in the previewed command, the very attestation the
	// operator asked for.
	//
	// It is also already correct for the fixed flag: a last-"=" split reads
	// this exact string the way the operator meant it, so the day
	// `build` adopts registerDigestFlag nothing here changes.
	for _, u := range spec.URLs {
		if u.SHA256 != "" {
			argv = append(argv, "--digest", u.URL+"="+u.SHA256)
		}
	}

	// --json is not optional for this application. It suppresses colour,
	// progress rendering and — the reason it is mandatory — every interactive
	// prompt. A GUI that let debark ask a question would hang forever.
	argv = append(argv, "--json")

	// Positional inputs, behind "--" and with explicit apt:/url:/file:
	// prefixes. The CLI would otherwise classify a bare argument by shape
	// (https:// is a URL, a .deb suffix is a file, anything else is a package
	// name); being explicit means a spec round-trips exactly, whatever the
	// operator typed.
	pos := make([]string, 0, len(spec.Packages)+len(spec.URLs)+len(spec.LocalDebs))
	for _, p := range spec.Packages {
		pos = append(pos, "apt:"+p)
	}
	for _, u := range spec.URLs {
		pos = append(pos, "url:"+u.URL)
	}
	for _, f := range spec.LocalDebs {
		pos = append(pos, "file:"+f)
	}
	if len(pos) > 0 {
		argv = append(argv, "--")
		argv = append(argv, pos...)
	}
	return argv
}

// ---------------------------------------------------------------------------
// The --json-events destination
// ---------------------------------------------------------------------------

// EventsDestination names how this adapter receives the NDJSON event stream.
// It is documentation with a compile-time home, not a knob: the interface
// deliberately does not let a caller choose, and no method takes or returns a
// path.
//
// The choice is a TEMPORARY FILE, whose name the adapter reserves, passes as
// --json-events <path>, tails while the process runs, drains after it exits,
// and removes. The alternatives, and why they lost:
//
//   - --json-events "-" writes NDJSON to stdout, which --json is also using.
//     The event lines are compact single-line objects and the final result is
//     MarshalIndent'ed, so they CAN be told apart — but only by a heuristic
//     about JSON formatting, in the one place where being wrong means the
//     operator's build result silently fails to parse. Rejected.
//
//   - An OS pipe passed as an extra file descriptor (cmd.ExtraFiles plus
//     /dev/fd/3) is the cleanest thing on Linux and does not exist on
//     Windows: ExtraFiles is documented as Unix-only and there is no /dev/fd.
//     This repository is developed on Windows and Wails builds for it.
//     Rejected.
//
//   - A FIFO (mkfifo) is Unix-only for the same reason, and worse: opening
//     one for reading blocks until a writer appears, so a debark that fails
//     before it opens the stream deadlocks the reader. Rejected.
//
// The temp file costs a polling tail — a few reads a second against the page
// cache, invisible next to a build that downloads packages — and buys three
// things: stdout stays a clean, plain json.Decoder input; the mechanism is
// identical on Linux and Windows; and the file is still on disk if the build
// crashes, which is the moment its contents matter most. debark opens the
// path with os.Create, so a name the adapter has reserved but not written is
// exactly what it expects.
const EventsDestination = "temporary file, tailed by the adapter"

// ProgressAttrs is one decoded byte-progress event.
//
// There is no cumulative counter anywhere on the wire. A consumer that wants
// "17 of 240 files, 412 MB of 1.1 GB" must accumulate it itself from
// fetch.file events; this struct describes ONE file's download, and the
// stream interleaves several of them when the fetcher runs in parallel. Key
// the accumulation on URL.
type ProgressAttrs struct {
	// URL is the file being downloaded. It is the only correlation key.
	URL string
	// Bytes is how much of THIS file has arrived so far.
	Bytes int64
	// TotalBytes is this file's size, or -1 when the server sent no
	// Content-Length. -1 is common for vendor URLs behind a CDN, and a
	// progress bar that multiplies by it renders a negative width. Check it.
	TotalBytes int64
	// Retry is true when this is the retry-notice shape rather than the byte
	// shape: attrs{url, attempt, attempts} and no bytes at all.
	Retry bool
	// Attempt and Attempts are set only when Retry is true, 1-based.
	Attempt, Attempts int
}

// Done reports whether this file's download has finished, as far as the byte
// counters can say. It is false whenever the total is unknown.
func (p ProgressAttrs) Done() bool { return p.TotalBytes > 0 && p.Bytes >= p.TotalBytes }

// Fraction returns how far this file has got, or -1 when the total is
// unknown. Callers must handle -1 by rendering an indeterminate bar.
func (p ProgressAttrs) Fraction() float64 {
	if p.TotalBytes <= 0 {
		return -1
	}
	f := float64(p.Bytes) / float64(p.TotalBytes)
	if f > 1 {
		return 1
	}
	return f
}

// ProgressOf decodes a "progress" event's attrs into ProgressAttrs. It
// reports false for any other event type.
//
// Both progress shapes are handled: attrs{url, bytes, total_bytes} for byte
// progress (throttled to once per 250 ms per file, plus a final emit), and
// attrs{url, attempt, attempts} for a retry notice.
func ProgressOf(e Event) (ProgressAttrs, bool) {
	if e.Type != evidence.TypeProgress {
		return ProgressAttrs{}, false
	}
	p := ProgressAttrs{URL: attrString(e.Attrs, "url"), TotalBytes: -1}
	if _, ok := e.Attrs["attempts"]; ok {
		p.Retry = true
		p.Attempt = int(attrInt(e.Attrs, "attempt"))
		p.Attempts = int(attrInt(e.Attrs, "attempts"))
		return p, true
	}
	p.Bytes = attrInt(e.Attrs, "bytes")
	if _, ok := e.Attrs["total_bytes"]; ok {
		p.TotalBytes = attrInt(e.Attrs, "total_bytes")
	}
	return p, true
}

// FetchFileAttrs is one decoded fetch.file event: a file that finished
// downloading. This is the ONLY event that says a download completed, so a
// files-done counter is a count of these.
type FetchFileAttrs struct {
	URL      string
	Filename string
	// Digest is the lowercase hex SHA-256 of the fetched file.
	Digest string
	// Size is the file's size in bytes. Accumulate it for bytes-done.
	Size int64
	// Verification is the provenance claim: apt-signed, url-unverified,
	// user-digest or user-signature (core/lock.PublisherVerification). The UI
	// should surface url-unverified, because it is the weakest claim debark
	// will make about anything it carries.
	Verification string
}

// FetchFileOf decodes a "fetch.file" event. It reports false for any other
// event type.
func FetchFileOf(e Event) (FetchFileAttrs, bool) {
	if e.Type != evidence.TypeFetchFile {
		return FetchFileAttrs{}, false
	}
	return FetchFileAttrs{
		URL:          attrString(e.Attrs, "url"),
		Filename:     attrString(e.Attrs, "filename"),
		Digest:       attrString(e.Attrs, "digest"),
		Size:         attrInt(e.Attrs, "size"),
		Verification: attrString(e.Attrs, "verification"),
	}, true
}

func attrString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// attrInt reads a numeric attr. JSON decoding gives float64 (or json.Number
// when the decoder was told to); an event built in Go and never serialised —
// the fake's script — gives int or int64. All of them are accepted, so the
// fake and the wire cannot disagree about what a progress event means.
func attrInt(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return n
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// Error
// ---------------------------------------------------------------------------

// MaxStderrBytes caps how much captured stderr an Error carries. A usage-class
// failure includes cobra's whole help text, and a build that fails late can
// print more; the details drawer needs enough to diagnose and not a megabyte.
// Truncation is marked, never silent.
const MaxStderrBytes = 64 << 10

// Error is what every Adapter method returns on failure.
//
// It is built from the only three things a finished debark process leaves
// behind: the argv it was given, the exit code it returned, and what it wrote
// to stderr. --json does not change failure output, dferr.Error has no JSON
// tags and is never serialised, and so the exit code is the sole
// machine-readable channel. That is the reality this type is shaped for.
//
// The definition of done says no error path may surface a raw exit code
// alone. The shape enforces it rather than asking:
//
//   - There is no exported exit-code field. The number is reachable only
//     through ExitCode(), whose documentation says it is for logs and tests.
//   - Summary is never empty. Every constructor funnels through newError,
//     which falls back to the class's own one-line description — a sentence,
//     not a number.
//   - Error() always renders the summary.
//   - The fields are unexported, so outside this package there is no way to
//     construct an Error that lacks a summary at all.
//
// errors.go owns enrichment — a hint catalogue keyed on class and
// stderr, plus classification of the failures that never reach a process at
// all (binary not found, JSON that would not decode, a cancelled context). It
// does that with WithSummary, WithHint and WithCause, and must NOT redeclare
// NewError, newError or Error.
type Error struct {
	class    dferr.Class
	exitCode int
	summary  string
	hint     string
	argv     []string
	stderr   string
	canceled bool
	cause    error
}

// NewError builds an *Error from a finished process.
//
// It always returns a non-nil *Error. Deciding whether the process actually
// failed is the caller's job — and calling it with exitCode 0 is legitimate
// and meaningful: it is the "debark succeeded but the adapter could not use
// its output" case, such as a --json document that would not decode.
//
// The mapping, in one place:
//
//   - Exit 0..7 is the frozen dferr table and the code IS the class.
//   - Anything else — 127 from a shell that could not find the binary, -1
//     from a process killed by a signal, a Windows exception status — is
//     dferr.Environment, not dferr.Usage. dferr.ClassOf falls back to Usage
//     for an unclassified in-process error, which is right there and wrong
//     here: a process that died in a way debark did not choose is a fact
//     about the machine, not about the operator's flags. The raw number is
//     kept and appears in the summary.
//   - The summary is the first line of stderr with the "debark: " prefix
//     removed, taken from before cobra's usage block. Every line after it and
//     before that block becomes the hint, because that is exactly where
//     dferr.Error's own Hint is printed.
//   - An empty stderr — which is what a failing `build` leaves behind,
//     because it reports through its --json result and a silent error — falls
//     back to the class's own frozen description.
func NewError(argv []string, exitCode int, stderr string) *Error {
	summary, hint := splitStderr(stderr)
	return newError(argv, exitCode, stderr, summary, hint)
}

// NewCanceledError is the one failure that is not debark's: the operator
// pressed Stop, or the app is shutting down.
//
// dferr has no class for cancellation — the taxonomy is about what went wrong
// with the work, and nothing did. Class is dferr.Usage, matching dferr's own
// fallback for an error carrying no class, and Canceled() is how the UI tells
// the two apart without matching on a string. See docs/dev/cli-surface.md,
// "What the CLI does not give the GUI".
func NewCanceledError(argv []string) *Error {
	e := newError(argv, int(dferr.Usage), "",
		"the build was cancelled before it finished",
		"nothing reached the target; the partial output directory can be deleted, or reused by building again")
	e.canceled = true
	e.cause = context.Canceled
	return e
}

// newError is the single place an Error comes into existence. Every exported
// constructor funnels through it, so the invariants — non-empty summary,
// bounded stderr, copied argv — hold for all of them.
func newError(argv []string, exitCode int, stderr, summary, hint string) *Error {
	class := dferr.Environment
	if exitCode >= int(dferr.Success) && exitCode <= int(dferr.TargetMismatch) {
		class = dferr.Class(exitCode)
	}
	if summary == "" {
		if exitCode < int(dferr.Success) || exitCode > int(dferr.TargetMismatch) {
			summary = fmt.Sprintf("debark stopped without a message (exit status %d)", exitCode)
		} else {
			summary = class.Description()
		}
	}
	if len(stderr) > MaxStderrBytes {
		stderr = stderr[:MaxStderrBytes] + "\n… (truncated)"
	}
	return &Error{
		class:    class,
		exitCode: exitCode,
		summary:  summary,
		hint:     hint,
		argv:     append([]string(nil), argv...),
		stderr:   stderr,
	}
}

// splitStderr turns debark's failure output into a summary and a hint.
//
// The shape it parses is exactly what internal/cli's Execute prints:
//
//	debark: <message>
//	<hint>              (only when the error carried one)
//
//	Usage:              (only for usage-class errors)
//	  debark build ...
//	  ...
func splitStderr(stderr string) (summary, hint string) {
	s := strings.ReplaceAll(stderr, "\r\n", "\n")
	// Cut cobra's usage block. It starts at a line that is exactly "Usage:",
	// and everything from there on is help text, not a message.
	if i := strings.Index(s, "\nUsage:"); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, "Usage:") {
		s = ""
	}
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimRight(line, " \t"); strings.TrimSpace(t) != "" {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return "", ""
	}
	summary = strings.TrimSpace(strings.TrimPrefix(kept[0], ProgramName+": "))
	if len(kept) > 1 {
		hint = strings.TrimSpace(strings.Join(kept[1:], "\n"))
	}
	return summary, hint
}

// Error renders the failure as one line: the summary, then the class in
// parentheses. Never a bare exit code — there is nothing here a caller can
// print that does not say what happened.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s (%s)", e.summary, e.class)
}

// Unwrap exposes the cause, when one was attached, so errors.Is and errors.As
// keep working through this type.
func (e *Error) Unwrap() error { return e.cause }

// Class is the dferr class, whose numeric value is debark's exit code.
func (e *Error) Class() dferr.Class { return e.class }

// ClassName is the stable string — "resolution", "policy", … — safe to put in
// a UI, a log or a test assertion.
func (e *Error) ClassName() string { return e.class.String() }

// ExitCode is the raw status the process returned.
//
// For logs, tests and bug reports. Never render it to an operator on its own:
// Summary and Hint are what a person reads, and "exit 5" is not an error
// message.
func (e *Error) ExitCode() int { return e.exitCode }

// Summary is the human sentence. It is never empty.
func (e *Error) Summary() string { return e.summary }

// Hint is what to do next, or "" when debark offered none. The UI shows it
// under the summary, in the same place debark's own terminal output does.
func (e *Error) Hint() string { return e.hint }

// Argv is the command that produced this failure, for the details drawer and
// for "copy the command". A copy: the caller cannot reach into the error.
func (e *Error) Argv() []string { return append([]string(nil), e.argv...) }

// CommandLine is Argv joined for display. It quotes nothing and escapes
// nothing — it is a label, not something to hand to a shell.
func (e *Error) CommandLine() string { return strings.Join(e.argv, " ") }

// Stderr is everything debark wrote to stderr, capped at MaxStderrBytes and
// marked when truncated. This is the details drawer's body; cobra's usage
// block lives in here, which is why it is not in Summary.
func (e *Error) Stderr() string { return e.stderr }

// Canceled reports whether this is a cancellation rather than a failure. The
// UI must not show a cancelled build as an error.
func (e *Error) Canceled() bool { return e.canceled }

// HasClass reports whether this error carries class c, so callers can branch
// on the taxonomy without reaching for dferr's own helpers.
//
// Named HasClass rather than Is: a method Is(dferr.Class) bool on an error
// type shadows nothing but reads exactly like errors.Is's optional
// Is(error) bool hook, and go vet flags the near-miss. The two mean different
// things and should not look alike.
func (e *Error) HasClass(c dferr.Class) bool { return e != nil && e.class == c }

// WithSummary returns a copy carrying a better summary.
//
// The case it exists for: a failing `build` prints its BuildResult and then
// exits with the mapped class through a SILENT error, so stderr is empty and
// the fallback summary is the class's generic description. A caller holding
// the decoded result can say "3 packages could not be resolved: …" instead.
func (e *Error) WithSummary(format string, args ...any) *Error {
	c := *e
	c.summary = fmt.Sprintf(format, args...)
	c.argv = append([]string(nil), e.argv...)
	return &c
}

// WithHint returns a copy carrying a next action. errors.go's hint catalogue
// is built on this.
func (e *Error) WithHint(format string, args ...any) *Error {
	c := *e
	c.hint = fmt.Sprintf(format, args...)
	c.argv = append([]string(nil), e.argv...)
	return &c
}

// WithCause returns a copy wrapping err, so errors.Is and errors.As can find
// it.
func (e *Error) WithCause(err error) *Error {
	c := *e
	c.cause = err
	c.argv = append([]string(nil), e.argv...)
	return &c
}

// AsError extracts the *Error from err, if there is one anywhere in its
// chain. Every Adapter method returns one, so this always succeeds on a
// non-nil error out of this package; the boolean is there so a caller that
// receives an error from somewhere else does not have to guess.
func AsError(err error) (*Error, bool) {
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e, true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}
