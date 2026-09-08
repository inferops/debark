package cliadapter

// errors.go turns the only three facts a finished debark process leaves
// behind — argv, exit code, stderr — into something a person can act on.
//
// It is the implementing package. Its whole job is one definition-of-done item from
// docs/dev/contract-brief.md:
//
//	Every error path renders an actionable message; none surface a raw exit
//	code alone.
//
// # The seam
//
// classify(argv, exitCode, stderr) *Error is the only thing invoke.go and
// events.go call here. It ALWAYS returns a non-nil *Error with a
// non-empty summary and a non-empty hint — for an empty stderr, for garbage
// stderr, and for an exit code nobody has seen before. Everything else in this
// file is unexported and prefixed err/classify so it cannot collide with the
// sibling packages writing into this package at the same time.
//
// # Two sentences, in two fields
//
//	Summary()  what is wrong          — never empty, never a number on its own
//	Hint()     what to do about it    — never empty
//
// Summary is a clause with no trailing full stop, matching dferr's own message
// style, because Error() renders it as "<summary> (<class>)". Hint is prose and
// is punctuated normally. TestClassifyRuleTableStyle pins both conventions.
//
// # Why a table and not a switch
//
// Recognising a specific failure is a one-line addition to errRules: a class, a
// command, a regexp, a summary template, a hint. Adding a row cannot break the
// fallback, because the fallback is not a branch anyone can forget — errResolve
// has exactly one return statement, and everything reaching it has already been
// through errFloor, which is total over every class and every exit code. A rule
// that does not match, or that matches but captures an empty group, simply does
// not fire: a confidently wrong sentence is worse than a correct generic one.
//
// # What the CLI does not give us
//
// Errors are never JSON (docs/dev/cli-surface.md, C1). --json does not change
// failure output; dferr.Error has no JSON tags and is never serialised. So the
// exit code is the only machine-readable channel and stderr is the only prose.
//
// And two commands do not even give us that. `build` prints its BuildResult and
// then returns a SILENCED error, so a failed build usually writes nothing at all
// to stderr. Verified here against a real binary: `verify` does the same —
// `debark verify <not-a-bundle> --json` exits 4 with zero bytes on stderr
// (testdata/stderr/verification-verify-silent.stderr). The rules whose matcher
// is errMatchSilent exist for exactly that: they are keyed on the command and
// the class alone, and they are the only thing standing between the operator
// and a blank error dialog. See the "silent failures" comment on errRules.

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/inferops/debark/core/dferr"
)

// ---------------------------------------------------------------------------
// The seam
// ---------------------------------------------------------------------------

// classify builds the *Error for a finished debark process.
//
// It is the single entry point invoke.go and events.go use. The contract is
// deliberately total:
//
//   - the result is never nil;
//   - Summary() is never empty and is never just a number;
//   - Hint() is never empty;
//   - Stderr() carries what the process actually wrote, capped at the frozen
//     MaxStderrBytes, cobra usage block and all — that is the details drawer,
//     and a bug report needs the bytes rather than a tidied version of them.
//
// exitCode 0 is legitimate and means "debark succeeded but this app could
// not use its output"; deciding whether the process failed at all is the
// caller's job, not this function's.
func classify(argv []string, exitCode int, stderr string) *Error {
	// NewError is frozen and does three things worth keeping: it maps the exit
	// code onto dferr's table (with out-of-range codes landing on Environment,
	// not Usage), it caps stderr at MaxStderrBytes and marks the truncation,
	// and it copies argv. Its own summary/hint parse is then replaced below,
	// because it necessarily runs on raw bytes and knows nothing about argv.
	base := NewError(argv, exitCode, stderr)

	// Only the head of stderr can hold a message: debark prints it first and
	// cobra's usage block after. Bounding the input here bounds every regexp in
	// the rule table, so a process that printed a megabyte cannot make error
	// rendering slow.
	head := stderr
	if len(head) > MaxStderrBytes {
		head = head[:MaxStderrBytes]
	}
	region, cliSummary, cliHint := classifyStderr(head)
	summary, hint := errResolve(base.Class(), exitCode, classifyCommand(argv), region, cliSummary, cliHint)

	// "%s" and not summary: a captured message may contain a percent sign, and
	// WithSummary/WithHint are Printf-shaped.
	return base.WithSummary("%s", summary).WithHint("%s", hint)
}

// errResolve is the whole decision, in one place with one exit.
//
// Order of preference, highest first:
//
//	summary: "this is not debark" > a matching rule's template > debark's own first stderr line > the floor
//	hint:    "this is not debark" > a matching rule marked hintWins > debark's own hint > the rule's hint > the floor
//
// debark's own hint outranks a rule's by default because it was written by
// the code that knows what actually went wrong. A rule overrides it only when
// it says so explicitly — used where the CLI's hint is aimed at a developer
// with a terminal rather than an operator with a window.
//
// errNotDebark sits above both because when it fires, the text being ranked
// was not written by debark at all: quoting another program's message as
// though debark had said it is how "point this at git.exe" renders as
// "error: unknown option `json'" and sends the operator hunting for a flag.
func errResolve(class dferr.Class, exitCode int, cmd, region, cliSummary, cliHint string) (summary, hint string) {
	summary = errClip(errMeaningful(cliSummary), errMaxSummaryRunes)
	hint = errClip(errMeaningful(cliHint), errMaxHintRunes)

	switch s, h, foreign := errNotDebark(exitCode, cmd, region); {
	case foreign:
		summary, hint = s, h

	// The rule table is keyed on dferr's classes, and a class is only real
	// when the exit code is inside the frozen 0-7 table. Outside it, the class
	// is a synthetic Environment that NewError assigned because a process that
	// died in a way debark did not choose is a fact about the machine — so
	// applying, say, the silent-build rules there would invent a build failure
	// out of a missing binary. The exit code itself is the evidence, and
	// errFloor is where it is read.
	case errInTable(exitCode):
		if rule, vals, ok := errMatch(class, cmd, region); ok {
			if s := errClip(errExpand(rule.summary, vals), errMaxSummaryRunes); s != "" {
				summary = s
			}
			if h := errClip(errExpand(rule.hint, vals), errMaxHintRunes); h != "" && (rule.hintWins || hint == "") {
				hint = h
			}
		}
	}

	// The floor. Not a branch that can be forgotten: it is the last thing
	// before the only return, and errFloor is total.
	floorSummary, floorHint := errFloor(class, exitCode)
	if summary == "" {
		summary = floorSummary
	}
	if hint == "" {
		hint = floorHint
	}
	return summary, hint
}

// ---------------------------------------------------------------------------
// Reading stderr
// ---------------------------------------------------------------------------

// errUsageBlock finds the start of cobra's usage block. For a usage-class
// (exit 1) failure that block is most of stderr — measured at about 2 KB for
// `debark build --json` — and none of it is a message. It belongs in the
// details drawer, which keeps the raw bytes, and nowhere near the summary.
var errUsageBlock = regexp.MustCompile(`(?m)^Usage:[ \t]*$`)

// errRunHelp is cobra's trailing "Run 'x --help' for usage." line, which can
// survive on its own when the usage block itself is suppressed.
var errRunHelp = regexp.MustCompile(`(?m)^Run '[^']*' for usage\.[ \t]*$`)

// classifyStderr splits debark's failure output into the three pieces the
// rules and the floor need.
//
// The shape it parses is exactly what internal/cli's Execute prints:
//
//	debark: <message>
//	<hint>              (only when the error carried one; may be several lines)
//
//	Usage:              (usage-class errors only)
//	  debark build ...
//
// It returns:
//
//	region   every message line, joined — what the rule table matches against.
//	         Empty exactly when debark said nothing, which is the silent
//	         `build` and `verify` case.
//	summary  the first line, with the "debark: " prefix removed.
//	hint     the remaining lines.
//
// The text is cleaned first (see errCleanText): invalid UTF-8 cannot reach a
// summary, and neither can an escape sequence or an embedded credential.
func classifyStderr(stderr string) (region, summary, hint string) {
	s := errCleanText(stderr)
	if loc := errUsageBlock.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	s = errRunHelp.ReplaceAllString(s, "")

	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimRight(line, " \t"); strings.TrimSpace(t) != "" {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return "", "", ""
	}
	region = strings.Join(kept, "\n")
	summary = strings.TrimSpace(strings.TrimPrefix(kept[0], ProgramName+": "))
	if len(kept) > 1 {
		hint = strings.TrimSpace(strings.Join(kept[1:], "\n"))
	}
	return region, summary, hint
}

var (
	// errANSI matches CSI and OSC escape sequences. --json already suppresses
	// colour, so this is belt and braces for a debark that changes its mind
	// and for anything else that ends up on the same pipe.
	errANSI = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")

	// errCredentials matches the userinfo part of a URL: the "user:token@" in
	// https://user:token@example.com/private.deb. A vendor URL an operator
	// pasted can carry one, and a summary is the one place it must never be
	// repeated — a screenshot of an error dialog travels further than a log.
	errCredentials = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]*@`)

	// errURLQuery matches a URL's query string. Presigned download links carry
	// their signature there, so the summary keeps the path and drops the rest.
	errURLQuery = regexp.MustCompile(`(https?://[^\s?]*)\?\S*`)
)

// errCleanText makes stderr safe to quote in a message.
//
// It normalises line endings, strips escape sequences and control characters,
// replaces invalid UTF-8 with U+FFFD so a partial rune cannot corrupt a
// message, and redacts URL credentials and query strings.
//
// It is used for the summary and hint only. Stderr() keeps the raw bytes: the
// drawer is opt-in, it is what a bug report needs, and hiding the difference
// between what debark printed and what the app quoted would make a real
// failure harder to diagnose, not easier.
func errCleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	s = errANSI.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == utf8.RuneError:
			return r
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, s)
	s = errCredentials.ReplaceAllString(s, "$1")
	s = errURLQuery.ReplaceAllString(s, "$1?…")
	return s
}

// ---------------------------------------------------------------------------
// Deciding whether debark printed this at all
// ---------------------------------------------------------------------------

// errDebarkVoice reports whether a message region was written by debark.
//
// internal/cli's Execute prints every failure as "debark: <message>"
// (docs/dev/cli-surface.md C1), so that prefix on any line of the region is
// debark's signature. Its absence is not proof of anything on its own — a
// silent failure has no lines at all — but combined with an exit status
// debark would not have chosen it is strong evidence.
func errDebarkVoice(region string) bool {
	for _, line := range strings.Split(region, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ProgramName+":") {
			return true
		}
	}
	return false
}

// errNotDebark recognises a program that is not debark at all, so that the
// operator is told to fix the configured path rather than shown another
// program's complaint as though debark had made it.
//
// Two shapes were driven for real and are what this exists for:
//
//   - the path points at a program that rejects the probe's flags. git.exe run
//     as `debark version --json --no-color` exits 129 and prints
//     "error: unknown option `json'"; that whole sentence used to become the
//     summary, and git's usage line became the hint.
//   - the path points at a program that fails quietly in its own dialect.
//     where.exe exits 1 — INSIDE the frozen table — and prints "INFO: Could
//     not find files for the given pattern(s).", which rendered as that
//     sentence over the usage-class floor hint about version skew.
//
// The evidence is deliberately conservative, because being wrong here would
// accuse a real debark of being an impostor:
//
//   - the region must be non-empty and must NOT be in debark's voice. An
//     ancient debark that rejects --no-color still says "debark: ...", so
//     it keeps its own message and the version-skew rules still apply to it.
//   - the exit code must be positive. A process killed by a signal, or one
//     that never started, is a fact about the machine and errExitFloor already
//     has the better sentence for it.
//   - an exit code errExitTexts already describes (127, the Windows NTSTATUS
//     values) keeps that description, which is more specific than this one.
//   - inside the frozen 0–7 table this fires ONLY for `version`, which is the
//     one command a working debark cannot fail: it prints a struct compiled
//     into the binary. Outside the table, any command qualifies.
func errNotDebark(exitCode int, cmd, region string) (summary, hint string, ok bool) {
	region = strings.TrimSpace(region)
	switch {
	case region == "" || errDebarkVoice(region):
		return "", "", false
	case exitCode <= 0:
		return "", "", false
	case cmd != "version" && errInTable(exitCode):
		return "", "", false
	}
	if _, described := errExitTexts[exitCode]; described {
		return "", "", false
	}
	first := errClip(errMeaningful(strings.SplitN(region, "\n", 2)[0]), errMaxFieldRunes)
	if first == "" {
		return "", "", false
	}
	return fmt.Sprintf(
			"the program this application is running as %s does not answer like %s: it exited with status %d and printed %q",
			ProgramName, ProgramName, exitCode, first),
		fmt.Sprintf(
			"Point the %s path in settings at the %s binary itself, or clear it so that PATH is searched instead. The command that was run is below.",
			ProgramName, ProgramName),
		true
}

// errCommands is every debark subcommand the GUI can produce argv for, plus
// the plumbing commands it never runs — listed so that a stray invocation is
// still recognised rather than silently treated as no command at all.
// Two-word forms are tried first, so "snapshot inspect" beats "snapshot".
var errCommands = []string{
	"snapshot list-bases",
	"snapshot inspect",
	"snapshot create",
	"snapshot from-base",
	"store ls",
	"store gc",
	"config show",
	"build",
	"verify",
	"install",
	"inspect",
	"doctor",
	"keygen",
	"version",
	"snapshot",
	"store",
	"config",
	"resolve",
	"fetch",
	"apt-root",
}

// classifyCommand recovers the debark subcommand from argv, so a rule can be
// scoped to the command it is actually about.
//
// argv[0] is the program (ProgramName, or a resolved binary path), so the scan
// starts at 1 and stops at the "--" separator, past which everything is a
// package name, a URL or a file. Flag values are skipped by looking for a known
// command rather than by trusting a position: `debark --config x build` must
// not report the command as "x".
func classifyCommand(argv []string) string {
	var tokens []string
	if len(argv) < 2 {
		return ""
	}
	for _, a := range argv[1:] {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		tokens = append(tokens, a)
	}
	for i := range tokens {
		var pair string
		if i+1 < len(tokens) {
			pair = tokens[i] + " " + tokens[i+1]
		}
		for _, c := range errCommands {
			if c == pair || c == tokens[i] {
				return c
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Shaping a captured group
// ---------------------------------------------------------------------------

// errShape says how a regexp capture is made fit for a one-line summary.
// Every shape clips its result, so no stderr — however hostile or however
// large — can produce an unbounded message.
type errShape uint8

const (
	// errShapeText is the default: whitespace collapsed, length clipped.
	errShapeText errShape = iota
	// errShapePath keeps the last two path elements. The full path is in the
	// drawer; a summary that spans a window with a home directory is worse
	// than one that names the file.
	errShapePath
	// errShapeMount keeps the FIRST two path elements: the opposite end, for
	// the one question a tail cannot answer. "Which disk filled up" is
	// answered by /out/bundle and not by
	// …/libonig5/.tmp-3630954993, which is a temporary name the operator has
	// never seen and cannot act on.
	errShapeMount
	// errShapeURL keeps host and last path element, and nothing else — no
	// credentials, no query string, no token.
	errShapeURL
	// errShapeList renders a comma/space separated list, deduplicated and
	// capped, so "17 packages" does not become a paragraph.
	errShapeList
)

const (
	errMaxFieldRunes   = 120
	errMaxSummaryRunes = 400
	errMaxHintRunes    = 800
	errMaxHintLines    = 4
	errListMax         = 4
)

// errShapeValue applies one shape.
func errShapeValue(shape errShape, s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	switch shape {
	case errShapePath:
		return errClip(errTailPath(s), errMaxFieldRunes)
	case errShapeMount:
		return errClip(errHeadPath(s), errMaxFieldRunes)
	case errShapeURL:
		return errClip(errSafeURL(s), errMaxFieldRunes)
	case errShapeList:
		return errClip(errList(s), errMaxFieldRunes)
	default: // errShapeText
		return errClip(strings.Join(strings.Fields(s), " "), errMaxFieldRunes)
	}
}

// errTailPath reduces a path to its last two elements, marking the cut.
func errTailPath(s string) string {
	s = strings.TrimRight(s, `/\`)
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) <= 2 {
		return s
	}
	return "…" + string(filepathSep(s)) + strings.Join(parts[len(parts)-2:], string(filepathSep(s)))
}

// errHeadPath reduces a path to its first two elements, marking the cut and
// preserving a leading separator so an absolute path still looks absolute.
func errHeadPath(s string) string {
	sep := string(filepathSep(s))
	lead := ""
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) {
		lead = sep
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) <= 2 {
		return s
	}
	return lead + strings.Join(parts[:2], sep) + sep + "…"
}

// filepathSep reports which separator a path is written with, so the shortened
// form still looks like the path the operator typed. This is presentation, not
// path handling: nothing here opens a file.
func filepathSep(s string) rune {
	if strings.Contains(s, `\`) && !strings.Contains(s, "/") {
		return '\\'
	}
	return '/'
}

// errSafeURL renders a URL as host plus final path element. Userinfo, query
// and fragment are dropped rather than redacted: there is no reason for a
// summary to carry any of them, and dropping cannot leak.
func errSafeURL(s string) string {
	s = strings.Trim(s, `"'<>,;`)
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		// Not parseable as a URL. Fall back to credential-stripped text.
		return strings.Join(strings.Fields(errCredentials.ReplaceAllString(s, "$1")), " ")
	}
	name := u.Path
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return u.Host
	}
	return u.Host + "/" + name
}

// errList renders a separated list: deduplicated, order preserved, capped at
// errListMax with a count of the remainder.
func errList(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})
	seen := make(map[string]bool, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, `"'.`)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	switch {
	case len(out) == 0:
		return ""
	case len(out) <= errListMax:
		return strings.Join(out, ", ")
	default:
		return fmt.Sprintf("%s and %d more", strings.Join(out[:errListMax], ", "), len(out)-errListMax)
	}
}

// errClip collapses a value to one line and bounds it in runes, not bytes, so
// a multi-byte character cannot be cut in half.
func errClip(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.ContainsAny(s, "\n\t") {
		s = strings.Join(strings.Fields(s), " ")
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	// Leave room for the ellipsis, so the result is at most maxRunes runes.
	keep := maxRunes - 1
	if keep < 1 {
		keep = 1
	}
	var i, n int
	for i = range s {
		if n == keep {
			break
		}
		n++
	}
	return strings.TrimRight(s[:i], " ,;:") + "…"
}

// errExpand substitutes shaped captures into a template. $1 is the first
// capture group; $$ is a literal dollar. A reference to a group that does not
// exist expands to nothing — but errMatch has already rejected any match with
// an empty capture, so a live template never sees one.
func errExpand(tmpl string, vals []string) string {
	if tmpl == "" || !strings.Contains(tmpl, "$") {
		return tmpl
	}
	var b strings.Builder
	b.Grow(len(tmpl))
	for i := 0; i < len(tmpl); i++ {
		c := tmpl[i]
		if c != '$' || i+1 >= len(tmpl) {
			b.WriteByte(c)
			continue
		}
		switch n := tmpl[i+1]; {
		case n == '$':
			b.WriteByte('$')
			i++
		case n >= '1' && n <= '9':
			if idx := int(n - '1'); idx < len(vals) {
				b.WriteString(vals[idx])
			}
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// The rule table
// ---------------------------------------------------------------------------

// errClassAny is the wildcard for errRule.class: the rule is about a shape of
// message rather than about one exit code.
const errClassAny = dferr.Class(-1)

// errMatchSilent matches a message region that is empty — debark exited
// without printing anything. It is not an edge case: it is what a failing
// `build` does, and (verified against a real binary) what a failing `verify`
// does too.
var errMatchSilent = regexp.MustCompile(`^$`)

// errRule is one row of the recognition table.
//
// A rule fires when the class matches (or class is errClassAny), the command
// matches (or cmd is ""), and match matches the message region. First match in
// table order wins, so specific rows go above general ones.
//
// summary and hint are templates: $1 is the first capture group, shaped by
// shapes[0] (errShapeText when shapes is short or nil). An empty summary means
// "keep what debark said"; an empty hint means "keep debark's hint, or fall
// through to the class floor".
type errRule struct {
	name     string
	class    dferr.Class
	cmd      string
	match    *regexp.Regexp
	shapes   []errShape
	summary  string
	hint     string
	hintWins bool
}

// errRules is the recognition table. Adding a newly-recognised failure is one
// row; it cannot break the fallback.
//
// The corpus behind it is in testdata/stderr — real stderr captured from a real
// binary at the commit docs/dev/cli-surface.md was verified against — plus the
// realistic text in internal/cliadapter/fake's failure constructors.
var errRules = []errRule{
	// -- Silent failures ------------------------------------------------
	//
	// These rows were the reason this file exists. A failing `build` printed
	// its BuildResult to stdout and returned a silenced error, so stderr was
	// empty and the exit code was genuinely all there was; `verify` behaved
	// the same way. Without a rule here the operator got dferr's one-line
	// class description and nothing to do about it.
	//
	// The core repository has since removed silentError (debark `3ed80bd`,
	// "Make a failing command say why on stderr, and name what failed"), so
	// against a current binary `build` and `verify` both print a message and
	// these rows no longer fire. They are kept, not deleted: the GUI is
	// shipped separately from the CLI and must stay usable against the
	// binaries an operator already has installed, which are exactly the ones
	// that fail in silence. The rows that replaced them for a current binary
	// are in "Incomplete" and "Verification" below.
	{
		name: "build/incomplete/silent", class: dferr.Incomplete, cmd: "build", match: errMatchSilent,
		summary:  "the bundle was written but it is not complete: a download failed, or an external .deb needs something the target cannot supply",
		hint:     "The build report lists every URL that failed and every dependency that could not be satisfied. Fix those inputs and build again — what has already been fetched is reused.",
		hintWins: true,
	},
	{
		name: "build/resolution/silent", class: dferr.Resolution, cmd: "build", match: errMatchSilent,
		summary:  "apt could not satisfy the request for this target, so no bundle was written",
		hint:     "The build report names the packages that could not be resolved. Check them against the catalogue for this target; a package the target's archive does not carry has to be added as a vendor .deb URL or a local file.",
		hintWins: true,
	},
	{
		name: "build/policy/silent", class: dferr.Policy, cmd: "build", match: errMatchSilent,
		summary:  "the plan broke a rule in the policy file or the approved-keys list, so nothing was written",
		hint:     "The build report lists each policy finding with the rule that produced it. Supply what the rule wants — usually an expected SHA-256 for a download, or a trusted archive key — or change the rule.",
		hintWins: true,
	},
	{
		name: "build/verification/silent", class: dferr.Verification, cmd: "build", match: errMatchSilent,
		summary:  "something the build downloaded did not match the digest or signature it was expected to have",
		hint:     "The build report names the file that failed. Re-run the build to rule out a corrupted download; if it fails again on the same file, the copy being served does not match what the archive says it should be.",
		hintWins: true,
	},
	{
		name: "build/environment/silent", class: dferr.Environment, cmd: "build", match: errMatchSilent,
		summary:  "the build could not run on this machine",
		hint:     "Check that a container runtime is installed and running, that the output folder is writable, and that there is free disk space. The readiness screen checks all three.",
		hintWins: true,
	},
	{
		name: "verify/verification/silent", class: dferr.Verification, cmd: "verify", match: errMatchSilent,
		summary:  "the bundle did not pass verification",
		hint:     "The verification report lists each problem and the file it is about. Do not install a bundle that fails verification: obtain a fresh copy from the machine that built it.",
		hintWins: true,
	},
	{
		name: "install/target-mismatch/silent", class: dferr.TargetMismatch, cmd: "install", match: errMatchSilent,
		summary:  "this bundle was not built for this machine",
		hint:     "Build a bundle from a snapshot of the machine you are installing on; a bundle carries the architecture and release it was resolved against.",
		hintWins: true,
	},
	{
		name: "doctor/verification/silent", class: dferr.Verification, cmd: "doctor", match: errMatchSilent,
		summary:  "doctor could not read that target",
		hint:     "Point it at a bundle folder — the one containing lock.json — or at a snapshot archive produced by `debark snapshot create`.",
		hintWins: true,
	},

	// -- Incomplete (exit 3) --------------------------------------------
	//
	// A current debark names the shortfall on stderr, in one line built by
	// internal/cli's buildFailureError:
	//
	//	build: <bundle>: incomplete: 1 URL that did not download: <url>
	//	the bundle holds everything that did resolve; supply the missing input
	//	as a local .deb (--local-dir) or drop it from the request, then re-run
	//
	// The summary is worth rewording only slightly — debark names the bundle
	// and the input, which is most of the job. The hint is not: it tells a
	// person at a window to pass --local-dir, a flag they cannot reach, and it
	// does not say the one thing they need, which is WHERE the reason lives.
	//
	// It lives in the event log and nowhere else. Driven three ways against
	// debark `3ed80bd` in debian:bookworm-slim, the stderr line was
	// byte-identical apart from the URL for a refused connection, a SHA-256
	// that did not match the operator's own --digest, and a TLS certificate
	// with no trust anchor; `BuildResult.FetchFailed` carried the URL and no
	// reason in all three. Only the warn-level `input.external` event's
	// `error` attr told the three apart. So the hint sends the operator to the
	// drawer, and names the three reasons it will find there.
	{
		name: "build/incomplete", class: dferr.Incomplete, cmd: "build",
		match:   regexp.MustCompile(`build: (\S+?): incomplete: (.+)`),
		shapes:  []errShape{errShapePath, errShapeText},
		summary: "the bundle at $1 was written, but it is not complete: $2",
		hint: "The bundle holds everything that did resolve. Open the details drawer: the log names the reason for each input that is missing — an unreachable host, " +
			"a certificate this machine does not trust, a SHA-256 that did not match the digest you supplied, or a dependency the target's archive does not carry. " +
			"Correct the input, or add the file from disk instead, and build again; whatever already downloaded is reused.",
		hintWins: true,
	},

	// -- Resolution (exit 5) --------------------------------------------
	//
	// The first row is the one that matters most on a machine with no working
	// network, and it is here because of what it replaced. Driven twice —
	// `--network none`, and an unroutable http_proxy — a build against a stock
	// base failed with:
	//
	//	debark: apt: base seeds do not resolve against this release's archive:
	//	apt (unable to locate package); init (unable to locate package)
	//
	// which rendered under the resolution floor as "Check the package names
	// against the catalogue for this target." The operator had not typed those
	// names: `apt` and `init` are the base definition's own seeds, and their
	// being unfindable means the indexes were never read. Sending someone to
	// check the spelling of a package they did not choose is worse than saying
	// nothing.
	{
		name: "resolution/base-seeds", class: dferr.Resolution,
		match:   regexp.MustCompile(`(?i)base seeds do not resolve against this release's archive`),
		summary: "the target's package indexes could not be read, so even the base system's own packages look missing",
		hint: "This is almost always the network rather than the packages: the seeds that failed belong to the base definition, not to anything you chose. " +
			"Check that this machine can reach the archive named in the target's sources, and check the proxy settings if it uses one, then build again.",
		hintWins: true,
	},
	{
		name: "resolution/no-candidate", class: dferr.Resolution, match: regexp.MustCompile(`(?is)unable to satisfy the request:\s*(.+?)\s+ha[sv]e? no installation candidate`),
		shapes:  []errShape{errShapeList},
		summary: "apt has no installation candidate for $1 on this target",
		hint:    "Check the name against the catalogue for this target: the package may not exist in the target's release, or may live in a component its sources do not enable. A vendor package has to be added as a .deb URL or a local file instead.",
	},
	{
		name: "resolution/unable-to-locate", class: dferr.Resolution, match: regexp.MustCompile(`(?i)unable to locate package\s+(\S+)`),
		shapes:  []errShape{errShapeText},
		summary: "the target's apt indexes contain no package called $1",
		hint:    "Check the spelling against the catalogue for this target, and check that the component it lives in is enabled in the target's sources.",
	},
	{
		name: "resolution/unmet", class: dferr.Resolution, match: regexp.MustCompile(`(?i)(?:unmet dependencies|broken packages|held broken packages)`),
		hint: "One of the selected packages needs a version the target's archive does not offer. Remove the package that conflicts, or build against a snapshot of the target rather than a stock base — a snapshot records what is actually installed.",
	},

	// -- Environment (exit 2) -------------------------------------------
	//
	// The first two rows are about the same wall from two sides, and both were
	// driven. debark picks a backend before it resolves anything, and when
	// neither is usable it says so in one compound sentence:
	//
	//	apt: auto backend unavailable: local: host distro "debian" does not
	//	match target distro "ubuntu"; container: container: no container
	//	runtime found (tried docker, podman)
	//
	// Both halves matter and the generic no-runtime row below reported only
	// one of them, so an operator on a Debian builder aiming at an Ubuntu
	// snapshot was told to install Docker without being told why apt on this
	// machine would not do. This row says both, and its hint keeps the second
	// way out — pick a target whose release matches — which "install Docker"
	// alone hides.
	{
		name: "environment/backend-unavailable", class: dferr.Environment,
		match:   regexp.MustCompile(`auto backend unavailable: local: (.+?); container: (?:container: )?(.+)`),
		shapes:  []errShape{errShapeText, errShapeText},
		summary: "neither way of resolving packages for that target works on this machine: apt here was refused ($1), and the container backend was refused ($2)",
		hint: "Either install a container runtime and start it — the readiness screen checks for one and says how — or pick a target whose release matches this machine's, " +
			"which lets apt resolve here directly with no container at all.",
		hintWins: true,
	},
	{
		// C9. Driven on Windows with Docker Desktop running Linux containers,
		// against debark `3ed80bd`:
		//
		//	debark: container: C:\...\debark.exe does not look like a Linux
		//	ELF binary (bad magic number '[77 90 144 0]' in record at byte 0x0)
		//
		// The container backend mounts a debark binary into the container and
		// re-execs it, and on a non-Linux host the executable it reaches for is
		// the .exe. debark's own hint is now correct and names --self-binary,
		// DEBARK_SELF_BINARY and the bin/debark-linux-<arch> convention —
		// an improvement on the Go struct field it named before — but it is
		// still a `go build` command line handed to someone who is looking at
		// a progress bar. The summary is worse: a magic-number dump in an
		// error dialog reads as a corrupt file, which is the wrong worry.
		name: "environment/self-binary", class: dferr.Environment,
		match:   regexp.MustCompile(`(?i)(\S+) does not look like a Linux ELF binary`),
		shapes:  []errShape{errShapePath},
		summary: "this build has to run apt inside a Linux container, and the only debark it can find to put in that container is $1, which is not a Linux program",
		hint: "Put a Linux build of debark named bin/debark-linux-<arch> beside the debark this application runs — that path is found with no further setting — " +
			"or run the build on Linux, or on WSL. Nothing is wrong with the target you picked; this machine cannot reach it from here.",
		hintWins: true,
	},
	{
		name: "environment/no-runtime", class: dferr.Environment, match: regexp.MustCompile(`(?i)no container runtime found`),
		summary: "no container runtime is available, and the local backend cannot resolve for a different release",
		hint:    "Install Docker or Podman on this machine, or build against a target whose release matches this machine's and choose the local backend.",
	},
	{
		name: "environment/docker-daemon", class: dferr.Environment, match: regexp.MustCompile(`(?i)cannot connect to the docker daemon|is the docker daemon running`),
		summary:  "Docker is installed but its daemon is not reachable from this account",
		hint:     "Start the Docker service, and check that this account is in the docker group (a group change needs a new login to take effect). Podman works without a daemon if that is easier.",
		hintWins: true,
	},
	{
		name: "environment/needs-linux", class: dferr.Environment, match: regexp.MustCompile(`(?i)requires linux \(dpkg/apt\); this host is (\w+)`),
		shapes:  []errShape{errShapeText},
		summary: "capturing the real system needs Linux with dpkg and apt, and this machine is $1",
		// debark's own hint here names a Go struct field, which is right for
		// a developer at a terminal and useless in a window.
		hint:     "Run `debark snapshot create` on the target itself and copy the snapshot file here, or build against a stock base OS instead of a snapshot.",
		hintWins: true,
	},
	{
		// A build writes to two places on two potentially different drives —
		// the output folder and the package store — so "the disk is full" is
		// only half an instruction. Driven against a 256 KiB tmpfs mounted at
		// the output folder, the message named the file it was writing:
		//
		//	debark: bundle: materialise pool/…/libonig5_6.9.8-1_amd64.deb:
		//	store: … write /out/bundle/repo/pool/…/.tmp-2485458070: no space
		//	left on device
		//
		// so the row that names it goes above the one that does not. The path
		// is shaped from its HEAD, not its tail: the tail here is
		// ".tmp-3630954993", a name the operator has never seen, and the head
		// is what tells the output folder from the store.
		name: "environment/no-space-at", class: dferr.Environment,
		match:    regexp.MustCompile(`(?i)(?:write|open|create|copy to) (\S+?): no space left on device`),
		shapes:   []errShape{errShapeMount},
		summary:  "the drive holding $1 filled up before the work finished",
		hint:     "Free space there and run this again. A build writes to two places — the output folder and the package store — and either can be the one that filled; a full bundle for a desktop target can be several gigabytes.",
		hintWins: true,
	},
	{
		name: "environment/no-space", class: dferr.Environment, match: regexp.MustCompile(`(?i)no space left on device|not enough space`),
		summary:  "the disk filled up before the work finished",
		hint:     "Free space on the drive holding the output folder and on the one holding the package store, then run this again. A full bundle for a desktop target can be several gigabytes.",
		hintWins: true,
	},
	{
		// A TLS certificate with no trust anchor is what a company proxy that
		// inspects HTTPS looks like from the inside, and it is common in
		// exactly this audience. It deserves its own row because the network
		// row below sends the operator to look at connectivity, which is fine
		// and which is not the problem: the connection worked.
		//
		// HONESTY NOTE. This row has NOT been seen to fire. Driven for real —
		// an https:// vendor .deb in an image with no ca-certificates — the
		// text does not reach stderr at all: the build exits 3 with the
		// incomplete line above, and "x509: certificate signed by unknown
		// authority" appears only in the warn-level `input.external` event's
		// `error` attr (docs/dev/cli-surface.md §5.3, re-confirmed here). The
		// row is kept for the paths where the same failure IS classified
		// environment and does reach stderr — `snapshot from-base`, and any
		// future command that fetches outside the build — and the build path
		// is covered instead by naming the cause in build/incomplete's hint.
		name: "environment/tls-untrusted", class: dferr.Environment,
		match:   regexp.MustCompile(`(?i)x509: certificate signed by unknown authority|failed to verify certificate`),
		summary: "the HTTPS certificate offered for that download was signed by an authority this machine does not trust",
		hint: "This is what a company proxy that inspects HTTPS looks like from the inside — the connection worked, the certificate did not. " +
			"Install your organisation's root certificate in this machine's trust store (on Debian and Ubuntu, put the .crt under /usr/local/share/ca-certificates and run `update-ca-certificates`). " +
			"On a stripped-down machine the same message means the ca-certificates package is not installed at all.",
		hintWins: true,
	},
	{
		name: "environment/permission", class: dferr.Environment, match: regexp.MustCompile(`(?i)permission denied|access is denied|operation not permitted`),
		hint: "The account running this app cannot read or write that path. Choose a different location, or fix the folder's permissions.",
	},
	{
		name: "environment/network", class: dferr.Environment, match: regexp.MustCompile(`(?i)no such host|connection refused|connection reset|i/o timeout|network is unreachable|tls handshake timeout|proxyconnect|certificate (?:is not|has expired|signed by unknown)`),
		hint: "This machine could not reach the distro archive. Check the network, and check the proxy settings if this machine uses one — the builder needs to reach the hosts named in the target's own sources.",
	},

	// -- Verification (exit 4) ------------------------------------------
	//
	// The three snapshot rows come first because `snapshot inspect` is the one
	// command where exit 4 is not about a bundle at all, and the class floor
	// beneath them is written entirely about bundles. Driven with a snapshot
	// truncated to its first 4 KiB — the likeliest real corruption, a copy that
	// did not finish — the operator was told "Do not install a bundle that
	// fails verification. Obtain a fresh copy from the machine that built it,
	// and check that the key you trust is the one it was signed with." There
	// was no bundle and no signature anywhere in the story.
	{
		name: "snapshot/truncated", class: dferr.Verification, cmd: "snapshot inspect",
		match:    regexp.MustCompile(`(?i)zstd decode: unexpected EOF|unexpected EOF`),
		summary:  "that snapshot file is incomplete: it ends part-way through",
		hint:     "The copy did not finish. Copy the .tar.zst again from the machine that made it, and let the copy complete before ejecting the drive — a snapshot cut short looks exactly like this.",
		hintWins: true,
	},
	{
		name: "snapshot/not-zstd", class: dferr.Verification, cmd: "snapshot inspect",
		match:    regexp.MustCompile(`(?i)zstd decode: invalid input|magic number mismatch`),
		summary:  "that file is not a debark snapshot: its contents are not a zstd archive",
		hint:     "Pick the .tar.zst file that `debark snapshot create` wrote on the target machine. A file that was edited, renamed from something else, or truncated by a copy fails this way.",
		hintWins: true,
	},
	{
		// The catch-all for the command, so that no future exit-4 shape out of
		// `snapshot inspect` can inherit the bundle-shaped class floor. It
		// leaves debark's own summary alone and replaces only the hint.
		name: "snapshot/unreadable", class: dferr.Verification, cmd: "snapshot inspect",
		match:    regexp.MustCompile(`(?s).+`),
		hint:     "That file could not be read as a snapshot. Pick the .tar.zst that `debark snapshot create` wrote on the target machine, or choose a stock base OS on the target screen instead.",
		hintWins: true,
	},
	{
		// The shape a tampered bundle actually produces, verified by appending
		// one byte to a file inside a freshly built, signed bundle:
		//
		//	debark: verify: /work/tampered failed verification: 1 problem:
		//	repo/Packages: file size does not match the manifest
		//
		// The digest row below never matched it: it wants "<file>.deb: digest
		// mismatch", and neither the wording nor the .deb suffix was there.
		name: "verification/manifest-mismatch", class: dferr.Verification,
		match:   regexp.MustCompile(`(?i)(\S+?): (?:file size|digest|checksum|content) does not match the manifest`),
		shapes:  []errShape{errShapePath},
		summary: "$1 inside the bundle is not the file that was signed",
		hint: "The bundle is not byte-for-byte the one that was signed, so it must not be installed. Copy it again from the machine that built it; " +
			"if a fresh copy fails on the same file, treat the media it travelled on as suspect.",
		hintWins: true,
	},
	{
		name: "verification/digest", class: dferr.Verification, match: regexp.MustCompile(`(?i)(\S+\.deb)\s*:?\s*digest mismatch`),
		shapes:  []errShape{errShapePath},
		summary: "$1 does not match the digest recorded in the signed manifest",
		hint:    "The bundle is not byte-for-byte the one that was signed. Do not install it: obtain a fresh copy from the machine that built it, and if a fresh copy fails the same way, treat the media as suspect.",
	},
	{
		// This row said "that folder is not a debark bundle" and told the
		// operator to pick the folder inside the one they chose. Core
		// 5837d82 made that wrong, and driving it proved it: a REAL signed
		// bundle with its manifest deleted still exits 4 (verification) and
		// still reaches here, and a directory that is not a bundle at all now
		// exits 1 (usage) and is handled by verify/not-a-bundle below. The
		// two are no longer the same failure and must not share a sentence.
		//
		// Driven through the export screen's "Check the copy" action against
		// core 5d26dac: a freshly built, signed, exported bundle with only
		// debark.manifest.json removed gave
		//
		//	debark: verify: /sp/exp1 failed verification: 1 problem:
		//	debark.manifest.json: no debark.manifest.json in this bundle
		//
		// at exit 4, and the screen answered "Pick the bundle folder itself
		// … rather than the folder above it." They had picked the right
		// folder. The one file that says what the bundle should contain was
		// gone, which is either tampering or a copy that stopped, and the
		// exit code core assigns says exactly that.
		//
		// The wrong-folder advice is not lost: it moved to the row that now
		// owns the wrong-folder case, at the exit code core moved it to.
		name: "verification/no-manifest", class: dferr.Verification, match: regexp.MustCompile(`(?i)no debark\.manifest\.json|manifest-missing`),
		summary: "this bundle has no debark.manifest.json — the one file that says what it should contain",
		hint: "Everything in a bundle is checked against that manifest, so with it gone nothing here can be checked at all. Do not install it. " +
			"Copy the bundle again from the machine that built it: a copy that stopped part-way looks exactly like this, and so does a folder somebody has edited. " +
			"If a fresh copy is missing it too, treat the media it travelled on as suspect.",
		hintWins: true,
	},
	{
		name: "verification/not-zstd", class: dferr.Verification, match: regexp.MustCompile(`(?i)zstd decode: invalid input|magic number mismatch`),
		summary:  "that file is not a debark snapshot: its contents are not a zstd archive",
		hint:     "Pick the .tar.zst file that `debark snapshot create` wrote on the target. A snapshot that was edited, renamed from something else, or truncated by a copy will fail this way.",
		hintWins: true,
	},
	{
		// An UNSIGNED bundle, which is a different failure from an untrusted
		// one and was rendering as an untrusted one.
		//
		// Driven end to end: the build screen's third signing option writes an
		// unsigned bundle deliberately, raises its own "This bundle will be
		// unsigned" banner, and the result panel says "Unsigned" again. Export
		// that bundle, press "Check the copy with debark verify", and the
		// screen said:
		//
		//	the bundle did not verify: bundle is not signed and AllowUnsigned
		//	was not given
		//	the operator public key must reach this machine independently of
		//	the bundle's media: pass it with --key, or list it under
		//	verify_keys in the config file
		//
		// That hint is debark's own and it is right at a terminal for a
		// bundle that IS signed by a key this machine does not hold. Here
		// there is no key to bring: nothing signed it, and the application
		// knew that, having been told to build it that way and having said so
		// twice. The rules below never matched it — "not signed and" is not
		// "not signed by" — so debark's hint passed straight through.
		//
		// The summary is replaced as well as the hint, because "AllowUnsigned"
		// is a Go field name and this sentence is read by an operator.
		name: "verification/unsigned", class: dferr.Verification,
		match:    regexp.MustCompile(`(?i)(?:bundle )?is not signed and allowunsigned was not given|no signature block|bundle is unsigned`),
		summary:  "this bundle carries no signature, and a bundle has to be signed for anything to check who produced it",
		hint:     "There is no key to add: nothing signed this bundle. If you built it here, the signing option was set to write an unsigned bundle — build again with a signing key if the offline machine has to be able to check who produced it. If it came from someone else, ask them for a signed copy rather than accepting this one. An unsigned bundle can still be installed, with `--allow-unsigned`, and that is a decision rather than a default.",
		hintWins: true,
	},
	{
		// The same "wrong folder" correction the verification rules make, at
		// the exit code core 5837d82 moved it to. `verify` on a directory that
		// has never been near debark now exits 1 (usage) with ZERO bytes on
		// stdout, so there is no report to read and this rule is the only
		// thing standing between the operator and the usage class floor —
		// which says "this is usually a mismatch between this app and the
		// debark it is driving. Check the debark version". They picked the
		// wrong folder.
		//
		// The boundary is structural and this rule must not widen past it: a
		// REAL bundle whose manifest was deleted still exits 4 and is still
		// tampering. See cli-surface.md C10.
		name: "verify/not-a-bundle", class: dferr.Usage, cmd: "verify",
		match:    regexp.MustCompile(`is not a debark bundle`),
		hint:     "Pick the bundle folder itself — the one `debark build` wrote, containing debark.manifest.json and lock.json — rather than the drive's root or a folder beside it.",
		hintWins: true,
	},
	{
		// "no signature verifies against a trusted key" is the wording a real
		// binary produces, and the alternation below did not have it: driven by
		// deleting the .pub file beside the signing key, the operator got
		// debark's own hint about passing --key or editing verify_keys in a
		// config file. Both are the right answer at a terminal and neither is
		// reachable from this window: the application already passes the public
		// key it finds beside the signing key (cliadapter.KeyedVerifier), so
		// the actionable fact is that the file is not there.
		name: "verification/signature", class: dferr.Verification,
		match: regexp.MustCompile(`(?i)no valid signature|no signature verifies against a trusted key|signature (?:verification )?failed|bad signature|unknown signer|not signed by`),
		hint: "Nothing here trusts the key this bundle was signed with. If you built it on this machine, the operator public key that was written beside the signing key is missing — the readiness screen names the path it looks at. " +
			"If it came from someone else, get their public key by a route other than the media the bundle arrived on, and add it to the trusted keys.",
		hintWins: true,
	},
	{
		name: "verification/release", class: dferr.Verification, match: regexp.MustCompile(`(?i)release file .*(?:expired|is not valid|not signed)|repository metadata`),
		hint: "The archive's own metadata did not check out. This is usually a stale mirror or a captive portal returning a login page; try again, and check whether this network intercepts HTTP.",
	},

	// -- Policy (exit 6) ------------------------------------------------
	{
		name: "policy/url-unverified", class: dferr.Policy, match: regexp.MustCompile(`policy: ([A-Za-z0-9._-]+):.*?url-unverified: (\S+)`),
		shapes:  []errShape{errShapeText, errShapeURL},
		summary: "policy rule $1 rejected the plan: the download from $2 has no expected digest",
		hint:    "Supply the vendor's published SHA-256 for that download so it can be checked, or relax the rule in the policy file. The digest is the only thing that makes a vendor URL reproducible.",
	},
	{
		name: "policy/approved-keys", class: dferr.Policy, match: regexp.MustCompile(`(?i)approved[- ]keys|not in the approved key|unapproved key`),
		hint: "An archive key used by this target is not in the approved-keys list. Add its fingerprint to that list if you trust it, or drop the source that uses it.",
	},
	{
		name: "policy/rule", class: dferr.Policy, match: regexp.MustCompile(`policy: ([A-Za-z0-9._-]+):`),
		shapes:  []errShape{errShapeText},
		summary: "the local policy rule $1 rejected this plan",
		hint:    "Either change the inputs so the rule passes, or change the rule in the policy file. Policy is evaluated against the plan, so nothing was downloaded.",
	},

	// -- Target mismatch (exit 7) ---------------------------------------
	{
		// NOT DRIVEN, and it says so. `install` runs on the offline target and
		// the Adapter has no method for it, so nothing in this application can
		// produce this failure; an arm64 target to install on could not be
		// reached from this machine either (see docs/dev/error-catalogue.md's
		// "what could not be caused here").
		//
		// The alternation is here because the wording it was written against
		// no longer exists. debark `3ed80bd` rewrote the line as
		// "install: <bundle> did not reach the requested state: the bundle is
		// for amd64, this machine is arm64" — a comma where there was a
		// semicolon — which the old pattern silently stopped matching. Read
		// from internal/cli/cmd_install.go at commit 6549f6f, not from a run.
		name: "target/bundle-for", class: dferr.TargetMismatch,
		match:   regexp.MustCompile(`(?im)th(?:is|e) bundle is for (.+?)[;,] this machine is (.+?)$`),
		shapes:  []errShape{errShapeText, errShapeText},
		summary: "this bundle was built for $1, and this machine is $2",
		hint:    "Build a bundle from a snapshot of the machine you are installing on. Architecture and release are recorded when the bundle is resolved and cannot be changed afterwards.",
	},

	// -- Usage (exit 1) -------------------------------------------------
	//
	// The GUI builds every command line itself, so a usage error is almost
	// never something the operator did: it is version skew between this app and
	// the debark it found, or a bug here. The hints say so.
	{
		// Driven by pointing a build at a signing key that is not there:
		//
		//	debark: sign: read private key /work/absent.key: open
		//	/work/absent.key: no such file or directory
		//	generate one with "debark key generate", or pass an existing .key file
		//
		// Without this row the any/missing-file-unix row below matched, and
		// the operator was shown a bare path and "the file may have been moved
		// or renamed" — true of any file, and silent about the fact that this
		// one is the signing key and that the application can make another.
		//
		// It also drops debark's own hint on purpose, and not only because
		// it names a flag: "debark key generate" is not a command this CLI
		// has. The command is `debark keygen --out PATH`. Reported upstream.
		name: "usage/sign-key-missing", class: dferr.Usage, cmd: "build",
		match:   regexp.MustCompile(`sign: read private key (\S+?): (?:open|stat|lstat) \S+?: (?:no such file or directory|The system cannot find)`),
		shapes:  []errShape{errShapePath},
		summary: "the build was told to sign with the key at $1, and there is no file there",
		hint: "Create an operator signing key — the readiness screen has an action that does it — or point the signing key setting at a .key file that exists. " +
			"Building without a signature is the other option, and it is a deliberate choice rather than a default.",
		hintWins: true,
	},
	{
		// A debark too old to know `snapshot list-bases` swallows the
		// subcommand and lets cobra reject the flag on the parent group
		// (docs/dev/cli-surface.md C6), so the only thing on stderr is
		// "unknown flag: --arch". Driven against a binary built from debark
		// `10ec7bf`, the commit before list-bases existed, the operator was
		// told an option was unsupported when the whole feature is.
		//
		// --arch is the only flag this application ever passes to this
		// command, so "unknown flag" here can mean nothing else.
		name: "usage/bases-too-old", class: dferr.Usage, cmd: "snapshot list-bases",
		match:    regexp.MustCompile(`unknown (?:shorthand )?flag`),
		summary:  "this debark is older than this application: it has no `snapshot list-bases`, so it cannot offer stock base targets",
		hint:     "Choose a snapshot file taken from the target machine instead — that works with every version — or install a newer debark. The About panel shows which one this application is running.",
		hintWins: true,
	},
	{
		// The same skew, one screen later, driven against the same binary.
		name: "usage/build-base-too-old", class: dferr.Usage, cmd: "build",
		match:    regexp.MustCompile(`unknown (?:shorthand )?flag: --base`),
		summary:  "this debark is too old to build against a stock base OS: it can only build from a snapshot",
		hint:     "Choose a snapshot file taken from the target machine, or install a newer debark. The About panel shows which one this application is running.",
		hintWins: true,
	},
	{
		// The 2d6d5c7 wording, from three driven variants: random bytes named
		// .tar.zst, a bare snapshot.json, and a directory. debark's summary
		// is good and says which of the three it was; its hint names two CLI
		// commands, one of which (`snapshot from-base`) this application never
		// runs and which duplicates the target screen's own base picker.
		name: "snapshot/not-a-snapshot", class: dferr.Usage, cmd: "snapshot inspect",
		match:    regexp.MustCompile(`is not a debark snapshot`),
		hint:     "Pick the .tar.zst file that `debark snapshot create` wrote on the target machine. If you do not have one, choose a stock base OS on the target screen instead — it assumes a clean install of that release rather than describing a real machine.",
		hintWins: true,
	},
	{
		name: "snapshot/missing-file", class: errClassAny, cmd: "snapshot inspect",
		match:    regexp.MustCompile(`(?:open|stat|lstat|GetFileAttributesEx) (\S+?): (?:.*: )?(?:no such file or directory|The system cannot find)`),
		shapes:   []errShape{errShapePath},
		summary:  "there is no snapshot file at $1",
		hint:     "Choose the file again. If it was on a removable drive, plug the drive back in and wait for it to appear before picking it.",
		hintWins: true,
	},
	{
		name: "usage/unknown-command", class: dferr.Usage, match: regexp.MustCompile(`unknown command "([^"]+)" for`),
		shapes:  []errShape{errShapeText},
		summary: "this debark has no `$1` command",
		hint:    "The app asked for something this debark cannot do. Check the debark version shown in settings: this is a mismatch between the two, not something you typed.",
	},
	{
		name: "usage/unknown-flag", class: dferr.Usage, match: regexp.MustCompile(`unknown (?:shorthand )?flag: (-{1,2}[A-Za-z0-9-]+)`),
		shapes:  []errShape{errShapeText},
		summary: "this debark does not accept the option $1",
		hint:    "The app built a command line this debark does not understand. Check the debark version shown in settings; the command it tried is below.",
	},
	{
		name: "usage/required-flag", class: dferr.Usage, match: regexp.MustCompile(`required flag\(s\) "([^"]+)" not set`),
		shapes:  []errShape{errShapeList},
		summary: "debark needs the $1 option and it was not supplied",
		hint:    "Fill in the matching field before running this again. If the field is already filled in, this is a bug in the app: the command it tried is below.",
	},
	{
		name: "usage/build-no-target", class: dferr.Usage, cmd: "build", match: regexp.MustCompile(`at least one of the flags in the group \[[^\]]*\] is required`),
		summary:  "no target was chosen: a build needs either a stock base OS or a snapshot file",
		hint:     "Pick a target on the first screen. A stock base describes a clean install of a release; a snapshot describes a machine as it actually is.",
		hintWins: true,
	},
	{
		name: "usage/group-required", class: dferr.Usage, match: regexp.MustCompile(`at least one of the flags in the group \[([^\]]+)\] is required`),
		shapes:  []errShape{errShapeList},
		summary: "this command needs one of these options and got none of them: $1",
		hint:    "The app left out something debark requires. The command it tried is below.",
	},
	{
		name: "usage/build-exclusive", class: dferr.Usage, cmd: "build", match: regexp.MustCompile(`if any flags in the group \[[^\]]*snapshot[^\]]*\] are set none of the others can be`),
		summary:  "a target was given twice: --base and --snapshot describe the same thing two different ways",
		hint:     "Choose one target — a stock base OS, or a snapshot file — and clear the other.",
		hintWins: true,
	},
	{
		name: "usage/group-exclusive", class: dferr.Usage, match: regexp.MustCompile(`if any flags in the group \[([^\]]+)\] are set none of the others can be`),
		shapes:  []errShape{errShapeList},
		summary: "these options contradict each other and only one may be set: $1",
		hint:    "Clear all but one of them and try again.",
	},
	{
		name: "usage/arg-count", class: dferr.Usage, match: regexp.MustCompile(`accepts (\d+) arg\(s\), received (\d+)`),
		shapes:  []errShape{errShapeText, errShapeText},
		summary: "this command takes exactly $1 argument and was given $2",
		hint:    "The app built the command line, so this is a bug here rather than something you did. The command it tried is below.",
	},
	{
		name: "usage/arch-with-snapshot", class: dferr.Usage, match: regexp.MustCompile(`--arch describes a --base`),
		summary:  "an architecture override was set alongside a snapshot, and a snapshot already records the target's architecture",
		hint:     "Clear the architecture override, or choose a stock base OS instead of a snapshot.",
		hintWins: true,
	},
	{
		name: "usage/unknown-base", class: dferr.Usage, match: regexp.MustCompile(`base: "([^"]+)" is neither a builtin base id nor a file that exists`),
		shapes:  []errShape{errShapeText},
		summary: "$1 is not one of debark's builtin base IDs, and there is no file by that name",
		// debark's own hint lists all fifteen builtin bases. That is the
		// right answer at a terminal and noise next to a picker that already
		// shows them.
		hint:     "Pick a base from the list this app loaded from debark, or point at a base definition file that exists.",
		hintWins: true,
	},
	{
		// No summary: debark's own line already names the architecture that
		// was rejected AND every one it accepts, which is more than a rule can
		// say without clipping the list. The row exists for the hint.
		name: "usage/unknown-arch", class: dferr.Usage, match: regexp.MustCompile(`base: unknown architecture "[^"]+"; known architectures are `),
		hint:     "Choose one of the architectures named above. The architecture describes the target, so it normally comes from the base or the snapshot that was picked rather than being typed.",
		hintWins: true,
	},
	{
		name: "usage/config-parse", class: dferr.Usage, match: regexp.MustCompile(`config: parse (\S+?): (.+)`),
		shapes:  []errShape{errShapePath, errShapeText},
		summary: "the config file $1 could not be read: $2",
		hint:    "Fix that file or move it aside; debark falls back to its defaults when there is no config file at all.",
	},
	{
		name: "usage/doctor-target", class: dferr.Usage, match: regexp.MustCompile(`doctor: give exactly one of BUNDLE or --snapshot`),
		summary:  "doctor needs exactly one target: a bundle folder, or a snapshot file",
		hint:     "Choose one of the two and run it again.",
		hintWins: true,
	},

	// -- Shapes that can appear under more than one class ----------------
	//
	// Below the class-specific rows on purpose: a resolution failure that
	// happens to mention a missing file should still be read as a resolution
	// failure.
	{
		name: "any/not-a-bundle", class: errClassAny, match: regexp.MustCompile(`lock: read (\S+?)[\\/]lock\.json`),
		shapes:   []errShape{errShapePath},
		summary:  "$1 is not a debark bundle: it has no lock.json",
		hint:     "Pick the bundle folder itself — the one containing lock.json and debark.manifest.json — rather than the folder above it.",
		hintWins: true,
	},
	{
		name: "any/missing-file-windows", class: errClassAny, match: regexp.MustCompile(`GetFileAttributesEx (\S+?): The system cannot find the file`),
		shapes:   []errShape{errShapePath},
		summary:  "$1 does not exist",
		hint:     "Check the path: the file may have been moved or renamed, or the drive it is on may not be connected.",
		hintWins: true,
	},
	{
		name: "any/missing-file-unix", class: errClassAny, match: regexp.MustCompile(`open (\S+?): no such file or directory`),
		shapes:   []errShape{errShapePath},
		summary:  "$1 does not exist",
		hint:     "Check the path: the file may have been moved or renamed, or the volume it is on may not be mounted.",
		hintWins: true,
	},
	{
		name: "any/is-a-directory", class: errClassAny, match: regexp.MustCompile(`(?i)(\S+): is a directory`),
		shapes:  []errShape{errShapePath},
		summary: "$1 is a folder, and a file was expected here",
	},

	// -- The floor's companion, and the last row -------------------------
	//
	// Any silent failure not recognised above. It leaves the summary to the
	// class floor and only improves the hint, by saying where the detail
	// actually is: the command's JSON document, because the message that would
	// have explained it was discarded before it reached stderr.
	//
	// Keep this row LAST. A new command-specific silent rule added above it
	// will win; one added below it would never fire.
	{
		// The C6 trap, driven against a debark built from `10ec7bf`. An
		// unknown subcommand of a group makes cobra run the PARENT, which has
		// no RunE, so it prints the group's help to stdout and exits 0. The
		// adapter then fails to decode a baselist document and reports exit 0
		// with empty stderr — and the success/silent row below called that a
		// bug worth reporting, which sent the operator to write a bug report
		// about a debark that is simply older than this application.
		//
		// No summary: invoke.go's decode error already says what was printed,
		// and it is more specific than anything a rule could add.
		name: "bases/success/silent", class: dferr.Success, cmd: "snapshot list-bases", match: errMatchSilent,
		hint: "This is nearly always a debark older than this application: one with no `snapshot list-bases` prints the snapshot group's help and exits 0, " +
			"so it looks like success. Choose a snapshot file as the target instead, or install a newer debark.",
		hintWins: true,
	},
	{
		// Exit 0 with an error means debark did the work and this app could
		// not read what it printed. The generic silent row below would tell the
		// operator to go and read that report, which is precisely the thing
		// that could not be read.
		name: "success/silent", class: dferr.Success, match: errMatchSilent,
		summary:  "debark finished successfully, but this app could not read the output it printed",
		hint:     "Run the command shown below in a terminal. Its output will show what debark printed that this app did not understand, which is a bug worth reporting.",
		hintWins: true,
	},
	{
		name: "any/silent", class: errClassAny, match: errMatchSilent,
		hint: "This was reported through debark's JSON document instead of a message, so there is no text to show. The details below carry the command that was run; the report it printed has the specifics.",
	},
}

// errMatch finds the first rule that applies, and shapes its captures.
//
// A rule is rejected when any of its capture groups is empty after shaping.
// That is the guard against a brittle regexp producing a confidently wrong
// sentence: an unfilled template reads as a bug, and the generic class-level
// message it falls back to does not.
func errMatch(class dferr.Class, cmd, region string) (errRule, []string, bool) {
	region = strings.TrimSpace(region)
	for _, rule := range errRules {
		if rule.class != errClassAny && rule.class != class {
			continue
		}
		if rule.cmd != "" && rule.cmd != cmd {
			continue
		}
		m := rule.match.FindStringSubmatch(region)
		if m == nil {
			continue
		}
		vals := make([]string, 0, len(m)-1)
		ok := true
		for i, g := range m[1:] {
			shape := errShapeText
			if i < len(rule.shapes) {
				shape = rule.shapes[i]
			}
			v := errShapeValue(shape, g)
			if v == "" {
				ok = false
				break
			}
			vals = append(vals, v)
		}
		if !ok {
			continue
		}
		return rule, vals, true
	}
	return errRule{}, nil, false
}

// ---------------------------------------------------------------------------
// The floor
// ---------------------------------------------------------------------------

// errText is a summary/hint pair. Both halves are always populated in the
// tables below; errFloor's own guards make that structural rather than a
// convention.
type errText struct {
	summary string
	hint    string
}

// errClassTexts is the floor for the frozen dferr table: the sentence every
// failure of that class gets when nothing more specific was recognised.
//
// The summaries are close to dferr.Class.Description() and deliberately not
// identical: Description() is a table entry for a manual page, and this is what
// a person reads in a dialog. Description() remains the last resort in
// errFloor, so a class added to dferr still produces a sentence — and
// TestClassifyClassTextsCoverEveryClass fails until someone writes a better one
// here.
//
// dferr.Success needs its own wording rather than "success; bundle matches the
// request": an *Error carrying class Success means debark succeeded and this
// app could not use what it printed. Rendering the success description there
// would be the single most confusing message in the app.
var errClassTexts = map[dferr.Class]errText{
	dferr.Success: {
		summary: "debark finished successfully, but this app could not read the output it printed",
		hint:    "This is a bug in the app rather than something you did. Run the command shown below in a terminal and report what it prints.",
	},
	dferr.Usage: {
		summary: "debark rejected the command this app built for it",
		hint:    "This is usually a mismatch between this app and the debark it is driving. Check the debark version in settings, and report the command shown below.",
	},
	dferr.Environment: {
		summary: "this machine cannot do the job as it is currently set up",
		hint:    "Check the three things debark needs: a container runtime it can start, a writable output folder, and free disk space. The readiness screen checks all three.",
	},
	dferr.Incomplete: {
		summary: "the bundle was written but it is not complete",
		hint:    "A download failed, or an external .deb needs something the target cannot supply. The report lists which; fix those inputs and build again — what has already been fetched is reused.",
	},
	dferr.Verification: {
		summary: "something did not match the digest or signature it was expected to have",
		hint:    "Do not install a bundle that fails verification. Obtain a fresh copy from the machine that built it, and check that the key you trust is the one it was signed with.",
	},
	dferr.Resolution: {
		summary: "apt could not satisfy the request for this target",
		hint:    "Check the package names against the catalogue for this target. A package the target's archive does not carry has to be added as a vendor .deb URL or a local file instead.",
	},
	dferr.Policy: {
		summary: "the plan broke a rule in the policy file or the approved-keys list",
		hint:    "Supply what the rule wants — usually an expected SHA-256 for a download, or a trusted archive key — or change the rule. Nothing was downloaded: policy is evaluated against the plan.",
	},
	dferr.TargetMismatch: {
		summary: "this bundle was built for a different target than the machine it was given to",
		hint:    "Build a bundle from a snapshot of the machine you are installing on. Architecture and release are fixed when the bundle is resolved.",
	},
}

// errExitTexts covers exit codes outside the frozen 0–7 table by exact value.
//
// NewError already maps every one of these to dferr.Environment rather than
// dferr.Usage — a process that died in a way debark did not choose is a fact
// about the machine, not about the operator's flags — but "environment" on its
// own does not tell anyone that the binary is missing or that Windows killed
// the process. These do.
var errExitTexts = map[int]errText{
	126: {
		summary: "the debark binary was found but could not be run",
		hint:    "The file exists but is not executable by this account. Check its permissions, or reinstall the debark package.",
	},
	127: {
		summary: "debark could not be started: no such program on this machine",
		hint:    "Install the debark package, or set the full path to the binary in settings. The path this app tried is in the command below.",
	},
	// Windows NTSTATUS values, reported verbatim by os/exec as the exit code.
	0xC0000005: {
		summary: "debark crashed with a memory access violation",
		hint:    "This is a bug in debark, not a configuration problem. Report the command below along with the debark version in settings.",
	},
	0xC0000135: {
		summary: "debark could not start: a DLL it needs is missing",
		hint:    "The installation is incomplete. Reinstall debark, or point settings at a different copy of the binary.",
	},
	0xC000013A: {
		summary: "debark was interrupted before it finished",
		hint:    "Nothing reached the target. Any partial output folder can be deleted, or reused by running this again.",
	},
}

// errInTable reports whether an exit code is one debark itself chose, i.e.
// one of the eight frozen dferr classes. Everything else — 127 from a shell
// that could not find the binary, -1 from a signal, a Windows exception status
// — is a fact about the machine.
func errInTable(exitCode int) bool {
	return exitCode >= int(dferr.Success) && exitCode <= int(dferr.TargetMismatch)
}

// errMeaningful returns s when it reads as prose, and "" when it does not.
//
// The test is one run of three or more ASCII letters — one word. A line that
// survives cleaning as replacement characters, punctuation or binary debris is
// worse than the class-level sentence it would displace, so it is discarded and
// the floor answers instead. debark's messages are English and every one of
// them clears this by a wide margin; the cost is that a hypothetical message
// with no ASCII word in it at all would be replaced by its class sentence, and
// the bytes would still be in the details drawer.
func errMeaningful(s string) string {
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			run++
			if run >= 3 {
				return s
			}
			continue
		}
		run = 0
	}
	return ""
}

// errFloor is the last stop. It is total: it returns two non-empty strings for
// every class and every exit code, including ones that do not exist yet.
//
// Nothing here ever renders a bare number. Where an exit code has to appear —
// because there is genuinely nothing else known about it — it appears inside a
// sentence that says what it means.
func errFloor(class dferr.Class, exitCode int) (summary, hint string) {
	if t, ok := errClassTexts[class]; ok {
		summary, hint = t.summary, t.hint
	}

	// An exit code outside the frozen table is not a class-level failure at
	// all; it is a fact about the process. Its own text supersedes the class's.
	if !errInTable(exitCode) {
		summary, hint = errExitFloor(exitCode)
	}

	if summary == "" {
		if d := class.Description(); d != "" && d != "unknown" {
			summary = d
		} else {
			summary = "debark stopped with an error this app does not recognise"
		}
	}
	if hint == "" {
		hint = "Run the command shown below in a terminal and report what it prints; that output has detail this app was not given."
	}
	return summary, hint
}

// errExitFloor describes an exit code the frozen table does not cover.
func errExitFloor(exitCode int) (summary, hint string) {
	if t, ok := errExitTexts[exitCode]; ok {
		return t.summary, t.hint
	}
	switch {
	case exitCode < 0:
		// os/exec reports -1 for a process killed by a signal on Unix, and for
		// a process whose status could not be read.
		return "debark was killed before it finished",
			"Nothing was completed. On Linux this is usually the out-of-memory killer or a Stop from outside the app: check free memory and free disk, then run it again."
	case exitCode > 128 && exitCode <= 192:
		// The shell convention: 128 + signal number.
		return "debark was killed by signal " + strconv.Itoa(exitCode-128) + " before it finished",
			"Nothing was completed. Check free memory and free disk on this machine, then run it again; if it keeps happening at the same point, report the command below."
	case exitCode > 0xFFFF:
		// Printed straight from the int: a Windows NTSTATUS arrives here as a
		// positive value in that range, and converting to uint32 to format it
		// would be a narrowing conversion for no gain.
		return fmt.Sprintf("debark crashed: the process ended with Windows status 0x%08X", exitCode),
			"This is a fault in the program rather than something you did. Report the command below along with the debark version in settings."
	default:
		return "debark stopped with exit status " + strconv.Itoa(exitCode) + ", which is not one of the statuses it documents",
			"Either this is a newer debark than this app knows about, or the binary in settings is not debark at all. Check the path and the version in settings."
	}
}
