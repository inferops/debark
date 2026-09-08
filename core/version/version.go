// Package version carries the build identity that appears in every manifest,
// every evidence file and debark version --json.
//
// Values are set at link time:
//
//	-X github.com/inferops/debark/core/version.Version=1.0.0
//	-X github.com/inferops/debark/core/version.Commit=abc1234
//	-X github.com/inferops/debark/core/version.Date=2026-09-03T00:00:00Z
//	-X github.com/inferops/debark/core/version.Edition=official
//
// Edition is community unless the project's own release pipeline built the
// binary. It is branding and audit metadata, never a feature gate: no code path
// in this repository behaves differently because of its value (ADR-010).
package version

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"sync"

	"github.com/inferops/debark/core/manifest"
)

// Name is the tool's name as it appears in artefacts.
const Name = "debark"

var (
	// Version is the semantic version, or "dev" for an untagged build.
	Version = "dev"
	// Commit is the source revision.
	Commit = ""
	// Date is the build timestamp, RFC 3339 UTC.
	Date = ""
	// Edition is community, official or enterprise.
	Edition = manifest.EditionCommunity
)

// Info is the full build identity.
type Info struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	Edition   string `json:"edition"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
	// BuildDigest is the SHA-256 of the running executable, when it can be
	// read. It lets an auditor tie a bundle to a published release.
	BuildDigest string `json:"build_digest,omitempty"`
}

// Get returns the build identity.
func Get() Info {
	i := Info{
		Name:        Name,
		Version:     Version,
		Commit:      Commit,
		Date:        Date,
		Edition:     Edition,
		GoVersion:   runtime.Version(),
		Platform:    runtime.GOOS + "/" + runtime.GOARCH,
		BuildDigest: BuildDigest(),
	}
	// A binary built without -ldflags still knows its own revision and commit
	// time: the go command stamps them into debug.BuildInfo. No gate here --
	// every decision about what the stamp may fill in lives in applyVCSStamp,
	// so it is all reachable from a test.
	if bi, ok := debug.ReadBuildInfo(); ok {
		i = applyVCSStamp(i, bi.Settings)
	}
	return i
}

// applyVCSStamp fills Info.Commit and Info.Date from the vcs.* settings the go
// command embeds, never overwriting a value link time already set: an explicit
// -X wins over the stamp, because a release pipeline sets it on purpose.
//
// The two fields are decided independently. -X can legitimately set one and
// not the other (`go build -ldflags "-X ...version.Commit=$(git rev-parse
// HEAD)"` is a common one-liner), and deciding Date only when Commit happens
// to be empty silently dropped the build time for exactly those builds -- the
// ones whose provenance an auditor most needs pinned down.
//
// Split out of Get so it can be tested against hand-built settings:
// debug.BuildInfo is not injectable, and under `go test` the vcs stamp is
// absent altogether, so Get alone can never exercise this.
func applyVCSStamp(i Info, settings []debug.BuildSetting) Info {
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			if i.Commit == "" {
				i.Commit = s.Value
			}
		case "vcs.time":
			if i.Date == "" {
				i.Date = s.Value
			}
		}
	}
	return i
}

var (
	digestOnce sync.Once
	digestVal  string
)

// BuildDigest returns the SHA-256 of the running executable, or "" when it
// cannot be read. It is computed at most once.
func BuildDigest() string {
	digestOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(exe)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return
		}
		digestVal = hex.EncodeToString(h.Sum(nil))
	})
	return digestVal
}

// Tool renders the identity as the manifest and snapshot tool block.
func Tool() manifest.Tool {
	i := Get()
	return manifest.Tool{
		Name:        i.Name,
		Version:     i.Version,
		BuildDigest: i.BuildDigest,
		Edition:     i.Edition,
	}
}

// String is the one-line human version, e.g. "debark 1.0.0 (community)".
func String() string {
	i := Get()
	return i.Name + " " + i.Version + " (" + i.Edition + ")"
}
