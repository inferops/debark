// Package fake is a complete, offline implementation of cliadapter.Adapter.
//
// It exists so every other packages can be built and demoed before the real
// adapter (invoke.go, events.go, errors.go) has executed a single process:
// the picker, the build screen's progress UI, the export flow and the error
// drawer all talk to this. It runs no subprocess, opens no socket and touches
// no file.
//
// Three things it deliberately gets right, because a fake that is wrong about
// them lets a bug ship:
//
//   - Command returns cliadapter.BuildArgv(spec), the same function the real
//     adapter uses, so a test that pins flag construction pins the real thing.
//   - ListBases returns the binary's real builtin table via base.Builtin,
//     digests and all, rather than a hand-written list that would drift.
//   - Build replays a scripted event stream that contains the shapes a real
//     build produces and a naive UI gets wrong: total_bytes = -1, a retry that
//     rewinds a byte counter, warnings arriving before any progress, and no
//     cumulative counter anywhere.
package fake

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/base"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/verify"
	"github.com/inferops/debark/core/version"

	"github.com/inferops/debark/gui/internal/cliadapter"
)

// Failure builds a *cliadapter.Error for a given argv. Every injection knob
// takes one of these rather than a ready-made error, so the error the UI
// renders carries the argv that actually produced it — which is what the
// details drawer shows.
type Failure func(argv []string) *cliadapter.Error

// Adapter is the fake. The zero value works: it replays the default script
// instantly and succeeds.
//
// # Knobs
//
// Pacing:
//
//	Speed     scales every scripted delay. 0 (default) is instant, which is
//	          what tests want; 1 is real time; 0.1 is a ten-times-faster demo.
//	Script    replaces DefaultScript entirely.
//
// Failure injection (see EnvironmentFailure and friends, and FailureForClass
// for a test that must cover all eight exit classes):
//
//	FailWith  makes Build fail. Every dferr class is reachable.
//	FailAfter is how many scripted events are delivered first. 0 fails
//	          immediately; a negative value replays the whole script and then
//	          fails, which is what an incomplete build looks like.
//	ProbeErr, BasesErr, SnapshotErr, VerifyErr, KeygenErr fail the other five
//	          methods. They take a plain error, so any Failure works:
//	          fake.PolicyFailure()(nil).
//
// Canned answers:
//
//	Result, Bases, ProbeResult, Snapshot, VerifyReport, Key each replace what
//	the corresponding method would otherwise invent.
//
// Recording: Calls returns every method invocation in order, and LastSpec
// returns the last BuildSpec Build was given.
//
// An Adapter is safe for concurrent use.
type Adapter struct {
	// Speed scales the scripted delays. 0 replays instantly.
	Speed float64
	// Script replaces DefaultScript when non-nil.
	Script []Step

	// FailWith makes Build fail with this class after FailAfter events.
	FailWith Failure
	// FailAfter is how many scripted events reach the sink before FailWith
	// fires. Negative means "the whole script, then fail".
	FailAfter int

	// Result replaces the invented BuildResult.
	Result *buildjob.BuildResult
	// Bases replaces the real builtin table returned by ListBases.
	Bases *base.List
	// ProbeResult replaces the invented Probe.
	//
	// Setting one whose Capabilities.JSONEvents is false also makes Build
	// deliver no events, the way a debark too old for --json-events does:
	// the real adapter leaves the flag off and nothing streams. It is the one
	// knob here that changes more than one method, because on a real binary it
	// is one fact and not two.
	ProbeResult *cliadapter.Probe
	// Snapshot replaces the invented snapshot document.
	Snapshot *snapshot.Snapshot
	// VerifyReport replaces the invented verify report.
	VerifyReport *cliadapter.VerifyReport
	// Key replaces the invented KeyInfo.
	Key *cliadapter.KeyInfo

	// Per-method errors. Any error works; the Failure helpers below produce
	// realistic ones.
	ProbeErr    error
	BasesErr    error
	SnapshotErr error
	VerifyErr   error
	KeygenErr   error

	mu       sync.Mutex
	calls    []Call
	lastSpec cliadapter.BuildSpec
}

// Call is one recorded invocation.
type Call struct {
	// Method is "Probe", "ListBases", "InspectSnapshot", "Build", "Keygen",
	// "Verify" or "Command".
	Method string
	// Argv is the command line the real adapter would have run, when the
	// method has one.
	Argv []string
}

// New returns a fake with realistic demo pacing (a tenth of real time, so a
// scripted build finishes in about a second and a half). Tests should use the
// zero value instead, which is instant.
func New() *Adapter { return &Adapter{Speed: 0.1} }

var _ cliadapter.Adapter = (*Adapter)(nil)

func (a *Adapter) record(method string, argv []string) {
	a.mu.Lock()
	a.calls = append(a.calls, Call{Method: method, Argv: argv})
	a.mu.Unlock()
}

// Calls returns every recorded invocation, in order.
func (a *Adapter) Calls() []Call {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Call(nil), a.calls...)
}

// LastSpec returns the BuildSpec the most recent Build (or Command) received.
func (a *Adapter) LastSpec() cliadapter.BuildSpec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastSpec
}

// Reset clears the recorded calls. The knobs are left alone.
func (a *Adapter) Reset() {
	a.mu.Lock()
	a.calls = nil
	a.lastSpec = cliadapter.BuildSpec{}
	a.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Adapter implementation
// ---------------------------------------------------------------------------

// Probe reports a plausible community build of debark.
func (a *Adapter) Probe(ctx context.Context) (cliadapter.Probe, error) {
	argv := []string{cliadapter.ProgramName, "version", "--json"}
	a.record("Probe", argv)
	if err := ctxErr(ctx, argv); err != nil {
		return cliadapter.Probe{}, err
	}
	if a.ProbeErr != nil {
		return cliadapter.Probe{}, a.ProbeErr
	}
	if a.ProbeResult != nil {
		return *a.ProbeResult, nil
	}
	return cliadapter.Probe{
		Path: "/usr/bin/debark",
		Info: version.Info{
			Name:        version.Name,
			Version:     "1.2.0",
			Commit:      "94612f558cd26fa57d6c0185956ff153c5162833",
			Date:        "2026-09-01T09:12:44Z",
			Edition:     manifest.EditionCommunity,
			GoVersion:   "go1.26.0",
			Platform:    "linux/amd64",
			BuildDigest: "54cfa54b026b0b7323f789fa71d03950e15822b38a0e30defd807c08cc6592d3",
		},
		HostArch: "amd64",
		Capabilities: cliadapter.Capabilities{
			Bases: true, JSONEvents: true, Keygen: true, SBOM: true,
		},
	}, nil
}

// ListBases returns debark's own builtin base table for arch.
//
// It calls base.Builtin and base.NewList rather than carrying a hand-written
// list, so the fake shows exactly the fifteen releases the real
// `snapshot list-bases --json` shows, with the same ids, seeds and digests. A
// hand-written list would be wrong the first time a release is added, and
// wrong silently.
func (a *Adapter) ListBases(ctx context.Context, arch string) (*base.List, error) {
	if arch == "" {
		arch = base.HostArch()
	}
	argv := []string{cliadapter.ProgramName, "snapshot", "list-bases", "--arch", arch, "--json"}
	a.record("ListBases", argv)
	if err := ctxErr(ctx, argv); err != nil {
		return nil, err
	}
	if a.BasesErr != nil {
		return nil, a.BasesErr
	}
	if a.Bases != nil {
		return a.Bases, nil
	}
	defs, err := base.Builtin(arch)
	if err != nil {
		return nil, cliadapter.NewError(argv, int(dferr.Usage), "debark: "+err.Error())
	}
	list, err := base.NewList(arch, defs)
	if err != nil {
		return nil, cliadapter.NewError(argv, int(dferr.Usage), "debark: "+err.Error())
	}
	return &list, nil
}

// InspectSnapshot invents a plausible captured snapshot of an Ubuntu 24.04
// machine, unless Snapshot is set.
func (a *Adapter) InspectSnapshot(ctx context.Context, path string) (cliadapter.SnapshotInfo, error) {
	argv := []string{cliadapter.ProgramName, "snapshot", "inspect", path, "--json"}
	a.record("InspectSnapshot", argv)
	if err := ctxErr(ctx, argv); err != nil {
		return cliadapter.SnapshotInfo{}, err
	}
	if a.SnapshotErr != nil {
		return cliadapter.SnapshotInfo{}, a.SnapshotErr
	}
	if a.Snapshot != nil {
		return cliadapter.SnapshotInfo{Path: path, Snapshot: *a.Snapshot}, nil
	}
	return cliadapter.SnapshotInfo{Path: path, Snapshot: DemoSnapshot()}, nil
}

// DemoSnapshot is the captured snapshot InspectSnapshot invents: a real-
// looking Ubuntu 24.04 desktop with 1,847 packages installed.
func DemoSnapshot() snapshot.Snapshot {
	return snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		CreatedAt:     "2026-09-04T08:31:07Z",
		Tool: snapshot.Tool{
			Name: version.Name, Version: "1.2.0", Edition: manifest.EditionCommunity,
		},
		Target: snapshot.Target{
			DistroID: "ubuntu", VersionID: "24.04", Codename: "noble",
			PrettyName: "Ubuntu 24.04.1 LTS", Arch: "amd64",
			APTVersion: "2.7.14build2", DpkgVersion: "1.22.6ubuntu6.1",
			MachineID: "6d2c1f9a4b7e4c0d8f3a2b5c7e9d1f04",
		},
		DpkgStatus: snapshot.File{
			Path: "/var/lib/dpkg/status", ArchivePath: "var/lib/dpkg/status",
			Size: 4_182_336, Mode: "0644",
			SHA256: "b7d3e1f5a9c2048d6e0f8a1b3c5d7e9f1a3b5c7d9e1f3a5b7c9d1e3f5a7b9c1d",
		},
		Origin:         snapshot.Origin{Kind: snapshot.OriginCaptured},
		InstalledCount: 1847,
	}
}

// Build replays the scripted event stream and returns a plausible result.
//
// Cancellation is honoured between every step, so a ctx cancelled mid-stream
// stops promptly and returns a cancelled *cliadapter.Error — the one error
// the UI must not render as a failure.
func (a *Adapter) Build(ctx context.Context, spec cliadapter.BuildSpec, sink cliadapter.EventSink) (*buildjob.BuildResult, error) {
	argv := cliadapter.BuildArgv(spec)
	a.record("Build", argv)
	a.mu.Lock()
	a.lastSpec = spec
	a.mu.Unlock()

	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := ctxErr(ctx, argv); err != nil {
		return nil, err
	}

	script := a.Script
	if script == nil {
		script = DefaultScript(spec)
	}

	// How many events reach the sink before FailWith fires.
	stop := len(script)
	if a.FailWith != nil {
		if a.FailAfter < 0 {
			stop = len(script)
		} else if a.FailAfter < stop {
			stop = a.FailAfter
		}
	}

	// A binary with no --json-events streams nothing, and the real adapter
	// leaves the flag off rather than dying on cobra's usage block. The fake
	// reproduces the CONSEQUENCE — a build that runs to completion and calls
	// the sink not once — so a screen can be driven down the indeterminate
	// path without a decade-old debark to hand. The pacing is kept, because
	// a build that finishes instantly is not the case being exercised.
	streams := a.ProbeResult == nil || a.ProbeResult.Capabilities.JSONEvents

	for i := 0; i < stop; i++ {
		if err := a.wait(ctx, script[i].Delay, argv); err != nil {
			return nil, err
		}
		if sink != nil && streams {
			sink(script[i].Event)
		}
	}

	if a.FailWith != nil {
		err := a.FailWith(argv)
		// `build` is the one command that prints its --json result and THEN
		// exits with the mapped class. An incomplete build (exit 3) therefore
		// hands back a fully populated result alongside the error, and the UI
		// has to read both. Reproduce that faithfully: it is the path most
		// likely to be got wrong.
		if err.HasClass(dferr.Incomplete) {
			return a.incompleteResult(spec), err
		}
		return nil, err
	}
	if a.Result != nil {
		return a.Result, nil
	}
	return a.successResult(spec), nil
}

// wait sleeps for d scaled by Speed, returning a cancelled error if ctx ends
// first. With Speed 0 it does not sleep at all but still checks ctx, so even
// an instant replay can be cancelled.
func (a *Adapter) wait(ctx context.Context, d time.Duration, argv []string) error {
	scaled := time.Duration(float64(d) * a.Speed)
	if a.Speed <= 0 || scaled <= 0 {
		return ctxErr(ctx, argv)
	}
	t := time.NewTimer(scaled)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return cliadapter.NewCanceledError(argv)
	case <-t.C:
		return nil
	}
}

func ctxErr(ctx context.Context, argv []string) error {
	select {
	case <-ctx.Done():
		return cliadapter.NewCanceledError(argv)
	default:
		return nil
	}
}

func (a *Adapter) successResult(spec cliadapter.BuildSpec) *buildjob.BuildResult {
	out := spec.OutPath
	if out == "" {
		out = "bundle"
	}
	count := len(archiveFiles) + len(spec.URLs) + len(spec.LocalDebs)
	var warnings []string
	for _, u := range spec.URLs {
		if u.SHA256 == "" {
			warnings = append(warnings,
				u.URL+" was downloaded over HTTPS with no publisher signature and no expected digest")
		}
	}
	return &buildjob.BuildResult{
		SchemaVersion: buildjob.SchemaVersion,
		LockRef:       "lock.json",
		ManifestRef:   "manifest.json",
		BundlePath:    out,
		BundleID:      "2c9f0d18-6b4a-4f31-9c7e-5a0b3d8e1f26",
		Signed:        !spec.NoSign,
		Stats: buildjob.Stats{
			Added:           count,
			Removed:         0,
			Unchanged:       0,
			Bytes:           totalScriptBytes(spec),
			DownloadedBytes: totalScriptBytes(spec) - 157_264,
			PackageCount:    count,
			DurationSeconds: 47,
		},
		Warnings:  warnings,
		ExitClass: buildjob.ExitSuccess,
	}
}

// incompleteResult is what exit 3 looks like: a bundle really was written,
// and Unresolved and FetchFailed are the only place the detail exists.
func (a *Adapter) incompleteResult(spec cliadapter.BuildSpec) *buildjob.BuildResult {
	r := a.successResult(spec)
	r.ExitClass = buildjob.ExitIncomplete
	r.Unresolved = []string{"acme-agent (depends on libssl1.1, which noble does not have)"}
	r.FetchFailed = []string{"https://downloads.example.com/acme-agent_4.2.0_amd64.deb"}
	r.Warnings = append(r.Warnings,
		"the bundle is incomplete: 1 input could not be resolved and 1 download failed")
	return r
}

// Keygen invents a key pair, deriving the public path the same way core/sign
// does rather than by string-appending .pub.
func (a *Adapter) Keygen(ctx context.Context, outPath string) (cliadapter.KeyInfo, error) {
	argv := []string{cliadapter.ProgramName, "keygen", "--out", outPath, "--json"}
	a.record("Keygen", argv)
	if err := ctxErr(ctx, argv); err != nil {
		return cliadapter.KeyInfo{}, err
	}
	if a.KeygenErr != nil {
		return cliadapter.KeyInfo{}, a.KeygenErr
	}
	if a.Key != nil {
		return *a.Key, nil
	}
	if outPath == "" {
		// keygen marks --out required, and cobra refuses before RunE runs.
		return cliadapter.KeyInfo{}, cliadapter.NewError(argv, int(dferr.Usage),
			"debark: required flag(s) \"out\" not set\n\nUsage:\n  debark keygen [flags]\n")
	}
	pub := outPath + sign.PublicKeyFileSuffix
	if strings.HasSuffix(outPath, sign.PrivateKeyFileSuffix) {
		pub = strings.TrimSuffix(outPath, sign.PrivateKeyFileSuffix) + sign.PublicKeyFileSuffix
	}
	return cliadapter.KeyInfo{
		KeyID:          "a1b2c3d4e5f60718",
		PrivateKeyPath: outPath,
		PublicKeyPath:  pub,
	}, nil
}

// Verify reports a bundle that passes, unless VerifyReport or VerifyErr says
// otherwise.
func (a *Adapter) Verify(ctx context.Context, bundlePath string) (cliadapter.VerifyReport, error) {
	argv := []string{cliadapter.ProgramName, "verify", bundlePath, "--json"}
	a.record("Verify", argv)
	if err := ctxErr(ctx, argv); err != nil {
		return cliadapter.VerifyReport{}, err
	}
	if a.VerifyErr != nil {
		rep := cliadapter.VerifyReport{}
		if a.VerifyReport != nil {
			rep = *a.VerifyReport
		}
		return rep, a.VerifyErr
	}
	if a.VerifyReport != nil {
		return *a.VerifyReport, nil
	}
	return cliadapter.VerifyReport{
		SchemaVersion: verify.SchemaVersion,
		CheckedAt:     "2026-09-06T10:16:22Z",
		BundlePath:    bundlePath,
		BundleID:      "2c9f0d18-6b4a-4f31-9c7e-5a0b3d8e1f26",
		OK:            true,
		Signed:        true,
		Signatures: []verify.SignatureResult{{
			SignerKind: "ed25519", KeyID: "a1b2c3d4e5f60718",
			Algorithm: "ed25519", Valid: true, Trusted: true,
		}},
		FilesChecked: len(archiveFiles) + 5,
		BytesChecked: 4_112_308,
		Target: verify.ReportTarget{
			DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64",
		},
		CreatedAt:   "2026-09-06T10:15:52Z",
		ToolVersion: "1.2.0",
		Edition:     manifest.EditionCommunity,
	}, nil
}

// Command returns the argv the real adapter would run. It is
// cliadapter.BuildArgv and nothing else, so a test that pins this pins the
// real flag construction.
func (a *Adapter) Command(spec cliadapter.BuildSpec) []string {
	argv := cliadapter.BuildArgv(spec)
	a.record("Command", argv)
	a.mu.Lock()
	a.lastSpec = spec
	a.mu.Unlock()
	return argv
}

// ---------------------------------------------------------------------------
// Failure injection
// ---------------------------------------------------------------------------

// ExitFailure is the general knob: any exit code with any stderr. The eight
// helpers below are this with realistic text, and are what a test or a demo
// should reach for first.
func ExitFailure(exitCode int, stderr string) Failure {
	return func(argv []string) *cliadapter.Error {
		return cliadapter.NewError(argv, exitCode, stderr)
	}
}

// UsageFailure is exit 1: bad flags, contradictory options, an unreadable
// input path. Its stderr carries cobra's usage block, which is why the
// details drawer exists.
func UsageFailure(msg string) Failure {
	if msg == "" {
		msg = "build: --arch describes a --base; it has no meaning with --snapshot, which records the target's own architecture"
	}
	return ExitFailure(1, "debark: "+msg+"\n\nUsage:\n  debark build [pkg...] [flags]\n\nFlags:\n      --arch string   dpkg architecture the --base describes (default: this machine's)\n      --base string   stock release to assume instead of a snapshot\n")
}

// EnvironmentFailure is exit 2: no apt, no container runtime, no disk, no
// permission. The most common first-run failure by a wide margin.
func EnvironmentFailure() Failure {
	return ExitFailure(2, "debark: build: no container runtime found and the local backend cannot resolve for a different release\n"+
		"install docker or podman, or run debark on a machine of the same release as the target and pass --backend=local\n")
}

// IncompleteFailure is exit 3: a bundle WAS written, but a URL failed or an
// external .deb has unsatisfiable dependencies.
//
// Its stderr is empty on purpose. `build` reports this through its --json
// result and a silent error, so there is no "debark: …" line at all — which
// is exactly why cliadapter.Error falls back to the class description, and
// why Build returns the result alongside the error.
func IncompleteFailure() Failure {
	return ExitFailure(3, "")
}

// VerificationFailure is exit 4: signature, digest or repository metadata
// mismatch. On a bundle that came off removable media this is the one an
// operator must escalate rather than retry.
func VerificationFailure() Failure {
	return ExitFailure(4, "debark: verify: pool/n/nginx/nginx_1.24.0-2ubuntu7.3_all.deb: digest mismatch\n"+
		"the bundle does not match its signed manifest; do not install it, and obtain a fresh copy\n")
}

// ResolutionFailure is exit 5: apt could not satisfy the request.
func ResolutionFailure(pkgs ...string) Failure {
	if len(pkgs) == 0 {
		pkgs = []string{"acme-agent"}
	}
	return ExitFailure(5, fmt.Sprintf(
		"debark: apt: unable to satisfy the request: %s has no installation candidate\n"+
			"check the package name against `debark` search results for this target, or add the vendor .deb as a URL\n",
		strings.Join(pkgs, ", ")))
}

// PolicyFailure is exit 6: a local policy or approved-keys violation.
func PolicyFailure() Failure {
	return ExitFailure(6, "debark: policy: require-signed-publisher: 1 input is url-unverified: https://downloads.example.com/acme-agent_4.2.0_amd64.deb\n"+
		"supply the vendor's expected SHA-256 for that download, or relax the rule in the policy file\n")
}

// TargetMismatchFailure is exit 7: an architecture or release mismatch. It
// belongs to install rather than build, and is here so the error renderer can
// be exercised against every class in the frozen table.
func TargetMismatchFailure() Failure {
	return ExitFailure(7, "debark: install: this bundle is for ubuntu 24.04 (noble) amd64; this machine is debian 12 (bookworm) arm64\n"+
		"build a bundle from a snapshot of THIS machine\n")
}

// SuccessFailure is exit 0 with something wrong anyway: debark succeeded
// but its --json output could not be used. Rare, and the one case where a
// zero exit code still has to reach the operator as an error.
func SuccessFailure() Failure {
	return func(argv []string) *cliadapter.Error {
		return cliadapter.NewError(argv, 0, "").
			WithSummary("debark finished but its JSON output could not be read").
			WithHint("run the command shown below in a terminal and report what it prints")
	}
}

// FailureForClass returns the helper for one dferr class, so a test can loop
// over dferr.Classes() and prove every exit code renders an actionable
// message. Every class in the frozen 0–7 table is covered.
func FailureForClass(c dferr.Class) Failure {
	switch c {
	case dferr.Success:
		return SuccessFailure()
	case dferr.Usage:
		return UsageFailure("")
	case dferr.Environment:
		return EnvironmentFailure()
	case dferr.Incomplete:
		return IncompleteFailure()
	case dferr.Verification:
		return VerificationFailure()
	case dferr.Resolution:
		return ResolutionFailure()
	case dferr.Policy:
		return PolicyFailure()
	case dferr.TargetMismatch:
		return TargetMismatchFailure()
	default:
		return ExitFailure(int(c), "")
	}
}

// BinaryMissingFailure is the failure that never reaches a process at all:
// the operator has no debark installed. Its exit code is outside the frozen
// table, which is why cliadapter.NewError classifies it as Environment rather
// than Usage.
func BinaryMissingFailure() Failure {
	return func(argv []string) *cliadapter.Error {
		return cliadapter.NewError(argv, 127, "").
			WithSummary("debark could not be found on this machine").
			WithHint("install the debark package, or set the path to the binary in settings")
	}
}
