package snapshot

import (
	"bufio"
	"bytes"
	"strings"
)

// deb822Field is one Field: Value pair from a deb822/RFC822-style stanza
// (apt's .sources files, dpkg's status file, preferences files all share this
// grammar). Value has continuation lines joined by "\n"; a continuation line
// that is exactly "." represents a blank line inside the value, per the usual
// debian control-file convention.
type deb822Field struct {
	Name  string
	Value string
}

// deb822Stanza is one paragraph: an ordered list of fields. Order is
// preserved because a value's continuation lines must stay attached to the
// field that introduced them.
type deb822Stanza []deb822Field

// Get returns the value of the first field named name (case-insensitive), and
// whether it was present.
func (s deb822Stanza) Get(name string) (string, bool) {
	for _, f := range s {
		if strings.EqualFold(f.Name, name) {
			return f.Value, true
		}
	}
	return "", false
}

// parseDeb822 splits data into stanzas separated by blank lines. It is
// deliberately minimal: just enough of RFC 822/deb822 to recover fields and
// their (possibly multi-line) values from apt's own file formats. It does not
// validate required fields or field grammar -- that is apt's job, not ours;
// we only need to find the handful of fields debark cares about.
//
// Lines whose first non-whitespace character is '#' are comments and are
// skipped entirely (real .sources files use this, e.g. a commented-out
// snapshot-mirror URL between "Types:" and "URIs:"). A line beginning with
// whitespace continues the previous field's value; any other non-blank line
// starts a new field at "Name:Value" (colon required).
func parseDeb822(data []byte) []deb822Stanza {
	var stanzas []deb822Stanza
	var cur deb822Stanza

	flush := func() {
		if len(cur) > 0 {
			stanzas = append(stanzas, cur)
			cur = nil
		}
	}

	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()

		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(cur) > 0 {
			cont := strings.TrimPrefix(strings.TrimPrefix(line, "\t"), " ")
			if cont == "." {
				cont = ""
			}
			cur[len(cur)-1].Value += "\n" + cont
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			// Not a recognisable field line and not a continuation (e.g. a
			// stray line with no colon at column 0). Ignore it rather than
			// fail: we are a lenient reader of apt's own files.
			continue
		}
		cur = append(cur, deb822Field{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)})
	}
	flush()
	return stanzas
}
