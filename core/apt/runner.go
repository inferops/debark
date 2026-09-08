package apt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/inferops/debark/core/dferr"
)

// RunError is returned (wrapped in a *dferr.Error of class Resolution) when
// apt-get exits non-zero. It carries the full captured Output so a caller can
// parse apt's own diagnostics instead of trusting the exit code alone.
// Because dferr.Error.Unwrap returns its wrapped cause, errors.As(err,
// &runErr) reaches this type through the dferr wrapper.
type RunError struct {
	Output Output
	// Err is the underlying *exec.ExitError.
	Err error
}

func (e *RunError) Error() string {
	if tail := lastLines(e.Output.Combined(), 4); tail != "" {
		return fmt.Sprintf("apt-get exit %d: %s", e.Output.ExitCode, tail)
	}
	return fmt.Sprintf("apt-get exit %d", e.Output.ExitCode)
}

func (e *RunError) Unwrap() error { return e.Err }

func lastLines(b []byte, n int) string {
	trimmed := strings.TrimRight(string(b), "\r\n")
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

type execRunner struct {
	aptGetPath string
	// aptConfigPath, when set, is exported as APT_CONFIG in the child
	// process's environment — the only mechanism that actually redirects
	// apt's apt.conf/apt.conf.d scan (docs/experiments/
	// E4-aptconfd-leakage.md; see PrivateRoot.AptConfigPath). It is never a
	// "-c" flag: that was measured not to work.
	aptConfigPath string
}

// withAptConfig returns a copy of r that exports APT_CONFIG=path to the
// child process. It returns a copy rather than mutating r so concurrent
// callers (a backend used for more than one resolution) never race over
// which root's configuration a given Run call actually uses.
func (r *execRunner) withAptConfig(path string) *execRunner {
	c := *r
	c.aptConfigPath = path
	return &c
}

// newRunner is api.go's NewRunner body: the exec-based apt-get runner. An
// empty aptGetPath means "apt-get" resolved via PATH.
func newRunner(aptGetPath string) Runner {
	if aptGetPath == "" {
		aptGetPath = "apt-get"
	}
	return &execRunner{aptGetPath: aptGetPath}
}

// Run executes apt-get with opts as "-o value" pairs followed by args,
// capturing stdout and stderr separately. LC_ALL/LANG are forced to C and
// DEBIAN_FRONTEND to noninteractive so every parser in this package can rely
// on apt's untranslated, non-interactive output shape.
func (r *execRunner) Run(ctx context.Context, opts []string, args ...string) (Output, error) {
	full := make([]string, 0, len(opts)*2+len(args))
	for _, o := range opts {
		full = append(full, "-o", o)
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, r.aptGetPath, full...)
	cmd.Env = cleanEnv(os.Environ(), r.aptConfigPath)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	out := Output{
		// argv[0] is recorded as the logical command name, not r.aptGetPath
		// (which may be an absolute path that varies by machine or is a test
		// fixture), so Output.Argv stays stable enough to hash and compare
		// across builders (ClosedWorld.CommandDigest).
		Argv:   append([]string{"apt-get"}, full...),
		Stdout: stdout.Bytes(),
		Stderr: stderr.Bytes(),
	}

	if runErr == nil {
		return out, nil
	}

	if ctx.Err() != nil {
		return out, dferr.Wrap(dferr.Resolution, ctx.Err(), "apt-get %s: cancelled", strings.Join(args, " "))
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		out.ExitCode = exitErr.ExitCode()
		rerr := &RunError{Output: out, Err: runErr}
		return out, dferr.Wrap(dferr.Resolution, rerr, "apt-get %s: exit %d", strings.Join(args, " "), out.ExitCode)
	}

	// exec itself failed (binary missing, not executable, ...): an
	// environment problem, not a resolution failure.
	envErr := &dferr.Error{
		Class: dferr.Environment,
		Msg:   fmt.Sprintf("apt-get: exec %s", r.aptGetPath),
		Err:   runErr,
	}
	return out, envErr.WithHint("install apt-get (the apt package) or pass a working apt-get path")
}

// cleanEnv copies base with LC_ALL, LANG, LANGUAGE and DEBIAN_FRONTEND
// removed, then appends the fixed values apt-get must run under.
// cleanEnv copies base with LC_ALL, LANG, LANGUAGE, DEBIAN_FRONTEND and
// APT_CONFIG removed, then appends the fixed values apt-get must run under
// plus APT_CONFIG=aptConfigPath when aptConfigPath is non-empty (see
// execRunner.aptConfigPath).
func cleanEnv(base []string, aptConfigPath string) []string {
	env := make([]string, 0, len(base)+5)
	for _, kv := range base {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		switch strings.ToUpper(k) {
		case "LC_ALL", "LANG", "LANGUAGE", "DEBIAN_FRONTEND", "APT_CONFIG":
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "LC_ALL=C", "LANG=C", "LANGUAGE=C", "DEBIAN_FRONTEND=noninteractive")
	if aptConfigPath != "" {
		env = append(env, "APT_CONFIG="+aptConfigPath)
	}
	return env
}
