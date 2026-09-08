package catalog

// Apt sources parsing — the one piece of duplicated logic this repository
// accepts, and the reason docs/dev/catalogue-sourcing.md names it explicitly.
//
// Nothing in debark exports a sources parser that yields suites and
// components: base.Definition.Sources is a raw deb822 *string*, and a
// snapshot's APT.Sources are raw captured *files*. Under either option in that
// document the catalogue has to read them itself, so it does — here, in one
// file, and nowhere else.
//
// # The rule that shapes every function below
//
// A source this parser cannot use is REPORTED, never dropped in silence. A
// silently ignored source is a silently incomplete catalogue, which is the one
// failure here an operator cannot see: the picker simply does not offer a
// package, and nothing on screen says why. So every skip — the deliberate ones
// as much as the accidental ones — comes back as a SourceProblem carrying the
// file, the line, and a sentence a person can act on.
//
// The second rule is that a wrong answer is worse than a missing one. A stanza
// that is malformed, contradictory or unrecognisable produces no Source at
// all; it never produces a guess. A catalogue short by one repository costs a
// row in a picker, and the operator can still add that .deb by URL. A
// catalogue that invented an archive URI would have this application fetching
// somewhere the target never named.
//
// # Two grammars, both real
//
// deb822 (*.sources) is what every base definition is written in and what
// modern Ubuntu and Debian ship. The older one-line grammar (sources.list,
// *.list) is still what almost every third-party vendor's install script
// writes. Both appear in a single real machine's /etc/apt, so both are
// handled, and ParseSourcesFile picks between them by file name exactly as apt
// does.
//
// # Signed-By is deliberately dropped
//
// The catalogue does not verify archive signatures, ships no keyrings and is
// not a trust boundary — debark verifies everything that is actually
// installed. The frozen Source type has no field for a keyring, and recording
// one here would imply a check this package does not perform.

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"
)

// SourceMaxDocumentBytes bounds one sources document.
//
// A real /etc/apt/sources.list.d entry is tens of lines. This limit is not
// about those: it is about what happens when something that is not a sources
// file at all reaches this parser — a Packages index, a log, a truncated
// archive member — and the answer must be a bounded complaint rather than a
// gigabyte of problem records.
const SourceMaxDocumentBytes = 4 << 20

// srcMaxProblems bounds how many problems one document may report. Past it,
// one SourceProblemTruncated record stands for the rest: a UI that has to say
// "3 sources were ignored" cannot say anything useful with sixty thousand.
const srcMaxProblems = 200

// srcMaxProblemText is how much of an offending line a problem quotes back.
const srcMaxProblemText = 160

// srcBOM is the UTF-8 byte-order mark, spelled in hex so that this file
// contains no literal one of its own. A captured sources file may carry it:
// a snapshot can come from anywhere, and something on the way may have been an
// editor on Windows.
const srcBOM = "\xef\xbb\xbf"

// SourceFormat is which of apt's two source grammars a document was read with.
type SourceFormat string

const (
	// SourceFormatDeb822 is the modern paragraph format used by *.sources
	// files: Types:/URIs:/Suites:/Components:, one stanza per paragraph. It is
	// what every base.Definition.Sources document is, always.
	SourceFormatDeb822 SourceFormat = "deb822"
	// SourceFormatOneLine is the legacy one-entry-per-line format used by
	// sources.list and *.list files: "deb [opts] URI suite components...".
	SourceFormatOneLine SourceFormat = "one-line"
)

// SourceProblemKind says why one source stanza produced no catalogue entries.
//
// The kinds are separated by what an operator would do about them, which is
// the only test worth applying to an error taxonomy. Deliberate reports which
// side of that line a kind falls on.
type SourceProblemKind string

const (
	// SourceProblemSourceOnly is a deb-src stanza with no deb type. Source
	// packages are not browsable software and the catalogue indexes none of
	// them. Deliberate; documented in docs/dev/catalogue-sourcing.md.
	SourceProblemSourceOnly SourceProblemKind = "source-only"
	// SourceProblemFlatRepo is a suite ending in "/" (or otherwise carrying a
	// path): a flat repository, with no dists/ hierarchy, so no components and
	// no DEP-11. The operator reaches those .debs by URL, on a different
	// screen. Deliberate.
	SourceProblemFlatRepo SourceProblemKind = "flat-repository"
	// SourceProblemDisabled is a deb822 stanza switched off with
	// "Enabled: no". apt ignores it, so the catalogue does too. Deliberate.
	SourceProblemDisabled SourceProblemKind = "disabled"
	// SourceProblemOtherArch is a stanza restricted to architectures that do
	// not include the target's — a one-line "[arch=...]" option or a deb822
	// "Architectures:" field. One catalogue is one architecture, so a stanza
	// that serves other ones contributes nothing to this one. Deliberate.
	SourceProblemOtherArch SourceProblemKind = "other-architecture"

	// SourceProblemNoURI is a stanza with no archive URI. Nothing can be
	// fetched from it and nothing about it can be guessed.
	SourceProblemNoURI SourceProblemKind = "no-uri"
	// SourceProblemNoSuite is a stanza with no suite.
	SourceProblemNoSuite SourceProblemKind = "no-suite"
	// SourceProblemNoComponent is a stanza naming a dists/ suite but no
	// component. There is no default: apt itself refuses such a line.
	SourceProblemNoComponent SourceProblemKind = "no-component"
	// SourceProblemUnknownType is a type that is neither deb nor deb-src.
	SourceProblemUnknownType SourceProblemKind = "unknown-type"
	// SourceProblemPlaceholder is a surviving ${...} template placeholder.
	//
	// base.Validate refuses a definition whose Sources still carries one, so
	// seeing this from a base means the core repository's own guarantee was
	// broken; ResolveBase turns it into a hard error rather than a note. From
	// a captured snapshot it means the target's file was templated and never
	// expanded, which is the operator's to fix.
	SourceProblemPlaceholder SourceProblemKind = "placeholder"
	// SourceProblemMalformed is a stanza this parser could not read
	// confidently: a line that is not a field, a field given twice, a
	// paragraph that carries none of the four fields a source needs.
	SourceProblemMalformed SourceProblemKind = "malformed"
	// SourceProblemUnreadable is a source file that could not be read at all —
	// missing from a snapshot's files/ tree, unreadable, or larger than
	// SourceMaxDocumentBytes.
	SourceProblemUnreadable SourceProblemKind = "unreadable"
	// SourceProblemTruncated stands for the problems past srcMaxProblems that
	// were not recorded. Its presence means the document is very probably not
	// a sources file at all.
	SourceProblemTruncated SourceProblemKind = "too-many-problems"
	// SourceProblemUnsupportedScheme is an archive URI whose scheme the
	// catalogue cannot fetch over: cdrom:, file:, copy:, ftp:, mirror+file:
	// and everything else apt supports that is not http or https.
	//
	// Deliberate, because it is a documented scope limit rather than a fault
	// in the operator's file: an install-media or local-mirror source is a
	// perfectly valid thing for a target to have, and this catalogue simply
	// does not read it. It is a *scope* limit and not a silent one, which is
	// the distinction docs/dev/catalogue-sourcing.md draws — a catalogue that
	// is short by an archive with nothing on screen to say so is the one
	// failure mode named there as unacceptable.
	//
	// See IndexRefs for why the filtering happens there as well as here.
	SourceProblemUnsupportedScheme SourceProblemKind = "unsupported-scheme"
)

// igIndexSchemes is the allow-list: the only URI schemes the catalogue will
// issue a request for.
//
// It exists because Target.IndexRefs turned every scheme a target's sources
// named into an http.NewRequestWithContext URL, and a snapshot is untrusted
// input by construction — a file produced on another machine and carried
// across the gap. Two different things followed from that, and the allow-list
// answers both:
//
//   - Local schemes were harmless but produced the wrong answer. Go's
//     transport has no handler for file:, cdrom:, copy: or mirror+file:, so
//     no local file was ever read; what the operator saw was a network-shaped
//     failure where the honest answer is "this source is not fetchable over
//     HTTP, so its packages are not in the catalogue".
//   - A plain http:// URI was a request to a host the operator did not
//     choose. Selecting a snapshot resolves the target and builds the
//     catalogue, which issued a GET to every archive URI the snapshot named,
//     from the builder, before the operator had done anything but open a
//     file, and with no signature check anywhere on the path.
//
// http stays on the list. Ubuntu still publishes plain-http archive URIs and
// a target that names one is ordinary rather than suspicious; refusing it
// would make the catalogue empty for a large, legitimate population. What the
// allow-list removes is the *set of protocols* an untrusted file can steer
// this process into, which is the part that was never anyone's choice.
var igIndexSchemes = map[string]bool{"http": true, "https": true}

// igFetchableURI reports whether uri is something the catalogue can actually
// issue a request for, and why not when it is not.
//
// The scheme test alone is not enough, and FuzzSecSources* proved it twice.
// "URIs:https:" passes any scheme check -- the scheme really is https -- and
// IndexRef.URL then concatenates it into a string with no host in it at all.
// A hand-written authority check fixed that and was itself beaten by
// "http://@", where the authority is a non-empty userinfo separator and the
// host is still empty.
//
// So once the scheme is known to be one the fetcher uses, the rest of the
// question is handed to net/url -- the package http.NewRequestWithContext
// parses with, so this cannot disagree with the request it stands in for. It
// is strictly better than the hand-written check was, and measurably: it
// rejects a space in the host, an ASCII control character anywhere, and a
// truncated percent-escape, and it accepts an IPv6 literal, a port, and
// userinfo, all of which real archive URIs carry.
//
// The scheme is still extracted by hand, and that split is the point rather
// than an inconsistency: url.Parse cannot name the scheme of
// "cdrom:[Ubuntu 24.04]/" at all, because the space makes it refuse the whole
// string, and "this source is install media" is the sentence that URI needs.
// Hand-parsing answers "which scheme", net/url answers "is this a usable
// http URL", and neither can do the other's job.
//
// The two failure modes are told apart because an operator does different
// things about them. A cdrom: source is a documented scope limit and a
// deliberate skip; "https:" with no host is a broken line in a file they
// should go and look at.
func igFetchableURI(uri string) (scheme string, malformed bool, ok bool) {
	scheme = igURIScheme(uri)
	if !igIndexSchemes[scheme] {
		return scheme, false, false
	}
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return scheme, true, false
	}
	return scheme, false, true
}

// igURIScheme returns the lower-cased scheme of an apt archive URI, or "" when
// it has none.
//
// Deliberately not net/url.Parse: apt URIs are not all valid URLs. "cdrom:[Ubuntu
// 24.04]/" has an unescaped space and brackets in what a parser would call the
// opaque part, and a parse failure would then be indistinguishable from an
// unsupported scheme — two different sentences for the operator. Everything
// before the first colon is exactly what apt's own method dispatch uses, and
// it is the only thing this decision needs.
func igURIScheme(uri string) string {
	i := strings.IndexByte(uri, ':')
	if i <= 0 {
		return ""
	}
	scheme := uri[:i]
	// A scheme is ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ) per RFC 3986.
	// Anything else is not a scheme at all, which is how a bare Windows path
	// or a "host:port/path" typo is told from "cdrom:".
	for j := 0; j < len(scheme); j++ {
		c := scheme[j]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9', c == '+', c == '-', c == '.':
			if j == 0 {
				return ""
			}
		default:
			return ""
		}
	}
	return strings.ToLower(scheme)
}

// SourceProblem is one source the catalogue will not be built from, and why.
//
// It exists so a UI can say "3 of this target's 9 apt sources were not used"
// and show which, rather than presenting a short catalogue as a complete one.
// Every field answers a question an operator asks in order: which file, which
// line, what did it say, and what does that mean.
type SourceProblem struct {
	// Kind is the machine-readable reason. Branch on this, never on Reason.
	Kind SourceProblemKind `json:"kind"`
	// File is where the source came from — a snapshot's captured path
	// ("/etc/apt/sources.list.d/ubuntu.sources"), or the path a base
	// definition's document is written to in a synthesized snapshot. Empty
	// only when a caller parsed an anonymous document.
	File string `json:"file,omitempty"`
	// Format is the grammar the document was read with.
	Format SourceFormat `json:"format,omitempty"`
	// Line is the 1-based line the stanza or entry starts at, 0 when unknown.
	Line int `json:"line,omitempty"`
	// Stanza is the 0-based index of the entry within its file, which is what
	// apt's own diagnostics count and what an operator editing the file will
	// look for when the line numbers have since moved.
	Stanza int `json:"stanza"`
	// Text is the offending text, clipped. It is evidence for a details
	// drawer, never the message.
	Text string `json:"text,omitempty"`
	// Reason is one complete sentence in plain language.
	Reason string `json:"reason"`
}

// Deliberate reports whether this problem is a documented scope limit rather
// than something that went wrong.
//
// The distinction is what lets a UI show "2 source-package repositories were
// skipped, as always" quietly and "1 source could not be read" loudly. A
// deliberate skip is a decision this project has already made and written
// down; anything else is a file the operator may want to look at.
func (p SourceProblem) Deliberate() bool {
	switch p.Kind {
	case SourceProblemSourceOnly, SourceProblemFlatRepo, SourceProblemDisabled, SourceProblemOtherArch,
		SourceProblemUnsupportedScheme:
		return true
	default:
		return false
	}
}

// String renders the problem for a log or a details drawer: where, then why.
func (p SourceProblem) String() string {
	var b strings.Builder
	if p.File != "" {
		b.WriteString(p.File)
		if p.Line > 0 {
			fmt.Fprintf(&b, ":%d", p.Line)
		}
		b.WriteString(": ")
	}
	b.WriteString(string(p.Kind))
	b.WriteString(": ")
	b.WriteString(p.Reason)
	if p.Text != "" {
		b.WriteString(" (")
		b.WriteString(p.Text)
		b.WriteString(")")
	}
	return b.String()
}

// SourceProblemsSummary is the one line a UI shows above the details: how many
// sources were not used, and in what proportions. Empty when there were none.
//
// It counts sources rather than listing them because the sentence has to fit
// on a screen that is mostly a package list, and because the number is the
// part that tells an operator whether to go looking.
func SourceProblemsSummary(problems []SourceProblem) string {
	if len(problems) == 0 {
		return ""
	}
	counts := make(map[SourceProblemKind]int, len(problems))
	for _, p := range problems {
		counts[p.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		kind := SourceProblemKind(k)
		parts = append(parts, srcKindPhrase(kind, counts[kind]))
	}
	verb := "apt sources were"
	if len(problems) == 1 {
		verb = "apt source was"
	}
	return fmt.Sprintf("%d %s not used: %s", len(problems), verb, strings.Join(parts, ", "))
}

func srcKindPhrase(kind SourceProblemKind, n int) string {
	var what string
	switch kind {
	case SourceProblemSourceOnly:
		what = "source-package (deb-src) %s"
	case SourceProblemFlatRepo:
		what = "flat %s with no dists/ hierarchy"
	case SourceProblemDisabled:
		what = "switched-off %s"
	case SourceProblemOtherArch:
		what = "%s for another architecture"
	case SourceProblemNoURI:
		what = "%s with no archive URI"
	case SourceProblemNoSuite:
		what = "%s with no suite"
	case SourceProblemNoComponent:
		what = "%s with no component"
	case SourceProblemUnknownType:
		what = "%s of an unrecognised type"
	case SourceProblemPlaceholder:
		what = "%s with an unexpanded ${...} placeholder"
	case SourceProblemMalformed:
		what = "unreadable %s"
	case SourceProblemUnreadable:
		what = "source file %s that could not be read"
	case SourceProblemTruncated:
		what = "further %s not listed"
	case SourceProblemUnsupportedScheme:
		what = "%s the catalogue cannot fetch over"
	default:
		what = "%s"
	}
	return fmt.Sprintf("%d "+what, n, srcPlural(n, "entry", "entries"))
}

func srcPlural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// SourceEntry is one parsed source stanza: the Source the catalogue can use,
// plus the two things the frozen Source type has no field for.
//
// Provenance (File, Line, Stanza) is here because a catalogue built from nine
// sources across three files is otherwise unattributable: when one archive is
// unreachable the operator needs to be told which file named it. Arches is
// here because "one catalogue is one architecture" is a property of Target,
// not of Source, so the architecture restriction an entry carries has to be
// applied by the caller that knows the target's arch — SourcesForArch.
type SourceEntry struct {
	// Source is the transcribed stanza, ready for Target.Sources.
	Source Source `json:"source"`
	// File, Line and Stanza say where this came from. Line is 1-based and
	// Stanza 0-based within the file.
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Stanza int    `json:"stanza"`
	// Format is the grammar the entry was read with.
	Format SourceFormat `json:"format,omitempty"`
	// Arches is the entry's architecture restriction — the one-line
	// "[arch=...]" option or the deb822 "Architectures:" field — and is empty
	// when the entry named none, which is the ordinary case and means "every
	// architecture the archive publishes".
	Arches []string `json:"arches,omitempty"`
}

// ParseSourcesFile parses one source file, choosing the grammar by file name
// exactly as apt does: *.sources is deb822 and everything else — sources.list,
// *.list — is the one-line format.
//
// name is used for that choice and is recorded on every entry and problem, so
// pass the path the target itself knows the file by.
func ParseSourcesFile(name string, data []byte) ([]SourceEntry, []SourceProblem) {
	if srcIsDeb822Name(name) {
		return ParseSourcesDeb822(name, data)
	}
	return ParseSourcesOneLine(name, data)
}

// srcIsDeb822Name applies apt's own rule for which grammar a file is written
// in. It is the file name and nothing else: apt does not sniff the content,
// and a parser that guessed differently from apt would build a catalogue from
// sources the target does not actually have.
func srcIsDeb822Name(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".sources")
}

// ParseSourcesDeb822 parses a deb822 .sources document — the format every
// base.Definition.Sources is written in.
//
// Paragraphs separated by blank lines are stanzas; a line beginning with "#"
// is a comment anywhere, including between the fields of a stanza (Debian's
// own generated debian.sources does exactly that); a line beginning with
// whitespace continues the field above it, which is how an inline armoured
// Signed-By key is spelled. Field names are matched case-insensitively and
// values are whitespace-separated lists.
func ParseSourcesDeb822(name string, data []byte) ([]SourceEntry, []SourceProblem) {
	rec := &srcRecorder{file: name, format: SourceFormatDeb822}
	if srcTooBig(rec, data) {
		return nil, rec.problems
	}

	var entries []SourceEntry
	var st srcStanza
	curKey := ""
	stanzaIndex := 0

	flush := func() {
		if st.empty() {
			st = srcStanza{}
			curKey = ""
			return
		}
		if e, ok := srcStanzaEntry(rec, st, stanzaIndex); ok {
			entries = append(entries, e)
		}
		stanzaIndex++
		st = srcStanza{}
		curKey = ""
	}

	for i, line := range srcLines(data) {
		lineNo := i + 1
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case line[0] == '#':
			// A whole-line comment, which deb822 allows inside a stanza as
			// well as between them. It does not end the paragraph.
		case line[0] == ' ' || line[0] == '\t':
			// A continuation of the field above. Appended only for the fields
			// this parser reads: the one field realistically folded in the
			// wild is an inline armoured Signed-By key, which is both large
			// and none of the catalogue's business.
			if curKey != "" && srcTrackedField(curKey) {
				st.appendTo(curKey, strings.TrimSpace(line))
			}
		default:
			key, value, ok := strings.Cut(line, ":")
			key = strings.ToLower(strings.TrimSpace(key))
			if !ok || key == "" {
				st.breakWith(lineNo, "this line is neither a field nor the continuation of one")
				curKey = ""
				continue
			}
			st.set(key, strings.TrimSpace(value), lineNo)
			curKey = key
		}
	}
	flush()
	return entries, rec.problems
}

// ParseSourcesOneLine parses a legacy sources.list document.
//
// One entry per line: a type, optional bracketed options, an archive URI, a
// suite, and then the components. Blank lines and lines beginning with "#" are
// comments, and a "#" preceded by whitespace starts a trailing comment.
//
// The bracketed options are read but almost entirely discarded. signed-by is
// dropped because the catalogue is not a trust boundary; trusted, by-hash,
// pdiffs, lang and the rest describe how apt fetches, which is apt's business.
// Only arch survives, on SourceEntry.Arches, because it decides whether the
// entry describes this catalogue's architecture at all.
func ParseSourcesOneLine(name string, data []byte) ([]SourceEntry, []SourceProblem) {
	rec := &srcRecorder{file: name, format: SourceFormatOneLine}
	if srcTooBig(rec, data) {
		return nil, rec.problems
	}

	var entries []SourceEntry
	index := 0
	for i, raw := range srcLines(data) {
		line := srcStripLineComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		e, ok := srcOneLineEntry(rec, line, i+1, index)
		index++
		if ok {
			entries = append(entries, e)
		}
	}
	return entries, rec.problems
}

// SourcesForArch reduces parsed entries to the Sources for one architecture,
// reporting every entry it leaves out.
//
// One catalogue is one architecture (Target.Arch), so an entry restricted to
// other architectures contributes nothing to this one and is dropped here
// rather than in the parser — the parser does not know, and must not guess,
// which target it is reading for. An entry naming no architectures serves
// every architecture the archive publishes and is always kept.
//
// arch is compared against the dpkg names apt itself uses. The value "any",
// which apt accepts in an arch= list, keeps the entry.
func SourcesForArch(arch string, entries []SourceEntry) ([]Source, []SourceProblem) {
	var out []Source
	var problems []SourceProblem
	for _, e := range entries {
		if !srcServesArch(arch, e.Arches) {
			problems = append(problems, SourceProblem{
				Kind:   SourceProblemOtherArch,
				File:   srcSanitize(e.File),
				Format: e.Format,
				Line:   e.Line,
				Stanza: e.Stanza,
				Text:   srcClip(strings.Join(e.Source.URIs, " "), srcMaxProblemText),
				// srcClip, not raw interpolation: the architecture names come
				// out of the file being parsed, so they can carry invalid
				// UTF-8 and can be arbitrarily long. Clipping bounds both.
				Reason: fmt.Sprintf("this source serves %s only, and this catalogue is for %s",
					srcClip(strings.Join(e.Arches, ", "), srcMaxProblemText), srcSanitize(arch)),
			})
			continue
		}
		out = append(out, e.Source)
	}
	return out, problems
}

func srcServesArch(arch string, arches []string) bool {
	if len(arches) == 0 {
		return true
	}
	for _, a := range arches {
		if a == arch || a == "any" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// deb822 stanza accumulation
// ---------------------------------------------------------------------------

type srcField struct {
	line  int
	value string
}

// srcStanza is one deb822 paragraph as it is being read.
//
// broken is sticky and is checked before anything else: a stanza that said
// something this parser could not read is refused whole, never salvaged. Half
// a stanza is a guess about what the operator meant, and a guessed archive URI
// is exactly the wrong answer this file exists to avoid.
type srcStanza struct {
	fields     map[string]srcField
	first      int
	head       string
	broken     string
	brokenLine int
}

func (s *srcStanza) empty() bool { return len(s.fields) == 0 && s.broken == "" }

func (s *srcStanza) set(key, value string, line int) {
	if s.fields == nil {
		s.fields = make(map[string]srcField, 8)
	}
	if _, dup := s.fields[key]; dup {
		s.breakWith(line, fmt.Sprintf("the field %q appears more than once in one stanza, so which value applies is undefined", key))
		return
	}
	s.fields[key] = srcField{line: line, value: value}
	if s.first == 0 {
		s.first = line
		s.head = strings.TrimSpace(key + ": " + value)
	}
}

func (s *srcStanza) appendTo(key, more string) {
	f, ok := s.fields[key]
	if !ok || more == "" {
		return
	}
	f.value = strings.TrimSpace(f.value + " " + more)
	s.fields[key] = f
}

func (s *srcStanza) breakWith(line int, reason string) {
	if s.broken != "" {
		return
	}
	s.broken = reason
	s.brokenLine = line
	if s.first == 0 {
		s.first = line
	}
}

func (s srcStanza) get(key string) (srcField, bool) {
	f, ok := s.fields[key]
	return f, ok
}

func (s srcStanza) values(key string) []string {
	f, ok := s.fields[key]
	if !ok {
		return nil
	}
	return strings.Fields(f.value)
}

// srcTrackedField reports whether a continuation line is worth appending. Only
// the fields this parser reads are accumulated; everything else — Signed-By
// above all — is discarded as it arrives.
func srcTrackedField(key string) bool {
	switch key {
	case "types", "uris", "suites", "components", "architectures", "enabled":
		return true
	default:
		return false
	}
}

// srcStanzaEntry turns one accumulated stanza into an entry, or into problems.
func srcStanzaEntry(rec *srcRecorder, st srcStanza, index int) (SourceEntry, bool) {
	line := st.first
	if st.broken != "" {
		at := st.brokenLine
		if at == 0 {
			at = line
		}
		rec.note(SourceProblemMalformed, at, index, st.head, st.broken)
		return SourceEntry{}, false
	}

	_, hasTypes := st.get("types")
	_, hasURIs := st.get("uris")
	_, hasSuites := st.get("suites")
	_, hasComponents := st.get("components")
	if !hasTypes && !hasURIs && !hasSuites && !hasComponents {
		rec.note(SourceProblemMalformed, line, index, st.head,
			"this paragraph carries none of Types, URIs, Suites or Components, so it does not describe an apt source")
		return SourceEntry{}, false
	}

	if f, ok := st.get("enabled"); ok && srcIsFalse(f.value) {
		rec.note(SourceProblemDisabled, line, index, st.head,
			`the stanza is switched off with "Enabled: no", so apt ignores it too`)
		return SourceEntry{}, false
	}

	types, ok := srcNormalizeTypes(rec, st.values("types"), line, index, st.head)
	if !ok {
		return SourceEntry{}, false
	}

	uris := srcFilterPlaceholders(rec, st.values("uris"), "URI", line, index)
	if len(uris) == 0 {
		rec.note(SourceProblemNoURI, line, index, st.head,
			"this stanza names no archive URI, so there is nothing to fetch from")
		return SourceEntry{}, false
	}

	rawSuites := srcFilterPlaceholders(rec, st.values("suites"), "suite", line, index)
	if len(rawSuites) == 0 {
		rec.note(SourceProblemNoSuite, line, index, st.head,
			"this stanza names no suite, so no dists/ path can be formed from it")
		return SourceEntry{}, false
	}
	suites := srcUsableSuites(rec, rawSuites, uris[0], line, index)
	if len(suites) == 0 {
		return SourceEntry{}, false
	}

	comps := srcFilterPlaceholders(rec, st.values("components"), "component", line, index)
	if len(comps) == 0 {
		rec.note(SourceProblemNoComponent, line, index, st.head,
			"this stanza names a suite but no component, and apt has no default for one")
		return SourceEntry{}, false
	}

	return SourceEntry{
		Source: Source{Types: types, URIs: uris, Suites: suites, Components: comps},
		File:   rec.file,
		Line:   line,
		Stanza: index,
		Format: SourceFormatDeb822,
		Arches: srcFilterPlaceholders(rec, st.values("architectures"), "architecture", line, index),
	}, true
}

// ---------------------------------------------------------------------------
// one-line entries
// ---------------------------------------------------------------------------

// srcOneLineEntry parses one "deb [opts] URI suite components..." line.
func srcOneLineEntry(rec *srcRecorder, line string, lineNo, index int) (SourceEntry, bool) {
	head := srcClip(line, srcMaxProblemText)
	rest := strings.TrimSpace(line)

	typeTok, rest, _ := srcCutField(rest)
	types, ok := srcNormalizeTypes(rec, []string{typeTok}, lineNo, index, head)
	if !ok {
		return SourceEntry{}, false
	}

	var arches []string
	if strings.HasPrefix(rest, "[") {
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			rec.note(SourceProblemMalformed, lineNo, index, head,
				`the [options] on this line are never closed with a "]"`)
			return SourceEntry{}, false
		}
		arches = srcOneLineArches(rest[1:end])
		rest = strings.TrimLeft(rest[end+1:], " \t")
	}

	uri, rest, _ := srcCutField(rest)
	// TrimSpace as well as an emptiness test, because srcCutField splits on
	// space and tab -- which is what apt's own one-line parser does -- while
	// Go's notion of whitespace is wider. "deb  suite comp" therefore left a
	// vertical tab standing as the archive URI, and it travelled all the way
	// to IndexRefs before being dropped there in silence, which is the one
	// failure this file's header calls unacceptable. Found by
	// FuzzSecSourcesOneLine.
	if strings.TrimSpace(uri) == "" {
		rec.note(SourceProblemNoURI, lineNo, index, head,
			"this line names no archive URI, so there is nothing to fetch from")
		return SourceEntry{}, false
	}
	uris := srcFilterPlaceholders(rec, []string{uri}, "URI", lineNo, index)
	if len(uris) == 0 {
		return SourceEntry{}, false
	}

	suite, rest, _ := srcCutField(rest)
	// Same reasoning as the URI above: a field made only of whitespace this
	// splitter does not split on is not a suite.
	if strings.TrimSpace(suite) == "" {
		rec.note(SourceProblemNoSuite, lineNo, index, head,
			"this line names an archive but no suite, so no dists/ path can be formed from it")
		return SourceEntry{}, false
	}
	rawSuites := srcFilterPlaceholders(rec, []string{suite}, "suite", lineNo, index)
	if len(rawSuites) == 0 {
		return SourceEntry{}, false
	}
	suites := srcUsableSuites(rec, rawSuites, uris[0], lineNo, index)
	if len(suites) == 0 {
		return SourceEntry{}, false
	}

	comps := srcFilterPlaceholders(rec, strings.Fields(rest), "component", lineNo, index)
	if len(comps) == 0 {
		rec.note(SourceProblemNoComponent, lineNo, index, head,
			"this line names a suite but no component, and apt has no default for one")
		return SourceEntry{}, false
	}

	return SourceEntry{
		Source: Source{Types: types, URIs: uris, Suites: suites, Components: comps},
		File:   rec.file,
		Line:   lineNo,
		Stanza: index,
		Format: SourceFormatOneLine,
		Arches: arches,
	}, true
}

// srcOneLineArches pulls the arch= option out of a one-line entry's brackets.
//
// Every other option is discarded on purpose. apt's option syntax is
// "key=value" separated by whitespace, with values that may themselves be
// comma-separated lists; nothing here needs to be exact about the ones it
// throws away.
func srcOneLineArches(opts string) []string {
	for _, tok := range strings.Fields(opts) {
		key, value, ok := strings.Cut(tok, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "arch") {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		var out []string
		for _, a := range strings.Split(value, ",") {
			if a = strings.TrimSpace(a); a != "" {
				out = append(out, a)
			}
		}
		return out
	}
	return nil
}

// srcCutField takes the first whitespace-separated token off s.
func srcCutField(s string) (tok, rest string, ok bool) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", "", false
	}
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, "", true
	}
	return s[:i], strings.TrimLeft(s[i:], " \t"), true
}

// srcStripLineComment removes a comment from a one-line entry.
//
// A "#" as the first non-blank character comments the whole line, which is how
// every generated sources.list marks its header. A "#" preceded by whitespace
// starts a trailing comment. A "#" with neither — inside a URI or a keyring
// path — is left alone, because truncating there would silently shorten an
// archive URI, and a shortened URI is the wrong-answer failure this file
// exists to avoid.
func srcStripLineComment(line string) string {
	if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
		return ""
	}
	for i := 1; i < len(line); i++ {
		if line[i] == '#' && (line[i-1] == ' ' || line[i-1] == '\t') {
			return line[:i]
		}
	}
	return line
}

// ---------------------------------------------------------------------------
// shared field handling
// ---------------------------------------------------------------------------

// srcNormalizeTypes reduces a Types field to the known types, reporting the
// rest. An absent Types field means "deb", which is deb822's own default and
// what Target.IndexRefs already assumes.
func srcNormalizeTypes(rec *srcRecorder, raw []string, line, index int, head string) ([]string, bool) {
	if len(raw) == 0 {
		return []string{"deb"}, true
	}
	var kept []string
	hasDeb, hasSrc := false, false
	for _, t := range raw {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "deb":
			hasDeb = true
			kept = append(kept, "deb")
		case "deb-src":
			hasSrc = true
			kept = append(kept, "deb-src")
		case "":
		default:
			rec.note(SourceProblemUnknownType, line, index, head,
				fmt.Sprintf("%q is not an apt source type; only deb and deb-src exist", t))
		}
	}
	if hasDeb {
		return kept, true
	}
	if hasSrc {
		rec.note(SourceProblemSourceOnly, line, index, head,
			"this is a deb-src source: it carries source packages, which the catalogue does not index")
	}
	return nil, false
}

// srcUsableSuites drops the suites that have no dists/ hierarchy, reporting
// each one.
//
// The test matches Target.IndexRefs exactly — a suite containing "/" at all —
// because a suite this function kept and IndexRefs then skipped would be a
// source counted as used while silently contributing nothing, which is
// precisely the failure this file is written to prevent.
func srcUsableSuites(rec *srcRecorder, suites []string, uri string, line, index int) []string {
	var out []string
	for _, s := range suites {
		if strings.Contains(s, "/") {
			rec.note(SourceProblemFlatRepo, line, index, srcClip(uri+" "+s, srcMaxProblemText),
				fmt.Sprintf("suite %q is a flat repository: it has no dists/ hierarchy, so it publishes no components and no application metadata; reach those .debs by URL instead", s))
			continue
		}
		out = append(out, s)
	}
	return out
}

// srcFilterPlaceholders drops any value still carrying a ${...} template
// placeholder and reports it.
//
// base.Definition.Expand substitutes ${codename}, ${version_id} and ${arch}
// before a definition is ever validated, and base.Validate refuses a document
// that still contains "${" — so a survivor here means a guarantee upstream did
// not hold. Fetching from a literal "http://archive/${codename}" would be a
// confusing network error much later; refusing the value now is loud and
// local.
func srcFilterPlaceholders(rec *srcRecorder, values []string, what string, line, index int) []string {
	var out []string
	for _, v := range values {
		if strings.Contains(v, "${") {
			rec.note(SourceProblemPlaceholder, line, index, srcClip(v, srcMaxProblemText),
				fmt.Sprintf("this %s still contains an unexpanded ${...} placeholder", what))
			continue
		}
		out = append(out, v)
	}
	return out
}

func srcIsFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "no", "false", "0", "off":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// plumbing
// ---------------------------------------------------------------------------

// srcRecorder collects problems, bounded.
type srcRecorder struct {
	file     string
	format   SourceFormat
	problems []SourceProblem
	capped   bool
}

func (r *srcRecorder) note(kind SourceProblemKind, line, stanza int, text, reason string) {
	if r.capped {
		return
	}
	if len(r.problems) >= srcMaxProblems {
		r.capped = true
		r.problems = append(r.problems, SourceProblem{
			Kind:   SourceProblemTruncated,
			File:   srcSanitize(r.file),
			Format: r.format,
			Reason: fmt.Sprintf("more than %d entries in this file could not be used, so it is very probably not an apt sources file; only the first %d are listed", srcMaxProblems, srcMaxProblems),
		})
		return
	}
	r.problems = append(r.problems, SourceProblem{
		Kind:   kind,
		File:   srcSanitize(r.file),
		Format: r.format,
		Line:   line,
		Stanza: stanza,
		Text:   srcClip(text, srcMaxProblemText),
		// Reason and File are sanitised for the same reason Text is: both are
		// rendered, and both can carry bytes from the file being parsed --
		// Reason through interpolation, File through a snapshot's captured
		// path. Only Text was, which is the asymmetry FuzzSecSources* found.
		Reason: srcSanitize(reason),
	})
}

func srcTooBig(rec *srcRecorder, data []byte) bool {
	if len(data) <= SourceMaxDocumentBytes {
		return false
	}
	rec.note(SourceProblemUnreadable, 0, 0, "",
		fmt.Sprintf("this file is %d bytes, past the %d a sources file may be; it was not read", len(data), SourceMaxDocumentBytes))
	return true
}

// srcLines splits a document into lines, absorbing the two encodings a
// captured file may arrive in. A snapshot may have been taken anywhere and
// copied through anything, so CRLF and a UTF-8 byte-order mark are both
// ordinary rather than exceptional. A lone CR is treated as a line break too:
// that is corruption either way, and splitting makes it a loud malformed-line
// complaint instead of one very long unreadable line.
func srcLines(data []byte) []string {
	s := strings.TrimPrefix(string(data), srcBOM)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}

// srcClip bounds a quoted fragment, and flattens the control characters that
// would otherwise reach a terminal or a details drawer intact.
// srcSanitize makes one string from an untrusted file safe to put in a
// rendered sentence: control characters become spaces and invalid UTF-8
// becomes U+FFFD.
//
// The second half is the point and it is easy to miss, because it is
// strings.Map's own behaviour rather than anything written here: Map ranges
// over the string, and ranging yields utf8.RuneError for an invalid byte, so
// the invalid byte is replaced by the mapping. Nothing else in this function
// looks at encoding at all.
//
// It exists because a fuzz target found the asymmetry it fixes. SourceProblem
// carries two strings a UI renders — Text and Reason — and only Text went
// through this. An "Architectures: <invalid bytes>" field therefore reached
// SourceProblem.Reason verbatim and crossed the Wails bridge as JSON, where
// encoding/json substitutes U+FFFD silently rather than failing, so the
// operator saw a replacement character in a sentence and nothing said why.
// The same property is already enforced for the catalogue's own strings by
// cacheClip; this is the sources parser's half of it.
func srcSanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F {
			return ' '
		}
		return r
	}, s)
}

func srcClip(s string, limit int) string {
	s = srcSanitize(s)
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	// Cut on a rune boundary: the fragment is displayed, and half a rune
	// renders as a replacement character in the one place an operator is
	// trying to read what their file actually said.
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return strings.TrimSpace(s[:limit]) + "…"
}
