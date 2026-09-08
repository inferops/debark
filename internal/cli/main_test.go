package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// TestMain installs debark as a testscript command (rogpeppe/go-internal,
// per the contract brief) so internal/cli/testdata/script/*.txtar scripts run
// `exec debark ...` against this package's own Execute, in-process — no
// binary is built or shelled out to; testscript.Main re-execs this same
// test binary under the name "debark" for the "exec debark" case and
// otherwise just runs the ordinary Go tests in this package (cli_surface_test.go's
// exact, per-code assertions; golden_test.go's renderer tests) via m.Run().
//
// It also isolates every test in this package from the real user's XDG
// config: DEBARK_CONFIG is pointed at a path that is guaranteed not to
// exist before any test runs, so a test that does not explicitly pass
// --config never reads, and can never be affected by, a real
// ~/.config/debark/config.yaml or %APPDATA%\debark\config.yaml on the
// machine running the suite. testscript.Run gives each script its own
// isolated environment regardless (Setup below also unsets DEBARK_CONFIG
// there, since scripts get a fresh env, not this process's).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "debark-cli-tests-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	os.Setenv("DEBARK_CONFIG", filepath.Join(dir, "unused-default-config.yaml"))
	os.Setenv("DEBARK_PROFILE", "")
	os.Setenv("NO_COLOR", "")
	os.Setenv("TERM", "")

	testscript.Main(m, map[string]func(){
		"debark": func() {
			os.Exit(Execute(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
		},
	})
}

// TestScripts runs every internal/cli/testdata/script/*.txtar scenario: the
// hidden debark command installed by TestMain above, invoked with `exec
// debark ...` (or via the debarkcode custom command below, for the
// scenarios that assert a specific exit code — see its own comment for why
// that is a custom command rather than plain `exec`).
func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata/script",
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"debarkcode": debarkCodeCmd,
		},
		Setup: func(env *testscript.Env) error {
			// A fresh, guaranteed-empty config path per script, the
			// testscript-idiomatic equivalent of this file's own
			// DEBARK_CONFIG isolation above (scripts do not inherit this
			// process's environment).
			env.Setenv("DEBARK_CONFIG", filepath.Join(env.WorkDir, "unused-default-config.yaml"))
			return nil
		},
	})
}

// debarkCodeCmd implements the `debarkcode CODE ARGS...` script command:
// runs debark with ARGS, capturing stdout/stderr the same way `exec`
// would (so later `stdout`/`stderr` pattern lines in the script still work),
// and fails the script unless the resulting exit code equals CODE (or, with
// a leading `!`, does not equal it). testscript's own `exec`/`!  exec` only
// distinguish success from failure, not which specific non-zero code came
// back, and this suite's whole point (per the contract brief: "a missing
// required flag is exit 1; a missing bundle is exit 1; and so on") is
// checking the exact documented code, so a small custom command earns its
// place here instead of layering something after the fact.
func debarkCodeCmd(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) < 1 {
		ts.Fatalf("usage: debarkcode CODE [debark-args...]")
	}
	want, err := strconv.Atoi(args[0])
	if err != nil {
		ts.Fatalf("debarkcode: %q is not a numeric exit code", args[0])
	}
	got := Execute(context.Background(), args[1:], strings.NewReader(""), ts.Stdout(), ts.Stderr())
	if (got == want) == neg {
		if neg {
			ts.Fatalf("debark exited %d, want anything other than %d", got, want)
		} else {
			ts.Fatalf("debark exited %d, want %d", got, want)
		}
	}
}
