// Package repository writes the flat apt repository inside a bundle:
// Packages, Packages.gz and Release, in pure Go, deterministically
// (ADR-011). apt-ftparchive is never required, so the builder works in slim
// containers and on machines that have no apt at all.
package repository

import (
	"context"
	"strings"

	"github.com/inferops/debark/core/digest"
)

// Writer generates the repository index files.
type Writer interface {
	// Write indexes the pool and writes Packages, Packages.gz and Release
	// under in.Dir. It is a pure function of its input: two runs over the same
	// pool produce byte-identical files.
	Write(ctx context.Context, in Input) (*Result, error)
}

// Input is one repository generation.
type Input struct {
	// Dir is the repository root inside the bundle (the directory that will
	// hold Release, Packages and pool/).
	Dir string
	// Files is every .deb to index, already placed under Dir.
	Files []PoolFile
	// Release carries the fields written into Release.
	Release ReleaseFields
	// Compress writes Packages.gz alongside Packages. Always true in v1;
	// the field exists for tests that want the plain file only.
	Compress bool
}

// PoolFile is one .deb in the pool.
type PoolFile struct {
	// Path is the file's path relative to Dir, always forward slashes, e.g.
	// pool/v/vlc/vlc_3.0.21-1build1_amd64.deb. It becomes the Filename field
	// in the Packages stanza, which is how apt finds the file.
	Path string
	// Hashes are the file's size and digests. When zero, the writer computes
	// them.
	Hashes digest.FileHashes
}

// ReleaseFields are the values written into Release. Suite, Origin, Label and
// Codename are what a target's apt_preferences would match on, so they are
// explicit rather than inferred.
type ReleaseFields struct {
	Origin        string
	Label         string
	Suite         string
	Codename      string
	Architectures []string
	Components    []string
	Description   string
	// Date is the canonical timestamp written into Release. The bundle
	// assembler passes a fixed value so two builds of the same request are
	// byte-identical.
	Date string
	// NotAutomatic and ButAutomaticUpgrades are written when set, for
	// operators who want the bundle to be a non-default source.
	NotAutomatic         bool
	ButAutomaticUpgrades bool
	// ExtraFields are written verbatim after the known ones, sorted by key.
	ExtraFields map[string]string
}

// DefaultOrigin and friends are the Release values debark writes unless an
// operator overrides them. A distinct Origin is what lets an operator pin
// against the bundle if they choose to.
const (
	DefaultOrigin      = "debark"
	DefaultLabel       = "debark bundle"
	DefaultSuite       = "bundle"
	DefaultComponent   = "main"
	DefaultDescription = "debark offline bundle"
)

// Result is what the writer produced.
type Result struct {
	// PackagesPath, PackagesGzPath and ReleasePath are relative to Input.Dir.
	PackagesPath   string
	PackagesGzPath string
	ReleasePath    string

	PackagesSHA256   string
	PackagesGzSHA256 string
	ReleaseSHA256    string

	// PackageCount is the number of stanzas written.
	PackageCount int
	// PoolBytes is the total size of the indexed files.
	PoolBytes int64
}

// PoolPath returns the canonical pool location for a package: pool/<letter>/
// <source-or-package>/<filename>, with the "lib" prefix convention Debian uses
// (libfoo goes under pool/libf/libfoo). It is exported because the bundle
// assembler and the lock writer must agree on it exactly.
//
// SECURITY. Both arguments originate in a .deb's control data, which is
// attacker-controlled for a vendor .deb: a package whose Package: field is
// "../../../../tmp/evil" would otherwise produce a path that filepath.Join
// resolves outside the bundle, writing an arbitrary file on the builder host.
// PoolPath therefore never returns an escaping path — an invalid name or
// filename yields the empty string, and every caller must treat that as a
// hard error. Validate with ValidPackageName and ValidFilename to report a
// good message before you get here.
func PoolPath(pkg, filename string) string {
	if !ValidPackageName(pkg) || !ValidFilename(filename) {
		return ""
	}
	prefix := poolPrefix(pkg)
	return "pool/" + prefix + "/" + pkg + "/" + filename
}

// ValidPackageName reports whether s is a legal Debian binary package name.
//
// Debian Policy 5.6.1 specifies lowercase alphanumerics plus "+", "-" and
// ".", starting with an alphanumeric. That grammar contains no path
// separator and no "." run that could form a parent reference, which is what
// makes it safe to interpolate into a path.
//
// Policy also requires at least two characters; this check deliberately
// accepts one. The safety property comes entirely from the character set, not
// the length, and debark's whole purpose is to handle real vendor .deb
// files, which bend policy in small ways. Rejecting a legitimate package for
// a rule that buys no security would be the worse failure.
func ValidPackageName(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	if !isLowerAlnum(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isLowerAlnum(c) || c == '+' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	return true
}

// ValidFilename reports whether s is safe to use as the base name of a file in
// the pool: a single path element, no separators, no parent reference, no
// leading dot, no control characters, and ending in .deb.
func ValidFilename(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	if s == "." || s == ".." || s[0] == '.' {
		return false
	}
	if !strings.HasSuffix(s, ".deb") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '/' || c == '\\' || c == ':' || c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func isLowerAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func poolPrefix(pkg string) string {
	if len(pkg) >= 4 && pkg[:3] == "lib" {
		return pkg[:4]
	}
	if pkg == "" {
		return "_"
	}
	return pkg[:1]
}
