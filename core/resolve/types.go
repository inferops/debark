// Package resolve holds the plan a backend produces: the complete list of
// files that must cross the gap, with enough provenance attached to write the
// lock without asking apt anything again.
package resolve

import (
	"sort"
	"strings"

	"github.com/inferops/debark/core/lock"
)

// Plan is the outcome of one resolution. It is a value: no file handles, no
// open processes, nothing that must be closed.
type Plan struct {
	Target   lock.Target   `json:"target"`
	Resolver lock.Resolver `json:"resolver"`

	// Selections is every file apt chose, plus every external .deb the
	// operator supplied, in no particular order. Sort with SortSelections
	// before writing anything derived from it.
	Selections []Selection `json:"selections"`

	// Install is the exact set install must request, each entry in
	// name:arch=version form. It excludes files that are only in the pool to
	// satisfy a possible future need.
	Install []string `json:"install"`

	Warnings   []lock.Warning    `json:"warnings,omitempty"`
	Unresolved []lock.Unresolved `json:"unresolved,omitempty"`
}

// Selection is one chosen file.
type Selection struct {
	Name    string `json:"name"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
	// SourcePackage is the source name without its version.
	SourcePackage string `json:"source_package,omitempty"`
	// Section is the apt section (e.g. "universe/net"), used to derive
	// component flags such as multiverse or non-free.
	Section string `json:"section,omitempty"`
	// Filename is the file's BASE NAME, e.g. vlc_3.0.21-1build1_amd64.deb.
	// Note the difference from lock.Package.Filename, which is the full pool
	// path inside the bundle: a Selection describes a file that has not been
	// placed in a bundle yet.
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`

	// URI is where the file came from: the archive URI apt used, or the
	// vendor URL. Empty for a local file input.
	URI    string      `json:"uri,omitempty"`
	Origin lock.Origin `json:"origin"`

	// Reason is requested, upgrade, external or dependency-of:<pkg>.
	Reason string `json:"reason"`
	// PublisherVerification is the strength of the provenance claim.
	PublisherVerification lock.PublisherVerification `json:"publisher_verification"`

	// Essential marks packages the dpkg fallback must unpack first.
	Essential bool `json:"essential,omitempty"`
	// Flags carries redistribution and doctor findings known at resolve time.
	Flags []string `json:"flags,omitempty"`

	// StagedPath is where the file currently is on disk: apt's archives dir,
	// the external staging dir, or a store path. Empty means not yet fetched.
	//
	// It is a builder-side path and is deliberately NOT part of any durable
	// artefact: it crosses the resolve-contract boundary only because the
	// container backend must tell the host where inside the mounted archives
	// directory each file landed.
	StagedPath string `json:"staged_path,omitempty"`
	// UserSupplied marks files the operator provided, which are never pruned.
	UserSupplied bool `json:"user_supplied,omitempty"`
}

// Key is the identity used for deduplication and pruning: a package is one
// (name, arch) pair, and only its version varies.
func (s Selection) Key() string { return s.Name + ":" + s.Arch }

// NameVersion renders the selection as apt's exact-version install syntax.
func (s Selection) NameVersion() string { return s.Name + "=" + s.Version }

// DependencyOf builds the reason string for a package pulled in by another.
func DependencyOf(pkg string) string { return lock.ReasonDependencyOfPrefix + pkg }

// IsDependency reports whether a reason string names a dependency edge, and of
// what.
func IsDependency(reason string) (string, bool) {
	if rest, ok := strings.CutPrefix(reason, lock.ReasonDependencyOfPrefix); ok {
		return rest, true
	}
	return "", false
}

// SortSelections orders selections deterministically by name, then
// architecture, then version. Every derived artefact is written from this
// order, which is what makes bundles byte-identical across runs.
func SortSelections(s []Selection) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Name != s[j].Name {
			return s[i].Name < s[j].Name
		}
		if s[i].Arch != s[j].Arch {
			return s[i].Arch < s[j].Arch
		}
		return s[i].Version < s[j].Version
	})
}
