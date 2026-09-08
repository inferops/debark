//go:build linux

package readiness

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
)

// platformChecks adds the check that only means something on Linux: whether
// this host has apt of its own.
//
// This is the check the shipping story rests on. Linux is v1's target precisely
// because a Debian or Ubuntu host runs "debark build" with no prerequisites —
// no container, no VM, no WSL — so a green row here is the difference between
// the zero-setup path and every other path.
func platformChecks() []check {
	return []check{{CheckAPT, "Native apt", aptCheck}}
}

// aptVersionRe matches the version in "apt 2.7.14 (amd64)", which is what
// apt-get --version prints on its first line.
var aptVersionRe = regexp.MustCompile(`(?m)^apt\s+(\S+)`)

// aptObservation is what this host has of a native apt toolchain.
type aptObservation struct {
	// AptGetPath and DpkgPath are where each was found; empty means absent.
	// Both matter: apt-get resolves and fetches, dpkg is what debark reads
	// the target's installed set with, and a host with one and not the other is
	// a broken install rather than a container-only machine.
	AptGetPath string
	DpkgPath   string
	// Version is what apt-get --version reported.
	Version string
	// VersionOut is the raw attempt, so a timeout reads differently from a
	// refusal.
	VersionOut CommandOutput
}

func aptCheck(ctx context.Context, o Options) Result {
	return decideAPT(probeAPT(ctx, o))
}

func probeAPT(ctx context.Context, o Options) aptObservation {
	var obs aptObservation
	if p, err := o.LookPath("apt-get"); err == nil {
		obs.AptGetPath = p
	}
	if p, err := o.LookPath("dpkg"); err == nil {
		obs.DpkgPath = p
	}
	if obs.AptGetPath == "" {
		return obs
	}
	obs.VersionOut = o.Runner(ctx, obs.AptGetPath, "--version")
	if obs.VersionOut.OK() {
		if m := aptVersionRe.FindStringSubmatch(obs.VersionOut.Stdout); len(m) == 2 {
			obs.Version = m[1]
		} else {
			obs.Version = firstLine(obs.VersionOut.Stdout)
		}
	}
	return obs
}

func decideAPT(obs aptObservation) Result {
	switch {
	case obs.AptGetPath == "" && obs.DpkgPath == "":
		return Result{
			Status: StatusProblem,
			// Degraded, never blocking: a container runs the target's own apt
			// perfectly well, and that is the whole reason the container
			// backend exists. Only the absence of every option is fatal, and
			// DeriveBuildEnvironment owns that judgement.
			Severity: SeverityDegraded,
			Summary:  "This host has no apt of its own, so builds will need a container.",
			Remedy:   "Nothing to do if a container runtime is available — debark runs the target's own apt inside it. Running this application on a Debian or Ubuntu host removes the need entirely.",
		}

	case obs.AptGetPath == "" || obs.DpkgPath == "":
		missing, present := "apt-get", obs.DpkgPath
		if obs.DpkgPath == "" {
			missing, present = "dpkg", obs.AptGetPath
		}
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary:  fmt.Sprintf("This host has a partial apt toolchain: %s is missing.", missing),
			Remedy:   fmt.Sprintf("Reinstall %s, or let builds use a container instead — debark needs both apt-get and dpkg to use a host directly.", missing),
			Detail:   "found " + present,
		}

	case obs.VersionOut.TimedOut:
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary:  "apt-get is installed but did not answer within the timeout.",
			Remedy:   "Run apt-get --version in a terminal; if it hangs there too, another package operation is probably holding the dpkg lock.",
			Detail:   obs.AptGetPath,
		}

	case obs.Version == "":
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary:  "apt-get is installed but did not report a version.",
			Remedy:   "Run apt-get --version in a terminal to see what it says; builds can use a container in the meantime.",
			Detail:   strings.TrimSpace(obs.AptGetPath + " — " + obs.VersionOut.Message()),
		}

	default:
		return Result{
			Status:   StatusOK,
			Severity: SeverityInfo,
			Summary:  fmt.Sprintf("apt %s is available natively, so builds need no container.", obs.Version),
			Detail:   obs.AptGetPath + ", " + obs.DpkgPath,
		}
	}
}

// noBuildEnvironmentRemedy is what to say on Linux when neither apt nor a
// container is usable — an unusual machine, since a host without apt is
// normally a Fedora or Arch desktop that can still install podman in one
// command.
func noBuildEnvironmentRemedy() string {
	return "Install Docker or Podman with your distribution's package manager, or run this application on a Debian or Ubuntu host, where builds need nothing at all. The rows above carry the exact commands."
}

// freeSpace reports the space an unprivileged process may actually use, which
// is Bavail rather than Bfree: the difference is the reserved blocks only root
// can write into, and reporting those as free would promise the operator room a
// build cannot have.
func freeSpace(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	// Signed, and their width varies by port, so a conversion is
	// unavoidable. A negative value is not a real answer; reporting zero
	// makes the check say "could not measure" rather than inventing an
	// enormous free-space figure that would hide a genuinely full disk.
	bs := statfsUint(st.Bsize)
	return statfsUint(st.Bavail) * bs, statfsUint(st.Blocks) * bs, nil
}

// hideConsole is a no-op here; the console-window problem it solves is
// Windows-only.
func hideConsole(*exec.Cmd) {}

// statfsField is every integer type a statfs field takes across the ports this
// builds for. The fields are unsigned on linux/amd64 and signed on others,
// which is why a fixed cast direction cannot work and why this is generic.
type statfsField interface {
	~int32 | ~int64 | ~uint32 | ~uint64
}

// statfsUint converts a statfs field to uint64, treating a negative value as
// zero.
//
// Negative is not a real filesystem answer, but a naive cast turns one into an
// enormous positive figure — and this value is multiplied into the free space
// the disk-space check reports. Reporting zero makes an unmeasurable
// filesystem read as "could not measure" rather than inventing a free-space
// figure that would hide a genuinely full disk.
func statfsUint[T statfsField](v T) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
