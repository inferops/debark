// Package prompt is debark's line-oriented question layer: the guided
// interactive modes of `debark build` and `debark snapshot from-base`
// ask their questions through this package and through nothing else.
//
// The shape of it is fixed by the product definition, not by taste. The
// permanent do-not-build list forbids "a GUI, or a
// full-screen TUI as the primary interface", and records the reason beside
// it: debark must work "over SSH, serial consoles, CI logs, transcripted
// sessions". internal/cli/render says the same thing in code — "colour and
// tables only, never a full-screen UI" — and this package is
// held to it strictly:
//
//   - one question per line, written with fmt to a plain io.Writer;
//   - one answer read as one line from a bufio.Reader;
//   - no alternate screen, no cursor addressing, no raw mode, no repaint,
//     no carriage-return rewriting of a line already printed;
//   - scrollback therefore stays intact, so script(1), tee, a CI log and a
//     9600-baud serial console all capture the whole exchange, in order,
//     exactly as the operator saw it.
//
// Concretely: the only escape sequence that may ever reach the writer is
// SGR (bold, faint, the four render.Style colours). Nothing here moves the
// cursor. TestNeverEmitsCursorMovementOrAlternateScreen pins that as a
// property of the whole package rather than of any single method, because
// the way this constraint gets broken is not by someone deciding to write a
// TUI — it is by one convenient in-place redraw at a time.
//
// # Everything is written to survive a transcript
//
// A guided build is a decision an operator may have to defend later, so the
// exchange has to be readable months afterwards by someone who was not
// there. Two rules follow, and the second is the one that is easy to get
// wrong:
//
//   - The operator's own keystrokes are already on the screen — a terminal
//     in canonical mode echoes them — so this package never prints a typed
//     answer back. Doing so would make one answer look like two.
//   - Everything debark *resolved* is not on the screen, and is always
//     printed: an accepted default, a menu number turned into a name, a "y"
//     turned into the word yes. See the comment on echo below, which
//     enumerates which case is which.
//
// # EOF is an error, never a silent default
//
// Every method that reads returns a dferr.Usage error when standard input
// ends with a question still pending. This is the single most important
// behaviour in the package. Interactive mode is gated on a TTY, but a TTY
// gate is not proof of an operator: a CI job can attach /dev/null to a
// pseudo-terminal, a pipe can run dry halfway through, an ssh session can
// drop. If this package answered its own questions with their defaults in
// that situation, debark would build a bundle nobody asked for and report
// success — and the operator would find out on the far side of the air gap.
// Failing loudly with exit class 1 is the only safe answer.
//
// Nothing here parses anything. Collect hands each line to a classifier the
// caller supplies (built on the packages.txt grammar that core/fetch and
// internal/cli already own between them) and collects the raw entries; this
// package must never grow a second, divergent copy of that grammar.
//
// No dependency beyond the standard library, core/dferr and
// internal/cli/render. A terminal library is exactly what the permanent
// do-not-build list rules out.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/internal/cli/render"
)

// Prompter asks questions on one pair of streams. It is not safe for
// concurrent use, and deliberately so: two goroutines asking questions on
// one terminal is not a thing an operator can answer.
type Prompter struct {
	// in is built once, in New, and reused for the Prompter's whole
	// lifetime. bufio reads ahead from the underlying io.Reader, so a
	// fresh bufio.Reader per call would silently discard whatever it had
	// buffered beyond the line it returned. That is not a corner case: it
	// is the operator who pastes a block of answers at once, and it is
	// every scripted `printf 'a\nb\nc\n' | debark ...`. One reader for
	// the Prompter means a caller can mix Line, Confirm, Choose and
	// Collect in any order without losing input between them.
	in *bufio.Reader
	// out is where questions, echoes and complaints all go. One writer,
	// not a stdout/stderr pair: an interleaved question-and-answer only
	// makes sense as a single ordered stream, and splitting it would
	// scramble the transcript the moment either stream were redirected.
	out io.Writer
	// style is the only route to colour in this package (decides
	// once, up front, whether colour is on at all). A zero-value
	// render.Style is fully usable — its renderer is built lazily — which
	// is what lets tests drive every path with colour off.
	style render.Style
}

// New returns a Prompter reading from in and writing to out.
func New(in io.Reader, out io.Writer, style render.Style) *Prompter {
	return &Prompter{in: bufio.NewReader(in), out: out, style: style}
}

// Option is one choice in a list.
type Option struct {
	// Value is what the caller gets back.
	Value string
	// Label is what is shown. Empty means Value.
	Label string
	// Note is dimmed trailing text, e.g. a caveat or an explanation.
	Note string
}

// label is what Choose prints for o.
func (o Option) label() string {
	if o.Label != "" {
		return o.Label
	}
	return o.Value
}

// Line asks a free-text question. def is shown in brackets and returned
// when the operator just presses enter. An empty answer with an empty def
// re-asks: a question with no default has no answer debark could invent,
// and accepting "" there would push a blank release name or a blank output
// path into the build.
func (p *Prompter) Line(question, def string) (string, error) {
	for {
		p.ask(question, def)
		answer, err := p.read(question)
		if err != nil {
			return "", err
		}
		if answer != "" {
			// Already on the screen, typed by the operator. Not echoed.
			return answer, nil
		}
		if def == "" {
			p.complain("an answer is required")
			continue
		}
		p.echo(def)
		return def, nil
	}
}

// OptionalLine is Line but accepts an empty answer as an empty result. It
// never loops, because every answer it can receive — including none — is a
// valid one.
func (p *Prompter) OptionalLine(question, def string) (string, error) {
	p.ask(question, def)
	answer, err := p.read(question)
	if err != nil {
		return "", err
	}
	switch {
	case answer != "":
		return answer, nil
	case def != "":
		p.echo(def)
		return def, nil
	default:
		// "(none)" rather than a bare blank line. The result really is
		// the empty string, and the parentheses mark this as debark
		// describing the answer rather than quoting one — but a reader of
		// the transcript can now tell "the operator skipped this" apart
		// from "a line went missing".
		p.echo("(none)")
		return "", nil
	}
}

// Confirm asks a yes/no question. y, yes, n and no are accepted in any
// case; an empty answer takes def; anything else re-asks.
func (p *Prompter) Confirm(question string, def bool) (bool, error) {
	bracket := "y/N"
	if def {
		bracket = "Y/n"
	}
	for {
		p.ask(question, bracket)
		answer, err := p.read(question)
		if err != nil {
			return false, err
		}
		// Unlike Line, Confirm echoes even an answer the operator typed
		// out in full. What the caller receives here is a boolean, and
		// "y" is not that boolean written down; normalising every
		// accepted spelling to one of two words makes a transcript
		// greppable and settles any question of how "Y" was read.
		switch strings.ToLower(answer) {
		case "":
			p.echoBool(def)
			return def, nil
		case "y", "yes":
			p.echoBool(true)
			return true, nil
		case "n", "no":
			p.echoBool(false)
			return false, nil
		default:
			p.complain("please answer yes or no (y, yes, n or no)")
		}
	}
}

// Choose prints a numbered list and asks for one of them. The operator may
// answer with the number or with the Value itself. defIndex is the
// zero-based default, or -1 for none.
//
// An out-of-range number or an unrecognised name re-asks with a one-line
// complaint; neither is an error, because a mistyped answer at an
// interactive prompt is a normal event and abandoning a long guided flow
// over one would be hostile. A caller mistake — no options at all, or a
// defIndex that is not a real option — is a different thing, and is
// returned as an error.
func (p *Prompter) Choose(question string, options []Option, defIndex int) (Option, error) {
	if len(options) == 0 {
		return Option{}, dferr.New(dferr.Usage,
			"prompt: Choose(%q) was given no options to choose from", oneLine(question))
	}
	if defIndex < -1 || defIndex >= len(options) {
		return Option{}, dferr.New(dferr.Usage,
			"prompt: Choose(%q) default index %d is out of range for %d options (use -1 for no default)",
			oneLine(question), defIndex, len(options))
	}

	p.printOptions(question, options)

	// The bracketed default is the NUMBER, not the name. The numbered list
	// is directly above, so "[1]" points at a line the operator can see,
	// and it is literally the keystroke that pressing enter stands in for.
	// It also makes the echo below carry real information instead of
	// repeating the bracket: "[1]" resolves to "bookworm", which is the
	// half a transcript reader actually needs.
	bracket := ""
	if defIndex >= 0 {
		bracket = strconv.Itoa(defIndex + 1)
	}

	for {
		p.ask("Enter a number or a name", bracket)
		answer, err := p.read(question)
		if err != nil {
			return Option{}, err
		}

		if answer == "" {
			// The bound is re-checked here rather than relied on from the
			// entry validation above. It costs nothing, it is what lets a
			// static checker see the index is safe, and it means a later
			// edit to the entry check cannot turn an empty answer into a
			// panic in front of an operator halfway through a guided run.
			if defIndex < 0 || defIndex >= len(options) {
				p.complain("a choice is required; enter a number from 1 to %d, or one of the names above", len(options))
				continue
			}
			//nolint:gosec // G602: defIndex is bounded twice before this
			// line -- once when Choose validates its arguments, and once by
			// the branch immediately above -- but gosec's range analysis does
			// not carry either fact into the loop body. The check above is
			// kept regardless of this annotation; it is there for the next
			// editor, not for the linter.
			return p.chose(options[defIndex]), nil
		}

		// A number is tried first, so a list whose Values happen to look
		// like numbers is still navigable by position. It does mean an
		// option whose Value is "2" cannot be selected by typing "2"
		// unless it is also the second entry; that is the right trade,
		// because the numbers are the affordance the list itself
		// advertises, and the echo says which option was actually taken.
		if n, convErr := strconv.Atoi(answer); convErr == nil {
			if n < 1 || n > len(options) {
				p.complain("there is no option %d; enter a number from 1 to %d", n, len(options))
				continue
			}
			return p.chose(options[n-1]), nil
		}

		if i := matchValue(options, answer); i >= 0 {
			return p.chose(options[i]), nil
		}
		p.complain("%q is not one of the options; enter a number from 1 to %d, or one of the names above", answer, len(options))
	}
}

// Collect reads one entry per line until a blank line. classify, when
// non-nil, is called with each accepted entry and its return value is
// echoed back under it as feedback; returning an error rejects the entry,
// prints the error and re-asks without recording it.
//
// This function parses nothing. It does not know what a package name, a URL
// or a .deb path looks like, and it must not learn: the packages.txt
// grammar has exactly one implementation per surface already
// (core/fetch.ParseListFile for list files, classifyBuildArg for positional
// arguments) and a third copy here would be a third thing to keep in
// agreement. classify's signature is the one both of those can be wrapped
// in unchanged — an entry in, a human description out, an error to refuse
// it.
//
// One entry is one whole line, trimmed of surrounding whitespace and of a
// trailing carriage return, and nothing else happens to it. In particular
// it is never split on spaces or commas, because a perfectly ordinary entry
// is a path — "C:\builds\packages.txt", "/srv/lists/base list.txt" — and
// splitting one would hand the caller two half-paths.
func (p *Prompter) Collect(question string, classify func(entry string) (string, error)) ([]string, error) {
	fmt.Fprintln(p.out, question)
	fmt.Fprintf(p.out, "  %s\n", p.style.Faint("one per line; an empty line ends the list"))

	var entries []string
	for {
		// A short, distinct prompt per entry, so an operator part way
		// through a long list can still see that debark is waiting for
		// another one. When a block is pasted the terminal echoes it all
		// at once and these prompts bunch up ahead of it; that is
		// cosmetic, universal to line-oriented collectors, and far
		// preferable to the alternative, which is repainting.
		fmt.Fprint(p.out, "> ")
		entry, err := p.read(question)
		if err != nil {
			// A list that never received its terminating blank line is
			// not a finished list. Returning the entries gathered so far
			// alongside the error would offer a caller a silently
			// truncated set of build inputs, which is the exact failure
			// the package's EOF rule exists to prevent, so nothing is
			// returned with it.
			return nil, err
		}
		if entry == "" {
			return entries, nil
		}
		if classify != nil {
			note, cerr := classify(entry)
			if cerr != nil {
				p.reject(cerr)
				continue
			}
			if note != "" {
				p.echo(note)
			}
		}
		entries = append(entries, entry)
	}
}

// Say writes an informational line. The trailing newline is added here
// rather than asked of every caller, so a forgotten one can never leave the
// next question stranded on the end of an unrelated sentence.
//
// These are ordinary fmt semantics, deliberately, even for a call with no
// arguments: an earlier draft wrote a no-argument format string verbatim so
// that "the pool is 100% cached" needed no escaping, and go vet's printf
// analysis — which recognises this method as a printf wrapper from its
// signature — immediately, and correctly, objected. That analysis is worth
// far more than the convenience: it is what will catch a mismatched verb in
// the guided build flow before an operator ever sees "%!s(MISSING)" in a
// transcript. A literal percent sign is written "%%", as everywhere else in
// Go.
func (p *Prompter) Say(format string, args ...any) {
	fmt.Fprintf(p.out, "%s\n", fmt.Sprintf(format, args...))
}

// Section writes a blank line and a bold heading, so a long flow has
// visible structure without any cursor movement.
//
// This is the whole of the package's structure mechanism, on purpose. A
// long guided flow needs chapters; a TUI would draw them as panes, tabs or
// a sidebar, and the do-not-build list rules all three out. So debark does what a
// well-behaved shell script does instead — vertical whitespace and a bold
// word — both of which survive a pipe into less, a CI log, a serial console
// and a paste into a ticket, and neither of which moves the cursor.
func (p *Prompter) Section(title string) {
	fmt.Fprintf(p.out, "\n%s\n", p.style.Bold(title))
}

// --- output primitives -----------------------------------------------------

// ask writes the question and deliberately leaves the cursor on the same
// line. That is what makes this a prompt rather than a banner: the operator
// types immediately after the colon and the terminal echoes the answer
// there, so one exchange occupies one line of the transcript.
//
// The consequence, when stdin is a pipe rather than a terminal, is that
// nothing echoes the answer or its newline and the next thing debark
// prints continues on the same line. That is not a defect to paper over
// with a speculative newline — a speculative newline would put a blank line
// into every real interactive session, which is the case that matters — and
// the golden tests capture the piped shape exactly so that it stays known.
func (p *Prompter) ask(question, bracket string) {
	if bracket != "" {
		fmt.Fprintf(p.out, "%s %s: ", question, p.style.Faint("["+bracket+"]"))
		return
	}
	fmt.Fprintf(p.out, "%s: ", question)
}

// echo records a resolved answer on its own line.
//
// Which answers are echoed, and which are not, is worth stating exactly,
// because getting it wrong makes a transcript either unreadable or a lie:
//
//   - What the operator TYPED is already on the screen. A terminal in
//     canonical mode echoes every keystroke as it is typed, so printing the
//     same text back doubles it, and "name: curl" followed by "curl" reads
//     as two answers to two questions when someone comes back to the log.
//     So an answer the operator typed which is already the final value is
//     never echoed: Line and OptionalLine with a non-empty answer, and
//     Collect's raw entries.
//
//   - What debark RESOLVED is not on the screen, and it is the part that
//     matters. Pressing enter at "release [bookworm]: " leaves nothing in
//     the transcript but an empty line, and nobody reading it later can
//     tell whether the build used bookworm or whether an answer was typed
//     and lost. Likewise "2" is not something anyone can read back;
//     "trixie" is. So every resolved value IS echoed: an accepted default
//     (Line, OptionalLine, Choose, Confirm), a menu number turned into a
//     name (Choose), a keystroke turned into the word yes or no (Confirm),
//     and the classifier's verdict on an entry (Collect).
//
// The two-space indent marks the line as debark's rather than the
// operator's, so the two stay distinguishable in a plain capture with no
// colour at all — which,, is what NO_COLOR, TERM=dumb and every
// redirected stream get.
func (p *Prompter) echo(value string) {
	fmt.Fprintf(p.out, "  %s\n", value)
}

func (p *Prompter) echoBool(v bool) {
	if v {
		p.echo("yes")
		return
	}
	p.echo("no")
}

// chose echoes the selected option and returns it, so that every successful
// return path in Choose echoes exactly once. The echoed text is Value, not
// Label: Value is what the caller receives and what will end up in the
// resulting build invocation, and a transcript showing only the pretty
// label would not let a reader reconstruct the command.
func (p *Prompter) chose(o Option) Option {
	p.echo(o.Value)
	return o
}

// complain reports an answer debark could not use, before re-asking.
// Yellow, via render.Style: nothing has failed and nothing is being
// abandoned.
func (p *Prompter) complain(format string, args ...any) {
	fmt.Fprintf(p.out, "  %s\n", p.style.Warn(fmt.Sprintf(format, args...)))
}

// reject reports a caller-supplied classifier's refusal of an entry. Red
// rather than yellow, and the distinction is not decoration: complain is
// debark's own nudge about the shape of an answer, whereas this is
// somebody else's error value in somebody else's wording, and the operator
// should be able to see at a glance which of the two they are reading.
func (p *Prompter) reject(err error) {
	fmt.Fprintf(p.out, "  %s\n", p.style.Red(err.Error()))
}

// printOptions writes the question and the numbered list beneath it.
//
// Notes are aligned into a column so that a list of options with caveats
// reads as a list rather than as ragged prose. The alignment is done with
// spaces and a trailing newline, never by positioning the cursor, and a row
// with no note is emitted with no trailing padding at all — trailing
// whitespace is invisible noise in a transcript and it makes a golden test
// fragile for no benefit.
func (p *Prompter) printOptions(question string, options []Option) {
	width := 0
	for _, o := range options {
		if n := utf8.RuneCountInString(o.label()); n > width {
			width = n
		}
	}
	numWidth := len(strconv.Itoa(len(options)))

	fmt.Fprintln(p.out, question)
	for i, o := range options {
		line := fmt.Sprintf("  %*d) ", numWidth, i+1)
		if o.Note != "" {
			line += padRight(o.label(), width) + "  " + p.style.Faint(o.Note)
		} else {
			line += o.label()
		}
		fmt.Fprintln(p.out, line)
	}
}

// --- input primitives ------------------------------------------------------

// read returns the next line of input, or a classified error.
//
// EOF is an error and never a silent default; see the package doc for why
// that is the most important sentence in this file. The one subtlety is a
// final line with no terminator — "yes" from `printf yes | ...`, or the
// last line of a file some editor saved without one. bufio hands that text
// back together with io.EOF, and discarding it would throw away an answer
// the operator genuinely gave. So a non-empty unterminated line is accepted
// as the answer, and the *next* read is the one that reports that input
// ended.
func (p *Prompter) read(question string) (string, error) {
	line, err := p.in.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			if line != "" {
				return clean(line), nil
			}
			return "", inputEnded(question)
		}
		// A genuine I/O failure on stdin is the machine failing, not the
		// operator: a closed pty after a dropped ssh session, a read
		// error on a redirected file. Environment (exit 2), unlike the
		// Usage that EOF earns, because there is no invocation the
		// operator could have typed differently.
		return "", dferr.Wrap(dferr.Environment, err,
			"prompt: reading the answer to %q", oneLine(question))
	}
	return clean(line), nil
}

// inputEnded is the error every reading method returns when standard input
// closes with a question still on the screen. Usage (exit 1) is the class:
// the run was invoked interactively against something that cannot answer,
// and that is a mistake in how debark was called.
func inputEnded(question string) error {
	return dferr.New(dferr.Usage,
		"prompt: input ended while the question %q was still waiting for an answer",
		oneLine(question)).
		WithHint("guided mode needs a terminal on stdin; if stdin is a pipe, a file or /dev/null, run the command with explicit flags instead")
}

// clean turns one raw input line into an entry: the line terminator and any
// surrounding whitespace are removed, and NOTHING else is done to it — no
// splitting, no case folding, no path interpretation, no comment stripping.
// Collect's contract is that a line which is a path reaches the caller's
// classifier byte for byte.
//
// The explicit carriage-return strip is not redundant with TrimSpace; it is
// documentation of a bug this project would otherwise meet twice a week.
// Input arriving through Git Bash, a Windows terminal, a file edited on
// Windows or any CRLF pipe is "yes\r\n", and an unstripped \r turns "yes"
// into "yes\r", which matches no case in Confirm and would re-ask for ever.
func clean(line string) string {
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return strings.TrimSpace(line)
}

// matchValue finds the option the operator named. An exact match on Value
// wins outright; a case-insensitive match is only the fallback, so an
// operator who types "Bookworm" is not sent round again over one capital
// letter, while a set of options whose Values differ only by case stays
// resolvable by typing one of them precisely.
func matchValue(options []Option, answer string) int {
	for i, o := range options {
		if o.Value == answer {
			return i
		}
	}
	for i, o := range options {
		if strings.EqualFold(o.Value, answer) {
			return i
		}
	}
	return -1
}

// padRight pads s to w columns, counting runes rather than bytes so that a
// label containing a non-ASCII character does not drag the note column out
// of line.
func padRight(s string, w int) string {
	if n := w - utf8.RuneCountInString(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// oneLine collapses a question to a single line of single-spaced words, so
// that a multi-line question cannot smear an error message across a log.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
