package apt

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SignedByRecord captures one Signed-By relationship found in a captured
// source file, and what the private-root builder did with it: rewritten to
// the copied keyring (preferred), left untouched because it was an inline
// armoured key, or stripped because no captured keyring matched (fallback —
// the source still verifies, against the private root's combined
// trusted.gpg.d, just without that source's own pinning). The lock uses this
// to state which key authenticated which suite.
type SignedByRecord struct {
	// SourceFile is the snapshot ArchivePath of the source file this
	// Signed-By came from.
	SourceFile string
	// Stanza is the 0-based index of the entry within SourceFile (a deb822
	// file can hold several; a one-line file has one per line).
	Stanza int
	// OriginalPath is the Signed-By value as captured (an absolute path on
	// the target); empty when Inline is true.
	OriginalPath string
	// Inline is true when Signed-By carried an armoured key block rather
	// than a path. Nothing needs rewriting: the material is self-contained.
	Inline bool
	// RewrittenTo is the new absolute path under the private root's
	// trusted.gpg.d, set when the rewrite succeeded.
	RewrittenTo string
	// Stripped is true when OriginalPath had no matching captured keyring,
	// so the field was removed instead of rewritten.
	Stripped bool
	// Fingerprints are the OpenPGP fingerprints (uppercase hex, sorted) the
	// snapshot recorded for the referenced keyring, best-effort.
	Fingerprints []string
}

// isDeb822SourceFile reports whether a source file's name should be parsed as
// deb822 (*.sources) rather than the legacy one-line list format
// (sources.list, *.list) — the same rule apt itself applies.
func isDeb822SourceFile(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".sources")
}

var signedByFieldRE = regexp.MustCompile(`(?i)^Signed-By:\s*(.*)$`)

// rewriteSignedBy rewrites (or strips) the Signed-By relationships in one
// captured source file's bytes, choosing the deb822 or one-line grammar by
// file name. keyringDest maps a keyring's captured absolute target path to
// where it now lives inside the private root's trusted.gpg.d.
func rewriteSignedBy(archivePath string, data []byte, keyringDest map[string]string) ([]byte, []SignedByRecord) {
	if isDeb822SourceFile(archivePath) {
		return rewriteSignedByDeb822(archivePath, data, keyringDest)
	}
	return rewriteSignedByOneLine(archivePath, data, keyringDest)
}

func rewriteSignedByDeb822(file string, data []byte, keyringDest map[string]string) ([]byte, []SignedByRecord) {
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	var records []SignedByRecord
	stanza := 0
	i := 0
	for i < len(lines) {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			stanza++
			out = append(out, line)
			i++
			continue
		}
		m := signedByFieldRE.FindStringSubmatch(line)
		if m == nil {
			out = append(out, line)
			i++
			continue
		}
		value := strings.TrimSpace(m[1])
		j := i + 1
		var cont []string
		for j < len(lines) && len(lines[j]) > 0 && (lines[j][0] == ' ' || lines[j][0] == '\t') {
			cont = append(cont, lines[j])
			j++
		}
		if len(cont) == 0 && strings.HasPrefix(value, "/") {
			// A deb822 Signed-By is a LIST, not a single path (see
			// splitSignedByValue): several keyrings, separated by a comma or
			// by whitespace. rewriteSignedByList rewrites each element on its
			// own and reports the field stripped only when not one of them
			// could be resolved.
			rewritten, recs := rewriteSignedByList(file, stanza, value, keyringDest, deb822SignedBySeps)
			if rewritten != "" {
				out = append(out, "Signed-By: "+rewritten)
			}
			// (nothing is emitted when every entry was stripped: the field is
			// dropped, exactly as it was before)
			records = append(records, recs...)
		} else {
			// Inline armoured key (or an empty/unrecognised value): the
			// material is self-contained, or there is nothing sensible to
			// rewrite. Leave it exactly as captured.
			out = append(out, line)
			out = append(out, cont...)
			records = append(records, SignedByRecord{SourceFile: file, Stanza: stanza, Inline: true})
		}
		i = j
	}
	return []byte(strings.Join(out, "\n")), records
}

// deb822SignedBySeps and oneLineSignedBySeps are the separators each source
// grammar allows between the individual keyring entries of one Signed-By
// value. A one-line entry's options live inside a single "[...]" group whose
// options are themselves whitespace-separated, so only ',' can separate
// entries there; a deb822 field body may use either.
func deb822SignedBySeps(r rune) bool  { return r == ',' || r == ' ' || r == '\t' }
func oneLineSignedBySeps(r rune) bool { return r == ',' }

// splitSignedByValue splits one Signed-By value into its individual entries.
//
// A comma-separated list is documented apt syntax and appears on entirely
// ordinary targets ("signed-by=/a.gpg,/b.gpg" means "this repository must be
// signed by one of these keys"), so nothing attacker-controlled is needed to
// reach it. Reading the whole value as a single path -- which is what a bare
// "(\S+)" capture did -- makes every multi-keyring pin fail to resolve and so
// be stripped, quietly widening that repository from "verifiable by its own
// two keys" to "verifiable by anything in the private root's combined
// trusted.gpg.d".
//
// The double quotes apt accepts around an option value are removed, both from
// around the whole value and from around an individual entry.
func splitSignedByValue(value string, sep func(rune) bool) []string {
	var out []string
	for _, f := range strings.FieldsFunc(unquoteSourceValue(value), sep) {
		if f = unquoteSourceValue(strings.TrimSpace(f)); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func unquoteSourceValue(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// rewriteSignedByList rewrites one Signed-By value entry by entry. It returns
// the replacement value ("" when the whole field must be dropped) and one
// SignedByRecord per entry.
//
// Stripped is set only when NOT ONE entry survived, because that is the only
// case where the source genuinely stops being pinned to its own key(s) --
// which is exactly what root.go's private-root.signed-by-stripped warning
// tells an operator has happened. An entry that could not be traced to a
// captured keyring while a sibling could is still recorded (OriginalPath set,
// RewrittenTo empty, Stripped false) but raises no such warning: the pin
// survives, one key smaller.
//
// The whole value is tried against keyringDest before any splitting, so a
// captured keyring path that legitimately contains a separator character
// still resolves exactly the way it did before.
func rewriteSignedByList(file string, stanza int, value string, keyringDest map[string]string, sep func(rune) bool) (string, []SignedByRecord) {
	if dest, ok := keyringDest[value]; ok {
		return dest, []SignedByRecord{{SourceFile: file, Stanza: stanza, OriginalPath: value, RewrittenTo: dest}}
	}
	entries := splitSignedByValue(value, sep)
	if len(entries) == 0 {
		return "", []SignedByRecord{{SourceFile: file, Stanza: stanza, OriginalPath: value, Stripped: true}}
	}
	var kept []string
	records := make([]SignedByRecord, 0, len(entries))
	for _, e := range entries {
		rec := SignedByRecord{SourceFile: file, Stanza: stanza, OriginalPath: e}
		switch {
		case !strings.HasPrefix(e, "/"):
			// Not a path: a bare key fingerprint, apt's other documented
			// Signed-By form. It needs no rewriting to keep its meaning
			// inside the private root, so it is carried through verbatim
			// rather than dropped. Recorded as Inline for the same reason
			// the deb822 branch uses that flag -- there was nothing here to
			// rewrite -- rather than as a path that failed to resolve.
			rec.OriginalPath = ""
			rec.Inline = true
			kept = append(kept, e)
		case keyringDest[e] != "":
			rec.RewrittenTo = keyringDest[e]
			kept = append(kept, keyringDest[e])
		}
		records = append(records, rec)
	}
	if len(kept) == 0 {
		for i := range records {
			records[i].Stripped = true
		}
		return "", records
	}
	return strings.Join(kept, ","), records
}

var (
	oneLineSourceRE = regexp.MustCompile(`^(\s*)(deb|deb-src)(\s+)(\[[^\]]*\])?(\s*)(.*)$`)
	// signedByOptRE captures the WHOLE value of a one-line "signed-by="
	// option, its quoted form included. That value is a list
	// (splitSignedByValue), so what is captured here is handed on to be
	// split, never used directly as a single keyring path.
	signedByOptRE = regexp.MustCompile(`(?i)signed-by=("[^"]*"|\S+)`)
)

func rewriteSignedByOneLine(file string, data []byte, keyringDest map[string]string) ([]byte, []SignedByRecord) {
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	var records []SignedByRecord
	stanza := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}
		m := oneLineSourceRE.FindStringSubmatch(line)
		if m == nil {
			out = append(out, line)
			continue
		}
		opts := m[4]
		if opts == "" {
			out = append(out, line)
			stanza++
			continue
		}
		inner := opts[1 : len(opts)-1]
		sm := signedByOptRE.FindStringSubmatchIndex(inner)
		if sm == nil {
			out = append(out, line)
			stanza++
			continue
		}
		orig := inner[sm[2]:sm[3]]
		rewritten, recs := rewriteSignedByList(file, stanza, orig, keyringDest, oneLineSignedBySeps)
		var newInner string
		if rewritten != "" {
			newInner = inner[:sm[2]] + rewritten + inner[sm[3]:]
		} else {
			newInner = inner[:sm[0]] + inner[sm[1]:]
		}
		newInner = strings.Join(strings.Fields(newInner), " ")
		rebuilt := m[2]
		if newInner != "" {
			rebuilt += " [" + newInner + "]"
		}
		if rest := strings.TrimSpace(m[6]); rest != "" {
			rebuilt += " " + rest
		}
		out = append(out, rebuilt)
		records = append(records, recs...)
		stanza++
	}
	return []byte(strings.Join(out, "\n")), records
}

// --- trusted=yes detection -------------------------------------------------
//
// A source carrying "trusted=yes" (one-line) or "Trusted: yes" (deb822) tells
// apt to accept its packages with no signature check whatsoever. apt still
// resolves and downloads from it happily and still exits 0, so nothing about
// the run's outcome distinguishes a package that came from there from one apt
// actually authenticated -- which is why fetchAndBuildSelections cannot simply
// stamp every selection lock.VerifiedAPTSigned. The lock is the one record an
// auditor reads for provenance; claiming a check that provably did not run is
// worse than recording no claim at all. See verificationFor in local.go.

// aptSourceEntry is one "deb"/"deb-src" line or one deb822 stanza, reduced to
// what provenance attribution needs: where it fetches from, and whether it
// was declared trusted-without-verification.
type aptSourceEntry struct {
	URIs    []string
	Trusted bool
}

var trustedOptRE = regexp.MustCompile(`(?i)(?:^|\s)trusted="?(?:yes|true|1)"?(?:\s|$)`)

// parseSourceEntries reads one source file's bytes, choosing the deb822 or
// one-line grammar by file name exactly as apt does (isDeb822SourceFile).
func parseSourceEntries(name string, data []byte) []aptSourceEntry {
	if isDeb822SourceFile(name) {
		return parseSourceEntriesDeb822(data)
	}
	return parseSourceEntriesOneLine(data)
}

func parseSourceEntriesOneLine(data []byte) []aptSourceEntry {
	var out []aptSourceEntry
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := oneLineSourceRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		e := aptSourceEntry{}
		if opts := m[4]; len(opts) >= 2 {
			e.Trusted = trustedOptRE.MatchString(opts[1 : len(opts)-1])
		}
		if fields := strings.Fields(m[6]); len(fields) > 0 {
			e.URIs = []string{fields[0]}
		}
		if len(e.URIs) > 0 {
			out = append(out, e)
		}
	}
	return out
}

func parseSourceEntriesDeb822(data []byte) []aptSourceEntry {
	var out []aptSourceEntry
	cur := aptSourceEntry{}
	flush := func() {
		if len(cur.URIs) > 0 {
			out = append(out, cur)
		}
		cur = aptSourceEntry{}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			// A field continuation (an inline armoured key, typically):
			// neither URIs nor Trusted is ever spelled that way.
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "uris":
			cur.URIs = append(cur.URIs, strings.Fields(value)...)
		case "trusted":
			v := strings.ToLower(unquoteSourceValue(value))
			cur.Trusted = v == "yes" || v == "true" || v == "1"
		}
	}
	flush()
	return out
}

// aptURItoFileName mangles a source URI the way apt names the files it stores
// under var/lib/apt/lists: the scheme identifier and any embedded credentials
// are removed, every unsafe byte is percent-encoded, and '/' becomes '_'.
//
// Derived from apt's own output, not from reading apt's source: the list files
// this repository has captured from real runs (see
// hack/experiments/out/e2/*/aptroot/var/lib/apt/lists/) show
// "http://archive.ubuntu.com/ubuntu" stored as
// "archive.ubuntu.com_ubuntu_dists_noble_main_binary-amd64_Packages" and
// "file:///src/hack/experiments/out/e2/repo/" as
// "_src_hack_experiments_out_e2_repo_._Packages" -- scheme gone, separators
// flattened, and the leading '/' of a file: path preserved as its own '_'.
//
// The result is used only as a PREFIX test against an index file's name
// (indexFileBelongsTo). It therefore does not have to reproduce apt's quoting
// byte for byte to be useful, and it fails in the safe direction: a URI this
// mangles differently than apt did simply does not match, and a match that is
// broader than apt's own (one source URI being a path prefix of another)
// downgrades a provenance claim rather than inflating one.
func aptURItoFileName(uri string) string {
	rest := uri
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+len("://"):]
	}
	// Strip userinfo ("user:password@host"), which apt clears before
	// mangling -- and which must never reach a name written into a
	// bundle-shipped artefact either.
	authority := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		authority = rest[:i]
	}
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		rest = rest[at+1:]
	}
	const bad = `\|{}[]<>"^~_=!@#$%&*`
	var b strings.Builder
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c == '/':
			b.WriteByte('_')
		case c <= 0x20 || c >= 0x7f || strings.IndexByte(bad, c) >= 0:
			fmt.Fprintf(&b, "%%%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// trustedIndexPrefixes returns the mangled list-file name prefix of every
// source in the private root that declared itself trusted=yes / Trusted: yes.
// A Packages index whose file name starts with one of these was fetched from
// such a source.
//
// The root's own materialised sources are read, not the snapshot's, because
// those are the files apt was actually pointed at: they include the external
// staging repo BuildPrivateRoot wrote from RootSpec.ExtraSources as well as
// everything copied out of the snapshot, and they are the post-rewrite text.
func trustedIndexPrefixes(rootDir string) []string {
	etc := filepath.Join(rootDir, "etc", "apt")
	files := []string{filepath.Join(etc, "sources.list")}
	if entries, err := os.ReadDir(filepath.Join(etc, "sources.list.d")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				files = append(files, filepath.Join(etc, "sources.list.d", e.Name()))
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, e := range parseSourceEntries(filepath.Base(f), data) {
			if !e.Trusted {
				continue
			}
			for _, u := range e.URIs {
				// apt normalises a base URI to end in '/' before appending
				// "dists/..." or a flat repository's own path, so the stored
				// name always has the separator that becomes this trailing
				// '_'. Keeping it is what stops "http://a/" matching
				// "http://ab/".
				if !strings.HasSuffix(u, "/") {
					u += "/"
				}
				p := aptURItoFileName(u)
				if p != "" && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// indexFileBelongsTo reports whether a fetched index file's base name was
// stored for a source with one of these mangled prefixes.
func indexFileBelongsTo(indexFile string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(indexFile, p) {
			return true
		}
	}
	return false
}
