package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/distro"
)

// This file is the end-to-end scenario: start a target
// container, bring it to the fixture's installed state, capture a snapshot,
// freeze that exact machine into a per-row image, build the bundle on a
// separate builder container, transfer it to a fresh --network-none target
// started FROM THAT IMAGE, verify, install, and assert the requested binaries
// actually run — then tear every container and image down, always, even on
// failure or a panic.
//
// The "started from that image" step is the one a reader is most likely to
// mistake for an optimisation and remove; commitTargetImage explains, at
// length, what four fixtures were measuring before it existed.
//
// Both test/e2e/e2e_test.go (`go test`) and hack/matrix (the standalone
// runner) call RunFixture; it is the one place this whole pipeline is
// implemented.

var exitCodeByName = func() map[string]int {
	m := make(map[string]int, len(dferr.Classes()))
	for _, c := range dferr.Classes() {
		m[c.String()] = int(c)
	}
	return m
}()

func exitClassName(code int) string {
	for name, c := range exitCodeByName {
		if c == code {
			return name
		}
	}
	return fmt.Sprintf("unknown(%d)", code)
}

// isProductExitCode reports whether code is one of debark's own documented
// exit classes (core/dferr: 0..7). Anything else did not come from debark:
// `docker exec` reports 125/126/127 for its own failures (daemon error,
// command not invocable, command not found), and a process killed by a
// signal reports 128+signo — 137 for SIGKILL, the shape an out-of-memory
// container leaves behind. Treating one of those as the product's answer is
// how a plumbing failure becomes a recorded product verdict.
func isProductExitCode(code int) bool {
	for _, c := range exitCodeByName {
		if c == code {
			return true
		}
	}
	return false
}

// reportStageFailure records a stage that did not exit as expected, and is
// the one place that decides who to blame for it: the container runtime (an
// exit code debark cannot produce), a missing feature (a "not implemented"
// stub), or the product itself.
func (r *fixtureRun) reportStageFailure(stage, detail string, res CmdResult) {
	switch {
	case !isProductExitCode(res.ExitCode):
		// Recorded as a fact on the row, not merely narrated in the
		// blocker: the runner turns it into a "no verdict" status the way
		// it does for TimedOut, so a runtime-chosen exit stops reading as a
		// product signal. The status stays blocked here — this package does
		// not own the extended status set, hack/matrix does — and the flag
		// is what lets it reclassify.
		r.row.RuntimeError = true
		r.setBlocked(stage, fmt.Sprintf(
			"exit %d is not one of debark's own exit classes (0-%d), so the product did not choose it — "+
				"the container runtime or a signal did: %s",
			res.ExitCode, len(exitCodeByName)-1, detail))
	case Blocked(res.Combined()):
		r.setBlocked(stage, detail)
	default:
		r.setFail(stage, detail)
	}
}

// RunOpts configures one RunFixture call.
type RunOpts struct {
	// RunID scopes every container this call creates (see docker.go's
	// LabelRun) so cleanup — including Sweep's backstop — never touches a
	// container this harness did not create itself.
	RunID string
	// WorkDir is the host scratch root. Never inside the debark repo (see
	// test/e2e/README.md): the harness owns test/e2e and hack/matrix as
	// *source*, not as a place to dump run artefacts.
	WorkDir string
	// Release is the target release to run against. Arch comes from
	// Scenario.Target.Arch, not from Release, because one Release image
	// table entry is used for both amd64 and arm64 (distro.Platform maps
	// the pair).
	Release distro.Release
	// Timeout bounds the whole row. A row that would otherwise hang (a
	// container that never responds) fails cleanly instead — task:
	// "every scenario must fail with a clear, specific message ... rather
	// than hanging".
	Timeout time.Duration
	// KeepWorkDirOnFailure leaves the host scratch directory behind (with
	// its full stage transcript) when the row does not pass, for local
	// debugging. hack/matrix and `go test` both default this true; it only
	// ever affects host temp files, never containers.
	KeepWorkDirOnFailure bool
}

// decoyVendorPackage is a fixed, tiny, near-universal package used only to
// give a swapped-signature tamper fixture a second bundle with different
// content, signed by the same trusted key. It is unrelated to any fixture's
// own request, deliberately, so the decoy never collides with it.
const decoyVendorPackage = "hello"

// RunFixture executes one fixture against one release/arch and always
// returns a RowResult — it never panics out to the caller, and it always
// tears down every container it started, even when it recovers a panic.
func RunFixture(ctx context.Context, s Scenario, opts RunOpts) RowResult {
	started := time.Now()
	row := RowResult{
		Fixture:     s.Name,
		Protects:    s.Protects,
		Distro:      opts.Release.DistroID,
		Version:     opts.Release.VersionID,
		Arch:        s.Target.Arch,
		StartedAt:   started,
		ExitClasses: map[string]string{},
		StageMS:     map[string]int64{},
	}

	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Minute
	}
	rctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	run := &fixtureRun{
		s:        s,
		opts:     opts,
		row:      &row,
		log:      &strings.Builder{},
		release:  opts.Release,
		platform: "",
		byRole:   map[string]*Container{},
	}
	if p, ok := distro.Platform(s.Target.Arch); ok {
		run.platform = p
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				row.Status = StatusBlocked
				if row.Stage == "" {
					row.Stage = "panic"
				}
				row.Blocker = fmt.Sprintf("panic in %s: %v", row.Stage, r)
			}
		}()
		run.execute(rctx)
	}()

	// Read here, immediately after execute() returned and BEFORE cleanup:
	// this is the only moment at which "the deadline fired while the row was
	// running" is distinguishable from "the deadline fired during teardown".
	// Cleanup below runs on its own fresh context precisely so containers are
	// removed even after a timeout, which is correct but makes rctx.Err()
	// useless as evidence about the row from then on. See RowResult.TimedOut
	// for why a caller cannot reconstruct this afterwards.
	row.TimedOut = rctx.Err() != nil

	// A row whose deadline expired has not proved anything, so it must never
	// fall through to the "no status set means everything passed" default
	// below. execute() sets a status on every path that can observe a
	// cancellation, so this is a backstop rather than the primary report —
	// but the primary report being wrong here is precisely what
	// hack/matrix/results/README.md is about, and a false pass is worse than
	// a false failure.
	if ctxErr := rctx.Err(); ctxErr != nil && row.Status == "" {
		stage := row.Stage
		if stage == "" {
			stage = "timeout"
		}
		run.setBlocked(stage, fmt.Sprintf(
			"the harness ran out of time after %s (%v) — debark never reached a verdict", opts.Timeout, ctxErr))
	}

	// Teardown always runs, on a fresh context: rctx may already be
	// cancelled (timeout, or the panic path above), and cleanup must still
	// happen (task: "leave no containers or volumes behind, even on
	// failure").
	cctx, ccancel := context.WithTimeout(context.Background(), 90*time.Second)
	run.cleanup(cctx)
	ccancel()

	row.FinishedAt = time.Now()
	row.TotalMS = row.FinishedAt.Sub(row.StartedAt).Milliseconds()

	logPath := filepath.Join(run.fixtureWorkDir(), "transcript.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err == nil {
		if err := os.WriteFile(logPath, []byte(run.log.String()), 0o644); err == nil {
			row.LogPath = logPath
		}
	}
	if row.Status == StatusPass && !opts.KeepWorkDirOnFailure {
		// The transcript lives inside the directory being removed, so the
		// row must stop advertising a path that no longer resolves — a
		// LogPath pointing at nothing is worse than no LogPath at all,
		// because it sends a reader looking for evidence that was deleted.
		if err := os.RemoveAll(run.fixtureWorkDir()); err == nil {
			row.LogPath = ""
		}
	}

	if row.Status == "" {
		// execute() finished without explicitly setting a terminal status,
		// which only happens if every stage and assertion passed.
		row.Status = StatusPass
		row.Stage = "done"
	}
	return row
}

// fixtureRun is RunFixture's working state: everything one row's execute()
// and cleanup() share.
type fixtureRun struct {
	s        Scenario
	opts     RunOpts
	row      *RowResult
	log      *strings.Builder
	release  distro.Release
	platform string

	containers []*Container // torn down in reverse order by cleanup()
	// byRole is the same containers indexed by the role that created them
	// ("state", "builder", "freshtarget"), so an assertion can name the
	// container it means. containers stays the ordered teardown list; this
	// is a lookup and nothing else.
	byRole map[string]*Container
	// images are the image references this row created with `docker commit`
	// (today: exactly one, targetImage below). cleanup() removes them after
	// every container is gone. They are tracked separately from containers
	// because a leaked image is the quieter of the two failures — a stray
	// container shows up in `docker ps`, while a stray image just sits in the
	// daemon's store until someone runs a prune — and this host has had both.
	images []string
	// targetImage is the per-row image the fresh target is started from: a
	// `docker commit` of the state container taken the instant after the
	// snapshot was captured. Empty until commitTargetImage has run, and
	// freshTargetImage() refuses to substitute anything for it.
	targetImage string
	binPath     string      // host path to the built linux binary
	repoSrv     *RepoServer // serves any TargetSpec.Installed.Repos and Request.VendorURLs; closed by cleanup()
	vendorURLs  []string    // http:// inputs for Request.VendorURLs, built once the server's port is known
}

func (r *fixtureRun) fixtureWorkDir() string {
	slug := sanitize(r.s.Name)
	return filepath.Join(r.opts.WorkDir, r.opts.RunID, fmt.Sprintf("%s-%s-%s-%s", slug, r.release.DistroID, r.release.VersionID, r.s.Target.Arch))
}

func (r *fixtureRun) logf(format string, a ...any) {
	fmt.Fprintf(r.log, format+"\n", a...)
}

// setBlocked/setFail/setPass finalise row.Status exactly once; later calls
// (e.g. from cleanup-adjacent code) are no-ops so the first real outcome
// wins.
func (r *fixtureRun) setBlocked(stage, detail string) {
	if r.row.Status != "" {
		return
	}
	r.row.Status = StatusBlocked
	r.row.Stage = stage
	r.row.Blocker = detail
	r.logf("=== BLOCKED at %s: %s", stage, detail)
}

func (r *fixtureRun) setFail(stage, detail string) {
	if r.row.Status != "" {
		return
	}
	r.row.Status = StatusFail
	r.row.Stage = stage
	r.row.Blocker = detail
	r.logf("=== FAIL at %s: %s", stage, detail)
}

func (r *fixtureRun) setSkipped(stage, detail string) {
	if r.row.Status != "" {
		return
	}
	r.row.Status = StatusSkipped
	r.row.Stage = stage
	r.row.Blocker = detail
	r.logf("=== SKIPPED at %s: %s", stage, detail)
}

// reportContainerStartFailure classifies a container that failed to start.
// hack/matrix's own preflight (harness.PlatformSupported) checks emulation
// once per (release, arch) at the start of a run and caches the result for
// every row that shares it; on a host where QEMU/binfmt registration itself
// is flaky under load (observed directly during development on a shared
// Docker host), that cached "available" can go
// stale between the check and this row's actual `docker run`. Recognising
// the same exec-format-error signature docker.go's preflight probe already
// looks for turns a late-detected emulation failure into what it actually
// is — task: "skip cleanly and say why when ... an emulator is
// unavailable" — a skip, not a product-shaped blocked/fail.
func (r *fixtureRun) reportContainerStartFailure(stage string, err error) {
	if execFormatErrRe.MatchString(err.Error()) {
		r.setSkipped(stage, "container failed to start under this architecture's emulation: "+err.Error())
		return
	}
	r.setBlocked(stage, err.Error())
}

func (r *fixtureRun) timeStage(name string, fn func() error) error {
	start := time.Now()
	err := fn()
	r.row.StageMS[name] = time.Since(start).Milliseconds()
	return err
}

func (r *fixtureRun) newContainer(c *Container) *Container {
	r.containers = append(r.containers, c)
	return c
}

func (r *fixtureRun) cleanup(ctx context.Context) {
	for i := len(r.containers) - 1; i >= 0; i-- {
		c := r.containers[i]
		if c == nil {
			continue
		}
		if err := c.Remove(ctx); err != nil {
			r.logf("cleanup: remove %s: %v", c.Name, err)
		}
	}
	// Images strictly AFTER every container, never interleaved: `docker
	// image rm -f` on an image a container still references does not fail,
	// it merely untags it and leaves the layers behind as a dangling <none>
	// image — a leak wearing a different name, and one that no longer
	// carries the run label a later sweep would need to find it.
	for i := len(r.images) - 1; i >= 0; i-- {
		if err := RemoveImage(ctx, r.images[i]); err != nil {
			r.logf("cleanup: remove image %s: %v", r.images[i], err)
		}
	}
	if r.repoSrv != nil {
		if err := r.repoSrv.Close(); err != nil {
			r.logf("cleanup: close synthetic repo server: %v", err)
		}
	}
}

func (r *fixtureRun) containerLabels(role string) map[string]string {
	return map[string]string{
		LabelMarker:          "1",
		LabelRun:             r.opts.RunID,
		"debark.e2e.fixture": sanitize(r.s.Name),
		"debark.e2e.release": r.release.DistroID + "-" + r.release.VersionID,
		"debark.e2e.arch":    r.s.Target.Arch,
		"debark.e2e.role":    role,
	}
}

// containerName must be unique per (fixture, release, arch, role): a
// target.matrix:true fixture runs the same fixture name against the same
// release at more than one architecture concurrently, and a name that
// dropped arch collided between those rows — two different goroutines'
// `docker run --name X` raced for the same container, one lost with a
// docker-level "name already in use" (reported as a confusing "target-start"
// block), and the winner was silently shared by two unrelated fixtureRuns.
// Found and fixed while building the integration matrix.
func (r *fixtureRun) containerName(role string) string {
	base := fmt.Sprintf("dfe2e-%s-%s-%s-%s-%s", r.opts.RunID, sanitize(r.s.Name), r.release.VersionID, r.s.Target.Arch, role)
	if len(base) <= 63 {
		return base
	}
	// Hash-and-suffix rather than a bare truncation, which could itself
	// collide two different fixtures sharing a long common name prefix.
	sum := sha256.Sum256([]byte(base))
	suffix := "-" + hex.EncodeToString(sum[:4])
	keep := 63 - len(suffix)
	if keep < 0 {
		keep = 0
	}
	return base[:keep] + suffix
}

// execute runs the whole pipeline. It sets r.row.Status (via setBlocked/
// setFail) and returns early the moment a stage does not let the pipeline
// continue meaningfully; leaving r.row.Status empty means everything that
// ran matched its expectation.
func (r *fixtureRun) execute(ctx context.Context) {
	work := r.fixtureWorkDir()
	if err := os.MkdirAll(work, 0o755); err != nil {
		r.setBlocked("workdir", err.Error())
		return
	}
	r.logf("=== fixture %s on %s %s (%s), work=%s", r.s.Name, r.release.DistroID, r.release.VersionID, r.s.Target.Arch, work)

	// Stage: build the linux debark binary for this arch (shared across
	// every fixture in the run via BuildLinuxBinary's cache).
	var build BuildResult
	err := r.timeStage("build-binary", func() error {
		var berr error
		build, berr = BuildLinuxBinary(ctx, filepath.Join(r.opts.WorkDir, r.opts.RunID, "bin"), r.s.Target.Arch)
		return berr
	})
	r.logf("--- go build ./cmd/debark (GOARCH=%s) ---\n%s", r.s.Target.Arch, build.Output)
	if err != nil {
		r.setBlocked("build-binary", err.Error())
		return
	}
	r.binPath = build.Path

	// Any synthetic third-party repositories this fixture's target state
	// needs are built once on the host and served over HTTP for the whole
	// run (see httprepo.go / syntheticdeb.go's package doc): a
	// container-local file:// URI would be invisible to the
	// separately-started builder container that resolves the snapshot's
	// recorded sources.
	// The same server also serves Request.VendorURLs, which is why the
	// condition is not just "does this fixture install from a synthetic
	// repo": a fixture may need HTTP for its build inputs and not for its
	// target state.
	if len(r.s.Target.Installed.Repos) > 0 || len(r.s.Request.VendorURLs) > 0 {
		srv, err := StartRepoServer(filepath.Join(work, "repos"))
		if err != nil {
			r.setBlocked("target-setup", "start synthetic repo server: "+err.Error())
			return
		}
		r.repoSrv = srv
	}

	// Vendor .deb inputs that are DOWNLOADED rather than staged locally are
	// written into the served tree here, on the host, before any container
	// needs them. They are ordinary bytes on disk until `debark build`
	// asks for them over HTTP — see RequestSpec.VendorURLs.
	var vendorURLs []string
	for _, spec := range r.s.Request.VendorURLs {
		spec = spec.ResolveArch(r.s.Target.Arch)
		debBytes, berr := BuildSyntheticDebBytes(spec)
		if berr != nil {
			r.setBlocked("target-setup", "build vendor URL .deb "+spec.Name+": "+berr.Error())
			return
		}
		dir := r.repoSrv.VendorDir(filepath.Join(work, "repos"))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			r.setBlocked("target-setup", "create vendor URL dir: "+err.Error())
			return
		}
		if err := os.WriteFile(filepath.Join(dir, spec.debFilename()), debBytes, 0o644); err != nil {
			r.setBlocked("target-setup", "write vendor URL .deb "+spec.Name+": "+err.Error())
			return
		}
		vendorURLs = append(vendorURLs, r.repoSrv.VendorURL(spec.debFilename(), r.s.Request.VendorURLQuery))
	}
	r.vendorURLs = vendorURLs

	// Stage: start the state container and bring it to the fixture's
	// installed state.
	state, err := r.startContainer(ctx, "state", r.release.Ref(), "" /* real network: apt-get needs the archive */)
	if err != nil {
		r.reportContainerStartFailure("target-start", err)
		return
	}
	err = r.timeStage("target-setup", func() error {
		var baseURL string
		if r.repoSrv != nil {
			baseURL = r.repoSrv.BaseURL()
		}
		return ApplyTargetState(ctx, state, r.s.Target, filepath.Join(work, "repos"), baseURL)
	})
	if err != nil {
		r.setBlocked("target-setup", err.Error())
		return
	}

	// Stage: snapshot create.
	if err := state.CopyIn(ctx, r.binPath, "/debark"); err != nil {
		r.setBlocked("target-setup", "copy binary into state container: "+err.Error())
		return
	}
	debState := Debark{C: state, BinPath: "/debark"}
	var snapRes CmdResult
	err = r.timeStage("snapshot-create", func() error {
		var e error
		snapRes, e = debState.SnapshotCreate(ctx, SnapshotCreateArgs{Out: "/work/snapshot.tar.zst"})
		return e
	})
	r.logCmd("snapshot create", snapRes, err)
	if err != nil {
		r.setBlocked("snapshot-create", err.Error())
		return
	}
	if snapRes.ExitCode != 0 {
		detail := fmt.Sprintf("debark snapshot create: exit %d (%s): %s", snapRes.ExitCode, exitClassName(snapRes.ExitCode), truncate(snapRes.Combined(), 800))
		r.reportStageFailure("snapshot-create", detail, snapRes)
		return
	}
	hostSnapshot := filepath.Join(work, "snapshot.tar.zst")
	if err := state.CopyOut(ctx, "/work/snapshot.tar.zst", hostSnapshot); err != nil {
		r.setBlocked("snapshot-create", "copy snapshot out: "+err.Error())
		return
	}

	// Stage: freeze the snapshotted machine into a per-row image, so the
	// fresh target further down is that machine and not a stranger.
	//
	// Taken here, the instant after the snapshot was copied out, rather than
	// later next to the fresh target: nothing may have happened to the state
	// container in between, and the shortest way to guarantee that is to
	// leave no code between the two.
	if err := r.timeStage("target-commit", func() error { return r.commitTargetImage(ctx, state) }); err != nil {
		// Blocked, not failed: the row never got as far as asking debark
		// anything, and an image the harness could not make says nothing
		// whatsoever about the product.
		r.setBlocked("target-commit", err.Error())
		return
	}

	// Stage: builder container, keys, vendor debs, then build.
	builder, err := r.startContainer(ctx, "builder", r.release.Ref(), "")
	if err != nil {
		r.reportContainerStartFailure("build", err)
		return
	}
	if err := builder.CopyIn(ctx, r.binPath, "/debark"); err != nil {
		r.setBlocked("build", "copy binary into builder container: "+err.Error())
		return
	}
	if err := builder.CopyIn(ctx, hostSnapshot, "/work/snapshot.tar.zst"); err != nil {
		r.setBlocked("build", "copy snapshot into builder container: "+err.Error())
		return
	}
	// One pinned build clock for every `debark build` this row runs.
	//
	// Without this the determinism check (checkDeterminism, below) compares
	// two builds made seconds apart and reports four files as "not
	// byte-identical" — debark.manifest.json, debark.manifest.sig,
	// lock.json and repo/Release — every one of them the SAME LENGTH in both
	// builds, and repo/Packages identical. That signature (equal length,
	// differing content, only in the files carrying a stamp) is a timestamp,
	// not a reordering: manifest.CreatedAt, lock.CreatedAt, repo/Release's
	// Date, and the signature's own CreatedAt, which core/engine overwrites
	// from the same build clock (core/engine/finalize.go).
	//
	// That is not a product defect. A bundle recording when it was built is
	// deliberate — manifest.CreatedAt is an input to BundleID — and the
	// reproducible-builds convention for exactly this situation is to fix
	// the clock and require everything else to match. core/engine/clock.go
	// implements SOURCE_DATE_EPOCH for that purpose and is emphatic that it
	// is the ONLY place "now" is decided, so setting it here reaches every
	// stamp in the bundle through one value.
	//
	// The 2026-09-05 matrix run is what made this visible: eight rows (every
	// release/arch of basic-install-matrix that got as far as the check)
	// failed determinism. Before the harness was fixed to give a real
	// verdict, the check passed by racing the clock — two builds landing in
	// the same wall-clock second — which is why a defect this systematic had
	// never been reported.
	//
	// Pinning the clock does NOT make the check vacuous: it removes the one
	// difference that is expected and leaves every difference that is not.
	// Map iteration order, unsorted repository entries, a leaked host path, a
	// cache-dependent statistic and a nondeterministic signature would all
	// still be caught — those are what the byte-identical claim is about.
	//
	// The value is this row's own start time rather than a hardcoded
	// constant so the bundle still carries a realistic date, and it is
	// computed once, here, so the main build, any PriorBuild, the decoy
	// signature build and the determinism rerun all share it.
	debBuilder := Debark{
		C:       builder,
		BinPath: "/debark",
		Env: map[string]string{
			"SOURCE_DATE_EPOCH": strconv.FormatInt(time.Now().Unix(), 10),
		},
	}

	var signKey, operatorPub, wrongPub string
	// wrongKeyErr remembers why the decoy keypair is missing, so the row
	// that actually needs it can say what went wrong instead of quietly
	// verifying with the real key (see the TamperWrongKey guard below).
	var wrongKeyErr string
	if r.s.Request.Sign {
		res, kerr := debBuilder.Keygen(ctx, "/work/keys/operator.key", "debark e2e harness operator key")
		r.logCmd("keygen operator", res, kerr)
		if kerr != nil || res.ExitCode != 0 {
			detail := keygenFailureDetail(res, kerr)
			if kerr != nil || Blocked(res.Combined()) {
				r.setBlocked("build", "keygen: "+detail)
			} else {
				r.setFail("build", "keygen: "+detail)
			}
			return
		}
		signKey = "/work/keys/operator.key"
		operatorPub = "/work/keys/operator.pub"

		res2, kerr2 := debBuilder.Keygen(ctx, "/work/keys/wrong.key", "unrelated key — must never verify this bundle")
		r.logCmd("keygen wrong (decoy)", res2, kerr2)
		switch {
		case kerr2 == nil && res2.ExitCode == 0:
			wrongPub = "/work/keys/wrong.pub"
		default:
			// Only the wrong-key fixture consumes this keypair, so a failure
			// is not fatal to every signed row — but it must not evaporate
			// either, or that one fixture silently changes what it tests.
			wrongKeyErr = keygenFailureDetail(res2, kerr2)
			r.logf("--- decoy keygen failed (only the wrong-key tamper needs it): %s ---", wrongKeyErr)
		}
	}

	// Vendor .deb inputs are built once in Go on the host, then copied
	// straight into the builder container (a local file path
	// is a first-class build input) — never downloaded, matching the task's
	// "simulation over real downloads" constraint, and never needing
	// dpkg-deb since these bytes only ever have to exist on this one
	// container's filesystem (unlike a TargetSpec repo, no other container
	// needs to see a vendor .deb).
	var vendorPaths []string
	for i, spec := range r.s.Request.VendorDebs {
		spec = spec.ResolveArch(r.s.Target.Arch)
		debBytes, berr := BuildSyntheticDebBytes(spec)
		if berr != nil {
			r.setBlocked("build", "build vendor .deb "+spec.Name+": "+berr.Error())
			return
		}
		out := fmt.Sprintf("/work/input/local-debs/vendor-%d-%s", i, spec.debFilename())
		if err := builder.WriteFile(ctx, out, debBytes); err != nil {
			r.setBlocked("build", "copy vendor .deb "+spec.Name+" into builder: "+err.Error())
			return
		}
		vendorPaths = append(vendorPaths, out)
	}

	buildArgs := BuildArgs{
		Snapshot:   "/work/snapshot.tar.zst",
		Packages:   append(append(append(append([]string{}, r.s.Request.Packages...), r.s.Request.URLs...), r.vendorURLs...), vendorPaths...),
		Out:        "/work/bundle",
		Backend:    "local", // already inside the correct release's container (resolve-contract.md's own re-entry convention)
		Sign:       signKey,
		NoSign:     signKey == "",
		ExtraFlags: r.s.Request.BuildFlags,
	}
	// A fixture whose claim is about a bundle directory that already has a
	// history builds that history first, into the same --out directory. A
	// run is additive (design 3.7) and prune only ever removes a version
	// that some higher version of the same (name, arch) supersedes, so
	// whatever this earlier request pulled into repo/pool is still sitting
	// there when the real build runs. No single build can produce that
	// state, which is why this stage exists at all — see
	// fixtures/pool-unreferenced.json.
	if pb := r.s.Request.PriorBuild; pb != nil {
		priorArgs := buildArgs
		priorArgs.Packages = append([]string{}, pb.Packages...)
		priorArgs.ExtraFlags = append([]string{}, pb.BuildFlags...)
		var priorRes CmdResult
		err = r.timeStage("prior-build", func() error {
			var e error
			priorRes, e = debBuilder.Build(ctx, priorArgs)
			return e
		})
		r.logCmd("build (prior request)", priorRes, err)
		if err != nil {
			r.setBlocked("prior-build", err.Error())
			return
		}
		if priorRes.ExitCode != 0 {
			// Reported against the prior-build stage, not folded into
			// "build": the fixture's premise never came true, so the real
			// build is about to run against a directory in a state nobody
			// described, and every assertion downstream of it would be
			// measuring something else.
			r.reportStageFailure("prior-build", fmt.Sprintf(
				"the earlier build this fixture's premise depends on exited %d (%s), so the bundle directory the real build runs against is not the one the fixture describes: %s",
				priorRes.ExitCode, exitClassName(priorRes.ExitCode), truncate(priorRes.Combined(), 800)), priorRes)
			return
		}
	}

	var buildRes CmdResult
	err = r.timeStage("build", func() error {
		var e error
		buildRes, e = debBuilder.Build(ctx, buildArgs)
		return e
	})
	r.logCmd("build", buildRes, err)
	if err != nil {
		r.setBlocked("build", err.Error())
		return
	}
	r.row.ExitClasses["build"] = exitClassName(buildRes.ExitCode)

	// Checked BEFORE the exit-class comparison, deliberately. A
	// container-path check answers "did something the snapshot carried
	// execute on this builder", and that question has to be answered
	// whatever exit code the build settled on: a build that failed for an
	// unrelated reason must not be allowed to bury the fact that a captured
	// apt.conf hook ran as root on the one host in the pipeline that holds a
	// signing key. Running it here also means it is evaluated for every row
	// that reaches the build at all, including the ones that stop
	// immediately after it — which is why it is absent from
	// blockUncheckedExpectations below.
	if len(r.s.Expect.ContainerPaths) > 0 {
		r.row.Stage = "assert-containers"
		if !r.reportFindings("assert-containers", AssertContainerPaths(ctx, r.byRole, r.s.Expect.ContainerPaths)) {
			return
		}
	}

	if !r.stageMatches("build", buildRes, r.s.Expect.Build) {
		return
	}
	// The incomplete case still produces a bundle worth inspecting (engine
	// iface.go: "It returns a BuildResult even for the incomplete case").
	bundleExists := buildRes.ExitCode == 0 || buildRes.ExitCode == int(dferr.Incomplete)

	if len(r.s.Expect.UnresolvedContains) > 0 {
		for _, f := range AssertUnresolvedContains(buildRes.Combined(), r.s.Expect.UnresolvedContains) {
			if !f.OK {
				r.setFail("build", f.Detail)
				return
			}
		}
	}

	if !bundleExists || stageSkipped(r.s.Expect.Verify) {
		// fixture only cares about the build outcome (e.g. external-deb-missing-deps)
		r.blockUncheckedExpectations("verify", "this row stops after the build stage")
		return
	}

	// Stage: copy the bundle out, apply any tamper, prepare a swapped-
	// signature decoy if this fixture needs one.
	hostBundle := filepath.Join(work, "bundle")
	err = r.timeStage("bundle-transfer", func() error { return builder.CopyOut(ctx, "/work/bundle", hostBundle) })
	if err != nil {
		r.setBlocked("bundle-transfer", err.Error())
		return
	}
	if !r.recordBundleStats(hostBundle) {
		return
	}

	// Read from the host copy, before any tamper touches it: these checks
	// are about what the BUILD wrote, so they must see the bundle exactly as
	// it came off the builder.
	if len(r.s.Expect.BundleFiles) > 0 {
		r.row.Stage = "assert-bundle"
		if !r.reportFindings("assert-bundle", AssertBundleFiles(hostBundle, r.s.Expect.BundleFiles)) {
			return
		}
	}

	if r.s.Determinism {
		if !r.checkDeterminism(ctx, debBuilder, buildArgs, work) {
			return
		}
	}

	var tamperExtra TamperExtra
	if r.s.Tamper != nil && r.s.Tamper.Kind == TamperSwappedSignature {
		sig, derr := r.buildDecoySignature(ctx, debBuilder, signKey)
		if derr != nil {
			r.setBlocked("tamper", "prepare swapped-signature decoy: "+derr.Error())
			return
		}
		tamperExtra.DecoySigBytes = sig
	}
	if r.s.Tamper != nil && !r.s.Tamper.Kind.AppliedInTarget() {
		if err := r.timeStage("tamper", func() error { return ApplyTamper(*r.s.Tamper, hostBundle, tamperExtra) }); err != nil {
			r.setBlocked("tamper", err.Error())
			return
		}
		r.logf("--- applied tamper: %s ---", r.s.Tamper.Kind)
	}

	// Stage: fresh, offline target — a brand-new container of the machine
	// the snapshot captured (commitTargetImage), NEVER the stock release
	// image, and never the container the snapshot or the build ran on.
	targetImage, err := r.freshTargetImage()
	if err != nil {
		r.setBlocked("verify", err.Error())
		return
	}
	fresh, err := r.startContainer(ctx, "freshtarget", targetImage, "none")
	if err != nil {
		r.reportContainerStartFailure("verify", err)
		return
	}
	if err := fresh.CopyIn(ctx, r.binPath, "/debark"); err != nil {
		r.setBlocked("verify", "copy binary into fresh target: "+err.Error())
		return
	}
	if err := fresh.CopyIn(ctx, hostBundle, "/work/bundle"); err != nil {
		r.setBlocked("verify", "copy bundle into fresh target: "+err.Error())
		return
	}
	// The one tamper that cannot be applied on the host: see
	// ApplyTamperInTarget. It runs here, on the bundle the target will
	// actually verify, before anything has read it.
	if r.s.Tamper != nil && r.s.Tamper.Kind.AppliedInTarget() {
		if err := r.timeStage("tamper", func() error {
			return ApplyTamperInTarget(ctx, fresh, r.s.Tamper.Kind, "/work/bundle")
		}); err != nil {
			r.setBlocked("tamper", err.Error())
			return
		}
		r.logf("--- applied tamper in the fresh target: %s ---", r.s.Tamper.Kind)
	}
	debFresh := Debark{C: fresh, BinPath: "/debark"}
	// Every one of these four transfers is checked, and each failure blocks
	// the row. An unchecked copy here does not stay quiet: verifyKey below
	// still names /work/keys/operator.pub, `debark verify --key` then runs
	// against a key file that is not there, and the row records a
	// verification failure as though the product had rejected a bundle it
	// signed itself. That is the harness manufacturing evidence against the
	// thing it exists to measure.
	if operatorPub != "" {
		if err := builder.CopyOut(ctx, operatorPub, filepath.Join(work, "operator.pub")); err != nil {
			r.setBlocked("verify", "copy operator key out of builder: "+err.Error())
			return
		}
		if err := fresh.CopyIn(ctx, filepath.Join(work, "operator.pub"), "/work/keys/operator.pub"); err != nil {
			r.setBlocked("verify", "copy operator key into fresh target: "+err.Error())
			return
		}
	}
	if wrongPub != "" {
		if err := builder.CopyOut(ctx, wrongPub, filepath.Join(work, "wrong.pub")); err != nil {
			r.setBlocked("verify", "copy decoy key out of builder: "+err.Error())
			return
		}
		if err := fresh.CopyIn(ctx, filepath.Join(work, "wrong.pub"), "/work/keys/wrong.pub"); err != nil {
			r.setBlocked("verify", "copy decoy key into fresh target: "+err.Error())
			return
		}
	}

	verifyKey := ""
	if operatorPub != "" {
		verifyKey = "/work/keys/operator.pub"
	}
	if r.s.Tamper != nil && r.s.Tamper.Kind == TamperWrongKey {
		// The whole claim of the wrong-key fixture is that verify refuses a
		// perfectly valid signature when handed a key that did not make it
		// (tamper-wrong-key.json). Falling back to the *correct* key would
		// make verify succeed and the row would be recorded as a product
		// failure — "exit 0 (success), want 4 (verification)" — for a
		// mistake the harness made. Whatever went wrong upstream (keygen,
		// either copy, an unsigned fixture), the honest answer is that this
		// row could not be run.
		if wrongPub == "" {
			r.setBlocked("verify", "wrong-key tamper needs a decoy public key on the target and the harness has none"+
				decoyKeyReason(r.s.Request.Sign, wrongKeyErr))
			return
		}
		verifyKey = "/work/keys/wrong.pub"
	}
	verifyArgs := VerifyArgs{Bundle: "/work/bundle", JSON: true, ExtraFlags: r.s.Request.VerifyFlags}
	if verifyKey != "" {
		verifyArgs.Keys = []string{verifyKey}
	} else {
		verifyArgs.AllowUnsigned = true
	}

	var verifyRes CmdResult
	err = r.timeStage("verify", func() error {
		var e error
		verifyRes, e = debFresh.Verify(ctx, verifyArgs)
		return e
	})
	r.logCmd("verify", verifyRes, err)
	if err != nil {
		r.setBlocked("verify", err.Error())
		return
	}
	r.row.ExitClasses["verify"] = exitClassName(verifyRes.ExitCode)
	verifyExpect := StageExpect{ExitClass: "success"}
	if r.s.Expect.Verify != nil {
		verifyExpect = *r.s.Expect.Verify
	}
	if !r.stageMatches("verify", verifyRes, verifyExpect) {
		return
	}
	if verifyRes.ExitCode != 0 || stageSkipped(r.s.Expect.Install) {
		// a tamper fixture's job is done once verify refused it before apt ever ran
		r.blockUncheckedExpectations("install", "this row stops after the verify stage")
		return
	}

	// Stage: install.
	installArgs := InstallArgs{Bundle: "/work/bundle", Yes: true, JSON: true, ExtraFlags: r.s.Request.InstallFlags}
	if verifyKey != "" {
		installArgs.Keys = []string{verifyKey}
	} else {
		installArgs.AllowUnsigned = true
	}

	var installRes CmdResult
	err = r.timeStage("install", func() error {
		var e error
		installRes, e = debFresh.Install(ctx, installArgs)
		return e
	})
	r.logCmd("install", installRes, err)
	if err != nil {
		r.setBlocked("install", err.Error())
		return
	}
	r.row.ExitClasses["install"] = exitClassName(installRes.ExitCode)
	installExpect := StageExpect{ExitClass: "success"}
	if r.s.Expect.Install != nil {
		installExpect = *r.s.Expect.Install
	}
	if !r.stageMatches("install", installRes, installExpect) {
		return
	}
	if installRes.ExitCode != 0 {
		// The install exited as the fixture expected, but non-zero: nothing
		// was installed, so nothing downstream can be asserted.
		r.blockUncheckedExpectations("install", "install exited non-zero as expected, so nothing was installed to assert on")
		return
	}

	// Stage: assertions. dpkg's opinion, then — the one that matters — did
	// the binary actually run.
	var findings []Finding
	findings = append(findings, AssertPackagesPresent(ctx, fresh, r.s.Expect.PackagesPresent)...)
	findings = append(findings, AssertPackagesAbsent(ctx, fresh, r.s.Expect.PackagesAbsent)...)
	findings = append(findings, AssertBinaries(ctx, fresh, r.s.Expect.Binaries)...)
	findings = append(findings, AssertELF(ctx, fresh, work, r.s.Expect.ElfChecks)...)
	if len(r.s.Expect.DoctorContains) > 0 {
		dres, derr := debFresh.Doctor(ctx, DoctorArgs{Bundle: "/work/bundle", JSON: true})
		r.logCmd("doctor", dres, derr)
		if derr != nil {
			r.setBlocked("assert", "doctor: "+derr.Error())
			return
		}
		if Blocked(dres.Combined()) {
			r.setBlocked("assert", "doctor: "+truncate(dres.Combined(), 500))
			return
		}
		findings = append(findings, AssertDoctorContains(dres.Combined(), r.s.Expect.DoctorContains)...)
	}
	r.row.Stage = "assert"
	if !r.reportAssertFindings(findings) {
		return
	}
	r.row.Stage = "done"
}

// reportAssertFindings turns the assertion stage's findings into a row
// status and reports whether the pipeline should continue.
//
// Order is the whole point. Findings the harness could not actually perform
// (docker exec would not run, a docker cp failed) are checked first and
// reported as blocked: "the binary did not run because we could not reach
// the container" is not evidence that the installed package is broken, and
// recording it as a failure would be an accusation this run cannot support.
// Genuine mismatches are only reported once it is established that every
// check was actually carried out.
func (r *fixtureRun) reportAssertFindings(findings []Finding) bool {
	return r.reportFindings("assert", findings)
}

// reportFindings is reportAssertFindings for a named stage, so the earlier
// assertion points (assert-containers after the build, assert-bundle after
// the copy-out) report against the stage they actually ran in rather than
// against a stage the row has not reached yet. The ordering rule above is
// the whole of the logic and is stated once, here.
func (r *fixtureRun) reportFindings(stage string, findings []Finding) bool {
	if blockers := PlumbingSummary(findings); blockers != "" {
		r.setBlocked(stage, "assertion could not be carried out: "+blockers)
		return false
	}
	if !AllOK(findings) {
		r.setFail(stage, FailureSummary(findings))
		return false
	}
	return true
}

// startContainer starts one of this row's containers from image.
//
// image is a parameter rather than always r.release.Ref() because the fresh
// target is NOT started from the release image any more — it is started from
// this row's own commit of the state container (commitTargetImage). Every
// call site therefore has to say which machine it means, which is the point:
// a default here would be a default answer to the one question this row's
// correctness turns on.
func (r *fixtureRun) startContainer(ctx context.Context, role, image, network string) (*Container, error) {
	name := r.containerName(role)
	c, err := StartContainer(ctx, ContainerOpts{
		Image:    image,
		Platform: r.platform,
		Network:  network,
		Name:     name,
		Labels:   r.containerLabels(role),
	})
	if err != nil {
		return nil, err
	}
	r.newContainer(c)
	if r.byRole != nil {
		r.byRole[role] = c
	}
	// docker cp refuses to create a missing destination directory (only the
	// final path component of a *new* target may be implicit) — every
	// container in this pipeline gets files copied under /work at some
	// point, so the directory has to exist before the first such copy
	// rather than relying on each call site to remember.
	if _, err := c.MustSucceed(ctx, ExecOpts{}, "mkdir", "-p", "/work/keys"); err != nil {
		return c, fmt.Errorf("prepare /work in %s: %w", name, err)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// The fresh target is the snapshotted machine
// ---------------------------------------------------------------------------

// preCommitScript strips the harness's own scaffolding off the state
// container just before it is committed, so the image is the operator's
// machine and not this test rig's working copy of it. Three actions, each
// with its own reason:
//
//	rm -rf /work   the harness's scratch: the snapshot tarball it just
//	               copied out, and the /work/keys directory startContainer
//	               creates. No target machine has these, and the fresh
//	               target makes its own the moment it starts.
//	rm -f /debark  the binary this harness copied in to take the snapshot.
//	               A real target receives debark across the air gap, on
//	               the media; baking one into the image would let a row's
//	               install stage succeed against a binary the pipeline never
//	               actually transferred.
//	apt-get clean  the .deb files ApplyTargetState's own `apt-get install`
//	               left in /var/cache/apt/archives.
//
// The third one is a CORRECTNESS requirement, not housekeeping, and must not
// be dropped as a "the images already do that" simplification. install builds
// a private apt view (core/install/privateroot.go) that overrides
// Dir::Etc::sourcelist, Dir::Etc::sourceparts, Dir::Etc::preferences*,
// Dir::State::lists and the two pkgcache paths — but NOT Dir::Cache::archives.
// apt checks that cache before fetching, so a .deb of the same version sitting
// there is used INSTEAD of the bundle's pool copy, and a bundle missing that
// very file would install cleanly anyway. That is the same shape of green-row
// lie this whole change exists to remove, just one layer down. (Debian's and
// Ubuntu's official images do ship /etc/apt/apt.conf.d/docker-clean, which
// usually empties that cache already — "usually", on an image tag we do not
// control, is not a property to rest an offline-closure claim on.)
//
// Deliberately NOT removed: /var/lib/apt/lists. Those index files are part of
// what the snapshotted machine looks like, and unlike the archive cache they
// cannot make an install succeed for the wrong reason — the private root
// points Dir::State::lists at a temporary directory of its own, so apt on the
// target cannot acquire anything through them.
const preCommitScript = `set -e
rm -rf /work
rm -f /debark
apt-get clean`

// commitTargetImage freezes the state container — the machine whose dpkg
// status, foreign architectures, holds and apt configuration the snapshot
// just captured — into a per-row image, and records it for cleanup.
//
// READ THIS BEFORE "SIMPLIFYING" THE FRESH TARGET BACK TO r.release.Ref().
//
// Until 2026-09-05 the fresh target was started from the STOCK release image.
// A debark bundle is a closed-world install plan for ONE SPECIFIC MACHINE
// : the one the snapshot describes. Installing it on a
// different machine measures nothing any fixture claims, and that day's matrix
// run showed both ways this goes wrong:
//
//   - Correct product, red row. foreign-arch-i386's bundle was right all
//     along. Mounting that run's real bundle into a stock debian:12-slim and
//     installing it exactly as this harness does gives exit 100, with dpkg
//     refusing "package architecture (i386) does not match system (amd64)";
//     running the one command the fixture declares — dpkg --add-architecture
//     i386 — first and then installing the SAME bundle gives exit 0, clean.
//     multiarch-coexist failed the same way: its snapshot has libc6 installed
//     at both amd64 and i386, so the bundle correctly omits libc6:i386, and
//     apt on a stock image then says "zlib1g:i386 : Depends: libc6:i386
//     (>= 2.4) but it is not installable".
//   - Worse: green rows that could not fail. held-package asserts a package
//     stays at 1.0 through an upgrade *because it is held*; on a stock image
//     it had never been installed and never held, so the row was a plain
//     fresh install of 1.0 that would have passed with hold handling entirely
//     absent from the product. stale-installed-version had the identical
//     hole: "2.0 after an upgrade from 1.0" asserted against a machine that
//     had no 1.0 on it. A test that cannot fail is the defect class this tree
//     has been bitten by five times and treats as the most serious thing a
//     suite can contain.
//
// `docker commit` rather than re-running ApplyTargetState on a second
// container, for two reasons that are both load-bearing:
//
//   - Fidelity. The image IS the snapshotted machine, byte for byte, so the
//     target can never drift from the snapshot the bundle was solved against.
//     A second `apt-get install` minutes later resolves against a moving
//     archive and may legitimately land on different versions, at which point
//     a row's failure says something about Debian's mirrors rather than about
//     debark.
//   - Isolation. Reconstructing that state needs the archive, i.e. a network,
//     on the one container in this pipeline that must have none.
//
// What must survive any future edit here: the fresh target is still a
// brand-new container (never the state container, never the builder), it is
// still started with --network none, it has still never seen the builder, and
// the committed image is still registered for removal in cleanup(). And a
// commit that fails must BLOCK the row — falling back to r.release.Ref()
// would silently restore the exact bug this replaced.
//
// One consequence worth knowing before writing a fixture: the state container
// loses /work and /debark at this point, so an Expect.ContainerPaths check
// against the "state" role must be about a path the target state itself
// creates (as apt-conf-hook-dropped's /tmp marker is), never about the
// harness's own scratch.
func (r *fixtureRun) commitTargetImage(ctx context.Context, state *Container) error {
	if _, err := state.ShellMust(ctx, ExecOpts{}, preCommitScript); err != nil {
		return fmt.Errorf("strip the harness's own scaffolding before committing the target image: %w", err)
	}
	ref := r.targetImageRef()
	// Registered BEFORE the commit runs, not after it succeeds: `docker
	// commit` can fail having already written the image (a cancelled context
	// after the layer landed, an error while tagging), and an image this row
	// created but never recorded is one cleanup() will never remove.
	// RemoveImage treats "no such image" as success, so registering a
	// reference that never came to exist costs one no-op docker call;
	// leaking one on a shared Docker host costs a human.
	r.images = append(r.images, ref)
	if err := CommitContainer(ctx, state, ref, r.containerLabels("targetimage")); err != nil {
		return err
	}
	r.targetImage = ref
	r.logf("--- committed the snapshotted machine as %s (the fresh target's image) ---", ref)
	return nil
}

// targetImageRef names this row's committed target image.
//
// Derived from containerName so it inherits that function's uniqueness
// argument in full (run id, fixture, release and arch, hash-suffixed when the
// name would be too long): a target.matrix:true fixture runs the same fixture
// name against the same release at more than one architecture concurrently,
// and a shared image name would have one row's `docker commit` retag the
// other row's target out from under it mid-run — the image-level twin of the
// container-name collision containerName's own comment records.
//
// Lowercased because a Docker repository name may not contain an uppercase
// letter (a container name may), and the fixture slug is the one component
// that comes from a fixture file rather than from this package. The explicit
// ":e2e" tag keeps the reference from silently becoming ":latest", which is
// the tag a human is most likely to have something else sitting on.
func (r *fixtureRun) targetImageRef() string {
	return strings.ToLower(r.containerName("targetimg")) + ":e2e"
}

// freshTargetImage returns the image the fresh target must be started from,
// and refuses to name one at all when this row never committed the state
// container.
//
// It is a guard, not a convenience. The regression it exists to stop is a
// single-token edit that compiles and runs — passing r.release.Ref() to
// startContainer for the freshtarget role — and whose only symptom is that
// four fixtures go back to testing a machine nobody described, two of them
// silently green. There is no safe default: an empty targetImage means the
// row reached the verify stage without ever freezing the snapshotted machine,
// and the honest report for that is a blocked row, not a row quietly run
// against a substitute.
func (r *fixtureRun) freshTargetImage() (string, error) {
	if r.targetImage == "" {
		return "", errors.New(
			"the state container was never committed, so there is no image of the snapshotted machine to install on — " +
				"the fresh target must never fall back to the stock release image (see commitTargetImage)")
	}
	return r.targetImage, nil
}

// stageMatches records the observed exit class and, on a mismatch, sets the
// row's terminal status (blocked when the output looks like an unimplemented
// stub, fail otherwise). It returns whether the pipeline should continue.
func (r *fixtureRun) stageMatches(stage string, res CmdResult, expect StageExpect) bool {
	want, ok := exitCodeByName[expect.ExitClass]
	if !ok {
		r.setBlocked(stage, fmt.Sprintf("fixture error: unknown expected exit_class %q", expect.ExitClass))
		return false
	}
	if res.ExitCode == want {
		return true
	}
	detail := fmt.Sprintf("exit %d (%s), want %d (%s): %s",
		res.ExitCode, exitClassName(res.ExitCode), want, expect.ExitClass, truncate(res.Combined(), 800))
	r.reportStageFailure(stage, detail, res)
	return false
}

func stageSkipped(e *StageExpect) bool { return e != nil && e.ExitClass == "skip" }

// blockUncheckedExpectations refuses to record a pass for a row that ended
// before it could evaluate expectations the fixture actually declared.
//
// Every early return in execute() leaves row.Status empty, and an empty
// status means pass. That is correct when the fixture's claims all lie
// upstream of where the row stopped (a tamper fixture is finished the moment
// verify refuses the bundle) and a silent lie when they do not: a fixture
// listing packages_present alongside an install it never runs would be
// reported as a green row that asserted nothing. No fixture in
// test/e2e/fixtures does that today — this exists so that none ever can.
func (r *fixtureRun) blockUncheckedExpectations(stage, why string) {
	var names []string
	if len(r.s.Expect.PackagesPresent) > 0 {
		names = append(names, "packages_present")
	}
	if len(r.s.Expect.PackagesAbsent) > 0 {
		names = append(names, "packages_absent")
	}
	if len(r.s.Expect.Binaries) > 0 {
		names = append(names, "binaries")
	}
	if len(r.s.Expect.ElfChecks) > 0 {
		names = append(names, "elf_checks")
	}
	if len(r.s.Expect.DoctorContains) > 0 {
		names = append(names, "doctor_contains")
	}
	// bundle_files is evaluated after the bundle is copied out, which is
	// downstream of the "stops after the build stage" return — so a fixture
	// that declares it and never gets a bundle has to be blocked like any
	// other unevaluated expectation. container_paths is deliberately NOT
	// here: it runs immediately after the build, upstream of both callers.
	if len(r.s.Expect.BundleFiles) > 0 {
		names = append(names, "bundle_files")
	}
	if len(names) == 0 {
		return
	}
	r.setBlocked(stage, fmt.Sprintf(
		"fixture error: %s, so its %s expectation(s) were never evaluated — a row that skips its own assertions must not be recorded as a pass",
		why, strings.Join(names, ", ")))
}

func (r *fixtureRun) logCmd(label string, res CmdResult, err error) {
	if err != nil {
		r.logf("--- %s: harness error: %v ---", label, err)
		return
	}
	r.logf("--- %s: exit %d (%s) in %s ---\n%s", label, res.ExitCode, exitClassName(res.ExitCode), res.Duration, res.Combined())
}

// decoyKeyReason explains, in a blocker message, why no decoy public key is
// available: either the fixture never asked for signing (a fixture bug —
// a wrong-key tamper is meaningless unsigned) or the decoy keygen failed.
func decoyKeyReason(sign bool, keygenErr string) string {
	switch {
	case !sign:
		return ": the fixture does not set request.sign, so no keys were generated at all"
	case keygenErr != "":
		return ": decoy keygen failed: " + keygenErr
	default:
		return ""
	}
}

func keygenFailureDetail(res CmdResult, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("exit %d: %s", res.ExitCode, truncate(res.Combined(), 500))
}

// recordBundleStats measures the bundle copied out to hostBundleDir and
// records the performance baseline (size, .deb count) on the row. It reports
// whether the pipeline should continue.
//
// The walk's errors are propagated rather than skipped, and a walk that
// fails records nothing at all: BundleSizeBytes=0 / PackageCount=0 for an
// unreadable or missing directory is indistinguishable in the result JSON
// from a bundle that genuinely contains nothing, and both fields are
// omitempty, so the zeros do not even show up as suspicious. "We could not
// measure this" is a blocked row, not a bundle of size zero.
func (r *fixtureRun) recordBundleStats(hostBundleDir string) bool {
	var total int64
	count := 0
	err := filepath.Walk(hostBundleDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		total += info.Size()
		if strings.HasSuffix(p, ".deb") {
			count++
		}
		return nil
	})
	if err != nil {
		r.setBlocked("bundle-transfer", "measuring the copied-out bundle at "+hostBundleDir+": "+err.Error())
		return false
	}
	r.row.BundleSizeBytes = total
	r.row.PackageCount = count
	return true
}

// checkDeterminism runs a second, identical build into a different output
// directory on the same builder container and compares the manifest and
// repository metadata byte-for-byte (/ contract-brief.md rule
// 2: "Two builds of the same request must produce byte-identical manifests
// and repository metadata"). It returns whether the pipeline should
// continue; a determinism mismatch is always a StatusFail (the build itself
// already succeeded twice — this is a real correctness claim, not a missing
// feature), unless the second build's own exit class was itself blocked.
func (r *fixtureRun) checkDeterminism(ctx context.Context, d Debark, args BuildArgs, work string) bool {
	args2 := args
	// The underscore is load-bearing, not a naming choice.
	//
	// apt spells a source URI as a cache FILE NAME by turning "/" into "_"
	// and percent-encoding a character set that includes "_" itself, and
	// the bundle's own output directory reaches lock.ClosedWorld.
	// OutputDigest through two ordinary success-path warnings that name
	// those files. maskClosedWorldPaths has to undo that encoding, and the
	// first attempt at it modelled apt as a bare "/" -> "_" replacement,
	// which is wrong: every --out path containing "_", " ", "~" or "="
	// still leaked, so two builds of one request into different directories
	// produced different bundle ids.
	//
	// That bug survived its own fix's test because the test generated its
	// fixture with the same rule it was checking. It ALSO survived this
	// check, because "bundle" and "bundle-determinism-2" contain no
	// character apt encodes — so the one row in the suite whose whole job is
	// to compare two output directories was comparing the two spellings
	// that cannot tell the correct encoder from the wrong one.
	//
	// Naming the second directory with an underscore closes that. It costs
	// nothing, it applies to every determinism row rather than to a special
	// one, and a re-introduction of the naive encoder turns those rows red
	// here rather than in a signed artefact.
	args2.Out = "/work/bundle_determinism_2"
	var res2 CmdResult
	err := r.timeStage("determinism", func() error {
		var e error
		res2, e = d.Build(ctx, args2)
		return e
	})
	r.logCmd("build (determinism rerun)", res2, err)
	if err != nil {
		r.setBlocked("determinism", err.Error())
		return false
	}
	if res2.ExitCode != 0 {
		detail := fmt.Sprintf("second build exit %d (%s): %s", res2.ExitCode, exitClassName(res2.ExitCode), truncate(res2.Combined(), 500))
		r.reportStageFailure("determinism", detail, res2)
		return false
	}
	hostBundle2 := filepath.Join(work, "bundle_determinism_2")
	if err := d.C.CopyOut(ctx, args2.Out, hostBundle2); err != nil {
		r.setBlocked("determinism", "copy second bundle out: "+err.Error())
		return false
	}
	compareFiles := []string{"debark.manifest.json", "debark.manifest.sig", "lock.json", "repo/Packages", "repo/Release"}
	hostBundle1 := filepath.Join(work, "bundle")
	var diffs []string
	for _, rel := range compareFiles {
		a, aerr := os.ReadFile(filepath.Join(hostBundle1, filepath.FromSlash(rel)))
		b, berr := os.ReadFile(filepath.Join(hostBundle2, filepath.FromSlash(rel)))
		// "This file does not exist" is a fact about what the build wrote;
		// any other read error (a permission problem, an I/O error on the
		// host scratch directory) is a fact about the harness, and calling
		// the second one a determinism violation would blame the product for
		// the host's filesystem.
		if unreadable := firstUnreadable(aerr, berr); unreadable != nil {
			r.setBlocked("determinism", "comparing "+rel+": could not read a bundle file the harness itself copied out: "+unreadable.Error())
			return false
		}
		switch {
		case aerr != nil && berr != nil:
			continue // neither build wrote this optional file
		case aerr != nil || berr != nil:
			diffs = append(diffs, fmt.Sprintf("%s: present in one build only (run1 err=%v, run2 err=%v)", rel, aerr, berr))
		case string(a) != string(b):
			diffs = append(diffs, fmt.Sprintf("%s: differs between two builds of the same request (%d vs %d bytes)", rel, len(a), len(b)))
		}
	}
	ok := len(diffs) == 0
	r.row.DeterminismOK = &ok
	if !ok {
		r.setFail("determinism", "not byte-identical: "+strings.Join(diffs, "; "))
		return false
	}
	return true
}

// firstUnreadable returns the first error that is not simply "the file is
// not there" — i.e. the first one that says the harness could not read a
// file rather than that the build did not write it.
func firstUnreadable(errs ...error) error {
	for _, err := range errs {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// buildDecoySignature builds a small, differently-contented bundle signed
// with the same key and returns its debark.manifest.sig bytes.
func (r *fixtureRun) buildDecoySignature(ctx context.Context, d Debark, signKey string) ([]byte, error) {
	args := BuildArgs{
		Snapshot: "/work/snapshot.tar.zst",
		Packages: []string{decoyVendorPackage},
		Out:      "/work/decoy-bundle",
		Backend:  "local",
		Sign:     signKey,
	}
	res, err := d.Build(ctx, args)
	r.logCmd("build (swapped-signature decoy)", res, err)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("decoy build exit %d: %s", res.ExitCode, truncate(res.Combined(), 500))
	}
	work := r.fixtureWorkDir()
	hostSig := filepath.Join(work, "decoy.manifest.sig")
	if err := d.C.CopyOut(ctx, "/work/decoy-bundle/debark.manifest.sig", hostSig); err != nil {
		return nil, err
	}
	return os.ReadFile(hostSig)
}
