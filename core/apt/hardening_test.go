package apt

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
)

// This file covers the apt-invocation and apt-output-parsing defects fixed in
// local.go, parse.go and sources.go. Every test here fails without its fix —
// each was written by first reproducing the defect (printing the argv apt was
// actually handed, or the actual parse result) and only then asserting the
// corrected behaviour.
//
// It reuses local_test.go's routedRunner, argsEqual/argsAreSet and
// minimalSnapshotWithHolds rather than introducing a second fake.

// --- shared fixture helpers ------------------------------------------------

// writeSnapshotSource adds one captured apt source file to snap, writing its
// bytes where BuildPrivateRoot's own reader (readSnapshotFile, through
// safeExtractPath) will find them. This is what makes a test able to say
// "the target had this source" and have the private root really be built
// from it, rather than asserting against a hand-made PrivateRoot.
func writeSnapshotSource(t *testing.T, snap *snapshot.Snapshot, filesDir, targetPath, content string) {
	t.Helper()
	archivePath := strings.TrimPrefix(targetPath, "/")
	dst := filepath.Join(filesDir, filepath.FromSlash(archivePath))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	snap.APT.Sources = append(snap.APT.Sources, snapshot.File{
		Path:        targetPath,
		ArchivePath: archivePath,
		Size:        int64(len(content)),
	})
}

// writeNamedPackagesIndex is writeFakePackagesIndex with the index file's own
// name under the caller's control. The name is not decoration: apt derives
// the suite, the component and — since the trusted=yes fix — which source a
// stanza came from, from that name alone.
func writeNamedPackagesIndex(t *testing.T, listsDir, indexName string, pkgs ...fakeArchivePkg) {
	t.Helper()
	if err := os.MkdirAll(listsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, p := range pkgs {
		sb.WriteString("Package: " + p.Name + "\nVersion: " + p.Version + "\nArchitecture: " + p.Arch +
			"\nFilename: pool/x/" + p.Name + "/" + p.Name + "_" + p.Version + "_" + p.Arch + ".deb" +
			"\nSize: 12\nSHA256: " + strings.Repeat("c", 64) + "\nSection: misc\n\n")
	}
	if err := os.WriteFile(filepath.Join(listsDir, indexName), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func warningByCode(ws []lock.Warning, code string) (lock.Warning, bool) {
	for _, w := range ws {
		if w.Code == code {
			return w, true
		}
	}
	return lock.Warning{}, false
}

// --- defect: argv operands are options unless "--" and validation say otherwise

func TestAptOperandArgs_TerminatorAndRejection(t *testing.T) {
	got, err := aptOperandArgs("test", []string{"acme-tool", "libfoo:i386=1:2.3-1~bpo", "bar/bookworm"})
	if err != nil {
		t.Fatalf("legitimate operands rejected: %v", err)
	}
	want := []string{"--", "acme-tool", "libfoo:i386=1:2.3-1~bpo", "bar/bookworm"}
	if len(got) != len(want) {
		t.Fatalf("aptOperandArgs = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("aptOperandArgs = %q, want %q", got, want)
		}
	}

	// Every one of these was accepted verbatim as a "package name" before,
	// and apt reads each as an option, not as a package.
	for _, bad := range []string{
		"-o",
		"--allow-unauthenticated",
		"--allow-insecure-repositories",
		"--option=APT::Get::AllowUnauthenticated=true",
		"-oAcquire::AllowInsecureRepositories=true",
		"--print-uris",
		"-y",
		"-y:amd64=1.0",
		"--print-uris:amd64=1.0",
		"../../../../tmp/evil",
		"pkg name with spaces",
		"",
		"acme=",
	} {
		if _, err := aptOperandArgs("test", []string{bad}); err == nil {
			t.Errorf("aptOperandArgs accepted %q; apt would read it as an option or a bad name", bad)
		} else if dferr.ClassOf(err) != dferr.Usage {
			t.Errorf("aptOperandArgs(%q) class = %v, want %v", bad, dferr.ClassOf(err), dferr.Usage)
		}
	}
}

// TestResolve_RejectsOptionShapedRequestedPackage drives the whole Resolve so
// the claim is about what apt is actually handed, not about a helper in
// isolation: an option-shaped name must never reach an apt-get argv at all.
func TestResolve_RejectsOptionShapedRequestedPackage(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	workDir := t.TempDir()

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	// A catch-all so that, with the fix removed, the run gets far enough to
	// reach this test's real assertions instead of dying on an unrouted call:
	// what is being proved is that the operand never reaches an argv, not
	// that some particular call was or was not made.
	runner.on(func([]string) bool { return true }, Output{})

	b := newLocalBackend(LocalOptions{Runner: runner})
	_, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Packages:         []string{"acme-tool", "--allow-unauthenticated"},
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err == nil {
		t.Fatal("Resolve accepted an option-shaped package name")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("error class = %v, want %v: %v", dferr.ClassOf(err), dferr.Usage, err)
	}
	for _, call := range runner.calls {
		for _, a := range call {
			if a == "--allow-unauthenticated" {
				t.Fatalf("the option-shaped name still reached an apt-get argv: %q", call)
			}
		}
	}
}

// TestResolve_RejectsHostileInstLineName is the third, least obvious source
// of an unvalidated operand: the names in `versions` come from parsing apt's
// own Inst lines, which a hostile archive writes. Reproduced before the fix
// by printing the argv, which was
// ["apt-get" ... "install" "--print-uris" "-y" "-y:amd64=1.0"].
func TestResolve_RejectsHostileInstLineName(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	workDir := t.TempDir()
	writeFakePackagesIndex(t, filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists"),
		fakeArchivePkg{Name: "acme-tool", Version: "1.0", Arch: "amd64", Size: 12})

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	runner.on(argsEqual("-s", "install", "--", "acme-tool"), Output{Stdout: []byte(
		"Inst acme-tool (1.0 acme:12 [amd64])\n" +
			"Inst -y (1.0 acme:12 [amd64])\n",
	)})
	// See the catch-all in TestResolve_RejectsOptionShapedRequestedPackage:
	// with the fix removed this test must reach its own assertions, not die
	// on an unrouted call.
	runner.on(func([]string) bool { return true }, Output{})

	b := newLocalBackend(LocalOptions{Runner: runner})
	_, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Packages:         []string{"acme-tool"},
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err == nil {
		t.Fatal("Resolve accepted a package name apt itself printed that is option-shaped")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("error class = %v, want %v: %v", dferr.ClassOf(err), dferr.Usage, err)
	}
	for _, call := range runner.calls {
		for _, a := range call {
			if strings.HasPrefix(a, "-y:") {
				t.Fatalf("the hostile name still reached an apt-get argv: %q", call)
			}
		}
	}
}

// --- defect: trusted=yes sources were still recorded as apt-signed ---------

// resolveSelectionFrom runs a complete Resolve for a single package whose
// index stanza lives in indexName, with sourceLine as the target's only
// captured apt source, and returns the one selection produced. Driving the
// real Resolve (and so the real BuildPrivateRoot, which materialises the
// source file the fix reads back) is the point: a hand-built PrivateRoot
// would prove nothing about whether the source really reaches apt.
func resolveSelectionFrom(t *testing.T, sourceFile, sourceLine, indexName string) resolve.Selection {
	t.Helper()
	snap, filesDir := minimalSnapshotWithHolds(t)
	writeSnapshotSource(t, snap, filesDir, sourceFile, sourceLine)

	workDir := t.TempDir()
	archivesDir := filepath.Join(workDir, "archives")
	writeNamedPackagesIndex(t, filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists"), indexName,
		fakeArchivePkg{Name: "acme-tool", Version: "1.0", Arch: "amd64", Size: 12})

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	runner.on(argsEqual("-s", "install", "--", "acme-tool"), Output{Stdout: []byte(
		"Inst acme-tool (1.0 acme:12 [amd64])\n",
	)})
	runner.on(argsEqual("install", "--print-uris", "-y", "--", "acme-tool:amd64=1.0"), Output{})
	runner.on(argsEqual("--download-only", "-y", "install", "--", "acme-tool:amd64=1.0"), Output{})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Packages:         []string{"acme-tool"},
		ArchivesDir:      archivesDir,
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Selections) != 1 {
		t.Fatalf("Selections = %+v, want exactly 1", plan.Selections)
	}
	return plan.Selections[0]
}

// TestResolve_TrustedYesSource_IsNotAPTSigned is the lock-integrity defect:
// a source that told apt to skip signature verification entirely still had
// its packages stamped apt-signed, so lock.json asserted a check that
// provably did not run.
func TestResolve_TrustedYesSource_IsNotAPTSigned(t *testing.T) {
	sel := resolveSelectionFrom(t,
		"/etc/apt/sources.list.d/vendor.list",
		"deb [trusted=yes] http://attacker.example/ stable main\n",
		"attacker.example_dists_stable_main_binary-amd64_Packages")
	if sel.PublisherVerification == lock.VerifiedAPTSigned {
		t.Errorf("PublisherVerification = %q: this package came from a [trusted=yes] source, where apt performs no signature check at all — recording apt-signed makes lock.json assert a check that never ran",
			sel.PublisherVerification)
	}
	if sel.PublisherVerification != lock.VerifiedURLUnverified {
		t.Errorf("PublisherVerification = %q, want %q (the same answer the external staging repo and core/fetch's FromLocalFile already give)",
			sel.PublisherVerification, lock.VerifiedURLUnverified)
	}
}

// TestResolve_SignedSource_StaysAPTSigned is the other half: the fix must not
// downgrade an ordinary, genuinely verified archive. Without it this test
// would pass for the wrong reason, so it is what keeps the one above honest.
func TestResolve_SignedSource_StaysAPTSigned(t *testing.T) {
	sel := resolveSelectionFrom(t,
		"/etc/apt/sources.list",
		"deb http://deb.debian.org/debian bookworm main\n",
		"deb.debian.org_debian_dists_bookworm_main_binary-amd64_Packages")
	if sel.PublisherVerification != lock.VerifiedAPTSigned {
		t.Errorf("PublisherVerification = %q, want %q: this source carries no trusted=yes, so apt did verify it",
			sel.PublisherVerification, lock.VerifiedAPTSigned)
	}
}

// TestResolve_Deb822TrustedSource_IsNotAPTSigned covers the deb822 spelling
// of the same option, which is what a modern Debian or Ubuntu target uses.
func TestResolve_Deb822TrustedSource_IsNotAPTSigned(t *testing.T) {
	sel := resolveSelectionFrom(t,
		"/etc/apt/sources.list.d/vendor.sources",
		"Types: deb\nURIs: http://attacker.example/repo\nSuites: stable\nComponents: main\nTrusted: yes\n",
		"attacker.example_repo_dists_stable_main_binary-amd64_Packages")
	if sel.PublisherVerification != lock.VerifiedURLUnverified {
		t.Errorf("PublisherVerification = %q, want %q for a deb822 \"Trusted: yes\" source",
			sel.PublisherVerification, lock.VerifiedURLUnverified)
	}
}

func TestTrustedIndexPrefixes_MatchesAptsOwnListFileNames(t *testing.T) {
	// The mangling is checked against real captured list file names in this
	// repository (hack/experiments/out/e2/*/aptroot/var/lib/apt/lists), not
	// against a guess: those are the names a real apt actually wrote.
	cases := map[string]string{
		"http://archive.ubuntu.com/ubuntu":   "archive.ubuntu.com_ubuntu_",
		"http://archive.ubuntu.com/ubuntu/":  "archive.ubuntu.com_ubuntu_",
		"file:///src/hack/experiments/repo/": "_src_hack_experiments_repo_",
		"https://user:pw@vendor.example/deb": "vendor.example_deb_",
		"http://deb.debian.org/debian":       "deb.debian.org_debian_",
		"http://a.example/has%20space/x":     "a.example_has%2520space_x_",
		"http://a.example/under_score":       "a.example_under%5fscore_",
	}
	for uri, want := range cases {
		u := uri
		if !strings.HasSuffix(u, "/") {
			u += "/"
		}
		if got := aptURItoFileName(u); got != want {
			t.Errorf("aptURItoFileName(%q) = %q, want %q", u, got, want)
		}
	}

	real := "archive.ubuntu.com_ubuntu_dists_noble_main_binary-amd64_Packages"
	if !indexFileBelongsTo(real, []string{"archive.ubuntu.com_ubuntu_"}) {
		t.Errorf("%q should belong to the archive.ubuntu.com/ubuntu source", real)
	}
	// The trailing '_' is what stops one host's prefix swallowing another's.
	if indexFileBelongsTo(real, []string{"archive.ubuntu.com_ubuntu-ports_"}) {
		t.Error("archive.ubuntu.com/ubuntu-ports must not claim archive.ubuntu.com/ubuntu's index")
	}
}

// --- defect: credentials and host paths reached lock.json ------------------

func TestLockSafeText_RedactsCredentialsAndMasksHostPaths(t *testing.T) {
	masks := []hostPathMask{
		{`C:\Temp\debark-build-9182\aptroot`, "<PRIVATE-ROOT>"},
		{`C:\Temp\debark-build-9182`, "<WORK-DIR>"},
	}
	in := "E: Failed to fetch https://ci-bot:s3cr3t-token@artifactory.corp/deb/dists/stable/InRelease  401  Unauthorized; " +
		`apt: parse C:\Temp\debark-build-9182\aptroot\var\lib\apt\lists\evil_Packages: Bad line; ` +
		"see https://vendor.example/docs?token=abc123."
	got := lockSafeText(in, masks)

	for _, secret := range []string{"s3cr3t-token", "ci-bot", "token=abc123", "debark-build-9182"} {
		if strings.Contains(got, secret) {
			t.Errorf("lockSafeText left %q in a string bound for lock.json:\n%s", secret, got)
		}
	}
	// Redacting must stay visible and must keep what makes the record useful.
	for _, keep := range []string{"REDACTED", "artifactory.corp", "401", "<PRIVATE-ROOT>", "Bad line"} {
		if !strings.Contains(got, keep) {
			t.Errorf("lockSafeText dropped %q, which the record still needs:\n%s", keep, got)
		}
	}
	// The trailing full stop must survive outside the URI, not be eaten as
	// part of it.
	if !strings.HasSuffix(got, ".") {
		t.Errorf("sentence punctuation after a URI was swallowed:\n%s", got)
	}
}

func TestResolveHostPathMasks_LongestFirst(t *testing.T) {
	// Order is the whole correctness property: masking the containing
	// directory first leaves "<WORK-DIR>/aptroot/..." behind, which still
	// varies run to run.
	masks := resolveHostPathMasks("/tmp/build-1/aptroot", ResolveInput{
		WorkDir:          "/tmp/build-1",
		ArchivesDir:      "/tmp/build-1/archives",
		SnapshotFilesDir: "/tmp/snap-2/files",
		ExternalRepoDir:  "/tmp/build-1/external",
	})
	for i := 1; i < len(masks); i++ {
		if len(masks[i-1].Path) < len(masks[i].Path) {
			t.Fatalf("masks are not longest-first: %+v", masks)
		}
	}
	got := maskHostPaths("/tmp/build-1/aptroot/var/lib and /tmp/build-1/archives/x and /tmp/build-1/y", masks)
	if strings.Contains(got, "build-1") {
		t.Errorf("a per-run directory survived masking: %s", got)
	}
}

// TestResolve_Warnings_AreLockSafe drives the real Resolve so the assertion
// is about what actually lands in the Plan, and covers the two live leaks
// together: an apt-get update Err: line naming a credentialed URI, and a
// Packages-index read failure naming the run's own temp directory.
func TestResolve_Warnings_AreLockSafe(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	workDir := t.TempDir()
	archivesDir := filepath.Join(workDir, "archives")

	// A file matching *_Packages that ReadPackagesIndex cannot parse: its
	// error names the full path, which is under this run's own temp dir.
	listsDir := filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists")
	if err := os.MkdirAll(listsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(listsDir, "evil_Packages"), []byte("not: a\nvalid stanza at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{Stdout: []byte(
		"Err:1 https://ci-bot:s3cr3t-token@artifactory.corp/deb stable InRelease\n" +
			"  401  Unauthorized\n" +
			"W: GPG error: https://ci-bot:s3cr3t-token@artifactory.corp/deb stable InRelease: NO_PUBKEY DEADBEEFCAFE\n",
	)})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		ArchivesDir:      archivesDir,
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("no warnings at all; this test cannot prove anything")
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w.Message, "s3cr3t-token") || strings.Contains(w.Message, "ci-bot") {
			t.Errorf("warning %s ships a credential inside lock.json: %s", w.Code, w.Message)
		}
		if strings.Contains(w.Message, workDir) {
			t.Errorf("warning %s leaks this run's own temp directory (breaking byte-identical lock.json across two builds of one request): %s", w.Code, w.Message)
		}
	}
	if w, ok := warningByCode(plan.Warnings, "resolver.packages-index-unreadable"); !ok {
		t.Errorf("expected a resolver.packages-index-unreadable warning, got %+v", plan.Warnings)
	} else if !strings.Contains(w.Message, "<PRIVATE-ROOT>") {
		t.Errorf("the masked private root should still be visible as a placeholder: %s", w.Message)
	}
}

// --- defect: tolerated apt-get update GPG/fetch errors were parsed and dropped

// TestResolve_ToleratedGPGError_IsRecorded is the case that made this matter:
// with insecure-repository options in play apt reports a bad signature as a
// "W:" line and exits 0, so ParseUpdate reports Failed=false, nothing ever
// read GPGErrors (its only consumer, UpdateResult.Error(), is called on the
// already-failed path), and the build resolved against an unauthenticated
// repository while recording nothing at all.
func TestResolve_ToleratedGPGError_IsRecorded(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	workDir := t.TempDir()

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{Stdout: []byte(
		"Get:1 http://vendor.example/deb stable InRelease [2000 B]\n" +
			"W: GPG error: http://vendor.example/deb stable InRelease: The following signatures were invalid: BADSIG DEADBEEFCAFE Vendor\n" +
			"Err:2 http://mirror.example/deb stable/main amd64 Packages\n" +
			"  404  Not Found\n",
	), ExitCode: 0})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v (apt exited 0, so this must not be an error)", err)
	}
	gpg, ok := warningByCode(plan.Warnings, "apt-update.gpg-error")
	if !ok {
		t.Fatalf("apt reported a GPG error and exited 0; nothing recorded it: %+v", plan.Warnings)
	}
	if !strings.Contains(gpg.Message, "vendor.example") {
		t.Errorf("the GPG-error warning does not say which repository: %s", gpg.Message)
	}
	if _, ok := warningByCode(plan.Warnings, "apt-update.fetch-error"); !ok {
		t.Errorf("an Err: entry apt tolerated was not recorded either: %+v", plan.Warnings)
	}
}

func TestUpdateDiagnosticWarnings_SilentOnACleanUpdate(t *testing.T) {
	upd := ParseUpdate(Output{Stdout: []byte(
		"Hit:1 http://deb.debian.org/debian bookworm InRelease\n" +
			"Get:2 http://deb.debian.org/debian bookworm-updates InRelease [55.4 kB]\n",
	)})
	if got := updateDiagnosticWarnings(upd); len(got) != 0 {
		t.Errorf("a clean update produced warnings: %+v", got)
	}
}

// --- defect: ParseSimulate dropped unreadable Inst/Conf lines and called it success

func TestParseSimulate_UnreadableActionLineIsReported(t *testing.T) {
	// A package name containing a space makes instConfRE fail. Before the
	// fix the line simply vanished: failed=false, actions=[].
	sim := ParseSimulate(Output{Stdout: []byte(
		"Inst acme-tool (1.0 Debian:12/stable [amd64])\n" +
			"Inst acme tool (1.0 Debian:12/stable [amd64])\n" +
			"Conf acme-tool (1.0 Debian:12/stable [amd64])\n",
	)})
	if len(sim.Actions) != 2 {
		t.Errorf("Actions = %+v, want the two readable ones", sim.Actions)
	}
	if len(sim.UnparsedActions) != 1 {
		t.Fatalf("UnparsedActions = %q, want exactly the one unreadable line", sim.UnparsedActions)
	}
	if !strings.Contains(sim.UnparsedActions[0], "acme tool") {
		t.Errorf("UnparsedActions[0] = %q", sim.UnparsedActions[0])
	}
	// apt is the oracle for whether the run failed; this parser is not.
	if sim.Failed {
		t.Error("an unreadable line must not be reported as an apt failure: apt exited 0")
	}
}

func TestParseSimulate_ShortBreaksSuffixIsStillReadable(t *testing.T) {
	// apt appends the broken set after the closing parenthesis when a
	// simulated transaction leaves something broken. Not present in the
	// recorded goldens (every captured run resolved cleanly), so this half
	// is defensive — but dropping the line would be exactly the silent loss
	// UnparsedActions exists to prevent.
	sim := ParseSimulate(Output{Stdout: []byte(
		"Inst acme-tool [1.0] (2.0 Debian:12/stable [amd64]) [other:amd64 ]\n",
	)})
	if len(sim.UnparsedActions) != 0 {
		t.Fatalf("UnparsedActions = %q, want none", sim.UnparsedActions)
	}
	if len(sim.Actions) != 1 || sim.Actions[0].Name != "acme-tool" || sim.Actions[0].Version != "2.0" || sim.Actions[0].Arch != "amd64" {
		t.Fatalf("Actions = %+v", sim.Actions)
	}
}

// TestResolve_UnreadableInstLine_IsHardError is the whole point of the
// UnparsedActions field. Before it, the dropped package never entered
// `versions`, was never fetched, never reached the closure — and its absence
// then came out as unsatisfiedNonHeldRequest's reassuring
// "resolver.already-satisfied" warning with the build exiting 0. On the
// install-time target that is an unmet dependency inside a bundle whose whole
// promise is that there are none.
func TestResolve_UnreadableInstLine_IsHardError(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	workDir := t.TempDir()
	writeFakePackagesIndex(t, filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists"),
		fakeArchivePkg{Name: "acme-tool", Version: "1.0", Arch: "amd64", Size: 12})

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	runner.on(argsEqual("-s", "install", "--", "acme-tool", "acme-dep"), Output{Stdout: []byte(
		"Inst acme-tool (1.0 acme:12 [amd64])\n" +
			"Inst acme dep (2.0 acme:12 [amd64])\n",
	)})
	// Catch-all: with the fix removed the dropped package simply never gets
	// fetched, and the run continues to a successful Plan. That completed run
	// is the thing being caught here, so it has to be allowed to happen.
	runner.on(func([]string) bool { return true }, Output{})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Packages:         []string{"acme-tool", "acme-dep"},
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err == nil {
		var codes []string
		for _, w := range plan.Warnings {
			codes = append(codes, w.Code)
		}
		t.Fatalf("Resolve succeeded with a package apt announced but this parser could not read; warnings=%v selections=%d — this is the exit-0-with-a-missing-package case",
			codes, len(plan.Selections))
	}
	if dferr.ClassOf(err) != dferr.Resolution {
		t.Errorf("error class = %v, want %v: %v", dferr.ClassOf(err), dferr.Resolution, err)
	}
	if !strings.Contains(err.Error(), "acme dep") {
		t.Errorf("the error should quote the line apt printed: %v", err)
	}
}

// --- defect: a package both requested and supplied as a vendor .deb --------

// TestResolve_RequestedAndExternal_TakesTheExternalBranch covers
// "debark build foo ./foo_1.0_amd64.deb", an ordinary operator action. The
// requested case used to win the switch, so Reason became "requested",
// PublisherVerification stayed apt-signed, and the ReasonExternal-gated
// StagedPath correction never ran — leaving StagedPath pointing at a file
// apt's file: method never copied into the archives directory.
func TestResolve_RequestedAndExternal_TakesTheExternalBranch(t *testing.T) {
	snap, filesDir := minimalSnapshotWithHolds(t)
	workDir := t.TempDir()
	externalRepoDir := filepath.Join(workDir, "external")
	writeFakePackagesIndex(t, filepath.Join(workDir, "aptroot", "var", "lib", "apt", "lists"),
		fakeArchivePkg{Name: "acme-vendor-tool", Version: "1.0", Arch: "amd64", Size: 12})

	runner := &routedRunner{t: t}
	runner.on(argsEqual("-v"), Output{Stdout: []byte("apt 2.6.1 (amd64)\n")})
	runner.on(argsEqual("update"), Output{})
	runner.on(argsEqual("-s", "install", "--", "acme-vendor-tool"), Output{Stdout: []byte(
		"Inst acme-vendor-tool (1.0 debark-external [amd64])\n",
	)})
	runner.on(argsEqual("install", "--print-uris", "-y", "--", "acme-vendor-tool:amd64=1.0"), Output{})
	runner.on(argsEqual("--download-only", "-y", "install", "--", "acme-vendor-tool:amd64=1.0"), Output{})

	b := newLocalBackend(LocalOptions{Runner: runner})
	plan, err := b.Resolve(context.Background(), ResolveInput{
		Snapshot:         snap,
		SnapshotFilesDir: filesDir,
		Packages:         []string{"acme-vendor-tool"},
		ExternalRepoDir:  externalRepoDir,
		ExternalNames:    []string{"acme-vendor-tool"},
		ArchivesDir:      filepath.Join(workDir, "archives"),
		WorkDir:          workDir,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Selections) != 1 {
		t.Fatalf("Selections = %+v, want exactly 1", plan.Selections)
	}
	sel := plan.Selections[0]
	if sel.Reason != lock.ReasonExternal {
		t.Errorf("Reason = %q, want %q: the bytes came from the operator, which is what the lock has to say, whether or not the name was also typed on the command line", sel.Reason, lock.ReasonExternal)
	}
	if sel.PublisherVerification != lock.VerifiedURLUnverified {
		t.Errorf("PublisherVerification = %q, want %q", sel.PublisherVerification, lock.VerifiedURLUnverified)
	}
	if filepath.Dir(sel.StagedPath) != externalRepoDir {
		t.Errorf("StagedPath = %q, want it inside the external staging dir %q (apt's file: method never copies into the archives dir)", sel.StagedPath, externalRepoDir)
	}
}

// --- FileURI ---------------------------------------------------------------

func TestFileURI(t *testing.T) {
	cases := map[string]string{
		"/tmp/plain":          "file:///tmp/plain",
		"/tmp/has space/repo": "file:///tmp/has%20space/repo",
	}
	for in, want := range cases {
		if got := FileURI(in); got != want {
			t.Errorf("FileURI(%q) = %q, want %q", in, got, want)
		}
	}
	// A Windows drive-letter path is sample STRING input here: nothing is
	// mounted or touched. "file://C:/..." has only two slashes, which puts
	// "C:" in the URI's authority instead of its path.
	//
	// The expected result is genuinely OS-dependent, and this assertion used
	// to hard-code the Windows one, so the whole test failed on Linux — which
	// is where apt, and therefore CI, actually runs. It is not fixed by
	// skipping: both answers are correct, and both are worth asserting.
	// FileURI goes through filepath.ToSlash, which is a no-op wherever the
	// separator is already "/". So on Windows the backslashes become path
	// separators; on Linux a backslash is an ordinary, legal character in a
	// filename, and percent-encoding it is the right thing to do — apt's
	// file: method decodes it straight back to the name on disk.
	wantDriveLetter := "file:///D:%5Cprojects%5Cext%20dir"
	if runtime.GOOS == "windows" {
		wantDriveLetter = "file:///D:/projects/ext%20dir"
	}
	if got := FileURI(`D:\projects\ext dir`); got != wantDriveLetter {
		t.Errorf("FileURI(drive-letter path) = %q, want %q (GOOS=%s)", got, wantDriveLetter, runtime.GOOS)
	}
	// The whole reason the encoding matters: this is one field of a
	// whitespace-separated one-line source entry.
	line := "deb [trusted=yes] " + FileURI("/tmp/has space/repo") + " ./"
	if got := len(strings.Fields(line)); got != 4 {
		t.Errorf("source line split into %d fields, want 4: %s", got, line)
	}

	// The two properties that must hold on EVERY platform, whatever the
	// separator is: the URI is a single whitespace-free field, and decoding
	// it gives back exactly the path that went in (with the platform's own
	// separators normalised to "/"). Asserting the property rather than one
	// platform's literal string is what the drive-letter case above could
	// not do; it is also what makes a future change to FileURI's escaping
	// fail here rather than in apt.
	for _, in := range []string{
		"/tmp/plain",
		"/tmp/has space/repo",
		`D:\projects\ext dir`,
		"/tmp/awkward#name?with&specials",
		"/tmp/percent%20already",
	} {
		got := FileURI(in)
		if strings.ContainsAny(got, " 	") {
			t.Errorf("FileURI(%q) = %q: contains whitespace, so it cannot be one field of a sources line", in, got)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Errorf("FileURI(%q) = %q: does not parse as a URL: %v", in, got, err)
			continue
		}
		if u.Scheme != "file" || u.Host != "" {
			t.Errorf("FileURI(%q) = %q: scheme=%q host=%q, want scheme file and an empty authority", in, got, u.Scheme, u.Host)
		}
		if want := "/" + strings.TrimPrefix(filepath.ToSlash(in), "/"); u.Path != want {
			t.Errorf("FileURI(%q) = %q: decoded path %q, want %q", in, got, u.Path, want)
		}
	}
}
