// Public API of the repository package. repository/iface.go is
// the frozen contract (the Writer interface and its types); this file is the
// concrete, pure-Go implementation (ADR-011).

package repository

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"pault.ag/go/debian/deb"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
)

// NewWriter returns the pure-Go repository writer. It never shells out to
// apt-ftparchive or dpkg-scanpackages, and needs no apt-utils or dpkg-dev
// installed on the builder: the normal case is a slim container that has
// neither (see docs/dev/prototype-baseline.md, where the Bash prototype
// silently produced no Release file for exactly this reason).
func NewWriter() Writer { return writer{} }

type writer struct{}

func (writer) Write(ctx context.Context, in Input) (*Result, error) {
	if in.Dir == "" {
		return nil, dferr.New(dferr.Usage, "repository: Write: Input.Dir is required")
	}

	stanzas := make([]stanza, 0, len(in.Files))
	var poolBytes int64
	for _, pf := range in.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, err := readStanza(in.Dir, pf)
		if err != nil {
			return nil, err
		}
		stanzas = append(stanzas, st)
		poolBytes += st.hashes.Size
	}
	sort.SliceStable(stanzas, func(i, j int) bool { return stanzas[i].less(stanzas[j]) })

	texts := make([]string, len(stanzas))
	for i, st := range stanzas {
		texts[i] = st.text
	}
	// Stanzas are separated by exactly one blank line; each stanza's own text
	// already ends in a single "\n" after its last field (control.Paragraph.
	// WriteTo does not add a trailing blank line), so joining with "\n"
	// inserts the blank-line separator and the file ends in exactly one
	// trailing newline, never two.
	packagesBytes := []byte(strings.Join(texts, "\n"))

	if err := os.WriteFile(filepath.Join(in.Dir, "Packages"), packagesBytes, 0o644); err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "repository: write Packages")
	}
	packagesHashes, err := digest.AllReader(bytes.NewReader(packagesBytes))
	if err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "repository: hash Packages")
	}

	res := &Result{
		PackagesPath:   "Packages",
		PackagesSHA256: packagesHashes.SHA256,
		PackageCount:   len(stanzas),
		PoolBytes:      poolBytes,
	}

	var gzHashes digest.FileHashes
	if in.Compress {
		gzBytes, err := gzipBytes(packagesBytes)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(in.Dir, "Packages.gz"), gzBytes, 0o644); err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "repository: write Packages.gz")
		}
		gzHashes, err = digest.AllReader(bytes.NewReader(gzBytes))
		if err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "repository: hash Packages.gz")
		}
		res.PackagesGzPath = "Packages.gz"
		res.PackagesGzSHA256 = gzHashes.SHA256
	}

	releaseBytes, err := renderRelease(in.Release, packagesHashes, gzHashes, in.Compress)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(in.Dir, "Release"), releaseBytes, 0o644); err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "repository: write Release")
	}
	res.ReleasePath = "Release"
	res.ReleaseSHA256 = digest.Bytes(releaseBytes)

	return res, nil
}

// stanza is one Packages entry, already rendered to its final text, plus the
// sort key extracted from the control data.
type stanza struct {
	name, arch, version string
	text                string
	hashes              digest.FileHashes
}

func (s stanza) less(o stanza) bool {
	if s.name != o.name {
		return s.name < o.name
	}
	if s.arch != o.arch {
		return s.arch < o.arch
	}
	return s.version < o.version
}

// readStanza extracts pf's control stanza and renders the full Packages
// entry for it: the control fields as dpkg-deb wrote them into the .deb, in
// their original order (Package always first) minus the ones only this writer
// is allowed to state, followed by Filename, Size, MD5sum, SHA1 and SHA256.
func readStanza(dir string, pf PoolFile) (stanza, error) {
	if pf.Path == "" {
		return stanza{}, dferr.New(dferr.Usage, "repository: pool file has an empty Path")
	}
	if err := checkPoolPath(pf.Path); err != nil {
		return stanza{}, err
	}
	full := filepath.Join(dir, filepath.FromSlash(pf.Path))

	d, closer, err := deb.LoadFile(full)
	if err != nil {
		return stanza{}, dferr.Wrap(dferr.Usage, err, "repository: read control data of %s", pf.Path)
	}
	// The .deb is only read here, so a failed close has nothing to flush and
	// no bearing on the stanza already extracted; ignored at the call site
	// so a reader can tell this from a forgotten error.
	defer func() { _ = closer() }()

	para := d.Control.Paragraph
	name := para.Values["Package"]
	if name == "" {
		return stanza{}, dferr.New(dferr.Usage, "repository: %s: control stanza has no Package field", pf.Path)
	}
	arch := para.Values["Architecture"]
	version := para.Values["Version"]

	hashes := pf.Hashes
	if (hashes == digest.FileHashes{}) {
		hashes, err = digest.AllFile(full)
		if err != nil {
			return stanza{}, dferr.Wrap(dferr.Environment, err, "repository: hash %s", pf.Path)
		}
	}

	order, values, err := controlFields(pf.Path, para.Order, para.Values)
	if err != nil {
		return stanza{}, err
	}
	order = append(order, "Filename", "Size", "MD5sum", "SHA1", "SHA256")
	values["Filename"] = pf.Path
	values["Size"] = strconv.FormatInt(hashes.Size, 10)
	values["MD5sum"] = hashes.MD5
	values["SHA1"] = hashes.SHA1
	values["SHA256"] = hashes.SHA256

	text := renderStanza(order, values)
	return stanza{name: name, arch: arch, version: version, text: text, hashes: hashes}, nil
}

// renderStanza writes one Packages entry: each key in order, in "Key: value"
// form, with multi-line values re-indented one field at a time.
//
// This does not reuse control.Paragraph.WriteTo, which has a round-trip bug
// for exactly the case Debian Description fields hit constantly: when a
// multi-line value's last continuation line is ordinary text (not itself a
// blank " ." marker), WriteTo's blanket "\n" -> "\n " replacement also
// rewrites the value's own trailing newline, leaving a stray line containing
// a single space between the field and whatever follows it - a value that
// re-parses as an extra empty trailing continuation line, which is not an
// exact round-trip.
func renderStanza(order []string, values map[string]string) string {
	var b strings.Builder
	for _, key := range order {
		writeControlField(&b, key, values[key])
	}
	return b.String()
}

// writeControlField appends one "Key: value\n" field to b, re-indenting a
// multi-line value one continuation line per source line and restoring the
// " ." convention for a blank line in the middle of the value - never after
// the last line, which simply ends the field.
//
// A multi-line value coming out of control.Paragraph.Values always carries
// exactly one trailing "\n" beyond what its content lines need (every
// continuation line the parser reads appends its own trailing "\n", the
// final one included), so trimming a single trailing "\n" before splitting
// recovers precisely the original set of lines, whether or not the value's
// true last line was itself a blank " ." marker.
func writeControlField(b *strings.Builder, key, value string) {
	lines := strings.Split(strings.TrimSuffix(value, "\n"), "\n")
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(lines[0])
	b.WriteByte('\n')
	for _, line := range lines[1:] {
		b.WriteByte(' ')
		if line == "" {
			b.WriteByte('.')
		} else {
			b.WriteString(line)
		}
		b.WriteByte('\n')
	}
}

// controlFields turns one .deb's control paragraph into the field order and
// value map the Packages stanza is rendered from: "Package" first, then the
// remaining control fields in their original order, with every field the
// writer itself is the sole authority for removed.
//
// SECURITY. Everything in a control stanza is attacker-controlled for a
// vendor .deb, and until this function existed all of it was copied verbatim
// into Packages before the writer appended its own Filename, Size, MD5sum,
// SHA1 and SHA256. Only those five exact spellings were overwritten, so
// anything else survived - and apt parses field names case-insensitively
// (pkgTagSection folds case in its field lookup), so "filename:", "SIZE:" and
// "sha256:" all reached apt as duplicates of the writer's own authoritative
// lines.
//
// The sharpest of these needs no duplicate-resolution argument at all:
// "SHA512:". debark never writes that field, so apt sees exactly one - the
// attacker's. A vendor .deb declaring a SHA512 of some other file makes apt
// refuse to install the package with a hash mismatch on the far side of the
// air gap, long after "debark verify" has passed: the manifest covers
// Packages' bytes faithfully, and those bytes are exactly what the .deb asked
// for. A deniable, targeted, install-time denial of service that debark's
// trust layer never sees.
//
// Two rules close it. Field names are compared case-folded, never verbatim,
// so no spelling of a reserved name gets through; and a name that is not a
// legal deb822 field name is refused outright rather than echoed, which is
// what stops a zero-length name (a control line beginning with ":") or an
// ESC or NUL byte smuggled into a name from ever reaching the file.
func controlFields(poolPath string, order []string, values map[string]string) ([]string, map[string]string, error) {
	outOrder := make([]string, 0, len(order)+5)
	outValues := make(map[string]string, len(values)+5)
	seen := make(map[string]bool, len(order))

	keep := func(key string) error {
		if !validFieldName(key) {
			return dferr.New(dferr.Verification,
				"repository: %s: control stanza has an illegal field name %q (a field name must match %s)",
				poolPath, key, fieldNameGrammar)
		}
		lower := strings.ToLower(key)
		if seen[lower] {
			// Two field names that differ only in case are one field as far
			// as apt is concerned. Dropping one silently would discard data;
			// emitting both would hand apt a stanza whose meaning depends on
			// which duplicate its parser happens to keep. Neither is a
			// decision this writer is entitled to make on an operator's
			// behalf.
			return dferr.New(dferr.Verification,
				"repository: %s: control stanza declares %q more than once (field names are case-insensitive)",
				poolPath, key)
		}
		seen[lower] = true
		if aptOwnsField(lower) {
			// Refusing here instead of dropping was considered and rejected:
			// a .deb carrying its own Filename or SHA256 is not by itself
			// evidence of an attack (some vendor tooling copies a Packages
			// stanza back into the control file), and stating those fields is
			// precisely this writer's job. Dropping is the outcome that is
			// both safe and true.
			return nil
		}
		if err := checkFieldValue(poolPath, key, values[key]); err != nil {
			return err
		}
		outOrder = append(outOrder, key)
		outValues[key] = values[key]
		return nil
	}

	// Debian's own tooling always writes Package first; hoisting it makes
	// that convention an invariant of the writer's output rather than an
	// assumption about its input.
	for _, k := range order {
		if k == "Package" {
			if err := keep(k); err != nil {
				return nil, nil, err
			}
			break
		}
	}
	for _, k := range order {
		if k == "Package" {
			continue
		}
		if err := keep(k); err != nil {
			return nil, nil, err
		}
	}
	return outOrder, outValues, nil
}

// aptOwnsField reports whether a lowercased control field name is one the
// repository writer alone gets to state, because apt reads it as a fact about
// the .deb file sitting in the pool rather than about the package inside it.
//
// The digest names are apt's own supported-hash list, not a guess:
// HashString::SupportedHashes() in apt-pkg/contrib/hashes.cc is
// {"SHA512", "SHA256", "SHA1", "MD5Sum"}, and debRecordParser::Hashes()
// (apt-pkg/deb/debrecords.cc) reads exactly those four out of a Packages
// stanza before taking the file's length from "Size". "Filename" is the URI
// apt resolves against the repository root. All six are matched case-folded,
// because apt's own field lookup is case-insensitive.
//
// "Description-md5" is deliberately NOT in this set: it digests the package's
// description, not the file, and apt genuinely wants the .deb's own value.
// Matching whole field names rather than substrings is what keeps it out.
func aptOwnsField(lower string) bool {
	switch lower {
	case "filename", "size", "md5sum", "sha1", "sha256", "sha512":
		return true
	}
	return false
}

// fieldNameGrammar describes validFieldName where an error message has to
// quote it.
const fieldNameGrammar = "[A-Za-z0-9][A-Za-z0-9-]*"

// validFieldName reports whether s is a deb822 field name this writer is
// willing to emit.
//
// Debian Policy 5.1 permits a wider set (any US-ASCII except control
// characters, space and colon, not starting with "#" or "-"), but every field
// name apt, dpkg and Debian's own archive actually use is alphanumerics and
// hyphens. Narrowing to that subset costs nothing on real input and removes a
// whole class of bytes a Packages consumer might read as structure rather
// than as a name.
func validFieldName(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	if !isASCIIAlnum(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isASCIIAlnum(s[i]) && s[i] != '-' {
			return false
		}
	}
	return true
}

func isASCIIAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// checkFieldValue refuses control bytes inside a control field's value.
//
// Newline is allowed because it is how a multi-line value (every Description)
// is held in memory; writeControlField re-indents every line after the first
// into a continuation line, so a newline can never split a value into a
// forged second field. Tab is allowed because real Description fields contain
// them. Everything else below U+0020, plus DEL, is refused: a NUL truncates
// the field for any C consumer that reads the index with str* functions, and
// an ESC turns "apt show" and "debark inspect" output into whatever the
// attacker would like an operator to read.
func checkFieldValue(poolPath, key, value string) error {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\n' || c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return dferr.New(dferr.Verification,
				"repository: %s: control field %q contains the control byte %#02x at offset %d",
				poolPath, key, c, i)
		}
	}
	return nil
}

// checkPoolPath refuses a PoolFile.Path that is not a plain, clean,
// forward-slashed path relative to the repository root.
//
// SECURITY. Path is not just where the writer opens the file - it is also,
// verbatim, the Filename field written into Packages, which is the URI apt
// resolves against the repository root when install mounts the bundle's repo
// with "Trusted: yes". Until this check existed the two were not even the
// same string: the writer opened filepath.Join(dir, FromSlash(Path)), which
// silently cleans, and wrote Path raw. So "../outside_1_amd64.deb" indexed a
// file outside the manifest-covered tree while still resolving to a real file
// on the builder, and "pool/x/../g/good/..." was emitted uncleaned. That was
// the one route found to break manifest coverage from inside a bundle that
// otherwise verifies clean.
//
// PoolPath already refuses an escaping path and iface.go's comment on it says
// so at length - but Write never enforced that its input had come from
// PoolPath, and a contract that only holds when callers remember it is not a
// boundary. This check is deliberately weaker than "Path must equal
// PoolPath(pkg, filename)", for two reasons. Debian's pool layout keys the
// directory on the SOURCE package name, not the binary one, so recomputing
// PoolPath from the .deb's own Package field would reject perfectly ordinary
// archives; and this same writer indexes the flat staging repository the
// engine builds for external .deb inputs (core/engine/inputs.go), where each
// file legitimately sits at the repository root under its own name and no
// pool/ prefix exists at all. What matters for safety is not the shape of the
// path but that it cannot leave the tree and cannot differ from the string
// that was opened - which is exactly what is checked here.
func checkPoolPath(p string) error {
	bad := func(why string) error {
		return dferr.New(dferr.Usage, "repository: pool file path %q %s", p, why)
	}
	if len(p) > 4096 {
		return bad("is longer than 4096 bytes")
	}
	if !utf8.ValidString(p) {
		return bad("is not valid UTF-8")
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '\\':
			return bad("contains a backslash (pool paths are always forward-slashed)")
		case c < 0x20 || c == 0x7f:
			return bad(fmt.Sprintf("contains the control byte %#02x", c))
		}
	}
	if strings.HasPrefix(p, "/") {
		return bad("is absolute")
	}
	if len(p) >= 2 && p[1] == ':' {
		return bad("names a drive letter")
	}
	// path.Clean, not filepath.Clean: the contract is a forward-slashed
	// relative path on every OS, so the comparison must not depend on which
	// separator the builder happens to run with. A path that is already its
	// own Clean has no ".", no "..", no empty component and no trailing
	// slash - which is what makes "the string opened" and "the string
	// written" provably the same string.
	if cleaned := path.Clean(p); cleaned != p {
		return bad(fmt.Sprintf("is not in cleaned form (%q would be written into Filename but %q would be opened)", p, cleaned))
	}
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return bad("escapes the repository root")
	}
	return nil
}

// gzipBytes compresses data with a fixed header - no modification time, no
// original filename, no comment - so that compressing the same input twice,
// on any machine, at any time, produces byte-identical output.
// compress/gzip writes the current time into the header unless ModTime is
// set explicitly; a zero time.Time is before the Unix epoch, which the
// package treats as "omit the mtime field" (gzip spec section 2.3.1), so it
// is set here even though it is also compress/gzip's zero value, to make the
// determinism requirement an explicit invariant rather than an accident of
// defaults.
func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "repository: create gzip writer")
	}
	zw.ModTime = time.Time{}
	zw.Name = ""
	zw.Comment = ""
	zw.OS = 255 // 255 = unknown, per the gzip spec; debark runs on several OSes
	if _, err := zw.Write(data); err != nil {
		// Cleanup on an error path: the write already failed, so the flush
		// this Close would perform cannot make the output usable and its
		// error would only mask the real one. The Close that matters is the
		// one below, on the success path, and that one is checked.
		_ = zw.Close()
		return nil, dferr.Wrap(dferr.Environment, err, "repository: gzip write")
	}
	if err := zw.Close(); err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "repository: gzip close")
	}
	return buf.Bytes(), nil
}

// renderRelease writes the Release document: the fields listed in the design
// (Origin, Label, Suite, Codename, Architectures, Components, Date,
// Description) followed by the MD5Sum/SHA1/SHA256 hash blocks for Packages
// and, when compressed, Packages.gz. Field order is fixed so two runs over
// the same Input produce byte-identical bytes.
func renderRelease(f ReleaseFields, pkgHashes, gzHashes digest.FileHashes, compress bool) ([]byte, error) {
	var b strings.Builder
	var ferr error
	field := func(key, value string) {
		if value == "" || ferr != nil {
			return
		}
		if err := checkReleaseField(key, value); err != nil {
			ferr = err
			return
		}
		fmt.Fprintf(&b, "%s: %s\n", key, value)
	}

	field("Origin", f.Origin)
	field("Label", f.Label)
	field("Suite", f.Suite)
	field("Codename", f.Codename)
	if len(f.Architectures) > 0 {
		field("Architectures", strings.Join(f.Architectures, " "))
	}
	if len(f.Components) > 0 {
		field("Components", strings.Join(f.Components, " "))
	}
	field("Date", f.Date)
	field("Description", f.Description)
	if f.NotAutomatic {
		field("NotAutomatic", "yes")
	}
	if f.ButAutomaticUpgrades {
		field("ButAutomaticUpgrades", "yes")
	}
	if len(f.ExtraFields) > 0 {
		keys := make([]string, 0, len(f.ExtraFields))
		for k := range f.ExtraFields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			field(k, f.ExtraFields[k])
		}
	}
	if ferr != nil {
		return nil, ferr
	}

	type hashed struct {
		path string
		h    digest.FileHashes
	}
	entries := []hashed{{"Packages", pkgHashes}}
	if compress {
		entries = append(entries, hashed{"Packages.gz", gzHashes})
	}

	// The hash blocks are written last so that no operator-supplied field can
	// ever precede them, and are built entirely from values this writer
	// computed - so nothing below this line needs validating.
	fmt.Fprint(&b, "MD5Sum:\n")
	for _, e := range entries {
		fmt.Fprintf(&b, " %s %d %s\n", e.h.MD5, e.h.Size, e.path)
	}
	fmt.Fprint(&b, "SHA1:\n")
	for _, e := range entries {
		fmt.Fprintf(&b, " %s %d %s\n", e.h.SHA1, e.h.Size, e.path)
	}
	fmt.Fprint(&b, "SHA256:\n")
	for _, e := range entries {
		fmt.Fprintf(&b, " %s %d %s\n", e.h.SHA256, e.h.Size, e.path)
	}

	return []byte(b.String()), nil
}

// checkReleaseField refuses a Release field whose name or value could forge a
// second field.
//
// SECURITY. Release is a deb822 document in which a line beginning with a
// space is a continuation of the field above it and any other line starts a
// new field - so an unvalidated value containing a newline does not merely
// look untidy, it writes fields. Codename comes from the target snapshot and
// ExtraFields is a public map on a frozen interface, and a Codename of
// "noble\nSHA256:\n <digest> <size> Packages" produced a Release carrying a
// second, attacker-chosen SHA256 block AHEAD of the writer's real one -
// exactly the position from which a lenient parser takes the first value it
// finds. Refusing CR as well as LF matters for the same reason: a consumer
// that splits on either sees two lines where this writer counted one.
//
// A leading space in a value is refused because "Key:  value" re-parses with
// a leading space in the value on a strict reader and, more importantly,
// because it is the shape a continuation line takes - allowing it would let a
// value dictate whether the NEXT line is read as data or as structure.
//
// The class is Usage, not Verification: ReleaseFields is an argument this
// package's caller assembles, so a bad one is a debark bug or an operator
// configuration error, not a hostile bundle being examined.
func checkReleaseField(key, value string) error {
	if !validFieldName(key) {
		return dferr.New(dferr.Usage,
			"repository: Release field name %q is not a legal field name (must match %s)",
			key, fieldNameGrammar)
	}
	if strings.HasPrefix(value, " ") || strings.HasPrefix(value, "\t") {
		return dferr.New(dferr.Usage,
			"repository: Release field %q has a value beginning with whitespace (%q), which re-parses as a continuation line",
			key, value)
	}
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 0x20 || c == 0x7f {
			return dferr.New(dferr.Usage,
				"repository: Release field %q contains the control byte %#02x at offset %d, which would forge a second field",
				key, c, i)
		}
	}
	return nil
}
