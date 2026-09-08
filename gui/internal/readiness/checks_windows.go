//go:build windows

package readiness

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// platformChecks adds the Windows-only check: WSL 2.
//
// Windows is "build it, do not promise it". A build here needs either WSL 2 or
// a container, and on the locked-down laptops this audience carries both are
// frequently blocked by policy — which is the case this check has to handle
// well. An honest row saying so, with the exact command that would fix it and
// the alternative of building somewhere else, is worth far more than a green
// tick that turns into a failed build ten minutes later.
func platformChecks() []check {
	return []check{
		{CheckWSL, "WSL 2", wslCheck},
		// Windows is the platform the self-binary problem was measured on:
		// a debark.exe is a PE binary and the container backend re-execs
		// what it mounts. See checks_selfbinary.go.
		{CheckSelfBinary, "Linux debark for containers", selfBinaryCheck},
	}
}

// wslDistro is one registered distribution as "wsl --list --verbose" reports
// it.
type wslDistro struct {
	Name string
	// Version is 1 or 2. Only 2 can run a build: WSL 1 translates syscalls
	// rather than running a kernel, and apt's use of mount namespaces and
	// overlay filesystems does not survive the translation.
	Version int
	// Default marks the distribution wsl.exe runs when none is named.
	Default bool
}

// wslObservation is what wsl.exe said about itself.
type wslObservation struct {
	// Path is wsl.exe, empty when this Windows does not have it at all.
	Path string
	// ComponentPresent is whether the Windows Subsystem for Linux is actually
	// enabled, as opposed to wsl.exe merely existing — on Windows 10 2004 and
	// later the stub ships with the OS whether or not the feature is on, which
	// is exactly why "wsl --install" is offerable in the first place.
	ComponentPresent bool
	// Version is the wsl.exe version, when this build has "wsl --version" at
	// all. Only the Store-distributed WSL does; the in-box one does not, and
	// its absence is not a fault.
	Version string
	// Distros are the registered distributions.
	Distros []wslDistro
	// StatusOut, VersionOut and ListOut are the raw attempts, so a timeout is
	// distinguishable from a refusal.
	StatusOut  CommandOutput
	VersionOut CommandOutput
	ListOut    CommandOutput
}

func wslCheck(ctx context.Context, o Options) Result {
	return decideWSL(probeWSL(ctx, o))
}

// probeWSL asks wsl.exe three short questions under the caller's single
// deadline, so a WSL service that is starting a VM and not answering costs the
// check's timeout once rather than three times.
func probeWSL(ctx context.Context, o Options) wslObservation {
	var obs wslObservation
	p, err := o.LookPath("wsl")
	if err != nil {
		return obs
	}
	obs.Path = p

	obs.StatusOut = o.Runner(ctx, p, "--status")
	obs.ComponentPresent = obs.StatusOut.OK()

	obs.VersionOut = o.Runner(ctx, p, "--version")
	if obs.VersionOut.OK() {
		obs.Version = parseWSLVersion(decodeConsole(obs.VersionOut.Stdout))
	}

	obs.ListOut = o.Runner(ctx, p, "--list", "--verbose")
	obs.Distros = parseWSLDistros(decodeConsole(obs.ListOut.Stdout))
	if len(obs.Distros) > 0 {
		// Distributions cannot be registered without the component, so this is
		// a stronger signal than --status, which some builds answer oddly.
		obs.ComponentPresent = true
	}
	return obs
}

func decideWSL(obs wslObservation) Result {
	if obs.Path == "" {
		return Result{
			Status: StatusProblem,
			// Degraded, not blocking: Docker Desktop is a complete alternative
			// on Windows, and DeriveBuildEnvironment owns the "no path at all"
			// judgement.
			Severity: SeverityDegraded,
			Summary:  "This Windows build has no wsl.exe, so WSL cannot be used or installed from here.",
			Remedy:   "Use Docker Desktop, which is what runs a build on this platform, or run the builder on a Linux machine you can reach — a bundle is an ordinary folder, so building it elsewhere and copying it back is a fully supported way to work.",
		}
	}

	timedOut := obs.StatusOut.TimedOut || obs.ListOut.TimedOut
	switch {
	case timedOut && len(obs.Distros) == 0:
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary:  "WSL did not answer within the timeout, so its state is unknown.",
			Remedy:   "Run wsl --status in a terminal; WSL can be slow the first time after a reboot, so trying this check again often settles it.",
			Action:   &Action{Label: "Check WSL again", Command: []string{"wsl", "--status"}},
			Detail:   obs.Path,
		}

	case len(obs.Distros) == 0 && !obs.ComponentPresent:
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary:  "The Windows Subsystem for Linux is not installed.",
			Remedy:   "Install Docker Desktop, which is what actually runs a build here and which installs WSL 2 for itself; or install WSL 2 first with the command below; or run the builder on a Linux machine you can reach and copy the bundle back, which on a managed laptop may be the only one your policy allows.",
			Action: &Action{
				Label:    "Install WSL 2",
				Command:  []string{"wsl", "--install"},
				Elevated: true,
				Note:     "This enables Windows features and needs a restart to finish.",
			},
			Detail: strings.TrimSpace(obs.Path + " — " + obs.StatusOut.Message()),
		}

	case len(obs.Distros) == 0:
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary:  "WSL is installed but no Linux distribution is registered.",
			Remedy:   "This does not stop a build on its own — builds go through the container runtime. Install a distribution with the command below if you want one; Docker Desktop registers its own and will satisfy this row as a side effect.",
			Action: &Action{
				Label:    "Install Ubuntu on WSL",
				Command:  []string{"wsl", "--install", "--distribution", "Ubuntu"},
				Elevated: true,
			},
			Detail: strings.TrimSpace(obs.Path + " — " + obs.ListOut.Message()),
		}

	case !hasWSL2(obs.Distros):
		d := obs.Distros[0]
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary:  fmt.Sprintf("The registered distribution %q runs on WSL 1, which Docker Desktop cannot use as a backend.", d.Name),
			Remedy:   "Convert it to WSL 2 with the command below; the conversion keeps the distribution's files and can take a few minutes. A build goes through the container runtime either way, so this matters only if Docker Desktop is using this distribution.",
			Action: &Action{
				Label:   "Convert " + d.Name + " to WSL 2",
				Command: []string{"wsl", "--set-version", d.Name, "2"},
			},
			Detail: describeDistros(obs.Distros),
		}

	default:
		// Deliberately does NOT say builds can run without a container. They
		// cannot: debark's only routes are native apt and a container, and
		// nothing in either repository invokes wsl.exe to do work. This row
		// said otherwise for two waves, on the screen, in green.
		//
		// It is still worth reporting, because Docker Desktop's default
		// backend on Windows is WSL 2 and docker-desktop registers itself as a
		// distribution, so a WSL fault is very often the explanation for the
		// container row failing beside it.
		summary := "WSL 2 is available. It is what Docker Desktop normally runs on; builds themselves go through the container runtime."
		if v := obs.Version; v != "" {
			summary = fmt.Sprintf("WSL %s is available. It is what Docker Desktop normally runs on; builds themselves go through the container runtime.", v)
		}
		return Result{
			Status:   StatusOK,
			Severity: SeverityInfo,
			Summary:  summary,
			Detail:   describeDistros(obs.Distros),
		}
	}
}

func hasWSL2(ds []wslDistro) bool {
	for _, d := range ds {
		if d.Version >= 2 {
			return true
		}
	}
	return false
}

func describeDistros(ds []wslDistro) string {
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		s := fmt.Sprintf("%s (WSL %d)", d.Name, d.Version)
		if d.Default {
			s += " [default]"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// wslVersionRe matches the dotted version in "WSL version: 2.0.9.0". The label
// is localised; the number is not, which is why the pattern ignores the label
// entirely.
var wslVersionRe = regexp.MustCompile(`(\d+(?:\.\d+)+)`)

func parseWSLVersion(out string) string {
	if m := wslVersionRe.FindStringSubmatch(firstLine(out)); len(m) == 2 {
		return m[1]
	}
	return ""
}

// parseWSLDistros reads "wsl --list --verbose" output.
//
// The header row and the STATE column are localised, so neither is relied on: a
// data row is recognised by ending in a bare integer, which is the VERSION
// column and is the same on every Windows in every language. The header's
// trailing word never parses as an integer and so drops out without needing to
// be counted or skipped, which also makes the parser indifferent to the banner
// some builds print above the table.
func parseWSLDistros(out string) []wslDistro {
	var ds []wslDistro
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 {
			continue
		}
		version, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			continue
		}
		isDefault := false
		if fields[0] == "*" {
			isDefault = true
			fields = fields[1:]
		} else if strings.HasPrefix(fields[0], "*") {
			isDefault = true
			fields[0] = strings.TrimPrefix(fields[0], "*")
		}
		if len(fields) < 3 || fields[0] == "" {
			continue
		}
		ds = append(ds, wslDistro{Name: fields[0], Version: version, Default: isDefault})
	}
	return ds
}

// decodeConsole converts wsl.exe's output to UTF-8.
//
// wsl.exe writes UTF-16LE, not the console code page, and has done since it
// gained the --verbose table. Reading it as bytes yields a string with a NUL
// after every character, which no amount of field splitting recovers from — so
// this runs before any parsing. The BOM is honoured when present; when it is
// not, a high proportion of NUL bytes is the signal, since valid UTF-8 never
// contains one.
func decodeConsole(s string) string {
	b := []byte(s)
	switch {
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		b = b[2:]
	case looksUTF16LE(b):
		// no BOM, but the NUL density says otherwise
	default:
		return s
	}
	if len(b) < 2 {
		return s
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u))
}

func looksUTF16LE(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	nuls := 0
	for _, c := range b {
		if c == 0 {
			nuls++
		}
	}
	return nuls*4 > len(b)
}

// noBuildEnvironmentRemedy is the Windows sentence for a machine with neither
// WSL 2 nor a container. It names the remote builder as a first-class third
// option rather than a footnote, because on a managed laptop where both local
// options are blocked by policy it is the only one that will ever work, and
// telling the operator that plainly is more use than a red row.
// noBuildEnvironmentRemedy leads with Docker Desktop because on Windows the
// container backend is the only route a build can take. It used to offer
// "WSL 2 or Docker Desktop", which read as two alternatives and was one.
func noBuildEnvironmentRemedy() string {
	return "Install Docker Desktop using the rows above — on Windows a build runs inside a container, so that is the one thing that makes this machine able to build. Otherwise build on a Linux machine you can reach: a bundle is an ordinary folder, so building it there and copying it back is fully supported and is often the only option on a managed laptop."
}

// freeSpace asks Windows how much of a volume a build may actually use.
//
// It goes through golang.org/x/sys/windows rather than a hand-rolled
// syscall.NewLazyDLL wrapper. This used to be the other way round, on the
// grounds that x/sys was only an indirect dependency and importing it would
// change a frozen go.mod. That is no longer true — internal/export's Windows
// volume enumeration imports it, and go.mod has listed it as a direct
// requirement since — so the hand-rolled version was buying nothing and
// costing an unsafe.Pointer block and a scoped gosec waiver for G103. x/sys's
// wrappers are generated and widely exercised; this file's were neither.
//
// The first out-parameter is the space available to the calling user, which is
// what a quota would limit and therefore what a build actually gets; the second
// is the volume's total size. The third, the volume-wide free figure, is
// deliberately discarded — internal/export/volumes_windows.go makes the same
// choice for the same reason.
func freeSpace(path string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var availToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &availToCaller, &totalBytes, &totalFree); err != nil {
		return 0, 0, err
	}
	return availToCaller, totalBytes, nil
}

// hideConsole stops a console window flashing on screen every time this
// application probes wsl.exe or docker.exe. A GUI process spawning a console
// subsystem binary gets a new window by default, and a first-run screen that
// runs four probes concurrently would flash four of them.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
