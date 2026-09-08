package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMain swaps execCommandContext for the whole test binary: every test in
// this package that runs apt-get/dpkg through runProcess gets the fake
// helper-process exec instead of a real one. The headline --network none
// end-to-end test (e2e_linux_test.go) does not go through this at all — it
// shells out to `docker` directly — so there is no conflict.
func TestMain(m *testing.M) {
	execCommandContext = fakeExecCommandContext
	// Deterministic default: "no terminal attached", regardless of how this
	// suite happens to be invoked. Individual tests that need the other
	// branch reassign it and restore via t.Cleanup.
	stdinIsTerminal = func() bool { return false }
	os.Exit(m.Run())
}

// fakeExecCommandContext re-execs this same test binary, selecting only
// TestHelperProcess, and hands it name+args after a "--" separator. This is
// the standard cross-platform trick for faking exec.Command in Go: no shell,
// no script interpreter, no real apt-get/dpkg anywhere on the machine.
func fakeExecCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cs := append([]string{"-test.run=TestHelperProcess", "--", name}, args...)
	return exec.CommandContext(ctx, os.Args[0], cs...)
}

// TestHelperProcess is not a real test. Run normally (GO_WANT_HELPER_PROCESS
// unset) it does nothing and passes trivially; the fake exec above invokes
// it with GO_WANT_HELPER_PROCESS=1 (inherited from the parent test process's
// environment via runProcess's env argument, which is always built from
// os.Environ()), at which point it plays the part of apt-get or dpkg,
// entirely driven by the FAKE_* environment variables the calling test set
// with t.Setenv.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	rest := args[1:] // args[0] is the configured AptPath/DpkgPath; the fake
	// routes purely on the real flags that follow, since a test may point
	// both AptPath and DpkgPath at the same re-exec'able binary.

	if logPath := os.Getenv("FAKE_LOG_FILE"); logPath != "" {
		if line, err := json.Marshal(args); err == nil {
			if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				f.Write(line)
				f.Write([]byte("\n"))
				f.Close()
			}
		}
	}

	has := func(s string) bool {
		for _, a := range rest {
			if a == s {
				return true
			}
		}
		return false
	}
	catFile := func(path string) {
		if path == "" {
			return
		}
		data, err := os.ReadFile(path)
		if err == nil {
			os.Stdout.Write(data)
		}
	}

	switch {
	// Matched before --print-architecture only for readability: has() is an
	// exact-token comparison, so the two flags can never be confused for one
	// another. Real dpkg prints one architecture per line here, and prints
	// NOTHING (exit 0) on a machine that has never run
	// `dpkg --add-architecture`, which is the default this fake reproduces:
	// every pre-existing test in this package describes a single-architecture
	// machine and must keep behaving exactly as it did.
	case has("--print-foreign-architectures"):
		for _, a := range strings.Fields(os.Getenv("FAKE_FOREIGN_ARCHS")) {
			fmt.Fprintln(os.Stdout, a)
		}
		os.Exit(envExitCode("FAKE_DPKG_FOREIGN_ARCH_EXIT"))

	case has("--print-architecture"):
		fmt.Fprintln(os.Stdout, envOr("FAKE_ARCH", "amd64"))
		os.Exit(envExitCode("FAKE_DPKG_ARCH_EXIT"))

	// dpkg-query -W --showformat=... : the installed-set query
	// basedivergence.go makes, and the ONLY invocation in this package that
	// goes to Deps.DpkgQueryPath rather than Deps.DpkgPath. The default is an
	// empty answer with exit 0, i.e. a machine dpkg knows no packages on -
	// which is deliberately NOT the state any pre-existing test in this file
	// describes, because a bundle built from a captured snapshot never
	// reaches this branch at all. A test that wants an answer points
	// FAKE_DPKG_QUERY_FILE at one.
	case has("-W"):
		catFile(os.Getenv("FAKE_DPKG_QUERY_FILE"))
		os.Exit(envExitCode("FAKE_DPKG_QUERY_EXIT"))

	case has("--unpack"):
		catFile(os.Getenv("FAKE_DPKG_UNPACK_OUTPUT_FILE"))
		os.Exit(envExitCode("FAKE_DPKG_UNPACK_EXIT"))

	case has("--configure"):
		catFile(os.Getenv("FAKE_DPKG_CONFIGURE_OUTPUT_FILE"))
		os.Exit(envExitCode("FAKE_DPKG_CONFIGURE_EXIT"))

	case has("-s") && has("install"):
		catFile(os.Getenv("FAKE_APT_SIM_FILE"))
		os.Exit(envExitCode("FAKE_APT_SIM_EXIT"))

	case has("-s") && has("full-upgrade"):
		catFile(os.Getenv("FAKE_APT_SIM_UPGRADE_FILE"))
		os.Exit(envExitCode("FAKE_APT_SIM_UPGRADE_EXIT"))

	case has("update"):
		catFile(os.Getenv("FAKE_APT_UPDATE_OUTPUT_FILE"))
		os.Exit(envExitCode("FAKE_APT_UPDATE_EXIT"))

	case has("install"):
		catFile(os.Getenv("FAKE_APT_INSTALL_OUTPUT_FILE"))
		os.Exit(envExitCode("FAKE_APT_INSTALL_EXIT"))

	case has("full-upgrade"):
		catFile(os.Getenv("FAKE_APT_FULLUPGRADE_OUTPUT_FILE"))
		os.Exit(envExitCode("FAKE_APT_FULLUPGRADE_EXIT"))

	default:
		fmt.Fprintln(os.Stderr, "fake helper: unrecognised invocation:", rest)
		os.Exit(127)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envExitCode(key string) int {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// fakeArgvLog reads back the FAKE_LOG_FILE a test pointed the helper
// process at: one JSON array of argv (name followed by real args) per
// invocation, in call order.
func fakeArgvLog(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read fake argv log: %v", err)
	}
	var out [][]string
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var argv []string
		if err := dec.Decode(&argv); err != nil {
			break
		}
		out = append(out, argv)
	}
	return out
}

// newFakeLogFile returns a fresh log-file path under t.TempDir() and wires
// FAKE_LOG_FILE to it for the duration of the test.
func newFakeLogFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-argv.log")
	t.Setenv("FAKE_LOG_FILE", path)
	return path
}
