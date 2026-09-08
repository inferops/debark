package cliadapter

// Tests for errors.go — the implementing package.
//
// The definition-of-done item this file exists to prove:
//
//	Every error path renders an actionable message; none surface a raw exit
//	code alone.
//
// So the tests are written as invariants over generated inputs rather than as
// a handful of golden strings. errTestInvariants is applied to every *Error
// this file produces, from every source: the frozen dferr class table, the
// fake's realistic stderr, real stderr captured from a real binary, exit codes
// nobody has seen, random bytes and invalid UTF-8.
//
// This is an internal test package because classify is unexported — it is the
// seam invoke.go and events.go call, and it is deliberately not part of the
// public surface. That is also why the fake's failure text is mirrored below
// instead of imported: internal/cliadapter/fake imports this package, so an
// internal test cannot import it back. TestClassifyFakeCorpusHasNotDrifted
// reads fake.go off disk and fails if the mirrored strings stop matching.
//
// Every identifier here is prefixed errTest, classifyTest, TestClassify or
// TestErr: invoke_test.go and events_test.go are being written into this same
// package concurrently, and a redeclaration breaks the build for all three.

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/inferops/debark/core/dferr"
)

// ---------------------------------------------------------------------------
// The invariants
// ---------------------------------------------------------------------------

// errTestBareNumber matches a message that is nothing but a number, with or
// without the "exit status" wrapper Go's os/exec would give you for free. This
// is the shape the definition of done forbids.
var errTestBareNumber = regexp.MustCompile(`^\s*(?:exit(?:ed)?(?:\s+(?:with\s+)?status)?|signal|error|status|code)?\s*:?\s*-?\d+\s*\.?\s*$`)

// errTestLetters is the cheapest possible test for "this carries words and not
// just a number". It is deliberately weak: text debark itself printed is
// passed through, and the invariant being defended is only that a message is
// never a bare status.
var errTestLetters = regexp.MustCompile(`[A-Za-z]{3,}`)

// errTestInvariants asserts everything that must hold for EVERY *Error this
// package produces, whatever it was built from.
func errTestInvariants(t *testing.T, what string, e *Error, exitCode int) {
	t.Helper()
	if e == nil {
		t.Fatalf("%s: classify returned nil", what)
	}

	summary := e.Summary()
	hint := e.Hint()

	if strings.TrimSpace(summary) == "" {
		t.Errorf("%s: empty summary", what)
	}
	if strings.TrimSpace(hint) == "" {
		t.Errorf("%s: empty hint — an error with nothing to do about it is half an error", what)
	}

	// No raw exit code alone, in any of the three places a person can see.
	for _, field := range []struct{ name, value string }{
		{"Summary()", summary},
		{"Hint()", hint},
		{"Error()", e.Error()},
	} {
		if errTestBareNumber.MatchString(field.value) {
			t.Errorf("%s: %s is a bare exit code: %q", what, field.name, field.value)
		}
		if !errTestLetters.MatchString(field.value) {
			t.Errorf("%s: %s carries no words at all: %q", what, field.name, field.value)
		}
		if field.value == strconv.Itoa(exitCode) {
			t.Errorf("%s: %s is just the exit code %d", what, field.name, exitCode)
		}
	}

	// Error() must render the summary, not degrade to "exit status N".
	if !strings.Contains(e.Error(), summary) {
		t.Errorf("%s: Error() %q does not contain the summary %q", what, e.Error(), summary)
	}
	if strings.Contains(strings.ToLower(e.Error()), "exit status") && !strings.Contains(summary, "exit status") {
		t.Errorf("%s: Error() leaked an exec-style status: %q", what, e.Error())
	}

	// Cobra's usage block belongs in the drawer, never in the message.
	for _, needle := range []string{"Usage:", "Global Flags:", "Available Commands:", "--json-events string"} {
		if strings.Contains(summary, needle) || strings.Contains(hint, needle) {
			t.Errorf("%s: usage block leaked into the message (%q): summary=%q hint=%q",
				what, needle, summary, hint)
		}
	}

	// Nothing the operator reads may carry an embedded credential.
	if strings.Contains(summary, "@") && strings.Contains(summary, "://") {
		t.Errorf("%s: summary carries a URL with userinfo: %q", what, summary)
	}

	// Well-formed text only. A partial rune in a dialog is a bug report about
	// the app rather than about the failure.
	if !utf8.ValidString(summary) || !utf8.ValidString(hint) {
		t.Errorf("%s: summary or hint is not valid UTF-8", what)
	}

	// Bounded. stderr is attacker-adjacent (it can quote a URL an operator
	// pasted), so a message must not be able to grow without limit.
	if n := utf8.RuneCountInString(summary); n > errMaxSummaryRunes {
		t.Errorf("%s: summary is %d runes, over the %d cap", what, n, errMaxSummaryRunes)
	}
	if n := utf8.RuneCountInString(hint); n > errMaxHintRunes {
		t.Errorf("%s: hint is %d runes, over the %d cap", what, n, errMaxHintRunes)
	}

	// The class must be one the UI can branch on, and the exit code must be
	// preserved for logs.
	if e.ExitCode() != exitCode {
		t.Errorf("%s: ExitCode() = %d, want %d", what, e.ExitCode(), exitCode)
	}
	if e.ClassName() == "" || strings.HasPrefix(e.ClassName(), "unknown(") {
		t.Errorf("%s: unusable class name %q", what, e.ClassName())
	}
}

// ---------------------------------------------------------------------------
// Exhaustive over the frozen class table
// ---------------------------------------------------------------------------

// errTestStderrForClass is realistic stderr for one class — the same text
// internal/cliadapter/fake's failure constructors carry. See the note at the
// top of this file for why it is mirrored rather than imported, and
// TestClassifyFakeCorpusHasNotDrifted for the drift guard.
func errTestStderrForClass(c dferr.Class) string {
	switch c {
	case dferr.Success:
		return ""
	case dferr.Usage:
		return "debark: build: --arch describes a --base; it has no meaning with --snapshot, which records the target's own architecture\n" +
			"\nUsage:\n  debark build [pkg...] [flags]\n\nFlags:\n" +
			"      --arch string   dpkg architecture the --base describes (default: this machine's)\n" +
			"      --base string   stock release to assume instead of a snapshot\n"
	case dferr.Environment:
		return "debark: build: no container runtime found and the local backend cannot resolve for a different release\n" +
			"install docker or podman, or run debark on a machine of the same release as the target and pass --backend=local\n"
	case dferr.Incomplete:
		// Empty on purpose: this is the silent-build case.
		return ""
	case dferr.Verification:
		return "debark: verify: pool/n/nginx/nginx_1.24.0-2ubuntu7.3_all.deb: digest mismatch\n" +
			"the bundle does not match its signed manifest; do not install it, and obtain a fresh copy\n"
	case dferr.Resolution:
		return "debark: apt: unable to satisfy the request: acme-agent has no installation candidate\n" +
			"check the package name against `debark` search results for this target, or add the vendor .deb as a URL\n"
	case dferr.Policy:
		return "debark: policy: require-signed-publisher: 1 input is url-unverified: https://downloads.example.com/acme-agent_4.2.0_amd64.deb\n" +
			"supply the vendor's expected SHA-256 for that download, or relax the rule in the policy file\n"
	case dferr.TargetMismatch:
		return "debark: install: this bundle is for ubuntu 24.04 (noble) amd64; this machine is debian 12 (bookworm) arm64\n" +
			"build a bundle from a snapshot of THIS machine\n"
	default:
		return ""
	}
}

// errTestArgvForClass is a plausible argv for the command each class comes out
// of, so the command-scoped rules are exercised too.
func errTestArgvForClass(c dferr.Class) []string {
	switch c {
	case dferr.Verification:
		return []string{ProgramName, "verify", "./bundle", "--json"}
	case dferr.TargetMismatch:
		return []string{ProgramName, "install", "./bundle", "--json"}
	default:
		return []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--out", "./bundle", "--json", "--", "apt:nginx"}
	}
}

// errTestGarbage is stderr that means nothing: control bytes, invalid UTF-8,
// a stray usage block, an unterminated escape sequence and a very long word.
const errTestGarbage = "\x00\x01\x07\x1b[31m\xff\xfe\xfd garbage \x1b]0;title" +
	"\nUsage:\n  nonsense\n" +
	"\n\xc3\x28 lone continuation \xed\xa0\x80 surrogate\n" +
	"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz\n"

// TestClassifyEveryClassIsActionable is the definition-of-done test. It loops
// the real dferr.Classes(), so a class added to the frozen table cannot be
// forgotten here.
func TestClassifyEveryClassIsActionable(t *testing.T) {
	stderrs := []struct{ name, text string }{
		{"empty", ""},
		{"realistic", ""}, // filled per class below
		{"garbage", errTestGarbage},
		{"whitespace", "   \n\t\n  \n"},
		{"usage-block-only", "Usage:\n  debark build [pkg...] [flags]\n\nFlags:\n  -h, --help\n"},
	}

	for _, class := range dferr.Classes() {
		for _, s := range stderrs {
			text := s.text
			if s.name == "realistic" {
				text = errTestStderrForClass(class)
			}
			name := fmt.Sprintf("%s/%s", class, s.name)
			t.Run(name, func(t *testing.T) {
				e := classify(errTestArgvForClass(class), int(class), text)
				errTestInvariants(t, name, e, int(class))
				if e.Class() != class {
					t.Errorf("class = %v, want %v", e.Class(), class)
				}
				// Two sentences, in two fields, both saying something.
				if len(e.Summary()) < 12 {
					t.Errorf("summary is too short to be useful: %q", e.Summary())
				}
				if len(e.Hint()) < 20 {
					t.Errorf("hint is too short to be an action: %q", e.Hint())
				}
			})
		}
	}
}

// TestClassifyClassTextsCoverEveryClass makes adding a class to dferr a
// visible, failing decision here rather than a silent fall-through to
// Class.Description().
func TestClassifyClassTextsCoverEveryClass(t *testing.T) {
	for _, class := range dferr.Classes() {
		txt, ok := errClassTexts[class]
		if !ok {
			t.Errorf("dferr class %v has no entry in errClassTexts; write one", class)
			continue
		}
		if txt.summary == "" || txt.hint == "" {
			t.Errorf("errClassTexts[%v] is half-written: %+v", class, txt)
		}
	}
	if len(errClassTexts) != len(dferr.Classes()) {
		t.Errorf("errClassTexts has %d entries, dferr.Classes() has %d", len(errClassTexts), len(dferr.Classes()))
	}
}

// TestErrFloorIsTotal proves the fallback cannot be skipped: every class in a
// generous range, crossed with every exit code in a generous range, produces
// two non-empty sentences.
func TestErrFloorIsTotal(t *testing.T) {
	for class := dferr.Class(-3); class <= dferr.Class(12); class++ {
		for code := -5; code <= 300; code++ {
			summary, hint := errFloor(class, code)
			if strings.TrimSpace(summary) == "" || strings.TrimSpace(hint) == "" {
				t.Fatalf("errFloor(%v, %d) = (%q, %q); the floor must always answer", class, code, summary, hint)
			}
			if errTestBareNumber.MatchString(summary) {
				t.Fatalf("errFloor(%v, %d) summary is a bare number: %q", class, code, summary)
			}
		}
	}
	for _, code := range []int{0xC0000005, 0xC0000135, 0xC000013A, 0x7FFFFFFF, -2147483648} {
		summary, hint := errFloor(dferr.Environment, code)
		if strings.TrimSpace(summary) == "" || strings.TrimSpace(hint) == "" {
			t.Fatalf("errFloor(environment, %d) = (%q, %q)", code, summary, hint)
		}
	}
}

// ---------------------------------------------------------------------------
// Exit codes the frozen table does not cover
// ---------------------------------------------------------------------------

func TestClassifyExitCodesOutsideTheTable(t *testing.T) {
	argv := []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"}

	cases := []struct {
		name     string
		code     int
		stderr   string
		wantWord string // a word the summary must contain, "" for none
	}{
		{"eight", 8, "", "exit status 8"},
		{"one-hundred", 100, "", "exit status 100"},
		{"not-executable", 126, "", "could not be run"},
		{"not-found", 127, "", "could not be started"},
		{"shell-not-found-with-stderr", 127, "sh: 1: debark: not found\n", ""},
		{"sigkill-shell", 137, "", "signal 9"},
		{"two-fifty-five", 255, "", "exit status 255"},
		{"signal-killed", -1, "", "killed"},
		{"very-negative", -1073741819, "", "killed"},
		{"windows-access-violation", 0xC0000005, "", "access violation"},
		{"windows-missing-dll", 0xC0000135, "", "DLL"},
		{"windows-ctrl-c", 0xC000013A, "", "interrupted"},
		{"windows-unknown-status", 0xC0000374, "", "0xC0000374"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := classify(argv, tc.code, tc.stderr)
			errTestInvariants(t, tc.name, e, tc.code)

			// Every out-of-table code is an environment fact, not a usage
			// error. dferr.ClassOf would say Usage; across a process boundary
			// that is wrong, and this is the decision recorded in
			// docs/dev/cli-surface.md §6.
			if e.Class() != dferr.Environment {
				t.Errorf("class = %v, want environment for exit %d", e.Class(), tc.code)
			}
			if tc.wantWord != "" && !strings.Contains(e.Summary(), tc.wantWord) {
				t.Errorf("summary %q does not mention %q", e.Summary(), tc.wantWord)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The silent-failure gap
// ---------------------------------------------------------------------------

// TestClassifySilentFailuresStillSaySomething is the hardest case in the
// brief: a failing `build` writes NOTHING to stderr, because renderBuildResult
// prints the JSON result and then returns a silenced error. Verified against a
// real binary, `verify` does the same. The exit class is all there is.
func TestClassifySilentFailuresStillSaySomething(t *testing.T) {
	cases := []struct {
		name  string
		argv  []string
		code  dferr.Class
		words []string // all must appear somewhere in summary+hint
	}{
		{
			name: "build/incomplete", argv: []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"},
			code: dferr.Incomplete, words: []string{"not complete", "report"},
		},
		{
			name: "build/resolution", argv: []string{ProgramName, "build", "--snapshot", "t.tar.zst", "--json"},
			code: dferr.Resolution, words: []string{"apt could not satisfy", "catalogue"},
		},
		{
			name: "build/policy", argv: []string{ProgramName, "build", "--base", "debian:13/server", "--policy", "p.yaml", "--json"},
			code: dferr.Policy, words: []string{"policy", "rule"},
		},
		{
			name: "build/verification", argv: []string{ProgramName, "build", "--base", "debian:13/server", "--json"},
			code: dferr.Verification, words: []string{"digest", "report"},
		},
		{
			name: "build/environment", argv: []string{ProgramName, "build", "--base", "debian:13/server", "--json"},
			code: dferr.Environment, words: []string{"could not run", "container runtime"},
		},
		{
			name: "verify/verification", argv: []string{ProgramName, "verify", "./bundle", "--json"},
			code: dferr.Verification, words: []string{"did not pass verification", "Do not install"},
		},
		{
			name: "install/target-mismatch", argv: []string{ProgramName, "install", "./bundle", "--json"},
			code: dferr.TargetMismatch, words: []string{"not built for this machine", "snapshot"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := classify(tc.argv, int(tc.code), "")
			errTestInvariants(t, tc.name, e, int(tc.code))

			both := e.Summary() + "\n" + e.Hint()
			for _, w := range tc.words {
				if !strings.Contains(both, w) {
					t.Errorf("silent %s failure says nothing about %q:\n  summary: %s\n  hint:    %s",
						tc.name, w, e.Summary(), e.Hint())
				}
			}
			// A silent failure must not fall back to dferr's raw table entry:
			// that is a manual-page line, not something to act on.
			if e.Summary() == e.Class().Description() {
				t.Errorf("silent %s failure degraded to the raw class description", tc.name)
			}
		})
	}
}

// TestClassifyUnrecognisedSilentFailureStillPointsSomewhere covers the last
// row of the table: a command with no rule of its own that exits silently
// still gets a hint saying where the detail actually is.
func TestClassifyUnrecognisedSilentFailureStillPointsSomewhere(t *testing.T) {
	e := classify([]string{ProgramName, "store", "gc", "./bundle", "--json"}, int(dferr.Policy), "")
	errTestInvariants(t, "store gc silent", e, int(dferr.Policy))
	if !strings.Contains(e.Hint(), "JSON document") {
		t.Errorf("hint does not explain where the detail is: %q", e.Hint())
	}
}

// ---------------------------------------------------------------------------
// The cobra usage block
// ---------------------------------------------------------------------------

func TestClassifyStripsUsageBlockButKeepsIt(t *testing.T) {
	raw := errTestReadSample(t, "usage-build-no-target.stderr")
	if !strings.Contains(raw, "Usage:") || len(raw) < 2000 {
		t.Fatalf("sample is not the ~2 KB usage-block failure it should be (%d bytes)", len(raw))
	}

	e := classify([]string{ProgramName, "build", "--json"}, 1, raw)
	errTestInvariants(t, "usage block", e, 1)

	if strings.Contains(e.Summary(), "Usage:") || strings.Contains(e.Hint(), "Usage:") {
		t.Errorf("usage block reached the message: summary=%q hint=%q", e.Summary(), e.Hint())
	}
	if !strings.Contains(e.Stderr(), "Usage:") || !strings.Contains(e.Stderr(), "--acknowledge-redistribution") {
		t.Error("the details drawer lost the usage block; that is where it belongs")
	}
	if e.Stderr() != raw {
		t.Error("Stderr() is not what the process actually wrote")
	}
	// And the summary is the useful half.
	if !strings.Contains(e.Summary(), "no target was chosen") {
		t.Errorf("summary did not recognise the missing-target failure: %q", e.Summary())
	}
}

func TestClassifyTruncatesOverlongStderr(t *testing.T) {
	head := "debark: verify: pool/n/nginx/nginx_1.24.0_all.deb: digest mismatch\n"
	raw := head + strings.Repeat("noise noise noise\n", MaxStderrBytes/6)
	if len(raw) <= MaxStderrBytes {
		t.Fatalf("test input is only %d bytes; it must exceed MaxStderrBytes (%d)", len(raw), MaxStderrBytes)
	}

	e := classify([]string{ProgramName, "verify", "./bundle", "--json"}, 4, raw)
	errTestInvariants(t, "overlong stderr", e, 4)

	if len(e.Stderr()) > MaxStderrBytes+len("\n… (truncated)") {
		t.Errorf("Stderr() is %d bytes, over the %d cap", len(e.Stderr()), MaxStderrBytes)
	}
	if !strings.Contains(e.Stderr(), "(truncated)") {
		t.Error("truncation was silent; it must be marked")
	}
	if !strings.Contains(e.Summary(), "nginx_1.24.0_all.deb") {
		t.Errorf("the message at the head of a huge stderr was lost: %q", e.Summary())
	}
}

// ---------------------------------------------------------------------------
// Stderr from failures that were deliberately caused
// ---------------------------------------------------------------------------

// The constants below are byte-exact stderr from a real debark, captured by
// T-ERRORS while causing each failure on purpose rather than by writing
// plausible text. Provenance, so the next person can re-cause them rather than
// trust them:
//
//	Binary:    debark built from the engine at commit 6549f6f
//	           ("Record that the container backend has now been run, from
//	           Windows"), CGO_ENABLED=0 GOOS=linux GOARCH=amd64, plus a native
//	           windows/amd64 build of the same commit for errTestDrivenNotELF.
//	Where:     docker 29.6.2, image debian:bookworm-slim, building
//	           `--base debian:12/minimal --arch amd64 -- apt:jq`, signed with a
//	           key the run generated. errTestDrivenNotELF is from the Windows
//	           host itself, which is the only place that failure exists.
//	When:      2026-09-06/07.
//	Method:    docs/dev/error-catalogue.md records the exact invocation and the
//	           conditions for each — --network none, a 256 KiB tmpfs at the
//	           output folder, an unroutable port, a binary built from debark
//	           `10ec7bf` for the two "too old" rows, and so on.
//
// Three of them postdate debark `3ed80bd`, which removed silentError:
// errTestDrivenFetchFailed, errTestDrivenTampered and errTestDrivenUntrusted
// are all cases that wrote ZERO bytes to stderr before that commit and are
// the reason the silent-* rows in errRules no longer fire against a current
// binary. See the "Silent failures" comment there.
//
// Where a sample ended in cobra's usage block it is elided after the first
// line of it, and the constant says so: classifyStderr discards everything
// from `Usage:` onwards before any rule sees it, so keeping two kilobytes of
// help text here would test nothing and hide the message it follows.
const (
	// build against a stock base with `--network none`. Also reproduced with
	// an unroutable http_proxy, byte for byte.
	errTestDrivenNoNetwork = "debark: apt: base seeds do not resolve against this release's archive: " +
		"apt (unable to locate package); init (unable to locate package)\n"

	// build --sign pointed at a path with no file. Usage block elided.
	errTestDrivenNoSigningKey = "debark: sign: read private key /work/absent.key: open /work/absent.key: no such file or directory\n" +
		// Quoted with " rather than the backticks debark printed: "debark key
		// generate" is not a command, and the repository-wide guard in
		// internal/cli forbids writing one in backticks even as a capture.
		// Nothing here parses the hint; the row below only proves it is dropped.
		"generate one with \"debark key generate\", or pass an existing .key file\n" +
		"\nUsage:\n  debark build [pkg...] [flags]\n"

	// build with a vendor URL on a closed port. The same line, differing only
	// in the URL, came back from a SHA-256 that did not match --digest and
	// from an https:// download with no trust anchor.
	errTestDrivenFetchFailed = "debark: build: /work/bundle: incomplete: 1 URL that did not download: " +
		"http://127.0.0.1:9/vendor-tool_1.0_amd64.deb\n" +
		"the bundle holds everything that did resolve; supply the missing input as a local .deb (--local-dir) " +
		"or drop it from the request, then re-run\n"

	// build with the output folder on a 256 KiB tmpfs.
	errTestDrivenNoSpace = "debark: bundle: materialise pool/libo/libonig5/libonig5_6.9.8-1_amd64.deb: " +
		"store: materialise 59ecfce6d88c7c4b09496ce182b3b8303e8e8477664e009b16ae83a09cd12be7: " +
		"store: copy to /out/bundle/repo/pool/libo/libonig5/.tmp-3313392646: " +
		"write /out/bundle/repo/pool/libo/libonig5/.tmp-3313392646: no space left on device\n"

	// build --snapshot with an Ubuntu 24.04 snapshot, inside a Debian 12
	// container with no container runtime of its own.
	errTestDrivenNoBackend = "debark: apt: auto backend unavailable: local: host distro \"debian\" does not match target distro \"ubuntu\"; " +
		"container: container: no container runtime found (tried docker, podman)\n" +
		"install Docker (https://docs.docker.com/engine/install/, e.g. `sudo apt-get install docker.io`) " +
		"or Podman (e.g. `sudo apt-get install podman`), then make sure `docker` or `podman` is on PATH\n"

	// build on Windows with Docker Desktop running Linux containers, and no
	// bin/debark-linux-amd64 beside the executable.
	errTestDrivenNotELF = `debark: container: C:\Users\operator\debark\debark.exe does not look like a Linux ELF binary ` +
		"(bad magic number '[77 90 144 0]' in record at byte 0x0)\n" +
		"build a static linux/amd64 debark and point --self-binary at it: " +
		"CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/debark-linux-amd64 ./cmd/debark " +
		"(bin/debark-linux-amd64 beside the debark you are running is found without any flag; " +
		"DEBARK_SELF_BINARY and the self_binary config key also work)\n"

	// snapshot inspect on a real snapshot cut to its first 4 KiB.
	errTestDrivenTruncatedSnapshot = "debark: snapshot: decompress /work/truncated.tar.zst: snapshot: zstd decode: unexpected EOF\n"

	// snapshot inspect on 200 KB of /dev/urandom named .tar.zst. A bare
	// snapshot.json and a directory produce the same shape, differing only in
	// the clause after the colon. Usage block elided.
	errTestDrivenNotASnapshot = "debark: snapshot: /work/garbage.tar.zst is not a debark snapshot: it does not begin with a zstd frame header\n" +
		"`debark snapshot create` on the target machine writes one; `debark snapshot from-base` synthesizes one from a stock release\n" +
		"\nUsage:\n  debark snapshot inspect FILE [flags]\n"

	// verify on a copy of a freshly built, signed bundle with one byte
	// appended to repo/Packages.
	errTestDrivenTampered = "debark: verify: /work/tampered failed verification: 1 problem: " +
		"repo/Packages: file size does not match the manifest\n"

	// verify on an untouched bundle whose operator public key is not present.
	errTestDrivenUntrusted = "debark: verify: /work/untrusted failed verification: 1 problem: " +
		"debark.manifest.sig: no signature verifies against a trusted key\n" +
		"the operator public key must reach this machine independently of the bundle's media: " +
		"pass it with --key, or list it under verify_keys in the config file\n"

	// verify on a bundle the build screen was told to write UNSIGNED, driven
	// end to end through the app: build with "Write an unsigned bundle,
	// deliberately", export it, press "Check the copy with debark verify".
	// Captured from debark at 5d26dac. Note that debark's own hint is
	// identical to the untrusted case above, and here there is no key to
	// bring: nothing signed this bundle.
	errTestDrivenUnsigned = "debark: verify: /out/c1/bundle failed verification: 1 problem: " +
		"debark.manifest.sig: bundle is not signed and AllowUnsigned was not given\n" +
		"the operator public key must reach this machine independently of the bundle's media: " +
		"pass it with --key, or list it under verify_keys in the config file\n"

	// verify on a REAL signed bundle with only debark.manifest.json removed.
	// This is the other half of core 5837d82's boundary and it stays at exit 4
	// on purpose: the folder is shaped like a bundle, the file that says what
	// it should contain is gone, and that is what tampering looks like.
	// Captured from debark at 5d26dac, driven through the export screen's
	// "Check the copy with debark verify" on a bundle this application had
	// just built, signed and exported.
	errTestDrivenManifestDeleted = "debark: verify: /sp/exp1 failed verification: 1 problem: " +
		"debark.manifest.json: no debark.manifest.json in this bundle\n"

	// verify on a directory that has never been near debark, at the exit
	// code core 5837d82 moved it to: 1 (usage), zero bytes on stdout, usage
	// block on stderr. Captured from debark at 5d26dac.
	errTestDrivenNotABundle = "debark: verify: /tmp/notabundle is not a debark bundle: it has no " +
		"debark.manifest.json and none of a bundle's other parts\n" +
		"`debark build` writes a bundle; point verify at the directory it names, not at the media root " +
		"or a folder beside it\n\nUsage:\n  debark verify BUNDLE [flags]\n"

	// Both from a debark built from commit 10ec7bf, the commit before
	// `snapshot list-bases` and `build --base` existed. Usage blocks elided —
	// and the first one's block is the SNAPSHOT GROUP's, which is the whole
	// tell: cobra ran the parent command (cli-surface.md C6).
	errTestDrivenOldListBases = "debark: unknown flag: --arch\n\nUsage:\n  debark snapshot [command]\n"
	errTestDrivenOldBuildBase = "debark: unknown flag: --base\n\nUsage:\n  debark build [pkg...] [flags]\n"
)

// errTestBuildArgv is the argv the application actually produced for the runs
// above, so the command-scoped rules are matched against the real thing.
var errTestBuildArgv = []string{
	ProgramName, "build", "--base", "debian:12/minimal", "--arch", "amd64",
	"--out", "/work/bundle", "--sign", "/work/operator.key", "--json", "--", "apt:jq",
}

// ---------------------------------------------------------------------------
// Recognised failures
// ---------------------------------------------------------------------------

func TestClassifyRecognisedFailures(t *testing.T) {
	cases := []struct {
		name        string
		argv        []string
		code        int
		stderr      string
		wantSummary string   // exact
		wantInHint  []string // substrings
		wantNotIn   []string // must appear in neither summary nor hint
	}{
		{
			name:   "resolution names one package",
			argv:   []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"},
			code:   5,
			stderr: "debark: apt: unable to satisfy the request: acme-agent has no installation candidate\ncheck the package name against `debark` search results for this target, or add the vendor .deb as a URL\n",
			// The rule names the package; debark's own hint survives, because
			// the code that produced the failure knew more than the rule does.
			wantSummary: "apt has no installation candidate for acme-agent on this target",
			wantInHint:  []string{"check the package name"},
		},
		{
			name:        "resolution names several packages",
			argv:        []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"},
			code:        5,
			stderr:      "debark: apt: unable to satisfy the request: acme-agent, zztop-driver, libfoo1 have no installation candidate\n",
			wantSummary: "apt has no installation candidate for acme-agent, zztop-driver, libfoo1 on this target",
			wantInHint:  []string{"catalogue for this target"},
		},
		{
			name:        "resolution caps a very long package list",
			argv:        []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"},
			code:        5,
			stderr:      "debark: apt: unable to satisfy the request: a1, a2, a3, a4, a5, a6, a7 have no installation candidate\n",
			wantSummary: "apt has no installation candidate for a1, a2, a3, a4 and 3 more on this target",
		},
		{
			name:        "environment names the missing runtime",
			argv:        []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"},
			code:        2,
			stderr:      "debark: build: no container runtime found and the local backend cannot resolve for a different release\ninstall docker or podman, or run debark on a machine of the same release as the target and pass --backend=local\n",
			wantSummary: "no container runtime is available, and the local backend cannot resolve for a different release",
			wantInHint:  []string{"docker or podman"},
		},
		{
			name:        "environment: this host is not linux",
			argv:        []string{ProgramName, "snapshot", "create", "--out", "s.tar.zst", "--json"},
			code:        2,
			stderr:      "debark: snapshot: capturing the real system requires Linux (dpkg/apt); this host is windows\npoint CaptureOptions.Root at a fixture tree, or run debark on the target itself\n",
			wantSummary: "capturing the real system needs Linux with dpkg and apt, and this machine is windows",
			wantInHint:  []string{"on the target itself"},
			// The CLI's own hint names a Go struct field. That is right at a
			// terminal and useless in a window, so this rule replaces it.
			wantNotIn: []string{"CaptureOptions"},
		},
		{
			name:        "verification names the file",
			argv:        []string{ProgramName, "verify", "./bundle", "--json"},
			code:        4,
			stderr:      "debark: verify: pool/n/nginx/nginx_1.24.0-2ubuntu7.3_all.deb: digest mismatch\nthe bundle does not match its signed manifest; do not install it, and obtain a fresh copy\n",
			wantSummary: "…/nginx/nginx_1.24.0-2ubuntu7.3_all.deb does not match the digest recorded in the signed manifest",
			wantInHint:  []string{"do not install it"},
		},
		{
			name:        "policy names the rule and redacts the URL",
			argv:        []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--policy", "p.yaml", "--json"},
			code:        6,
			stderr:      "debark: policy: require-signed-publisher: 1 input is url-unverified: https://deploy:s3cr3t@downloads.example.com/private/acme-agent_4.2.0_amd64.deb?token=abc123\nsupply the vendor's expected SHA-256 for that download, or relax the rule in the policy file\n",
			wantSummary: "policy rule require-signed-publisher rejected the plan: the download from downloads.example.com/acme-agent_4.2.0_amd64.deb has no expected digest",
			wantInHint:  []string{"SHA-256"},
			wantNotIn:   []string{"s3cr3t", "deploy:", "token=abc123"},
		},
		{
			name:        "target mismatch names both sides",
			argv:        []string{ProgramName, "install", "./bundle", "--json"},
			code:        7,
			stderr:      "debark: install: this bundle is for ubuntu 24.04 (noble) amd64; this machine is debian 12 (bookworm) arm64\nbuild a bundle from a snapshot of THIS machine\n",
			wantSummary: "this bundle was built for ubuntu 24.04 (noble) amd64, and this machine is debian 12 (bookworm) arm64",
			wantInHint:  []string{"snapshot"},
		},
		{
			name:        "usage: unknown command is version skew",
			argv:        []string{ProgramName, "frobnicate", "--json"},
			code:        1,
			stderr:      errTestSampleOrSkip("usage-unknown-command.stderr"),
			wantSummary: "this debark has no `frobnicate` command",
			wantInHint:  []string{"version"},
		},
		{
			name:        "usage: unknown flag is version skew",
			argv:        []string{ProgramName, "verify", "./b", "--nonexistent-flag", "--json"},
			code:        1,
			stderr:      errTestSampleOrSkip("usage-unknown-flag.stderr"),
			wantSummary: "this debark does not accept the option --nonexistent-flag",
			wantInHint:  []string{"version"},
		},
		{
			name:        "usage: required flag",
			argv:        []string{ProgramName, "keygen", "--json"},
			code:        1,
			stderr:      errTestSampleOrSkip("usage-keygen-required-flag.stderr"),
			wantSummary: "debark needs the out option and it was not supplied",
		},
		{
			name:        "usage: build with two targets",
			argv:        []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--snapshot", "x.tar.zst", "--json"},
			code:        1,
			stderr:      errTestSampleOrSkip("usage-build-mutually-exclusive.stderr"),
			wantSummary: "a target was given twice: --base and --snapshot describe the same thing two different ways",
		},
		{
			name:   "usage: unknown architecture lists the known ones",
			argv:   []string{ProgramName, "snapshot", "list-bases", "--arch", "not-an-arch", "--json"},
			code:   1,
			stderr: errTestSampleOrSkip("usage-listbases-unknown-arch.stderr"),
			// debark's own line names the rejected value and every accepted
			// one; the rule adds the action and leaves the list alone.
			wantSummary: `base: unknown architecture "not-an-arch"; known architectures are amd64, arm64, armel, armhf, i386, ppc64el, riscv64, s390x`,
			wantInHint:  []string{"describes the target"},
		},
		{
			name:        "usage: unknown base",
			argv:        []string{ProgramName, "snapshot", "from-base", "not-a-base", "--json"},
			code:        1,
			stderr:      errTestSampleOrSkip("usage-frombase-unknown-base.stderr"),
			wantSummary: "not-a-base is not one of debark's builtin base IDs, and there is no file by that name",
			// debark's own hint lists fifteen base ids; the picker already
			// shows them, so the rule replaces it.
			wantNotIn: []string{"ubuntu:22.04/desktop"},
		},
		{
			name:        "not a bundle folder",
			argv:        []string{ProgramName, "inspect", "./emptydir", "--json"},
			code:        1,
			stderr:      errTestSampleOrSkip("usage-inspect-not-a-bundle.stderr"),
			wantSummary: "emptydir is not a debark bundle: it has no lock.json",
			wantInHint:  []string{"lock.json"},
		},
		{
			// The command-scoped snapshot/missing-file row now answers these
			// two, and says what the file was for rather than only that a
			// path is not there. The generic any/missing-file rows below it
			// still answer every other command; "missing file, generic" holds
			// them to it.
			name:        "missing snapshot file, windows",
			argv:        []string{ProgramName, "snapshot", "inspect", "./no-such.snapshot.json", "--json"},
			code:        1,
			stderr:      errTestSampleOrSkip("usage-snapshot-inspect-missing-file.stderr"),
			wantSummary: "there is no snapshot file at ./no-such.snapshot.json",
			wantInHint:  []string{"Choose the file again"},
		},
		{
			name:        "missing snapshot file, unix",
			argv:        []string{ProgramName, "snapshot", "inspect", "/srv/snapshots/target.tar.zst", "--json"},
			code:        1,
			stderr:      "debark: snapshot: open /srv/snapshots/target.tar.zst: no such file or directory\n\nUsage:\n  debark snapshot inspect FILE [flags]\n",
			wantSummary: "there is no snapshot file at …/snapshots/target.tar.zst",
		},
		{
			// The generic rows, on a command with no row of its own.
			name:        "missing file, generic",
			argv:        []string{ProgramName, "verify", "/srv/bundles/site-42", "--json"},
			code:        1,
			stderr:      "debark: verify: open /srv/bundles/site-42/lock.json: no such file or directory\n",
			wantSummary: "…/site-42/lock.json does not exist",
			wantInHint:  []string{"moved or renamed"},
		},
		{
			name:        "snapshot that is not zstd",
			argv:        []string{ProgramName, "snapshot", "inspect", "./bad.snapshot.json", "--json"},
			code:        4,
			stderr:      errTestSampleOrSkip("verification-snapshot-not-zstd.stderr"),
			wantSummary: "that file is not a debark snapshot: its contents are not a zstd archive",
			wantInHint:  []string{"snapshot create"},
		},
		{
			name:        "doctor with no target",
			argv:        []string{ProgramName, "doctor", "--json"},
			code:        1,
			stderr:      errTestSampleOrSkip("usage-doctor-no-target.stderr"),
			wantSummary: "doctor needs exactly one target: a bundle folder, or a snapshot file",
		},
		{
			name:        "config file will not parse",
			argv:        []string{ProgramName, "--config", "./bad-config.yaml", "config", "show", "--json"},
			code:        1,
			stderr:      errTestSampleOrSkip("usage-config-parse.stderr"),
			wantSummary: "the config file ./bad-config.yaml could not be read: yaml: mapping values are not allowed in this context",
		},
		{
			// environment/no-space-at now answers this, because a build writes
			// to two places on two possibly different drives and the path is
			// the only thing that says which one filled.
			name:        "disk full, and the message names where",
			argv:        []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"},
			code:        2,
			stderr:      "debark: fetch: write /srv/store/pool/n/nginx/nginx_1.24.0_amd64.deb: no space left on device\n",
			wantSummary: "the drive holding /srv/store/… filled up before the work finished",
			wantInHint:  []string{"Free space"},
		},
		{
			// And the generic row still catches a message with no path in it.
			name:        "disk full, with no path to name",
			argv:        []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"},
			code:        2,
			stderr:      "debark: bundle: assemble: not enough space to write the bundle\n",
			wantSummary: "the disk filled up before the work finished",
			wantInHint:  []string{"Free space"},
		},
		{
			name:        "docker daemon not reachable",
			argv:        []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"},
			code:        2,
			stderr:      "debark: backend: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n",
			wantSummary: "Docker is installed but its daemon is not reachable from this account",
			wantInHint:  []string{"docker group"},
		},

		// -- Driven for real. See errTestDriven* below for provenance. ------
		{
			name:        "no network: the base's own seeds are blamed",
			argv:        errTestBuildArgv,
			code:        5,
			stderr:      errTestDrivenNoNetwork,
			wantSummary: "the target's package indexes could not be read, so even the base system's own packages look missing",
			wantInHint:  []string{"network rather than the packages", "proxy"},
			// The failing seeds must not be repeated as though the operator
			// had chosen them: that is the whole defect this row closes.
			wantNotIn: []string{"Check the package names against the catalogue"},
		},
		{
			name:        "no signing key",
			argv:        errTestBuildArgv,
			code:        1,
			stderr:      errTestDrivenNoSigningKey,
			wantSummary: "the build was told to sign with the key at /work/absent.key, and there is no file there",
			wantInHint:  []string{"readiness screen"},
			// debark's own hint names a command that does not exist.
			wantNotIn: []string{"key generate"},
		},
		{
			name:        "a vendor download that failed",
			argv:        errTestBuildArgv,
			code:        3,
			stderr:      errTestDrivenFetchFailed,
			wantSummary: "the bundle at /work/bundle was written, but it is not complete: 1 URL that did not download: http://127.0.0.1:9/vendor-tool_1.0_amd64.deb",
			wantInHint:  []string{"details drawer", "SHA-256", "certificate"},
			// A flag is not an instruction anyone at a window can follow.
			wantNotIn: []string{"--local-dir"},
		},
		{
			name:        "the disk that filled is named",
			argv:        errTestBuildArgv,
			code:        2,
			stderr:      errTestDrivenNoSpace,
			wantSummary: "the drive holding /out/bundle/… filled up before the work finished",
			wantInHint:  []string{"two places"},
		},
		{
			name:        "neither backend is usable",
			argv:        errTestBuildArgv,
			code:        2,
			stderr:      errTestDrivenNoBackend,
			wantSummary: `neither way of resolving packages for that target works on this machine: apt here was refused (host distro "debian" does not match target distro "ubuntu"), and the container backend was refused (no container runtime found (tried docker, podman))`,
			wantInHint:  []string{"readiness screen", "release matches"},
		},
		{
			name:        "the container backend has no Linux debark to mount",
			argv:        errTestBuildArgv,
			code:        2,
			stderr:      errTestDrivenNotELF,
			wantSummary: "this build has to run apt inside a Linux container, and the only debark it can find to put in that container is …\\debark\\debark.exe, which is not a Linux program",
			wantInHint:  []string{"bin/debark-linux-"},
			// A `go build` command line is not a remedy for an operator.
			wantNotIn: []string{"CGO_ENABLED"},
		},
		{
			name:        "a snapshot cut short by a copy",
			argv:        []string{ProgramName, "snapshot", "inspect", "/work/truncated.tar.zst", "--json"},
			code:        4,
			stderr:      errTestDrivenTruncatedSnapshot,
			wantSummary: "that snapshot file is incomplete: it ends part-way through",
			wantInHint:  []string{"copy did not finish"},
			// There is no bundle anywhere in this story.
			wantNotIn: []string{"Do not install a bundle"},
		},
		{
			name:   "a file that is not a snapshot at all",
			argv:   []string{ProgramName, "snapshot", "inspect", "/work/garbage.tar.zst", "--json"},
			code:   1,
			stderr: errTestDrivenNotASnapshot,
			// debark's own summary already says which of the three
			// structural cases it was, so only the hint is replaced.
			wantSummary: "snapshot: /work/garbage.tar.zst is not a debark snapshot: it does not begin with a zstd frame header",
			wantInHint:  []string{"target screen"},
			wantNotIn:   []string{"from-base"},
		},
		{
			name:        "a bundle file that is not the one that was signed",
			argv:        []string{ProgramName, "verify", "/work/tampered", "--json"},
			code:        4,
			stderr:      errTestDrivenTampered,
			wantSummary: "repo/Packages inside the bundle is not the file that was signed",
			wantInHint:  []string{"media it travelled on"},
		},
		{
			name:        "a signature nothing here trusts",
			argv:        []string{ProgramName, "verify", "/work/untrusted", "--json"},
			code:        4,
			stderr:      errTestDrivenUntrusted,
			wantSummary: "verify: /work/untrusted failed verification: 1 problem: debark.manifest.sig: no signature verifies against a trusted key",
			wantInHint:  []string{"readiness screen"},
			wantNotIn:   []string{"verify_keys"},
		},
		{
			// The application built this bundle unsigned, on purpose, and said
			// so twice on the way. Being told afterwards to go and find a
			// public key is an instruction that cannot be followed.
			name:        "a bundle that was never signed at all",
			argv:        []string{ProgramName, "verify", "/out/c1/bundle", "--json"},
			code:        4,
			stderr:      errTestDrivenUnsigned,
			wantSummary: "this bundle carries no signature, and a bundle has to be signed for anything to check who produced it",
			wantInHint:  []string{"no key to add", "allow-unsigned"},
			wantNotIn:   []string{"verify_keys"},
		},
		{
			// The other side of the same boundary, and the reason the row
			// above it had to stop saying "you picked the wrong folder": at
			// exit 4 they did not. The folder IS the bundle; its manifest has
			// been removed. Sending them one level in would be sending them
			// somewhere that does not exist, and calling it a wrong folder
			// hides the only thing that matters, which is that nothing in it
			// can be checked any more.
			name:        "a real bundle whose manifest was deleted",
			argv:        []string{ProgramName, "verify", "/sp/exp1", "--json"},
			code:        4,
			stderr:      errTestDrivenManifestDeleted,
			wantSummary: "this bundle has no debark.manifest.json — the one file that says what it should contain",
			wantInHint:  []string{"Do not install it", "stopped part-way"},
			wantNotIn:   []string{"folder above it", "is not a debark bundle"},
		},
		{
			// Exit 1, not 4: they picked the wrong folder, and the usage class
			// floor would have told them to check for version skew instead.
			name:        "a folder that is not a bundle at all",
			argv:        []string{ProgramName, "verify", "/tmp/notabundle", "--json"},
			code:        1,
			stderr:      errTestDrivenNotABundle,
			wantSummary: "verify: /tmp/notabundle is not a debark bundle: it has no debark.manifest.json and none of a bundle's other parts",
			wantInHint:  []string{"bundle folder itself"},
			wantNotIn:   []string{"debark version"},
		},
		{
			name:        "a debark with no list-bases",
			argv:        []string{ProgramName, "snapshot", "list-bases", "--arch", "amd64", "--json", "--no-color"},
			code:        1,
			stderr:      errTestDrivenOldListBases,
			wantSummary: "this debark is older than this application: it has no `snapshot list-bases`, so it cannot offer stock base targets",
			wantInHint:  []string{"snapshot file"},
		},
		{
			name:        "a debark that cannot build from a base",
			argv:        errTestBuildArgv,
			code:        1,
			stderr:      errTestDrivenOldBuildBase,
			wantSummary: "this debark is too old to build against a stock base OS: it can only build from a snapshot",
			wantInHint:  []string{"snapshot file"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.stderr == errTestSampleMissing {
				t.Skip("captured sample is not present")
			}
			e := classify(tc.argv, tc.code, tc.stderr)
			errTestInvariants(t, tc.name, e, tc.code)

			if e.Summary() != tc.wantSummary {
				t.Errorf("summary\n  got:  %q\n  want: %q", e.Summary(), tc.wantSummary)
			}
			for _, w := range tc.wantInHint {
				if !strings.Contains(e.Hint(), w) {
					t.Errorf("hint %q does not contain %q", e.Hint(), w)
				}
			}
			for _, w := range tc.wantNotIn {
				if strings.Contains(e.Summary(), w) || strings.Contains(e.Hint(), w) {
					t.Errorf("%q leaked into the message: summary=%q hint=%q", w, e.Summary(), e.Hint())
				}
			}
		})
	}
}

// TestClassifyDegradesRatherThanGuesses is the other half of the rule table's
// contract: near-miss text must NOT be forced through a rule. A wrong sentence
// stated confidently is worse than a correct generic one.
func TestClassifyDegradesRatherThanGuesses(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		stderr string
	}{
		{"resolution, unfamiliar wording", 5, "debark: apt: the solver gave up after 40 iterations\n"},
		{"policy, no rule name", 6, "debark: the plan was refused\n"},
		{"verification, no file named", 4, "debark: verify: something did not add up\n"},
		{"environment, unfamiliar wording", 2, "debark: backend: the sandbox refused to start\n"},
		{"empty group would leave a hole", 5, "debark: apt: unable to satisfy the request:  has no installation candidate\n"},
	}

	argv := []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := classify(argv, tc.code, tc.stderr)
			errTestInvariants(t, tc.name, e, tc.code)

			// It must not have invented a template hole.
			if strings.Contains(e.Summary(), "$") || strings.Contains(e.Hint(), "$") {
				t.Errorf("an unexpanded template reached the operator: %q / %q", e.Summary(), e.Hint())
			}
			if strings.Contains(e.Summary(), " for  ") || strings.HasSuffix(e.Summary(), " ") {
				t.Errorf("a rule fired with an empty capture: %q", e.Summary())
			}
			// And it must still be actionable: the class floor is the answer.
			if strings.TrimSpace(e.Hint()) == "" {
				t.Error("degraded to no hint at all")
			}
		})
	}

	// Specifically: the empty-package-name case must fall through to the class
	// floor, not to "no installation candidate for  on this target".
	e := classify(argv, 5, "debark: apt: unable to satisfy the request:  has no installation candidate\n")
	if strings.Contains(e.Summary(), "installation candidate for  ") {
		t.Errorf("a rule with an empty capture was allowed to fire: %q", e.Summary())
	}
}

// ---------------------------------------------------------------------------
// Real stderr, from a real binary
// ---------------------------------------------------------------------------

const errTestSampleDir = "testdata/stderr"

// errTestSampleMissing marks a table row whose sample could not be read, so
// the row skips instead of failing on a machine with an incomplete checkout.
const errTestSampleMissing = "\x00missing-sample\x00"

func errTestSampleOrSkip(name string) string {
	b, err := os.ReadFile(filepath.Join(errTestSampleDir, name))
	if err != nil {
		return errTestSampleMissing
	}
	return string(b)
}

func errTestReadSample(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(errTestSampleDir, name))
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	return string(b)
}

// errTestSample is one row of testdata/stderr/INDEX.txt.
type errTestSample struct {
	file   string
	exit   int
	argv   []string
	stderr string
}

// errTestLoadSamples reads INDEX.txt, which records the exact invocation that
// produced each captured file. Keeping the invocation next to the bytes is what
// makes the corpus re-checkable rather than folklore.
func errTestLoadSamples(t *testing.T) []errTestSample {
	t.Helper()
	index, err := os.ReadFile(filepath.Join(errTestSampleDir, "INDEX.txt"))
	if err != nil {
		t.Fatalf("read INDEX.txt: %v", err)
	}
	var out []errTestSample
	for lineNo, line := range strings.Split(string(index), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		var cols []string
		for _, f := range fields {
			if strings.TrimSpace(f) != "" {
				cols = append(cols, strings.TrimSpace(f))
			}
		}
		if len(cols) != 3 {
			t.Fatalf("INDEX.txt:%d: want 3 tab-separated columns, got %d: %q", lineNo+1, len(cols), line)
		}
		code, err := strconv.Atoi(cols[1])
		if err != nil {
			t.Fatalf("INDEX.txt:%d: bad exit code %q", lineNo+1, cols[1])
		}
		body, err := os.ReadFile(filepath.Join(errTestSampleDir, cols[0]))
		if err != nil {
			t.Fatalf("INDEX.txt:%d: %v", lineNo+1, err)
		}
		out = append(out, errTestSample{
			file:   cols[0],
			exit:   code,
			argv:   strings.Fields(cols[2]),
			stderr: string(body),
		})
	}
	if len(out) < 15 {
		t.Fatalf("INDEX.txt lists only %d samples; the corpus was meant to be broader", len(out))
	}
	return out
}

// TestClassifyRealCapturedStderr runs every sample captured from the real
// binary through classify and asserts the invariants. It is the test that
// would have caught a mapping written against imagined output.
func TestClassifyRealCapturedStderr(t *testing.T) {
	for _, s := range errTestLoadSamples(t) {
		t.Run(s.file, func(t *testing.T) {
			e := classify(s.argv, s.exit, s.stderr)
			errTestInvariants(t, s.file, e, s.exit)

			// Nothing from a real usage block may survive into the message.
			for _, line := range strings.Split(e.Summary()+"\n"+e.Hint(), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "-h, --help") {
					t.Errorf("help text reached the message: %q", line)
				}
			}
			// The drawer keeps what the process wrote, byte for byte.
			if e.Stderr() != s.stderr {
				t.Error("Stderr() is not the captured bytes")
			}
			t.Logf("exit %d %v\n  summary: %s\n  hint:    %s", s.exit, s.argv, e.Summary(), e.Hint())
		})
	}
}

// TestClassifyRealSilentVerifyFailure pins the finding this package made against
// the real binary: `verify` is a second silent-error command, not just `build`.
// If a future debark starts printing a message there, this test still passes
// — but the zero-byte sample stops being reproducible, and INDEX.txt says how
// it was produced so the next person can check.
func TestClassifyRealSilentVerifyFailure(t *testing.T) {
	raw := errTestReadSample(t, "verification-verify-silent.stderr")
	if raw != "" {
		t.Fatalf("the sample is meant to be empty; it is %d bytes", len(raw))
	}
	e := classify([]string{ProgramName, "verify", "./emptydir", "--json"}, 4, raw)
	errTestInvariants(t, "silent verify", e, 4)
	if !strings.Contains(e.Summary(), "did not pass verification") {
		t.Errorf("summary: %q", e.Summary())
	}
	if !strings.Contains(e.Hint(), "verification report") {
		t.Errorf("hint: %q", e.Hint())
	}
}

// ---------------------------------------------------------------------------
// A program that is not debark
// ---------------------------------------------------------------------------

// TestClassifyRecognisesAnImpostor covers the failure an operator causes by
// typing the wrong path into settings, which used to render as whatever the
// other program happened to complain about.
//
// Both stderr samples are real. git.exe and where.exe were each run as
// `debark version --json --no-color` on the Windows host, through the
// application's own Probe.
func TestClassifyRecognisesAnImpostor(t *testing.T) {
	version := []string{ProgramName, "version", "--json", "--no-color"}
	cases := []struct {
		name       string
		argv       []string
		code       int
		stderr     string
		wantInMsg  []string
		wantNotIn  []string
		wantImpost bool
	}{
		{
			// Out of the frozen table, and in another program's voice.
			name: "git.exe", argv: version, code: 129,
			stderr:     "error: unknown option `json'\nusage: git version [--build-options]\n\n    --[no-]build-options  also print build options\n",
			wantInMsg:  []string{"does not answer like debark", "status 129", "unknown option"},
			wantImpost: true,
		},
		{
			// Exit 1 is INSIDE the frozen table, so only the command saves
			// this one: `version` is the command debark cannot fail.
			name: "where.exe", argv: version, code: 1,
			stderr:     "INFO: Could not find files for the given pattern(s).\r\n",
			wantInMsg:  []string{"does not answer like debark", "status 1"},
			wantNotIn:  []string{"mismatch between this app"},
			wantImpost: true,
		},
		{
			// A real debark, too old to know --no-color, still speaks in
			// debark's voice and must keep its own message.
			name: "an ancient but real debark", argv: version, code: 1,
			stderr:    "debark: unknown flag: --no-color\n",
			wantInMsg: []string{"does not accept the option --no-color"},
			wantNotIn: []string{"does not answer like"},
		},
		{
			// A build that failed in debark's own voice, out of the table.
			// Nothing here says "impostor": the voice is right.
			name: "a real debark killed late", argv: errTestBuildArgv, code: -1,
			stderr:    "debark: build: interrupted\n",
			wantNotIn: []string{"does not answer like"},
		},
		{
			// 127 already has a row in errExitTexts, and its hint is the right
			// one, so the recogniser stands aside. The summary here is the
			// shell's own line, which is accurate; note that this shape is
			// hypothetical for this application, because nothing runs debark
			// through a shell — procExec reports 127 with empty stderr, and
			// then the floor answers with both halves.
			name: "a shell that could not find it", argv: version, code: 127,
			stderr:    "sh: 1: debark: not found\n",
			wantInMsg: []string{"set the full path to the binary in settings"},
			wantNotIn: []string{"does not answer like"},
		},
		{
			// A build failing outside the table with foreign stderr is a
			// wrapper or a shell, not evidence about the binary's identity —
			// but the command is not `version`, so it still fires. That is
			// deliberate: any program that answers a debark command with a
			// status debark does not use is not being debark.
			name: "a wrapper script", argv: errTestBuildArgv, code: 200,
			stderr:     "wrapper: refusing to run builds outside working hours\n",
			wantInMsg:  []string{"does not answer like debark", "status 200"},
			wantImpost: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := classify(tc.argv, tc.code, tc.stderr)
			errTestInvariants(t, tc.name, e, tc.code)
			both := e.Summary() + " || " + e.Hint()
			for _, w := range tc.wantInMsg {
				if !strings.Contains(both, w) {
					t.Errorf("%q missing from\n  summary: %q\n  hint:    %q", w, e.Summary(), e.Hint())
				}
			}
			for _, w := range tc.wantNotIn {
				if strings.Contains(both, w) {
					t.Errorf("%q should not appear:\n  summary: %q\n  hint:    %q", w, e.Summary(), e.Hint())
				}
			}
			if tc.wantImpost && !strings.Contains(e.Hint(), "path in settings") {
				t.Errorf("an impostor must be blamed on the configured path; hint: %q", e.Hint())
			}
		})
	}
}

// TestErrNotDebarkStaysQuietWithoutEvidence holds the recogniser to its
// premise. Accusing a real debark of being an impostor would be a worse
// failure than the one it fixes, so every input with no positive evidence must
// leave it alone.
func TestErrNotDebarkStaysQuietWithoutEvidence(t *testing.T) {
	quiet := []struct {
		name   string
		code   int
		cmd    string
		region string
	}{
		{"nothing was printed", 129, "version", ""},
		{"debark's own voice", 129, "version", "debark: something went wrong"},
		{"debark's voice on a later line", 200, "build", "warning: slow\ndebark: build failed"},
		{"a status debark chose, on a command that can fail", 5, "build", "apt gave up"},
		{"a process that never started", -1, "version", "some text"},
		{"success", 0, "version", "some text"},
		{"an exit code with a better sentence", 127, "version", "sh: debark: not found"},
		{"a region with no words in it", 129, "version", "!!! ??? ***"},
	}
	for _, tc := range quiet {
		t.Run(tc.name, func(t *testing.T) {
			if s, h, ok := errNotDebark(tc.code, tc.cmd, tc.region); ok {
				t.Errorf("fired without evidence:\n  summary: %q\n  hint:    %q", s, h)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The commands where a class means something different
// ---------------------------------------------------------------------------

// TestClassifySnapshotFailuresNeverMentionBundles is the invariant behind the
// snapshot/* rows.
//
// `snapshot inspect` is the one command whose exit 4 is not about a bundle, and
// the Verification class floor is written entirely about bundles: "Do not
// install a bundle that fails verification. Obtain a fresh copy from the
// machine that built it, and check that the key you trust is the one it was
// signed with." A person who picked a half-copied snapshot in a file dialog was
// being told about signatures and installation, neither of which is in the
// story. Any future exit-4 shape out of this command must not reintroduce it.
func TestClassifySnapshotFailuresNeverMentionBundles(t *testing.T) {
	argv := []string{ProgramName, "snapshot", "inspect", "/work/x.tar.zst", "--json"}
	stderrs := []string{
		errTestDrivenTruncatedSnapshot,
		errTestDrivenNotASnapshot,
		"debark: snapshot: verify /work/x.tar.zst: digest does not match the recorded one\n",
		"debark: snapshot: read member: some future wording nobody has written yet\n",
		"",
	}
	for _, code := range []int{1, 4} {
		for _, s := range stderrs {
			e := classify(argv, code, s)
			errTestInvariants(t, "snapshot inspect", e, code)
			for _, banned := range []string{"install a bundle", "the machine that built it", "signed with"} {
				if strings.Contains(e.Hint(), banned) {
					t.Errorf("exit %d, stderr %q: bundle advice leaked into a snapshot failure: %q", code, s, e.Hint())
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The fake's failure constructors, end to end
// ---------------------------------------------------------------------------

// errTestFakeCorpus mirrors internal/cliadapter/fake's failure constructors.
// It is a copy and not an import because fake imports this package; an
// internal test cannot import it back. TestClassifyFakeCorpusHasNotDrifted is
// the guard against the copy going stale.
var errTestFakeCorpus = []struct {
	name string
	fn   string // the constructor in fake.go this row mirrors
	code int
	argv []string
}{
	{"SuccessFailure", "SuccessFailure", 0, []string{ProgramName, "version", "--json"}},
	{"UsageFailure", "UsageFailure", 1, []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"}},
	{"EnvironmentFailure", "EnvironmentFailure", 2, []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"}},
	{"IncompleteFailure", "IncompleteFailure", 3, []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"}},
	{"VerificationFailure", "VerificationFailure", 4, []string{ProgramName, "verify", "./bundle", "--json"}},
	{"ResolutionFailure", "ResolutionFailure", 5, []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"}},
	{"PolicyFailure", "PolicyFailure", 6, []string{ProgramName, "build", "--base", "ubuntu:24.04/server", "--json"}},
	{"TargetMismatchFailure", "TargetMismatchFailure", 7, []string{ProgramName, "install", "./bundle", "--json"}},
	{"BinaryMissingFailure", "BinaryMissingFailure", 127, []string{ProgramName, "version", "--json"}},
}

func TestClassifyFakeFailureConstructors(t *testing.T) {
	for _, tc := range errTestFakeCorpus {
		t.Run(tc.name, func(t *testing.T) {
			stderr := errTestStderrForClass(dferr.Class(tc.code))
			e := classify(tc.argv, tc.code, stderr)
			errTestInvariants(t, tc.name, e, tc.code)
			t.Logf("%s → %s | %s", tc.name, e.Summary(), e.Hint())
		})
	}
}

// TestClassifyFakeCorpusHasNotDrifted reads fake.go off disk and checks that
// the text mirrored into errTestStderrForClass is still the text the fake
// produces. It cannot import the package (that would be an import cycle), so it
// reads the source — which is enough to catch the failure mode that matters:
// the fake's realistic stderr being reworded while this file keeps testing the
// old wording.
func TestClassifyFakeCorpusHasNotDrifted(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("fake", "fake.go"))
	if err != nil {
		t.Skipf("fake.go not readable (%v); the drift guard needs the sibling source", err)
	}
	src := string(b)

	needles := []string{
		"no container runtime found and the local backend cannot resolve for a different release",
		"unable to satisfy the request: %s has no installation candidate",
		"digest mismatch",
		"url-unverified",
		"this bundle is for ubuntu 24.04 (noble) amd64; this machine is debian 12 (bookworm) arm64",
		"--arch describes a --base",
	}
	for _, n := range needles {
		if !strings.Contains(src, n) {
			t.Errorf("fake.go no longer contains %q; errTestStderrForClass is stale", n)
		}
	}
	// And every constructor this file claims to mirror still exists.
	for _, tc := range errTestFakeCorpus {
		if !strings.Contains(src, "func "+tc.fn+"(") {
			t.Errorf("fake.go has no %s; errTestFakeCorpus is stale", tc.fn)
		}
	}
}

// ---------------------------------------------------------------------------
// Hostile and malformed input
// ---------------------------------------------------------------------------

func TestClassifyNonUTF8StderrDoesNotCorruptTheMessage(t *testing.T) {
	cases := []struct{ name, stderr string }{
		{"lone continuation byte", "debark: verify: \xff\xfe bad bytes here\n"},
		{"truncated rune", "debark: apt: unable to satisfy the request: acme\xc3 has no installation candidate\n"},
		{"raw surrogate", "debark: policy: rule\xed\xa0\x80name: refused\n"},
		{"nul bytes", "debark: build:\x00\x00 something failed\n"},
		{"all invalid", "\xff\xff\xff\xff\xff\xff\xff\xff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := classify([]string{ProgramName, "build", "--json"}, 5, tc.stderr)
			errTestInvariants(t, tc.name, e, 5)
			if !utf8.ValidString(e.Summary()) {
				t.Errorf("summary is not valid UTF-8: %q", e.Summary())
			}
			if !utf8.ValidString(e.Hint()) {
				t.Errorf("hint is not valid UTF-8: %q", e.Hint())
			}
			if strings.ContainsRune(e.Summary(), 0) {
				t.Errorf("summary carries a NUL: %q", e.Summary())
			}
		})
	}
}

func TestClassifyRedactsCredentialsAndTokens(t *testing.T) {
	cases := []struct {
		name      string
		code      int
		stderr    string
		forbidden []string
	}{
		{
			name: "userinfo in a fetch failure", code: 3,
			stderr:    "debark: fetch: https://svc-account:hunter2@nexus.internal/repo/acme_1.0_amd64.deb: 401 Unauthorized\n",
			forbidden: []string{"hunter2", "svc-account"},
		},
		{
			name: "presigned query string", code: 3,
			stderr:    "debark: fetch: https://bucket.s3.amazonaws.com/acme.deb?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900: 403\n",
			forbidden: []string{"X-Amz-Signature", "deadbeefcafe"},
		},
		{
			name: "credential in a policy finding", code: 6,
			stderr:    "debark: policy: require-signed-publisher: 1 input is url-unverified: https://u:p@example.com/x.deb\n",
			forbidden: []string{"u:p@", "//u:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := classify([]string{ProgramName, "build", "--json"}, tc.code, tc.stderr)
			errTestInvariants(t, tc.name, e, tc.code)
			shown := e.Summary() + "\n" + e.Hint()
			for _, f := range tc.forbidden {
				if strings.Contains(shown, f) {
					t.Errorf("%q reached the operator: summary=%q hint=%q", f, e.Summary(), e.Hint())
				}
			}
		})
	}
}

// TestClassifySurvivesArbitraryInput throws structured noise at classify: every
// exit code in a wide range crossed with a set of hostile stderr bodies, plus
// random bytes. Nothing here checks wording — only that the contract holds.
func TestClassifySurvivesArbitraryInput(t *testing.T) {
	bodies := []string{
		"",
		"\n\n\n",
		"debark: ",
		"debark:",
		"Usage:",
		"Usage:\nFlags:\n",
		strings.Repeat("A", 200000),
		strings.Repeat("$1 $2 $9 $$ %s %d %v\n", 50),
		"debark: policy: : \n",
		"debark: apt: unable to satisfy the request: has no installation candidate\n",
		errTestGarbage,
	}
	argvs := [][]string{
		nil,
		{},
		{ProgramName},
		{ProgramName, "build"},
		{"/usr/lib/debark/debark", "--config", "build", "verify", "./x", "--json"},
		{ProgramName, "--", "build"},
	}

	for _, code := range []int{-1073741510, -1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 42, 126, 127, 128, 130, 137, 255, 256, 1000, 0xC0000005} {
		for bi, body := range bodies {
			for ai, argv := range argvs {
				what := fmt.Sprintf("code=%d body=%d argv=%d", code, bi, ai)
				e := classify(argv, code, body)
				errTestInvariants(t, what, e, code)
				if strings.Contains(e.Summary(), "%!") || strings.Contains(e.Hint(), "%!") {
					t.Fatalf("%s: a Printf verb was interpreted: %q / %q", what, e.Summary(), e.Hint())
				}
			}
		}
	}

	// And random bytes, deterministically seeded so a failure is reproducible.
	rng := rand.New(rand.NewSource(20260906))
	for i := 0; i < 400; i++ {
		buf := make([]byte, rng.Intn(2048))
		for j := range buf {
			buf[j] = byte(rng.Intn(256))
		}
		code := rng.Intn(600) - 100
		e := classify([]string{ProgramName, "build", "--json"}, code, string(buf))
		errTestInvariants(t, fmt.Sprintf("random/%d", i), e, code)
	}
}

// ---------------------------------------------------------------------------
// The table itself
// ---------------------------------------------------------------------------

// TestClassifyRuleTableStyle keeps the table reviewable: unique names, no
// template referring to a group its regexp cannot produce, no shape list
// longer than the groups it shapes, and one voice for summaries and hints.
func TestClassifyRuleTableStyle(t *testing.T) {
	seen := map[string]bool{}
	ref := regexp.MustCompile(`\$([1-9])`)

	for i, rule := range errRules {
		if rule.name == "" {
			t.Errorf("errRules[%d] has no name", i)
			continue
		}
		if seen[rule.name] {
			t.Errorf("errRules[%d]: duplicate rule name %q", i, rule.name)
		}
		seen[rule.name] = true

		if rule.match == nil {
			t.Errorf("%s: no matcher", rule.name)
			continue
		}
		groups := rule.match.NumSubexp()
		if len(rule.shapes) > groups {
			t.Errorf("%s: %d shapes for %d capture groups", rule.name, len(rule.shapes), groups)
		}
		for _, tmpl := range []string{rule.summary, rule.hint} {
			for _, m := range ref.FindAllStringSubmatch(tmpl, -1) {
				n, _ := strconv.Atoi(m[1])
				if n > groups {
					t.Errorf("%s: template refers to $%d but the regexp has %d groups", rule.name, n, groups)
				}
			}
		}
		if rule.summary == "" && rule.hint == "" {
			t.Errorf("%s: a rule that changes nothing", rule.name)
		}
		if rule.hintWins && rule.hint == "" {
			t.Errorf("%s: hintWins with no hint to win with", rule.name)
		}

		// House style. Summary is a clause; Error() renders it as
		// "<summary> (<class>)", so a trailing full stop reads wrong. Hint is
		// prose and is punctuated.
		if strings.HasSuffix(rule.summary, ".") {
			t.Errorf("%s: summary ends with a full stop: %q", rule.name, rule.summary)
		}
		if rule.hint != "" && !strings.HasSuffix(rule.hint, ".") {
			t.Errorf("%s: hint does not end with a full stop: %q", rule.name, rule.hint)
		}
		if rule.hint != "" && !strings.ContainsAny(rule.hint[:1], "ABCDEFGHIJKLMNOPQRSTUVWXYZ$") {
			t.Errorf("%s: hint does not start with a capital: %q", rule.name, rule.hint)
		}
		if rule.class != errClassAny && (rule.class < dferr.Success || rule.class > dferr.TargetMismatch) {
			t.Errorf("%s: class %v is not in the frozen table", rule.name, rule.class)
		}
		if rule.cmd != "" {
			found := false
			for _, c := range errCommands {
				if c == rule.cmd {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: cmd %q is not a debark command errCommands knows", rule.name, rule.cmd)
			}
		}
	}

	// The catch-all silent row must be last, or a specific silent rule added
	// after it would never fire.
	if last := errRules[len(errRules)-1]; last.name != "any/silent" {
		t.Errorf("the last rule is %q; the catch-all silent row must stay last", last.name)
	}
}

// TestClassifyClassTextsStyle applies the same voice check to the floor.
func TestClassifyClassTextsStyle(t *testing.T) {
	check := func(where, summary, hint string) {
		t.Helper()
		if strings.HasSuffix(summary, ".") {
			t.Errorf("%s: summary ends with a full stop: %q", where, summary)
		}
		if !strings.HasSuffix(hint, ".") {
			t.Errorf("%s: hint does not end with a full stop: %q", where, hint)
		}
		if errTestBareNumber.MatchString(summary) {
			t.Errorf("%s: summary is a bare number: %q", where, summary)
		}
	}
	for class, txt := range errClassTexts {
		check("errClassTexts["+class.String()+"]", txt.summary, txt.hint)
	}
	for code, txt := range errExitTexts {
		check(fmt.Sprintf("errExitTexts[%d]", code), txt.summary, txt.hint)
	}
}

// ---------------------------------------------------------------------------
// The pieces
// ---------------------------------------------------------------------------

func TestClassifyCommand(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{ProgramName}, ""},
		{[]string{ProgramName, "build", "--json"}, "build"},
		{[]string{ProgramName, "verify", "./bundle", "--json"}, "verify"},
		{[]string{ProgramName, "snapshot", "inspect", "s.tar.zst", "--json"}, "snapshot inspect"},
		{[]string{ProgramName, "snapshot", "list-bases", "--arch", "amd64", "--json"}, "snapshot list-bases"},
		{[]string{ProgramName, "store", "ls", "--json"}, "store ls"},
		{[]string{ProgramName, "--config", "/etc/df.yaml", "build", "--json"}, "build"},
		{[]string{"/usr/bin/debark", "install", "./b", "--json"}, "install"},
		{[]string{ProgramName, "build", "--json", "--", "apt:build"}, "build"},
		// A positional that happens to be a command name must not be picked up
		// from after the "--" separator.
		{[]string{ProgramName, "--json", "--", "apt:verify"}, ""},
		{[]string{ProgramName, "frobnicate", "--json"}, ""},
	}
	for _, tc := range cases {
		if got := classifyCommand(tc.argv); got != tc.want {
			t.Errorf("classifyCommand(%v) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

func TestClassifyStderrSplit(t *testing.T) {
	cases := []struct {
		name                  string
		in                    string
		region, summary, hint string
	}{
		{"empty", "", "", "", ""},
		{
			"message only",
			"debark: verify: digest mismatch\n",
			"debark: verify: digest mismatch", "verify: digest mismatch", "",
		},
		{
			"message and hint",
			"debark: build: no container runtime found\ninstall docker or podman\n",
			"debark: build: no container runtime found\ninstall docker or podman",
			"build: no container runtime found", "install docker or podman",
		},
		{
			"usage block cut",
			"debark: unknown flag: --nope\n\nUsage:\n  debark verify BUNDLE [flags]\n\nFlags:\n  -h, --help\n",
			"debark: unknown flag: --nope", "unknown flag: --nope", "",
		},
		{
			"usage block only",
			"Usage:\n  debark build\n",
			"", "", "",
		},
		{
			"crlf",
			"debark: a\r\nb\r\n\r\nUsage:\r\n  x\r\n",
			"debark: a\nb", "a", "b",
		},
		{
			"run-help line",
			"debark: bad thing\nRun 'debark --help' for usage.\n",
			"debark: bad thing", "bad thing", "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			region, summary, hint := classifyStderr(tc.in)
			if region != tc.region {
				t.Errorf("region\n  got:  %q\n  want: %q", region, tc.region)
			}
			if summary != tc.summary {
				t.Errorf("summary\n  got:  %q\n  want: %q", summary, tc.summary)
			}
			if hint != tc.hint {
				t.Errorf("hint\n  got:  %q\n  want: %q", hint, tc.hint)
			}
		})
	}
}

func TestErrShapeValue(t *testing.T) {
	cases := []struct {
		shape errShape
		in    string
		want  string
	}{
		{errShapeText, "  a   b\tc  ", "a b c"},
		{errShapeText, "", ""},
		{errShapePath, "nginx.deb", "nginx.deb"},
		{errShapePath, "pool/n/nginx/nginx_1.24_all.deb", "…/nginx/nginx_1.24_all.deb"},
		{errShapePath, `C:\Users\operator\bundles\acme`, `…\bundles\acme`},
		{errShapePath, "/srv/x", "/srv/x"},
		// errShapeMount answers the opposite question: which volume, not which
		// file. A leading separator survives, so an absolute path still looks
		// absolute.
		{errShapeMount, "/out/bundle/repo/pool/libo/libonig5/.tmp-3630954993", "/out/bundle/…"},
		{errShapeMount, "pool/n/nginx/nginx_1.24_all.deb", "pool/n/…"},
		{errShapeMount, `C:\Users\operator\bundles\acme`, `C:\Users\…`},
		{errShapeMount, "/srv/x", "/srv/x"},
		{errShapeMount, "bundle", "bundle"},
		{errShapeURL, "https://u:p@host.example.com/a/b/c.deb?sig=1#frag", "host.example.com/c.deb"},
		{errShapeURL, "https://host.example.com/", "host.example.com"},
		{errShapeURL, "not a url at all", "not a url at all"},
		{errShapeList, "a, b, b, c", "a, b, c"},
		{errShapeList, "a b c d e f", "a, b, c, d and 2 more"},
		{errShapeList, "snapshot base interactive", "snapshot, base, interactive"},
		{errShapeList, "   ", ""},
	}
	for _, tc := range cases {
		if got := errShapeValue(tc.shape, tc.in); got != tc.want {
			t.Errorf("errShapeValue(%d, %q) = %q, want %q", tc.shape, tc.in, got, tc.want)
		}
	}

	// Every shape clips.
	long := strings.Repeat("x", errMaxFieldRunes*3)
	for _, shape := range []errShape{errShapeText, errShapePath, errShapeMount, errShapeURL, errShapeList} {
		if n := utf8.RuneCountInString(errShapeValue(shape, long)); n > errMaxFieldRunes {
			t.Errorf("shape %d did not clip: %d runes", shape, n)
		}
	}
}

func TestErrExpand(t *testing.T) {
	vals := []string{"one", "two"}
	cases := []struct{ tmpl, want string }{
		{"", ""},
		{"no refs", "no refs"},
		{"$1", "one"},
		{"$1 and $2", "one and two"},
		{"$3", ""},
		{"$$1", "$1"},
		// $5 is a group reference like any other, so a table entry that wants a
		// literal dollar has to write $$. Pinned so nobody adds one by accident.
		{"costs $5.00", "costs .00"},
		{"trailing $", "trailing $"},
		{"$a", "$a"},
	}
	for _, tc := range cases {
		if got := errExpand(tc.tmpl, vals); got != tc.want {
			t.Errorf("errExpand(%q) = %q, want %q", tc.tmpl, got, tc.want)
		}
	}
}

func TestErrClipCountsRunesNotBytes(t *testing.T) {
	s := strings.Repeat("é", 40)
	got := errClip(s, 10)
	if n := utf8.RuneCountInString(got); n > 11 { // 10 plus the ellipsis
		t.Errorf("clip produced %d runes: %q", n, got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("clip cut a rune in half: %q", got)
	}
	if errClip("  spaced  ", 100) != "spaced" {
		t.Error("clip did not trim")
	}
	if errClip("a\nb\tc", 100) != "a b c" {
		t.Errorf("clip did not fold whitespace: %q", errClip("a\nb\tc", 100))
	}
}

func TestErrCleanTextStripsEscapesAndControls(t *testing.T) {
	in := "\x1b[31mdebark:\x1b[0m \x07red\x00 text\r\nsecond\r"
	got := errCleanText(in)
	if strings.ContainsAny(got, "\x00\x07\x1b") {
		t.Errorf("control characters survived: %q", got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("carriage returns survived: %q", got)
	}
	if !strings.Contains(got, "debark: red text") {
		t.Errorf("cleaning ate the message: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Interaction with the frozen constructors
// ---------------------------------------------------------------------------

// TestClassifyDoesNotDisturbCancellation records the boundary: cancellation is
// NewCanceledError's job, not classify's — dferr has no class for it (see
// docs/dev/cli-surface.md, "What the CLI does not give the GUI"). classify has
// no way to know a context was cancelled, and must never manufacture the flag.
func TestClassifyDoesNotDisturbCancellation(t *testing.T) {
	canceled := NewCanceledError([]string{ProgramName, "build", "--json"})
	if !canceled.Canceled() {
		t.Fatal("NewCanceledError does not report Canceled")
	}
	if canceled.Summary() == "" || canceled.Hint() == "" {
		t.Error("the frozen cancellation error is missing its message")
	}

	e := classify([]string{ProgramName, "build", "--json"}, 1, "")
	if e.Canceled() {
		t.Error("classify invented a cancellation")
	}
}

// TestClassifyIsUsableThroughAsError checks the shape callers actually see:
// every Adapter method returns error, and the UI reaches the summary through
// AsError.
func TestClassifyIsUsableThroughAsError(t *testing.T) {
	var err error = classify([]string{ProgramName, "build", "--json"}, 5, "")
	e, ok := AsError(err)
	if !ok {
		t.Fatal("AsError did not find the *Error")
	}
	if e.Summary() == "" || e.Hint() == "" {
		t.Error("the error a caller sees has no message")
	}
	if !e.HasClass(dferr.Resolution) {
		t.Errorf("HasClass(resolution) is false for exit 5; class is %v", e.Class())
	}
	if e.CommandLine() != ProgramName+" build --json" {
		t.Errorf("CommandLine() = %q", e.CommandLine())
	}
}
