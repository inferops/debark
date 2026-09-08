package verify

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/sign"
)

// This file extends verify_test.go's tamper matrix. The matrix there proves
// the common physical tampers (a flipped byte, an added file, a removed file)
// and the two signature swaps; what it does not reach is the second half of
// the trust boundary - the manifest as a *document*: what its paths and
// digests are allowed to say, what happens when the same manifest can be read
// two ways, whether a refusal happens before or after the bundle's contents
// are touched, and which dferr class an operator's automation will see.
//
// Every fixture below is a real bundle written to t.TempDir() by this package's
// own Build/Save/Sign pipeline (buildFixture), then damaged on disk. Nothing
// here mocks the verifier's inputs: a tamper test that fakes the artefact it
// is supposed to detect proves only that the fake behaved.

// --- shared helpers ---------------------------------------------------

// editManifest rewrites the manifest in place and re-signs it with the
// operator key, so steps 1-3 (parse, manifest_sha256, signature) all still
// pass and the specific document-level check under test is the only thing
// that can fail. Without the re-sign every one of these cases would stop at
// ProblemManifestDigest and prove nothing about the check it targets.
func editManifest(t *testing.T, f *fixture, fn func(m *manifest.Manifest)) {
	t.Helper()
	m, _, err := manifest.Load(f.dir)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	fn(m)
	// manifest.Save does not validate, and resign signs the raw bytes, so a
	// document core/manifest.Load will refuse can still be built and signed
	// here. That is deliberate: several cases below produce exactly such a
	// document, and verify has to refuse it rather than crash on it.
	if err := manifest.Save(f.dir, m); err != nil {
		t.Fatalf("manifest.Save: %v", err)
	}
	resign(t, f, f.privPath)
}

func loadSig(t *testing.T, f *fixture) *manifest.SignatureFile {
	t.Helper()
	sf, err := manifest.LoadSignature(f.dir)
	if err != nil {
		t.Fatalf("LoadSignature: %v", err)
	}
	if sf == nil {
		t.Fatal("bundle has no signature file")
	}
	return sf
}

func saveSig(t *testing.T, f *fixture, sf *manifest.SignatureFile) {
	t.Helper()
	if err := manifest.SaveSignature(f.dir, sf); err != nil {
		t.Fatalf("SaveSignature: %v", err)
	}
}

// problemKinds renders a report's problems as "kind[path]" strings, so a
// failure message says what verify actually found rather than dumping structs.
func problemKinds(r *Report) []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.Problems))
	for _, p := range r.Problems {
		out = append(out, p.Kind+"["+p.Path+"]")
	}
	return out
}

func hasProblem(r *Report, kind, path string) bool {
	if r == nil {
		return false
	}
	for _, p := range r.Problems {
		if p.Kind == kind && (path == "" || p.Path == path) {
			return true
		}
	}
	return false
}

// wantRefusedWith asserts the shape every report-level refusal must have: a
// report (not a Go error), OK false, and at least one problem of each wanted
// kind. Verify reserves Go errors for conditions that are not evidence about
// the bundle (a bad path, an unreadable key, a checker this machine cannot
// run); a tamper must never be reported that way, because the CLI turns a
// non-OK report into dferr.Verification (exit 4) and a Go error into whatever
// class it carries.
func wantRefusedWith(t *testing.T, r *Report, err error, kinds ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("Verify returned a Go error instead of a report: %v (class %d)", err, dferr.ClassOf(err))
	}
	if r == nil {
		t.Fatal("Verify returned a nil report and a nil error")
	}
	if r.OK {
		t.Fatalf("report.OK = true, want false; a tampered bundle must be refused (problems=%v)", problemKinds(r))
	}
	for _, k := range kinds {
		if !hasProblem(r, k, "") {
			t.Fatalf("problems = %v, want one of kind %q", problemKinds(r), k)
		}
	}
}

// wantOnlyKinds asserts the report carries NO problem of a kind outside the
// given set.
//
// It is the assertion this suite spent its first two revisions without, and
// the omission was structural rather than cosmetic. wantRefusedWith and
// wantProblem both ask only "is a problem of the wanted kind present", which a
// case satisfies just as well by failing for a reason it was not written to
// test - and several of the deepest cases here deliberately re-sign the
// manifest precisely so that the shallow checks pass, which is exactly the
// arrangement that would silently stop working if a step-1 refusal crept in
// front of it. A row that says "this specific check refuses this specific
// tamper" has to be able to fail when some *other* check does the refusing.
//
// Distinct kinds, not a multiset: a single tamper legitimately produces one
// problem per affected file (an empty files list produces one file-unexpected
// for every file in the bundle), and pinning those counts would make the
// assertion about the fixture's size rather than about verify's behaviour.
func wantOnlyKinds(t *testing.T, r *Report, kinds ...string) {
	t.Helper()
	allowed := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		allowed[k] = true
	}
	for _, p := range r.Problems {
		if !allowed[p.Kind] {
			t.Fatalf("report carries an unwanted problem %s[%s]: %s\nwant only %v, got %v",
				p.Kind, p.Path, p.Message, kinds, problemKinds(r))
		}
	}
}

// wantReachedContentChecks asserts the run got past steps 1-3 and actually
// walked the bundle.
//
// The counterpart to TestVerify_HardFailureStopsBeforeTheContentsAreTouched,
// which pins FilesChecked == 0 for the refusals that must abort early. This is
// the other half: a case whose whole point is a step-4-to-7 check proves
// nothing if the run aborted at step 3, because steps 4-7 never ran at all -
// and the report would look identical to a caller that only checks OK. Any row
// asserting a file, repository, lock or snapshot finding wants this.
func wantReachedContentChecks(t *testing.T, r *Report) {
	t.Helper()
	if r.FilesChecked == 0 {
		t.Fatalf("FilesChecked = 0: verification never reached the content checks, so this case proves nothing about them (problems=%v)",
			problemKinds(r))
	}
}

// knownProblemKinds is the frozen set from iface.go. Scripts branch on these
// strings, so a refusal reported under a kind outside this set is as bad as no
// refusal at all: the operator's automation will not recognise it.
var knownProblemKinds = map[string]bool{
	ProblemManifestMissing: true, ProblemManifestMalformed: true,
	ProblemSchemaUnknown: true, ProblemSignatureMissing: true,
	ProblemSignatureInvalid: true, ProblemSignatureUntrusted: true,
	ProblemManifestDigest: true, ProblemFileMissing: true,
	ProblemFileDigest: true, ProblemFileSize: true,
	ProblemFileUnexpected: true, ProblemFileNotRegular: true,
	ProblemRepoDigest: true,
	ProblemLockDigest: true, ProblemSnapshotDigest: true,
	ProblemSameMediaKey: true,
}

func wantOnlyKnownKinds(t *testing.T, r *Report) {
	t.Helper()
	for _, p := range r.Problems {
		if !knownProblemKinds[p.Kind] {
			t.Fatalf("problem kind %q is not one of the frozen kinds in iface.go", p.Kind)
		}
	}
}

// treeFingerprint records every path under root plus each file's exact bytes.
// It is the evidence for "verify never writes to the bundle it is checking":
// content, not just mtimes, because a rewrite that happened to preserve size
// and time would still be a write.
func treeFingerprint(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			lines = append(lines, "d "+rel)
			return nil
		}
		sum, size, herr := digest.SHA256File(p)
		if herr != nil {
			return herr
		}
		lines = append(lines, fmt.Sprintf("f %s %d %s", rel, size, sum))
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprint %s: %v", root, err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// --- manifest coverage ------------------------------------------------

// TestVerify_ManifestCoverageGaps covers the ways a manifest can fail to
// describe the bundle it travels with. Every one of them must be a refusal:
// the manifest is the only signed object, so anything it does not cover is
// unattested, and anything it covers that is not there is a bundle that was
// taken apart in transit.
func TestVerify_ManifestCoverageGaps(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f *fixture)
		want   []string
		// checkReport runs extra assertions specific to the case.
		checkReport func(t *testing.T, r *Report)
	}{
		{
			// An extra file dropped straight into repo/ - the directory apt
			// is pointed at. apt only reads what its indices name, but verify
			// deliberately does not assume the rest of repo/ is inert: an
			// unlisted .deb sitting next to the listed ones is exactly what a
			// "just add one more package" attack looks like.
			name: "unlisted file inside repo/",
			mutate: func(t *testing.T, f *fixture) {
				writeFile(t, filepath.Join(f.dir, "repo", "pool", "d", "demo", "backdoor_1.0_amd64.deb"), []byte("payload"))
			},
			want: []string{ProblemFileUnexpected},
		},
		{
			// Nothing is left describing the bundle's contents. Every real
			// file must then be reported as unattested, one problem each -
			// not a single summary line, because an operator has to see the
			// full extent.
			name: "manifest with an empty files list",
			mutate: func(t *testing.T, f *fixture) {
				editManifest(t, f, func(m *manifest.Manifest) { m.Files = nil })
			},
			want: []string{ProblemFileUnexpected},
			checkReport: func(t *testing.T, r *Report) {
				for _, want := range []string{"lock.json", "snapshot.json", "repo/Packages", "repo/Release"} {
					if !hasProblem(r, ProblemFileUnexpected, want) {
						t.Fatalf("no file-unexpected problem for %s; problems=%v", want, problemKinds(r))
					}
				}
			},
		},
		{
			// A manifest that covers zero files, in a bundle that genuinely
			// holds nothing else, is still refused - lock.json is not
			// optional, and checkLockAndSnapshot reads it unconditionally.
			// That single check is the whole of what stands between an
			// operator and an empty, validly-signed "bundle", so it is worth
			// its own case: FilesChecked is 0 here, meaning nothing in the
			// file-coverage machinery contributed to the refusal.
			name: "validly signed manifest covering an empty bundle",
			mutate: func(t *testing.T, f *fixture) {
				for _, n := range []string{"lock.json", "snapshot.json", "repo"} {
					if err := os.RemoveAll(filepath.Join(f.dir, n)); err != nil {
						t.Fatal(err)
					}
				}
				editManifest(t, f, func(m *manifest.Manifest) {
					m.Files = nil
					m.LockDigest = ""
					m.SnapshotDigest = ""
					m.Repository = manifest.Repository{}
				})
			},
			want: []string{ProblemLockDigest},
			checkReport: func(t *testing.T, r *Report) {
				if r.FilesChecked != 0 {
					t.Fatalf("FilesChecked = %d, want 0 for an empty bundle", r.FilesChecked)
				}
			},
		},
		{
			// snapshot.json is an optional companion, and
			// checkLockAndSnapshot returns early when it is absent - so
			// deleting it after signing is caught only by the file list.
			// This case exists to keep that one guard: if Files ever stopped
			// covering snapshot.json, a signed snapshot_digest would be
			// silently unverified.
			name: "snapshot.json deleted after signing",
			mutate: func(t *testing.T, f *fixture) {
				if err := os.Remove(filepath.Join(f.dir, "snapshot.json")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{ProblemFileMissing},
			checkReport: func(t *testing.T, r *Report) {
				if !hasProblem(r, ProblemFileMissing, "snapshot.json") {
					t.Fatalf("expected file-missing for snapshot.json; problems=%v", problemKinds(r))
				}
			},
		},
		{
			// The manifest and its signature are excluded from the walk by
			// path, so a manifest that lists itself describes a file the
			// checker will never see. It must fail closed (missing), never
			// pass by the entry being quietly ignored.
			name: "manifest lists itself",
			mutate: func(t *testing.T, f *fixture) {
				editManifest(t, f, func(m *manifest.Manifest) {
					m.Files = append(m.Files, manifest.File{
						Path: manifest.FileName, Size: 1, SHA256: strings.Repeat("0", 64),
					})
				})
			},
			want: []string{ProblemFileMissing},
			checkReport: func(t *testing.T, r *Report) {
				if !hasProblem(r, ProblemFileMissing, manifest.FileName) {
					t.Fatalf("expected file-missing for %s; problems=%v", manifest.FileName, problemKinds(r))
				}
			},
		},
		{
			// A listed file replaced by a directory of the same name. WalkDir
			// reports directories separately and the checker skips them, so
			// the listed path must come out missing - and the smuggled child
			// must come out unexpected. Neither may be silently walked past.
			name: "listed file replaced by a directory",
			mutate: func(t *testing.T, f *fixture) {
				p := filepath.Join(f.dir, "repo", "Packages")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(p, "smuggled"), []byte("hidden"))
			},
			want: []string{ProblemFileMissing, ProblemFileUnexpected},
			checkReport: func(t *testing.T, r *Report) {
				if !hasProblem(r, ProblemFileUnexpected, "repo/Packages/smuggled") {
					t.Fatalf("expected file-unexpected for repo/Packages/smuggled; problems=%v", problemKinds(r))
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			tc.mutate(t, f)
			r, err := verifyFixture(t, f, Options{})
			wantRefusedWith(t, r, err, tc.want...)
			wantOnlyKnownKinds(t, r)
			if tc.checkReport != nil {
				tc.checkReport(t, r)
			}
		})
	}
}

// --- path abuse -------------------------------------------------------

// TestVerify_ManifestPathAbuse fires hostile Path values at the file checker.
//
// Two properties are under test, and both matter:
//
//  1. No traversal. A manifest path is never joined onto the bundle root and
//     opened; the checker only ever walks the tree and looks paths up in a
//     map. So an entry naming something outside the bundle can only ever come
//     out "missing" - it must not be satisfied by a file that really exists
//     out there. Each case plants a canary file in the bundle's parent
//     directory (inside the test's own t.TempDir(), so an escape is contained
//     and observable) and gives the malicious entry the canary's true size and
//     digest: if verify ever resolved the path, the entry would match and the
//     bundle would pass.
//
//  2. No normalisation. "REPO/PACKAGES" and a decomposed-unicode spelling must
//     NOT be folded onto the real repo/Packages entry. Folding would let one
//     on-disk file satisfy two manifest entries, which is how a "covered"
//     bundle quietly stops being covered. The assertion for this is that the
//     legitimate files still match (no file-unexpected appears) while the
//     abusive spelling is reported missing under its own, verbatim, spelling.
//
// wantKind records WHICH layer refuses each spelling, because for most of them
// the answer moved and the difference is worth stating:
//
//   - manifest-malformed: core/manifest.Load's document validation refuses the
//     manifest outright at step 1 - an uncleaned, absolute, backslashed,
//     control-byte-carrying or empty path is not a manifest path at all. The
//     bundle's contents are then never read, so FilesChecked stays 0, which is
//     the strongest possible form of "the path was never resolved".
//   - file-missing: a spelling core/manifest permits, which therefore reaches
//     verify's own file checker and must come out missing under its verbatim
//     spelling.
//
// Both are refusals, and neither may ever be satisfied by the canary.
func TestVerify_ManifestPathAbuse(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantKind string
	}{
		{"parent traversal", "../canary.txt", ProblemManifestMalformed},
		{"parent traversal, backslash", `..\canary.txt`, ProblemManifestMalformed},
		{"deep traversal", "../../canary.txt", ProblemManifestMalformed},
		{"absolute posix path", "/etc/passwd", ProblemManifestMalformed},
		{"windows drive letter", `C:/Windows/win.ini`, ProblemManifestMalformed},
		{"windows drive letter, backslash", `C:\Windows\win.ini`, ProblemManifestMalformed},
		{"unc path", `\\server\share\payload`, ProblemManifestMalformed},
		{"backslash separator", `repo\Packages`, ProblemManifestMalformed},
		{"empty path", "", ProblemManifestMalformed},
		{"dot", ".", ProblemManifestMalformed},
		{"dot-slash prefix", "./repo/Packages", ProblemManifestMalformed},
		{"double slash", "repo//Packages", ProblemManifestMalformed},
		{"embedded dot component", "repo/./Packages", ProblemManifestMalformed},
		{"embedded parent component", "repo/sub/../Packages", ProblemManifestMalformed},
		{"nul byte", "repo/Packages\x00", ProblemManifestMalformed},
		// Trailing space and trailing dot survive path.Clean, so they are
		// legal manifest paths and do reach the file checker. Windows strips
		// both when resolving a name, which is exactly why they must not be
		// folded onto repo/Packages here - core/bundle refuses such a pair on
		// import for the same reason, and verify must not undo that by
		// matching them to a file that has neither.
		{"trailing space", "repo/Packages ", ProblemFileMissing},
		{"trailing dot", "repo/Packages.", ProblemFileMissing},
		{"case-folded collision", "REPO/PACKAGES", ProblemFileMissing},
		// NFD ("e" + combining acute) against a bundle that contains no such
		// file at all. A filesystem or checker that normalised unicode could
		// fold two distinct manifest entries together; this one must simply
		// come out missing.
		{"unicode decomposed", "repo/Pack\u0065\u0301ages", ProblemFileMissing},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)

			// The canary lives in the bundle's parent, which is a
			// t.TempDir() subtree: a traversal that "worked" would land here
			// and nowhere else on the machine.
			canary := []byte("canary: verify must never read outside the bundle")
			canaryPath := filepath.Join(filepath.Dir(f.dir), "canary.txt")
			if err := os.WriteFile(canaryPath, canary, 0o644); err != nil {
				t.Fatal(err)
			}

			editManifest(t, f, func(m *manifest.Manifest) {
				m.Files = append(m.Files, manifest.File{
					Path: tc.path, Size: int64(len(canary)), SHA256: digest.Bytes(canary),
				})
			})

			r, err := verifyFixture(t, f, Options{})
			wantRefusedWith(t, r, err, tc.wantKind)
			wantOnlyKnownKinds(t, r)

			if tc.wantKind == ProblemManifestMalformed {
				// Refused as a document, at step 1. The evidence that the
				// canary was never resolved is stronger here than any
				// assertion about the file checker could be: nothing under
				// the bundle root was opened at all.
				if !hasProblem(r, ProblemManifestMalformed, manifest.FileName) {
					t.Fatalf("expected manifest-malformed naming %s; problems=%v", manifest.FileName, problemKinds(r))
				}
				if r.FilesChecked != 0 || r.BytesChecked != 0 {
					t.Fatalf("a document-level refusal read %d file(s)/%d byte(s) of the bundle", r.FilesChecked, r.BytesChecked)
				}
				return
			}

			// The abusive entry is reported under its own spelling, verbatim:
			// an operator has to be able to see the exact string that was in
			// the signed document.
			if !hasProblem(r, ProblemFileMissing, tc.path) {
				t.Fatalf("expected file-missing carrying the verbatim path %q; problems=%v", tc.path, problemKinds(r))
			}

			// No legitimate file was knocked out of coverage: nothing folded.
			for _, p := range r.Problems {
				if p.Kind == ProblemFileUnexpected {
					t.Fatalf("path %q caused %s to lose its manifest entry - paths are being normalised; problems=%v",
						tc.path, p.Path, problemKinds(r))
				}
			}

			// Exactly one problem: the abusive entry. If verify had resolved
			// the path onto the canary, there would be none at all.
			if len(r.Problems) != 1 {
				t.Fatalf("want exactly one problem (the abusive entry), got %v", problemKinds(r))
			}
		})
	}
}

// TestVerify_DuplicateManifestEntriesAreRefused covers a signed manifest that
// lists one path twice with entries that contradict each other.
//
// This test previously pinned the ONE direction that happened to be refused,
// and recorded the other as a known gap. The gap was real: checkFiles built
// its lookup as expected[f.Path] = f and so took the LAST entry, while
// findFile (behind the repository cross-check) scanned the slice and so took
// the FIRST. Two checks in one file disagreed about what the manifest said,
// and which reading you got decided whether the contradiction was caught - a
// duplicate on repo/Packages was refused, a duplicate on a pool .deb, on
// lock.json or on snapshot.json verified OK with no problem and no warning,
// the losing entry discarded before any check saw it and files_checked quietly
// one short of the manifest's own file count.
//
// It is refused now in both directions, and at two independent layers, which
// is why this asserts the outcome rather than the layer:
//
//   - core/manifest.Load refuses the document outright, so verify's step 1
//     ends it (this is what fires here, and why FilesChecked is 0);
//   - verify's own indexFiles refuses it too, for a document that reached the
//     file checker some other way. That layer is exercised directly by
//     TestIndexFiles below, because this end-to-end route can no longer get
//     past the loader to reach it.
//
// Only the signing key can produce either shape, so this was never remotely
// exploitable. The property is what matters: the manifest is the one signed
// object the whole design rests on, and a document that can be read two ways
// does not have a single meaning to sign.
func TestVerify_DuplicateManifestEntriesAreRefused(t *testing.T) {
	const poolRel = "repo/pool/d/demo/demo_1.0-1_amd64.deb"

	// The bogus copy goes first in one case and last in the other. Under the
	// old code those were entirely different outcomes; the position must not
	// matter at all now.
	for _, tc := range []struct {
		name       string
		bogusFirst bool
	}{
		{"the bogus entry comes first", true},
		{"the bogus entry comes last", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			editManifest(t, f, func(m *manifest.Manifest) {
				var real manifest.File
				for _, x := range m.Files {
					if x.Path == poolRel {
						real = x
					}
				}
				if real.Path == "" {
					t.Fatalf("fixture manifest does not list %s", poolRel)
				}
				bogus := real
				bogus.SHA256 = strings.Repeat("0", 64)
				bogus.Size = 1
				if tc.bogusFirst {
					m.Files = append([]manifest.File{bogus}, m.Files...)
				} else {
					m.Files = append(m.Files, bogus)
				}
			})

			r, err := verifyFixture(t, f, Options{})
			wantRefusedWith(t, r, err, ProblemManifestMalformed)
			wantOnlyKnownKinds(t, r)

			// The refusal names the duplicated path and points at the
			// document carrying it, not at the file on disk: there is nothing
			// wrong with the file.
			var found bool
			for _, pr := range r.Problems {
				if pr.Kind == ProblemManifestMalformed && pr.Path == manifest.FileName && strings.Contains(pr.Message, poolRel) {
					found = true
				}
			}
			if !found {
				t.Fatalf("no manifest-malformed problem names %s; problems=%v", poolRel, problemKinds(r))
			}
			if r.FilesChecked != 0 {
				t.Fatalf("a document-level refusal still hashed %d file(s)", r.FilesChecked)
			}
		})
	}
}

// TestIndexFiles is the unit test for verify's own reading of the manifest's
// file list - the table checkFiles walks against and checkRepository resolves
// through.
//
// It is tested directly rather than through a bundle because core/manifest now
// refuses a duplicated path at load time, so no bundle can carry one this far.
// verify keeps its own check regardless, and the reason is the one that
// applies to everything at a trust boundary: this package is the gate in front
// of apt, and a gate that is only correct because some other package validated
// its input has no property of its own. core/manifest is a separate package
// with a separate owner and its own release cadence.
//
// Two properties, and they are the two the old code got wrong:
//
//  1. One table. There is exactly one answer to "what does the manifest say
//     about this path", and both callers get it from here. The old code had
//     two resolvers with opposite tie-breaks.
//  2. A duplicate is named, once, in sorted order - so the report is
//     deterministic, like every other output this project produces.
func TestIndexFiles(t *testing.T) {
	mk := func(path, sha string) manifest.File {
		return manifest.File{Path: path, Size: 1, SHA256: sha}
	}
	real := strings.Repeat("a", 64)
	bogus := strings.Repeat("0", 64)

	t.Run("no duplicates", func(t *testing.T) {
		idx, dup := indexFiles([]manifest.File{mk("b", real), mk("a", real)})
		if len(dup) != 0 {
			t.Fatalf("duplicates = %v, want none", dup)
		}
		if len(idx) != 2 {
			t.Fatalf("index has %d entries, want 2", len(idx))
		}
	})

	t.Run("a duplicated path is named and resolved first-wins", func(t *testing.T) {
		idx, dup := indexFiles([]manifest.File{mk("x", real), mk("x", bogus)})
		if len(dup) != 1 || dup[0] != "x" {
			t.Fatalf("duplicates = %v, want [x]", dup)
		}
		if got := idx["x"].SHA256; got != real {
			t.Fatalf("index resolved x to %q, want the first entry %q", got, real)
		}
	})

	t.Run("the tie-break does not depend on the order", func(t *testing.T) {
		// The reverse of the case above. Whichever entry wins, the duplicate
		// must be reported - that is what makes the tie-break arbitrary
		// rather than load-bearing, and it is exactly the direction that
		// used to be accepted silently.
		_, dup := indexFiles([]manifest.File{mk("x", bogus), mk("x", real)})
		if len(dup) != 1 || dup[0] != "x" {
			t.Fatalf("duplicates = %v, want [x]", dup)
		}
	})

	t.Run("a path listed three times is named once, and the list is sorted", func(t *testing.T) {
		_, dup := indexFiles([]manifest.File{
			mk("z", real), mk("z", bogus), mk("z", real),
			mk("a", real), mk("a", bogus),
		})
		if len(dup) != 2 || dup[0] != "a" || dup[1] != "z" {
			t.Fatalf("duplicates = %v, want [a z] exactly once each, sorted", dup)
		}
	})
}

// --- digest abuse -----------------------------------------------------

// TestVerify_DigestAbuse attacks the digest field itself rather than the file
// it describes. Every malformed spelling must fail closed: a digest that
// cannot be compared is not a digest that matches.
//
// wantKind again records which layer refuses, and the split is the whole
// point. core/manifest.Load now requires every sha256 to be 64 lowercase hex
// characters, so a digest that is the wrong length, padded, empty or not hex
// takes the manifest out at step 1 - it never reaches a comparison at all,
// which is a stronger refusal than a mismatch. Only a well-formed digest that
// happens to be the wrong value reaches verify's own comparison.
//
// The uppercase case is the one that changed meaning, and it is kept rather
// than deleted because the change deserves to be visible: core/digest.Equal
// still folds case by contract, but a manifest can no longer carry an
// uppercase digest to exercise it, so the fold is now unreachable through a
// bundle. That is a tightening, not a break - no bundle debark builds has
// ever contained one - but if the loader's rule is ever relaxed, this case
// will need to say ProblemFileDigest-or-OK again, deliberately.
func TestVerify_DigestAbuse(t *testing.T) {
	const target = "repo/Packages"

	cases := []struct {
		name     string
		mut      func(fl *manifest.File)
		wantKind string
	}{
		{"truncated to 32 hex chars", func(fl *manifest.File) { fl.SHA256 = fl.SHA256[:32] }, ProblemManifestMalformed},
		{"empty string", func(fl *manifest.File) { fl.SHA256 = "" }, ProblemManifestMalformed},
		{"whitespace padded", func(fl *manifest.File) { fl.SHA256 = " " + fl.SHA256 + " " }, ProblemManifestMalformed},
		{"doubled to 128 chars", func(fl *manifest.File) { fl.SHA256 = fl.SHA256 + fl.SHA256 }, ProblemManifestMalformed},
		{"non-hex characters", func(fl *manifest.File) { fl.SHA256 = strings.Repeat("z", 64) }, ProblemManifestMalformed},
		{"uppercase hex", func(fl *manifest.File) { fl.SHA256 = strings.ToUpper(fl.SHA256) }, ProblemManifestMalformed},
		// Well-formed, and simply not this file's digest: the one case that
		// reaches the comparison verify actually performs.
		{"all zeroes", func(fl *manifest.File) { fl.SHA256 = strings.Repeat("0", 64) }, ProblemFileDigest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			editManifest(t, f, func(m *manifest.Manifest) {
				for i := range m.Files {
					if m.Files[i].Path == target {
						tc.mut(&m.Files[i])
						// Keep the Repository summary in step with the Files
						// entry, so the only thing that can fail is the
						// on-disk digest comparison and not checkRepository's
						// separate internal-consistency check.
						m.Repository.PackagesSHA256 = m.Files[i].SHA256
					}
				}
			})

			r, err := verifyFixture(t, f, Options{})
			wantRefusedWith(t, r, err, tc.wantKind)
			wantOnlyKnownKinds(t, r)
			switch tc.wantKind {
			case ProblemManifestMalformed:
				if !hasProblem(r, ProblemManifestMalformed, manifest.FileName) {
					t.Fatalf("expected manifest-malformed naming %s; problems=%v", manifest.FileName, problemKinds(r))
				}
				if r.FilesChecked != 0 {
					t.Fatalf("a document-level refusal still hashed %d file(s)", r.FilesChecked)
				}
			default:
				if !hasProblem(r, ProblemFileDigest, target) {
					t.Fatalf("expected file-digest-mismatch for %s; problems=%v", target, problemKinds(r))
				}
			}
		})
	}
}

// TestVerify_CorrectDigestOnTheWrongFile swaps two files' digests inside the
// manifest, leaving each entry's true size in place. Both digests are real,
// current, correct SHA-256 values of files that really are in this bundle -
// they are simply attached to the wrong paths. A checker that only asked "is
// this digest one of the digests we expect" instead of "is this digest THIS
// file's digest" would pass.
func TestVerify_CorrectDigestOnTheWrongFile(t *testing.T) {
	f := buildFixture(t)
	editManifest(t, f, func(m *manifest.Manifest) {
		a, b := -1, -1
		for i := range m.Files {
			switch m.Files[i].Path {
			case "repo/Packages":
				a = i
			case "repo/Release":
				b = i
			}
		}
		if a < 0 || b < 0 {
			t.Fatal("fixture manifest is missing repo/Packages or repo/Release")
		}
		m.Files[a].SHA256, m.Files[b].SHA256 = m.Files[b].SHA256, m.Files[a].SHA256
		// Sizes stay truthful so the cheaper size check cannot mask the
		// digest check, and the Repository summary follows the Files entries
		// so checkRepository stays internally consistent: the digest
		// comparison against the bytes on disk is the only thing left.
		m.Repository.PackagesSHA256 = m.Files[a].SHA256
		m.Repository.ReleaseSHA256 = m.Files[b].SHA256
	})

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemFileDigest)
	for _, p := range []string{"repo/Packages", "repo/Release"} {
		if !hasProblem(r, ProblemFileDigest, p) {
			t.Fatalf("expected file-digest-mismatch for %s; problems=%v", p, problemKinds(r))
		}
	}
}

// TestVerify_OnDiskTamperOfIndexAndSidecars corrupts the actual files on the
// medium, as opposed to the manifest's description of them.
//
// The existing matrix's "corrupted Packages/Release" and "lock/snapshot digest
// mismatch" cases all edit the MANIFEST and re-sign; they prove the manifest
// is internally consistent with itself, not that a tampered file on disk is
// caught. These four cases are the on-disk counterparts, and they assert both
// findings where two independent checks should fire: for lock.json and
// snapshot.json the file-level digest AND the manifest's own binding digest,
// which are computed by different code paths from different bytes.
func TestVerify_OnDiskTamperOfIndexAndSidecars(t *testing.T) {
	cases := []struct {
		name  string
		file  string
		kinds []string
	}{
		{"repo/Packages, the index apt reads", "repo/Packages", []string{ProblemFileDigest}},
		{"repo/Release", "repo/Release", []string{ProblemFileDigest}},
		{"lock.json", "lock.json", []string{ProblemFileDigest, ProblemLockDigest}},
		{"snapshot.json", "snapshot.json", []string{ProblemFileDigest, ProblemSnapshotDigest}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			p := filepath.Join(f.dir, tc.file)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			b = append([]byte(nil), b...)
			// Change one byte to another printable byte: same length, so only
			// content hashing can catch it, and still valid JSON for the two
			// sidecars so the canonicalising digest path is exercised rather
			// than a parse error.
			for i := range b {
				if b[i] >= 'a' && b[i] <= 'y' {
					b[i]++
					break
				}
			}
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}

			r, verr := verifyFixture(t, f, Options{})
			wantRefusedWith(t, r, verr, tc.kinds...)
			for _, k := range tc.kinds {
				if !hasProblem(r, k, tc.file) {
					t.Fatalf("expected %s for %s; problems=%v", k, tc.file, problemKinds(r))
				}
			}
		})
	}
}

// TestVerify_LockGainsAnUnknownField is the regression test for the reason
// checkLockAndSnapshot re-canonicalises the raw bytes on disk instead of
// leaning on lock.Digest's struct-based canonicalisation. A field Go does not
// know about survives no round trip: it would be dropped on unmarshal and the
// re-marshalled digest would match, hiding the edit. Recomputing from the
// bytes that are actually there catches it.
func TestVerify_LockGainsAnUnknownField(t *testing.T) {
	f := buildFixture(t)
	p := filepath.Join(f.dir, "lock.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimSpace(string(b))
	if !strings.HasSuffix(s, "}") {
		t.Fatalf("lock.json does not end in }: %q", s[max(0, len(s)-40):])
	}
	s = strings.TrimSuffix(s, "}") + ",\n  \"debark_unknown_field\": \"payload\"\n}\n"
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}

	r, verr := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, verr, ProblemLockDigest)
	if !hasProblem(r, ProblemLockDigest, "lock.json") {
		t.Fatalf("expected lock-digest-mismatch for lock.json; problems=%v", problemKinds(r))
	}
}

// --- signature abuse --------------------------------------------------

// TestVerify_SignatureAbuse works the third step: everything that can be wrong
// with a signature block, a signature file, or the relationship between a
// signature and the manifest it claims to cover.
func TestVerify_SignatureAbuse(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f *fixture)
		want   string
	}{
		{
			// An empty signatures array in a well-formed signature file is
			// the same state as no signature file at all: unsigned. It must
			// not be mistaken for "checked and fine".
			name: "signature file present with an empty signatures array",
			mutate: func(t *testing.T, f *fixture) {
				sf := loadSig(t, f)
				sf.Signatures = nil
				saveSig(t, f, sf)
			},
			want: ProblemSignatureMissing,
		},
		{
			name: "empty signature string",
			mutate: func(t *testing.T, f *fixture) {
				sf := loadSig(t, f)
				sf.Signatures[0].Signature = ""
				saveSig(t, f, sf)
			},
			want: ProblemSignatureInvalid,
		},
		{
			name: "truncated signature",
			mutate: func(t *testing.T, f *fixture) {
				sf := loadSig(t, f)
				s := sf.Signatures[0].Signature
				sf.Signatures[0].Signature = s[:len(s)/2]
				saveSig(t, f, sf)
			},
			want: ProblemSignatureInvalid,
		},
		{
			name: "signature is not base64",
			mutate: func(t *testing.T, f *fixture) {
				sf := loadSig(t, f)
				sf.Signatures[0].Signature = "!!! not base64 at all !!!"
				saveSig(t, f, sf)
			},
			want: ProblemSignatureInvalid,
		},
		{
			// Valid base64 of the wrong length. ed25519.Verify would panic or
			// misbehave on a short signature, so the length check has to come
			// first - and its failure has to be a tamper signal, not a crash.
			name: "well-formed base64 of the wrong length",
			mutate: func(t *testing.T, f *fixture) {
				sf := loadSig(t, f)
				sf.Signatures[0].Signature = base64.StdEncoding.EncodeToString([]byte("far too short"))
				saveSig(t, f, sf)
			},
			want: ProblemSignatureInvalid,
		},
		{
			// A signer kind this build has no verifier for. There is no
			// "unknown algorithm, assume fine" branch: an unrecognised kind
			// is an untrusted key.
			name: "unrecognised signer kind",
			mutate: func(t *testing.T, f *fixture) {
				sf := loadSig(t, f)
				sf.Signatures[0].SignerKind = manifest.SignerSigstore
				sf.Signatures[0].Algorithm = "cosign-bundle"
				saveSig(t, f, sf)
			},
			want: ProblemSignatureUntrusted,
		},
		{
			name: "plugin signer kind nobody trusted",
			mutate: func(t *testing.T, f *fixture) {
				sf := loadSig(t, f)
				sf.Signatures[0].SignerKind = manifest.SignerPluginPrefix + "attacker"
				sf.Signatures[0].Algorithm = "rot13"
				saveSig(t, f, sf)
			},
			want: ProblemSignatureUntrusted,
		},
		{
			// A real ed25519 signature relabelled as belonging to some other
			// key id. The signature bytes are genuine; the claim about which
			// key made them is not, and the trust set is keyed by key id.
			name: "signature claims an unknown key id",
			mutate: func(t *testing.T, f *fixture) {
				sf := loadSig(t, f)
				sf.Signatures[0].KeyID = strings.Repeat("f", 16)
				saveSig(t, f, sf)
			},
			want: ProblemSignatureUntrusted,
		},
		{
			// Replay of a whole signature FILE from a different bundle,
			// signed by the same trusted operator key. Everything inside it
			// is internally consistent - it is simply about another manifest.
			// This has to be caught at step 2 (manifest_sha256 against the
			// canonical manifest bytes), before any signature maths runs,
			// which is what the assertion on the kind pins down.
			name: "whole signature file replayed from another bundle",
			mutate: func(t *testing.T, f *fixture) {
				other := buildFixture(t)
				resign(t, other, f.privPath)
				b, err := os.ReadFile(filepath.Join(other.dir, manifest.SigFileName))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.dir, manifest.SigFileName), b, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: ProblemManifestDigest,
		},
		{
			// The manifest is edited AND the signature file's manifest_sha256
			// is refreshed to match it - which any attacker can do, since
			// that field is plaintext and unauthenticated. The matrix's
			// "edited manifest field" case never reaches this: it stops at
			// step 2. This one has to be caught by the actual signature
			// check, and it is the only case in either file that proves the
			// ed25519 verification is load-bearing for a manifest edit.
			name: "manifest edited with manifest_sha256 refreshed to match",
			mutate: func(t *testing.T, f *fixture) {
				m, _, err := manifest.Load(f.dir)
				if err != nil {
					t.Fatal(err)
				}
				m.Target.Arch = "riscv64"
				if err := manifest.Save(f.dir, m); err != nil {
					t.Fatal(err)
				}
				_, canon, err := manifest.Load(f.dir)
				if err != nil {
					t.Fatal(err)
				}
				sf := loadSig(t, f)
				sf.ManifestSHA256 = canonical.DigestBytes(canon)
				saveSig(t, f, sf) // the old signature block is left in place
			},
			want: ProblemSignatureInvalid,
		},
		{
			// Both blocks are from the trusted operator key: one genuinely
			// over this manifest, one over another bundle's. A verifier that
			// stopped at the first success would pass this. It must not: a
			// signature that does not verify is evidence of tampering
			// regardless of what else is in the file, so the invalid block
			// has to win over the valid one.
			name: "a valid signature alongside an invalid one from the same trusted key",
			mutate: func(t *testing.T, f *fixture) {
				other := buildFixture(t)
				resign(t, other, f.privPath)
				otherSig, err := manifest.LoadSignature(other.dir)
				if err != nil {
					t.Fatal(err)
				}
				sf := loadSig(t, f)
				sf.Signatures = append(sf.Signatures, otherSig.Signatures...)
				saveSig(t, f, sf)
			},
			want: ProblemSignatureInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			tc.mutate(t, f)
			r, err := verifyFixture(t, f, Options{})
			wantRefusedWith(t, r, err, tc.want)
			wantOnlyKnownKinds(t, r)
		})
	}
}

// TestVerify_ManifestAndSignatureReplayedTogether swaps in another bundle's
// manifest AND its matching signature file. Steps 1 to 3 all pass: the
// document parses, its manifest_sha256 is right for it, and it carries a valid
// signature from a key this verifier trusts. The bundle is still not that
// bundle, and only the content checks can say so.
func TestVerify_ManifestAndSignatureReplayedTogether(t *testing.T) {
	f := buildFixture(t)
	other := buildFixture(t)
	resign(t, other, f.privPath)

	for _, n := range []string{manifest.FileName, manifest.SigFileName} {
		b, err := os.ReadFile(filepath.Join(other.dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.dir, n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemFileDigest, ProblemLockDigest)
	// The signature itself is genuinely fine - the report must say so, so an
	// operator can tell "signed by someone I trust, about a different bundle"
	// apart from "forged".
	if !r.Signed || len(r.Signatures) != 1 || !r.Signatures[0].Valid || !r.Signatures[0].Trusted {
		t.Fatalf("expected a valid, trusted signature over the replayed manifest: signed=%v sigs=%+v", r.Signed, r.Signatures)
	}
}

// TestVerify_UnverifiableSignatureIsNeverAPass covers the branch where the
// verifier cannot even attempt the check - here, a gpg-kind signature block on
// a machine with no gpg on PATH.
//
// The distinction it protects: "I checked and it failed" is a Problem in the
// report (dferr.Verification, exit 4, at the CLI). "I could not check" is an
// environment error (exit 2). What it must never become is "no problem
// recorded, so OK".
func TestVerify_UnverifiableSignatureIsNeverAPass(t *testing.T) {
	f := buildFixture(t) // built before PATH is emptied; nothing here execs
	sf := loadSig(t, f)
	sf.Signatures[0].SignerKind = manifest.SignerGPG
	saveSig(t, f, sf)

	// An empty directory as the entire PATH: exec.LookPath("gpg") must fail.
	t.Setenv("PATH", t.TempDir())

	r, err := New().Verify(context.Background(), f.dir, Options{Keys: sign.KeySource{Files: []string{f.pubPath}}})
	if err == nil {
		t.Fatalf("Verify returned no error for an uncheckable signature; report=%+v", r)
	}
	if r != nil && r.OK {
		t.Fatalf("report.OK = true for an uncheckable signature")
	}
	if got := dferr.ClassOf(err); got != dferr.Environment {
		t.Fatalf("dferr class = %d (%s), want %d (%s): %v",
			got, got, dferr.Environment, dferr.Environment, err)
	}
}

// TestVerify_AllowUnsignedDoesNotLaunderTamper is the boundary of the
// --allow-unsigned escape hatch. It means "I accept a bundle nobody vouched
// for"; it must never be readable as "I accept a bundle whose signature is
// demonstrably wrong". A signature from a trusted key that does not verify is
// the single clearest tamper signal there is, and AllowUnsigned has to lose to
// it.
func TestVerify_AllowUnsignedDoesNotLaunderTamper(t *testing.T) {
	t.Run("invalid signature from a trusted key still refuses", func(t *testing.T) {
		f := buildFixture(t)
		other := buildFixture(t)
		resign(t, other, f.privPath)
		otherSig, err := manifest.LoadSignature(other.dir)
		if err != nil {
			t.Fatal(err)
		}
		sf := loadSig(t, f)
		sf.Signatures = otherSig.Signatures
		saveSig(t, f, sf)

		r, verr := New().Verify(context.Background(), f.dir, Options{
			AllowUnsigned: true,
			Keys:          sign.KeySource{Files: []string{f.pubPath}},
		})
		wantRefusedWith(t, r, verr, ProblemSignatureInvalid)
	})

	t.Run("signature from an untrusted key is accepted, unsigned and warned", func(t *testing.T) {
		// This is the documented behaviour: with AllowUnsigned, a signature
		// nobody trusts is worth exactly as much as no signature. What the
		// report must never do is call the bundle Signed on that basis - the
		// caller has to be able to tell the operator the truth.
		f := buildFixture(t)
		resign(t, f, f.attackerPriv)

		r, err := New().Verify(context.Background(), f.dir, Options{
			AllowUnsigned: true,
			Keys:          sign.KeySource{Files: []string{f.pubPath}},
		})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !r.OK {
			t.Fatalf("report not OK: problems=%v", problemKinds(r))
		}
		if r.Signed {
			t.Fatal("report.Signed = true for a bundle signed only by an untrusted key")
		}
		if !hasWarningContaining(r, "no signature verifies against a trusted key") {
			t.Fatalf("expected a warning saying no trusted signature verified; warnings=%v", r.Warnings)
		}
		if len(r.Signatures) != 1 || r.Signatures[0].Valid || r.Signatures[0].Trusted {
			t.Fatalf("signature result should record valid=false trusted=false: %+v", r.Signatures)
		}
	})
}

// TestVerify_SameMediaKeySpellings widens the existing same-media case, which
// only covers a key file at the bundle root reached by a plain path. The
// refusal is worth nothing if it can be walked around by spelling the same
// location differently, so each of these points at a key that really is inside
// the bundle and must be refused identically.
func TestVerify_SameMediaKeySpellings(t *testing.T) {
	cases := []struct {
		name string
		// keys builds the KeySource, and may plant files inside f.dir.
		keys func(t *testing.T, f *fixture) sign.KeySource
		// bundleArg lets a case pass a differently-spelled bundle path.
		bundleArg func(f *fixture) string
	}{
		{
			name: "keyring directory inside the bundle",
			keys: func(t *testing.T, f *fixture) sign.KeySource {
				inner := filepath.Join(f.dir, "keys")
				src, err := os.ReadFile(f.pubPath)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(inner, "operator.pub"), src)
				return sign.KeySource{Dirs: []string{inner}}
			},
		},
		{
			name: "key inside the bundle reached through a .. component",
			keys: func(t *testing.T, f *fixture) sign.KeySource {
				src, err := os.ReadFile(f.pubPath)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(f.dir, "operator.pub"), src)
				// Same file, spelled as a detour through repo/.
				return sign.KeySource{Files: []string{filepath.Join(f.dir, "repo", "..", "operator.pub")}}
			},
		},
		{
			name: "gpg keyring file inside the bundle",
			keys: func(t *testing.T, f *fixture) sign.KeySource {
				writeFile(t, filepath.Join(f.dir, "release.gpg"), []byte("not a real keyring"))
				return sign.KeySource{GPGKeyring: filepath.Join(f.dir, "release.gpg")}
			},
		},
		{
			name: "bundle path given with a trailing separator",
			keys: func(t *testing.T, f *fixture) sign.KeySource {
				src, err := os.ReadFile(f.pubPath)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(f.dir, "operator.pub"), src)
				return sign.KeySource{Files: []string{filepath.Join(f.dir, "operator.pub")}}
			},
			bundleArg: func(f *fixture) string { return f.dir + string(filepath.Separator) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			ks := tc.keys(t, f)
			arg := f.dir
			if tc.bundleArg != nil {
				arg = tc.bundleArg(f)
			}
			r, err := New().Verify(context.Background(), arg, Options{Keys: ks})
			wantRefusedWith(t, r, err, ProblemSameMediaKey)
		})
	}
}

// --- malformed documents ----------------------------------------------

// TestVerify_MalformedDocuments checks that every document verify reads is
// refused when it cannot be read, and that "unreadable" is reported
// differently from "readable, but a schema version I do not recognise" - the
// operator's next action is different (a corrupt medium vs the wrong debark
// build), and iface.go freezes two distinct kinds precisely so a script can
// tell them apart.
func TestVerify_MalformedDocuments(t *testing.T) {
	cases := []struct {
		name string
		// prepare damages the bundle; nothing is re-signed, because these
		// failures must be reached before any signature check.
		prepare func(t *testing.T, f *fixture)
		want    string
		wantAt  string
	}{
		{
			name: "manifest is not JSON",
			prepare: func(t *testing.T, f *fixture) {
				writeFile(t, filepath.Join(f.dir, manifest.FileName), []byte("{ this is not json"))
			},
			want: ProblemManifestMalformed, wantAt: manifest.FileName,
		},
		{
			name: "manifest carries an unknown schema version",
			prepare: func(t *testing.T, f *fixture) {
				writeFile(t, filepath.Join(f.dir, manifest.FileName), []byte(`{"schema_version":"debark.manifest/v99"}`))
			},
			want: ProblemSchemaUnknown, wantAt: manifest.FileName,
		},
		{
			name: "signature file is not JSON",
			prepare: func(t *testing.T, f *fixture) {
				writeFile(t, filepath.Join(f.dir, manifest.SigFileName), []byte("{ this is not json"))
			},
			want: ProblemManifestMalformed, wantAt: manifest.SigFileName,
		},
		{
			name: "signature file carries an unknown schema version",
			prepare: func(t *testing.T, f *fixture) {
				writeFile(t, filepath.Join(f.dir, manifest.SigFileName), []byte(`{"schema":"debark.signature/v99"}`))
			},
			want: ProblemSchemaUnknown, wantAt: manifest.SigFileName,
		},
		{
			name: "manifest absent",
			prepare: func(t *testing.T, f *fixture) {
				if err := os.Remove(filepath.Join(f.dir, manifest.FileName)); err != nil {
					t.Fatal(err)
				}
			},
			want: ProblemManifestMissing, wantAt: manifest.FileName,
		},
		{
			// lock.json is digested by re-canonicalising its bytes, so a
			// document that will not canonicalise has to be a refusal and not
			// an ignored error.
			name: "lock.json will not canonicalise",
			prepare: func(t *testing.T, f *fixture) {
				writeFile(t, filepath.Join(f.dir, "lock.json"), []byte("{ this is not json"))
			},
			want: ProblemLockDigest, wantAt: "lock.json",
		},
		{
			name: "snapshot.json will not canonicalise",
			prepare: func(t *testing.T, f *fixture) {
				writeFile(t, filepath.Join(f.dir, "snapshot.json"), []byte("{ this is not json"))
			},
			want: ProblemSnapshotDigest, wantAt: "snapshot.json",
		},
		{
			// A directory where a document should be: os.ReadFile fails
			// rather than returning bytes. The failure must surface as the
			// digest problem for that document, not be swallowed.
			name: "lock.json replaced by a directory",
			prepare: func(t *testing.T, f *fixture) {
				p := filepath.Join(f.dir, "lock.json")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: ProblemLockDigest, wantAt: "lock.json",
		},
		{
			name: "snapshot.json replaced by a directory",
			prepare: func(t *testing.T, f *fixture) {
				p := filepath.Join(f.dir, "snapshot.json")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: ProblemSnapshotDigest, wantAt: "snapshot.json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			tc.prepare(t, f)
			r, err := verifyFixture(t, f, Options{})
			wantRefusedWith(t, r, err, tc.want)
			wantOnlyKnownKinds(t, r)
			if !hasProblem(r, tc.want, tc.wantAt) {
				t.Fatalf("expected %s at %s; problems=%v", tc.want, tc.wantAt, problemKinds(r))
			}
		})
	}
}

// TestVerify_RepositoryDigestReferencesUnlistedFile covers the other half of
// checkRepository: a Repository summary digest naming a file the manifest does
// not list at all. It is the shape a partially-written or partially-stripped
// bundle takes - the index summary still claims Packages.gz and InRelease
// exist, and nothing in Files backs the claim.
func TestVerify_RepositoryDigestReferencesUnlistedFile(t *testing.T) {
	f := buildFixture(t)
	editManifest(t, f, func(m *manifest.Manifest) {
		m.Repository.PackagesGzSHA256 = strings.Repeat("3", 64)
		m.Repository.InReleaseSHA256 = strings.Repeat("4", 64)
		m.Repository.ReleaseGPGSHA256 = strings.Repeat("5", 64)
	})

	r, err := verifyFixture(t, f, Options{})
	wantRefusedWith(t, r, err, ProblemRepoDigest)
	for _, p := range []string{"repo/Packages.gz", "repo/InRelease", "repo/Release.gpg"} {
		if !hasProblem(r, ProblemRepoDigest, p) {
			t.Fatalf("expected repo-digest-mismatch for %s; problems=%v", p, problemKinds(r))
		}
	}
}

// --- ordering, read-only, and error classification --------------------

// TestVerify_HardFailureStopsBeforeTheContentsAreTouched is the ordering
// proof available inside this package.
//
// verify is the gate that runs before install exposes repo/ to apt. Two things
// have to be true for that gate to mean anything, and both are checked here
// against real bundles:
//
//   - A hard failure in steps 1 to 3 aborts immediately. The evidence is
//     FilesChecked and BytesChecked still being zero: not one byte of the
//     repository was read, let alone published, so there is no window in which
//     a caller could have acted on a partial result.
//   - Verification is read-only. The bundle tree is byte-for-byte identical
//     afterwards - on the failing paths and on the passing one. verify never
//     stages, unpacks, rewrites or "repairs" anything, so a bundle that failed
//     verification is left exactly as the auditor found it.
//
// What this package cannot prove is the step after: that install refuses to
// write a sources.list entry when the report is not OK. That belongs to
// core/install, whose runner_test.go drives it with a failing report.
func TestVerify_HardFailureStopsBeforeTheContentsAreTouched(t *testing.T) {
	cases := []struct {
		name    string
		step    string
		mutate  func(t *testing.T, f *fixture)
		want    string
		wantSig int // expected len(report.Signatures)
	}{
		{
			name: "step 1: no manifest",
			step: "manifest parse",
			mutate: func(t *testing.T, f *fixture) {
				if err := os.Remove(filepath.Join(f.dir, manifest.FileName)); err != nil {
					t.Fatal(err)
				}
			},
			want: ProblemManifestMissing, wantSig: 0,
		},
		{
			name: "step 2: signature file describes another manifest",
			step: "manifest_sha256",
			mutate: func(t *testing.T, f *fixture) {
				sf := loadSig(t, f)
				sf.ManifestSHA256 = strings.Repeat("9", 64)
				saveSig(t, f, sf)
			},
			want: ProblemManifestDigest, wantSig: 0,
		},
		{
			name: "step 3: signed by a key nobody trusts",
			step: "signature",
			mutate: func(t *testing.T, f *fixture) {
				resign(t, f, f.attackerPriv)
			},
			// One signature RESULT is expected here: the block was examined
			// and rejected, which is information the operator needs.
			want: ProblemSignatureUntrusted, wantSig: 1,
		},
		{
			name: "step 3: not signed at all",
			step: "signature",
			mutate: func(t *testing.T, f *fixture) {
				if err := os.Remove(filepath.Join(f.dir, manifest.SigFileName)); err != nil {
					t.Fatal(err)
				}
			},
			want: ProblemSignatureMissing, wantSig: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFixture(t)
			tc.mutate(t, f)

			before := treeFingerprint(t, f.dir)
			r, err := verifyFixture(t, f, Options{})
			after := treeFingerprint(t, f.dir)

			wantRefusedWith(t, r, err, tc.want)
			if r.FilesChecked != 0 || r.BytesChecked != 0 {
				t.Fatalf("verification failed at the %s step but read %d file(s)/%d byte(s) of the bundle; the abort is not before the contents",
					tc.step, r.FilesChecked, r.BytesChecked)
			}
			if len(r.Signatures) != tc.wantSig {
				t.Fatalf("len(report.Signatures) = %d, want %d", len(r.Signatures), tc.wantSig)
			}
			if before != after {
				t.Fatalf("verify modified the bundle it was checking\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}

	t.Run("a passing verification also leaves the bundle untouched", func(t *testing.T) {
		f := buildFixture(t)
		// Fingerprint the whole fixture root, not just the bundle: this also
		// catches a stray temp or state file written next to it.
		root := filepath.Dir(f.dir)
		before := treeFingerprint(t, root)
		r, err := verifyFixture(t, f, Options{})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !r.OK {
			t.Fatalf("report not OK: problems=%v", problemKinds(r))
		}
		if r.FilesChecked == 0 || r.BytesChecked == 0 {
			t.Fatalf("a passing verification must have read the bundle: FilesChecked=%d BytesChecked=%d", r.FilesChecked, r.BytesChecked)
		}
		if after := treeFingerprint(t, root); before != after {
			t.Fatalf("verify wrote to the filesystem during a successful check\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
}

// TestVerify_ErrorClassification pins the two-channel contract verify has with
// its caller, because the CLI turns each channel into a different process exit
// code from the frozen table in core/dferr (ADR-012):
//
//   - Evidence about the bundle travels in the Report. Verify returns a nil Go
//     error, and cmd_verify maps !report.OK onto dferr.Verification (4).
//   - Conditions that are NOT evidence about the bundle - an unusable path, an
//     unreadable trust anchor, a checker this machine cannot run - travel as a
//     classified Go error, and the CLI returns that class instead.
//
// A refusal delivered under the wrong channel or the wrong class misleads an
// operator's automation, which is the whole reason the table is frozen.
func TestVerify_ErrorClassification(t *testing.T) {
	t.Run("bundle path does not exist", func(t *testing.T) {
		f := buildFixture(t)
		_, err := New().Verify(context.Background(), filepath.Join(f.dir, "no-such-bundle"), Options{})
		wantClass(t, err, dferr.Usage)
	})

	t.Run("bundle path is a file, not a directory", func(t *testing.T) {
		f := buildFixture(t)
		_, err := New().Verify(context.Background(), filepath.Join(f.dir, "lock.json"), Options{})
		wantClass(t, err, dferr.Usage)
	})

	t.Run("trusted key file cannot be read", func(t *testing.T) {
		// The operator pointed --key at something that is not there. This is
		// not evidence about the bundle: verify must say so as a classified
		// error rather than quietly proceeding with an empty trust set and
		// reporting the bundle "untrusted", which would send the operator
		// looking at the medium instead of at their own command line.
		f := buildFixture(t)
		missing := filepath.Join(filepath.Dir(f.dir), "not-a-key.pub")
		r, err := New().Verify(context.Background(), f.dir, Options{Keys: sign.KeySource{Files: []string{missing}}})
		wantClass(t, err, dferr.Usage)
		if r != nil && r.OK {
			t.Fatal("report.OK = true when the trust anchor could not be read")
		}
	})

	t.Run("an empty trust set fails closed", func(t *testing.T) {
		// No --key and no --keyring at all against a signed bundle. There is
		// nothing wrong with the operator's command line here, so this one is
		// evidence about the bundle: reported, not errored, and refused.
		f := buildFixture(t)
		r, err := New().Verify(context.Background(), f.dir, Options{})
		wantRefusedWith(t, r, err, ProblemSignatureUntrusted)
	})

	t.Run("every tamper refusal uses the report channel and a frozen kind", func(t *testing.T) {
		// One representative tamper per step, asserted as a set: a nil Go
		// error, OK false, and only kinds iface.go freezes. This is the
		// property cmd_verify relies on when it maps !OK onto exit 4.
		mutations := map[string]func(t *testing.T, f *fixture){
			"file digest": func(t *testing.T, f *fixture) {
				p := filepath.Join(f.dir, "repo", "Packages")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				b = append([]byte(nil), b...)
				b[0] ^= 0xFF
				if err := os.WriteFile(p, b, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			"untrusted signature": func(t *testing.T, f *fixture) { resign(t, f, f.attackerPriv) },
			"malformed manifest": func(t *testing.T, f *fixture) {
				writeFile(t, filepath.Join(f.dir, manifest.FileName), []byte("nope"))
			},
			"unexpected file": func(t *testing.T, f *fixture) {
				writeFile(t, filepath.Join(f.dir, "repo", "extra"), []byte("x"))
			},
			"lock digest": func(t *testing.T, f *fixture) {
				editManifest(t, f, func(m *manifest.Manifest) { m.LockDigest = strings.Repeat("7", 64) })
			},
		}
		for name, mutate := range mutations {
			t.Run(name, func(t *testing.T) {
				f := buildFixture(t)
				mutate(t, f)
				r, err := verifyFixture(t, f, Options{})
				if err != nil {
					t.Fatalf("tamper reported as a Go error (class %d) instead of a report problem: %v", dferr.ClassOf(err), err)
				}
				if r.OK {
					t.Fatalf("report.OK = true for a tampered bundle")
				}
				if len(r.Problems) == 0 {
					t.Fatal("report is not OK but carries no problems; nothing tells the operator what is wrong")
				}
				wantOnlyKnownKinds(t, r)
			})
		}
	})
}

func wantClass(t *testing.T, err error, want dferr.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error of class %d (%s), got nil", want, want)
	}
	if got := dferr.ClassOf(err); got != want {
		t.Fatalf("dferr class = %d (%s), want %d (%s): %v", got, got, want, want, err)
	}
}

// --- symlinks ---------------------------------------------------------

// TestVerify_SymlinkedBundleEntries checks the two symlink shapes that could
// let a bundle describe something other than what it carries. Creating
// symlinks needs a privilege Windows does not grant by default, so the test
// probes first and skips rather than failing for an environment reason.
//
// Both cases must be refused, and the assertion is deliberately on the path
// rather than the exact kind: what matters is that a listed path backed by a
// symlink verify cannot read is never silently satisfied.
func TestVerify_SymlinkedBundleEntries(t *testing.T) {
	if !symlinksAvailable(t) {
		t.Skip("this environment does not permit creating symlinks; skipping")
	}

	t.Run("listed file replaced by a dangling symlink", func(t *testing.T) {
		f := buildFixture(t)
		p := filepath.Join(f.dir, "repo", "Packages")
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(f.dir, "does-not-exist"), p); err != nil {
			t.Fatal(err)
		}
		r, err := verifyFixture(t, f, Options{})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if r.OK {
			t.Fatal("a dangling symlink at a manifest-listed path must not verify")
		}
		if !problemMentions(r, "repo/Packages") {
			t.Fatalf("no problem names repo/Packages; problems=%v", problemKinds(r))
		}
	})

	t.Run("listed file replaced by a symlink to a directory", func(t *testing.T) {
		f := buildFixture(t)
		p := filepath.Join(f.dir, "repo", "Packages")
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(f.dir, "repo", "pool"), p); err != nil {
			t.Fatal(err)
		}
		r, err := verifyFixture(t, f, Options{})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if r.OK {
			t.Fatal("a symlink to a directory at a manifest-listed path must not verify")
		}
		if !problemMentions(r, "repo/Packages") {
			t.Fatalf("no problem names repo/Packages; problems=%v", problemKinds(r))
		}
	})
}

func problemMentions(r *Report, path string) bool {
	for _, p := range r.Problems {
		if filepath.ToSlash(p.Path) == path {
			return true
		}
	}
	return false
}

func symlinksAvailable(t *testing.T) bool {
	t.Helper()
	d := t.TempDir()
	return os.Symlink(filepath.Join(d, "target"), filepath.Join(d, "link")) == nil
}
