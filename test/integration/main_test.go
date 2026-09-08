package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Environment variables read by runFakeAptHelper. Namespaced so they cannot
// collide with anything real apt-get/dpkg read (LC_ALL and friends,
// core/install/exec.go's buildEnv) or with core/install's own,
// unrelated GO_WANT_HELPER_PROCESS-based re-exec mechanism (see the doc
// comment on TestMain below).
const (
	envHelperMode = "DEBARK_IT_HELPER_MODE"
	envArch       = "DEBARK_IT_HELPER_ARCH"
	envSimFile    = "DEBARK_IT_HELPER_SIM_FILE"
)

// TestMain lets this test binary play two roles.
//
// Normally it just runs this package's tests. But core/install's
// Runner.Plan (exercised in pipeline_test.go's installPlanMatchesLock
// check, task requirement 7) shells out to real apt-get/dpkg binaries via
// Deps.AptPath/Deps.DpkgPath (core/install/exec.go's runProcess) — Deps
// exposes only two file-path strings for this, not a Go interface seam, so
// there is nothing to hand a fake implementation to the way apt.Backend or
// verify.Verifier let one. core/install's OWN tests solve this by
// re-executing their test binary with -test.run=TestHelperProcess through
// an unexported package variable (core/install/exechelper_test.go); that
// variable is private to core/install and unreachable from a different
// package, so a cross-package integration test needs its own copy of the
// same idea, wired through the one thing that IS exported: the path
// strings.
//
// The fix: point Deps.AptPath and Deps.DpkgPath at this very test binary
// (os.Executable()), and set envHelperMode before doing so. When
// core/install re-execs that path, the child is this same compiled test
// binary starting fresh — its generated main() calls TestMain before
// testing's own flag parsing or test selection ever run — so checking the
// env var here, first, and calling os.Exit directly, intercepts the child
// before it tries to interpret real apt-get/dpkg flags (-o, --print-
// architecture, ...) as go test flags, which would otherwise fail
// immediately. This is the same standard, cross-platform "re-exec the test
// binary as a fake subprocess" idiom
// (https://npf.io/2015/06/testing-exec-command/, and the Go standard
// library's own os/exec tests) that core/install already uses; it is
// reimplemented here, independently, rather than shared, because the
// mechanism it substitutes is unexported and package-private.
func TestMain(m *testing.M) {
	if os.Getenv(envHelperMode) == "1" {
		os.Exit(runFakeAptHelper())
	}
	os.Exit(m.Run())
}

// runFakeAptHelper plays apt-get or dpkg for exactly the small, fixed set
// of invocations install.Runner.Plan makes (core/install/runner.go):
// `dpkg --print-architecture`, `apt-get ... update`, and
// `apt-get ... -s install <names>`. It dispatches purely on which
// recognisable flags appear in os.Args[1:], the same way core/install's
// own fake helper (exechelper_test.go's TestHelperProcess) does.
func runFakeAptHelper() int {
	args := os.Args[1:]
	has := func(s string) bool {
		for _, a := range args {
			if a == s {
				return true
			}
		}
		return false
	}

	switch {
	case has("--print-architecture"):
		fmt.Fprintln(os.Stdout, os.Getenv(envArch))
		return 0

	case has("-s") && has("install"):
		catFile(os.Getenv(envSimFile))
		return 0

	case has("update"):
		// install.Runner discards `apt-get update`'s output beyond folding
		// it into AptOutputDigest (runner.go); it never parses it, so
		// silence is a faithful enough fake for what Plan actually reads.
		return 0

	default:
		fmt.Fprintln(os.Stderr, "fake apt/dpkg helper: unrecognised invocation:", strings.Join(args, " "))
		return 127
	}
}

func catFile(path string) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake apt/dpkg helper: read sim file:", err)
		return
	}
	os.Stdout.Write(data)
}

// installFakeAptDpkg points install.Deps.AptPath/DpkgPath at this test
// binary and arms TestMain's helper branch for the rest of the calling
// test: `dpkg --print-architecture` answers arch, and `apt-get -s install`
// answers with simOutput (see installSimOutput). t.Setenv scopes both
// env vars to the current test and restores the previous value on
// cleanup; that is safe here because every test in this package runs
// sequentially (none call t.Parallel).
func installFakeAptDpkg(t *testing.T, arch, simOutput string) (aptPath, dpkgPath string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	simFile := filepath.Join(t.TempDir(), "apt-sim-install.txt")
	if err := os.WriteFile(simFile, []byte(simOutput), 0o644); err != nil {
		t.Fatalf("write apt sim file: %v", err)
	}

	t.Setenv(envHelperMode, "1")
	t.Setenv(envArch, arch)
	t.Setenv(envSimFile, simFile)

	return exe, exe
}
