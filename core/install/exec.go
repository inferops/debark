package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/inferops/debark/core/dferr"
)

// cmdOutput is one apt-get/dpkg invocation's captured result.
type cmdOutput struct {
	Argv     []string
	Combined []byte
	ExitCode int
}

// failureLineRE matches the lines apt-get and dpkg use to say what actually
// went wrong: apt's own "E: ..." errors and dpkg's "dpkg: ...",
// "dpkg-deb: ...", "dpkg-query: ..." family. Warnings ("W: ...") are
// deliberately not matched: they are present on perfectly successful runs and
// would crowd out the real cause.
var failureLineRE = regexp.MustCompile(`^(?:E:|dpkg(?:-[a-z]+)?:)`)

// Bounds on failureDetail's result. This string ends up inside a Go error,
// inside Report.Problems, and from there inside the durable evidence log's
// JSON, so it must stay a message and never become a transcript: the full
// output is still available in cmdOutput and (via Deps.OnAptOutput and
// Report.AptOutputDigest) to the caller, which is where an operator goes for
// everything. Six lines and 600 bytes is enough for the measured failures
// below - a dpkg refusal line plus its indented explanation, or apt's unmet
// block plus its E: summary - with room to spare.
const (
	maxDetailLines = 6
	maxDetailBytes = 600
)

// failureDetail extracts the part of a failed apt-get/dpkg run's captured
// output that explains the failure.
//
// It replaces a first-non-empty-line summary, which discarded the explanation
// in every real failure this project has debugged. Measured 2026-09-06: a
// foreign-architecture bundle installed on a target without
// `dpkg --add-architecture i386` failed with rc=100 and this output —
//
//	Reading package lists...
//	...
//	dpkg: error processing archive /...  /zlib1g_..._i386.deb (--unpack):
//	 package architecture (i386) does not match system (amd64)
//	E: Sub-process /usr/bin/dpkg returned an error code (1)
//
// — and install reported it as "apt-get install: apt-get exited 100: Reading
// package lists...". The one line kept was apt's banner; every line that said
// what happened was thrown away, and two entirely different failures became
// indistinguishable in the report for hours. The failure mode this prevents
// is not "a sparse error message", it is an operator unable to tell which of
// two problems they have.
//
// Two details matter and are why this is not a one-line grep. dpkg puts the
// actual reason on the INDENTED CONTINUATION line after its "dpkg: error
// processing archive ..." header, so a filter that kept only matching lines
// would keep "error processing archive X" and drop "package architecture
// (i386) does not match system (amd64)" - the only text that names the
// cause. And when nothing matches at all (a tool that failed without an E:
// or dpkg: line), the LAST lines are kept rather than the first, because
// output ends with the failure and begins with the banner.
func failureDetail(b []byte) string {
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")

	var kept []string
	inFailure := false
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		switch {
		case trimmed == "":
			inFailure = false
		case failureLineRE.MatchString(trimmed):
			inFailure = true
			kept = append(kept, trimmed)
		case inFailure && (strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")):
			// dpkg's continuation line: the reason belonging to the header
			// line just above it.
			kept = append(kept, trimmed)
		default:
			inFailure = false
		}
	}
	if len(kept) == 0 {
		for i := len(lines) - 1; i >= 0 && len(kept) < maxDetailLines; i-- {
			if s := strings.TrimSpace(lines[i]); s != "" {
				kept = append([]string{s}, kept...)
			}
		}
	}
	if len(kept) == 0 {
		return ""
	}

	// Elide the middle, not the tail: the first kept lines name the first
	// thing that broke (the measured run refused several archives in a row,
	// all for the same reason) and the last is the summary apt exits on, so
	// dropping either end would lose one of the two facts worth having.
	if len(kept) > maxDetailLines {
		head := kept[:maxDetailLines-1]
		out := make([]string, 0, maxDetailLines+1)
		out = append(out, head...)
		out = append(out, "...", kept[len(kept)-1])
		kept = out
	}

	detail := strings.Join(kept, "; ")
	if len(detail) > maxDetailBytes {
		cut := maxDetailBytes - 3
		// Back off to a rune boundary: apt output is ASCII under LC_ALL=C,
		// but a package description quoted in an error need not be, and a
		// half-rune would corrupt the JSON this string is written into.
		for cut > 0 && !utf8.RuneStart(detail[cut]) {
			cut--
		}
		detail = detail[:cut] + "..."
	}
	return detail
}

// execCommandContext constructs the *exec.Cmd for one invocation. It is a
// package variable, not a direct call to exec.CommandContext, purely so
// tests can substitute a fake — the standard cross-platform "TestHelperProcess"
// exec idiom (https://npf.io/2015/06/testing-exec-command/, and Go's own
// os/exec tests): re-exec the test binary itself with -test.run=TestHelperProcess,
// so a fake apt-get/dpkg needs no shell, no script interpreter and no real
// binary on PATH, on any of Windows/Linux/macOS. Production code never
// reassigns this; it is exec.CommandContext, unmodified.
var execCommandContext = exec.CommandContext

// runProcess execs path with args, capturing stdout and stderr together
// (core/ must not print, and must not read a terminal — contract-brief rule
// 7 — so the child's stdio is never connected to the real console either
// way: Stdin is left unset, which every supported platform's exec package
// treats as an already-closed input, so a child that tries to prompt gets an
// immediate EOF rather than a hang, and Stdout/Stderr are captured, never
// inherited).
func runProcess(ctx context.Context, path string, args []string, env []string) (cmdOutput, error) {
	cmd := execCommandContext(ctx, path, args...)
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := cmdOutput{Argv: append([]string{path}, args...), Combined: buf.Bytes()}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return out, nil
	case errors.As(err, &exitErr):
		out.ExitCode = exitErr.ExitCode()
		if detail := failureDetail(out.Combined); detail != "" {
			return out, fmt.Errorf("%s exited %d: %s", filepath.Base(path), out.ExitCode, detail)
		}
		// A tool that failed silently gets no dangling ": " - "dpkg exited 1"
		// is the whole truth in that case.
		return out, fmt.Errorf("%s exited %d", filepath.Base(path), out.ExitCode)
	default:
		return out, fmt.Errorf("run %s: %w", filepath.Base(path), err)
	}
}

// buildEnv is the environment apt-get/dpkg run under. LC_ALL is the
// standard, sufficient way to force C locale output so line parsing is
// stable; LANGUAGE is also cleared because glibc's gettext consults it
// *before* LC_ALL for message-catalog selection, a well-known trap that would
// otherwise silently re-introduce translated apt output on a system whose
// operator has LANGUAGE set in their shell.
func buildEnv(fast bool) []string {
	env := os.Environ()
	env = setEnv(env, "LC_ALL", "C")
	env = setEnv(env, "LANG", "C")
	env = setEnv(env, "LANGUAGE", "C")
	if fast {
		// --fast: noninteractive debconf, paired with dpkg
		// --force-unsafe-io added at the private-root and dpkg-argv level.
		env = setEnv(env, "DEBIAN_FRONTEND", "noninteractive")
	}
	return env
}

func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}

// stdinIsTerminal reports whether this process's own stdin looks like an
// interactive terminal. It queries file-mode metadata only (a stat, not a
// read) — core/ never reads a terminal (contract-brief rule 7), and this
// never causes a byte to be consumed from stdin. It exists solely so
// requireConfirmation can tell "nobody is here to have been asked" (no
// Options.Yes, no terminal: refuse up front,) from "a human is
// plausibly present" (the CLI, whose job prompting is, presumably already
// asked). apt/dpkg's own stdio is never connected to the real terminal in
// either case (see runProcess) — that refusal is what actually prevents a
// hang.
//
// It is a package variable, like execCommandContext, so tests can pin the
// answer instead of depending on whatever stdin happens to be under `go
// test` (which varies by how the suite is invoked). Production code never
// reassigns it.
var stdinIsTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// requireConfirmation is the "without Options.Yes and without a TTY, refuse
// rather than hang" rule. Without it, apt-get/dpkg run without -y and
// (since their stdio is never connected to the real terminal, see
// runProcess) any prompt they would have shown hits an immediate EOF instead
// — never a hang either way. This check exists to turn that into a clear,
// immediate, classified refusal instead of a delegated, opaque apt failure
// whenever nothing is even plausibly present to answer a prompt.
func requireConfirmation(opts Options) error {
	if opts.Yes {
		return nil
	}
	if stdinIsTerminal() {
		return nil
	}
	return dferr.New(dferr.Usage, "install: refusing to install without confirmation").
		WithHint("pass --yes to install non-interactively")
}

// lookExecutable reports whether path resolves to a runnable file: an
// absolute/relative path is stat'd directly, a bare name is resolved via
// PATH, exactly like the shell would. It never executes anything.
func lookExecutable(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("no path configured")
	}
	if strings.ContainsRune(path, '/') || strings.ContainsRune(path, filepath.Separator) {
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return fmt.Errorf("%s is a directory", path)
		}
		return nil
	}
	_, err := exec.LookPath(path)
	return err
}
