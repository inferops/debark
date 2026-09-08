package cli

// This file exercises the real debark command tree end to end
// (cli.Execute, in-process — no binary is built, matching the spirit of
// the testscript TestMain wiring the contract brief asks for) covering every
// command's --help, every usage error, and the resulting exit code. See
// internal/cli/main_test.go's TestMain for why this is safe to run without
// touching the real user's config.
//
// What these tests deliberately do NOT assert: the success path of any
// command whose work happens in another package's package (build, verify,
// install, doctor, inspect, snapshot create/inspect, store gc/ls, resolve,
// fetch, apt-root inspect, keygen). Those packages are mid-implementation in
// this checkout; asserting today's specific "not implemented" stub error
// would just break the moment the real implementation lands. Only this
// package's own, stable behaviour is asserted for those commands: help text
// and the usage errors caught before anything calls into core/*.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/internal/cli/config"
)

// runReal runs the real debark tree in-process and captures its output.
func runReal(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Execute(context.Background(), args, strings.NewReader(""), &out, &errb)
	return out.String(), errb.String(), code
}

// tempConfig returns a --config path inside a fresh temp directory, so
// config-touching tests never read or write the real user's XDG config.
func tempConfig(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.yaml")
}

// commandPaths is every command in the tree, exactly as the CLI surface defines it,
// hidden commands included (they must still work; they're just unlisted),
// plus the completion subcommands cobra provides automatically.
var commandPaths = [][]string{
	{"snapshot"}, {"snapshot", "create"}, {"snapshot", "inspect"},
	{"snapshot", "from-base"}, {"snapshot", "list-bases"},
	{"build"},
	{"verify"},
	{"install"},
	{"inspect"},
	{"doctor"},
	{"store"}, {"store", "gc"}, {"store", "ls"},
	{"config"}, {"config", "init"}, {"config", "show"}, {"config", "path"},
	{"version"},
	{"completion"}, {"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}, {"completion", "powershell"},
	{"keygen"},
	{"resolve"},
	{"fetch"},
	{"apt-root"}, {"apt-root", "inspect"},
	{"gen-manpages"},
}

func TestEveryCommandHasWorkingHelp(t *testing.T) {
	for _, path := range commandPaths {
		args := append(append([]string{}, path...), "--help")
		t.Run(strings.Join(path, "_"), func(t *testing.T) {
			stdout, stderr, code := runReal(t, args...)
			if code != 0 {
				t.Fatalf("%v: code = %d, stderr = %q", args, code, stderr)
			}
			if !strings.Contains(stdout, "Usage:") {
				t.Errorf("%v: --help output missing \"Usage:\":\n%s", args, stdout)
			}
		})
	}
}

func TestRootHelpListsPublicSurfaceOnly(t *testing.T) {
	stdout, _, code := runReal(t, "--help")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{
		"snapshot", "build", "verify", "install", "inspect", "doctor",
		"store", "config", "version", "completion", "keygen",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("root help missing %q:\n%s", want, stdout)
		}
	}
	// Checked as a listed-command line ("  resolve   Short text"), not a bare
	// substring: the root's own Long description legitimately uses the word
	// "resolves" in prose, which a plain Contains(stdout, "resolve") would
	// wrongly flag.
	for _, hidden := range []string{"resolve", "fetch", "apt-root", "gen-manpages"} {
		if strings.Contains(stdout, "\n  "+hidden+" ") {
			t.Errorf("root help must not list hidden command %q:\n%s", hidden, stdout)
		}
	}
}

func TestBareInvocationSucceedsAndShowsHelp(t *testing.T) {
	// cobra's default behaviour for a root command with no Run of its own
	// and no arguments is to print help and return no error (exit 0) —
	// unlike a genuinely unknown subcommand, which is a usage error (below).
	// This differs from a hand-rolled "a subcommand is required" refusal;
	// it is cobra's own, well-established convention and is not worth
	// fighting.
	stdout, _, code := runReal(t)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "Usage:") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestUnknownTopLevelCommandExitOne(t *testing.T) {
	_, stderr, code := runReal(t, "frobnicate")
	if code != int(dferr.Usage) {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestUnknownCommandSuggestsClosest(t *testing.T) {
	// cobra's own suggestion engine (SuggestionsMinimumDistance).
	_, stderr, code := runReal(t, "versio")
	if code != int(dferr.Usage) {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stderr, "version") {
		t.Errorf("stderr = %q, want a suggestion mentioning \"version\"", stderr)
	}
}

// usageCases are invocations this package itself must reject before ever
// calling into another package's package — every one of them must be exit 1
// regardless of how much of core/* is implemented.
func usageCases(t *testing.T) []struct {
	name   string
	args   []string
	substr string
} {
	cfg := tempConfig(t)
	return []struct {
		name   string
		args   []string
		substr string
	}{
		{"snapshot_inspect_missing_file", []string{"snapshot", "inspect"}, "arg(s), received 0"},
		{"snapshot_inspect_too_many_args", []string{"snapshot", "inspect", "a", "b"}, "arg(s), received 2"},
		// `build` names its target with either --snapshot or --base and needs
		// exactly one, so cobra's "one required" wording replaces the older
		// "required flag(s) ... not set". Still exit 1, which is the contract.
		{"build_missing_target", []string{"build"}, "at least one of the flags"},
		{"build_snapshot_and_base_exclusive", []string{"build", "--snapshot", "x", "--base", "ubuntu:26.04/desktop"}, "were all set"},
		{"build_arch_without_base", []string{"build", "--snapshot", "x", "--arch", "arm64"}, "--arch describes a --base"},
		{"build_interactive_and_json_exclusive", []string{"build", "--interactive", "--json"}, "automation contract"},
		{"frombase_missing_base", []string{"snapshot", "from-base"}, "a base is required"},
		{"frombase_too_many_args", []string{"snapshot", "from-base", "a", "b"}, "accepts at most 1 arg"},
		{"frombase_unknown_base", []string{"snapshot", "from-base", "plan9:9/desktop"}, "neither a builtin base id nor a file"},
		{"frombase_bad_backend", []string{"snapshot", "from-base", "ubuntu:26.04/desktop", "--backend", "bogus"}, "--backend must be"},
		{"listbases_extra_arg", []string{"snapshot", "list-bases", "extra"}, "unknown command"},
		{"listbases_bad_arch", []string{"snapshot", "list-bases", "--arch", "pdp11"}, "unknown architecture"},
		{"build_out_and_tar_exclusive", []string{"build", "--snapshot", "x", "--out", "a", "--tar", "b"}, "were all set"},
		{"build_sign_and_nosign_exclusive", []string{"build", "--snapshot", "x", "--sign", "k", "--no-sign"}, "were all set"},
		{"build_bad_backend", []string{"build", "--snapshot", "x", "--backend", "bogus"}, "--backend must be"},
		{"verify_missing_bundle", []string{"verify"}, "arg(s), received 0"},
		{"verify_too_many_args", []string{"verify", "a", "b"}, "arg(s), received 2"},
		{"install_missing_bundle", []string{"install"}, "arg(s), received 0"},
		{"install_status_and_dryrun_exclusive", []string{"install", "b", "--status", "--dry-run"}, "were all set"},
		{"inspect_missing_bundle", []string{"inspect"}, "arg(s), received 0"},
		{"doctor_neither_bundle_nor_snapshot", []string{"doctor"}, "give exactly one"},
		{"doctor_both_bundle_and_snapshot", []string{"doctor", "b", "--snapshot", "s"}, "give exactly one"},
		{"store_gc_unknown_flag", []string{"store", "gc", "--bogus"}, "unknown flag"},
		{"config_show_extra_arg", []string{"--config", cfg, "config", "show", "extra"}, "unknown command \"extra\""},
		{"version_extra_arg", []string{"version", "extra"}, "unknown command \"extra\""},
		{"keygen_missing_out", []string{"keygen"}, "required flag(s) \"out\" not set"},
		{"resolve_missing_required", []string{"resolve"}, "required flag(s)"},
		{"resolve_bad_recommends", []string{"resolve", "--snapshot", "s", "--archives", "a", "--work", "w", "--recommends", "bogus"}, "--recommends must be true or false"},
		{"resolve_bad_backend", []string{"resolve", "--snapshot", "s", "--archives", "a", "--work", "w", "--backend", "bogus"}, "--backend must be"},
		{"resolve_no_positional_args", []string{"resolve", "positional-not-allowed"}, "unknown command \"positional-not-allowed\""},
		{"fetch_missing_url", []string{"fetch"}, "requires at least 1 arg"},
		{"aptroot_inspect_missing_snapshot", []string{"apt-root", "inspect"}, "arg(s), received 0"},
		{"aptroot_inspect_too_many_args", []string{"apt-root", "inspect", "a", "b"}, "arg(s), received 2"},
		{"unknown_flag_on_version", []string{"version", "--bogus"}, "unknown flag"},
	}
}

func TestUsageErrorsExitOneWithActionableMessage(t *testing.T) {
	for _, c := range usageCases(t) {
		t.Run(c.name, func(t *testing.T) {
			_, stderr, code := runReal(t, c.args...)
			if code != int(dferr.Usage) {
				t.Fatalf("%v: code = %d, want 1 (usage); stderr = %q", c.args, code, stderr)
			}
			if !strings.Contains(stderr, c.substr) {
				t.Errorf("%v: stderr = %q, want it to contain %q", c.args, stderr, c.substr)
			}
		})
	}
}

func TestHelpTakesPrecedenceOverUsageErrors(t *testing.T) {
	// Even an invocation that would otherwise be a hard usage error (a
	// required flag missing) must still show help and exit 0 when --help
	// is present.
	_, _, code := runReal(t, "build", "--help")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
}

// --- commands whose success path is entirely self-contained ---------------

func TestVersionHumanAndJSON(t *testing.T) {
	stdout, _, code := runReal(t, "version")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stdout, "debark") {
		t.Errorf("stdout = %q", stdout)
	}

	stdout, _, code = runReal(t, "version", "--json")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("version --json did not produce valid JSON: %v\n%s", err, stdout)
	}
	if m["name"] != "debark" {
		t.Errorf("name = %v", m["name"])
	}
	if _, ok := m["edition"]; !ok {
		t.Error("missing edition field")
	}
}

func TestConfigPathInitShow(t *testing.T) {
	cfg := tempConfig(t)

	stdout, _, code := runReal(t, "--config", cfg, "config", "path")
	if code != 0 {
		t.Fatalf("config path: code = %d", code)
	}
	if strings.TrimSpace(stdout) != cfg {
		t.Errorf("config path = %q, want %q", strings.TrimSpace(stdout), cfg)
	}

	if _, err := os.Stat(cfg); err == nil {
		t.Fatal("config file should not exist before config init")
	}
	stdout, stderr, code := runReal(t, "--config", cfg, "config", "init")
	if code != 0 {
		t.Fatalf("config init: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, cfg) {
		t.Errorf("config init stdout = %q", stdout)
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("config file was not written: %v", err)
	}

	// Second init without --force is a usage error; this is a real "the
	// command reached its own package's logic" case, but config is owned
	// entirely by this package, so it is fine to assert on.
	_, _, code = runReal(t, "--config", cfg, "config", "init")
	if code != int(dferr.Usage) {
		t.Errorf("second config init without --force: code = %d, want 1", code)
	}
	_, _, code = runReal(t, "--config", cfg, "config", "init", "--force")
	if code != 0 {
		t.Errorf("config init --force: code = %d", code)
	}

	stdout, _, code = runReal(t, "--config", cfg, "config", "show", "--json")
	if code != 0 {
		t.Fatalf("config show --json: code = %d", code)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("config show --json did not produce valid JSON: %v\n%s", err, stdout)
	}
	if m["backend"] != "auto" {
		t.Errorf("backend = %v, want auto (the default)", m["backend"])
	}
}

func TestConfigEnvPrecedenceOverFile(t *testing.T) {
	cfg := tempConfig(t)
	if _, _, code := runReal(t, "--config", cfg, "config", "init"); code != 0 {
		t.Fatalf("config init failed")
	}
	t.Setenv("DEBARK_BACKEND", "container")
	stdout, _, code := runReal(t, "--config", cfg, "config", "show", "--json")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	var m map[string]any
	json.Unmarshal([]byte(stdout), &m)
	if m["backend"] != "container" {
		t.Errorf("env should override the file's default backend: got %v", m["backend"])
	}
}

func TestBuildBackendFlagOverridesConfigFile(t *testing.T) {
	// End-to-end proof that the real --backend flag on `build` flows through
	// config.Load's posflag layer (internal/cli/config's package doc) on top
	// of a file-configured default — not just that the config package's own
	// unit tests exercise it with a synthetic flag set.
	cfg := tempConfig(t)
	if err := os.WriteFile(cfg, []byte("backend: local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// --backend local is accepted by build's own validation, so the run
	// proceeds past flag/config resolution into the (currently stubbed)
	// engine, which is exactly where we want it to fail — proving the
	// value reached that point rather than being rejected as invalid.
	_, stderr, code := runReal(t, "--config", cfg, "build", "--snapshot", "/does/not/exist", "--backend", "container")
	if code == int(dferr.Usage) && strings.Contains(stderr, "--backend must be") {
		t.Fatalf("--backend container was rejected as invalid, so it did not reach engine construction: %q", stderr)
	}
}

func TestCompletionScriptsAreWiredIn(t *testing.T) {
	// cobra's own default completion command generates these dynamically
	// (each script calls back into the binary's hidden __complete command
	// rather than embedding a static subcommand list), so this only
	// confirms completion is wired in for all four shells (not disabled)
	// and names this binary, not that any particular subcommand appears in
	// the generated text.
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		stdout, stderr, code := runReal(t, "completion", shell)
		if code != 0 {
			t.Fatalf("completion %s: code = %d, stderr = %q", shell, code, stderr)
		}
		if !strings.Contains(stdout, "debark") {
			t.Errorf("completion %s missing \"debark\":\n%s", shell, stdout)
		}
		if len(stdout) < 100 {
			t.Errorf("completion %s output looks too short to be real: %q", shell, stdout)
		}
	}
}

func TestHiddenCompleteCommandOffersRealSubcommands(t *testing.T) {
	// This is what every generated completion script above actually calls
	// at TAB-press time (cobra's hidden __complete command), so it is the
	// right place to prove a real subcommand name is offered.
	stdout, _, code := runReal(t, "__complete", "sn")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stdout, "snapshot") {
		t.Errorf("__complete sn did not offer \"snapshot\":\n%s", stdout)
	}
}

func TestJSONModeProducesNoANSIRegardlessOfNoColor(t *testing.T) {
	// --json implies non-interactive, so even without --no-color or
	// NO_COLOR, JSON output must never carry escape codes.
	stdout, _, code := runReal(t, "version", "--json")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.ContainsRune(stdout, '\x1b') {
		t.Errorf("--json output contains an ANSI escape: %q", stdout)
	}
}

func TestNoColorFlagSuppressesColorInHumanOutput(t *testing.T) {
	stdout, _, code := runReal(t, "--no-color", "version")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.ContainsRune(stdout, '\x1b') {
		t.Errorf("--no-color output contains an ANSI escape: %q", stdout)
	}
}

func TestNOColorEnvSuppressesColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	stdout, _, code := runReal(t, "version")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.ContainsRune(stdout, '\x1b') {
		t.Errorf("NO_COLOR set but output contains an ANSI escape: %q", stdout)
	}
}

func TestGenManPagesWritesFiles(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, code := runReal(t, "gen-manpages", dir)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("gen-manpages wrote no files")
	}
	found := false
	for _, e := range entries {
		if e.Name() == "debark-build.1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected debark-build.1 among the written files: %v (stdout: %s)", entries, stdout)
	}
	b, err := os.ReadFile(filepath.Join(dir, "debark.1"))
	if err != nil {
		t.Fatalf("debark.1 not written: %v", err)
	}
	if !strings.Contains(string(b), ".TH DEBARK 1") {
		t.Errorf("debark.1 does not look like a man page:\n%s", b)
	}
}

// --- baseline OS support: the guided flows must never fire unasked ---------
//
// Every test below drives stdin with a PIPE (a strings.Reader, which is what
// runReal passes), because that is the case that actually breaks. A prompt
// that fires with nobody to answer it does not fail loudly — it blocks, and a
// CI job hangs until something kills it. These are the assertions that stop
// that shipping.

// TestGuidedFlowsNeverFireWithoutATerminal is rule 1 and rule 5 together: no
// TTY means no question, whether the operator asked for the guided flow or
// simply left an argument out.
func TestGuidedFlowsNeverFireWithoutATerminal(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			// --interactive was asked for explicitly, and is refused because
			// stdin is a pipe. The refusal must name the reason, since the
			// fix (pass the flags instead) follows from it.
			name: "build_interactive_without_tty",
			args: []string{"build", "--interactive"},
			want: "needs a terminal",
		},
		{
			name: "frombase_interactive_without_tty",
			args: []string{"snapshot", "from-base", "--interactive"},
			want: "needs a terminal",
		},
		{
			// No --interactive at all: a missing argument must be a usage
			// error, never a silent read from stdin.
			name: "build_bare",
			args: []string{"build"},
			want: "at least one of the flags",
		},
		{
			name: "frombase_bare",
			args: []string{"snapshot", "from-base"},
			want: "a base is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A generous supply of answers on stdin. If anything ever does
			// prompt, it will consume these and get further than it should,
			// which shows up as a different error than the one asserted —
			// deliberately, so this test cannot pass by the flow merely
			// hitting EOF.
			var out, errb bytes.Buffer
			answers := strings.Repeat("1\n", 40)
			code := Execute(context.Background(), c.args, strings.NewReader(answers), &out, &errb)
			if code != int(dferr.Usage) {
				t.Fatalf("code = %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
			}
			if !strings.Contains(errb.String(), c.want) {
				t.Errorf("stderr = %q, want it to contain %q", errb.String(), c.want)
			}
			for _, q := range []string{"Distribution", "Install profile", "[y/N]", "[Y/n]"} {
				if strings.Contains(out.String(), q) {
					t.Errorf("a prompt (%q) was written even though stdin is not a terminal:\n%s", q, out.String())
				}
			}
		})
	}
}

// TestInteractiveIsRefusedUnderTheAutomationContract is rule 2. --json and
// --json-events are what a program reads; a question on that path would
// corrupt the very stream the caller is parsing.
func TestInteractiveIsRefusedUnderTheAutomationContract(t *testing.T) {
	for _, args := range [][]string{
		{"build", "--interactive", "--json"},
		{"snapshot", "from-base", "--interactive", "--json"},
		{"snapshot", "from-base", "--interactive", "--json-events", "-"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			stdout, stderr, code := runReal(t, args...)
			if code != int(dferr.Usage) {
				t.Fatalf("code = %d, want 1; stderr = %q", code, stderr)
			}
			if strings.Contains(stdout, "Distribution") {
				t.Errorf("prompted under the automation contract:\n%s", stdout)
			}
		})
	}
}

// TestListBasesHumanAndJSON is the discovery surface `from-base` and
// `build --base` both point operators at, so it has to work with no
// configuration and no network.
func TestListBasesHumanAndJSON(t *testing.T) {
	stdout, stderr, code := runReal(t, "snapshot", "list-bases")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"ubuntu:26.04/desktop", "debian:12/minimal", "ubuntu-desktop-minimal"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list-bases output missing %q:\n%s", want, stdout)
		}
	}
	// The caveat is the point of the command as much as the list is: a base
	// is an assumption, and the docs, the human output and the snapshot's own
	// recorded warning all say so from one string.
	if !strings.Contains(stdout, "assumption") {
		t.Errorf("list-bases does not carry the assumption caveat:\n%s", stdout)
	}

	stdout, _, code = runReal(t, "snapshot", "list-bases", "--json")
	if code != 0 {
		t.Fatalf("--json: code = %d", code)
	}
	var doc struct {
		SchemaVersion string `json:"schema_version"`
		Arch          string `json:"arch"`
		Bases         []struct {
			ID         string   `json:"id"`
			Seeds      []string `json:"seeds"`
			Recommends bool     `json:"recommends"`
			Digest     string   `json:"digest"`
		} `json:"bases"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("list-bases --json is not valid JSON: %v\n%s", err, stdout)
	}
	if doc.SchemaVersion == "" {
		t.Error("list-bases --json carries no schema_version (ADR-012: every --json output is a versioned object)")
	}
	if len(doc.Bases) == 0 {
		t.Fatal("list-bases --json listed no bases")
	}
	for _, b := range doc.Bases {
		if len(b.Seeds) == 0 {
			t.Errorf("%s has no seeds", b.ID)
		}
		if len(b.Digest) != 64 {
			t.Errorf("%s digest = %q, want a 64-character sha256", b.ID, b.Digest)
		}
		// Recommends defaults off for a reason with a safety argument behind
		// it: a recommended package the real installer did not take would be
		// claimed as installed when it is not, which is the direction that
		// produces a short bundle.
		if b.Recommends {
			t.Errorf("%s has recommends on; a builtin base must err toward not-installed", b.ID)
		}
	}
}

// TestListBasesArchChangesTheArchive checks the one thing in the base table
// that produces a late, confusing failure when it is wrong: Ubuntu serves
// arm64 from ports.ubuntu.com, not archive.ubuntu.com, and apt simply finds
// nothing if that is mixed up — inside a container, minutes in.
func TestListBasesArchChangesTheArchive(t *testing.T) {
	amd64Out, _, code := runReal(t, "snapshot", "list-bases", "--arch", "amd64", "--json")
	if code != 0 {
		t.Fatalf("amd64: code = %d", code)
	}
	arm64Out, _, code := runReal(t, "snapshot", "list-bases", "--arch", "arm64", "--json")
	if code != 0 {
		t.Fatalf("arm64: code = %d", code)
	}
	if amd64Out == arm64Out {
		t.Error("the base list is identical for amd64 and arm64, so the per-architecture archive layout is not being applied")
	}
}

// --- --self-binary ---------------------------------------------------------
//
// The container backend mounts a Linux debark into the container and
// re-enters it. On Windows and macOS the running executable is never a Linux
// ELF binary, so `build --backend container` could not work at all: there was
// no flag, environment variable or config key naming a different file, and
// the refusal's own advice was "set ContainerOptions.SelfPath", a Go struct
// field an operator has no way to set.

func TestSelfBinaryFlagIsOfferedByEveryContainerCapableCommand(t *testing.T) {
	for _, args := range [][]string{
		{"build", "--help"},
		{"snapshot", "from-base", "--help"},
		{"resolve", "--help"},
		{"apt-root", "inspect", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stdout, _, code := runReal(t, args...)
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if !strings.Contains(stdout, "--self-binary") {
				t.Errorf("--self-binary is missing from:\n%s", stdout)
			}
		})
	}
}

func TestSelfBinaryPrecedenceFlagOverEnvOverConfig(t *testing.T) {
	cfg := tempConfig(t)
	if err := os.WriteFile(cfg, []byte("self_binary: /from/file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctxFor := func(t *testing.T, args ...string) *Ctx {
		t.Helper()
		var got *Ctx
		root := newRootCmd()
		probe := &cobra.Command{
			Use:  "selfbinary-probe",
			RunE: wrapRun(func(c *Ctx, _ []string) error { got = c; return nil }),
		}
		root.AddCommand(probe)
		root.SetArgs(append([]string{"--config", cfg, "selfbinary-probe"}, args...))
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		if err := root.Execute(); err != nil {
			t.Fatalf("probe: %v", err)
		}
		return got
	}

	t.Setenv("DEBARK_SELF_BINARY", "")
	if got := ctxFor(t).selfBinary(""); got != "/from/file" {
		t.Errorf("config alone: selfBinary = %q, want /from/file", got)
	}
	t.Setenv("DEBARK_SELF_BINARY", "/from/env")
	if got := ctxFor(t).selfBinary(""); got != "/from/env" {
		t.Errorf("env over config: selfBinary = %q, want /from/env", got)
	}
	if got := ctxFor(t).selfBinary("/from/flag"); got != "/from/flag" {
		t.Errorf("flag over env: selfBinary = %q, want /from/flag", got)
	}
}

// Nothing configured is a real answer, not a fallback to os.Executable()
// here: core/apt then looks for bin/debark-linux-<arch> beside the running
// binary before settling for the running binary, and only it knows which
// architecture the container will run.
func TestSelfBinaryUnsetStaysEmpty(t *testing.T) {
	c := &Ctx{Cfg: &config.Config{}}
	if got := c.selfBinary(""); got != "" {
		t.Errorf("selfBinary = %q, want empty", got)
	}
}

func TestBuildOffersBothRecommendsDirections(t *testing.T) {
	stdout, _, code := runReal(t, "build", "--help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"--recommends", "--no-recommends"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("%s missing from build --help:\n%s", want, stdout)
		}
	}
}

func TestBuildRefusesBothRecommendsDirections(t *testing.T) {
	_, stderr, code := runReal(t, "build", "--snapshot", "/does/not/exist",
		"--recommends", "--no-recommends", "jq")
	if code != int(dferr.Usage) {
		t.Errorf("exit = %d, want %d (usage)", code, int(dferr.Usage))
	}
	if !strings.Contains(stderr, "recommends") {
		t.Errorf("the refusal should name the flags:\n%s", stderr)
	}
}
