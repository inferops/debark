package doctor

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// maxDeb822Stanzas bounds how many stanzas parseDeb822 will accumulate, and
// it exists because the two documents this parser is pointed at have wildly
// different honest sizes and only one of them is trusted.
//
// A .deb control file has exactly one stanza. A target's
// /var/lib/dpkg/status has one per installed package: a full Debian desktop
// runs to a few thousand, and 100,000 is more than an apt-managed system can
// realistically reach. A hostile control member can be neither: 8 MiB of
// "P:1\n\n" (the most scanByteLimit allows, and about 30 KB once gzipped
// inside the .deb) parsed into 1,676,903 stanza maps costing 639,428,320
// bytes — a 610 MiB heap spike out of ~30 KB of media, ~20,000x
// amplification, for a control file from which doctor reads exactly one
// stanza.
//
// Refusing is the safe direction here, unlike in ar.go: a caller that loses
// the control stanza still gets every maintainer script (they are separate
// tar entries, parsed separately), so the network check — the one whose
// silence would be a false clean result — is untouched; and a caller that
// loses dpkg status degrades to checkDKMSHeaders' explicit "no snapshot dpkg
// status was available" warning rather than to silence.
const maxDeb822Stanzas = 100_000

// parseDeb822 parses a small subset of RFC 2822/deb822-style stanzas: the
// format shared by .deb control files and /var/lib/dpkg/status. Each stanza is
// a sequence of "Key: value" fields; a line beginning with whitespace
// continues the previous field's value (used by multi-line Description).
// Stanzas are separated by one or more blank lines. This is intentionally
// narrow — just enough to read the handful of fields doctor's checks need —
// not a full deb822/control-file writer or validator.
//
// The folded-field accumulation below is a strings.Builder rather than the
// obvious `cur[lastKey] += "\n" + cont`, and that is a bound, not a style
// preference. `+=` on a string rebuilds the whole value on every
// continuation line, so a field folded over n lines copies O(n²) bytes.
// Measured on the `+=` version, with a control file that is one field and
// nothing but continuation lines:
//
//	  65,538 bytes ->  0.08s,     517,533,024 bytes allocated
//	 131,073 bytes ->  0.24s,   2,038,446,656 bytes allocated
//	 262,146 bytes ->  0.93s,   7,943,716,928 bytes allocated
//	 524,289 bytes ->  2.73s,  31,207,348,240 bytes allocated
//	1,048,578 bytes ->  7.65s, 123,547,850,784 bytes allocated
//	2,097,153 bytes -> 31.21s, 491,480,352,480 bytes allocated
//
// — four times the work for twice the input, all the way up. scanByteLimit
// lets a .deb's control file reach 8 MiB, four more doublings, which
// extrapolates to ~8 minutes and ~7.5 TiB of allocation churn for ONE
// package; and that 8 MiB is ~11 KB of gzip inside the .deb, so the whole
// attack fits in a bundle nobody would look at twice. `debark doctor
// BUNDLE` is the command an operator runs FIRST on media that just arrived,
// on a machine chosen for being air-gapped rather than for being fast.
//
// The Builder makes the same input linear: the field is written once and
// handed to the map when it ends.
func parseDeb822(r io.Reader) ([]map[string]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024) // control/status stanzas can have long Description fields
	var (
		stanzas []map[string]string
		cur     map[string]string
		lastKey string
		// folded holds the value of the field lastKey names — its first line
		// plus every continuation line — until the field ends.
		folded  strings.Builder
		overrun bool
	)
	// commit stores the field that has just ended. Deliberately NOT called
	// for a line that is neither a field nor a continuation: the old code
	// left lastKey alone there, so a later continuation line still folded
	// into it, and that behaviour is preserved.
	commit := func() {
		if cur != nil && lastKey != "" {
			cur[lastKey] = folded.String()
		}
		folded.Reset()
	}
	flush := func() {
		commit()
		if cur != nil {
			stanzas = append(stanzas, cur)
		}
		cur = nil
		lastKey = ""
	}
	for sc.Scan() {
		if len(stanzas) >= maxDeb822Stanzas {
			overrun = true
			break
		}
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && cur != nil && lastKey != "" {
			cont := strings.TrimPrefix(line, " ")
			cont = strings.TrimPrefix(cont, "\t")
			if cont == "." {
				cont = "" // deb822's marker for a blank line inside a folded field
			}
			folded.WriteString("\n")
			folded.WriteString(cont)
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue // not a field line; ignore rather than fail, this is a heuristic reader
		}
		commit()
		key = strings.TrimSpace(key)
		val = strings.TrimPrefix(val, " ")
		if cur == nil {
			cur = make(map[string]string)
		}
		lastKey = key
		folded.WriteString(val)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if overrun {
		return nil, fmt.Errorf("doctor: deb822 document has more than %d stanzas", maxDeb822Stanzas)
	}
	flush()
	return stanzas, nil
}

// stanzaGet looks up a field case-insensitively, since debark only ever
// reads well-known field names but real-world control files occasionally
// vary casing.
func stanzaGet(stanza map[string]string, key string) string {
	if v, ok := stanza[key]; ok {
		return v
	}
	for k, v := range stanza {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
