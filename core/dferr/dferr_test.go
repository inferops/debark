package dferr

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// core/dferr is the frozen exit-code table (ADR-012) and it had no tests at
// all. Every operator's automation branches on these eight integers: exit 4
// means "the media is compromised, escalate". The tests below pin the table
// itself, then the classification rules that decide which number an error
// reaches the CLI with.

// --- the frozen table -----------------------------------------------------

// The eight classes, their integer values, their stable strings. ADR-012
// freezes all three. A change here is a contract break, not a refactor.
func TestClassTableIsFrozen(t *testing.T) {
	table := []struct {
		class Class
		code  int
		name  string
	}{
		{Success, 0, "success"},
		{Usage, 1, "usage"},
		{Environment, 2, "environment"},
		{Incomplete, 3, "incomplete"},
		{Verification, 4, "verification"},
		{Resolution, 5, "resolution"},
		{Policy, 6, "policy"},
		{TargetMismatch, 7, "target-mismatch"},
	}
	for _, tc := range table {
		if int(tc.class) != tc.code {
			t.Errorf("%s: value = %d, want %d (ADR-012 freezes it)", tc.name, int(tc.class), tc.code)
		}
		if got := tc.class.String(); got != tc.name {
			t.Errorf("Class(%d).String() = %q, want %q", tc.code, got, tc.name)
		}
	}
	if len(table) != 8 {
		t.Fatalf("the table has %d entries; ADR-012 says exactly eight", len(table))
	}
}

// Classes() is what generates the documented exit-code table and what
// internal/cli's exitClassFromString searches. It must be complete and in
// exit-code order, or the published table and the reverse mapping both lose
// a class silently.
func TestClassesIsCompleteAndInExitCodeOrder(t *testing.T) {
	got := Classes()
	if len(got) != 8 {
		t.Fatalf("Classes() returned %d classes, want 8", len(got))
	}
	for i, c := range got {
		if int(c) != i {
			t.Errorf("Classes()[%d] = %v (%d), want exit code %d", i, c, int(c), i)
		}
	}
}

// The mapping must be total and unambiguous: every class in the table has a
// String and a Description, no two share a String, and neither method
// returns an empty answer.
func TestClassStringsAndDescriptionsAreTotalAndUnambiguous(t *testing.T) {
	seenName := map[string]Class{}
	seenDesc := map[string]Class{}
	for _, c := range Classes() {
		name, desc := c.String(), c.Description()
		if name == "" || strings.HasPrefix(name, "unknown") {
			t.Errorf("Class(%d).String() = %q: a table class has no stable name", int(c), name)
		}
		if desc == "" || desc == "unknown" {
			t.Errorf("Class(%d).Description() = %q: a table class has no description", int(c), desc)
		}
		if prev, dup := seenName[name]; dup {
			t.Errorf("classes %d and %d share the string %q", int(prev), int(c), name)
		}
		if prev, dup := seenDesc[desc]; dup {
			t.Errorf("classes %d and %d share the description %q", int(prev), int(c), desc)
		}
		seenName[name] = c
		seenDesc[desc] = c
	}
}

// An integer outside the table is reported as unknown rather than silently
// rendered as one of the eight. --json and evidence both carry this string.
func TestClassOutsideTheTableRendersAsUnknown(t *testing.T) {
	for _, c := range []Class{Class(8), Class(42), Class(-1), Class(255)} {
		if got := c.String(); got != fmt.Sprintf("unknown(%d)", int(c)) {
			t.Errorf("Class(%d).String() = %q, want unknown(%d)", int(c), got, int(c))
		}
		if got := c.Description(); got != "unknown" {
			t.Errorf("Class(%d).Description() = %q, want %q", int(c), got, "unknown")
		}
	}
}

// --- nothing exits 0 on failure -------------------------------------------

// The single most important property in the package: an error that reaches
// the CLI boundary carrying no class at all still exits non-zero. ClassOf
// maps it to Usage (1), never Success.
func TestUnclassifiedErrorIsUsageNeverSuccess(t *testing.T) {
	for _, err := range []error{
		errors.New("a plain error"),
		fmt.Errorf("a wrapped plain error: %w", errors.New("cause")),
		fs.ErrNotExist,
		bareSentinel{},
	} {
		if got := ClassOf(err); got != Usage {
			t.Errorf("ClassOf(%v) = %v, want %v", err, got, Usage)
		}
		if got := ExitCode(err); got == 0 {
			t.Errorf("ExitCode(%v) = 0: a failure exited successfully", err)
		}
	}
}

// bareSentinel is a bare error type carrying no *Error anywhere in its chain.
type bareSentinel struct{}

func (bareSentinel) Error() string { return "some other package's sentinel" }

// Only a nil error is Success, and every classified error reaches the CLI
// with its own class.
func TestExitCodeAcrossTheWholeTable(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Errorf("ExitCode(nil) = %d, want 0", got)
	}
	if got := ClassOf(nil); got != Success {
		t.Errorf("ClassOf(nil) = %v, want Success", got)
	}
	for _, c := range Classes()[1:] { // Success is not a failure class
		err := New(c, "failure of class %s", c)
		if got := ExitCode(err); got != int(c) {
			t.Errorf("ExitCode(New(%v, ...)) = %d, want %d", c, got, int(c))
		}
	}
}

// A *dferr.Error whose Class was never set carries the ZERO VALUE of Class,
// which is Success — so it exits 0 while describing a failure. Nothing in
// the tree builds one today (every &dferr.Error{...} literal in core/ sets
// Class explicitly, checked), but the shape is one forgotten field away and
// the CLI has no floor under it: internal/cli/root.go returns
// dferr.ExitCode(err) directly for any non-nil error.
//
// This test records the hazard rather than asserting it is impossible; if
// the package ever grows a floor (ClassOf mapping Success on a non-nil error
// to Usage), this test is the one to flip.
func TestZeroValueClassIsSuccessWhichIsTheHazard(t *testing.T) {
	err := &Error{Msg: "the medium is corrupt"}
	if got := ExitCode(err); got != 0 {
		t.Skipf("ExitCode of a Class-less *Error is now %d; the documented hazard is closed, update the finding", got)
	}
	t.Logf("KNOWN HAZARD: &Error{Msg:%q} exits %d — a failure that exits successfully", err.Msg, ExitCode(err))
}

// ExitCode does not clamp: a Class outside 0-7 becomes a process exit status
// outside the frozen table. os.Exit(-1) is reported as 255 by a shell, which
// no ADR-012 consumer knows how to read.
func TestExitCodeDoesNotClampOutOfTableClasses(t *testing.T) {
	if got := ExitCode(&Error{Class: Class(42), Msg: "x"}); got != 42 {
		t.Skipf("ExitCode now clamps (got %d); the documented gap is closed, update the finding", got)
	}
	if got := ExitCode(&Error{Class: Class(-1), Msg: "x"}); got != -1 {
		t.Skipf("ExitCode now clamps negatives (got %d); update the finding", got)
	}
}

// --- ClassOf / HintOf resolution ------------------------------------------

// ClassOf resolves the OUTERMOST *Error, which is what the doc comment now
// says and what the call sites want — and it is exactly why an unconditional
// Wrap over an already-classified cause silently overrides that cause's
// class. Two live bugs of this shape were found and fixed elsewhere this
// week (core/install turning a missing gpg into exit 4, core/engine the same
// over a signer call), so the behaviour is pinned here in both directions.
func TestClassOfResolvesTheOutermostError(t *testing.T) {
	cause := Verifyf("digest mismatch")
	outer := Wrap(Environment, cause, "while checking the bundle")

	if got := ClassOf(outer); got != Environment {
		t.Errorf("ClassOf(outer) = %v, want %v (outermost wins)", got, Environment)
	}
	if got := ClassOf(cause); got != Verification {
		t.Errorf("the cause's own class changed: %v", got)
	}
	if !errors.Is(outer, cause) {
		t.Error("Wrap broke the chain: errors.Is cannot reach the cause")
	}
	// The consequence, stated as a test: exit 4 became exit 2.
	if ExitCode(outer) == ExitCode(cause) {
		t.Error("this test no longer demonstrates the override it exists to document")
	}
}

// The same rule reaches through a foreign wrapper that is not a *Error at
// all — internal/cli's silentError is exactly this shape, and it must not
// cost an error its class.
func TestClassOfReachesThroughAForeignWrapper(t *testing.T) {
	inner := Verifyf("manifest signature does not verify")
	wrapped := fmt.Errorf("cli: %w", inner)
	if got := ClassOf(wrapped); got != Verification {
		t.Errorf("ClassOf(fmt.Errorf(%%w)) = %v, want %v", got, Verification)
	}
	if got := ExitCode(wrapped); got != 4 {
		t.Errorf("ExitCode = %d, want 4: an operator's escalation trigger was lost", got)
	}
}

// A cause several levels down is still found when nothing above it is a
// *Error.
func TestClassOfWalksAnArbitrarilyDeepChain(t *testing.T) {
	err := error(Policyf("approved-keys violation"))
	for i := 0; i < 20; i++ {
		err = fmt.Errorf("layer %d: %w", i, err)
	}
	if got := ClassOf(err); got != Policy {
		t.Errorf("ClassOf(deep chain) = %v, want %v", got, Policy)
	}
}

// HintOf resolves the outermost *Error too — including when that outermost
// Error has no hint and an inner one does. A hint is pure operator guidance
// with no downside to preserving, so losing it to a wrap costs the operator
// their next action and gains nothing. Pinned as the current behaviour, and
// reported.
func TestHintOfTakesTheOutermostErrorsHintEvenWhenEmpty(t *testing.T) {
	inner := Envf("gpg not found on PATH").WithHint("install the gnupg package")
	if got := HintOf(inner); got != "install the gnupg package" {
		t.Fatalf("HintOf(inner) = %q, want the hint it was given", got)
	}
	outer := Wrap(Verification, inner, "signing failed")
	if got := HintOf(outer); got != "" {
		t.Skipf("HintOf now searches for a non-empty hint (got %q); the documented gap is closed, update the finding", got)
	}
	t.Log("KNOWN GAP: an unconditional Wrap over a hinted cause drops the hint as well as the class")
}

func TestHintOfOnUnhintedAndUnclassifiedErrors(t *testing.T) {
	if got := HintOf(nil); got != "" {
		t.Errorf("HintOf(nil) = %q, want empty", got)
	}
	if got := HintOf(errors.New("plain")); got != "" {
		t.Errorf("HintOf(plain) = %q, want empty", got)
	}
	if got := HintOf(Usagef("no hint here")); got != "" {
		t.Errorf("HintOf(unhinted) = %q, want empty", got)
	}
}

// WithHint copies rather than mutating, so a shared sentinel cannot acquire a
// caller's hint.
func TestWithHintCopies(t *testing.T) {
	base := Envf("no container runtime")
	hinted := base.WithHint("install docker or podman, or pass --backend=local")
	if base.Hint != "" {
		t.Errorf("WithHint mutated the receiver: Hint = %q", base.Hint)
	}
	if hinted.Hint == "" {
		t.Error("WithHint returned a copy with no hint")
	}
	if hinted.Class != base.Class || hinted.Msg != base.Msg {
		t.Error("WithHint did not carry Class and Msg into the copy")
	}
	if hinted == base {
		t.Error("WithHint returned the same pointer")
	}
	// A second WithHint replaces rather than appends.
	again := hinted.WithHint("second")
	if again.Hint != "second" {
		t.Errorf("second WithHint = %q, want %q", again.Hint, "second")
	}
}

// --- Is -------------------------------------------------------------------

func TestIs(t *testing.T) {
	verify := Verifyf("bad digest")
	if !Is(verify, Verification) {
		t.Error("Is(verification error, Verification) = false")
	}
	if Is(verify, Environment) {
		t.Error("Is(verification error, Environment) = true")
	}
	if Is(errors.New("plain"), Usage) != true {
		t.Error("Is(unclassified, Usage) = false; ClassOf maps it to Usage")
	}
	// nil is never any class, Success included: Is answers "did this error
	// fail in way c", and a nil error did not fail.
	for _, c := range Classes() {
		if Is(nil, c) {
			t.Errorf("Is(nil, %v) = true, want false", c)
		}
	}
}

// --- Error/Unwrap mechanics ----------------------------------------------

func TestErrorMessageComposition(t *testing.T) {
	cause := errors.New("no such file or directory")
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{"msg and cause", &Error{Class: Usage, Msg: "open config", Err: cause}, "open config: no such file or directory"},
		{"msg only", &Error{Class: Usage, Msg: "open config"}, "open config"},
		{"cause only", &Error{Class: Usage, Err: cause}, "no such file or directory"},
		{"neither", &Error{Class: Verification}, "verification"},
		{"neither, out of table", &Error{Class: Class(9)}, "unknown(9)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnwrapAndErrorsIsAs(t *testing.T) {
	sentinel := errors.New("sentinel")
	err := Wrap(Resolution, sentinel, "apt could not satisfy the request")

	if !errors.Is(err, sentinel) {
		t.Error("errors.Is could not reach the sentinel")
	}
	var target *Error
	if !errors.As(err, &target) {
		t.Fatal("errors.As could not extract the *Error")
	}
	if target.Class != Resolution {
		t.Errorf("extracted class = %v, want %v", target.Class, Resolution)
	}
	if errors.Unwrap(err) != sentinel {
		t.Error("Unwrap did not return the cause")
	}
	// An Error with no cause unwraps to nil, not to itself (which would make
	// errors.Is loop forever).
	if got := errors.Unwrap(New(Usage, "leaf")); got != nil {
		t.Errorf("Unwrap(leaf) = %v, want nil", got)
	}
}

// errors.Is over two sibling classes must not conflate them just because both
// are *Error.
func TestTwoClassifiedErrorsAreNotEqual(t *testing.T) {
	a := Verifyf("x")
	b := Verifyf("x")
	if errors.Is(a, b) {
		t.Error("two independently constructed *Error values compare equal under errors.Is")
	}
}

// --- nil handling ---------------------------------------------------------

// Wrap returning nil for a nil cause is what makes `return dferr.Wrap(c, err,
// ...)` safe to write unconditionally. If it ever allocated instead, every
// such call site would start reporting a failure on success.
func TestWrapReturnsNilForANilCause(t *testing.T) {
	err := Wrap(Verification, nil, "checking %s", "the manifest")
	if err != nil {
		t.Fatalf("Wrap(_, nil, _) = %v, want nil", err)
	}
	// And the interface is genuinely nil, not a typed nil.
	if ExitCode(err) != 0 {
		t.Errorf("ExitCode of a nil Wrap = %d, want 0", ExitCode(err))
	}
}

// A typed-nil *Error assigned to an error interface panics ClassOf. Go's
// classic typed-nil trap, reachable from any helper declared to return
// *dferr.Error rather than error (core/sign's wrapErr has that signature
// today, though it never returns nil). Recorded because the panic lands on
// the exit path: internal/cli's recover turns it into exit 2 and prints
// "internal error", so it degrades rather than exiting 0 — but the operator
// loses the real class.
func TestTypedNilErrorPanicsClassOf(t *testing.T) {
	var typed *Error //nolint:staticcheck // SA4023: the typed nil is the point of the test, see below
	err := error(typed)
	// staticcheck is right that this comparison is never true today -- that
	// IS the assertion. A *Error(nil) boxed into an error interface is not
	// equal to nil, and the whole point of this test is to fail loudly if
	// that ever stops being so, because the panic documented above depends
	// on it. Removing the branch to satisfy the linter would delete the
	// canary and leave only the panic check, which passes for the wrong
	// reason if the trap disappears.
	if err == nil { //nolint:staticcheck // SA4023: never-true today, and that IS the assertion; see above
		t.Fatal("a typed nil *Error compared equal to nil; the trap no longer exists")
	}
	defer func() {
		if r := recover(); r == nil {
			t.Skip("ClassOf no longer panics on a typed-nil *Error; the documented gap is closed")
		}
	}()
	_ = ClassOf(err)
	t.Error("expected a panic from ClassOf on a typed-nil *Error")
}

// --- formatting safety ----------------------------------------------------

// New/Wrap and the convenience constructors are printf wrappers, so `go vet`
// rejects a non-constant format string at every call site (it did during this
// review, which is how the protection was confirmed). What it cannot catch is
// a format string built at run time, so the runtime behaviour is pinned:
// stray directives degrade into visible %!verb(MISSING) noise rather than
// panicking or truncating.
func TestFormatDirectivesInArgumentsAreNotReinterpreted(t *testing.T) {
	// A package name from apt's output, carrying directives, passed as an
	// ARGUMENT (the correct call shape) must appear verbatim.
	evil := "pkg-%s-%d-%%-%!v"
	err := New(Resolution, "apt: could not satisfy %q", evil)
	if !strings.Contains(err.Error(), evil) {
		t.Errorf("Error() = %q, want it to contain the argument %q verbatim", err.Error(), evil)
	}
	if strings.Contains(err.Error(), "MISSING") || strings.Contains(err.Error(), "EXTRA") {
		t.Errorf("Error() = %q: the argument was reinterpreted as a format", err.Error())
	}
}

func TestFormatWithNoArgumentsKeepsPercentLiteral(t *testing.T) {
	// The correct way to render a literal percent, and the shape hints use.
	e := Incompletef("download reached 100%% and stalled")
	if got := e.Error(); got != "download reached 100% and stalled" {
		t.Errorf("Error() = %q", got)
	}
}

// Wrap composes its own message with the cause, and neither half may swallow
// the other.
func TestWrapMessageKeepsBothHalves(t *testing.T) {
	err := Wrap(Environment, errors.New("permission denied"), "apt: private root: write %s", "sources.list")
	want := "apt: private root: write sources.list: permission denied"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// --- terminal safety ------------------------------------------------------

// dferr does not filter terminal control sequences, and it is not the layer
// that should: the same Error.Error() string is what --json and core/evidence
// carry, where JSON escaping is the right and only treatment, and where
// stripping bytes would make an evidence record disagree with what happened.
// The rendering layer is the chokepoint (internal/cli/root.go's printErr does
// fmt.Fprintf(w, "debark: %s\n", err.Error()) with no filtering today).
//
// This test states dferr's half of that contract: bytes handed in come back
// out unchanged, so nothing downstream has to guess whether they were
// already sanitised.
func TestErrorMessagePreservesItsBytesExactly(t *testing.T) {
	// The shape that overwrote a printed digest line with a forged one.
	hostile := "\x1b[1A\x1b[2Kdigest: sha256:" + strings.Repeat("0", 64)
	err := Verifyf("apt: download failed: %s", hostile)
	if !strings.Contains(err.Error(), hostile) {
		t.Errorf("Error() = %q; dferr must neither strip nor alter message bytes", err.Error())
	}
	wrapped := Wrap(Incomplete, errors.New(hostile), "apt: download failed")
	if !strings.Contains(wrapped.Error(), hostile) {
		t.Errorf("wrapped Error() = %q; a cause's bytes were altered", wrapped.Error())
	}
}

// --- the classify helper this package does not have -----------------------

// core/engine keeps a private classify() that preserves an already-classified
// cause, and core/install and core/engine each shipped a bug from NOT having
// it to hand. This test builds the same helper from what dferr exports today,
// to show the semantics a promoted version would need — and that they are
// expressible with the current API, so promoting it is an addition rather
// than a change.
func classifyLikeEngine(err error, def Class, format string, args ...any) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	return Wrap(def, err, format, args...)
}

func TestClassifySemanticsAreExpressibleToday(t *testing.T) {
	// An already-classified cause keeps its class and its hint.
	cause := Envf("gpg not found on PATH").WithHint("install the gnupg package")
	got := classifyLikeEngine(cause, Verification, "signing failed")
	if ClassOf(got) != Environment {
		t.Errorf("ClassOf = %v, want %v: a classified cause was reclassified", ClassOf(got), Environment)
	}
	if HintOf(got) != "install the gnupg package" {
		t.Errorf("HintOf = %q: the cause's hint was lost", HintOf(got))
	}
	if ExitCode(got) == 4 {
		t.Error("an environment problem exited 4 — the exact bug this helper prevents")
	}

	// An unclassified cause takes the default class.
	plain := classifyLikeEngine(errors.New("exec: gpg: not found"), Environment, "signing failed")
	if ClassOf(plain) != Environment {
		t.Errorf("ClassOf(unclassified) = %v, want %v", ClassOf(plain), Environment)
	}
	if !strings.Contains(plain.Error(), "signing failed") {
		t.Errorf("Error() = %q, want the wrap message", plain.Error())
	}

	// nil in, nil out — the property that lets it be written unconditionally.
	if classifyLikeEngine(nil, Verification, "x") != nil {
		t.Error("classify(nil) was not nil")
	}
}
