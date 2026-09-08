// Frozen public API of the apt package, including the container backend.

package apt

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"
)

// Selection is the input to backend auto-selection.
type Selection struct {
	// Target is the snapshot's target identity.
	Target snapshot.Target
	// Requested is auto, local or container. Empty means auto.
	Requested lock.Backend
	// Image overrides the container image.
	Image string
	// Events receives backend.selected. Optional.
	Events evidence.Sink
	// SelfPath is the static linux/<arch> debark the container backend
	// mounts and re-enters. Empty means bin/debark-linux-<arch> beside the
	// running executable, else os.Executable() (containerSelfPath).
	SelfPath string
}

// SelectBackend applies the rule, sharpened by experiment E2
// (docs/experiments/E2-solver-divergence.md, which found that divergence
// tracks the effective solver, not apt major.minor):
//
//	local     when this host has apt, its distro id equals the target's, and
//	          its effective APT::Solver equals the target's expected one
//	container otherwise
//
// apt major.minor is still compared and recorded (as a non-blocking note at
// selection time, and as a lock.Warning at Resolve time), but no longer
// gates the choice by itself: E2 found two apt versions that agree on solver
// never diverged, while one apt version can diverge from itself via nothing
// more than -o APT::Solver=internal, which major.minor can never detect. See
// core/apt/solver.go and select.go's localMismatchReason for the full
// mechanism and its one known limitation (the target side can only be
// inferred from its apt version at selection time; the real captured
// apt.conf.d override is only checked once Resolve has the full snapshot).
//
// A requested backend is honoured when it is available and refused with a
// clear reason when it is not; auto never silently degrades to a second-class
// resolver, and a missing container runtime is an environment error carrying
// the exact command that fixes it.
func SelectBackend(ctx context.Context, sel Selection) (Backend, Capabilities, error) {
	return selectBackend(ctx, sel)
}

// NewLocalBackend returns the backend that runs apt-get on this machine.
func NewLocalBackend(opts LocalOptions) Backend { return newLocalBackend(opts) }

// LocalOptions configures the local backend.
type LocalOptions struct {
	// AptGetPath overrides the apt-get binary, for tests.
	AptGetPath string
	// DpkgPath overrides the dpkg binary, for tests.
	DpkgPath string
	// Runner overrides command execution entirely, which is how the parsers
	// are tested against recorded output.
	Runner Runner
	// Events receives apt.update and apt.resolve. Optional.
	Events evidence.Sink
}

// NewContainerBackend returns the backend that resolves inside a pinned image
// of the target release.
func NewContainerBackend(opts ContainerOptions) Backend { return newContainerBackend(opts) }

// ContainerOptions configures the container backend.
type ContainerOptions struct {
	// Runtime is docker or podman. Empty means autodetect, docker first.
	Runtime string
	// Image overrides the image from the distro table.
	Image string
	// Platform overrides the --platform value.
	Platform string
	// SelfPath is the debark binary mounted into the container and
	// re-entered with --backend=local: a static linux/<arch> build. Empty
	// means bin/debark-linux-<arch> beside the running executable, else
	// the running executable itself (containerSelfPath).
	SelfPath string
	// StoreVolume, when non-empty, is a named volume used for the store
	// instead of a bind mount (experiment E5).
	StoreVolume string
	// Events receives backend.selected and apt events proxied from inside.
	Events evidence.Sink
}

// NewRunner returns the exec-based apt-get runner.
func NewRunner(aptGetPath string) Runner { return newRunner(aptGetPath) }

// FileURI renders an absolute host path as a file: URI: the form a one-line
// apt source entry takes ("deb [trusted=yes] <uri> ./"), and the form an
// operating system's URL handler expects.
//
// Two things a plain "file://" + filepath.ToSlash concatenation gets wrong,
// both of which produce something that is read as a different path rather
// than as an error anyone would see:
//
//   - A path containing a space splits the one-line source entry into extra
//     fields, silently turning the suite/component tail into nonsense. Every
//     character a URI reserves is percent-encoded here instead (url.URL's own
//     path escaping), which apt — and every other URI consumer — decodes back
//     on the other side.
//   - A Windows drive-letter path produces "file://C:/..." — only two
//     slashes, so "C:" lands in the URI's AUTHORITY, not its path. The third
//     slash is added so the drive letter stays part of the path.
//     (Nothing on Windows runs a real apt-get; this matters because the
//     generated source text is compared and recorded, and a malformed URI in
//     an artefact is a defect regardless of who consumes it.)
//
// Exported, and living here in the package's public surface rather than
// beside its one apt caller, because it was not only apt's problem. The
// desktop GUI hands an operator-chosen bundle directory to the platform's
// file manager and had built that URL by concatenation, so a path out of a
// file dialog containing "#", "?" or a space opened a different folder or
// failed to parse at all; with nothing exported to call, the GUI landed a
// verbatim copy of this function. Two copies of one escaping rule drift, and
// the drift is invisible until a path with an awkward character reaches one
// of them. There is exactly one right answer to "how do I escape a path into
// a URL" and it is net/url's, so there should be exactly one implementation
// of it in this project.
//
// The result is platform-dependent and both answers are correct: filepath.
// ToSlash is a no-op wherever the separator is already "/", so
// `D:\projects\ext dir` becomes "file:///D:/projects/ext%20dir" on Windows and
// "file:///D:%5Cprojects%5Cext%20dir" on Linux, where a backslash is an
// ordinary legal character in a filename. What holds everywhere is the pair
// of properties TestFileURI pins: the result is a single whitespace-free
// field, and decoding it returns exactly the path that went in with the
// platform's separators normalised to "/". Assert those, not a literal —
// asserting the literal is how this function's test came to fail on Linux
// while passing on Windows (bf51914).
func FileURI(absPath string) string {
	p := filepath.ToSlash(absPath)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String()
}
