package base

import (
	"fmt"
	"runtime"
	"sort"
	"strings"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/distro"
)

// The builtin table.
//
// It covers exactly the releases the project already commits to CI-testing — Debian
// 12 and 13, Ubuntu 22.04, 24.04 and 26.04 — and no others. That is not a
// staffing limit, it is the point: the design already budgets one maintenance
// release per new distro release, and a table that tracked every derivative
// would be unbounded work debark cannot honestly promise. Anything else is
// served by an operator-supplied definition file (parse.go), which is a
// better answer anyway — an organisation's real baseline is almost never
// stock Ubuntu, it is their own golden image.
//
// There is NO hosted catalogue and no network call to anything debark
// operates, here or anywhere else (D8). Everything a base needs is either in
// this file or on infrastructure the project does not run: the seeds resolve
// against the distro's own archive, the sources are generated below, and the
// keyrings are read from the machine that does the resolving.

// variant is one row of the table, before a release and an architecture are
// applied to it.
type variant struct {
	name        string
	description string
	// seeds for Debian and for Ubuntu respectively. They are listed
	// separately rather than shared because the two distributions genuinely
	// disagree about what a "default install" is expressed as: Ubuntu ships
	// real ubuntu-* metapackages for each install profile, Debian does not
	// and its installer works from tasksel tasks and package priorities.
	debianSeeds []string
	ubuntuSeeds []string
}

// variants is ordered least-inclusive first, which is also the order
// `snapshot list-bases` shows within a release.
var variants = []variant{
	{
		name:        "minimal",
		description: "a minimal install: the base system and apt, nothing chosen by an installer profile",
		// apt and init between them pull in essentially every
		// Priority: required package plus the init system, which is the
		// closest apt can get to debootstrap's output without this package
		// hand-writing a list — which it may not do (ADR-001).
		debianSeeds: []string{"apt", "init"},
		ubuntuSeeds: []string{"ubuntu-minimal"},
	},
	{
		name:        "server",
		description: "a server install: the minimal system plus the standard server set",
		debianSeeds: []string{"apt", "init", "openssh-server"},
		// ubuntu-server-minimal, not ubuntu-server: the smaller of the two
		// real metapackages, per this package's err-toward-not-installed
		// rule. A machine that actually has the full ubuntu-server set is a
		// superset of this claim, and a superset is safe.
		ubuntuSeeds: []string{"ubuntu-server-minimal"},
	},
	{
		name:        "desktop",
		description: "a desktop install: the minimal system plus the default desktop environment",
		debianSeeds: []string{"apt", "init", "task-gnome-desktop"},
		// ubuntu-desktop-minimal, not ubuntu-desktop, for the same reason:
		// it is exactly what the installer's "Minimal installation" option
		// gives, and a full desktop install is a superset of it. Claiming
		// ubuntu-desktop and meeting a minimally-installed machine would
		// produce a SHORT bundle.
		ubuntuSeeds: []string{"ubuntu-desktop-minimal"},
	},
}

// builtinReleases are the CI-tested rows this table covers, in the order
// list-bases shows them.
var builtinReleases = []struct {
	distroID  string
	versionID string
}{
	{distro.Debian, "12"},
	{distro.Debian, "13"},
	{distro.Ubuntu, "22.04"},
	{distro.Ubuntu, "24.04"},
	{distro.Ubuntu, "26.04"},
}

// Archive layout. These four constants are the only URLs compiled into
// debark, and they are the distributions' own canonical archives — not
// mirrors debark chose, and emphatically not anything debark runs.
const (
	ubuntuArchive      = "http://archive.ubuntu.com/ubuntu"
	ubuntuSecurity     = "http://security.ubuntu.com/ubuntu"
	ubuntuPortsArchive = "http://ports.ubuntu.com/ubuntu-ports"
	debianArchive      = "http://deb.debian.org/debian"
	debianSecurity     = "http://security.debian.org/debian-security"
)

// ubuntuExcludes are packages an Ubuntu seed closure names that a real Ubuntu
// image does not install, and which the base must therefore not claim.
//
// lsb-base (version 11.6 in noble) is a TRANSITIONAL package -- its own
// description says so -- carrying nothing but a dependency on sysvinit-utils
// for Linux Standard Base init-script compatibility. apt resolving
// ubuntu-desktop-minimal from an empty root selects it; the published
// ubuntu-24.04.4-desktop-amd64.manifest does not list it, and a real noble
// system reports it "not-installed". That is the short-bundle direction: a
// base claiming it would make `build` omit it, and any requested package
// depending on it would then fail to install on the far side of the gap.
//
// Measured on 2026-09-06 (docs/experiments/E8-base-fidelity.md): it was the
// only assumed-but-absent package across all three Ubuntu 24.04 variants.
//
// It is excluded from EVERY Ubuntu base, not only desktop, even though the
// server image does carry it. Two reasons, both about the asymmetry this
// package is built on. Excluding it where it is genuinely present costs one
// small package of bundle size and nothing else. And E8 has so far measured
// one release; a per-variant exclusion fitted to that single measurement
// would be claiming knowledge about 22.04 and 26.04 that nobody has.
var ubuntuExcludes = []string{"lsb-base"}

// Keyring paths on the resolving system. Both are the path the distribution's
// own keyring package installs to, which is where a matching host and the
// pinned container image of the release both already have it.
const (
	ubuntuKeyring = "/usr/share/keyrings/ubuntu-archive-keyring.gpg"
	debianKeyring = "/usr/share/keyrings/debian-archive-keyring.gpg"
)

// ubuntuPortsArches are the architectures Ubuntu serves from ports.ubuntu.com
// rather than archive.ubuntu.com. Getting this wrong does not produce a
// subtly wrong bundle — apt simply finds nothing — but it produces it late,
// after a container has started, so it is a closed set here rather than a
// guess at request time.
var ubuntuPortsArches = map[string]bool{
	"arm64": true, "armhf": true, "ppc64el": true, "s390x": true, "riscv64": true,
}

// ubuntuComponents and debianComponents are what the respective installers
// enable by default.
//
// Debian's default is narrower than this in one respect: a stock netinst
// enables main and non-free-firmware, and only adds contrib and non-free if
// the operator asks. All four are enabled here so that a package an operator
// asks `build --base` for actually resolves, rather than failing with a
// "not found" that looks like a debark bug when it is a component choice.
// The cost is bounded and already handled: anything selected out of contrib
// or non-free raises the redistribution flag core/distro.ComponentFlag
// already defines, and the lock records it. The benefit to the *installed
// set* is what E8 measures, since a seed resolving through non-free would be
// claiming packages a main-only machine does not have.
var (
	ubuntuComponents = "main restricted universe multiverse"
	debianComponents = "main contrib non-free non-free-firmware"
)

// ubuntuSources renders the deb822 document for an Ubuntu release.
//
// -updates and -backports are included because Ubuntu's own installer enables
// them; -security comes from the separate security host on the primary
// architectures and from the same ports host everywhere else, which is how
// Ubuntu actually publishes it.
func ubuntuSources(arch string) string {
	if ubuntuPortsArches[arch] {
		return fmt.Sprintf(`Types: deb
URIs: %s
Suites: ${codename} ${codename}-updates ${codename}-backports ${codename}-security
Components: %s
Signed-By: %s
`, ubuntuPortsArchive, ubuntuComponents, ubuntuKeyring)
	}
	return fmt.Sprintf(`Types: deb
URIs: %s
Suites: ${codename} ${codename}-updates ${codename}-backports
Components: %s
Signed-By: %s

Types: deb
URIs: %s
Suites: ${codename}-security
Components: %s
Signed-By: %s
`, ubuntuArchive, ubuntuComponents, ubuntuKeyring,
		ubuntuSecurity, ubuntuComponents, ubuntuKeyring)
}

// debianSources renders the deb822 document for a Debian release. No
// -backports: Debian's installer does not enable it, and a base must describe
// what an installer produces rather than what an administrator might add.
func debianSources() string {
	return fmt.Sprintf(`Types: deb
URIs: %s
Suites: ${codename} ${codename}-updates
Components: %s
Signed-By: %s

Types: deb
URIs: %s
Suites: ${codename}-security
Components: %s
Signed-By: %s
`, debianArchive, debianComponents, debianKeyring,
		debianSecurity, debianComponents, debianKeyring)
}

// BuiltinID renders the canonical id for a release and variant.
func BuiltinID(distroID, versionID, variantName string) string {
	return distroID + ":" + versionID + "/" + variantName
}

// ParseID splits "<distro>:<version>[/<variant>]", defaulting the variant.
//
// It does not check the parts against the table — Lookup does that — so the
// same parse serves an operator-supplied id that is not in the table at all.
func ParseID(id string) (distroID, versionID, variantName string, err error) {
	rest, variantName, hasVariant := strings.Cut(id, "/")
	if !hasVariant {
		variantName = DefaultVariant
	}
	distroID, versionID, ok := strings.Cut(rest, ":")
	if !ok || strings.TrimSpace(distroID) == "" || strings.TrimSpace(versionID) == "" || strings.TrimSpace(variantName) == "" {
		return "", "", "", dferr.New(dferr.Usage,
			"base: %q is not a base id or a path to a base definition file", id).
			WithHint("a builtin id looks like ubuntu:26.04/desktop; run `debark snapshot list-bases` to see them all, or pass a path to a .yaml definition")
	}
	return strings.ToLower(distroID), versionID, strings.ToLower(variantName), nil
}

// Builtin returns every compiled-in definition, materialised for arch, in
// display order.
func Builtin(arch string) ([]Definition, error) {
	if err := checkArch(arch); err != nil {
		return nil, err
	}
	var out []Definition
	for _, r := range builtinReleases {
		for _, v := range variants {
			d, err := buildBuiltin(r.distroID, r.versionID, v, arch)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
	}
	return out, nil
}

// DistroIDs is every distribution in the table, in table order.
func DistroIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range builtinReleases {
		if !seen[r.distroID] {
			seen[r.distroID] = true
			out = append(out, r.distroID)
		}
	}
	return out
}

// ReleaseIDs is every "<distro>:<version>" for one distribution, in table
// order — which is oldest first, so the newest release is last.
func ReleaseIDs(distroID string) []string {
	var out []string
	for _, r := range builtinReleases {
		if r.distroID == distroID {
			out = append(out, r.distroID+":"+r.versionID)
		}
	}
	return out
}

// VariantNames is every install profile, in table order.
//
// The order is not cosmetic and callers must not re-sort it: variants are
// listed LEAST INCLUSIVE FIRST, so the first entry is always the smallest
// claim a base can make. Anything choosing a default from this list — the
// guided flow's picker does — gets the safe answer by taking the first, and
// the unsafe one by taking any other. Sorting alphabetically puts "desktop"
// first, which is the largest claim of the three and the exact inversion of
// this package's err-toward-not-installed rule.
func VariantNames() []string { return variantNames() }

// VariantDescription is the one-line description of an install profile,
// so a picker and `snapshot list-bases` describe it identically.
func VariantDescription(name string) string {
	for _, v := range variants {
		if v.name == name {
			return v.description
		}
	}
	return ""
}

// BuiltinIDs is every id in the table, sorted. Used by `snapshot list-bases`
// and by the shell-completion path, neither of which needs an architecture.
//
// Sorted, deliberately, and therefore NOT the right source for a picker's
// option list: see VariantNames.
func BuiltinIDs() []string {
	var out []string
	for _, r := range builtinReleases {
		for _, v := range variants {
			out = append(out, BuiltinID(r.distroID, r.versionID, v.name))
		}
	}
	sort.Strings(out)
	return out
}

// Lookup finds a builtin definition by id and materialises it for arch.
//
// The returned definition has every placeholder substituted and has passed
// Validate, so a caller can digest it and hand it to Synthesize directly. A
// failure here is a usage error naming the closed set the operator can
// choose from — the same shape core/distro.Resolve uses, and for the same
// reason: an id that is not in the table must produce no archive and no
// image at all, rather than being concatenated into one.
func Lookup(id, arch string) (Definition, error) {
	distroID, versionID, variantName, err := ParseID(id)
	if err != nil {
		return Definition{}, err
	}
	if err := checkArch(arch); err != nil {
		return Definition{}, err
	}
	for _, r := range builtinReleases {
		if r.distroID != distroID || r.versionID != versionID {
			continue
		}
		for _, v := range variants {
			if v.name != variantName {
				continue
			}
			return buildBuiltin(r.distroID, r.versionID, v, arch)
		}
		return Definition{}, dferr.New(dferr.Usage,
			"base: %s has no %q variant; known variants are %s",
			distroID+":"+versionID, variantName, strings.Join(variantNames(), ", "))
	}
	return Definition{}, dferr.New(dferr.Usage,
		"base: no builtin definition for %q; known bases are %s",
		id, strings.Join(BuiltinIDs(), ", ")).
		WithHint("a base debark does not ship can be described in a file: `debark snapshot from-base ./our-soe.yaml`")
}

// IsBuiltinID reports whether id names a row in the table. Used to tell an
// id apart from a path without touching the filesystem.
func IsBuiltinID(id string) bool {
	distroID, versionID, variantName, err := ParseID(id)
	if err != nil {
		return false
	}
	for _, r := range builtinReleases {
		if r.distroID == distroID && r.versionID == versionID {
			for _, v := range variants {
				if v.name == variantName {
					return true
				}
			}
		}
	}
	return false
}

func variantNames() []string {
	out := make([]string, 0, len(variants))
	for _, v := range variants {
		out = append(out, v.name)
	}
	return out
}

// checkArch refuses an architecture debark has no container platform for,
// because a base that cannot be resolved is not a base.
func checkArch(arch string) error {
	if arch == "" {
		return dferr.New(dferr.Usage, "base: no architecture given")
	}
	if _, ok := distro.Platform(arch); !ok {
		return dferr.New(dferr.Usage, "base: unknown architecture %q; known architectures are %s",
			arch, strings.Join(distro.Architectures(), ", "))
	}
	return nil
}

func buildBuiltin(distroID, versionID string, v variant, arch string) (Definition, error) {
	rel, ok := distro.Lookup(distroID, versionID)
	if !ok {
		// Unreachable from the table above, and a bug rather than operator
		// error if it ever fires: builtinReleases and core/distro's table
		// must name the same releases. builtin_test.go asserts exactly that,
		// so this branch exists to make the failure loud rather than to be
		// reached.
		return Definition{}, dferr.New(dferr.Usage,
			"base: builtin table names %s %s, which core/distro does not know", distroID, versionID)
	}

	var seeds, excludes []string
	var sources string
	var keyring string
	switch distroID {
	case distro.Debian:
		// No measured exclusions for Debian: E8 has only been run against
		// Ubuntu, whose images publish a manifest to compare against. An
		// unmeasured exclusion would be a guess, and this table does not
		// guess.
		seeds, sources, keyring = v.debianSeeds, debianSources(), debianKeyring
	case distro.Ubuntu:
		seeds, sources, keyring = v.ubuntuSeeds, ubuntuSources(arch), ubuntuKeyring
		excludes = ubuntuExcludes
	default:
		return Definition{}, dferr.New(dferr.Usage, "base: no archive layout known for distro %q", distroID)
	}

	d := Definition{
		SchemaVersion: SchemaVersion,
		ID:            BuiltinID(distroID, versionID, v.name),
		Description:   fmt.Sprintf("%s %s (%s) — %s", displayDistro(distroID), versionID, rel.Codename, v.description),
		DistroID:      distroID,
		VersionID:     versionID,
		Codename:      rel.Codename,
		Variant:       v.name,
		Seeds:         append([]string(nil), seeds...),
		Excludes:      append([]string(nil), excludes...),
		Recommends:    false,
		Sources:       sources,
		Keyrings:      []string{keyring},
	}
	d = d.Materialise(arch)
	if err := Validate(d); err != nil {
		return Definition{}, err
	}
	return d, nil
}

func displayDistro(distroID string) string {
	switch distroID {
	case distro.Debian:
		return "Debian"
	case distro.Ubuntu:
		return "Ubuntu"
	default:
		return distroID
	}
}

// goArchToDpkg maps Go's GOARCH names to dpkg's. The two agree on amd64,
// arm64 and s390x and disagree on everything else, and the disagreements are
// exactly where a silent wrong answer would build a bundle for the wrong
// machine.
var goArchToDpkg = map[string]string{
	"amd64":   "amd64",
	"arm64":   "arm64",
	"386":     "i386",
	"arm":     "armhf",
	"ppc64le": "ppc64el",
	"s390x":   "s390x",
	"riscv64": "riscv64",
}

// HostArch is the dpkg architecture of the machine debark is running on,
// which is what --arch defaults to.
//
// It is a default, not a claim about the target: someone deriving a base
// without saying which architecture almost always means the kind of machine
// they are sitting at. When this binary was built for something dpkg has no
// name for, it returns "amd64" — the only choice that is a usable default at
// all — and callers that care can require --arch instead. That fallback is
// safe in the direction that matters: an architecture mismatch is refused
// loudly by `install` (exit 7), never resolved into a wrong bundle
// that installs.
func HostArch() string {
	if a, ok := goArchToDpkg[runtime.GOARCH]; ok {
		return a
	}
	return "amd64"
}
