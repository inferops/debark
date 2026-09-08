package cliadapter

// Tests for the implementing package. Everything here is prefixed invoke/proc, and every test
// function is TestInvoke..., because three packages write tests into this one
// package and a redeclaration breaks the build for all of them.
//
// The parsing is driven by REAL captured output. testdata/json/ and
// testdata/help/ were produced by running a binary built from debark
// 94612f5 — the same commit docs/dev/cli-surface.md was verified against —
// and nothing in them was written by hand except keygen.json, whose paths were
// replaced with plausible Linux ones (the command's shape is three strings and
// its real output names this machine's temp directory).
//
//	json/version.json            debark version --json
//	json/list-bases-amd64.json   debark snapshot list-bases --json
//	json/list-bases-arm64.json   debark snapshot list-bases --arch arm64 --json
//	json/snapshot-inspect.json   debark snapshot inspect <a real .snapshot.tar.zst> --json
//	json/store-ls.json           debark store ls --json  (a valid document of the WRONG schema)
//	json/verify-ok.json          debark verify <bundle> --json --key <pub>   (exit 0)
//	json/verify-untrusted.json   debark verify <bundle> --json              (exit 4, empty stderr)
//	json/keygen.json             debark keygen --out ... --json
//	help/root.txt                debark --help
//	help/build.txt               debark build --help
//	help/snapshot-group.txt      debark snapshot bogus   (exit 0, group help on stdout)

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/base"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/verify"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// invokeReply is one scripted process result.
type invokeReply struct {
	stdout []byte
	stderr []byte
	exit   int
	err    error
	// block makes Run wait for the context instead of returning, so a test can
	// cancel mid-run.
	block bool
}

// invokeStub is a procRunner driven by a function of the arguments.
type invokeStub struct {
	mu    sync.Mutex
	calls [][]string
	bins  []string
	reply func(args []string) invokeReply
}

func (s *invokeStub) Run(ctx context.Context, bin string, args []string) ([]byte, []byte, int, error) {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string(nil), args...))
	s.bins = append(s.bins, bin)
	s.mu.Unlock()

	r := invokeReply{exit: procExitNotStarted, err: errors.New("no scripted reply")}
	if s.reply != nil {
		r = s.reply(args)
	}
	if r.block {
		<-ctx.Done()
		return nil, nil, procExitNotStarted, ctx.Err()
	}
	return r.stdout, r.stderr, r.exit, r.err
}

func (s *invokeStub) recorded() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// invokeFile reads one testdata fixture.
func invokeFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// invokeAdapter builds a real adapter with a scripted runner and a binary path
// that is never touched.
func invokeAdapter(t *testing.T, reply func(args []string) invokeReply) (*adapter, *invokeStub) {
	t.Helper()
	stub := &invokeStub{reply: reply}
	a, err := New(Options{BinaryPath: filepath.Join(t.TempDir(), "debark"), runner: stub})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a.(*adapter), stub
}

// invokeHas reports whether args contains an exact element.
func invokeHas(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// invokeIsCmd matches a scripted call by its leading subcommand words.
func invokeIsCmd(args []string, words ...string) bool {
	if len(args) < len(words) {
		return false
	}
	for i, w := range words {
		if args[i] != w {
			return false
		}
	}
	return true
}

// invokeFullBinary is the reply script a healthy, current debark produces.
func invokeFullBinary(t *testing.T) func(args []string) invokeReply {
	t.Helper()
	version := invokeFile(t, "json/version.json")
	rootHelp := invokeFile(t, "help/root.txt")
	buildHelp := invokeFile(t, "help/build.txt")
	bases := invokeFile(t, "json/list-bases-amd64.json")
	basesARM := invokeFile(t, "json/list-bases-arm64.json")
	snap := invokeFile(t, "json/snapshot-inspect.json")
	key := invokeFile(t, "json/keygen.json")
	verifyOK := invokeFile(t, "json/verify-ok.json")

	return func(args []string) invokeReply {
		switch {
		case invokeIsCmd(args, "version"):
			return invokeReply{stdout: version}
		case invokeIsCmd(args, "--help"):
			return invokeReply{stdout: rootHelp}
		case invokeIsCmd(args, "build", "--help"):
			return invokeReply{stdout: buildHelp}
		case invokeIsCmd(args, "snapshot", "list-bases"):
			if invokeHas(args, "arm64") {
				return invokeReply{stdout: basesARM}
			}
			return invokeReply{stdout: bases}
		case invokeIsCmd(args, "snapshot", "inspect"):
			return invokeReply{stdout: snap}
		case invokeIsCmd(args, "keygen"):
			return invokeReply{stdout: key}
		case invokeIsCmd(args, "verify"):
			return invokeReply{stdout: verifyOK}
		}
		return invokeReply{exit: int(dferr.Usage), stderr: []byte("debark: unknown command\n")}
	}
}

// ---------------------------------------------------------------------------
// Probe
// ---------------------------------------------------------------------------

func TestInvokeProbeReadsCapturedOutput(t *testing.T) {
	a, stub := invokeAdapter(t, invokeFullBinary(t))

	p, err := a.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if p.Version() != "dev" {
		t.Errorf("Version() = %q, want dev", p.Version())
	}
	if p.Edition() != "community" {
		t.Errorf("Edition() = %q, want community", p.Edition())
	}
	if p.Platform() != "windows/amd64" {
		t.Errorf("Platform() = %q, want windows/amd64", p.Platform())
	}
	if p.Info.Commit != "94612f558cd26fa57d6c0185956ff153c5162833" {
		t.Errorf("Commit = %q", p.Info.Commit)
	}
	// HostArch comes from the base listing's own Arch, never runtime.GOARCH.
	if p.HostArch != "amd64" {
		t.Errorf("HostArch = %q, want amd64 (from List.Arch)", p.HostArch)
	}
	if p.Path == "" || !filepath.IsAbs(p.Path) {
		t.Errorf("Path = %q, want an absolute path", p.Path)
	}
	want := Capabilities{Bases: true, JSONEvents: true, Keygen: true, SBOM: true}
	if p.Capabilities != want {
		t.Errorf("Capabilities = %+v, want %+v", p.Capabilities, want)
	}

	// The base listing must not have been asked for an architecture: an empty
	// arch means "whatever debark would default to", and that answer is what
	// HostArch is.
	for _, call := range stub.recorded() {
		if invokeIsCmd(call, "snapshot", "list-bases") && invokeHas(call, "--arch") {
			t.Errorf("Probe passed --arch to list-bases: %v", call)
		}
	}
}

// TestInvokeProbeDegradesOnAStaleBinary is the headline case for capability
// detection.
//
// A debark that predates `snapshot list-bases` does not report an error: cobra
// finds no matching subcommand, runs the parent, which has no RunE, and prints
// the GROUP HELP TO STDOUT with EXIT 0. help/snapshot-group.txt is that exact
// output. An adapter that treated exit 0 as "the command exists" would then hand
// the base picker a nil list; one that treated the JSON decode failure as fatal
// would refuse to probe at all. Neither is right: the answer is Bases false, an
// otherwise complete Probe, and a target screen that offers only snapshot files.
func TestInvokeProbeDegradesOnAStaleBinary(t *testing.T) {
	full := invokeFullBinary(t)
	groupHelp := invokeFile(t, "help/snapshot-group.txt")

	a, _ := invokeAdapter(t, func(args []string) invokeReply {
		if invokeIsCmd(args, "snapshot", "list-bases") {
			return invokeReply{stdout: groupHelp, exit: int(dferr.Success)}
		}
		return full(args)
	})

	p, err := a.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe must succeed against an older binary, got: %v", err)
	}
	if p.Capabilities.Bases {
		t.Error("Bases = true for a binary that answered list-bases with help text")
	}
	if p.HostArch != "" {
		t.Errorf("HostArch = %q, want empty when the base listing is unavailable", p.HostArch)
	}
	if !p.Capabilities.Keygen || !p.Capabilities.JSONEvents || !p.Capabilities.SBOM {
		t.Errorf("unrelated capabilities were lost: %+v", p.Capabilities)
	}
	if p.Version() != "dev" {
		t.Errorf("Version() = %q, want the probe to still report a version", p.Version())
	}
}

func TestInvokeProbeCapabilitiesSurviveAFailedHelp(t *testing.T) {
	full := invokeFullBinary(t)
	a, _ := invokeAdapter(t, func(args []string) invokeReply {
		if invokeIsCmd(args, "build", "--help") {
			return invokeReply{exit: int(dferr.Usage), stderr: []byte("debark: unknown command \"build\"\n")}
		}
		return full(args)
	})

	p, err := a.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if p.Capabilities.SBOM {
		t.Error("SBOM = true although `build --help` failed")
	}
	if !p.Capabilities.Bases || !p.Capabilities.Keygen {
		t.Errorf("one failed help probe took others with it: %+v", p.Capabilities)
	}
}

func TestInvokeProbeRejectsAForeignBinary(t *testing.T) {
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: []byte(`{"name":"apt","version":"2.8.3"}`)}
	})

	_, err := a.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe accepted a binary that is not debark")
	}
	e := invokeAsError(t, err)
	if !strings.Contains(e.Summary(), "does not identify itself") {
		t.Errorf("Summary() = %q", e.Summary())
	}
	if e.Hint() == "" {
		t.Error("Hint() is empty; the first screen has nothing to tell the operator")
	}
}

func TestInvokeProbeFailsWhenVersionFails(t *testing.T) {
	stderr := invokeFile(t, "stderr/usage-unknown-flag.stderr")
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{exit: int(dferr.Usage), stderr: stderr}
	})

	_, err := a.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe succeeded although `version --json` failed")
	}
	e := invokeAsError(t, err)
	if e.ExitCode() != int(dferr.Usage) {
		t.Errorf("ExitCode() = %d, want 1", e.ExitCode())
	}
}

// ---------------------------------------------------------------------------
// ListBases
// ---------------------------------------------------------------------------

func TestInvokeListBases(t *testing.T) {
	tests := []struct {
		name     string
		arch     string
		fixture  string
		wantArch string
		wantFlag bool
	}{
		{"default architecture", "", "json/list-bases-amd64.json", "amd64", false},
		{"explicit architecture", "arm64", "json/list-bases-arm64.json", "arm64", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := invokeFile(t, tc.fixture)
			a, stub := invokeAdapter(t, func([]string) invokeReply {
				return invokeReply{stdout: doc}
			})

			list, err := a.ListBases(context.Background(), tc.arch)
			if err != nil {
				t.Fatalf("ListBases: %v", err)
			}
			if list.SchemaVersion != base.ListSchemaVersion {
				t.Errorf("SchemaVersion = %q", list.SchemaVersion)
			}
			if list.Arch != tc.wantArch {
				t.Errorf("Arch = %q, want %q", list.Arch, tc.wantArch)
			}
			// The builtin table at the verified commit is fifteen rows.
			if len(list.Bases) != 15 {
				t.Errorf("len(Bases) = %d, want 15", len(list.Bases))
			}
			if list.Bases[0].ID != "debian:12/minimal" {
				t.Errorf("Bases[0].ID = %q", list.Bases[0].ID)
			}
			for _, b := range list.Bases {
				if b.Arch != tc.wantArch {
					t.Errorf("base %s rendered for %q, want %q", b.ID, b.Arch, tc.wantArch)
				}
			}

			call := stub.recorded()[0]
			if got := invokeHas(call, "--arch"); got != tc.wantFlag {
				t.Errorf("--arch present = %v, want %v (argv %v)", got, tc.wantFlag, call)
			}
		})
	}
}

func TestInvokeListBasesRejectsAnUnknownSchema(t *testing.T) {
	// A real, valid debark document — of the wrong kind. It decodes into
	// base.List without complaint, which is exactly why schema_version has to
	// be checked rather than trusted.
	doc := invokeFile(t, "json/store-ls.json")
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: doc}
	})

	_, err := a.ListBases(context.Background(), "")
	if err == nil {
		t.Fatal("ListBases accepted a debark.storeindex/v1 document")
	}
	e := invokeAsError(t, err)
	for _, want := range []string{"debark.storeindex/v1", base.ListSchemaVersion} {
		if !strings.Contains(e.Summary(), want) {
			t.Errorf("Summary() = %q, want it to name %q", e.Summary(), want)
		}
	}
	if e.Hint() == "" {
		t.Error("Hint() is empty")
	}
}

func TestInvokeUnreadableOutput(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{"nothing at all", "", "printed nothing"},
		{"truncated", `{"schema_version":"debark.baselist/v1","bases":[{"id":`, "could not read as JSON"},
		{"prose", "debark moves Debian/Ubuntu software across an air gap.\n", "could not read as JSON"},
		{"a bare number", "42", "could not read as JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := invokeAdapter(t, func([]string) invokeReply {
				return invokeReply{stdout: []byte(tc.stdout)}
			})

			_, err := a.ListBases(context.Background(), "")
			if err == nil {
				t.Fatalf("ListBases accepted %q", tc.stdout)
			}
			e := invokeAsError(t, err)
			if !strings.Contains(e.Summary(), tc.want) {
				t.Errorf("Summary() = %q, want it to contain %q", e.Summary(), tc.want)
			}
			// No error path may surface a raw exit code alone.
			if strings.TrimSpace(e.Summary()) == "" || e.Error() == "" {
				t.Error("an unreadable-output error has no sentence in it")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// InspectSnapshot
// ---------------------------------------------------------------------------

func TestInvokeInspectSnapshot(t *testing.T) {
	doc := invokeFile(t, "json/snapshot-inspect.json")
	a, stub := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: doc}
	})

	const path = "/media/usb/target.snapshot.tar.zst"
	info, err := a.InspectSnapshot(context.Background(), path)
	if err != nil {
		t.Fatalf("InspectSnapshot: %v", err)
	}
	// `snapshot inspect --json` prints the bare document with no path field,
	// which is the entire reason SnapshotInfo is a wrapper. The path must come
	// back from what we were asked to inspect.
	if info.Path != path {
		t.Errorf("Path = %q, want %q", info.Path, path)
	}
	if info.Snapshot.SchemaVersion != snapshot.SchemaVersion {
		t.Errorf("SchemaVersion = %q", info.Snapshot.SchemaVersion)
	}
	if got := info.Target(); got.DistroID != "ubuntu" || got.VersionID != "24.04" || got.Arch != "amd64" {
		t.Errorf("Target() = %+v", got)
	}
	if !invokeHas(stub.recorded()[0], path) {
		t.Errorf("the path was not passed to the command: %v", stub.recorded()[0])
	}
}

func TestInvokeInspectSnapshotRejectsAnUnknownSchema(t *testing.T) {
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: []byte(`{"schema_version":"debark.snapshot/v2","created_at":"2026-09-06T10:00:00Z"}`)}
	})

	_, err := a.InspectSnapshot(context.Background(), "/tmp/s.tar.zst")
	if err == nil {
		t.Fatal("InspectSnapshot accepted debark.snapshot/v2")
	}
	if e := invokeAsError(t, err); !strings.Contains(e.Summary(), "debark.snapshot/v2") {
		t.Errorf("Summary() = %q", e.Summary())
	}
}

// ---------------------------------------------------------------------------
// Keygen
// ---------------------------------------------------------------------------

func TestInvokeKeygen(t *testing.T) {
	doc := invokeFile(t, "json/keygen.json")
	a, stub := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: doc}
	})

	// keygen emits an ad-hoc map with no schema_version; KeyInfo is that map.
	key, err := a.Keygen(context.Background(), "/home/op/.config/debark/operator.key")
	if err != nil {
		t.Fatalf("Keygen: %v", err)
	}
	if key.KeyID != "86901d79b8808e65" {
		t.Errorf("KeyID = %q", key.KeyID)
	}
	if key.PrivateKeyPath != "/home/op/.config/debark/operator.key" {
		t.Errorf("PrivateKeyPath = %q", key.PrivateKeyPath)
	}
	if key.PublicKeyPath != "/home/op/.config/debark/operator.pub" {
		t.Errorf("PublicKeyPath = %q", key.PublicKeyPath)
	}
	call := stub.recorded()[0]
	if !invokeIsCmd(call, "keygen", "--out") {
		t.Errorf("argv = %v, want --out immediately after keygen", call)
	}
}

func TestInvokeKeygenDerivesAMissingPublicPath(t *testing.T) {
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: []byte(`{"key_id":"abc","private_key":"/keys/op.key"}`)}
	})

	key, err := a.Keygen(context.Background(), "/keys/op.key")
	if err != nil {
		t.Fatalf("Keygen: %v", err)
	}
	if key.PublicKeyPath != "/keys/op.pub" {
		t.Errorf("PublicKeyPath = %q, want /keys/op.pub", key.PublicKeyPath)
	}
}

func TestInvokeKeygenRefusesAnEmptyPathWithoutRunning(t *testing.T) {
	a, stub := invokeAdapter(t, func([]string) invokeReply {
		t.Error("keygen was executed with no --out; cobra would have answered with a help screen")
		return invokeReply{}
	})

	_, err := a.Keygen(context.Background(), "  ")
	if err == nil {
		t.Fatal("Keygen accepted an empty --out")
	}
	if n := len(stub.recorded()); n != 0 {
		t.Errorf("%d processes were started for a locally checkable error", n)
	}
	e := invokeAsError(t, err)
	if !e.HasClass(dferr.Usage) {
		t.Errorf("Class() = %s, want usage", e.ClassName())
	}
	if e.Hint() == "" {
		t.Error("Hint() is empty")
	}
}

func TestInvokeKeygenRejectsAnIncompleteAnswer(t *testing.T) {
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: []byte(`{}`)}
	})

	_, err := a.Keygen(context.Background(), "/keys/op.key")
	if err == nil {
		t.Fatal("Keygen accepted a key with no id")
	}
	if e := invokeAsError(t, err); e.Summary() == "" {
		t.Error("Summary() is empty")
	}
}

// ---------------------------------------------------------------------------
// Verify
// ---------------------------------------------------------------------------

func TestInvokeVerifyPasses(t *testing.T) {
	doc := invokeFile(t, "json/verify-ok.json")
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: doc}
	})

	rep, err := a.Verify(context.Background(), "/media/usb/bundle")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.SchemaVersion != verify.SchemaVersion {
		t.Errorf("SchemaVersion = %q", rep.SchemaVersion)
	}
	if !rep.OK || !rep.Signed {
		t.Errorf("OK = %v, Signed = %v, want both true", rep.OK, rep.Signed)
	}
	if rep.FilesChecked != 13 {
		t.Errorf("FilesChecked = %d, want 13", rep.FilesChecked)
	}
}

// TestInvokeVerifyReturnsBothOnAFailure covers a behaviour
// docs/dev/cli-surface.md does NOT record: C1 calls `build` "the exception"
// for printing its --json document and then failing silently, but `verify` is
// a second one. The fixture is real output — exit 4, a full report on stdout,
// and NOTHING on stderr.
func TestInvokeVerifyReturnsBothOnAFailure(t *testing.T) {
	doc := invokeFile(t, "json/verify-untrusted.json")
	silent := invokeFile(t, "stderr/verification-verify-silent.stderr")
	if len(strings.TrimSpace(string(silent))) != 0 {
		t.Fatalf("fixture assumption broken: a failing verify wrote %d bytes to stderr", len(silent))
	}

	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: doc, stderr: silent, exit: int(dferr.Verification)}
	})

	rep, err := a.Verify(context.Background(), "/media/usb/bundle")
	if err == nil {
		t.Fatal("Verify reported success for a bundle that did not verify")
	}
	// The report must come back too: it is the only place the detail exists.
	if rep.BundleID != "70b70224a13e5214" || len(rep.Problems) != 1 {
		t.Fatalf("report was not returned alongside the error: %+v", rep)
	}
	e := invokeAsError(t, err)
	if !e.HasClass(dferr.Verification) {
		t.Errorf("Class() = %s, want verification", e.ClassName())
	}
	// With an empty stderr the summary would otherwise fall back to dferr's
	// generic class description. It must say what actually went wrong.
	if !strings.Contains(e.Summary(), "no signature verifies against a trusted key") {
		t.Errorf("Summary() = %q, want the report's own problem", e.Summary())
	}
	if e.Summary() == dferr.Verification.Description() {
		t.Error("Summary() is the generic class description")
	}
}

func TestInvokeVerifyRefusesToPassANotOKReport(t *testing.T) {
	// Defence in depth: a report saying the bundle failed must never come back
	// as a success, whatever the process claimed with its exit status.
	doc := invokeFile(t, "json/verify-untrusted.json")
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: doc, exit: int(dferr.Success)}
	})

	rep, err := a.Verify(context.Background(), "/media/usb/bundle")
	if err == nil {
		t.Fatal("a report with ok=false was returned as a success")
	}
	if rep.OK {
		t.Error("rep.OK is true")
	}
	if e := invokeAsError(t, err); !e.HasClass(dferr.Verification) {
		t.Errorf("Class() = %s, want verification", e.ClassName())
	}
}

// ---------------------------------------------------------------------------
// Failure routing
// ---------------------------------------------------------------------------

// TestInvokeNonZeroExitGoesThroughClassify pins the contract with errors.go: a
// command that exits non-zero produces a *Error built from argv, exit code and
// stderr, and nothing else.
func TestInvokeNonZeroExitGoesThroughClassify(t *testing.T) {
	stderr := invokeFile(t, "stderr/usage-listbases-unknown-arch.stderr")
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{exit: int(dferr.Usage), stderr: stderr}
	})

	_, err := a.ListBases(context.Background(), "bogus")
	if err == nil {
		t.Fatal("ListBases succeeded for an unknown architecture")
	}
	e := invokeAsError(t, err)

	if e.ExitCode() != int(dferr.Usage) {
		t.Errorf("ExitCode() = %d, want 1", e.ExitCode())
	}
	if !e.HasClass(dferr.Usage) {
		t.Errorf("ClassName() = %q, want usage", e.ClassName())
	}
	// What the summary SAYS is errors.go's business — it may pass debark's
	// own first stderr line through or replace it with a better sentence. What
	// this test pins is that the failure reached that machinery at all and came
	// back as something a person can read: it names the thing that was wrong,
	// and cobra's usage block is not in it.
	if !strings.Contains(e.Summary(), "not-an-arch") {
		t.Errorf("Summary() = %q, want it to name the architecture that was rejected", e.Summary())
	}
	if strings.Contains(e.Summary(), "Usage:") {
		t.Errorf("Summary() = %q, cobra's usage block leaked into it", e.Summary())
	}
	if e.Summary() == dferr.Usage.Description() {
		t.Error("Summary() fell back to the generic class description")
	}
	// The usage block belongs in the details drawer, and argv must be the
	// command a person could paste — ProgramName, not an absolute path.
	if !strings.Contains(e.Stderr(), "Usage:") {
		t.Error("Stderr() lost the usage block")
	}
	if got := e.Argv(); len(got) == 0 || got[0] != ProgramName {
		t.Errorf("Argv() = %v, want it to start with %q", got, ProgramName)
	}
	if !strings.Contains(e.CommandLine(), "--json") {
		t.Errorf("CommandLine() = %q, want the --json the adapter really ran with", e.CommandLine())
	}
}

func TestInvokeStderrIsCapped(t *testing.T) {
	huge := strings.Repeat("debark: noise\n", (MaxStderrBytes/16)+4096)
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{exit: int(dferr.Environment), stderr: []byte(huge)}
	})

	_, err := a.ListBases(context.Background(), "")
	e := invokeAsError(t, err)
	if len(e.Stderr()) > MaxStderrBytes+64 {
		t.Errorf("Stderr() is %d bytes, want it capped near %d", len(e.Stderr()), MaxStderrBytes)
	}
	if !strings.Contains(e.Stderr(), "truncated") {
		t.Error("the truncation was not marked")
	}
}

func TestInvokeMissingBinaryIsActionable(t *testing.T) {
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{exit: procExitNotFound, err: exec.ErrNotFound}
	})

	_, err := a.ListBases(context.Background(), "")
	if err == nil {
		t.Fatal("ListBases succeeded with no binary to run")
	}
	e := invokeAsError(t, err)
	if !e.HasClass(dferr.Environment) {
		t.Errorf("ClassName() = %q, want environment (127 is outside the 0–7 table)", e.ClassName())
	}
	if e.ExitCode() != procExitNotFound {
		t.Errorf("ExitCode() = %d, want 127", e.ExitCode())
	}
	if !strings.Contains(e.Summary(), "could not be run") {
		t.Errorf("Summary() = %q", e.Summary())
	}
	if e.Hint() == "" {
		t.Error("Hint() is empty; this is the first thing an operator hits on a fresh machine")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Error("the cause was not preserved for errors.Is")
	}
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

func TestInvokeLocateFindsPATHFirst(t *testing.T) {
	dir := t.TempDir()
	name := ProgramName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got, err := Locate()
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if !strings.EqualFold(got, path) {
		t.Errorf("Locate() = %q, want %q", got, path)
	}
}

func TestInvokeLocateReportsAMissingBinary(t *testing.T) {
	// An empty PATH, and no debark beside the test binary in the temporary
	// directory `go test` builds it into.
	t.Setenv("PATH", t.TempDir())

	_, err := Locate()
	if err == nil {
		t.Skip("a debark binary sits beside the test binary on this machine")
	}
	e := invokeAsError(t, err)
	if !strings.Contains(e.Summary(), "was not found") {
		t.Errorf("Summary() = %q", e.Summary())
	}
	if !strings.Contains(e.Summary(), "PATH") {
		t.Errorf("Summary() = %q, want it to say where it looked", e.Summary())
	}
	if e.Hint() == "" {
		t.Error("Hint() is empty")
	}
	if e.ExitCode() != procExitNotFound {
		t.Errorf("ExitCode() = %d, want 127", e.ExitCode())
	}
}

func TestInvokeNewDoesNotTouchTheFilesystem(t *testing.T) {
	// Discovery is lazy on purpose: New must succeed on a machine where
	// debark is not installed yet, so the app can start and explain itself.
	a, err := New(Options{BinaryPath: filepath.Join(t.TempDir(), "not-installed-yet")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a == nil {
		t.Fatal("New returned a nil Adapter")
	}
}

func TestInvokeBinaryIsRediscoveredAfterAFailure(t *testing.T) {
	// A failed discovery is never cached, so installing debark and pressing
	// Retry works without restarting the application.
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	a := &adapter{runner: &invokeStub{}}
	if _, err := a.binary(); err == nil {
		t.Skip("a debark binary sits beside the test binary on this machine")
	}

	name := ProgramName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := a.binary()
	if err != nil {
		t.Fatalf("binary() after installing: %v", err)
	}
	if got == "" {
		t.Error("binary() returned an empty path")
	}
}

// ---------------------------------------------------------------------------
// Context and cancellation
// ---------------------------------------------------------------------------

func TestInvokeCancellationIsNotAFailure(t *testing.T) {
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{block: true}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := a.ListBases(ctx, "")
	if err == nil {
		t.Fatal("ListBases succeeded after its context was cancelled")
	}
	e := invokeAsError(t, err)
	if !e.Canceled() {
		t.Error("Canceled() is false; the UI would show a cancellation as an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Error("errors.Is(err, context.Canceled) is false")
	}
	if e.Summary() == "" {
		t.Error("Summary() is empty")
	}
}

func TestInvokeDeadlineIsAFailure(t *testing.T) {
	a, _ := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{block: true}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := a.ListBases(ctx, "")
	if err == nil {
		t.Fatal("ListBases succeeded after its deadline passed")
	}
	e := invokeAsError(t, err)
	if e.Canceled() {
		t.Error("a deadline was reported as an operator cancellation")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("errors.Is(err, context.DeadlineExceeded) is false")
	}
	if !strings.Contains(e.Summary(), "did not answer") {
		t.Errorf("Summary() = %q", e.Summary())
	}
}

func TestInvokeAlreadyCancelledContextStartsNothing(t *testing.T) {
	a, stub := invokeAdapter(t, func([]string) invokeReply {
		t.Error("a process was started for an already-cancelled context")
		return invokeReply{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := a.ListBases(ctx, ""); err == nil {
		t.Fatal("ListBases succeeded on a cancelled context")
	}
	if n := len(stub.recorded()); n != 0 {
		t.Errorf("%d processes were started", n)
	}
}

// invokeHelperSleepArg and invokeHelperExitArg mark the child process this test
// binary re-execs.
//
// They are arguments rather than environment variables on purpose: an env var
// would still be set when the parent's own test run reached the helper, and the
// helper calls os.Exit. And they deliberately do NOT begin with "-": the testing
// package's flag parsing rejects an unknown flag and exits 2 before any test
// runs, which would make the kill assertion below pass for the wrong reason. A
// non-flag argument stops flag parsing and stays in os.Args.
const (
	invokeHelperSleepArg = "t2a-helper-sleep"
	invokeHelperExitArg  = "t2a-helper-exit"
)

// TestInvokeHelperProcess is not a test. It is the child the two procExec tests
// below run, in one of two modes:
//
//	t2a-helper-sleep MARKER    sleep 30s, then write MARKER — so the parent can
//	                           prove the process really died rather than merely
//	                           that Run returned
//	t2a-helper-exit  CODE      print to both streams and exit with CODE
func TestInvokeHelperProcess(t *testing.T) {
	arg := func(name string) (string, bool) {
		for i, a := range os.Args {
			if a == name && i+1 < len(os.Args) {
				return os.Args[i+1], true
			}
		}
		return "", false
	}
	if marker, ok := arg(invokeHelperSleepArg); ok {
		time.Sleep(30 * time.Second)
		_ = os.WriteFile(marker, []byte("survived"), 0o600)
		os.Exit(0)
	}
	if code, ok := arg(invokeHelperExitArg); ok {
		n, err := strconv.Atoi(code)
		if err != nil {
			n = 1
		}
		_, _ = os.Stdout.WriteString(`{"schema_version":"debark.baselist/v1","arch":"amd64","bases":[]}`)
		_, _ = os.Stderr.WriteString("debark: something went wrong\nthe hint line\n")
		os.Exit(n)
	}
	t.Skip("not the helper child")
}

// TestInvokeProcExecReportsAnExitStatus pins procRunner's contract against the
// real os/exec: a process that ran and exited is a STATUS, never an error,
// however it exited — and both streams come back either way. Everything in this
// file that decodes a document out of a failed command depends on that.
func TestInvokeProcExecReportsAnExitStatus(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	for _, code := range []int{0, int(dferr.Verification), 5} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			args := []string{
				"-test.run=^TestInvokeHelperProcess$",
				invokeHelperExitArg, strconv.Itoa(code),
			}
			stdout, stderr, got, runErr := procExec{}.Run(context.Background(), self, args)
			if runErr != nil {
				t.Fatalf("Run reported an error for a process that exited: %v", runErr)
			}
			if got != code {
				t.Errorf("exitCode = %d, want %d", got, code)
			}
			if !strings.Contains(string(stdout), "debark.baselist/v1") {
				t.Errorf("stdout = %q", stdout)
			}
			if !strings.Contains(string(stderr), "something went wrong") {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
}

func TestInvokeProcCapWriterKeepsTheHead(t *testing.T) {
	w := &procCapWriter{limit: MaxStderrBytes + 1}
	chunk := []byte(strings.Repeat("x", 8192))
	total := 0
	for i := 0; i < 32; i++ {
		n, err := w.Write(chunk)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		// A short write would make os/exec's copier give up on the pipe.
		if n != len(chunk) {
			t.Fatalf("Write returned %d, want %d", n, len(chunk))
		}
		total += n
	}
	if total <= MaxStderrBytes {
		t.Fatal("the test did not write past the cap")
	}
	if len(w.Bytes()) != MaxStderrBytes+1 {
		t.Errorf("kept %d bytes, want %d", len(w.Bytes()), MaxStderrBytes+1)
	}
	// One byte over the cap, so newError marks the truncation rather than
	// silently trimming to exactly the limit.
	e := NewError([]string{ProgramName}, int(dferr.Environment), string(w.Bytes()))
	if !strings.Contains(e.Stderr(), "truncated") {
		t.Error("the truncation was not marked")
	}
}

// TestInvokeProcExecKillsTheChildOnCancel exercises the real procRunner, not a
// stub: cancelling a context must kill the process, not leave it orphaned.
func TestInvokeProcExecKillsTheChildOnCancel(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "survived.txt")
	args := []string{"-test.run=^TestInvokeHelperProcess$", invokeHelperSleepArg, marker}

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, _, _, runErr := procExec{}.Run(ctx, self, args)
	elapsed := time.Since(start)
	cancel()

	if elapsed > 10*time.Second {
		t.Fatalf("Run took %s; the child outlived its context", elapsed)
	}
	if runErr == nil {
		// exec reports the kill as an ExitError, which procExec turns into a
		// status rather than an error. Either shape is fine; what matters is
		// that it returned and that the child is gone.
		t.Log("Run reported the kill as an exit status")
	}

	// The child needed 30 seconds to write the marker. Give it two, well past
	// procWaitDelay, and require that it never did.
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the child ran to completion; cancelling the context did not kill it")
	}

	// And nothing was left running behind it.
	for i := 0; i < 40; i++ {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutines: %d before, %d after", before, runtime.NumGoroutine())
}

// ---------------------------------------------------------------------------
// Command, and the flags every invocation carries
// ---------------------------------------------------------------------------

func TestInvokeCommandIsBuildArgv(t *testing.T) {
	specs := []BuildSpec{
		{BaseID: "ubuntu:24.04/server", Packages: []string{"nginx"}},
		{
			SnapshotPath: "/media/usb/target.snapshot.tar.zst",
			Packages:     []string{"nginx", "curl=8.5.0-2ubuntu10.6"},
			URLs: []buildjob.URLInput{{
				URL:    "https://vendor.example/agent_2.1.0_amd64.deb",
				SHA256: strings.Repeat("ab", 32),
			}},
			LocalDebs: []string{"/srv/debs/tool.deb"},
			LocalDirs: []string{"/srv/debs"},
			ListFiles: []string{"/srv/packages.txt"},
			OutPath:   "/srv/out/bundle.debark.tar.zst",
			Format:    buildjob.FormatTar,
			SignerRef: "/keys/op.key",
			SBOM:      true,
			Update:    true,
			Backend:   "container",
		},
		{BaseID: "debian:13/minimal", Arch: "arm64", NoSign: true, ListFiles: []string{"/srv/p.txt"}},
	}
	a, _ := invokeAdapter(t, func([]string) invokeReply { return invokeReply{} })

	for i, spec := range specs {
		got := a.Command(spec)
		want := BuildArgv(spec)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("spec %d: Command = %v, BuildArgv = %v", i, got, want)
		}
		if invokeHas(got, "--interactive") {
			t.Errorf("spec %d: argv carries --interactive: %v", i, got)
		}
		if !invokeHas(got, "--json") {
			t.Errorf("spec %d: argv does not carry --json: %v", i, got)
		}
	}
}

// TestInvokeEveryCommandIsNonInteractive is the rule the whole adapter hangs
// on: --json (or --json-events) suppresses every interactive prompt, and
// --interactive would make debark ask a question on a terminal that is not
// there. A GUI that let that happen hangs forever with nothing on screen.
func TestInvokeEveryCommandIsNonInteractive(t *testing.T) {
	a, stub := invokeAdapter(t, invokeFullBinary(t))
	ctx := context.Background()

	if _, err := a.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if _, err := a.ListBases(ctx, "arm64"); err != nil {
		t.Fatalf("ListBases: %v", err)
	}
	if _, err := a.InspectSnapshot(ctx, "/tmp/s.tar.zst"); err != nil {
		t.Fatalf("InspectSnapshot: %v", err)
	}
	if _, err := a.Keygen(ctx, "/keys/op.key"); err != nil {
		t.Fatalf("Keygen: %v", err)
	}
	if _, err := a.Verify(ctx, "/media/usb/bundle"); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	calls := stub.recorded()
	if len(calls) < 7 {
		t.Fatalf("only %d commands were recorded", len(calls))
	}
	for _, call := range calls {
		if invokeHas(call, "--interactive") {
			t.Errorf("--interactive in %v", call)
		}
		if invokeHas(call, "--help") {
			// Help probes are the one shape that is not a --json document.
			continue
		}
		if !invokeHas(call, "--json") {
			t.Errorf("no --json in %v", call)
		}
		if !invokeHas(call, "--no-color") {
			t.Errorf("no --no-color in %v", call)
		}
	}
}

// ---------------------------------------------------------------------------
// Help parsing
// ---------------------------------------------------------------------------

// invokeProseHelp is the shape that makes scoping necessary: cobra prints a
// command's long description above the Usage block, and descriptions name
// flags and commands in prose.
const invokeProseHelp = `Resolve, fetch and sign, given directly or via --nonexistent-flag.
Run ghost first if you have not already.

Usage:
  debark thing [flags]

Flags:
  -h, --help   help for thing

Global Flags:
      --json   print JSON
`

func TestInvokeHasFlag(t *testing.T) {
	root := string(invokeFile(t, "help/root.txt"))
	build := string(invokeFile(t, "help/build.txt"))

	tests := []struct {
		name string
		help string
		flag string
		want bool
	}{
		{"global flag on root", root, "--json-events", true},
		{"a prefix of another flag", root, "--json", true},
		{"no --sbom on root", root, "--sbom", false},
		{"--sbom on build", build, "--sbom", true},
		{"--no-recommends on build", build, "--no-recommends", true},
		{"there is no --recommends", build, "--recommends", false},
		{"repeatable flag", build, "--local-dir", true},
		{"prose is not a flag block", invokeProseHelp, "--nonexistent-flag", false},
		{"a real flag in the block", invokeProseHelp, "--help", true},
		{"empty help", "", "--json", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := invokeHasFlag(tc.help, tc.flag); got != tc.want {
				t.Errorf("invokeHasFlag(%q) = %v, want %v", tc.flag, got, tc.want)
			}
		})
	}
}

func TestInvokeHasSubcommand(t *testing.T) {
	root := string(invokeFile(t, "help/root.txt"))
	group := string(invokeFile(t, "help/snapshot-group.txt"))

	tests := []struct {
		name string
		help string
		cmd  string
		want bool
	}{
		{"keygen on root", root, "keygen", true},
		{"build on root", root, "build", true},
		{"list-bases is not a root command", root, "list-bases", false},
		{"list-bases in the snapshot group", group, "list-bases", true},
		{"from-base in the snapshot group", group, "from-base", true},
		{"nothing invented", root, "frobnicate", false},
		{"prose is not a command block", invokeProseHelp, "ghost", false},
		{"empty help", "", "keygen", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := invokeHasSubcommand(tc.help, tc.cmd); got != tc.want {
				t.Errorf("invokeHasSubcommand(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func invokeAsError(t *testing.T, err error) *Error {
	t.Helper()
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("error is %T, want *cliadapter.Error: %v", err, err)
	}
	if e.Summary() == "" {
		t.Error("Summary() is empty; no error path may surface a raw exit code alone")
	}
	return e
}

// TestInvokeVerifyWithKeysPassesEachKey pins the argv, because the flag is the
// whole feature: `verify BUNDLE` with no --key and no configured verify_keys
// trusts nothing, so a bundle the GUI had just signed with the operator's own
// key failed its own verification screen.
func TestInvokeVerifyWithKeysPassesEachKey(t *testing.T) {
	doc := invokeFile(t, "json/verify-ok.json")
	a, stub := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: doc}
	})

	if _, err := a.VerifyWithKeys(context.Background(), "/media/usb/bundle",
		[]string{"/keys/operator.pub", "", "/keys/release.pub"}); err != nil {
		t.Fatalf("VerifyWithKeys: %v", err)
	}

	calls := stub.recorded()
	if len(calls) != 1 {
		t.Fatalf("ran %d commands, want 1: %v", len(calls), calls)
	}
	// The empty key is dropped rather than passed as an empty --key, which
	// debark would read as a file named "".
	want := []string{"verify", "/media/usb/bundle",
		"--key", "/keys/operator.pub", "--key", "/keys/release.pub", "--json", "--no-color"}
	if !reflect.DeepEqual(calls[0], want) {
		t.Errorf("argv = %v\nwant  %v", calls[0], want)
	}
}

// Verify itself must stay exactly what it was: no keys, so debark's own
// configured verify_keys/verify_keyring_dirs decide.
func TestInvokeVerifyPassesNoKeysByDefault(t *testing.T) {
	doc := invokeFile(t, "json/verify-ok.json")
	a, stub := invokeAdapter(t, func([]string) invokeReply {
		return invokeReply{stdout: doc}
	})

	if _, err := a.Verify(context.Background(), "/media/usb/bundle"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, arg := range stub.recorded()[0] {
		if arg == "--key" {
			t.Fatalf("Verify passed a key: %v", stub.recorded()[0])
		}
	}
}

// PublicKeyPath is exported for internal/app; the rule is keygen's own and is
// already written twice, so it is worth pinning where it can be seen.
func TestInvokePublicKeyPathIsExported(t *testing.T) {
	cases := map[string]string{
		"/keys/operator.key":   "/keys/operator.pub",
		"/keys/operator":       "/keys/operator.pub",
		"/keys/my.key.backup":  "/keys/my.key.backup.pub",
		`C:\keys\operator.key`: `C:\keys\operator.pub`,
	}
	for in, want := range cases {
		if got := PublicKeyPath(in); got != want {
			t.Errorf("PublicKeyPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The --json-events capability memo
// ---------------------------------------------------------------------------

// A help text that lists the flag settles it, and the answer is remembered:
// a build must not spawn `--help` again on every run.
func TestInvokeJSONEventsIsAnsweredAndRemembered(t *testing.T) {
	a, stub := invokeAdapter(t, invokeFullBinary(t))

	if !a.invokeJSONEventsSupported(context.Background()) {
		t.Fatal("a current debark was reported as unable to stream events")
	}
	first := len(stub.recorded())
	if !a.invokeJSONEventsSupported(context.Background()) {
		t.Fatal("the second answer disagreed with the first")
	}
	if n := len(stub.recorded()); n != first {
		t.Fatalf("the answer was not remembered: %d calls became %d", first, n)
	}
}

// A binary whose --help has no --json-events must be believed, or every build
// against it dies on cobra's usage block instead of running without a stream.
func TestInvokeJSONEventsBelievesAnOlderBinary(t *testing.T) {
	old := "Usage:\n  debark [command]\n\nFlags:\n" +
		"  -h, --help   help for debark\n" +
		"      --json   print the documented JSON object\n"
	a, _ := invokeAdapter(t, func(args []string) invokeReply {
		if invokeIsCmd(args, "--help") {
			return invokeReply{stdout: []byte(old)}
		}
		return invokeReply{exit: int(dferr.Usage), stderr: []byte("debark: unknown command\n")}
	})

	if a.invokeJSONEventsSupported(context.Background()) {
		t.Fatal("a binary whose help does not list --json-events was reported as having it")
	}
}

// The failure mode worth guarding: "we could not ask" must never be recorded
// as "it does not have it". A transient help failure that stuck would cost a
// modern debark its entire event stream for the life of the process.
func TestInvokeJSONEventsDoesNotRememberAFailedProbe(t *testing.T) {
	rootHelp := invokeFile(t, "help/root.txt")
	var broken atomic.Bool
	broken.Store(true)

	a, _ := invokeAdapter(t, func(args []string) invokeReply {
		if invokeIsCmd(args, "--help") {
			if broken.Load() {
				// What a binary too old to know --help does, and what the
				// events-test stand-in did before it learned to answer: an
				// error message on stderr and a non-zero status. It is not a
				// flag listing and must not be read as one.
				return invokeReply{exit: int(dferr.Usage), stderr: []byte("debark: unknown flag: --help\n")}
			}
			return invokeReply{stdout: rootHelp}
		}
		return invokeReply{exit: int(dferr.Usage)}
	})

	// No evidence either way: assume the flag is there, which is the
	// pre-existing behaviour and the right way to be wrong.
	if !a.invokeJSONEventsSupported(context.Background()) {
		t.Fatal("a help probe that failed was read as proof the flag is absent")
	}
	broken.Store(false)
	if !a.invokeJSONEventsSupported(context.Background()) {
		t.Fatal("the second, successful probe was not consulted")
	}
}
