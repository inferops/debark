package verify

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/sign"
)

// snapshotDocumentName mirrors core/snapshot.DocumentName's value,
// "snapshot.json" (frozen in core/snapshot/types.go, ADR-005's bundle
// layout). It is inlined here rather than imported so this package's build never depends on
// core/snapshot's compile state - core/snapshot is owned by a different,
// concurrently-developed package, and verify's only use of it is this one fixed
// filename.
const snapshotDocumentName = "snapshot.json"

// repoDirName mirrors core/bundle.RepoDir, inlined for the reason
// snapshotDocumentName is: this package must not take a compile dependency on
// core/bundle for one fixed name.
const repoDirName = "repo"

// notABundleHint is the operator-facing advice on every "you did not give me
// a bundle" refusal.
const notABundleHint = "`debark build` writes a bundle; point verify at the directory it names, not at the media root or a folder beside it"

// looksLikeBundle reports whether root has any of a bundle's structural
// parts. It is the same question core/snapshot.looksLikeZstd asks, in the
// same place and for the same reason: an input has to be SHAPED like the
// thing before a failed check on it can honestly be called a verification
// failure.
//
// The manifest is listed first and is by itself sufficient — a directory
// with a manifest is a bundle, whatever else is wrong with it. The rest
// exist so that a REAL bundle whose manifest has been deleted still reaches
// the manifest-missing problem and still exits 4. That is the case exit 4 is
// for: someone removed the one file that says what the bundle should
// contain, which is precisely what tampering looks like. Only a directory
// carrying none of these parts is "not a bundle".
//
// README.txt is deliberately not a marker. Every third directory on a USB
// stick has one, and a marker that fires on unrelated folders would put the
// alarming class back on exactly the mistake this refusal exists to catch.
func looksLikeBundle(root string) bool {
	for _, name := range []string{
		manifest.FileName,
		manifest.SigFileName,
		lock.FileName,
		snapshotDocumentName,
		repoDirName,
	} {
		if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
			return true
		}
	}
	return false
}

// standardVerifier is the real Verifier New returns. Its Verify method, and
// everything it calls in this file, is the whole of what the product asks an
// operator to trust: it performs NO apt call, NO network call of any kind, and
// NEVER writes to the bundle it is checking - every function below only opens
// files under bundlePath for reading. Keep it that way; a verifier that shells
// out or mutates state is not a verifier.
type standardVerifier struct{}

func (standardVerifier) Verify(ctx context.Context, bundlePath string, opts Options) (*Report, error) {
	return runVerify(ctx, bundlePath, opts)
}

func runVerify(ctx context.Context, bundlePath string, opts Options) (*Report, error) {
	report := &Report{
		SchemaVersion: SchemaVersion,
		CheckedAt:     canonical.Time(time.Now()),
		BundlePath:    bundlePath,
	}

	root, err := filepath.Abs(bundlePath)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "verify: resolve bundle path %s", bundlePath)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "verify: %s", root)
	}
	if !info.IsDir() {
		return nil, dferr.New(dferr.Usage, "verify: %s is not a directory", root)
	}
	// The root is resolved once, here, and every step below uses the resolved
	// form. Without this the two ways this function looks at its own root
	// disagree: os.Stat above follows a symlink, filepath.WalkDir does not -
	// so a symlink to a perfectly good bundle passed the "is a directory"
	// test and was then walked as a single non-directory entry, finding no
	// bundle at all. Reaching a bundle through a symlinked mount point is
	// ordinary operator behaviour and must work; links found INSIDE the tree
	// are a different question, refused by checkFiles.
	//
	// A failure here fails closed. EvalSymlinks has just been given a path
	// os.Stat resolved a moment ago, so it can only fail for a reason the
	// operator needs to hear about rather than have silently worked around.
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "verify: resolve bundle path %s", bundlePath)
	}

	// --- Step 0: is this a bundle at all? ---------------------------------
	//
	// Before this refusal, `debark verify some-folder` on a directory that
	// had never been near debark reported "1 problem: no
	// debark.manifest.json in this bundle" and exited 4. Exit 4 is
	// dferr.Verification, which ADR-012 fixes as "a signature, digest or
	// metadata check did not match" — the code a script escalates on and an
	// operator reads as "the media may have been tampered with". Nothing had
	// been verified. They had picked the wrong folder.
	//
	// This is structurally the same mistake 2d6d5c7 fixed for `snapshot
	// inspect`, and it draws the same boundary rather than a softer one:
	// only input that is not shaped like a bundle becomes Usage. Once it IS
	// shaped like one, every check keeps the class it had — including
	// manifest-missing, which on a directory that really is a bundle means
	// the manifest was removed, and exit 4 is exactly right for that.
	//
	// No report is produced, matching the two refusals above it (a path that
	// does not exist, a path that is not a directory): a document whose
	// entire subject is "can this artifact be trusted" has nothing to say
	// about a directory that is not an artifact.
	if !looksLikeBundle(root) {
		return nil, dferr.New(dferr.Usage, "verify: %s is not a debark bundle: it has no %s and none of a bundle's other parts",
			bundlePath, manifest.FileName).WithHint("%s", notABundleHint)
	}

	// --- Step 1: the manifest parses and its schema version is known. -----
	m, canon, err := manifest.Load(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemManifestMissing, Path: manifest.FileName,
				Message: "no " + manifest.FileName + " in this bundle",
			})
		} else {
			report.Problems = append(report.Problems, classifyManifestErr(err, manifest.FileName))
		}
		return report, nil
	}

	report.BundleID = m.BundleID
	report.CreatedAt = m.CreatedAt
	report.ToolVersion = m.Tool.Version
	report.Edition = m.Tool.Edition
	report.Target = ReportTarget{
		DistroID: m.Target.DistroID, VersionID: m.Target.VersionID,
		Codename: m.Target.Codename, Arch: m.Target.Arch,
		ForeignArchs: m.Target.ForeignArchs,
	}

	sigFile, sigErr := manifest.LoadSignature(root)
	if sigErr != nil {
		report.Problems = append(report.Problems, classifyManifestErr(sigErr, manifest.SigFileName))
		return report, nil
	}

	// --- Step 2: the signature file's manifest_sha256 matches the canonical
	// manifest bytes, before any signature maths runs. -----------------------
	manifestDigest := canonical.DigestBytes(canon)
	if sigFile != nil && !digest.Equal(sigFile.ManifestSHA256, manifestDigest) {
		report.Problems = append(report.Problems, Problem{
			Kind:     ProblemManifestDigest,
			Path:     manifest.SigFileName,
			Message:  manifest.SigFileName + "'s manifest_sha256 does not match the canonical manifest bytes",
			Expected: sigFile.ManifestSHA256,
			Got:      manifestDigest,
		})
		return report, nil
	}

	// --- Step 3: at least one signature verifies against a trusted key,
	// unless AllowUnsigned - and refusal of a same-media key (ADR-008). ------
	if stop, err := checkSignatures(ctx, root, opts, sigFile, canon, report); err != nil {
		return nil, err
	} else if stop {
		return report, nil
	}

	// --- Steps 4-7: file digests, unexpected files, repository metadata,
	// lock and snapshot digests, and the lock's own per-package digests
	// against the manifest. These do NOT stop at the first mismatch -
	// unlike steps 1-3, which gate whether the manifest can be trusted at
	// all, these accumulate every problem so an operator sees the full
	// extent of a tampered medium in one run.
	//
	// SkipFileDigests (never set by verify itself; reserved for inspect on
	// very large bundles) skips hashing only the large pool/*.deb files -
	// structural presence/absence of every file is still checked, and every
	// small file (lock.json, snapshot.json, the repo/ index files) is still
	// digested unconditionally. Lock and snapshot digests in particular must
	// never be gated on this flag: they are recomputed from the raw bytes on
	// disk (see canonicalDigestOfFile), independently of lock.Digest's own
	// struct-based canonicalisation, which is correct for lock.Save's
	// build-time use (there are no "raw bytes" yet when a lock is first
	// constructed) but would be the wrong thing to lean on here - it would
	// let an unknown field appended to lock.json survive an unmarshal/
	// remarshal round trip undetected.
	expected, duplicates := indexFiles(m.Files)
	for _, path := range duplicates {
		report.Problems = append(report.Problems, Problem{
			Kind: ProblemManifestMalformed, Path: manifest.FileName,
			Message: "manifest lists " + path + " more than once, with entries that need not agree; " +
				"the manifest is the one signed object and must say exactly one thing about each path",
		})
	}
	checkFiles(root, expected, report, opts.SkipFileDigests)
	checkRepository(m, expected, report)
	checkLockAndSnapshot(root, m, report)
	checkLockPackages(root, expected, report)

	report.OK = len(report.Problems) == 0
	return report, nil
}

// classifyManifestErr maps a manifest.Load/LoadSignature error onto the
// closest Problem kind: an unrecognised schema_version is distinguished from
// a document that does not parse at all.
func classifyManifestErr(err error, path string) Problem {
	if errors.Is(err, manifest.ErrUnknownSchema) {
		return Problem{Kind: ProblemSchemaUnknown, Path: path, Message: err.Error()}
	}
	return Problem{Kind: ProblemManifestMalformed, Path: path, Message: err.Error()}
}

// maxSignatureBlocks bounds how many signature blocks one
// debark.manifest.sig may carry before verify refuses to look at any of
// them. See the refusal in checkSignatures for why a bound is needed at all.
const maxSignatureBlocks = 32

// checkSignatures runs step 3. stop is true when verification must end here:
// no trusted signature was found and AllowUnsigned was not given, or a
// signature from an otherwise-trusted key failed cryptographic verification
// (the clearest possible tamper signal). err is non-nil only when
// verification itself could not be attempted at all (e.g. VerifierFor could
// not be built for a reason other than a same-media key) - a condition
// outside the check list, reported as a Go error rather than a Problem.
func checkSignatures(ctx context.Context, root string, opts Options, sigFile *manifest.SignatureFile, canon []byte, report *Report) (stop bool, err error) {
	if sigFile == nil || len(sigFile.Signatures) == 0 {
		if opts.AllowUnsigned {
			report.Signed = false
			report.Warnings = append(report.Warnings, "bundle is not signed; accepted because AllowUnsigned was given")
			return false, nil
		}
		report.Problems = append(report.Problems, Problem{
			Kind: ProblemSignatureMissing, Path: manifest.SigFileName,
			Message: "bundle is not signed and AllowUnsigned was not given",
		})
		return true, nil
	}

	// The signature file is attacker-supplied, unauthenticated at this point,
	// and read whole by core/manifest with no size bound. Every block in it
	// used to be checked, and a gpg block costs a subprocess: a 29 MB .sig
	// holding 200,000 gpg blocks kept verify running past a minute, while the
	// byte-identical file labelled ed25519-file finished in one second. The
	// attacker picks the label, so the operator's own configuration does not
	// bound the cost - only a count does.
	//
	// Refusing outright, rather than checking the first few and ignoring the
	// rest, is the honest answer: a file with hundreds of blocks is not a
	// bundle anyone signed, and silently ignoring the tail would let an
	// attacker bury the real signature behind padding. A real bundle carries
	// one block per key that signed it - a release key, a build key, a
	// customer's counter-signature - so the bound is generous by two orders of
	// magnitude.
	//
	// This runs before sign.VerifierFor: nothing is spawned, and no keyring is
	// opened, for a signature file already known to be implausible. Bounding
	// the .sig and manifest FILE sizes belongs to core/manifest's loaders,
	// which read them with os.ReadFile and no cap; that half is reported
	// separately and is not fixed here.
	if len(sigFile.Signatures) > maxSignatureBlocks {
		report.Problems = append(report.Problems, Problem{
			Kind: ProblemManifestMalformed, Path: manifest.SigFileName,
			Message: "signature file carries " + strconv.Itoa(len(sigFile.Signatures)) +
				" signature blocks, more than the " + strconv.Itoa(maxSignatureBlocks) +
				" a bundle may have; refusing rather than checking them",
			Expected: "at most " + strconv.Itoa(maxSignatureBlocks),
			Got:      strconv.Itoa(len(sigFile.Signatures)),
		})
		return true, nil
	}

	verifier, verr := sign.VerifierFor(ctx, opts.Keys, root)
	if verr != nil {
		if errors.Is(verr, sign.ErrSameMediaKey) {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemSameMediaKey, Message: verr.Error(),
			})
			return true, nil
		}
		return false, verr
	}

	anyValidTrusted := false
	anyInvalidTrusted := false
	// usedDefaultGPGKeyring records whether any signature that verified did
	// so through gpg's ambient default keyring rather than an explicit
	// KeySource.GPGKeyring (F4, docs/security/review-findings.md). This is
	// standard, documented gpg behaviour, not a bug, and it is not changed
	// here — but sign.VerifierFor returns an opaque Verifier (frozen
	// interface, core/sign/iface.go) with no way to ask it after the fact
	// which keyring a check actually used, so the one piece of information
	// this function needs — was GPGKeyring left empty — is read directly
	// from opts.Keys, which checkSignatures already has. No new plumbing
	// into core/sign is needed or added.
	usedDefaultGPGKeyring := false
	for _, block := range sigFile.Signatures {
		res := SignatureResult{SignerKind: block.SignerKind, KeyID: block.KeyID, Algorithm: block.Algorithm}
		cerr := verifier.Verify(ctx, manifest.SignPurpose, canon, block)
		switch {
		case cerr == nil:
			res.Valid, res.Trusted = true, true
			anyValidTrusted = true
			if block.SignerKind == manifest.SignerGPG && opts.Keys.GPGKeyring == "" {
				usedDefaultGPGKeyring = true
			}
		case errors.Is(cerr, sign.ErrUntrustedKey):
			res.Detail = "signed by a key not present in the trusted key sources"
		case dferr.ClassOf(cerr) == dferr.Verification:
			res.Trusted = true
			res.Detail = cerr.Error()
			anyInvalidTrusted = true
		default:
			// The verifier could not even attempt this check (e.g. gpg is
			// not installed on this machine). That is an environment
			// problem, not evidence about the bundle; surface it as a
			// genuine error rather than a tamper Problem.
			return false, cerr
		}
		report.Signatures = append(report.Signatures, res)
		if anyInvalidTrusted {
			// The verdict is settled and no later block can move it: the
			// switch below turns anyInvalidTrusted into ProblemSignatureInvalid
			// and stop=true whatever else the file holds, because a signature
			// from a trusted key that does not verify is the clearest tamper
			// signal there is. Continuing would only spend more subprocesses
			// on an attacker-chosen list after the answer is known.
			break
		}
	}

	if usedDefaultGPGKeyring {
		// Recorded regardless of which branch the switch below takes (even
		// when a *different* signature block goes on to fail as
		// ProblemSignatureInvalid): the fact that a gpg check ran against
		// whatever happens to be imported in this machine's default keyring
		// is true independently of the overall outcome, and an operator
		// reading the report should see it either way.
		report.Warnings = append(report.Warnings,
			"a gpg signature verified against this machine's default GPG keyring (no --gpg-keyring was given); "+
				"the trust set is whatever keys happen to already be imported there, not one explicitly designated for debark — "+
				"pass --gpg-keyring pointing at a keyring containing only the expected release key(s) to narrow it")
	}

	switch {
	case anyInvalidTrusted:
		report.Problems = append(report.Problems, Problem{
			Kind: ProblemSignatureInvalid, Path: manifest.SigFileName,
			Message: "a signature from a trusted key does not verify against this manifest",
		})
		return true, nil
	case anyValidTrusted:
		report.Signed = true
		return false, nil
	case opts.AllowUnsigned:
		report.Signed = false
		report.Warnings = append(report.Warnings, "no signature verifies against a trusted key; accepted because AllowUnsigned was given")
		return false, nil
	default:
		report.Problems = append(report.Problems, Problem{
			Kind: ProblemSignatureUntrusted, Path: manifest.SigFileName,
			Message: "no signature verifies against a trusted key",
		})
		return true, nil
	}
}

// poolPathPrefix is the bundle-relative directory every large .deb lives
// under ("repo/pool/<p>/<pkg>/<pkg>_<ver>_<arch>.deb"). It is the only
// thing skipPoolDigests may skip hashing.
const poolPathPrefix = "repo/pool/"

func isPoolFile(relPath string) bool { return strings.HasPrefix(relPath, poolPathPrefix) }

// checkFiles runs steps 4 and 5 in one walk of the bundle tree: every file the
// manifest lists must exist with the recorded size and digest (continuing
// past the first mismatch), and no file may be present that the manifest does
// not list - including anything under repo/: apt reads exactly what its
// indices name, but an operator inspecting the medium, and this checker, does
// not get to assume the rest of repo/ is inert just because apt would not
// walk it.
//
// skipPoolDigests, when true, still checks every file's presence and size but
// skips hashing the contents of files under repo/pool/ - the only files large
// enough for that cost to matter. Every other file (lock.json, snapshot.json,
// the repo/ index files, anything else in the bundle) is always digested.
//
// expected is the shared index built by indexFiles, not a second reading of
// m.Files: this walk and checkRepository must resolve every path the same
// way, or the two of them disagree about what the signed manifest said.
func checkFiles(root string, expected map[string]manifest.File, report *Report, skipPoolDigests bool) {
	seen := make(map[string]bool, len(expected))

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			report.Problems = append(report.Problems, Problem{Kind: ProblemFileMissing, Path: path, Message: "cannot read: " + err.Error()})
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// The entry-type gate, and the reason it comes before everything
		// else - before the manifest lookup, and before the manifest/
		// signature skip below.
		//
		// This walk is the only place that sees what lstat sees. Every check
		// under it opens the path and follows wherever it leads:
		// digest.SHA256File uses os.Open, checkLockAndSnapshot uses
		// os.ReadFile. So a name that merely POINTS at the right bytes
		// verifies exactly like the bytes themselves - and the difference
		// only shows up afterwards, because the data behind such a name never
		// lived under the root this walk covers. It can be rewritten between
		// the moment verify hashes it and the moment apt reads the same path,
		// without touching the medium that was just approved, which turns a
		// narrow race into "pre-position a file and wait". tar restores a
		// symlink target verbatim, absolute targets included, so an
		// attacker's archive extracted by the operator's own tar is a
		// sufficient delivery mechanism.
		//
		// The rule already holds on the way in - core/bundle/tar.go refuses
		// TypeSymlink and TypeLink outright ("bundles never contain links")
		// and refuses to export anything that is not a regular file - so a
		// tree reaching this walk with a link in it was not built by
		// debark. Devices, sockets and FIFOs go the same way: none of them
		// can be a bundle file, and reading one can block forever.
		if d.Type() != 0 {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemFileNotRegular, Path: rel,
				Message: "not a regular file, so this path does not hold the bytes the manifest describes; " +
					"a bundle contains only regular files and directories",
				Expected: "regular file", Got: d.Type().String(),
			})
			return nil
		}
		// A hard link is deliberately NOT refused, though an earlier version of
		// this check refused one with link count > 1. Written down because the
		// check reads as obviously correct and will otherwise be re-added:
		//
		// 1. The threat it named cannot occur. A hard link must live on the same
		//    filesystem as its inode, so "the same bytes have another name on a
		//    filesystem that may still be writable when the medium is not"
		//    describes a SYMLINK, not this. Read-only media gives every name the
		//    same read-only inode. Where the bundle does sit on writable storage,
		//    whoever can rewrite it through a second name can rewrite it through
		//    the first, so the extra name grants nothing the file's own mode did
		//    not already grant. Permissions are the control there, not link
		//    counts.
		//
		// 2. Refusing it broke the product. store.Materialise HARD LINKS pool
		//    objects out of the content-addressed store into the bundle - that is
		//    what makes assembly cheap, and the additive-run rule sanctions it - so every freshly
		//    built bundle carried link count 2 on every .deb and failed
		//    verification in place. Only a bundle round-tripped through tar
		//    export/import passed, because core/bundle/tar.go refuses link
		//    entries and the extracted copy has a single name.
		//
		// The symlink form, which is the real exposure, is refused outright by
		// the entry-type test above.
		if rel == manifest.FileName || rel == manifest.SigFileName {
			return nil
		}
		f, ok := expected[rel]
		if !ok {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemFileUnexpected, Path: rel,
				Message: "present in the bundle but not listed in the manifest",
			})
			return nil
		}
		seen[rel] = true

		if skipPoolDigests && isPoolFile(rel) {
			info, ierr := d.Info()
			if ierr != nil {
				report.Problems = append(report.Problems, Problem{Kind: ProblemFileMissing, Path: rel, Message: ierr.Error()})
				return nil
			}
			report.FilesChecked++
			size := info.Size()
			report.BytesChecked += size
			if size != f.Size {
				report.Problems = append(report.Problems, Problem{
					Kind: ProblemFileSize, Path: rel, Message: "file size does not match the manifest",
					Expected: strconv.FormatInt(f.Size, 10), Got: strconv.FormatInt(size, 10),
				})
			}
			return nil
		}

		sum, size, herr := digest.SHA256File(path)
		if herr != nil {
			report.Problems = append(report.Problems, Problem{Kind: ProblemFileMissing, Path: rel, Message: herr.Error()})
			return nil
		}
		report.FilesChecked++
		report.BytesChecked += size
		if size != f.Size {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemFileSize, Path: rel, Message: "file size does not match the manifest",
				Expected: strconv.FormatInt(f.Size, 10), Got: strconv.FormatInt(size, 10),
			})
			return nil
		}
		if !digest.Equal(sum, f.SHA256) {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemFileDigest, Path: rel, Message: "file digest does not match the manifest",
				Expected: f.SHA256, Got: sum,
			})
		}
		return nil
	})
	if walkErr != nil {
		report.Problems = append(report.Problems, Problem{Kind: ProblemFileMissing, Message: "walking bundle tree: " + walkErr.Error()})
	}

	missing := make([]string, 0)
	for path := range expected {
		if !seen[path] {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	for _, path := range missing {
		report.Problems = append(report.Problems, Problem{
			Kind: ProblemFileMissing, Path: path,
			Message:  "listed in the manifest but not present in the bundle",
			Expected: expected[path].SHA256,
		})
	}
}

// indexFiles turns the manifest's file list into the one lookup table every
// check shares, and names every path listed more than once.
//
// A duplicate is a refusal in its own right, not a resolution rule. The two
// resolvers that used to exist disagreed - checkFiles built a map and so took
// the LAST entry, checkRepository scanned the slice and so took the FIRST -
// which meant a signed manifest could carry two contradictory descriptions of
// one path and whether the contradiction was noticed depended on which path
// had been duplicated. A duplicate on repo/Packages was caught by the
// repository cross-check; a duplicate on a pool .deb, on lock.json or on
// snapshot.json was accepted with no problem and no warning, the losing entry
// discarded before any check saw it, and files_checked quietly one short of
// the manifest's own file count.
//
// That is not remotely exploitable without the signing key, and it is still
// worth refusing: the manifest is the single signed object the whole design
// rests on, and a document that can be read two ways does not have one
// meaning to sign. First-wins here is arbitrary but deterministic - the
// duplicate is reported either way, so no reading of it can pass - and it
// keeps the order the repository cross-check always had.
func indexFiles(files []manifest.File) (map[string]manifest.File, []string) {
	expected := make(map[string]manifest.File, len(files))
	var duplicates []string
	for _, f := range files {
		if _, ok := expected[f.Path]; ok {
			duplicates = append(duplicates, f.Path)
			continue
		}
		expected[f.Path] = f
	}
	sort.Strings(duplicates)
	return expected, slices.Compact(duplicates)
}

// checkRepository runs step 6: it cross-checks the manifest's own Repository
// summary digests against its Files entries for the same paths. This is
// deliberately independent of checkFiles's on-disk comparison above - it
// catches the manifest document being internally inconsistent (which would
// otherwise slip through if only the two summary forms were ever compared to
// each other and never to a third, independent source).
//
// It reads the same expected index checkFiles walks against, so both answer
// "what does the manifest say about this path" identically; see indexFiles.
func checkRepository(m *manifest.Manifest, expected map[string]manifest.File, report *Report) {
	check := func(relPath, want string) {
		if want == "" {
			return
		}
		f, ok := expected[relPath]
		if !ok {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemRepoDigest, Path: relPath,
				Message: "manifest repository digest references a file not listed in the manifest",
			})
			return
		}
		if !digest.Equal(f.SHA256, want) {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemRepoDigest, Path: relPath,
				Message:  "manifest repository digest does not match the manifest's own file entry",
				Expected: want, Got: f.SHA256,
			})
		}
	}
	check("repo/Packages", m.Repository.PackagesSHA256)
	check("repo/Packages.gz", m.Repository.PackagesGzSHA256)
	check("repo/Release", m.Repository.ReleaseSHA256)
	check("repo/InRelease", m.Repository.InReleaseSHA256)
	check("repo/Release.gpg", m.Repository.ReleaseGPGSHA256)
}

// checkLockAndSnapshot runs the first half of step 7: the lock's canonical
// digest matches manifest.LockDigest, and the snapshot's does too when
// snapshot.json is present (it is optional in a bundle). It asks whether
// lock.json is the document the manifest signed; checkLockPackages below asks
// the separate question of whether what that document SAYS is true of this
// bundle. Both are computed by re-canonicalising
// the raw bytes read from disk, the same defensive technique manifest.Load
// uses and for the same reason: it catches an added or reordered field that a
// struct-remarshal would silently drop.
func checkLockAndSnapshot(root string, m *manifest.Manifest, report *Report) {
	lockPath := filepath.Join(root, lock.FileName)
	if got, err := canonicalDigestOfFile(lockPath); err != nil {
		report.Problems = append(report.Problems, Problem{Kind: ProblemLockDigest, Path: lock.FileName, Message: "cannot read: " + err.Error()})
	} else if !digest.Equal(got, m.LockDigest) {
		report.Problems = append(report.Problems, Problem{
			Kind: ProblemLockDigest, Path: lock.FileName,
			Message:  "lock digest does not match the manifest's lock_digest",
			Expected: m.LockDigest, Got: got,
		})
	}

	snapPath := filepath.Join(root, snapshotDocumentName)
	if _, err := os.Stat(snapPath); err != nil {
		return // snapshot.json is an optional companion file in a bundle
	}
	if got, err := canonicalDigestOfFile(snapPath); err != nil {
		report.Problems = append(report.Problems, Problem{Kind: ProblemSnapshotDigest, Path: snapshotDocumentName, Message: "cannot read: " + err.Error()})
	} else if !digest.Equal(got, m.SnapshotDigest) {
		report.Problems = append(report.Problems, Problem{
			Kind: ProblemSnapshotDigest, Path: snapshotDocumentName,
			Message:  "snapshot digest does not match the manifest's snapshot_digest",
			Expected: m.SnapshotDigest, Got: got,
		})
	}
}

// lockRepoPrefix turns a lock.Package.Filename into a bundle-relative path.
//
// lock.Package.Filename is POOL-relative ("pool/d/demo/demo_1.0-1_amd64.deb"),
// not bundle-relative, because it is the same string that goes into the
// "Filename:" field of repo/Packages - and apt resolves that against the
// repository root. The bundle-relative path is therefore this prefix plus that
// value. The value mirrors core/bundle.RepoDir ("repo"), inlined here for the
// same reason snapshotDocumentName is: verify's build must not depend on the
// compile state of a package it needs one fixed string from. The invariant
// that ties the two together is lockRepoPrefix+"pool/" == poolPathPrefix.
const lockRepoPrefix = "repo/"

// checkLockPackages is the second half of step 7: every .deb the lock names
// must be the file the signed manifest attests at that path.
//
// The gap it closes. Before this, nothing anywhere compared lock.Package's
// SHA256 to the bytes it claims to describe. checkFiles proves the manifest
// matches the disk, and checkLockAndSnapshot proves lock.json is the document
// the manifest signed - but "the lock is the document that was signed" says
// nothing about whether the digests INSIDE it are true. A build in which a
// later selection overwrote an earlier one's pool file left the lock's
// recorded digest describing bytes that were no longer there, and verify
// passed, because the manifest was built after the overwrite and so agreed
// with the disk perfectly. The lock is the plan an auditor reads and the
// document core/install acts on (ADR-007); a plan whose digests describe
// files that are not in the bundle is not a plan anyone can check.
//
// The comparison is against the MANIFEST, not against the disk, and that is
// the whole reason this is cheap enough to be unconditional:
//
//   - The manifest already carries every pool file's digest, and checkFiles
//     has already compared that digest to the bytes on the medium. Transitivity
//     does the rest, so not one extra byte is hashed - which also means the
//     check still runs, and still means something, under SkipFileDigests.
//   - It keeps this a check on the DOCUMENTS. A direct lock-to-disk hash would
//     report the same failure twice whenever a file was also tampered with,
//     and would say nothing at all when the manifest and the lock disagreed
//     about a file that is not present.
//
// No filepath.Join, and nothing here opens anything. The derived path is used
// only as a key into the index checkFiles already built, exactly like every
// other path in this file, so a lock naming "../../etc/shadow" can only ever
// fail to match - it can never reach outside the bundle. (core/lock's own
// checkLoadSafety refuses such a filename at load time as well; this holds
// regardless, because a gate that is only correct because another package
// validated its input has no property of its own.)
//
// Reusing ProblemLockDigest rather than minting a kind: the frozen kind set is
// API-visible - api/schema/verifyreport.v1.schema.json and
// installreport.v1.schema.json carry the same enum - and this finding is the
// same sentence the existing kind already says, "the lock does not describe
// this bundle", with Path naming the pool file instead of lock.json. A script
// already branching on lock-digest-mismatch treats it correctly.
func checkLockPackages(root string, expected map[string]manifest.File, report *Report) {
	l, err := lock.Load(root)
	if err != nil {
		// Fail closed. This is reached only for a lock.json this build cannot
		// read at all, which is either damage (already reported by
		// checkLockAndSnapshot, and a second problem naming the consequence
		// costs an operator nothing) or a bundle written by a debark whose
		// lock schema this one does not know. The second case is the one that
		// matters: its lock digest can match the signed manifest perfectly, so
		// nothing else in this report would say that the packages the bundle
		// plans to install were never cross-checked. Certifying a document
		// that could not be read is the failure mode this project has already
		// been bitten by once.
		report.Problems = append(report.Problems, Problem{
			Kind: ProblemLockDigest, Path: lock.FileName,
			Message: "cannot read the lock, so the packages it names could not be checked against the manifest: " + err.Error(),
		})
		return
	}
	for _, p := range l.Packages {
		rel := lockRepoPrefix + p.Filename
		f, ok := expected[rel]
		if !ok {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemLockDigest, Path: rel,
				Message: "lock.json names " + p.Name + ":" + p.Arch + " at filename " + p.Filename +
					", which resolves to " + rel + " - a path the signed manifest does not list, " +
					"so nothing attests the file the lock plans to install",
			})
			continue
		}
		if !digest.Equal(f.SHA256, p.SHA256) {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemLockDigest, Path: rel,
				Message: "lock.json's sha256 for " + p.Name + ":" + p.Arch +
					" is not the digest the signed manifest records for that file",
				Expected: f.SHA256, Got: p.SHA256,
			})
			continue
		}
		if f.Size != p.Size {
			// Unreachable short of a SHA-256 collision, and checked anyway:
			// the two fields are independent claims in the document, and an
			// operator reading a size that disagrees with the manifest has
			// been told something false either way.
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemLockDigest, Path: rel,
				Message: "lock.json's size for " + p.Name + ":" + p.Arch +
					" is not the size the signed manifest records for that file",
				Expected: strconv.FormatInt(f.Size, 10), Got: strconv.FormatInt(p.Size, 10),
			})
		}
	}
}

func canonicalDigestOfFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	canon, err := canonical.Transform(raw)
	if err != nil {
		return "", err
	}
	return canonical.DigestBytes(canon), nil
}
