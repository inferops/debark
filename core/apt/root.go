package apt

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
)

// warnNoDpkgStatus is the lock.Warning code BuildPrivateRoot raises when it
// had to write an empty dpkg status. It is a constant rather than a literal
// because it is a product identifier that reaches lock.json inside a signed
// manifest, and because a second place now reads it back by value: a base
// closure resolves against an empty status on purpose (closure.go's
// filterBootstrapWarnings) and must suppress exactly this code and no other.
const warnNoDpkgStatus = "private-root.no-dpkg-status"

// ExtraSource is one additional one-line apt source entry written verbatim
// into its own sources.list.d file: the external-.deb staging repo or,
// for the closed-world check, the finished bundle's repo/.
type ExtraSource struct {
	// Name is the destination file's base name, e.g. "debark-external". A
	// ".list" extension is added if Name has neither ".list" nor ".sources".
	Name string
	// Line is the literal source entry, e.g.
	// `deb [trusted=yes] file:///work/bundle/repo ./`.
	Line string
}

// RootSpec configures one private apt root. The zero value builds a
// root with no sources at all, which is never useful on its own; a caller
// sets exactly the fields its case needs — the main resolve copies the
// snapshot's real sources and adds the external staging repo, the
// closed-world check copies only the target's dpkg status and supplies a
// single ExtraSource pointing at the finished bundle.
type RootSpec struct {
	// Dir is the root directory to materialise into. It is created if
	// missing; an existing tree at this path is written into, not cleared —
	// the caller picks a fresh WorkDir subdirectory per run.
	Dir string
	// ArchivesDir is Dir::Cache::archives — where apt leaves downloaded
	// .deb files. Created if non-empty.
	ArchivesDir string
	// Arch is APT::Architecture (the native/target architecture).
	Arch string
	// ForeignArchs adds one APT::Architectures:: entry per entry, after the
	// native one, skipping a duplicate of Arch itself.
	ForeignArchs []string
	// Recommends is the effective APT::Install-Recommends value.
	Recommends bool
	// PhasedPolicy selects between APT::Machine-ID (with MachineID) and
	// APT::Get::Never-Include-Phased-Updates=true. Both options are
	// real and apt honours them — but only for apt's automatic
	// upgrade-candidate selection (a full-upgrade pass). An explicitly named
	// "apt-get install <pkg>" bypasses phasing entirely regardless of this
	// setting, in every configuration tested; this is not a gap in how the
	// option is set here, it is upstream apt behaviour. Measured, not
	// assumed: docs/experiments/E1-phased-updates.md. Callers must not
	// present this policy as protecting the requested-packages pass.
	PhasedPolicy snapshot.PhasedPolicy
	MachineID    string

	// Snapshot and SnapshotFilesDir supply the target's captured state. When
	// Snapshot is non-nil, its dpkg status is always copied in, regardless of
	// CopySnapshotSources.
	Snapshot         *snapshot.Snapshot
	SnapshotFilesDir string
	// CopySnapshotSources also copies the snapshot's sources, preferences,
	// apt.conf.d and keyrings in. False for the closed-world check, which
	// wants only ExtraSources visible.
	CopySnapshotSources bool
	// ApprovedKeys, when non-empty, is the fingerprint allow-list every
	// copied source must be traceable to (uppercase hex); see
	// checkApprovedKeys for exactly what "traceable" means.
	ApprovedKeys []string
	// AllowProxy keeps Acquire::*::Proxy settings from the captured
	// apt.conf.d instead of dropping them.
	AllowProxy bool

	// ExtraSources are written after the snapshot's own sources, so they are
	// read last (apt's own tie-breaking then favours the operator's request
	// only where sources genuinely conflict, which they should not).
	ExtraSources []ExtraSource
}

// PrivateRoot is a materialised private apt root, ready for apt-get.
type PrivateRoot struct {
	Dir string
	// Options is every "-o key=value" apt-get must be invoked with, in
	// construction order. Some keys (APT::Architectures::) legitimately
	// repeat; order among those matters (native arch first) so this slice,
	// not SortedOptions, is what must be passed to Runner.Run.
	Options      []string
	PhasedPolicy snapshot.PhasedPolicy
	// AptConfigPath is the generated loader file every apt-get/apt-cache
	// invocation against this root MUST run with APT_CONFIG set to this
	// path in its environment (NOT a command-line flag — "-c" was measured
	// not to work either). It is what actually carries the captured
	// apt.conf/apt.conf.d into effect: Dir::Etc::main and Dir::Etc::parts
	// cannot be set via -o (docs/experiments/E4-aptconfd-leakage.md).
	// Empty only if BuildPrivateRoot somehow failed to write it, which
	// Runner callers should treat as a hard error, not a fallback to -o.
	AptConfigPath string
	// Dropped is every apt.conf.d entry withheld from the captured
	// configuration, and why.
	Dropped []DroppedConfEntry
	// SignedBy is every Signed-By relationship found in the copied sources,
	// and what happened to it.
	SignedBy []SignedByRecord
	// Warnings is Dropped and the stripped half of SignedBy, rendered as
	// lock.Warning, plus anything else worth telling an operator about this
	// root. The caller folds these into the Plan.
	Warnings []lock.Warning
}

// SortedOptions is Options sorted, for recording in the lock (Resolver
// .APTOptions documents "the sorted -o option list actually used").
func (r *PrivateRoot) SortedOptions() []string {
	out := append([]string(nil), r.Options...)
	sort.Strings(out)
	return out
}

// BuildPrivateRoot materialises a private apt root per spec. It never invokes
// apt-get itself: callers run apt-get with root.Options via a Runner.
func BuildPrivateRoot(spec RootSpec) (*PrivateRoot, error) {
	if spec.Dir == "" {
		return nil, dferr.New(dferr.Usage, "apt: private root: Dir is required")
	}
	if spec.Arch == "" {
		return nil, dferr.New(dferr.Usage, "apt: private root: Arch is required")
	}

	dirs := []string{
		filepath.Join(spec.Dir, "etc", "apt", "sources.list.d"),
		filepath.Join(spec.Dir, "etc", "apt", "preferences.d"),
		filepath.Join(spec.Dir, "etc", "apt", "apt.conf.d"),
		filepath.Join(spec.Dir, "etc", "apt", "trusted.gpg.d"),
		filepath.Join(spec.Dir, "var", "lib", "apt", "lists", "partial"),
		filepath.Join(spec.Dir, "var", "lib", "dpkg"),
		filepath.Join(spec.Dir, "var", "log", "apt"),
		filepath.Join(spec.Dir, "var", "cache", "apt", "archives", "partial"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "apt: private root: create %s", d)
		}
	}
	if spec.ArchivesDir != "" {
		if err := os.MkdirAll(filepath.Join(spec.ArchivesDir, "partial"), 0o755); err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "apt: private root: create archives dir")
		}
	}

	root := &PrivateRoot{Dir: spec.Dir, PhasedPolicy: spec.PhasedPolicy}

	statusDst := filepath.Join(spec.Dir, "var", "lib", "dpkg", "status")
	switch {
	case spec.Snapshot != nil && spec.Snapshot.DpkgStatus.ArchivePath != "":
		if err := copySnapshotFile(spec.SnapshotFilesDir, spec.Snapshot.DpkgStatus, statusDst); err != nil {
			return nil, dferr.Wrap(dferr.Usage, err, "apt: private root: copy dpkg status")
		}
	default:
		if err := os.WriteFile(statusDst, nil, 0o644); err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "apt: private root: write empty dpkg status")
		}
		root.Warnings = append(root.Warnings, lock.Warning{
			Code:    warnNoDpkgStatus,
			Message: "no dpkg status available; resolving as if nothing were installed on the target",
		})
	}

	if err := writeIfAbsent(filepath.Join(spec.Dir, "etc", "apt", "preferences")); err != nil {
		return nil, err
	}
	if err := writeIfAbsent(filepath.Join(spec.Dir, "etc", "apt", "sources.list")); err != nil {
		return nil, err
	}

	if spec.CopySnapshotSources && spec.Snapshot != nil {
		if err := root.copySnapshotConfig(spec); err != nil {
			return nil, err
		}
	}

	for _, es := range spec.ExtraSources {
		name := es.Name
		if !strings.HasSuffix(name, ".list") && !strings.HasSuffix(name, ".sources") {
			name += ".list"
		}
		dst := filepath.Join(spec.Dir, "etc", "apt", "sources.list.d", name)
		if err := os.WriteFile(dst, []byte(es.Line+"\n"), 0o644); err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "apt: private root: write source %s", name)
		}
	}

	root.Options = buildOptions(spec)

	aptConfigPath, err := writeAptConfigLoader(spec.Dir)
	if err != nil {
		return nil, err
	}
	root.AptConfigPath = aptConfigPath

	return root, nil
}

// writeAptConfigLoader writes the small apt.conf-syntax file that must be
// pointed to by the APT_CONFIG environment variable (never a "-c" flag —
// measured not to work, docs/experiments/E4-aptconfd-leakage.md) for every
// apt-get/apt-cache invocation against this root. It is the only mechanism
// that actually redirects apt's apt.conf/apt.conf.d scan: apt enumerates
// apt.conf.d at a bootstrap stage that runs before -o/-c are processed, so
// "-o Dir::Etc::parts=..." changes the in-memory value without changing
// which files were already read — confirmed byte-for-byte identical output
// with and without that override in the experiment above. APT_CONFIG is
// consulted early enough to still matter.
//
// Dir::Etc::main is pointed at ROOT/etc/apt/apt.conf even when no top-level
// apt.conf was captured (writeIfAbsent never creates that file, so it
// legitimately may not exist): apt treats a missing main config file as
// normal — most real systems do not have one — so pointing at it
// unconditionally is simpler than tracking whether one exists and behaves
// identically either way.
func writeAptConfigLoader(rootDir string) (string, error) {
	etc := filepath.Join(rootDir, "etc", "apt")
	loader := fmt.Sprintf(
		"Dir::Etc::main %s;\nDir::Etc::parts %s;\n",
		quoteConfValue(filepath.Join(etc, "apt.conf")),
		quoteConfValue(filepath.Join(etc, "apt.conf.d")),
	)
	path := filepath.Join(rootDir, "debark-apt.conf")
	if err := os.WriteFile(path, []byte(loader), 0o644); err != nil {
		return "", dferr.Wrap(dferr.Environment, err, "apt: private root: write APT_CONFIG loader")
	}
	return path, nil
}

// copySnapshotConfig copies keyrings, sources (rewriting Signed-By),
// preferences and apt.conf.d (filtered) from the snapshot into root.
func (r *PrivateRoot) copySnapshotConfig(spec RootSpec) error {
	snap := spec.Snapshot
	filesDir := spec.SnapshotFilesDir

	type keyFile struct {
		path        string
		archivePath string
		data        []byte
	}
	var keyFiles []keyFile
	collect := func(files []snapshot.File) error {
		for _, f := range files {
			data, err := readSnapshotFile(filesDir, f.ArchivePath)
			if err != nil {
				return dferr.Wrap(dferr.Usage, err, "apt: private root: read keyring %s", f.Path)
			}
			keyFiles = append(keyFiles, keyFile{path: f.Path, archivePath: f.ArchivePath, data: data})
		}
		return nil
	}
	if err := collect(snap.APT.Trusted); err != nil {
		return err
	}
	if err := collect(snap.APT.Keyrings); err != nil {
		return err
	}
	sort.Slice(keyFiles, func(i, j int) bool { return keyFiles[i].archivePath < keyFiles[j].archivePath })

	keyringDest := map[string]string{}
	// keyringFingerprints is derived from the key material this loop actually
	// writes into trusted.gpg.d, never from snap.KeyringFingerprints. The
	// document's own list is a claim made by the untrusted side of the air
	// gap (docs/threat-model.md §3.2): a tampered snapshot can swap a
	// keyring's bytes for an attacker's key while leaving the recorded
	// fingerprints naming the genuine archive key, and deciding
	// --approved-keys on that claim approves the attacker's key. Deriving it
	// here means the fingerprint recorded against a source -- and so
	// lock.Origin.KeyFingerprint, "the archive key that signed that Release"
	// -- is a fact about the bytes apt will really verify against.
	// core/snapshot.Open independently refuses a document whose claims
	// disagree with its key material; this is the second, local check, so
	// neither has to assume the other ran.
	keyringFingerprints := map[string][]string{}
	used := map[string]bool{}
	trustedDir := filepath.Join(spec.Dir, "etc", "apt", "trusted.gpg.d")
	for _, kf := range keyFiles {
		base := filepath.Base(filepath.FromSlash(kf.path))
		if base == "" || base == "." || base == string(filepath.Separator) {
			base = filepath.Base(filepath.FromSlash(kf.archivePath))
		}
		lower := strings.ToLower(base)
		if !strings.HasSuffix(lower, ".gpg") && !strings.HasSuffix(lower, ".asc") {
			base += ".gpg"
		}
		name := base
		if used[name] {
			sum := sha256.Sum256([]byte(kf.path))
			ext := filepath.Ext(name)
			name = fmt.Sprintf("%s-%x%s", strings.TrimSuffix(name, ext), sum[:4], ext)
		}
		used[name] = true
		dst := filepath.Join(trustedDir, name)
		if err := os.WriteFile(dst, kf.data, 0o644); err != nil {
			return dferr.Wrap(dferr.Environment, err, "apt: private root: write keyring %s", name)
		}
		if kf.path != "" {
			keyringDest[kf.path] = dst
			fps, perr := snapshot.FingerprintsIn(kf.data)
			if perr != nil {
				// Fail closed, loudly. Unparseable key material contributes no
				// fingerprints at all, so any source pinned to this keyring
				// can never satisfy an approved-key list -- which is the right
				// answer, because nothing here can say what key apt would end
				// up verifying that source against.
				r.Warnings = append(r.Warnings, lock.Warning{
					Code: "private-root.keyring-unparseable",
					Message: fmt.Sprintf(
						"keyring %s is not a readable OpenPGP keyring (%v); it contributes no fingerprints, so no approved-keys policy can be satisfied through it",
						kf.path, perr),
				})
			}
			keyringFingerprints[kf.path] = fps
		}
	}

	bySourceFile := map[string][]SignedByRecord{}
	var sourceFiles []string
	for _, f := range snap.APT.Sources {
		data, err := readSnapshotFile(filesDir, f.ArchivePath)
		if err != nil {
			return dferr.Wrap(dferr.Usage, err, "apt: private root: read source %s", f.Path)
		}
		rewritten, records := rewriteSignedBy(f.ArchivePath, data, keyringDest)
		for i := range records {
			records[i].Fingerprints = fingerprintsFor(records[i], keyringFingerprints)
		}
		dst := sourceDest(spec.Dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return dferr.Wrap(dferr.Environment, err, "apt: private root: mkdir")
		}
		if err := os.WriteFile(dst, rewritten, 0o644); err != nil {
			return dferr.Wrap(dferr.Environment, err, "apt: private root: write source %s", filepath.Base(dst))
		}
		r.SignedBy = append(r.SignedBy, records...)
		bySourceFile[f.ArchivePath] = records
		sourceFiles = append(sourceFiles, f.ArchivePath)
		for _, rec := range records {
			if rec.Stripped {
				r.Warnings = append(r.Warnings, lock.Warning{
					Code: "private-root.signed-by-stripped",
					Message: fmt.Sprintf(
						"no captured keyring for Signed-By %s (from %s); trusting it via the private root's combined keyring set instead of pinning it to its own key",
						rec.OriginalPath, f.Path),
				})
			}
		}
	}

	if err := checkApprovedKeys(sourceFiles, bySourceFile, spec.ApprovedKeys); err != nil {
		return err
	}

	for _, f := range snap.APT.Preferences {
		if err := copySnapshotFile(filesDir, f, preferencesDest(spec.Dir, f.Path)); err != nil {
			return dferr.Wrap(dferr.Usage, err, "apt: private root: copy preferences %s", f.Path)
		}
	}

	for _, f := range snap.APT.Conf {
		data, err := readSnapshotFile(filesDir, f.ArchivePath)
		if err != nil {
			return dferr.Wrap(dferr.Usage, err, "apt: private root: read %s", f.Path)
		}
		filtered, dropped := filterAptConf(f.ArchivePath, data, spec.AllowProxy)
		dst := confDest(spec.Dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return dferr.Wrap(dferr.Environment, err, "apt: private root: mkdir")
		}
		if err := os.WriteFile(dst, filtered, 0o644); err != nil {
			return dferr.Wrap(dferr.Environment, err, "apt: private root: write %s", filepath.Base(dst))
		}
		r.Dropped = append(r.Dropped, dropped...)
	}
	for _, d := range r.Dropped {
		r.Warnings = append(r.Warnings, lock.Warning{
			Code:    "private-root.apt-conf-dropped",
			Message: fmt.Sprintf("dropped %s from %s: %s", d.Key, d.File, d.Reason),
		})
	}

	return nil
}

// checkApprovedKeys enforces the --approved-keys policy at source-
// file granularity: a source file passes only when every Signed-By it
// declared was rewritten to a captured keyring whose own bytes hold at least
// one fingerprint in approvedKeys. Everything else fails closed — a file with
// no path-form Signed-By at all, a Signed-By that had to be stripped, an
// inline armoured key, and a keyring whose bytes yielded no fingerprint.
// This is coarser than per-suite pinning — a multi-suite deb822 stanza is
// approved or rejected as a whole — which is documented as a known
// limitation, not silently assumed away.
//
// The stripped and inline cases are refusals, not omissions. A stripped
// Signed-By means apt will verify that source against the private root's
// whole combined trusted.gpg.d instead of one pinned key, so "this source is
// authenticated by an approved key" is a statement nobody is in a position to
// make about it — whatever any fingerprint claim elsewhere in the document
// says. An inline armoured key is real key material, but it is carried in the
// source file itself and traceable to no keyring, so the same applies; a file
// whose only Signed-By is inline already failed closed here, and a file that
// mixes an inline stanza with a pinned one must not ride in on the pinned
// one's approval.
func checkApprovedKeys(files []string, bySourceFile map[string][]SignedByRecord, approvedKeys []string) error {
	if len(approvedKeys) == 0 {
		return nil
	}
	approved := make(map[string]bool, len(approvedKeys))
	for _, k := range approvedKeys {
		approved[strings.ToUpper(strings.TrimSpace(k))] = true
	}
	var bad []string
	for _, f := range files {
		if why := approvedKeyRefusal(bySourceFile[f], approved); why != "" {
			bad = append(bad, fmt.Sprintf("%s (%s)", f, why))
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return dferr.New(dferr.Policy, "apt: source(s) not authenticated by an approved key: %s", strings.Join(bad, "; "))
	}
	return nil
}

// approvedKeyRefusal returns why one source file fails the approved-key
// policy, or "" if it passes. The reason is part of the contract: an operator
// reading an exit-6 policy violation has to be able to tell "this key is not
// on your list" from "nothing pins this source to any key at all".
func approvedKeyRefusal(records []SignedByRecord, approved map[string]bool) string {
	sawPinned := false
	for _, rec := range records {
		switch {
		case rec.Stripped:
			return fmt.Sprintf("Signed-By %s was stripped because no captured keyring matched it, so apt verifies this source against the whole combined keyring set rather than an approved key", rec.OriginalPath)
		case rec.Inline:
			return "carries an inline armoured Signed-By key, which belongs to no keyring and so can be traced to no approved fingerprint"
		}
		sawPinned = true
		found := false
		for _, fp := range rec.Fingerprints {
			if approved[strings.ToUpper(fp)] {
				found = true
				break
			}
		}
		if !found {
			if len(rec.Fingerprints) == 0 {
				return fmt.Sprintf("no OpenPGP key could be read from the keyring %s names", rec.OriginalPath)
			}
			return fmt.Sprintf("the keyring %s names holds no approved key", rec.OriginalPath)
		}
	}
	if !sawPinned {
		return "declares no path-form Signed-By, so nothing pins it to a key"
	}
	return ""
}

// fingerprintsFor returns the fingerprints of the keyring a Signed-By was
// rewritten to, as read from that keyring's own bytes — see the
// keyringFingerprints comment in copySnapshotConfig for why this must never
// come from the snapshot document's keyring_fingerprints instead. A record
// with nothing to point at (inline, stripped, or no path at all) carries no
// fingerprints, which is what makes checkApprovedKeys fail closed on it.
func fingerprintsFor(rec SignedByRecord, keyringFingerprints map[string][]string) []string {
	if rec.Inline || rec.Stripped || rec.OriginalPath == "" {
		return nil
	}
	fps := keyringFingerprints[rec.OriginalPath]
	if len(fps) == 0 {
		return nil
	}
	return append([]string(nil), fps...)
}

// safeExtractPath joins filesDir with a snapshot File's ArchivePath after
// checking the invariant snapshot.File.ArchivePath now documents: a snapshot
// is untrusted input, captured on a machine the builder does not control, so
// this package never assumes snapshot.Validate already ran. ArchivePath must
// be a local relative path — no leading separator, no drive letter, no
// backslash, no ".." component — or extracting it could write (or, here,
// read) outside the snapshot's files directory.
func safeExtractPath(filesDir, archivePath string) (string, error) {
	if archivePath == "" {
		return "", fmt.Errorf("apt: snapshot file has an empty archive path")
	}
	if strings.ContainsRune(archivePath, '\\') {
		return "", fmt.Errorf("apt: snapshot archive path %q contains a backslash", archivePath)
	}
	if strings.HasPrefix(archivePath, "/") {
		return "", fmt.Errorf("apt: snapshot archive path %q is absolute", archivePath)
	}
	if len(archivePath) >= 2 && archivePath[1] == ':' {
		return "", fmt.Errorf("apt: snapshot archive path %q looks like it has a drive letter", archivePath)
	}
	for _, seg := range strings.Split(archivePath, "/") {
		if seg == ".." {
			return "", fmt.Errorf("apt: snapshot archive path %q has a parent-directory component", archivePath)
		}
	}
	joined := filepath.Join(filesDir, filepath.FromSlash(archivePath))
	// Defence in depth: filepath.Join already cleans "..", so the checks
	// above should make this unreachable, but re-derive and check the
	// relationship anyway rather than trust that reasoning forever.
	rel, err := filepath.Rel(filesDir, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("apt: snapshot archive path %q escapes the snapshot files directory", archivePath)
	}
	return joined, nil
}

// readSnapshotFile reads a captured file's bytes by its snapshot ArchivePath,
// through safeExtractPath.
func readSnapshotFile(filesDir, archivePath string) ([]byte, error) {
	p, err := safeExtractPath(filesDir, archivePath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func copySnapshotFile(filesDir string, f snapshot.File, dst string) error {
	src, err := safeExtractPath(filesDir, f.ArchivePath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// G703 traces dst back to a snapshot document, which is untrusted input,
	// and it is right to look. The confinement is real but it lives in the
	// caller: every dst passed here comes from sourceDest / preferencesDest /
	// confDest below, and each of those is filepath.Join(rootDir, "etc",
	// "apt", <constant subdir>, filepath.Base(targetPath)). filepath.Base
	// cannot return a value containing a separator, so the only free component
	// of the result is a single file name inside a directory this package
	// chose. A hostile "../../../etc/passwd" in the document collapses to
	// "passwd" under the private root; the degenerate ".." collapses to the
	// private root's own apt directory and the write then fails EISDIR.
	//
	// The SOURCE side of the same copy is confined separately and explicitly,
	// by safeExtractPath above -- that one reads from the snapshot's files/
	// tree, where a traversal really could reach outside.
	//
	// If you ever change a *Dest helper to stop calling filepath.Base, this
	// waiver stops being true. That is why it names the mechanism.
	// #nosec G703 -- dst is Join(root, const..., filepath.Base(x)); see above
	return os.WriteFile(dst, data, 0o644)
}

func writeIfAbsent(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return dferr.Wrap(dferr.Environment, err, "apt: private root: write %s", path)
	}
	return nil
}

// sourceDest, preferencesDest and confDest place a captured file either at
// the single top-level path (sources.list / preferences / apt.conf) when
// that is what it was, or under the matching *.d directory, preserving its
// original base name (and so its .list/.sources extension, which is what
// apt uses to choose a parser).
func sourceDest(rootDir, targetPath string) string {
	base := filepath.Base(filepath.FromSlash(targetPath))
	if base == "sources.list" && !strings.Contains(filepath.ToSlash(targetPath), "sources.list.d/") {
		return filepath.Join(rootDir, "etc", "apt", "sources.list")
	}
	return filepath.Join(rootDir, "etc", "apt", "sources.list.d", base)
}

func preferencesDest(rootDir, targetPath string) string {
	base := filepath.Base(filepath.FromSlash(targetPath))
	if base == "preferences" && !strings.Contains(filepath.ToSlash(targetPath), "preferences.d/") {
		return filepath.Join(rootDir, "etc", "apt", "preferences")
	}
	return filepath.Join(rootDir, "etc", "apt", "preferences.d", base)
}

func confDest(rootDir, targetPath string) string {
	base := filepath.Base(filepath.FromSlash(targetPath))
	if base == "apt.conf" && !strings.Contains(filepath.ToSlash(targetPath), "apt.conf.d/") {
		return filepath.Join(rootDir, "etc", "apt", "apt.conf")
	}
	return filepath.Join(rootDir, "etc", "apt", "apt.conf.d", base)
}

// buildOptions assembles the explicit -o option list. Every Dir::* path is
// set individually (never Dir= wholesale, which would relocate dpkg too);
// APT::Architectures:: entries preserve construction order (native first)
// because that order can matter to apt, which is why Options (not
// SortedOptions) is what gets executed.
func buildOptions(spec RootSpec) []string {
	etc := filepath.Join(spec.Dir, "etc", "apt")
	var opts []string
	add := func(kv string) { opts = append(opts, kv) }

	add("Dir::Etc::sourcelist=" + filepath.Join(etc, "sources.list"))
	add("Dir::Etc::sourceparts=" + filepath.Join(etc, "sources.list.d"))
	add("Dir::Etc::preferences=" + filepath.Join(etc, "preferences"))
	add("Dir::Etc::preferencesparts=" + filepath.Join(etc, "preferences.d"))
	add("Dir::Etc::trustedparts=" + filepath.Join(etc, "trusted.gpg.d"))
	// Dir::Etc::main and Dir::Etc::parts are deliberately NOT set here as -o
	// options: apt scans apt.conf.d at a config-bootstrap stage that runs
	// before -o/-c are processed, so a command-line override of those two
	// specific keys is silently a no-op (measured, docs/experiments/
	// E4-aptconfd-leakage.md — confirmed byte-identical output with and
	// without the "-o Dir::Etc::parts=..." override). They are carried
	// instead through the APT_CONFIG environment variable pointed at a
	// generated loader file; see PrivateRoot.AptConfigPath and
	// writeAptConfigLoader below. Every other Dir::Etc::* path here is
	// consulted lazily, later in the run, and -o does affect those.
	add("Dir::State=" + filepath.Join(spec.Dir, "var", "lib", "apt"))
	add("Dir::State::status=" + filepath.Join(spec.Dir, "var", "lib", "dpkg", "status"))
	add("Dir::Cache=" + filepath.Join(spec.Dir, "var", "cache", "apt"))
	if spec.ArchivesDir != "" {
		add("Dir::Cache::archives=" + spec.ArchivesDir)
	}
	add("Dir::Log=" + filepath.Join(spec.Dir, "var", "log", "apt"))

	add("APT::Architecture=" + spec.Arch)
	add("APT::Architectures::=" + spec.Arch)
	for _, fa := range spec.ForeignArchs {
		if fa == spec.Arch {
			continue
		}
		add("APT::Architectures::=" + fa)
	}

	add("Acquire::Languages=none")
	add("Acquire::Retries=3")
	// Keep fetched indices as plain text: it lets every downstream parser in
	// this package stay pure Go with zero extra dependencies, regardless of
	// which compression scheme (gzip/xz/lz4/zstd) a mirror offers on the
	// wire. Confirmed empirically against a real Debian 12 mirror: without
	// this, apt 2.6.1 stores lists as "*_Packages.lz4", which nothing in
	// this module can decompress.
	add("Acquire::GzipIndexes=false")
	add("APT::Sandbox::User=root")
	add("Debug::NoLocking=1")

	if spec.Recommends {
		add("APT::Install-Recommends=true")
	} else {
		add("APT::Install-Recommends=false")
	}

	if spec.PhasedPolicy == snapshot.PhasedTargetMachineID && spec.MachineID != "" {
		add("APT::Machine-ID=" + spec.MachineID)
	} else {
		add("APT::Get::Never-Include-Phased-Updates=true")
	}

	return opts
}
