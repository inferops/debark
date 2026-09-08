package readiness

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/inferops/debark/core/sign"
	"github.com/inferops/debark/core/store"
	"github.com/inferops/debark/core/version"
)

// Every check in this file is written as a probe and a decision, and they are
// separate functions on purpose.
//
// The probe does the I/O and returns a plain observation struct. The decision
// takes that struct and returns a Result, touching nothing outside it. All the
// value of this package is in the decisions — which severity, which sentence,
// which command — and splitting them this way is what lets every one of them be
// table-tested on a laptop with no docker, no WSL, no apt and no signing key,
// which is exactly the machine the code has to be right about.

// ---------------------------------------------------------------------------
// debark binary
// ---------------------------------------------------------------------------

// binaryObservation is what could be learned about the debark executable.
type binaryObservation struct {
	// Path is where it was found, empty when it was not.
	Path string
	// Located records how Path was obtained, for the details drawer.
	Located string
	// LocateErr is why the search failed.
	LocateErr error
	// Version is what "debark version --json" reported.
	Version string
	// VersionOut is the raw result of asking, so the decision can tell a
	// timeout from a refusal from a binary that is not debark at all.
	VersionOut CommandOutput
}

func binaryCheck(ctx context.Context, o Options) Result {
	return decideBinary(probeBinary(ctx, o))
}

func probeBinary(ctx context.Context, o Options) binaryObservation {
	var obs binaryObservation

	switch {
	case o.BinaryPath != "":
		obs.Path = o.BinaryPath
		obs.Located = "supplied by the application"
	default:
		p, err := o.Locator(ctx)
		if err != nil {
			obs.LocateErr = err
			return obs
		}
		obs.Path = p
		obs.Located = "found on PATH or beside this application"
	}

	obs.VersionOut = o.Runner(ctx, obs.Path, "version", "--json")
	if obs.VersionOut.OK() {
		// version.Info is imported rather than mirrored: it is the type the CLI
		// marshals, so a rename in the core repository becomes a compile error
		// here instead of a silently empty version string. A malformed or
		// unrecognised object degrades to the raw first line below rather than
		// to an error — this row exists to report that debark answered at
		// all, and a version it could not parse is still an answer.
		var info version.Info
		if err := json.Unmarshal([]byte(obs.VersionOut.Stdout), &info); err == nil {
			obs.Version = info.Version
		}
		if obs.Version == "" {
			obs.Version = firstLine(obs.VersionOut.Stdout)
		}
	}
	return obs
}

func decideBinary(obs binaryObservation) Result {
	switch {
	case obs.Path == "":
		return Result{
			Status:   StatusProblem,
			Severity: SeverityBlocking,
			Summary:  "The debark command-line tool was not found on this machine.",
			Remedy:   "Install the debark package, or put the debark binary somewhere on your PATH — every catalogue, build and export in this application runs it.",
			Detail:   locateDetail(obs.LocateErr),
		}

	case obs.VersionOut.TimedOut:
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary:  fmt.Sprintf("debark was found at %s but did not answer within the timeout.", obs.Path),
			Remedy:   "Run the command below in a terminal to see what it does; if it hangs there too, the binary is not usable and should be reinstalled.",
			Action:   &Action{Label: "Show the version", Command: []string{obs.Path, "version"}},
			Detail:   obs.VersionOut.Message(),
		}

	case obs.Version == "":
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary:  fmt.Sprintf("A binary at %s did not report a debark version.", obs.Path),
			Remedy:   "Run the command below in a terminal: if it is not debark, remove it from your PATH or point this application at the right binary.",
			Action:   &Action{Label: "Show the version", Command: []string{obs.Path, "version"}},
			Detail:   obs.VersionOut.Message(),
		}

	default:
		return Result{
			Status:   StatusOK,
			Severity: SeverityInfo,
			Summary:  fmt.Sprintf("debark %s is installed and answering.", obs.Version),
			Detail:   obs.Path + " (" + obs.Located + ")",
		}
	}
}

func locateDetail(err error) string {
	if err == nil {
		return ""
	}
	return "searched PATH and this application's own directory: " + err.Error()
}

// lookPathLocator is the default BinaryLocator: PATH first, then the directory
// this application was started from, which is where a .deb that ships both the
// GUI and the CLI puts them and where a development tree keeps them.
//
// It is deliberately this short. internal/cliadapter owns real location — the
// config file, an operator-configured override, the version and capability
// negotiation that goes with it — and duplicating any of that here would create
// a second answer to "where is debark" that can disagree with the one every
// other screen uses. This exists only so the first screen can report a missing
// binary before the adapter is reachable. See BinaryLocator.
func lookPathLocator(_ context.Context) (string, error) {
	name := "debark"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	if p, err := exec.LookPath("debark"); err == nil {
		return p, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("debark is not on PATH")
	}
	beside := filepath.Join(filepath.Dir(self), name)
	if st, statErr := os.Stat(beside); statErr == nil && !st.IsDir() {
		return beside, nil
	}
	return "", fmt.Errorf("debark is not on PATH and not beside %s", filepath.Dir(self))
}

// ---------------------------------------------------------------------------
// container runtime
// ---------------------------------------------------------------------------

// containerCandidates is the search order, matching the core repository's own
// (core/apt/container_driver.go): docker first, then podman. Agreeing with the
// engine here matters — reporting that podman is ready when debark will pick
// docker would be a green tick for a runtime the build never uses.
var containerCandidates = []string{"docker", "podman"}

// containerPermissionRe and containerUnreachableRe classify a runtime that is
// installed but refused. The patterns follow core/apt/container_driver.go's
// containerDaemonUnreachableRe; they are split in two here because the two
// cases need different sentences and different commands, and because the
// permission message contains the daemon-connection wording too — so it must be
// tested for first.
var (
	containerPermissionRe  = regexp.MustCompile(`(?i)permission denied`)
	containerUnreachableRe = regexp.MustCompile(`(?i)cannot connect to the docker daemon|is the docker daemon running|error during connect|connection refused|cannot connect to podman|no such file or directory|the system cannot find the file specified`)
)

// serverVersionRe pulls a version out of a runtime's info output, best effort.
// "Server Version:" is docker's; a bare "Version:" is podman's. Nothing decides
// anything on the result — it is decoration for a row that has already passed.
var serverVersionRe = regexp.MustCompile(`(?mi)^\s*(?:Server )?Version:\s*(\S+)\s*$`)

// runtimeProbe is one container runtime as this machine presented it.
type runtimeProbe struct {
	// Name is "docker" or "podman".
	Name string
	// Path is where it was found on PATH; empty means not installed.
	Path string
	// Probed is true when Out holds a real attempt to talk to it.
	Probed bool
	Out    CommandOutput
}

// containerObservation is the whole container picture, including the two facts
// the remedies need: who this user is, and whether there is a package manager
// to offer an install command through.
type containerObservation struct {
	Runtimes []runtimeProbe
	Username string
	// HasAPTGet and HasWinget gate the install Action. Offering a button that
	// runs apt-get on a machine with no apt-get, or winget on a machine with no
	// winget, is worse than offering no button: it turns a clear instruction
	// into a failed click.
	HasAPTGet bool
	HasWinget bool
}

func containerCheck(ctx context.Context, o Options) Result {
	return decideContainer(probeContainer(ctx, o))
}

func probeContainer(ctx context.Context, o Options) containerObservation {
	obs := containerObservation{Username: o.Username}
	_, aptErr := o.LookPath("apt-get")
	obs.HasAPTGet = aptErr == nil
	_, wingetErr := o.LookPath("winget")
	obs.HasWinget = wingetErr == nil

	for _, name := range containerCandidates {
		rp := runtimeProbe{Name: name}
		p, err := o.LookPath(name)
		if err != nil {
			obs.Runtimes = append(obs.Runtimes, rp)
			continue
		}
		rp.Path = p
		rp.Probed = true
		// "info" rather than "--version": the whole point of this check is that
		// a present binary says nothing about a running daemon, and only a
		// command that talks to the daemon can tell the difference.
		rp.Out = o.Runner(ctx, p, "info")
		obs.Runtimes = append(obs.Runtimes, rp)
		if rp.Out.OK() {
			// One working runtime is the answer. Probing the second would add
			// its own timeout to the cold start to learn nothing.
			break
		}
	}
	return obs
}

func decideContainer(obs containerObservation) Result {
	var firstFound *runtimeProbe
	for i := range obs.Runtimes {
		rp := &obs.Runtimes[i]
		// Probed is required as well as OK: a zero CommandOutput has exit code
		// zero, so a runtime that was found but never asked would otherwise
		// read as a working daemon.
		if rp.Probed && rp.Out.OK() {
			summary := fmt.Sprintf("%s is installed and its daemon is answering.", displayName(rp.Name))
			detail := rp.Path
			if v := serverVersionRe.FindStringSubmatch(rp.Out.Stdout); len(v) == 2 {
				detail = fmt.Sprintf("%s, server %s", rp.Path, v[1])
			}
			return Result{Status: StatusOK, Severity: SeverityInfo, Summary: summary, Detail: detail}
		}
		if rp.Path != "" && firstFound == nil {
			firstFound = rp
		}
	}

	if firstFound == nil {
		return containerNotInstalled(obs)
	}

	name := displayName(firstFound.Name)
	out := firstFound.Out
	base := Result{Status: StatusProblem, Severity: SeverityDegraded, Detail: strings.TrimSpace(firstFound.Path + " — " + out.Message())}

	switch {
	case out.TimedOut:
		base.Summary = fmt.Sprintf("%s is installed but did not answer within the timeout.", name)
		base.Remedy = fmt.Sprintf("Its socket is most likely wedged: restart %s and run this check again. Nothing else in this application is waiting on it.", name)
		return base

	case containerPermissionRe.MatchString(out.Stderr + out.Stdout):
		base.Summary = fmt.Sprintf("%s is installed and running, but this account is not permitted to talk to it.", name)
		base.Remedy = fmt.Sprintf("Your user needs to be in the %s group; add it, then log out and back in so the new group membership takes effect.", firstFound.Name)
		if obs.Username != "" && runtime.GOOS != "windows" {
			base.Action = &Action{
				Label:    "Add this user to the " + firstFound.Name + " group",
				Command:  []string{"sudo", "usermod", "-aG", firstFound.Name, obs.Username},
				Elevated: true,
				Note:     "Group membership only applies to new sessions: log out and back in afterwards.",
			}
		}
		return base

	case containerUnreachableRe.MatchString(out.Stderr + out.Stdout):
		base.Summary = fmt.Sprintf("%s is installed but its daemon is not running.", name)
		base.Remedy = containerStartRemedy(name)
		base.Action = containerStartAction(firstFound.Name)
		return base

	default:
		base.Summary = fmt.Sprintf("%s is installed but did not answer successfully.", name)
		base.Remedy = fmt.Sprintf("Run %s info in a terminal to see the whole message; a build can still use this host's own apt if it matches the target.", firstFound.Name)
		base.Action = &Action{Label: "Show " + name + " status", Command: []string{firstFound.Name, "info"}}
		return base
	}
}

func containerNotInstalled(obs containerObservation) Result {
	res := Result{
		Status:   StatusProblem,
		Severity: SeverityDegraded,
		Summary:  "No container runtime is installed.",
	}
	switch runtime.GOOS {
	case "linux":
		res.Remedy = "A container is only needed when this host's own apt cannot serve the target — a different distribution or release. Install Docker or Podman if you expect to build for one of those; see https://docs.docker.com/engine/install/."
		if obs.HasAPTGet {
			res.Action = &Action{
				Label:    "Install Docker",
				Command:  []string{"sudo", "apt-get", "install", "-y", "docker.io"},
				Elevated: true,
			}
		}
	case "windows":
		res.Remedy = "On Windows a build needs either WSL 2 or a container runtime. Install Docker Desktop (https://docs.docker.com/desktop/install/windows-install/) or Podman Desktop (https://podman-desktop.io/), or use WSL 2 instead."
		if obs.HasWinget {
			res.Action = &Action{
				Label:   "Install Docker Desktop",
				Command: []string{"winget", "install", "--exact", "--id", "Docker.DockerDesktop"},
				Note:    "Windows will ask for administrator confirmation while the installer runs.",
			}
		}
	default:
		res.Remedy = "Install Docker or Podman and make sure docker or podman is on your PATH."
	}
	return res
}

// containerStartRemedy and containerStartAction differ by platform because
// starting a daemon does. On Linux it is a service and there is an exact
// command; on Windows and macOS it is an application the operator launches, and
// pretending otherwise would put a button on the screen that cannot work.
func containerStartRemedy(name string) string {
	switch runtime.GOOS {
	case "linux":
		return fmt.Sprintf("Start the %s service, then run this check again.", strings.ToLower(name))
	default:
		return fmt.Sprintf("Start %s Desktop and wait for it to report that it is running, then run this check again.", name)
	}
}

func containerStartAction(name string) *Action {
	if runtime.GOOS != "linux" {
		return nil
	}
	service := name
	if name == "podman" {
		service = "podman.socket"
	}
	return &Action{
		Label:    "Start " + displayName(name),
		Command:  []string{"sudo", "systemctl", "start", service},
		Elevated: true,
	}
}

func displayName(runtimeName string) string {
	switch runtimeName {
	case "docker":
		return "Docker"
	case "podman":
		return "Podman"
	default:
		return runtimeName
	}
}

// ---------------------------------------------------------------------------
// operator signing key
// ---------------------------------------------------------------------------

// signingKeyObservation is what was found where a signing key is expected.
type signingKeyObservation struct {
	// Ref is the value of DEBARK_SIGN, which may name a file, a gpg key or a
	// signer plugin.
	Ref string
	// Path is the private key file that was checked.
	Path string
	// Exists is whether Path is a readable file.
	Exists bool
	// PublicPath is the matching .pub, derived the way sign.GenerateKey derives
	// it, and PublicExists whether it is there. A private key with no public key
	// beside it still signs; the target just has nothing to verify against.
	PublicPath   string
	PublicExists bool
}

func signingKeyCheck(_ context.Context, o Options) Result {
	return decideSigningKey(probeSigningKey(o))
}

func probeSigningKey(o Options) signingKeyObservation {
	obs := signingKeyObservation{
		// DEBARK_SIGN is read; the config file is not. Parsing debark's
		// YAML config here would mean reimplementing its flag/env/file/profile
		// precedence, and a second implementation of that rule is a second
		// answer that can disagree with the binary actually doing the signing.
		// The application layer, which does have the CLI adapter, is where a
		// config-derived override belongs: it sets Options.SigningKeyPath.
		Ref:  strings.TrimSpace(os.Getenv("DEBARK_SIGN")),
		Path: o.SigningKeyPath,
	}
	if isFileRef(obs.Ref) {
		obs.Path = obs.Ref
	}
	if st, err := os.Stat(obs.Path); err == nil && !st.IsDir() {
		obs.Exists = true
	}
	obs.PublicPath = publicKeyPathFor(obs.Path)
	if st, err := os.Stat(obs.PublicPath); err == nil && !st.IsDir() {
		obs.PublicExists = true
	}
	return obs
}

func decideSigningKey(obs signingKeyObservation) Result {
	if obs.Ref != "" && !isFileRef(obs.Ref) {
		kind := "gpg"
		if strings.HasPrefix(obs.Ref, "plugin:") {
			kind = "a signer plugin"
		}
		return Result{
			Status:   StatusOK,
			Severity: SeverityInfo,
			Summary:  "Signing is delegated to " + kind + " by DEBARK_SIGN.",
			// Whether that key or plugin actually works is settled by debark
			// at signing time. Probing it here would be a second opinion about
			// something this application does not perform.
			Detail: "DEBARK_SIGN=" + obs.Ref,
		}
	}

	if obs.Exists {
		detail := obs.Path
		if !obs.PublicExists {
			detail += " (no " + obs.PublicPath + " beside it; the target needs the public key to verify)"
		}
		return Result{
			Status:   StatusOK,
			Severity: SeverityInfo,
			Summary:  "An operator signing key is in place.",
			Detail:   detail,
		}
	}

	return Result{
		Status: StatusProblem,
		// Not a fault and never a blocker: debark writes an unsigned bundle
		// when no key is configured and says so in the result, so an operator
		// who has not made a key yet can still do everything except sign.
		Severity: SeverityInfo,
		Summary:  "No operator signing key was found, so bundles will be built unsigned.",
		Remedy:   "Create one now, or carry on and sign later — a bundle can be built, copied and installed without one.",
		Action: &Action{
			Label:   "Create a signing key",
			Command: []string{"debark", "keygen", "--out", obs.Path},
			Note:    "The public key is written beside it and must reach the target separately from the media you sign with it.",
		},
		Detail: "looked for " + obs.Path,
	}
}

// isFileRef reports whether a DEBARK_SIGN value names a key file rather than
// delegating to gpg or a plugin. The two prefixes are the core repository's
// own (core/sign/api.go).
func isFileRef(ref string) bool {
	if ref == "" {
		return false
	}
	return !strings.HasPrefix(ref, "gpg:") && !strings.HasPrefix(ref, "plugin:")
}

// publicKeyPathFor applies sign.GenerateKey's own rule — the private path with
// the .key suffix replaced by .pub — using the core repository's own constants,
// so a change to either suffix reaches this file as a recompile rather than as
// a row that confidently names a file that is not there. The rule itself
// mirrors publicKeyPathFor in the core repository's internal/cli/cmd_keygen.go,
// which is unreachable from another module.
func publicKeyPathFor(privPath string) string {
	if strings.HasSuffix(privPath, sign.PrivateKeyFileSuffix) {
		return strings.TrimSuffix(privPath, sign.PrivateKeyFileSuffix) + sign.PublicKeyFileSuffix
	}
	return privPath + sign.PublicKeyFileSuffix
}

// DefaultSigningKeyPath returns where this application looks for the operator's
// ed25519 signing key.
//
// debark itself has no default: "debark keygen" makes --out a required flag
// precisely so a key is never written somewhere the operator did not name. That
// leaves a GUI needing somewhere to look and somewhere to propose, so this is
// this application's convention rather than the CLI's, and Options.SigningKeyPath
// overrides it.
//
// The directory follows debark's own config discovery ($DEBARK_CONFIG's
// directory, else $XDG_CONFIG_HOME/debark, else %APPDATA%\debark on
// Windows, else ~/.config/debark) so the key sits beside the config that can
// name it. That rule is mirrored rather than imported because it lives in the
// core repository's internal/cli/config, which Go's internal rule makes
// unreachable from another module; if it changes there, it must change here.
func DefaultSigningKeyPath() string {
	const keyFile = "operator" + sign.PrivateKeyFileSuffix

	if v := os.Getenv("DEBARK_CONFIG"); v != "" {
		return filepath.Join(filepath.Dir(v), keyFile)
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "debark", keyFile)
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, "debark", keyFile)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "debark", keyFile)
	}
	return filepath.Join(home, ".config", "debark", keyFile)
}

// ---------------------------------------------------------------------------
// disk space
// ---------------------------------------------------------------------------

// pathSpace is free space for one location.
type pathSpace struct {
	// Path is what the caller asked about.
	Path string
	// Measured is the nearest existing ancestor that was actually measured. On
	// a first run the store directory does not exist yet, which is exactly when
	// this check matters most, so measuring an ancestor is the normal case
	// rather than a fallback.
	Measured string
	Free     uint64
	Total    uint64
	Err      error
}

type spaceObservation struct {
	Paths     []pathSpace
	Threshold uint64
}

func diskCheck(_ context.Context, o Options) Result {
	return decideDisk(probeDisk(o))
}

func probeDisk(o Options) spaceObservation {
	obs := spaceObservation{Threshold: o.LowDiskThreshold}
	for _, p := range o.SpacePaths {
		ps := pathSpace{Path: p, Measured: nearestExistingDir(p)}
		ps.Free, ps.Total, ps.Err = freeSpace(ps.Measured)
		obs.Paths = append(obs.Paths, ps)
	}
	return obs
}

func decideDisk(obs spaceObservation) Result {
	var tightest *pathSpace
	var details []string
	measured := 0

	for i := range obs.Paths {
		ps := &obs.Paths[i]
		if ps.Err != nil {
			details = append(details, ps.Path+": "+ps.Err.Error())
			continue
		}
		measured++
		details = append(details, fmt.Sprintf("%s: %s free of %s", ps.Path, humanBytes(ps.Free), humanBytes(ps.Total)))
		if tightest == nil || ps.Free < tightest.Free {
			tightest = ps
		}
	}

	if measured == 0 {
		return Result{
			Status:   StatusSkipped,
			Severity: SeverityInfo,
			Summary:  "Free disk space could not be measured on this machine.",
			Detail:   strings.Join(details, "; "),
		}
	}

	if tightest.Free < obs.Threshold {
		return Result{
			Status: StatusProblem,
			// A warning, never a blocker: how much space a bundle needs depends
			// entirely on what the operator picks, and refusing to start
			// because of a guess would be refusing builds that would have fit.
			Severity: SeverityDegraded,
			Summary:  fmt.Sprintf("Only %s is free at %s, and a bundle can easily be larger than that.", humanBytes(tightest.Free), tightest.Path),
			Remedy:   "Free some space, or pick an output folder on another drive when you build — nothing here stops you carrying on.",
			Detail:   strings.Join(details, "; "),
		}
	}

	return Result{
		Status:   StatusOK,
		Severity: SeverityInfo,
		Summary:  fmt.Sprintf("%s free where bundles and the package store will be written.", humanBytes(tightest.Free)),
		Detail:   strings.Join(details, "; "),
	}
}

// defaultSpacePaths is the debark store root, imported from the core
// repository so this check measures the filesystem debark will actually fill
// rather than one this file guessed at. The output directory is added by the
// application layer once the operator has chosen one; there is nothing to
// measure before that.
func defaultSpacePaths() []string { return []string{store.DefaultRoot()} }

// nearestExistingDir walks up from path to the first directory that exists,
// because free space is a property of the filesystem, not of a directory that
// has not been created yet.
func nearestExistingDir(path string) string {
	p := filepath.Clean(path)
	for {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// humanBytes renders a byte count in binary units, which is what a filesystem
// reports and what a bundle's own manifest counts in.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// ---------------------------------------------------------------------------
// archive reachability — opt-in, never in the default set
// ---------------------------------------------------------------------------

// archiveDialer opens the connection the reachability check makes. Indirected
// so a test can describe an unreachable host without one, following the core
// repository's own pattern for exec.LookPath in container_driver.go.
var archiveDialer = func(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// archiveObservation is one connection attempt.
type archiveObservation struct {
	// Host is Options.ArchiveHost as given.
	Host string
	// Addr is the host:port actually dialled.
	Addr string
	// Err is why the connection failed, nil when it did not.
	Err error
}

// archiveCheck reports whether the archive host of the operator's chosen target
// accepts a connection.
//
// It exists, and it is off by default, for reasons worth stating plainly.
// debark is an air-gap tool and rule 3 of this project is absolute: no
// network call to any endpoint this project operates, ever, for any reason. A
// distro archive is not such an endpoint — it is a host named in the target's
// own sources, the same host debark will fetch from — so checking it breaks
// no rule. What would break the spirit of the rule is choosing that host here:
// a hostname compiled into this file and dialled on launch is a beacon
// regardless of who owns the other end, and an air-gap tool that opens a socket
// on start-up is indefensible whatever it connects to.
//
// So the host is never this package's to pick. Options.ArchiveHost is empty
// unless the caller sets it, this check is not registered at all when it is
// empty, and the caller only knows a host after the operator has chosen a
// target. The result is a check that is lazy by construction, skippable by
// omission, and cannot run before the operator has done something that makes
// the archive relevant.
//
// The connection is a TCP handshake and nothing more: no request is sent, no
// bytes are written, and the socket is closed the instant it opens. Failure is
// SeverityDegraded because an unreachable archive costs a new catalogue and a
// new build, not the catalogues already on disk.
func archiveCheck(ctx context.Context, o Options) Result {
	return decideArchive(probeArchive(ctx, o))
}

func probeArchive(ctx context.Context, o Options) archiveObservation {
	obs := archiveObservation{Host: o.ArchiveHost, Addr: archiveAddr(o.ArchiveHost)}
	if obs.Addr == "" {
		return obs
	}
	conn, err := archiveDialer(ctx, obs.Addr)
	if err != nil {
		obs.Err = err
		return obs
	}
	_ = conn.Close()
	return obs
}

func decideArchive(obs archiveObservation) Result {
	switch {
	case obs.Addr == "":
		return Result{
			Status:   StatusSkipped,
			Severity: SeverityInfo,
			Summary:  "No archive host has been chosen yet, so nothing was contacted.",
		}
	case obs.Err != nil:
		return Result{
			Status:   StatusProblem,
			Severity: SeverityDegraded,
			Summary:  fmt.Sprintf("%s did not accept a connection.", obs.Host),
			Remedy:   "Check your network or proxy settings. Catalogues already on disk still work; building a new one, or fetching packages, needs this host.",
			Detail:   obs.Addr + ": " + obs.Err.Error(),
		}
	default:
		return Result{
			Status:   StatusOK,
			Severity: SeverityInfo,
			Summary:  fmt.Sprintf("%s is reachable.", obs.Host),
			Detail:   "TCP connection to " + obs.Addr + " opened and closed; nothing was sent.",
		}
	}
}

// archiveAddr normalises Options.ArchiveHost into a host:port. A bare host gets
// port 443; an explicit port is honoured, so an operator behind a proxy that
// only allows plain HTTP can pass "archive.example.com:80".
func archiveAddr(host string) string {
	h := strings.TrimSpace(host)
	if h == "" {
		return ""
	}
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h = strings.TrimSuffix(h, "/")
	if h == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(h); err == nil {
		return h
	}
	return net.JoinHostPort(h, "443")
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// currentUsername is the account name the container-group remedy names. Read
// from the environment rather than os/user because the only use is composing a
// command for the operator to read, and a cgo-backed passwd lookup on the
// cold-start path buys nothing here.
func currentUsername() string {
	for _, k := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
