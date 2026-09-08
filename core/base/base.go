// Package base is baseline OS support: describing a target debark has never
// seen, so a bundle can be built for a machine that does not exist yet
// (ADR-014).
//
// The unit is a Definition — a stock release, its architecture, the seed
// packages that stand for "a default install of it", and the archive those
// seeds resolve against. Synthesize turns one into a *synthesized snapshot*:
// the same debark.snapshot/v1 document `snapshot create` produces on a real
// machine, carrying origin.kind = "synthesized" so nothing downstream can
// mistake an assumption for a measurement.
//
// Two rules govern everything here.
//
// **apt is still the oracle.** A Definition names seeds; it never names a
// closure. What a stock install contains is decided by the release's own apt
// resolving those seeds in a private root where nothing is installed
// (core/apt's Closure). No package list in this package was written by hand,
// and none may be.
//
// **Err toward "not installed", every time.** A base that claims a package
// the real machine turns out to lack produces a bundle that is SHORT, and a
// short bundle fails at the far side of the air gap — the exact failure
// debark exists to prevent. A base that claims too little produces a bundle
// that is merely larger than it needed to be. The two errors are not
// symmetric, so neither are the choices in builtin.go: every variant seeds
// the smaller of two plausible metapackages, and Recommends defaults off.
// docs/experiments/E8-base-fidelity.md measures both directions.
package base

import (
	"sort"
	"strings"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
)

// SchemaVersion is the only base-definition schema debark reads or writes.
const SchemaVersion = "debark.base/v1"

// DefaultVariant is the variant a bare "distro:version" id resolves to.
//
// minimal, not desktop or server, because a bare id is the least informed
// request this package can receive and the smallest claim is the safe answer
// to it: the bundle comes out larger than it needed to be rather than short.
const DefaultVariant = "minimal"

// Definition is one baseline.
//
// It is a plain value with no behaviour of its own beyond rendering and
// validation, so that it serialises to YAML for an operator to commit to git,
// serialises to canonical JSON for Digest below, and can be compared field by
// field in a test. A builtin definition and one read from an operator's file
// are the same type: there is no privileged shape, which is what keeps
// "which bases do you support?" from becoming unbounded maintenance.
type Definition struct {
	// SchemaVersion is always SchemaVersion.
	SchemaVersion string `json:"schema_version" yaml:"schema_version"`
	// ID names the base: "<distro>:<version>/<variant>", e.g.
	// "ubuntu:26.04/desktop". An operator-supplied file may use any id that
	// is not one of the builtin ones.
	ID string `json:"id" yaml:"id"`
	// Description is one line for `snapshot list-bases`.
	Description string `json:"description,omitempty" yaml:"description,omitempty"`

	// DistroID, VersionID and Codename are the target identity, written
	// straight into the synthesized snapshot's Target and used to pick the
	// container image (core/distro.Resolve).
	DistroID  string `json:"distro_id" yaml:"distro_id"`
	VersionID string `json:"version_id" yaml:"version_id"`
	Codename  string `json:"codename" yaml:"codename"`
	// Variant is desktop, server, minimal or an operator's own word. It is
	// documentation, not behaviour: Seeds is what actually differs.
	Variant string `json:"variant,omitempty" yaml:"variant,omitempty"`
	// Arch is the dpkg architecture this definition was materialised for.
	// A builtin definition takes it from Lookup's argument; a file may pin
	// one or leave it empty to accept whatever is asked for.
	Arch string `json:"arch" yaml:"arch"`

	// Seeds are the metapackages whose closure stands for a stock install.
	// Resolved by apt, never expanded here.
	Seeds []string `json:"seeds" yaml:"seeds"`
	// Recommends is APT::Install-Recommends for the seed resolution.
	//
	// It defaults to false, and that default is load-bearing rather than
	// conservative-by-habit. A recommended package apt would pull in but the
	// real installer did not take is a package this base would claim as
	// installed when it is not — the short-bundle direction. Turning it on
	// makes the base's claim strictly larger, so it should only ever be set
	// by someone who has measured that their real image matches.
	Recommends bool `json:"recommends" yaml:"recommends"`

	// Sources is the apt sources for the target, as a deb822 document
	// (Types:/URIs:/Suites:/Components:/Signed-By:). It is written into the
	// synthesized snapshot verbatim as /etc/apt/sources.list.d/<distro>.sources,
	// so it is both what the seeds resolve against and what a later `build`
	// against this snapshot resolves against — one text, one meaning.
	//
	// ${codename}, ${version_id} and ${arch} are substituted before use; see
	// Expand.
	Sources string `json:"sources" yaml:"sources"`
	// Excludes are packages the seed closure may name but this base must NOT
	// claim the target already has.
	//
	// The safety argument is what makes this acceptable at all in a package
	// that may not hand-write package lists (ADR-001). An exclusion only ever
	// makes the base's claim SMALLER, and a smaller claim can only ever make
	// a bundle larger: the package moves from "assumed present" to "must be
	// carried". It cannot produce a short bundle, which is the one failure
	// that matters. A wrong exclusion costs bytes; a wrong inclusion costs an
	// install on the far side of an air gap.
	//
	// It exists because apt resolving a seed from scratch and a distribution
	// installer building an image do not always reach the same set, and the
	// difference is not always in the safe direction. Measured, not assumed:
	// docs/experiments/E8-base-fidelity.md. Every entry in a builtin
	// definition names the measurement that put it there.
	Excludes []string `json:"excludes,omitempty" yaml:"excludes,omitempty"`
	// Keyrings are absolute paths, on the machine that runs the seed
	// resolution, to the archive keyrings Sources' Signed-By lines name.
	//
	// They are read from the resolving system rather than shipped in the
	// binary on purpose: debark must never become a distributor of archive
	// keys (snapshot keyrings are captured policy, never an
	// independent root of trust), and the two places a base is ever resolved
	// both already have the right ones — a host whose distro matches the
	// target, or the pinned container image of the target release itself.
	Keyrings []string `json:"keyrings,omitempty" yaml:"keyrings,omitempty"`
}

// Expand substitutes ${codename}, ${version_id} and ${arch} in s.
//
// Deliberately three fixed names and no expression language. A base
// definition is a file an operator commits to git and a builder later runs
// apt against; the set of things it may vary is small and knowable, and
// anything richer would be a template engine deciding what archive a build
// talks to.
func (d Definition) Expand(s string) string {
	return strings.NewReplacer(
		"${codename}", d.Codename,
		"${version_id}", d.VersionID,
		"${arch}", d.Arch,
	).Replace(s)
}

// Materialise returns a copy of d with arch applied and every placeholder
// substituted, ready to be digested and resolved. It is the only way a
// Definition should reach Synthesize: a definition still carrying ${arch} has
// no single digest, and two builds of "the same base" would not be comparable.
func (d Definition) Materialise(arch string) Definition {
	out := d
	if arch != "" {
		out.Arch = arch
	}
	out.Sources = out.Expand(out.Sources)
	out.Keyrings = append([]string(nil), d.Keyrings...)
	for i, k := range out.Keyrings {
		out.Keyrings[i] = out.Expand(k)
	}
	out.Seeds = append([]string(nil), d.Seeds...)
	for i, s := range out.Seeds {
		out.Seeds[i] = out.Expand(s)
	}
	out.Excludes = append([]string(nil), d.Excludes...)
	for i, e := range out.Excludes {
		out.Excludes[i] = out.Expand(e)
	}
	return out
}

// Digest is the lowercase hex SHA-256 of the canonicalised definition, which
// is what a synthesized snapshot records in origin.source_digest.
//
// It exists because origin.base_id alone is not enough to tell two snapshots
// apart. "ubuntu:26.04/desktop" is a name, and a name can mean different
// things at different times — a maintenance release changing a seed, or an
// operator editing the golden-image file in their own repository. The digest
// makes that visible instead of silent: two snapshots claiming the same base
// with different digests were built against different assumptions, and an
// operator comparing them can see it.
func Digest(d Definition) (string, error) {
	return canonical.Digest(d)
}

// Validate checks a definition is usable before anything runs apt against it.
//
// Every failure is dferr.Usage: a base definition is operator input, whether
// it came from this binary's own table (in which case a failure here is a bug
// this package's tests must catch) or from a file the operator wrote.
func Validate(d Definition) error {
	if d.SchemaVersion != SchemaVersion {
		return dferr.New(dferr.Usage, "base: unknown schema_version %q (want %q)", d.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(d.ID) == "" {
		return dferr.New(dferr.Usage, "base: id is required")
	}
	// The id becomes origin.base_id, which travels into a bundle and is
	// printed by `install` on the target. An operator-supplied definition is
	// the one place a person types that string, so it is bounded and checked
	// for control characters here — the same rule core/snapshot applies to
	// every other string of its own that reaches a terminal, and for the same
	// reason (the threat model: a control sequence can scroll back over
	// the digest a human was told to compare).
	if err := checkDisplayString("id", d.ID); err != nil {
		return err
	}
	if err := checkDisplayString("description", d.Description); err != nil {
		return err
	}
	if err := checkDisplayString("variant", d.Variant); err != nil {
		return err
	}
	if strings.TrimSpace(d.DistroID) == "" {
		return dferr.New(dferr.Usage, "base %s: distro_id is required", d.ID)
	}
	if strings.TrimSpace(d.VersionID) == "" {
		return dferr.New(dferr.Usage, "base %s: version_id is required", d.ID)
	}
	if strings.TrimSpace(d.Codename) == "" {
		// Not optional, unlike in a captured snapshot where a stripped-down
		// system might genuinely lack VERSION_CODENAME. A base definition is
		// written, not measured, and the codename is what every suite name
		// in Sources is built from and what core/distro's fallback uses to
		// find a container image for a derivative.
		return dferr.New(dferr.Usage, "base %s: codename is required", d.ID)
	}
	if strings.TrimSpace(d.Arch) == "" {
		return dferr.New(dferr.Usage, "base %s: arch is required", d.ID)
	}
	if len(d.Seeds) == 0 {
		return dferr.New(dferr.Usage, "base %s: at least one seed package is required", d.ID).
			WithHint("seeds are the metapackages whose closure stands for a stock install, e.g. ubuntu-desktop-minimal")
	}
	for _, s := range d.Seeds {
		if strings.TrimSpace(s) == "" {
			return dferr.New(dferr.Usage, "base %s: empty seed package name", d.ID)
		}
	}
	for _, e := range d.Excludes {
		if strings.TrimSpace(e) == "" {
			return dferr.New(dferr.Usage, "base %s: empty exclude package name", d.ID)
		}
		if strings.Contains(e, ":") {
			// A bare name, never name:arch. The exclusion is a statement
			// about a package the installer does not install, which is a
			// fact about the package and not about one architecture of it;
			// accepting a qualified name would invite an exclusion that
			// silently applies to only half a multiarch target.
			return dferr.New(dferr.Usage,
				"base %s: exclude %q must be a bare package name, not an architecture-qualified one", d.ID, e)
		}
	}
	if strings.TrimSpace(d.Sources) == "" {
		return dferr.New(dferr.Usage, "base %s: sources is required", d.ID).
			WithHint("sources is the deb822 apt source document the seeds resolve against, and that a later build resolves against too")
	}
	if strings.Contains(d.Sources, "${") {
		return dferr.New(dferr.Usage, "base %s: sources still contains an unsubstituted ${...} placeholder; only ${codename}, ${version_id} and ${arch} are recognised", d.ID)
	}
	for _, k := range d.Keyrings {
		if !strings.HasPrefix(k, "/") {
			return dferr.New(dferr.Usage, "base %s: keyring %q must be an absolute path on the machine that resolves the seeds", d.ID, k)
		}
	}
	return nil
}

// maxDisplayString bounds a definition string that ends up in front of a
// human. It matches core/snapshot's own maxDisplayStringLen, because these
// strings end up in the same document and are read by the same code.
const maxDisplayString = 256

// checkDisplayString refuses a field that would be unbounded or unprintable
// once it reaches a terminal.
func checkDisplayString(field, s string) error {
	if len(s) > maxDisplayString {
		return dferr.New(dferr.Usage,
			"base: %s is %d bytes, longer than the %d allowed", field, len(s), maxDisplayString)
	}
	for _, r := range s {
		// Below 0x20 and 0x7F catch NUL, ESC, CR and LF; U+0080-U+009F catch
		// the 8-bit CSI a UTF-8 terminal will act on. Same two ranges
		// core/snapshot's hasControlChars uses, and the split is deliberate
		// there for the same reason.
		if r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) {
			return dferr.New(dferr.Usage, "base: %s contains a control character", field)
		}
	}
	return nil
}

// SortIDs orders base ids for display. Plain lexical order, so
// `snapshot list-bases` and the docs generated from it never disagree about
// which row comes first.
func SortIDs(ids []string) { sort.Strings(ids) }
