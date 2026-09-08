package snapshot

import (
	"bufio"
	"bytes"
	"strings"
)

// signedByRef is one Signed-By/signed-by= reference found in a sources file:
// either a path to a keyring file, or (deb822 only) an inline armoured key
// embedded directly in the field's continuation lines.
type signedByRef struct {
	Path    string
	Armored string
}

// extractSignedBy finds every keyring reference in one captured sources
// file's content. It runs both the one-line and the deb822 extractors
// unconditionally rather than sniffing the file's format first: a one-line
// "deb ..." line can never satisfy the deb822 extractor's field-name lookup,
// and a deb822 stanza's field lines never start with "deb ", so running both
// is safe and sidesteps the fact that apt itself tells the two formats apart
// by directory convention (.list vs .sources), not by content -- an operator
// can and sometimes does put deb822 stanzas in a plain sources.list.
func extractSignedBy(data []byte) []signedByRef {
	refs := extractOneLineSignedBy(data)
	refs = append(refs, extractDeb822SignedBy(data)...)
	return refs
}

// extractOneLineSignedBy handles sources.list(5)'s one-line format:
//
//	deb [option1=value1 option2=value2] URI SUITE [COMPONENT...]
func extractOneLineSignedBy(data []byte) []signedByRef {
	var refs []signedByRef
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || (fields[0] != "deb" && fields[0] != "deb-src") {
			continue
		}
		start := strings.IndexByte(line, '[')
		if start < 0 {
			continue
		}
		rest := line[start+1:]
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			continue
		}
		for _, tok := range strings.Fields(rest[:end]) {
			key, val, ok := strings.Cut(tok, "=")
			if ok && strings.EqualFold(key, "signed-by") {
				if p := strings.TrimSpace(val); p != "" {
					refs = append(refs, signedByRef{Path: p})
				}
			}
		}
	}
	return refs
}

// extractDeb822SignedBy handles the deb822 field format's "Signed-By" field,
// either form: a bare path on the field's own line, or an inline armoured
// key block carried in the field's continuation lines (RFC 8878 blank lines
// inside a value are written as a lone "." per deb822 convention; parseDeb822
// already turns those back into real blank lines).
func extractDeb822SignedBy(data []byte) []signedByRef {
	var refs []signedByRef
	for _, stz := range parseDeb822(data) {
		val, ok := stz.Get("Signed-By")
		if !ok {
			continue
		}
		val = strings.Trim(val, "\n")
		trimmed := strings.TrimSpace(val)
		if strings.Contains(val, "\n") || strings.HasPrefix(trimmed, "-----BEGIN PGP") {
			if trimmed != "" {
				refs = append(refs, signedByRef{Armored: val})
			}
			continue
		}
		if trimmed != "" {
			refs = append(refs, signedByRef{Path: trimmed})
		}
	}
	return refs
}
