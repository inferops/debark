package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/inferops/debark/core/lock"
)

// Bundle layout constants. install reads repo/ (the pool) directly rather
// than depending on core/bundle's Open/Assemble machinery: the path
// itself is the frozen contract (of the design), and going straight
// at it has fewer cross-package build dependencies than opening the whole
// bundle object model just to reach one directory.
const repoDirName = "repo"

// loadLock reads and validates lock.json from an already-verified bundle
// directory, via core/lock's own Load and Validate: Load checks
// the file parses and its schema version is known; Validate checks its
// internal consistency (every Install entry names a package that actually
// exists in Packages at that exact version, no duplicate package entries,
// well-formed digests). Verify already proved the bytes were not tampered
// with; Validate is the separate, complementary check that the bytes that
// did survive are self-consistent — a lock can be untampered and still
// malformed, and install must catch that before ever building an apt-get
// argv from it.
func loadLock(bundleDir string) (*lock.Lock, error) {
	lk, err := lock.Load(bundleDir)
	if err != nil {
		return nil, err
	}
	if err := lock.Validate(lk); err != nil {
		return nil, err
	}
	return lk, nil
}

// resolveBundleDir turns a bundle path into a directory to read from.
//
// install.Runner supports the directory form only: the common, documented
// case ("run this ... from inside the bundle folder", install-offline.sh;
// the whole walkthrough is directory-shaped). A `.debark.tar.zst`
// archive is not extracted here. That is a deliberate boundary, not an
// oversight: extracting one needs core/bundle.ImportTar (a zstd/tar
// reader is not a dependency this package may add on its own — contract-brief:
// no new dependencies), and depending on core/bundle transitively pulls in
// every package it imports (including core/snapshot), so a build break in
// any of those concurrently-edited packages becomes a build break here too —
// exactly the fragility the contract brief warns against. The CLI, which
// already needs a directory-or-archive helper for `verify` and `inspect`, is
// the natural single place to do this extraction once and hand install.Runner
// an extracted directory either way; ctx is accepted
// here (and ignored, for now) so that extraction can be added without an
// exported signature change if it ever belongs at this layer instead. See
// the "anything deferred" note in the implementation notes.
func resolveBundleDir(_ context.Context, path string) (dir string, cleanup func(), err error) {
	fi, statErr := os.Stat(path)
	if statErr != nil {
		return "", nil, fmt.Errorf("bundle path %s: %w", path, statErr)
	}
	if !fi.IsDir() {
		return "", nil, fmt.Errorf(
			"%s is not a directory: install only reads an already-extracted bundle directory "+
				"(extract a .debark.tar.zst archive first)", path)
	}
	return path, func() {}, nil
}

// missingPoolFiles reports, for the packages one run intends to install,
// every one whose .deb is not present in the bundle's pool as a regular
// file. Each entry is a ready-made Report.Problems sentence.
//
// It never follows a filename that is not a plain relative path inside the
// bundle: an absolute or escaping Filename is reported as missing rather
// than stat'd, so a lock naming "../../etc/shadow" can only ever be refused,
// never reached. core/lock refuses such a filename at load time as well;
// this holds regardless, because the whole point of the check is that it
// does not take the document's word for anything.
func missingPoolFiles(repoDir string, pkgs []lock.Package) []string {
	var problems []string
	for _, p := range pkgs {
		who := p.Name + ":" + p.Arch
		rel := filepath.FromSlash(p.Filename)
		if p.Filename == "" || !filepath.IsLocal(rel) {
			problems = append(problems, "lock names "+who+" with filename "+
				strconv.Quote(p.Filename)+", which is not a path inside the bundle")
			continue
		}
		fi, err := os.Stat(filepath.Join(repoDir, rel))
		if err != nil {
			problems = append(problems, "lock names "+who+" at "+repoDirName+"/"+p.Filename+
				", which the bundle does not hold: "+err.Error())
			continue
		}
		if !fi.Mode().IsRegular() {
			problems = append(problems, "lock names "+who+" at "+repoDirName+"/"+p.Filename+
				", which is not a regular file")
		}
	}
	return problems
}
