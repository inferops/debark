package prompt

// These tests do two jobs, and the second matters more than the first.
//
// The first is ordinary: every method's happy path, its re-ask paths and
// its EOF path, driven through a strings.Reader and a bytes.Buffer so the
// exact bytes written can be pinned. Several assertions below are on whole
// buffers rather than on substrings, on purpose — the transcript shape is
// part of this package's contract (a guided build is a decision somebody
// may have to reconstruct from a log months later), so a change to it
// should have to be made deliberately rather than drifting in.
//
// The second job is TestNeverEmitsCursorMovementOrAlternateScreen, at the
// bottom. The permanent do-not-build list forbids "a GUI, or a
// full-screen TUI as the primary interface" because debark must work over
// SSH, serial consoles, CI logs and script(1) transcripts, and
// internal/cli/render repeats the rule in code ("colour and tables only,
// never a full-screen UI"). That test is that rule expressed
// as an executable property of the whole package, with colour switched ON
// so it cannot pass vacuously by there being no escape sequences at all.

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/internal/cli/render"
)

// newPrompter drives the package with colour off, through the ZERO VALUE
// render.Style rather than render.Style{Enabled: false}. Both disable
// colour, but the zero value also leaves style.renderer nil, which is the
// case style.go builds lazily — so every test here also proves the package
// is usable without a real terminal ever having been consulted.
func newPrompter(input string) (*Prompter, *bytes.Buffer) {
	var out bytes.Buffer
	return New(strings.NewReader(input), &out, render.Style{}), &out
}

func wantOutput(t *testing.T, got *bytes.Buffer, want string) {
	t.Helper()
	if got.String() != want {
		t.Errorf("output mismatch\n got: %q\nwant: %q", got.String(), want)
	}
}

func wantUsageError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if c := dferr.ClassOf(err); c != dferr.Usage {
		t.Errorf("error class = %v (exit %d), want usage (exit 1); err = %v", c, int(c), err)
	}
	// The message has to say what happened in words an operator reading a
	// CI log can act on, not just carry the right integer.
	if !strings.Contains(err.Error(), "input ended") {
		t.Errorf("error should say plainly that input ended: %v", err)
	}
	if dferr.HintOf(err) == "" {
		t.Errorf("EOF error should carry a hint about running non-interactively: %v", err)
	}
}

// releases is the option list several tests share. It exercises all three
// Option fields: a Label that differs from Value, a Note on some rows and
// not others, and a row with no Label at all.
var releases = []Option{
	{Value: "bookworm", Label: "debian:bookworm-slim", Note: "Debian 12"},
	{Value: "trixie", Label: "debian:trixie-slim"},
	{Value: "noble", Note: "Ubuntu 24.04 LTS"},
}

// --- Line ------------------------------------------------------------------

func TestLineReturnsTheTypedAnswerAndDoesNotEchoIt(t *testing.T) {
	p, out := newPrompter("wget\n")
	got, err := p.Line("Package name", "curl")
	if err != nil {
		t.Fatalf("Line: %v", err)
	}
	if got != "wget" {
		t.Errorf("got %q, want %q", got, "wget")
	}
	// Nothing after the prompt: the terminal already echoed "wget". The
	// answer and the prompt share a line in a real session; in this piped
	// capture there is simply nothing after the trailing space.
	wantOutput(t, out, "Package name [curl]: ")
}

func TestLineTakesTheDefaultOnEmptyAndEchoesIt(t *testing.T) {
	p, out := newPrompter("\n")
	got, err := p.Line("Package name", "curl")
	if err != nil {
		t.Fatalf("Line: %v", err)
	}
	if got != "curl" {
		t.Errorf("got %q, want %q", got, "curl")
	}
	// The resolved default IS echoed: the operator's enter left nothing in
	// the transcript, so without this line nobody could tell what was built.
	wantOutput(t, out, "Package name [curl]:   curl\n")
}

func TestLineWithNoDefaultReAsksInsteadOfReturningEmpty(t *testing.T) {
	p, out := newPrompter("\n\nfinally\n")
	got, err := p.Line("Output path", "")
	if err != nil {
		t.Fatalf("Line: %v", err)
	}
	if got != "finally" {
		t.Errorf("got %q, want %q", got, "finally")
	}
	wantOutput(t, out,
		"Output path: "+
			"  an answer is required\n"+
			"Output path: "+
			"  an answer is required\n"+
			"Output path: ")
}

func TestLineTrimsSurroundingWhitespace(t *testing.T) {
	p, _ := newPrompter("   spaced out   \n")
	got, err := p.Line("Name", "")
	if err != nil {
		t.Fatalf("Line: %v", err)
	}
	// Trimmed at the ends, untouched in the middle.
	if got != "spaced out" {
		t.Errorf("got %q, want %q", got, "spaced out")
	}
}

func TestLineAcceptsAFinalLineWithNoNewline(t *testing.T) {
	// `printf wget | debark ...` and editors that save without a trailing
	// newline both produce this. Throwing the text away would discard an
	// answer the operator really gave.
	p, _ := newPrompter("wget")
	got, err := p.Line("Package name", "curl")
	if err != nil {
		t.Fatalf("Line: %v", err)
	}
	if got != "wget" {
		t.Errorf("got %q, want %q", got, "wget")
	}
}

// --- OptionalLine ----------------------------------------------------------

func TestOptionalLineAcceptsAnEmptyAnswerAsAnEmptyResult(t *testing.T) {
	p, out := newPrompter("\n")
	got, err := p.OptionalLine("Signing key", "")
	if err != nil {
		t.Fatalf("OptionalLine: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
	wantOutput(t, out, "Signing key:   (none)\n")
}

func TestOptionalLineTakesAndEchoesTheDefault(t *testing.T) {
	p, out := newPrompter("\n")
	got, err := p.OptionalLine("Signing key", "gpg:ABCD")
	if err != nil {
		t.Fatalf("OptionalLine: %v", err)
	}
	if got != "gpg:ABCD" {
		t.Errorf("got %q, want %q", got, "gpg:ABCD")
	}
	wantOutput(t, out, "Signing key [gpg:ABCD]:   gpg:ABCD\n")
}

func TestOptionalLineReturnsTheTypedAnswer(t *testing.T) {
	p, out := newPrompter("plugin:yubi\n")
	got, err := p.OptionalLine("Signing key", "gpg:ABCD")
	if err != nil {
		t.Fatalf("OptionalLine: %v", err)
	}
	if got != "plugin:yubi" {
		t.Errorf("got %q, want %q", got, "plugin:yubi")
	}
	wantOutput(t, out, "Signing key [gpg:ABCD]: ")
}

// --- Confirm ---------------------------------------------------------------

func TestConfirmAcceptedSpellingsInEveryCase(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"y", true}, {"Y", true}, {"yes", true}, {"Yes", true}, {"YES", true}, {"yEs", true},
		{"n", false}, {"N", false}, {"no", false}, {"No", false}, {"NO", false}, {"nO", false},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			p, out := newPrompter(c.input + "\n")
			got, err := p.Confirm("Include recommends?", true)
			if err != nil {
				t.Fatalf("Confirm: %v", err)
			}
			if got != c.want {
				t.Errorf("Confirm(%q) = %v, want %v", c.input, got, c.want)
			}
			// Every spelling normalises to one of two words in the
			// transcript, so a reader never has to know how "nO" was read.
			word := "no"
			if c.want {
				word = "yes"
			}
			wantOutput(t, out, "Include recommends? [Y/n]:   "+word+"\n")
		})
	}
}

func TestConfirmEmptyTakesTheDefaultBothWays(t *testing.T) {
	for _, def := range []bool{true, false} {
		t.Run(fmt.Sprint(def), func(t *testing.T) {
			p, out := newPrompter("\n")
			got, err := p.Confirm("Sign the bundle?", def)
			if err != nil {
				t.Fatalf("Confirm: %v", err)
			}
			if got != def {
				t.Errorf("got %v, want %v", got, def)
			}
			bracket, word := "y/N", "no"
			if def {
				bracket, word = "Y/n", "yes"
			}
			wantOutput(t, out, "Sign the bundle? ["+bracket+"]:   "+word+"\n")
		})
	}
}

func TestConfirmReAsksOnJunk(t *testing.T) {
	p, out := newPrompter("maybe\nsure\ny\n")
	got, err := p.Confirm("Proceed?", false)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !got {
		t.Error("expected true after the eventual y")
	}
	complaint := "  please answer yes or no (y, yes, n or no)\n"
	wantOutput(t, out,
		"Proceed? [y/N]: "+complaint+
			"Proceed? [y/N]: "+complaint+
			"Proceed? [y/N]:   yes\n")
}

// --- Choose ----------------------------------------------------------------

// The numbered list is the whole of Choose's visible contract, so it is
// pinned byte for byte: the two-space indent, the "N) " marker, the notes
// aligned into a column against the longest label, and no trailing
// whitespace on the row that has no note.
func TestChooseRendersTheNumberedListAndEchoesTheChosenValue(t *testing.T) {
	p, out := newPrompter("2\n")
	got, err := p.Choose("Which base image?", releases, 0)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if got.Value != "trixie" {
		t.Errorf("got %q, want %q", got.Value, "trixie")
	}
	wantOutput(t, out,
		"Which base image?\n"+
			"  1) debian:bookworm-slim  Debian 12\n"+
			"  2) debian:trixie-slim\n"+
			"  3) noble                 Ubuntu 24.04 LTS\n"+
			"Enter a number or a name [1]:   trixie\n")
}

func TestChooseByValue(t *testing.T) {
	p, out := newPrompter("noble\n")
	got, err := p.Choose("Which base image?", releases, 0)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if got.Value != "noble" {
		t.Errorf("got %q, want %q", got.Value, "noble")
	}
	// Echoed even though the operator typed it: Choose always records the
	// resolved value, because the answer may have been a number and a
	// reader should not have to work out which form was used.
	if !strings.HasSuffix(out.String(), "Enter a number or a name [1]:   noble\n") {
		t.Errorf("missing echo of the chosen value: %q", out.String())
	}
}

func TestChooseByValueIsCaseInsensitiveAsAFallback(t *testing.T) {
	p, _ := newPrompter("Trixie\n")
	got, err := p.Choose("Which base image?", releases, 0)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if got.Value != "trixie" {
		t.Errorf("got %q, want %q", got.Value, "trixie")
	}
}

func TestChooseEmptyTakesTheDefaultIndex(t *testing.T) {
	p, out := newPrompter("\n")
	got, err := p.Choose("Which base image?", releases, 2)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if got.Value != "noble" {
		t.Errorf("got %q, want %q", got.Value, "noble")
	}
	// [3] is the keystroke enter stood in for; "noble" is what it meant.
	// The echo is what turns the number back into something readable.
	if !strings.HasSuffix(out.String(), "Enter a number or a name [3]:   noble\n") {
		t.Errorf("bad tail: %q", out.String())
	}
}

func TestChooseOutOfRangeNumberReAsksAndIsNotAnError(t *testing.T) {
	p, out := newPrompter("0\n9\n-1\n2\n")
	got, err := p.Choose("Which base image?", releases, 0)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if got.Value != "trixie" {
		t.Errorf("got %q, want %q", got.Value, "trixie")
	}
	for _, n := range []string{"there is no option 0", "there is no option 9", "there is no option -1"} {
		if !strings.Contains(out.String(), n) {
			t.Errorf("missing complaint %q in %q", n, out.String())
		}
	}
	// The list is printed once, not once per re-ask: re-printing it would
	// be the beginning of a repaint.
	if n := strings.Count(out.String(), "Which base image?"); n != 1 {
		t.Errorf("the option list should be printed once, got %d", n)
	}
	if n := strings.Count(out.String(), "Enter a number or a name [1]: "); n != 4 {
		t.Errorf("expected 4 asks, got %d in %q", n, out.String())
	}
}

func TestChooseUnknownValueReAsks(t *testing.T) {
	p, out := newPrompter("sid\nbookworm\n")
	got, err := p.Choose("Which base image?", releases, 0)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if got.Value != "bookworm" {
		t.Errorf("got %q, want %q", got.Value, "bookworm")
	}
	if !strings.Contains(out.String(), `"sid" is not one of the options`) {
		t.Errorf("missing complaint: %q", out.String())
	}
}

func TestChooseWithNoDefaultReAsksOnAnEmptyAnswer(t *testing.T) {
	p, out := newPrompter("\n\n1\n")
	got, err := p.Choose("Which base image?", releases, -1)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if got.Value != "bookworm" {
		t.Errorf("got %q, want %q", got.Value, "bookworm")
	}
	// No bracket at all when there is no default: nothing to show.
	if strings.Contains(out.String(), "Enter a number or a name [") {
		t.Errorf("defIndex -1 must not print a bracketed default: %q", out.String())
	}
	if n := strings.Count(out.String(), "a choice is required"); n != 2 {
		t.Errorf("expected 2 complaints, got %d in %q", n, out.String())
	}
}

func TestChooseUsesValueWhenLabelIsEmpty(t *testing.T) {
	p, out := newPrompter("1\n")
	_, err := p.Choose("Arch?", []Option{{Value: "amd64"}}, -1)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	wantOutput(t, out, "Arch?\n  1) amd64\nEnter a number or a name:   amd64\n")
}

func TestChooseNumbersAreRightAlignedPastTen(t *testing.T) {
	var opts []Option
	for i := 1; i <= 10; i++ {
		opts = append(opts, Option{Value: fmt.Sprintf("opt%d", i)})
	}
	p, out := newPrompter("10\n")
	got, err := p.Choose("Pick", opts, -1)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if got.Value != "opt10" {
		t.Errorf("got %q", got.Value)
	}
	if !strings.Contains(out.String(), "   1) opt1\n") || !strings.Contains(out.String(), "  10) opt10\n") {
		t.Errorf("numbers should be right-aligned: %q", out.String())
	}
}

// A caller mistake is an error, unlike an operator mistake, because there
// is no answer the operator could give that would fix it.
func TestChooseRejectsCallerMistakes(t *testing.T) {
	t.Run("no options", func(t *testing.T) {
		p, out := newPrompter("1\n")
		if _, err := p.Choose("Pick", nil, -1); err == nil {
			t.Fatal("expected an error for an empty option list")
		} else if dferr.ClassOf(err) != dferr.Usage {
			t.Errorf("class = %v, want usage", dferr.ClassOf(err))
		}
		if out.Len() != 0 {
			t.Errorf("nothing should be printed before the caller check: %q", out.String())
		}
	})
	for _, bad := range []int{-2, 3, 99} {
		t.Run(fmt.Sprintf("defIndex %d", bad), func(t *testing.T) {
			p, _ := newPrompter("1\n")
			if _, err := p.Choose("Pick", releases, bad); err == nil {
				t.Fatalf("expected an error for defIndex %d", bad)
			} else if dferr.ClassOf(err) != dferr.Usage {
				t.Errorf("class = %v, want usage", dferr.ClassOf(err))
			}
		})
	}
}

// --- Collect ---------------------------------------------------------------

// classifier is the shape internal/cli's classifyBuildArg and
// core/fetch.ParseListFile can both be wrapped in: an entry in, a human
// description out, an error to refuse it. Collect must not know any of this
// grammar itself, which is why the test supplies it.
func classifier(entry string) (string, error) {
	switch {
	case strings.HasPrefix(entry, "https://"), strings.HasPrefix(entry, "http://"):
		return "url: " + entry, nil
	case strings.HasSuffix(entry, ".deb"):
		return "file: " + entry, nil
	case strings.HasPrefix(entry, "-"):
		return "", errors.New(`"` + entry + `" is not usable as an apt package name`)
	default:
		return "package: " + entry, nil
	}
}

func TestCollectStopsOnABlankLineAndEchoesClassifierFeedback(t *testing.T) {
	p, out := newPrompter("curl\nhttps://example.invalid/x.deb\n\ntrailing\n")
	got, err := p.Collect("What should the bundle contain?", classifier)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := []string{"curl", "https://example.invalid/x.deb"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
	wantOutput(t, out,
		"What should the bundle contain?\n"+
			"  one per line; an empty line ends the list\n"+
			"> "+
			"  package: curl\n"+
			"> "+
			"  url: https://example.invalid/x.deb\n"+
			"> ")
	// The blank line stopped it: "trailing" is still in the reader and is
	// available to whatever the caller asks next.
	rest, err := p.Line("Next", "")
	if err != nil {
		t.Fatalf("Line after Collect: %v", err)
	}
	if rest != "trailing" {
		t.Errorf("input after the blank line was lost: got %q", rest)
	}
}

func TestCollectReAsksOnAClassifierErrorWithoutRecordingTheEntry(t *testing.T) {
	p, out := newPrompter("-rf\ncurl\n\n")
	got, err := p.Collect("Packages", classifier)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 1 || got[0] != "curl" {
		t.Errorf("rejected entry must not be recorded; got %q", got)
	}
	wantOutput(t, out,
		"Packages\n"+
			"  one per line; an empty line ends the list\n"+
			"> "+
			`  "-rf" is not usable as an apt package name`+"\n"+
			"> "+
			"  package: curl\n"+
			"> ")
}

func TestCollectWithANilClassifierRecordsEveryLineVerbatim(t *testing.T) {
	p, out := newPrompter("a\nb\n\n")
	got, err := p.Collect("Anything", nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("got %q", got)
	}
	wantOutput(t, out, "Anything\n  one per line; an empty line ends the list\n> > > ")
}

func TestCollectImmediatelyBlankReturnsNothing(t *testing.T) {
	p, _ := newPrompter("\n")
	got, err := p.Collect("Extras", classifier)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %q, want an empty list", got)
	}
}

// The real use case: an operator pastes a list they already had. All fifty
// lines must come back, in order, none merged and none dropped — which is
// also what pins the single-bufio.Reader requirement, since a per-call
// reader would swallow the tail of the paste sitting in the buffer.
func TestCollectFiftyPastedLinesComeBackInOrder(t *testing.T) {
	var in strings.Builder
	want := make([]string, 50)
	for i := range want {
		want[i] = fmt.Sprintf("pkg-%02d", i)
		fmt.Fprintf(&in, "%s\n", want[i])
	}
	in.WriteString("\n")

	p, _ := newPrompter(in.String())
	got, err := p.Collect("Packages", classifier)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 50 {
		t.Fatalf("got %d entries, want 50", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A path is one entry, whole. Nothing here may split on a space, a comma or
// a backslash, because "the operator typed the path to their packages.txt"
// is a case the caller's classifier is expected to handle and it can only
// do so if the line arrives intact.
func TestCollectKeepsWholeLinesIncludingPathsIntact(t *testing.T) {
	lines := []string{
		`C:\builds\projects\packages.txt`,
		`/srv/lists/base list.txt`,
		`curl, wget, git`,
		`./relative/../packages.txt`,
		`https://example.invalid/a b/x.deb`,
		`pkg=1.2.3-1~bpo12+1`,
	}
	p, _ := newPrompter(strings.Join(lines, "\n") + "\n\n")
	got, err := p.Collect("Entries", nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != len(lines) {
		t.Fatalf("got %d entries, want %d: %q", len(got), len(lines), got)
	}
	for i, want := range lines {
		if got[i] != want {
			t.Errorf("entry %d = %q, want %q", i, got[i], want)
		}
	}
}

// --- Say and Section -------------------------------------------------------

func TestSayAndSection(t *testing.T) {
	p, out := newPrompter("")
	p.Section("Sources")
	p.Say("the snapshot names %d %s", 3, "sources")
	// Ordinary fmt semantics, so a literal percent is "%%" — see Say's
	// comment for why the verbatim shortcut was dropped in favour of
	// keeping go vet's printf analysis working for callers.
	p.Say("100%% of the pool is already cached")
	p.Say("no verbs here at all")
	wantOutput(t, out,
		"\nSources\n"+
			"the snapshot names 3 sources\n"+
			"100% of the pool is already cached\n"+
			"no verbs here at all\n")
}

// --- EOF, for every reading method ----------------------------------------

// The most important behaviour in the package. A pipe that ran dry or a CI
// job that attached /dev/null despite the TTY gate must stop the flow with
// exit class 1, never quietly accept the defaults and build something
// nobody asked for. Each method gets its own case, including the two whose
// defaults would otherwise make silence look like a valid answer
// (OptionalLine, whose empty answer is legitimate, and Confirm, which has a
// default by construction).
func TestEOFMidQuestionIsAUsageErrorForEveryMethod(t *testing.T) {
	// Every entry is driven with completely empty input, so the very first
	// read hits EOF with a question already on the screen.
	cases := []struct {
		name string
		call func(p *Prompter) error
	}{
		{"Line", func(p *Prompter) error { _, err := p.Line("Package name", "curl"); return err }},
		{"Line/no default", func(p *Prompter) error { _, err := p.Line("Package name", ""); return err }},
		{"OptionalLine", func(p *Prompter) error { _, err := p.OptionalLine("Signing key", "gpg:AB"); return err }},
		{"Confirm", func(p *Prompter) error { _, err := p.Confirm("Sign?", true); return err }},
		{"Choose", func(p *Prompter) error { _, err := p.Choose("Image?", releases, 0); return err }},
		{"Choose/no default", func(p *Prompter) error { _, err := p.Choose("Image?", releases, -1); return err }},
		{"Collect", func(p *Prompter) error { _, err := p.Collect("Packages", classifier); return err }},
		{"Collect/nil classifier", func(p *Prompter) error { _, err := p.Collect("Packages", nil); return err }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _ := newPrompter("")
			wantUsageError(t, c.call(p))
		})
	}
}

// EOF part way through, after some answers have already been given, is the
// pipe-ran-dry case proper. It must fail exactly as hard as EOF on the
// first question.
func TestEOFPartWayThroughAFlowIsStillAUsageError(t *testing.T) {
	p, _ := newPrompter("bookworm\ny\n")
	if _, err := p.Line("Release", ""); err != nil {
		t.Fatalf("first question: %v", err)
	}
	if _, err := p.Confirm("Sign?", true); err != nil {
		t.Fatalf("second question: %v", err)
	}
	_, err := p.Choose("Image?", releases, 0)
	wantUsageError(t, err)
}

func TestEOFDuringCollectDiscardsThePartialList(t *testing.T) {
	// An unterminated list is not a finished list. Handing back the entries
	// gathered so far would offer the caller a silently truncated build
	// input, which is the same failure as taking a default in silence.
	p, _ := newPrompter("curl\nwget\n")
	got, err := p.Collect("Packages", classifier)
	wantUsageError(t, err)
	if got != nil {
		t.Errorf("a truncated list must not be returned, got %q", got)
	}
}

func TestEOFAfterAnUnterminatedFinalLine(t *testing.T) {
	// "curl" with no newline is a real answer; the EOF that follows it is
	// the error. This checks the two do not collapse into one.
	p, _ := newPrompter("curl")
	got, err := p.Line("First", "")
	if err != nil || got != "curl" {
		t.Fatalf("first = %q, err = %v; want the unterminated line accepted", got, err)
	}
	_, err = p.Line("Second", "")
	wantUsageError(t, err)
}

func TestEOFErrorNamesTheQuestionThatWasPending(t *testing.T) {
	p, _ := newPrompter("")
	_, err := p.Line("Which release is the target running?", "")
	wantUsageError(t, err)
	if !strings.Contains(err.Error(), "Which release is the target running?") {
		t.Errorf("the error should name the pending question: %v", err)
	}
}

// --- CRLF ------------------------------------------------------------------

// Git Bash, a Windows terminal, a list edited in Notepad and any CRLF pipe
// all deliver "answer\r\n". An unstripped \r turns "y" into "y\r", which
// matches nothing, and Confirm would re-ask for ever.
func TestCRLFInputIsHandledByEveryMethod(t *testing.T) {
	p, out := newPrompter("wget\r\n\r\nYES\r\ntrixie\r\n" +
		`C:\builds\packages.txt` + "\r\ncurl\r\n\r\n")

	line, err := p.Line("Package name", "curl")
	if err != nil || line != "wget" {
		t.Fatalf("Line = %q, err = %v", line, err)
	}
	def, err := p.Line("Release", "bookworm")
	if err != nil || def != "bookworm" {
		t.Fatalf("Line default = %q, err = %v", def, err)
	}
	yes, err := p.Confirm("Sign?", false)
	if err != nil || !yes {
		t.Fatalf("Confirm = %v, err = %v", yes, err)
	}
	opt, err := p.Choose("Image?", releases, 0)
	if err != nil || opt.Value != "trixie" {
		t.Fatalf("Choose = %q, err = %v", opt.Value, err)
	}
	entries, err := p.Collect("Packages", nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := []string{`C:\builds\packages.txt`, "curl"}
	if strings.Join(entries, "|") != strings.Join(want, "|") {
		t.Fatalf("Collect = %q, want %q", entries, want)
	}
	// And no carriage return leaked into the transcript either.
	if strings.Contains(out.String(), "\r") {
		t.Errorf("a \\r from the input reached the output: %q", out.String())
	}
}

// --- one reader for the lifetime ------------------------------------------

// bufio reads ahead. A fresh bufio.Reader per method would drop whatever it
// had buffered past the line it returned, so a caller mixing methods over a
// piped or pasted answer set would lose input between them.
func TestMethodsCanBeMixedWithoutLosingBufferedInput(t *testing.T) {
	p, _ := newPrompter("myproject\ny\n2\nlibfoo\nlibbar\n\nlast answer\n")

	name, err := p.Line("Name", "")
	if err != nil {
		t.Fatal(err)
	}
	sign, err := p.Confirm("Sign?", false)
	if err != nil {
		t.Fatal(err)
	}
	img, err := p.Choose("Image?", releases, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := p.Collect("Packages", classifier)
	if err != nil {
		t.Fatal(err)
	}
	last, err := p.OptionalLine("Anything else", "")
	if err != nil {
		t.Fatal(err)
	}

	if name != "myproject" || !sign || img.Value != "trixie" ||
		strings.Join(pkgs, ",") != "libfoo,libbar" || last != "last answer" {
		t.Errorf("mixed flow lost input: name=%q sign=%v img=%q pkgs=%q last=%q",
			name, sign, img.Value, pkgs, last)
	}
}

// --- the interface constraints, as tests ----------------------------------

// exerciseEverything drives every method and every branch that produces
// output — happy paths, defaults, complaints, classifier rejections,
// headings — so the two escape-sequence tests below see the whole surface
// rather than one method's worth of it.
func exerciseEverything(t *testing.T, style render.Style) string {
	t.Helper()
	var out bytes.Buffer
	input := strings.Join([]string{
		"wget",          // Line, typed
		"",              // Line, default
		"",              // Line with no default: complains
		"path",          // Line, second go
		"",              // OptionalLine, no default: (none)
		"",              // OptionalLine, default
		"nonsense", "y", // Confirm: complains, then yes
		"",                   // Confirm: default
		"99", "sid", "", "2", // Choose: two complaints, one empty re-ask, then a number
		"",                                                 // Choose with a default
		"-rf", "curl", "https://example.invalid/x.deb", "", // Collect: reject, two accepts, end
		"", // Collect: immediately blank
	}, "\n") + "\n"

	p := New(strings.NewReader(input), &out, style)

	p.Section("Target")
	p.Say("reading the snapshot")
	p.Say("100%% cached")
	mustLine := func(q, def string) {
		if _, err := p.Line(q, def); err != nil {
			t.Fatalf("Line(%q): %v", q, err)
		}
	}
	mustLine("Package name", "curl")
	mustLine("Release", "bookworm")
	mustLine("Output path", "")
	if _, err := p.OptionalLine("Signing key", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OptionalLine("Policy file", "policy.yaml"); err != nil {
		t.Fatal(err)
	}
	p.Section("Signing")
	if _, err := p.Confirm("Sign the bundle?", true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Confirm("Include recommends?", false); err != nil {
		t.Fatal(err)
	}
	p.Section("Inputs")
	if _, err := p.Choose("Which base image?", releases, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Choose("Which base image?", releases, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Collect("What should the bundle contain?", classifier); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Collect("Anything else?", nil); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// With colour off, nothing may emit an escape byte at all — the same
// assertion internal/cli/cli_surface_test.go makes about the command tree,
// for the same reason: NO_COLOR, TERM=dumb and every redirected stream take
// this path, and an escape that leaks through it lands in a log file
// as mojibake.
func TestNoANSIAnywhereWhenTheStyleIsDisabled(t *testing.T) {
	for name, style := range map[string]render.Style{
		"zero value":     {},
		"explicitly off": {Enabled: false},
	} {
		t.Run(name, func(t *testing.T) {
			out := exerciseEverything(t, style)
			if strings.ContainsRune(out, '\x1b') {
				t.Errorf("colour is disabled but the output contains an ANSI escape: %q", out)
			}
		})
	}
}

// sgr matches an SGR sequence: ESC [ ; parameters ; m. Colour, bold and
// faint are all SGR, and SGR is the ONLY escape sequence this package is
// permitted to emit.
var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// This is that rule written as a test, and it is the one that stops a
// future change turning this package into a TUI by accident.
//
// the permanent do-not-build list forbids "a GUI, or a full-screen TUI as
// the primary interface", because debark has to work over SSH, serial
// consoles, CI logs and script(1) transcripts; it is repeated as
// "colour/tables only; no Bubble Tea", and internal/cli/render's package
// doc repeats it again. What actually breaks that rule is never a decision
// to write a TUI — it is one convenient in-place redraw, one cleared
// screen, one hidden cursor, added because it looked tidier on somebody's
// terminal. Each of those makes the transcript unreadable and the serial
// console unusable, and none of them would fail any other test here.
//
// So: with colour switched ON, the only escape sequence in the entire
// output must be SGR, and there must be no carriage return anywhere. If you
// are reading this because you just made it fail, the change you are making
// is one that list forbids; the answer is another line of text, not a redraw.
func TestNeverEmitsCursorMovementOrAlternateScreen(t *testing.T) {
	out := exerciseEverything(t, render.Style{Enabled: true})

	// First, prove the test is not vacuous. With colour on the output must
	// actually contain escape sequences, otherwise "no forbidden escapes"
	// would be true of an empty buffer and would prove nothing.
	if !strings.ContainsRune(out, '\x1b') {
		t.Fatalf("colour is enabled but no escape sequence was produced; this test would pass vacuously: %q", out)
	}

	// Named checks first, so a failure says which forbidden thing appeared.
	forbidden := map[string]string{
		"\x1b[?1049h": "smcup: switch to the alternate screen",
		"\x1b[?1049l": "rmcup: leave the alternate screen",
		"\x1b[?47h":   "the older alternate-screen switch",
		"\x1b[?25l":   "hide the cursor",
		"\x1b[A":      "cursor up",
		"\x1b[B":      "cursor down",
		"\x1b[C":      "cursor forward",
		"\x1b[D":      "cursor back",
		"\x1b[H":      "cursor home",
		"\x1b[2J":     "clear the screen",
		"\x1b[K":      "erase in line",
		"\x1b[s":      "save the cursor position",
		"\x1b[u":      "restore the cursor position",
		"\x1b7":       "save the cursor position (DEC)",
		"\x1b8":       "restore the cursor position (DEC)",
	}
	for seq, what := range forbidden {
		if strings.Contains(out, seq) {
			t.Errorf("output contains %q (%s); a full-screen interface is forbidden", seq, what)
		}
	}

	// Then the catch-all, which is the assertion that actually holds the
	// line: strip every SGR sequence and no escape byte may remain. That
	// covers parameterised forms ("\x1b[12A"), private modes nobody thought
	// to name above, OSC, and anything a future dependency might emit.
	if rest := sgr.ReplaceAllString(out, ""); strings.ContainsRune(rest, '\x1b') {
		i := strings.IndexRune(rest, '\x1b')
		t.Errorf("output contains a non-SGR escape sequence %q; only colour/bold/faint are permitted",
			rest[i:min(i+16, len(rest))])
	}

	// A carriage return is how a line-oriented program becomes a
	// cursor-addressing one without anybody noticing: it rewrites a line
	// already printed, which destroys the transcript (contrast
	// internal/cli/progress, which uses \r deliberately and only after the
	// caller has confirmed a real TTY — a question, unlike a progress bar,
	// is part of the record and is never redrawn).
	if strings.Contains(out, "\r") {
		t.Errorf("output contains a carriage return; a question is never redrawn in place: %q", out)
	}
}
