package harness

import (
	"context"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Finding is one assertion outcome, accumulated rather than failing fast, so
// a fixture that fails three checks reports all three instead of only the
// first (mirrors verify.Report's own "keep checking so an operator sees the
// full extent" philosophy).
type Finding struct {
	Check  string
	OK     bool
	Detail string
	// Plumbing marks a finding that did not hold because the *harness*
	// could not carry out the check — `docker exec` would not run, a
	// `docker cp` failed — as opposed to the product having produced a
	// wrong answer. Both are !OK, but only the second is evidence about
	// debark, so the caller reports a Plumbing finding as a blocked row
	// (setBlocked) and never as a failed one. Without the distinction a
	// Docker hiccup during the assertion stage is written down as "the
	// installed package does not work", which is a false accusation of
	// exactly the kind hack/matrix/results/README.md documents.
	Plumbing bool
}

func ok(check string) Finding { return Finding{Check: check, OK: true} }
func fail(check, format string, a ...any) Finding {
	return Finding{Check: check, OK: false, Detail: fmt.Sprintf(format, a...)}
}

// plumbing builds a !OK finding that blames the harness, not the product.
func plumbing(check, format string, a ...any) Finding {
	return Finding{Check: check, OK: false, Plumbing: true, Detail: fmt.Sprintf(format, a...)}
}

// dockerExecFailedToRun reports whether code is one `docker exec` produces
// for its own failures rather than passing up from the command: 125 (the
// docker CLI/daemon itself errored), 126 (the command could not be invoked),
// 127 (the command was not found). For a tool that is part of the base image
// — dpkg-query, dpkg — these mean the harness could not ask the question,
// which is not the same as the target having answered it badly.
//
// This deliberately does NOT apply to AssertBinaries: there, 126/127 means
// the package's own binary is missing or unrunnable, which is the exact
// product failure that assertion exists to catch.
func dockerExecFailedToRun(code int) bool {
	return code == 125 || code == 126 || code == 127
}

// dpkgFullyInstalled reports whether a `dpkg-query -W -f=${Status}` answer
// means "this package is on the machine and usable".
//
// ${Status} is THREE fields — desired action, error flag, current status —
// and only the last two answer that question. Both assertions below used to
// test for the literal string "install ok installed", which silently folded
// the DESIRED field into the check. A package the operator has held reports
// "hold ok installed": it is installed, configured and usable, and its first
// field differs only because someone asked apt not to change it.
//
// That mistake was invisible until the harness started installing onto the
// machine the fixture actually declares. Before that, held-package's target
// was a stock image where nothing had ever been held, so the fixture asserted
// "install ok installed" against a package that had simply been installed
// fresh, and passed without exercising the hold at all.
//
// The negation is the dangerous direction and the reason this is one shared
// predicate rather than two spellings: under the old string test a HELD,
// INSTALLED package satisfied AssertPackagesAbsent. A fixture proving a
// package was correctly left out of a bundle would have passed while that
// package sat installed on the target.
//
// Kept deliberately narrow. "deinstall ok installed" and "purge ok installed"
// (installed, but marked for removal) are NOT accepted: those describe a
// machine mid-transaction, which is not a state any fixture here should be
// asserting against, and treating it as present would hide a half-finished
// install. Anything whose error flag is not "ok" (reinstreq) or whose status
// is not "installed" (unpacked, half-configured, config-files, ...) is not
// installed either.
func dpkgFullyInstalled(status string) bool {
	f := strings.Fields(strings.TrimSpace(status))
	if len(f) != 3 {
		return false
	}
	desired, errFlag, state := f[0], f[1], f[2]
	return (desired == "install" || desired == "hold") && errFlag == "ok" && state == "installed"
}

// AssertPackagesPresent runs `dpkg-query -W -f='${Status}'` for each name on
// the freshly-installed target and records whether dpkg considers it fully
// installed — see dpkgFullyInstalled, which accepts a HELD package too. This
// is the weaker of the two checks the task asks for; see AssertBinary for the
// one that actually matters.
func AssertPackagesPresent(ctx context.Context, target *Container, names []string) []Finding {
	var out []Finding
	for _, name := range names {
		res, err := target.Exec(ctx, ExecOpts{}, "dpkg-query", "-W", "-f=${Status}", name)
		switch {
		case err != nil:
			out = append(out, plumbing("package-present:"+name, "dpkg-query could not run: %v", err))
		case dockerExecFailedToRun(res.ExitCode):
			out = append(out, plumbing("package-present:"+name, "docker exec could not run dpkg-query (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Combined())))
		case res.ExitCode != 0:
			out = append(out, fail("package-present:"+name, "dpkg-query exit %d: %s", res.ExitCode, strings.TrimSpace(res.Combined())))
		case !dpkgFullyInstalled(string(res.Stdout)):
			out = append(out, fail("package-present:"+name, "dpkg status = %q, want a fully-installed package", string(res.Stdout)))
		default:
			out = append(out, ok("package-present:"+name))
		}
	}
	return out
}

// AssertPackagesAbsent is AssertPackagesPresent's negation: names must NOT
// be dpkg-installed (either dpkg-query fails to find them, which is success
// here, or it finds them in a state dpkgFullyInstalled rejects). It shares
// that predicate with its twin deliberately — see there for what a held
// package did to this assertion while the two spellings were separate.
// This is what proves a Recommends, a redistribution-flagged component or a
// policy exclusion was genuinely left out of the bundle, not merely absent
// from the assertion list.
func AssertPackagesAbsent(ctx context.Context, target *Container, names []string) []Finding {
	var out []Finding
	for _, name := range names {
		res, err := target.Exec(ctx, ExecOpts{}, "dpkg-query", "-W", "-f=${Status}", name)
		label := "package-absent:" + name
		switch {
		case err != nil:
			out = append(out, plumbing(label, "dpkg-query could not run: %v", err))
		case dockerExecFailedToRun(res.ExitCode):
			// This branch matters more than its twin in
			// AssertPackagesPresent: without it, a docker exec that never
			// reached dpkg-query at all would land in the "non-zero means
			// unknown package" case below and be recorded as a PASS. A
			// harness failure that manufactures a green result is the worst
			// outcome this package can produce.
			out = append(out, plumbing(label, "docker exec could not run dpkg-query (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Combined())))
		case res.ExitCode != 0:
			// dpkg-query exits non-zero for an unknown package — exactly
			// the expected outcome, so this is a pass.
			out = append(out, ok(label))
		case dpkgFullyInstalled(string(res.Stdout)):
			out = append(out, fail(label, "found installed (dpkg status = %q), want absent", string(res.Stdout)))
		default:
			out = append(out, ok(label))
		}
	}
	return out
}

// AssertBinaries is the assertion the task calls out as the one that
// matters: "Running the binary is the assertion that matters — a package can
// install and still be unusable if its dependencies were missed." dpkg can
// report a package fully configured while a missing shared library makes the
// binary itself refuse to start; only actually running it catches that.
func AssertBinaries(ctx context.Context, target *Container, checks []BinaryCheck) []Finding {
	var out []Finding
	for _, chk := range checks {
		argv := append([]string{chk.Path}, chk.Args...)
		res, err := target.Exec(ctx, ExecOpts{}, argv...)
		label := "binary-runs:" + chk.Path
		if err != nil {
			// The binary not running because docker exec itself failed is
			// not the same claim as the binary not running because its
			// dependencies were missed, even though this is the assertion
			// where the second one is caught.
			out = append(out, plumbing(label, "could not exec: %v", err))
			continue
		}
		if res.ExitCode != chk.ExitCode {
			out = append(out, fail(label, "exit code %d, want %d; output: %s", res.ExitCode, chk.ExitCode, truncate(res.Combined(), 500)))
			continue
		}
		if chk.StdoutContains != "" && !strings.Contains(string(res.Stdout), chk.StdoutContains) {
			out = append(out, fail(label, "stdout does not contain %q; got: %s", chk.StdoutContains, truncate(string(res.Stdout), 500)))
			continue
		}
		out = append(out, ok(label))
	}
	return out
}

// AssertELF checks that a foreign-arch, executable-less package (a shared
// library, typically) genuinely ships the declared machine architecture's
// code — the same spirit as AssertBinaries ("truly usable, not merely
// dpkg-registered") applied to a package with nothing to exec. It discovers
// a candidate regular file from `dpkg -L Package` and inspects it with Go's
// stdlib debug/elf after copying it to a host temp file, rather than
// depending on `file`/`readelf` (not guaranteed present — see ElfCheck's doc)
// or on a package-internal path that drifts across releases.
func AssertELF(ctx context.Context, target *Container, workDir string, checks []ElfCheck) []Finding {
	var out []Finding
	for _, chk := range checks {
		label := "elf-class:" + chk.Package
		f, detail, isPlumbing := findELFFile(ctx, target, workDir, chk.Package)
		if f == "" {
			if isPlumbing {
				out = append(out, plumbing(label, "%s", detail))
			} else {
				out = append(out, fail(label, "%s", detail))
			}
			continue
		}
		got, err := elfClassOf(f)
		if err != nil {
			// The candidate's first four bytes were ELF's magic when it was
			// copied out, so a header that will not parse points at the
			// copy (a truncated docker cp) rather than at anything debark
			// put in the bundle.
			out = append(out, plumbing(label, "reading ELF header of %s (copied to %s): %v", detail, f, err))
			continue
		}
		if got != chk.Class {
			out = append(out, fail(label, "%s is %s, want %s", detail, got, chk.Class))
			continue
		}
		out = append(out, ok(label))
	}
	return out
}

// maxELFCandidates bounds how many docker cp round trips findELFFile will
// pay for before giving up.
const maxELFCandidates = 8

// findELFFile lists Package's REGULAR files only (dpkg -L also lists the
// directories it owns — e.g. /usr/lib itself — and blindly `docker cp`-ing
// one of those would copy an entire shared system directory tree, so the
// regular-file filter runs server-side, inside the container, via `[ -f ]`,
// before anything is copied to the host). It copies up to
// maxELFCandidates likely candidates out in turn and returns the host path
// of the first one whose magic bytes are ELF's ("\x7fELF"), plus its
// in-container path for diagnostics.
//
// The third result says whether an empty hostPath means "the harness could
// not look" (docker would not run, every copy failed) rather than "we looked
// and this package ships no ELF file": the caller reports the first as a
// blocked row and only the second as a product failure.
func findELFFile(ctx context.Context, target *Container, workDir, pkg string) (hostPath, containerPathOrDetail string, isPlumbing bool) {
	script := fmt.Sprintf(`dpkg -L %s | while IFS= read -r f; do [ -f "$f" ] && printf '%%s\n' "$f"; done`, shQuote(pkg))
	res, err := target.Shell(ctx, ExecOpts{}, script)
	if err != nil {
		return "", fmt.Sprintf("dpkg -L %s could not run: %v", pkg, err), true
	}
	if dockerExecFailedToRun(res.ExitCode) {
		return "", fmt.Sprintf("docker exec could not run dpkg -L %s (exit %d): %s", pkg, res.ExitCode, strings.TrimSpace(res.Combined())), true
	}
	if res.ExitCode != 0 {
		return "", fmt.Sprintf("dpkg -L %s: exit %d: %s", pkg, res.ExitCode, strings.TrimSpace(res.Combined())), false
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			files = append(files, p)
		}
	}
	// Try the most plausible ELF locations first (libraries, binaries), then
	// anything else regular, so a lucky early hit skips the rest entirely.
	sortByLikelyELF(files)

	attempts := 0
	copied := 0
	var lastCopyErr error
	for i, p := range files {
		if attempts >= maxELFCandidates {
			break
		}
		attempts++
		dest := fmt.Sprintf("%s/elfcheck-%s-%d", workDir, sanitize(pkg), i)
		if err := target.CopyOut(ctx, p, dest); err != nil {
			// Vanished between listing and copy, or unreadable — try the
			// next, but remember why, because "every copy failed" and "we
			// inspected everything and none of it was ELF" are different
			// claims and only the second one is about the product.
			lastCopyErr = err
			continue
		}
		copied++
		if looksLikeELF(dest) {
			return dest, p, false
		}
	}
	if copied == 0 && lastCopyErr != nil {
		return "", fmt.Sprintf("could not copy any of %d candidate file(s) owned by %s out of the container (tried %d, last error: %v)",
			len(files), pkg, attempts, lastCopyErr), true
	}
	return "", fmt.Sprintf("no ELF file found among %d regular file(s) owned by %s (tried %d, inspected %d)", len(files), pkg, attempts, copied), false
}

func sortByLikelyELF(files []string) {
	score := func(p string) int {
		switch {
		case strings.Contains(p, "/lib"):
			return 0
		case strings.Contains(p, "/bin"):
			return 1
		default:
			return 2
		}
	}
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && score(files[j-1]) > score(files[j]); j-- {
			files[j-1], files[j] = files[j], files[j-1]
		}
	}
}

func looksLikeELF(hostPath string) bool {
	f, err := os.Open(hostPath)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic == [4]byte{0x7f, 'E', 'L', 'F'}
}

// elfClassOf reads hostPath's ELF class byte (e_ident[EI_CLASS]) directly —
// stdlib debug/elf.NewFile requires a ReaderAt, which os.File already is.
func elfClassOf(hostPath string) (string, error) {
	f, err := elf.Open(hostPath)
	if err != nil {
		return "", err
	}
	// Read-only open of a host file; nothing is written through f, so a close
	// failure cannot affect the class we report. (errcheck flags this one and
	// not the *os.File closes nearby because .golangci.yml excludes
	// (*os.File).Close by static type and elf.File is a different type.)
	defer func() { _ = f.Close() }()
	switch f.Class {
	case elf.ELFCLASS32:
		return "ELF32", nil
	case elf.ELFCLASS64:
		return "ELF64", nil
	default:
		return "", fmt.Errorf("unrecognised ELF class %v", f.Class)
	}
}

// AssertBundleFiles checks files inside the bundle as copied out to the host.
// hostBundleDir is the host path the bundle was copied to; each check's Path
// is bundle-relative with forward slashes.
//
// A file that cannot be read is a product finding, not a plumbing one, and
// that is on purpose: this assertion exists precisely to catch a build that
// did not write a report it promised, and "lock.json is not there" is the
// loudest possible form of that. The harness copied the whole directory out
// itself one stage earlier and recordBundleStats already walked it
// successfully, so a read failure here is about the bundle's contents rather
// than about the copy.
func AssertBundleFiles(hostBundleDir string, checks []BundleFileCheck) []Finding {
	var out []Finding
	for _, chk := range checks {
		label := "bundle-file:" + chk.Path
		data, err := os.ReadFile(filepath.Join(hostBundleDir, filepath.FromSlash(chk.Path)))
		if err != nil {
			out = append(out, fail(label, "the build did not leave a readable %s in the bundle: %v", chk.Path, err))
			continue
		}
		body := string(data)
		missing := false
		for _, want := range chk.Contains {
			if !strings.Contains(body, want) {
				out = append(out, fail(label, "does not contain %q; %s is %d bytes: %s", want, chk.Path, len(data), truncate(body, 800)))
				missing = true
			}
		}
		// The value itself is NOT quoted into the finding: an Absent check
		// exists to prove a secret-shaped string never reached the bundle,
		// and printing it into a row's blocker — which lands in the result
		// JSON and in the console table — would copy it right back out into
		// the artefact this suite hands a human. The path, the offset and
		// the length are enough to find it, and the fixture already says
		// what it planted.
		for _, unwanted := range chk.Absent {
			if idx := strings.Index(body, unwanted); idx >= 0 {
				out = append(out, fail(label,
					"contains a value the fixture requires to be absent (%d bytes, at offset %d of %d) — the fixture names it; it is not repeated here because this message is itself written into the run artefact",
					len(unwanted), idx, len(data)))
				missing = true
			}
		}
		if !missing {
			out = append(out, ok(label))
		}
	}
	return out
}

// containerPathProbe is the shell the presence check runs. It prints a word
// rather than relying on the exit status of `test -e`, because `test -e`
// answers "absent" with exit 1 and `docker exec` answers "I could not run
// anything at all" with 125/126/127 — and an absence check that reads a
// harness failure as a pass is the shape AssertPackagesAbsent's own comment
// warns about: a plumbing failure that manufactures a green result. Here the
// green result would be "the arbitrary-code-execution path is closed", which
// is the last claim in this tree that should ever be made by accident.
const containerPathProbe = `if [ -e %s ]; then echo PRESENT; else echo ABSENT; fi`

// AssertContainerPaths runs each check against the container its Container
// role names. byRole maps "state"/"builder" to the live containers; a role
// with no container is a plumbing finding, never a pass.
//
// See ContainerPathCheck for why a failed present:true check is reported as
// plumbing (the fixture's instrumentation did not fire, so the row proved
// nothing) while a failed present:false check is a product failure.
func AssertContainerPaths(ctx context.Context, byRole map[string]*Container, checks []ContainerPathCheck) []Finding {
	var out []Finding
	for _, chk := range checks {
		label := fmt.Sprintf("container-path:%s:%s", chk.Container, chk.Path)
		c := byRole[chk.Container]
		if c == nil {
			out = append(out, plumbing(label, "this run has no %q container to look in", chk.Container))
			continue
		}
		res, err := c.Shell(ctx, ExecOpts{}, fmt.Sprintf(containerPathProbe, shQuote(chk.Path)))
		switch {
		case err != nil:
			out = append(out, plumbing(label, "could not probe the path: %v", err))
			continue
		case dockerExecFailedToRun(res.ExitCode):
			out = append(out, plumbing(label, "docker exec could not run the probe (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Combined())))
			continue
		case res.ExitCode != 0:
			out = append(out, plumbing(label, "the probe exited %d: %s", res.ExitCode, strings.TrimSpace(res.Combined())))
			continue
		}
		var present bool
		switch strings.TrimSpace(string(res.Stdout)) {
		case "PRESENT":
			present = true
		case "ABSENT":
			present = false
		default:
			out = append(out, plumbing(label, "the probe printed %q, which is neither PRESENT nor ABSENT, so nothing was established",
				truncate(strings.TrimSpace(res.Combined()), 300)))
			continue
		}
		switch {
		case present == chk.Present:
			out = append(out, ok(label))
		case chk.Present:
			out = append(out, plumbing(label,
				"the fixture's own marker never appeared, so this row could not test what it claims to: "+
					"nothing planted it, and a negative check elsewhere would then pass for a reason that has nothing to do with debark"))
		default:
			out = append(out, fail(label,
				"exists in the %s container: whatever creates it ran, and this fixture's whole claim is that it must not",
				chk.Container))
		}
	}
	return out
}

// AssertDoctorContains checks that every substring in want appears somewhere
// in a `debark doctor` invocation's combined output (typically --json, so
// the check is against the machine-readable report, but any text works).
func AssertDoctorContains(output string, want []string) []Finding {
	var out []Finding
	for _, w := range want {
		label := "doctor-contains:" + w
		if strings.Contains(output, w) {
			out = append(out, ok(label))
		} else {
			out = append(out, fail(label, "doctor output does not contain %q", w))
		}
	}
	return out
}

// AssertUnresolvedContains checks build-stage output for expected
// unresolved-input substrings (the exit-3 "incomplete" fixtures).
func AssertUnresolvedContains(output string, want []string) []Finding {
	var out []Finding
	for _, w := range want {
		label := "unresolved-contains:" + w
		if strings.Contains(output, w) {
			out = append(out, ok(label))
		} else {
			out = append(out, fail(label, "build output does not contain %q", w))
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// AllOK reports whether every finding in every given slice passed.
func AllOK(sets ...[]Finding) bool {
	for _, set := range sets {
		for _, f := range set {
			if !f.OK {
				return false
			}
		}
	}
	return true
}

// FailureSummary renders every failing finding as one string, for a
// RowResult's Blocker/failure detail field.
func FailureSummary(sets ...[]Finding) string {
	var lines []string
	for _, set := range sets {
		for _, f := range set {
			if !f.OK {
				lines = append(lines, f.Check+": "+f.Detail)
			}
		}
	}
	return strings.Join(lines, "; ")
}

// PlumbingSummary renders only the findings that failed because the harness
// could not perform the check, and is empty when there are none. A caller
// must consult it before FailureSummary: if the harness could not run one
// assertion, the row's verdict is "could not run" (blocked), not "the
// product got it wrong" (fail) — even when other assertions did fail, since
// those may be downstream of whatever broke.
func PlumbingSummary(sets ...[]Finding) string {
	var lines []string
	for _, set := range sets {
		for _, f := range set {
			if !f.OK && f.Plumbing {
				lines = append(lines, f.Check+": "+f.Detail)
			}
		}
	}
	return strings.Join(lines, "; ")
}
