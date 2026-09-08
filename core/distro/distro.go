// Package distro is the release table: codename to version mapping, the
// container image used to resolve for each supported release, and the
// architecture names apt and containers use.
//
// Supported and CI-tested for 1.0: Debian 12 and 13, Ubuntu 22.04, 24.04 and
// 26.04, on amd64 and arm64. Everything else is best effort and says so.
package distro

import (
	"sort"
	"strings"

	"github.com/inferops/debark/core/dferr"
)

// Distro IDs as they appear in /etc/os-release.
const (
	Debian = "debian"
	Ubuntu = "ubuntu"
)

// Release is one distribution release.
type Release struct {
	DistroID  string
	VersionID string
	Codename  string
	// Supported marks the rows the CI matrix covers. A best-effort row still
	// resolves and builds; it is just not tested every night.
	Supported bool
	// Image is the container image used when resolution must run in a
	// container of the target release.
	Image string
	// ImageDigest pins Image by content, so a rebuild months later resolves
	// against the same apt the lock recorded rather than whatever the tag
	// points at that day. Regenerate with `go run hack/pin-images.go`, which
	// prints pasteable rows; these were resolved from docker.io on
	// 2026-09-03. An empty digest means the tag is used as-is and the build
	// warns, which is a supported but second-class mode.
	ImageDigest string
	// EOL is the release's end of standard support, informational only.
	EOL string
}

// Ref returns the image reference to run: pinned by digest when one is known.
func (r Release) Ref() string {
	if r.ImageDigest == "" {
		return r.Image
	}
	return stripImageTag(r.Image) + "@" + r.ImageDigest
}

// stripImageTag removes the tag or digest suffix from an OCI reference,
// leaving "[registry[:port]/]repository".
//
// The naive rule -- cut at the first ':' -- is wrong for any registry that
// carries a port, and wrong in the dangerous direction: it turns
// "registry.internal:5000/library/debian:trixie-slim" into
// "registry.internal", which is not a truncated form of the intended image
// but a completely different one (a bare name resolves against Docker Hub).
// Every row in this file's table happens to be port-free, so nothing in the
// tree hits that today; Release is exported and Ref is a method on it, so the
// rule is written correctly rather than left to depend on the table's current
// contents.
func stripImageTag(image string) string {
	// A digest suffix covers everything after it, port or no port.
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	// Only a ':' after the final '/' separates a tag; an earlier one is a
	// registry port. LastIndexByte returns -1 when absent, which makes the
	// no-slash and no-colon cases fall out of the same comparison.
	slash := strings.LastIndexByte(image, '/')
	if i := strings.LastIndexByte(image, ':'); i > slash {
		image = image[:i]
	}
	return image
}

// releases is the table. Order is stable for documentation generation.
var releases = []Release{
	{DistroID: Debian, VersionID: "11", Codename: "bullseye", Image: "docker.io/library/debian:bullseye-slim", ImageDigest: "sha256:e5b6442dd2e9684cf5e87d8338b5968f3b348636fc0be6d7850a381e3731a2bd", EOL: "2024-07-01"},
	{DistroID: Debian, VersionID: "12", Codename: "bookworm", Supported: true, Image: "docker.io/library/debian:bookworm-slim", ImageDigest: "sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171", EOL: "2028-06-30"},
	{DistroID: Debian, VersionID: "13", Codename: "trixie", Supported: true, Image: "docker.io/library/debian:trixie-slim", ImageDigest: "sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132", EOL: "2030-06-30"},
	{DistroID: Debian, VersionID: "14", Codename: "forky", Image: "docker.io/library/debian:forky-slim", ImageDigest: "sha256:91b0aaebf7a1ccacfe7a9cbff6ab2d6be7d9b3b6cf1dfcf44b25f9095c0e0464"},
	{DistroID: Ubuntu, VersionID: "20.04", Codename: "focal", Image: "docker.io/library/ubuntu:20.04", ImageDigest: "sha256:8feb4d8ca5354def3d8fce243717141ce31e2c428701f6682bd2fafe15388214", EOL: "2025-05-31"},
	{DistroID: Ubuntu, VersionID: "22.04", Codename: "jammy", Supported: true, Image: "docker.io/library/ubuntu:22.04", ImageDigest: "sha256:2edbbc5dc405e9612ba3584ce95480277e3eb374407b5505fe26f17df77c7dbc", EOL: "2027-06-01"},
	{DistroID: Ubuntu, VersionID: "24.04", Codename: "noble", Supported: true, Image: "docker.io/library/ubuntu:24.04", ImageDigest: "sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517", EOL: "2029-06-01"},
	{DistroID: Ubuntu, VersionID: "24.10", Codename: "oracular", Image: "docker.io/library/ubuntu:24.10", ImageDigest: "sha256:cdf755952ed117f6126ff4e65810bf93767d4c38f5c7185b50ec1f1078b464cc", EOL: "2025-07-01"},
	{DistroID: Ubuntu, VersionID: "25.04", Codename: "plucky", Image: "docker.io/library/ubuntu:25.04", ImageDigest: "sha256:27771fb7b40a58237c98e8d3e6b9ecdd9289cec69a857fccfb85ff36294dac20"},
	{DistroID: Ubuntu, VersionID: "25.10", Codename: "questing", Image: "docker.io/library/ubuntu:25.10", ImageDigest: "sha256:7cc5e35f6567ee8c66d2abb4aab0fd866669e6207c237c3a8f0947a5c7f17092"},
	{DistroID: Ubuntu, VersionID: "26.04", Codename: "resolute", Supported: true, Image: "docker.io/library/ubuntu:26.04", ImageDigest: "sha256:2260313b31c8c011cd2eebe728008efac1b3982be73eb71348ea2648d2c0e09b", EOL: "2031-06-01"},
}

// All returns every known release.
func All() []Release {
	out := make([]Release, len(releases))
	copy(out, releases)
	return out
}

// Supported returns the CI-tested releases.
func Supported() []Release {
	var out []Release
	for _, r := range releases {
		if r.Supported {
			out = append(out, r)
		}
	}
	return out
}

// Lookup finds a release by distro id and version id.
func Lookup(distroID, versionID string) (Release, bool) {
	distroID = strings.ToLower(distroID)
	for _, r := range releases {
		if r.DistroID == distroID && r.VersionID == versionID {
			return r, true
		}
	}
	return Release{}, false
}

// ByCodename finds a release by codename, which is unique across Debian and
// Ubuntu in practice.
func ByCodename(codename string) (Release, bool) {
	codename = strings.ToLower(codename)
	for _, r := range releases {
		if r.Codename == codename {
			return r, true
		}
	}
	return Release{}, false
}

// Resolve finds the best release row for a target, accepting either a version
// id or a codename.
//
// This is the only path from a snapshot's self-declared identity to a
// container image, so it is deliberately a closed lookup over the table
// above: the three arguments are only ever compared against fixed strings,
// never concatenated into the returned Image or Ref. An identity that is not
// in the table produces no image at all -- it fails, with a class, rather
// than letting an untrusted snapshot name the image the builder runs.
//
// The codename fallback is what makes Debian/Ubuntu derivatives work at all
// (Mint, Pop!_OS and friends report their own ID and VERSION_ID but the
// upstream VERSION_CODENAME), so it deliberately does not require the row's
// DistroID to match distroID. It still only ever selects a row that is
// already in the table.
func Resolve(distroID, versionID, codename string) (Release, error) {
	if r, ok := Lookup(distroID, versionID); ok {
		return r, nil
	}
	if r, ok := ByCodename(codename); ok {
		return r, nil
	}
	return Release{}, dferr.New(dferr.Usage,
		"distro: unknown release %q %q (%q); known releases: %s",
		distroID, versionID, codename, strings.Join(knownReleaseNames(), ", "))
}

// knownReleaseNames renders the table for the "unknown release" diagnostic,
// so an operator who mistyped a target sees the closed set they can choose
// from instead of having to go and find it.
func knownReleaseNames() []string {
	out := make([]string, 0, len(releases))
	for _, r := range releases {
		out = append(out, r.DistroID+" "+r.VersionID+" ("+r.Codename+")")
	}
	return out
}

// Architecture mapping. dpkg names and container platform names differ, and
// getting it wrong silently builds the wrong bundle.
var archToPlatform = map[string]string{
	"amd64":   "linux/amd64",
	"arm64":   "linux/arm64",
	"armhf":   "linux/arm/v7",
	"armel":   "linux/arm/v6",
	"i386":    "linux/386",
	"ppc64el": "linux/ppc64le",
	"s390x":   "linux/s390x",
	"riscv64": "linux/riscv64",
}

// Platform returns the container platform string for a dpkg architecture.
func Platform(dpkgArch string) (string, bool) {
	p, ok := archToPlatform[dpkgArch]
	return p, ok
}

// Architectures lists every dpkg architecture debark knows how to build for,
// sorted.
func Architectures() []string {
	out := make([]string, 0, len(archToPlatform))
	for a := range archToPlatform {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// PrimaryArchitectures are the architectures the CI matrix covers.
func PrimaryArchitectures() []string { return []string{"amd64", "arm64"} }

// Components are the archive components whose redistribution terms differ from
// the distribution's free component, mapped to the lock flag they raise.
var restrictedComponents = map[string]string{
	"multiverse":        "multiverse",
	"restricted":        "restricted",
	"non-free":          "non-free",
	"non-free-firmware": "non-free-firmware",
	"contrib":           "", // free-ish but depends on non-free; no flag, doctor mentions it
}

// ComponentFlag returns the lock flag a component raises, and whether it raises
// one at all.
func ComponentFlag(component string) (string, bool) {
	component = strings.ToLower(strings.TrimSpace(component))
	// A Section field looks like "universe/net" or "net"; take the component.
	if i := strings.IndexByte(component, '/'); i >= 0 {
		component = component[:i]
	}
	flag, ok := restrictedComponents[component]
	if !ok || flag == "" {
		return "", false
	}
	return flag, true
}
