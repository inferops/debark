package apt

// Regression tests for defects found reviewing the container backend for
// parity with the fixes that landed on the local backend. Like
// container_test.go, these run without docker/podman, without a network and
// without touching anything outside t.TempDir().

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/store"
)

// --- the envelope's filename is a host path (arbitrary-move defect) --------
//
// These cover one defect with one shape at three depths. The envelope is
// untrusted by this backend's own design -- containerCrossCheckEnvelope and
// containerReverifyFiles exist precisely so neither side of it is believed
// alone -- and Filename was the one field in it that became a host path with
// nothing looking at it: containerRewriteStagedPaths joined it onto
// ArchivesDir or ExternalRepoDir, and core/engine's ingestPlan then calls
// store.PutFile(StagedPath, moveOK=true), which renames the source into the
// content store (falling back to copy-then-os.Remove). So an escaping
// Filename did not read a file outside the mount -- it MOVED it off the
// builder, destructively. That was demonstrated end to end against the real
// core/store before these tests were written, not reasoned about.
//
// External selections were the worst case: containerCrossCheckEnvelope
// `continue`s past every ReasonExternal entry, and containerReverifyFiles
// iterates the envelope's files[], which by that same rule never lists them.
// Nothing host-side touched an external selection at all, at any point,
// before it became a path.

func TestContainerCrossCheckEnvelope_RefusesEscapingFilename(t *testing.T) {
	cases := []struct {
		name     string
		filename string
	}{
		{"parent reference", "../../../secrets/id_ed25519"},
		{"posix separator", "sub/dir/pkg.deb"},
		{"windows separator", `..\..\secrets\pkg.deb`},
		{"drive letter", "d:/secrets/pkg.deb"},
		{"bare parent", ".."},
		{"not a deb at all", "id_ed25519"},
	}
	for _, tc := range cases {
		t.Run("external selection: "+tc.name, func(t *testing.T) {
			env := containerBaseEnvelope()
			env.Plan.Selections = append(env.Plan.Selections, resolve.Selection{
				Name: "vendor-tool", Arch: "amd64", Version: "1.0",
				Filename: tc.filename, Reason: lock.ReasonExternal,
			})
			// "vendor-tool" IS among the names the host staged, so the
			// selection gets past the reason:external entitlement check and
			// the filename guard is what has to refuse it -- which is the
			// point of this test.
			err := containerCrossCheckEnvelope(env, []string{"vendor-tool"}, "")
			if err == nil || dferr.ClassOf(err) != dferr.Verification {
				t.Fatalf("got %v, want a Verification error: an external selection is skipped by every other "+
					"check in this function, so if its filename is not refused here nothing refuses it", err)
			}
		})
		t.Run("archives selection: "+tc.name, func(t *testing.T) {
			env := containerBaseEnvelope()
			env.Plan.Selections[0].Filename = tc.filename
			env.Files[0].Filename = tc.filename
			err := containerCrossCheckEnvelope(env, nil, "")
			if err == nil || dferr.ClassOf(err) != dferr.Verification {
				t.Fatalf("got %v, want a Verification error", err)
			}
		})
	}
}

func TestContainerRewriteStagedPaths_RefusesEscapingFilename(t *testing.T) {
	for _, reason := range []string{lock.ReasonExternal, lock.ReasonRequested} {
		t.Run(reason, func(t *testing.T) {
			plan := &resolve.Plan{Selections: []resolve.Selection{{
				Name: "vendor-tool", Filename: "../../secrets/id_ed25519", Reason: reason,
			}}}
			err := containerRewriteStagedPaths(plan, "/work/archives", "/work/external")
			if err == nil || dferr.ClassOf(err) != dferr.Verification {
				t.Fatalf("got %v, want a Verification error", err)
			}
			// And, whatever the caller then does with that error, no
			// escaping path was left in the plan for core/engine to move.
			if got := filepath.ToSlash(plan.Selections[0].StagedPath); strings.Contains(got, "secrets/id_ed25519") {
				t.Errorf("StagedPath = %q: the escaping path was written into the plan anyway", got)
			}
		})
	}
}

// TestContainerResolve_HostileEnvelopeFilename_NothingIsMoved is the same
// defect at the level an operator meets it: a full Backend.Resolve with the
// container run faked (no docker, no network -- fakeContainerPlanRun, the
// same substitution every ClosedWorld test in container_test.go uses), whose
// inner process writes an envelope naming an external .deb outside the
// external staging directory. The file it points at must still be on disk
// afterwards.
func TestContainerResolve_HostileEnvelopeFilename_NothingIsMoved(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)

	root := t.TempDir()
	externalDir := filepath.Join(root, "external")
	archivesDir := filepath.Join(root, "archives")
	secretsDir := filepath.Join(root, "secrets")
	for _, d := range []string{externalDir, archivesDir, secretsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(secretsDir, "id_ed25519")
	if err := os.WriteFile(victim, []byte("PRIVATE KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{Selections: []resolve.Selection{{
			Name: "vendor-tool", Arch: "amd64", Version: "1.0",
			Filename: "../secrets/id_ed25519",
			SHA256:   strings.Repeat("a", 64),
			Reason:   lock.ReasonExternal,
		}}},
	}
	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, env, &argv)

	b := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		Packages:         []string{"jq"},
		ExternalRepoDir:  externalDir,
		ExternalNames:    []string{"vendor-tool"},
		ArchivesDir:      archivesDir,
		WorkDir:          fx.workDir,
	})

	// Whatever Resolve decided, do to its result exactly what the next stage
	// does: core/engine/resolve.go's ingestPlan walks the returned
	// Selections and calls st.PutFile(sel.StagedPath, true). moveOK is what
	// makes this destructive -- store.PutFile renames the source into the
	// content store, and falls back to copy-then-os.Remove across
	// filesystems. Running it here is what turns "the path escaped" from an
	// argument into a demonstration.
	if plan != nil {
		st, oerr := store.Open(filepath.Join(root, "store"))
		if oerr != nil {
			t.Fatal(oerr)
		}
		for i := range plan.Selections {
			if sp := plan.Selections[i].StagedPath; sp != "" {
				_, _, _ = st.PutFile(context.Background(), sp, true)
			}
		}
	}
	if _, serr := os.Stat(victim); serr != nil {
		t.Errorf("the file the envelope pointed at is gone (%v): an escaping StagedPath is a destructive "+
			"MOVE off the builder, not a read", serr)
	}

	if err == nil || dferr.ClassOf(err) != dferr.Verification {
		t.Fatalf("Resolve error = %v, want a Verification error naming the filename", err)
	}
	if !strings.Contains(err.Error(), "id_ed25519") {
		t.Errorf("error %q does not name the refused filename", err)
	}
}

// --- external selections are now re-verified host-side ---------------------

// TestContainerReverifyExternalFiles covers the other half of the same
// finding: external selections were exempt from the host-side re-hash as well
// as from the plan/files cross-check, because the envelope's files[] is
// scoped to what is in --archives and an external .deb is used in place from
// the read-only --external-repo mount.
func TestContainerReverifyExternalFiles(t *testing.T) {
	dir := t.TempDir()
	content := []byte("fake vendor .deb\n")
	if err := os.WriteFile(filepath.Join(dir, "vendor-tool_1.0_amd64.deb"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, size, err := digest.SHA256File(filepath.Join(dir, "vendor-tool_1.0_amd64.deb"))
	if err != nil {
		t.Fatal(err)
	}
	planWith := func(mut func(*resolve.Selection)) *resolve.Plan {
		s := resolve.Selection{
			Name: "vendor-tool", Arch: "amd64", Version: "1.0",
			Filename: "vendor-tool_1.0_amd64.deb", SHA256: sum, Size: size, Reason: lock.ReasonExternal,
		}
		if mut != nil {
			mut(&s)
		}
		return &resolve.Plan{Selections: []resolve.Selection{s}}
	}

	t.Run("matches", func(t *testing.T) {
		if err := containerReverifyExternalFiles(planWith(nil), dir); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("digest mismatch", func(t *testing.T) {
		p := planWith(func(s *resolve.Selection) { s.SHA256 = strings.Repeat("f", 64) })
		if err := containerReverifyExternalFiles(p, dir); err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})
	t.Run("size mismatch", func(t *testing.T) {
		p := planWith(func(s *resolve.Selection) { s.Size = size + 1 })
		if err := containerReverifyExternalFiles(p, dir); err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})
	t.Run("a file the host never staged", func(t *testing.T) {
		p := planWith(func(s *resolve.Selection) { s.Filename = "never-staged_1.0_amd64.deb" })
		if err := containerReverifyExternalFiles(p, dir); err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error", err)
		}
	})
	t.Run("non-external selections are left to containerReverifyFiles", func(t *testing.T) {
		p := planWith(func(s *resolve.Selection) {
			s.Reason = lock.ReasonRequested
			s.Filename = "not-in-this-dir_1.0_amd64.deb"
		})
		if err := containerReverifyExternalFiles(p, dir); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	// This subtest used to assert the opposite -- that an external selection
	// with no ExternalRepoDir was fine -- on the strength of this function's
	// own doc comment claiming "containerReverifyFiles already covers them".
	// It does not: containerReverifyFiles iterates env.Files, and files[]
	// never lists an external selection. With dir == "" the selection was
	// therefore checked by nothing at all, and an envelope naming a
	// nonexistent file with an all-zero sha256 was accepted and returned with
	// a StagedPath.
	t.Run("an external selection with no external repo dir at all is refused", func(t *testing.T) {
		p := planWith(func(s *resolve.Selection) {
			s.Filename = "never-staged_1.0_amd64.deb"
			s.SHA256 = strings.Repeat("0", 64)
		})
		err := containerReverifyExternalFiles(p, "")
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("got %v, want a Verification error: with no external staging directory there is nothing "+
				"host-side that could hold this selection's bytes, so nothing can verify them", err)
		}
	})
	t.Run("no external repo dir and no external selection is fine", func(t *testing.T) {
		p := planWith(func(s *resolve.Selection) { s.Reason = lock.ReasonRequested })
		if err := containerReverifyExternalFiles(p, ""); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// TestContainerResolve_ExternalSelectionIsReverifiedHostSide proves the wiring,
// not just the helper: Resolve must actually consult
// containerReverifyExternalFiles. Before the fix an external selection reached
// the returned plan without any host-side check of its bytes at all, so an
// envelope could name a real, staged file and claim a digest that was not its
// -- and that claimed digest is what core/engine writes into the lock and what
// core/bundle then signs.
func TestContainerResolve_ExternalSelectionIsReverifiedHostSide(t *testing.T) {
	newFixture := func(t *testing.T, claimedSHA string) (*containerBackend, ResolveInput) {
		t.Helper()
		fx := setupContainerClosedWorldFixture(t)
		root := t.TempDir()
		externalDir := filepath.Join(root, "external")
		archivesDir := filepath.Join(root, "archives")
		for _, d := range []string{externalDir, archivesDir} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		deb := filepath.Join(externalDir, "vendor-tool_1.0_amd64.deb")
		content := []byte("fake vendor .deb\n")
		if err := os.WriteFile(deb, content, 0o644); err != nil {
			t.Fatal(err)
		}
		sum, size, err := digest.SHA256File(deb)
		if err != nil {
			t.Fatal(err)
		}
		if claimedSHA == "" {
			claimedSHA = sum
		}
		env := containerEnvelope{
			SchemaVersion: containerEnvelopeSchema,
			Plan: resolve.Plan{Selections: []resolve.Selection{{
				Name: "vendor-tool", Arch: "amd64", Version: "1.0",
				Filename: "vendor-tool_1.0_amd64.deb",
				SHA256:   claimedSHA, Size: size, Reason: lock.ReasonExternal,
			}}},
		}
		var argv []string
		execContainerFn = fakeContainerPlanRun(t, fx.workDir, env, &argv)
		return newContainerBackend(ContainerOptions{SelfPath: fx.selfPath}), ResolveInput{
			Snapshot:         fx.snap,
			SnapshotFilesDir: fx.filesDir,
			ExternalRepoDir:  externalDir,
			ExternalNames:    []string{"vendor-tool"},
			ArchivesDir:      archivesDir,
			WorkDir:          fx.workDir,
		}
	}

	t.Run("a digest the host file does not have is refused", func(t *testing.T) {
		b, in := newFixture(t, strings.Repeat("b", 64))
		_, err := b.Resolve(context.Background(), in)
		if err == nil || dferr.ClassOf(err) != dferr.Verification {
			t.Fatalf("Resolve error = %v, want a Verification error", err)
		}
	})

	t.Run("a genuine external selection still resolves", func(t *testing.T) {
		b, in := newFixture(t, "")
		plan, err := b.Resolve(context.Background(), in)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		want := filepath.Join(in.ExternalRepoDir, "vendor-tool_1.0_amd64.deb")
		if got := plan.Selections[0].StagedPath; got != want {
			t.Errorf("StagedPath = %q, want %q", got, want)
		}
	})
}

// --- WorkDir placement -----------------------------------------------------

// TestContainerRelativeWorkDirIsRefusedBeforeAnythingIsCreated is the
// container backend's answer to closedworld.go's "an empty WorkDir would make
// this a RELATIVE path, so the private apt root has nowhere to live". The
// container path already required WorkDir to be non-empty, but a RELATIVE one
// was accepted far enough to os.MkdirAll it and write a staged snapshot.json
// under it -- in whatever directory the process happened to be running from
// (`debark resolve --work ./work` is a legal command line: internal/cli's
// --work flag is operator-supplied and only marked required, never checked
// for shape) -- before containerMountSource finally rejected it several steps
// later. Nothing may be created until the path is known to be usable.
func TestContainerRelativeWorkDirIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	execContainerFn = func(context.Context, string, []string, func(evidence.Event)) (containerRunResult, error) {
		t.Error("a relative WorkDir must be refused long before a container runs")
		return containerRunResult{}, nil
	}
	b := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})

	t.Run("Resolve", func(t *testing.T) {
		_, err := b.Resolve(context.Background(), ResolveInput{
			Snapshot:         fx.snap,
			SnapshotFilesDir: fx.filesDir,
			Packages:         []string{"jq"},
			ArchivesDir:      t.TempDir(),
			WorkDir:          "relative-work",
		})
		if err == nil || dferr.ClassOf(err) != dferr.Usage {
			t.Fatalf("got %v, want a Usage error", err)
		}
	})
	t.Run("ClosedWorld", func(t *testing.T) {
		_, err := b.ClosedWorld(context.Background(), ClosedWorldInput{
			Snapshot:         fx.snap,
			SnapshotFilesDir: fx.filesDir,
			BundleRepoDir:    t.TempDir(),
			WorkDir:          "relative-work",
		})
		if err == nil || dferr.ClassOf(err) != dferr.Usage {
			t.Fatalf("got %v, want a Usage error", err)
		}
	})

	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("the working directory gained %q: a relative WorkDir must be refused before anything is created", e.Name())
	}
}

// --- "reason: external" is an envelope-controlled routing label -------------
//
// The envelope is produced by a process the host cannot vouch for -- the
// premise container_driver.go is written on -- and reason:external is not a
// description in it, it is a routing decision. Every host-side byte check
// short-circuits on the label: containerCrossCheckEnvelope skips past it
// (external .debs legitimately never appear in files[], which is scoped to
// --archives), containerReverifyFiles iterates files[] and so cannot see one
// either, and containerRewriteStagedPaths sends it to ExternalRepoDir instead
// of ArchivesDir. localBackend has no equivalent hole: it sets Reason =
// ReasonExternal only for names in in.ExternalNames (local.go's
// externalConfirmed), so there the label is always a host-side conclusion.
//
// The two tests below are the two ways the container backend used to let the
// envelope decide it for itself.

// TestContainerResolve_ExternalLabelMustMatchWhatTheHostStaged is the
// substitution: an envelope that labels a package the host never staged an
// external as reason:external, pointing it at a vendor .deb the host DID
// stage under a different identity.
//
// Measured before the fix: an envelope claiming `openssl` was reason:external
// with filename "vendorpkg_1.0_amd64.deb" was accepted, and the vendor bytes
// entered the bundle under the identity `openssl` -- correct digest, correct
// size, real file on disk, every re-hash passing -- with the provenance claim
// decided entirely by the untrusted side. That is the identity of a package
// in a signed lock being chosen by the process the checks exist to catch.
func TestContainerResolve_ExternalLabelMustMatchWhatTheHostStaged(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	root := t.TempDir()
	externalDir := filepath.Join(root, "external")
	archivesDir := filepath.Join(root, "archives")
	for _, d := range []string{externalDir, archivesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A real vendor .deb the host really staged, under its real name.
	deb := filepath.Join(externalDir, "vendorpkg_1.0_amd64.deb")
	if err := os.WriteFile(deb, []byte("fake vendor .deb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, size, err := digest.SHA256File(deb)
	if err != nil {
		t.Fatal(err)
	}

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{Selections: []resolve.Selection{{
			// The host asked for "vendorpkg". The envelope returns
			// "openssl", carrying vendorpkg's bytes and vendorpkg's digest.
			Name: "openssl", Arch: "amd64", Version: "1.0",
			Filename: "vendorpkg_1.0_amd64.deb",
			SHA256:   sum, Size: size, Reason: lock.ReasonExternal,
		}}},
	}, &argv)

	b := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		Packages:         []string{"openssl"},
		ExternalRepoDir:  externalDir,
		ExternalNames:    []string{"vendorpkg"},
		ArchivesDir:      archivesDir,
		WorkDir:          fx.workDir,
	})
	if err == nil || dferr.ClassOf(err) != dferr.Verification {
		t.Fatalf("Resolve error = %v (plan %+v), want a Verification error: the host staged no external named "+
			"openssl, so nothing entitles the envelope to route it past every byte check", err, plan)
	}
	if !strings.Contains(err.Error(), "openssl") {
		t.Errorf("error %q does not name the package whose external label was refused", err)
	}
	if plan != nil {
		t.Errorf("plan = %+v, want nil: nothing may reach the engine from a refused envelope", plan)
	}
}

// TestContainerResolve_ExternalSelectionWithNoExternalRepoDirIsRefused is the
// same label doing its routing with nowhere for it to route TO.
//
// With ExternalRepoDir == "" an external selection was checked by literally
// nothing: containerCrossCheckEnvelope skipped it, containerReverifyExternal-
// Files returned early on the empty dir, and containerReverifyFiles only ever
// iterates env.Files, which by that same rule never lists it. Measured: this
// envelope -- naming a file that does not exist, sha256 all zeros -- was
// accepted, and Resolve returned it to the engine with a StagedPath.
//
// ExternalNames deliberately DOES name "vendor-tool" here, so the entitlement
// check added alongside this one cannot be what refuses it: this test is only
// satisfied by the dir == "" branch itself.
func TestContainerResolve_ExternalSelectionWithNoExternalRepoDirIsRefused(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	archivesDir := t.TempDir()

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{Selections: []resolve.Selection{{
			Name: "vendor-tool", Arch: "amd64", Version: "1.0",
			Filename: "never-staged_1.0_amd64.deb",
			SHA256:   strings.Repeat("0", 64), Size: 1234,
			Reason: lock.ReasonExternal,
		}}},
	}, &argv)

	b := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		ExternalNames:    []string{"vendor-tool"},
		ExternalRepoDir:  "", // the host staged no external repository at all
		ArchivesDir:      archivesDir,
		WorkDir:          fx.workDir,
	})
	if err == nil || dferr.ClassOf(err) != dferr.Verification {
		t.Fatalf("Resolve error = %v, want a Verification error", err)
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil: an external selection with no external staging directory was accepted", plan)
	}
}

// --- the inner process's exit class is not the backend's to downgrade -------

// TestContainerResolve_InnerExit3WithNoDetailIsIncomplete: the inner process
// classified the run 3 (incomplete) against the frozen exit table, and the
// envelope named nothing unresolved.
//
// This used to append a "container.incomplete-no-detail" warning and return
// (plan, nil). The engine derives its own exit class from lockDoc.Unresolved
// (core/engine), which is empty on exactly this branch, so nothing downstream
// retained any trace of the 3 and the operator was told exit 0 -- a clean
// build -- for a run the process that did the resolving called incomplete. A
// warning in the lock is not an exit code a script can read.
func TestContainerResolve_InnerExit3WithNoDetailIsIncomplete(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	archivesDir := t.TempDir()

	env := containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan:          resolve.Plan{Selections: nil, Unresolved: nil},
	}
	execContainerFn = func(_ context.Context, runtimePath string, argv []string, _ func(evidence.Event)) (containerRunResult, error) {
		// Only the resolve run itself reports 3. Resolve may make a second,
		// unrelated container call afterwards (containerCopyVolumeToHost, on
		// the Windows/macOS named-volume default), and failing that one would
		// prove something else entirely.
		if !containsArg(argv, "/debark") {
			return containerRunResult{Argv: append([]string{runtimePath}, argv...), ExitCode: 0}, nil
		}
		data, merr := json.Marshal(env)
		if merr != nil {
			t.Fatal(merr)
		}
		if werr := os.WriteFile(filepath.Join(fx.workDir, "plan.json"), data, 0o644); werr != nil {
			t.Fatal(werr)
		}
		return containerRunResult{
			Argv:     append([]string{runtimePath}, argv...),
			Stderr:   []byte("debark: resolve incomplete\n"),
			ExitCode: int(dferr.Incomplete),
		}, nil
	}

	b := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		Packages:         []string{"jq"},
		ArchivesDir:      archivesDir,
		WorkDir:          fx.workDir,
	})
	if err == nil {
		t.Fatalf("Resolve returned (%+v, nil) for an inner exit 3: a backend must not downgrade a code the frozen exit table defines", plan)
	}
	if got := dferr.ClassOf(err); got != dferr.Incomplete {
		t.Errorf("dferr.ClassOf(err) = %v (%d), want dferr.Incomplete (%d): %v", got, int(got), int(dferr.Incomplete), err)
	}
}

// --- an --image override must not record a digest for an image that never ran

// TestContainerResolve_ImageOverrideRecordsNoBorrowedDigest: with
// ContainerOptions.Image set to a tag-only override, distro.Resolve still
// returns the target release's table row, and Resolve used to write
// Resolver.ImageDigest from it. The lock then simultaneously warned "not
// pinned by digest" and recorded a pin -- bookworm-slim's, for an image that
// never ran -- and the same wrong pair went into the backend.selected
// evidence event, which is inside the signed bundle.
func TestContainerResolve_ImageOverrideRecordsNoBorrowedDigest(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	archivesDir := t.TempDir()

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan:          resolve.Plan{},
	}, &argv)

	const override = "myregistry.example/debian:custom"
	b := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath, Image: override})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		Packages:         []string{"jq"},
		ArchivesDir:      archivesDir,
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Resolver.Image != override {
		t.Errorf("Resolver.Image = %q, want the override %q", plan.Resolver.Image, override)
	}
	if plan.Resolver.ImageDigest != "" {
		t.Errorf("Resolver.ImageDigest = %q for image %q, want empty: that digest belongs to the distro table's "+
			"bookworm-slim row, which is not the image that ran", plan.Resolver.ImageDigest, plan.Resolver.Image)
	}
	// And the warning that says why must still be there, so the record is not
	// merely silent about the pin it does not have.
	var warned bool
	for _, w := range plan.Warnings {
		if w.Code == containerWarnUnpinnedImage {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no %s warning for a tag-only override: %+v", containerWarnUnpinnedImage, plan.Warnings)
	}
}

// TestContainerResolve_ImageOverridePinnedByDigestRecordsThatDigest is the
// companion: an override that DOES carry a digest contributes it, so the fix
// is "record the digest of the image that ran", not "never record one".
func TestContainerResolve_ImageOverridePinnedByDigestRecordsThatDigest(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	archivesDir := t.TempDir()

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan:          resolve.Plan{},
	}, &argv)

	wantDigest := "sha256:" + strings.Repeat("c", 64)
	b := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath, Image: "myregistry.example/debian@" + wantDigest})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		Packages:         []string{"jq"},
		ArchivesDir:      archivesDir,
		WorkDir:          fx.workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Resolver.ImageDigest != wantDigest {
		t.Errorf("Resolver.ImageDigest = %q, want %q taken from the override itself", plan.Resolver.ImageDigest, wantDigest)
	}
}

// --- a stale plan.json is another run's answer -----------------------------

// TestContainerResolve_StalePlanEnvelopeIsNotReadBackAsThisRun: Resolve reads
// its result from a fixed <WorkDir>/plan.json, and never removed a
// pre-existing one. Measured: with a previous run's plan.json in place and a
// container that exits 0 having written nothing, Resolve returned the PREVIOUS
// run's selections as this run's answer. The engine's WorkDir is a fresh
// os.MkdirTemp today, but WorkDir is a public field on a frozen input struct
// carrying no such contract, and the cost of being wrong is a bundle built
// from another request's plan.
func TestContainerResolve_StalePlanEnvelopeIsNotReadBackAsThisRun(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)
	archivesDir := t.TempDir()

	stale, err := json.Marshal(containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{Selections: []resolve.Selection{{
			Name: "leftover-from-a-previous-build", Arch: "amd64", Version: "9.9",
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.workDir, "plan.json"), stale, 0o644); err != nil {
		t.Fatal(err)
	}

	// A container that succeeds but writes no envelope at all.
	execContainerFn = func(_ context.Context, runtimePath string, argv []string, _ func(evidence.Event)) (containerRunResult, error) {
		return containerRunResult{Argv: append([]string{runtimePath}, argv...), ExitCode: 0}, nil
	}

	b := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		Packages:         []string{"jq"},
		ArchivesDir:      archivesDir,
		WorkDir:          fx.workDir,
	})
	if err == nil {
		t.Fatalf("Resolve returned a plan (%+v) for a run that wrote no envelope: it read a previous run's plan.json", plan)
	}
	if plan != nil {
		t.Errorf("plan = %+v, want nil", plan)
	}
	if strings.Contains(err.Error(), "leftover-from-a-previous-build") {
		t.Errorf("error %q names the stale plan's contents", err)
	}
}
