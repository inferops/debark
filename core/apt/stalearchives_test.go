package apt

// The stale shared archives volume, and the boundary that must NOT move with
// it. Both halves are in one file on purpose, for the reason 5837d82 gives
// for the same shape in core/verify: they are one decision, and splitting
// them across files is how a later change softens one without noticing it has
// broken the other.
//
// The defect. On Windows and macOS, storeVolumeChoice backs /archives with
// the shared, persistent, never-cleaned named volume containerDefaultStore-
// Volume ("debark-archives-cache"), and scanArchivesDir
// (internal/cli/cmd_resolve.go) puts EVERY .deb sitting in that directory
// into the resolve envelope's files[]. containerCrossCheckEnvelope then
// requires files[] and the plan to agree in both directions, so one .deb left
// behind by an earlier build of a different target failed every later build
// on that machine -- any target, any architecture, any package list -- at
// exit 4, dferr.Verification, which ADR-012 fixes as "a signature, digest or
// metadata check did not match" and which the CLI and the GUI both dress with
// "do not install a bundle that fails verification; obtain a fresh copy from
// the machine that built it".
//
// Measured on the Windows builder this was found on, whose
// debark-archives-cache held seven .deb files from two distributions side
// by side (hello 2.10-3, jq 1.6 and 1.7.1, libjq1 1.6 and 1.7.1, libonig5
// 6.9.8 and 6.9.9): a fresh
//
//	debark build --snapshot <ubuntu 24.04 minimal> --backend container jq
//
// ran for 61 s and exited 4 with
//
//	container: envelope files[] lists hello (hello:amd64) but the plan does
//	not select it
//
// No bundle was produced. Nothing had been verified. hello had nothing to do
// with the build and could not have entered its bundle.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
)

// staleVolumeEnvelope is the envelope a real inner resolve produces when
// /archives is the shared volume in the state above: the plan selects only
// what this build asked for, and files[] carries the whole directory.
func staleVolumeEnvelope() containerEnvelope {
	env := containerEnvelope{
		SchemaVersion: containerEnvelopeSchema,
		Plan: resolve.Plan{Selections: []resolve.Selection{
			{Name: "jq", Arch: "amd64", Version: "1.7.1-3ubuntu0.24.04.2", Filename: "jq_1.7.1-3ubuntu0.24.04.2_amd64.deb", Reason: lock.ReasonRequested},
		}},
		Files: []containerEnvelopeFile{
			{Name: "jq", Arch: "amd64", Version: "1.7.1-3ubuntu0.24.04.2", Filename: "jq_1.7.1-3ubuntu0.24.04.2_amd64.deb"},
			// Left by something else entirely. This is the file the real run
			// failed on.
			{Name: "hello", Arch: "amd64", Version: "2.10-3", Filename: "hello_2.10-3_amd64.deb"},
		},
	}
	return env
}

// TestContainerStaleArchivesVolumeIsNotTampering is the defect, driven
// against containerCrossCheckEnvelope directly over the shapes the real
// volume produced.
//
// Every case must be dferr.Environment (exit 2, "the machine cannot do the
// job"), must name the volume in the message, and must carry a hint naming
// the volume and the command that empties it -- because there is no flag, no
// config key and no DEBARK_* variable that points a build anywhere else,
// so an operator who is not told `docker volume rm` has nothing to try.
func TestContainerStaleArchivesVolumeIsNotTampering(t *testing.T) {
	cases := []struct {
		name  string
		files []containerEnvelopeFile
		// leftover is the name the refusal must call out.
		leftover string
	}{
		{
			name:     "one .deb from an earlier build of a different target",
			files:    staleVolumeEnvelope().Files,
			leftover: "hello",
		},
		{
			name: "the other distribution's build of a package this one also wants",
			files: append(staleVolumeEnvelope().Files,
				containerEnvelopeFile{Name: "libonig5", Arch: "amd64", Version: "6.9.8-1", Filename: "libonig5_6.9.8-1_amd64.deb"}),
			leftover: "hello",
		},
		{
			name: "an architecture this machine has never built for",
			files: append(staleVolumeEnvelope().Files[:1:1],
				containerEnvelopeFile{Name: "jq", Arch: "arm64", Version: "1.7.1-3ubuntu0.24.04.2", Filename: "jq_1.7.1-3ubuntu0.24.04.2_arm64.deb"}),
			leftover: "jq",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := staleVolumeEnvelope()
			env.Files = tc.files

			err := containerCrossCheckEnvelope(env, nil, containerDefaultStoreVolume)
			if err == nil {
				t.Fatal("the cross-check passed; this test is only meaningful while it still refuses")
			}
			if got := dferr.ClassOf(err); got != dferr.Environment {
				t.Errorf("dferr.ClassOf = %v (exit %d), want %v (exit %d): exit 4 says a signature, digest or "+
					"metadata check did not match, and an unselected file in a shared cache is neither a "+
					"truncation (that is files MISSING) nor something that can enter a bundle.\nerror: %v",
					got, int(got), dferr.Environment, int(dferr.Environment), err)
			}
			if !strings.Contains(err.Error(), containerDefaultStoreVolume) {
				t.Errorf("the message does not name the volume, which is the only thing the operator can act on: %v", err)
			}
			if !strings.Contains(err.Error(), tc.leftover) {
				t.Errorf("the message does not name the leftover file %q: %v", tc.leftover, err)
			}
			hint := dferr.HintOf(err)
			if hint == "" {
				t.Fatal("no hint: the remedy appears in no help text, no config key and no document")
			}
			for _, want := range []string{containerDefaultStoreVolume, "docker volume rm", "podman volume rm"} {
				if !strings.Contains(hint, want) {
					t.Errorf("hint does not mention %q: %s", want, hint)
				}
			}
			// The honest limit, asserted so a later edit cannot quietly turn
			// this into a promise: emptying the volume is the whole remedy
			// available today, and the hint must not imply a flag exists.
			if !strings.Contains(hint, "no flag") {
				t.Errorf("hint does not say there is no flag for this yet: %s", hint)
			}
		})
	}
}

// TestContainerUnselectedFileRefusalIsStable pins the fix for a defect this
// file's own tests found: the refusal used to range over a map, so with more
// than one leftover in the volume it named whichever entry Go's randomised
// map iteration reached first, and two runs of one failing build blamed
// different packages.
//
// It surfaced as an intermittent failure of the case above on Linux and not
// at all on Windows in the same session, which is the shape this class of bug
// always has. An error message an operator is told to act on has to say the
// same thing twice, so the refusal now walks the envelope's own files[] order
// and names the first unselected entry in it.
func TestContainerUnselectedFileRefusalIsStable(t *testing.T) {
	env := staleVolumeEnvelope()
	// Five leftovers, which is what the real volume held: with a map, the
	// chance of 200 runs agreeing is nil.
	env.Files = append(env.Files, []containerEnvelopeFile{
		{Name: "libonig5", Arch: "amd64", Version: "6.9.8-1", Filename: "libonig5_6.9.8-1_amd64.deb"},
		{Name: "libjq1", Arch: "amd64", Version: "1.6-2.1+deb12u2", Filename: "libjq1_1.6-2.1+deb12u2_amd64.deb"},
		{Name: "cowsay", Arch: "all", Version: "3.03+dfsg2-8", Filename: "cowsay_3.03+dfsg2-8_all.deb"},
		{Name: "sl", Arch: "amd64", Version: "5.02-1", Filename: "sl_5.02-1_amd64.deb"},
	}...)

	first := containerCrossCheckEnvelope(env, nil, containerDefaultStoreVolume)
	if first == nil {
		t.Fatal("the cross-check passed")
	}
	for i := 0; i < 200; i++ {
		got := containerCrossCheckEnvelope(env, nil, containerDefaultStoreVolume)
		if got == nil || got.Error() != first.Error() {
			t.Fatalf("run %d reported a different file:\n first: %v\n now:   %v", i, first, got)
		}
	}
	// And it is the first unselected entry in the envelope's own order, not
	// merely a stable arbitrary one -- that order is the order
	// scanArchivesDir read the directory in, which is what an operator sees
	// when they list the volume.
	if !strings.Contains(first.Error(), "hello") {
		t.Errorf("the refusal names %v, want the first unselected entry in files[] (hello)", first)
	}
}

// TestContainerUnselectedFileInARunOwnedArchivesDirStaysVerification is the
// half that must NOT move, and the reason the refusal above is keyed on how
// /archives was backed rather than on "an extra file is harmless".
//
// With a bind mount -- every Linux build, and any run pointed at an explicit
// directory -- the host created that directory for this run and this run
// alone. A file in it the plan does not select was put there by the inner
// process, which is a real anomaly about the run, and exit 4 is what says so.
// The boundary is structural, in the shape 2d6d5c7 and 5837d82 drew, and not
// a general softening.
func TestContainerUnselectedFileInARunOwnedArchivesDirStaysVerification(t *testing.T) {
	err := containerCrossCheckEnvelope(staleVolumeEnvelope(), nil, "")
	if err == nil {
		t.Fatal("an unselected file in a run-owned archives dir was accepted")
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("dferr.ClassOf = %v, want %v: with no shared volume there is no stale cache to blame.\nerror: %v",
			got, dferr.Verification, err)
	}
	if dferr.HintOf(err) != "" {
		t.Errorf("a bind-mounted run must not be told to remove a volume it never used: %s", dferr.HintOf(err))
	}
}

// TestContainerMissingSelectionStaysVerificationWithOrWithoutAVolume pins the
// OTHER direction of the cross-check, which the volume must not reach.
//
// A selection with no files[] entry means a file the bundle NEEDS is absent
// -- a truncated mount or a partial download -- and that is exactly what exit
// 4 exists to say. It stays Verification whether or not a shared volume is in
// use, because no amount of stale clutter can make a required file vanish.
func TestContainerMissingSelectionStaysVerificationWithOrWithoutAVolume(t *testing.T) {
	for _, volume := range []string{"", containerDefaultStoreVolume} {
		name := "bind mount"
		if volume != "" {
			name = "shared volume " + volume
		}
		t.Run(name, func(t *testing.T) {
			env := staleVolumeEnvelope()
			// Everything the plan selects is gone from files[]; only the
			// leftover remains, which is what a truncated mount looks like.
			env.Files = env.Files[1:]
			err := containerCrossCheckEnvelope(env, nil, volume)
			if err == nil {
				t.Fatal("a selection with no files[] entry was accepted")
			}
			if got := dferr.ClassOf(err); got != dferr.Verification {
				t.Errorf("dferr.ClassOf = %v, want %v: a file the bundle needs is missing.\nerror: %v",
					got, dferr.Verification, err)
			}
		})
	}
}

// TestContainerStaleVolumeVersionCollisionKeepsItsClassAndGainsAHint covers
// the OTHER way the same stale volume fails a build, which this change
// deliberately does not reclassify.
//
// files[] is keyed by name:arch here, so when the volume holds two versions
// of one package -- jq 1.6-2.1+deb12u2 from a Debian 12 build beside jq
// 1.7.1-3ubuntu0.24.04.2 from an Ubuntu 24.04 one, both measured in the same
// volume -- the map keeps whichever came last and reports it as a
// disagreement with the plan, even though the file the plan selected is
// sitting right there. That keying is a defect in its own right and is
// reported rather than changed here: the class stays Verification because the
// file that would enter the bundle is the one the envelope is contradicting
// itself about, but the hint now names the volume, which is the difference
// between "your bundle was tampered with" and "your cache is stale".
func TestContainerStaleVolumeVersionCollisionKeepsItsClassAndGainsAHint(t *testing.T) {
	env := staleVolumeEnvelope()
	// The Debian 12 target's plan, against an Ubuntu build's leftovers.
	env.Plan.Selections = []resolve.Selection{
		{Name: "jq", Arch: "amd64", Version: "1.6-2.1+deb12u2", Filename: "jq_1.6-2.1+deb12u2_amd64.deb", Reason: lock.ReasonRequested},
	}
	env.Files = []containerEnvelopeFile{
		{Name: "jq", Arch: "amd64", Version: "1.7.1-3ubuntu0.24.04.2", Filename: "jq_1.7.1-3ubuntu0.24.04.2_amd64.deb"},
	}

	err := containerCrossCheckEnvelope(env, nil, containerDefaultStoreVolume)
	if err == nil {
		t.Fatal("the plan/files version disagreement was accepted")
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("dferr.ClassOf = %v, want %v: this branch is about the file that WOULD enter the bundle.\nerror: %v",
			got, dferr.Verification, err)
	}
	if hint := dferr.HintOf(err); !strings.Contains(hint, containerDefaultStoreVolume) {
		t.Errorf("hint does not name the volume: %q", hint)
	}
	// And with no volume there is nothing to name, so the hint stays empty
	// and the CLI's class wording applies unchanged.
	if hint := dferr.HintOf(containerCrossCheckEnvelope(env, nil, "")); hint != "" {
		t.Errorf("a bind-mounted run gained a volume hint: %q", hint)
	}
}

// TestContainerResolve_StaleArchivesVolume_IsEnvironmentNotVerification is
// the same defect at the level an operator meets it: a full Backend.Resolve
// with the container run faked (no docker, no network), whose inner process
// returns the envelope a populated shared volume produces.
//
// StoreVolume is set explicitly because that is the only way anything but a
// Windows or macOS host reaches storeVolumeChoice's volume branch -- and the
// value it is set to is the same constant those hosts default to. It is also
// the only place in the tree that sets it, which is the gap this refusal's
// hint has to talk around.
func TestContainerResolve_StaleArchivesVolume_IsEnvironmentNotVerification(t *testing.T) {
	fx := setupContainerClosedWorldFixture(t)

	root := t.TempDir()
	archivesDir := filepath.Join(root, "archives")
	if err := os.MkdirAll(archivesDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var argv []string
	execContainerFn = fakeContainerPlanRun(t, fx.workDir, staleVolumeEnvelope(), &argv)

	b := newContainerBackend(ContainerOptions{SelfPath: fx.selfPath, StoreVolume: containerDefaultStoreVolume})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         fx.snap,
		SnapshotFilesDir: fx.filesDir,
		Packages:         []string{"jq"},
		ArchivesDir:      archivesDir,
		WorkDir:          fx.workDir,
	})
	if err == nil {
		t.Fatalf("Resolve returned a plan for an envelope listing an unselected file: %+v", plan)
	}
	if got := dferr.ClassOf(err); got != dferr.Environment {
		t.Errorf("dferr.ClassOf = %v (exit %d), want %v (exit %d).\nerror: %v",
			got, int(got), dferr.Environment, int(dferr.Environment), err)
	}
	if hint := dferr.HintOf(err); !strings.Contains(hint, "docker volume rm "+containerDefaultStoreVolume) {
		t.Errorf("the hint an operator would actually see does not carry the remedy: %q", hint)
	}
	// The mount really was the volume, not a bind of archivesDir: without
	// this the test would pass for a run that never took the branch it is
	// about.
	if !containsArg(argv, containerDefaultStoreVolume+":/archives") {
		t.Errorf("the run did not mount the named volume at /archives; argv=%v", argv)
	}
}
