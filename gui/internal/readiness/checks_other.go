//go:build !linux && !windows

package readiness

import (
	"errors"
	"os/exec"
)

// This file keeps the package compiling everywhere Go does, so that a
// contributor on macOS can run "go build ./..." on the whole tree. macOS is out
// of scope for v1 and nothing here pretends otherwise: there is no
// platform-specific check, free space is reported as unmeasurable rather than
// guessed at, and the build-environment remedy says plainly that the build has
// to happen elsewhere.

// platformChecks contributes nothing on an unsupported platform. The portable
// checks still run, so the screen still reports the debark binary, any
// container runtime and the signing key.
func platformChecks() []check {
	// macOS has the same problem Windows does and for the same reason: a
	// darwin debark is a Mach-O binary, and the container backend mounts
	// and re-execs whatever it is given. The check is registered here rather
	// than left out with the rest of this file's checks because it is not a
	// probe of an unsupported platform's facilities — it is a file test, and
	// its answer is correct on any non-Linux host.
	return []check{{CheckSelfBinary, "Linux debark for containers", selfBinaryCheck}}
}

// noBuildEnvironmentRemedy is the sentence for a platform this application does
// not support building on.
func noBuildEnvironmentRemedy() string {
	return "Install Docker or Podman so builds can run in a container, or run the builder on a Linux machine and copy the bundle back — a bundle is an ordinary folder."
}

// errNoFreeSpaceAPI makes the disk check report StatusSkipped rather than
// invent a number. A wrong free-space figure is worse than none: it is the
// figure an operator would plan a build around.
var errNoFreeSpaceAPI = errors.New("free space is not reported on this platform")

func freeSpace(string) (free, total uint64, err error) { return 0, 0, errNoFreeSpaceAPI }

// hideConsole is a no-op; the console-window problem it solves is Windows-only.
func hideConsole(*exec.Cmd) {}
