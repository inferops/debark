package fetch

// Control-data extraction: the helper the engine needs to turn a batch of
// ingested external .deb files into apt package names it can ask the
// private staging repo to resolve, plus enough of the raw control
// stanza to record in the lock and warn on.
//
// This comment sits below the package clause, as debsniff.go's and
// fixture.go's do, so it stays a file-level note: api.go holds the one
// package doc comment for package fetch.

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/inferops/debark/core/dferr"

	pdeb "pault.ag/go/debian/deb"
)

// ControlInfo is the subset of a .deb's control stanza the engine needs.
// Relationship fields (Depends and the rest) are exactly as written in the
// control file — deb822 continuation lines folded into one, joined by a
// single space — never parsed into individual alternatives: apt is the
// oracle for what satisfies a dependency, debark only reports what the
// file itself declares.
type ControlInfo struct {
	Package      string
	Source       string
	Version      string
	Architecture string
	MultiArch    string
	Depends      string
	PreDepends   string
	Recommends   string
	Suggests     string
	Breaks       string
	Conflicts    string
	Replaces     string
	Provides     string
}

// SourceName mirrors Debian Policy §5.6.9: the source package name, falling
// back to Package when the control file has no separate Source field, and
// stripping a parenthesised source version when present ("pkg (1.2-3)").
func (c ControlInfo) SourceName() string {
	name := c.Source
	if name == "" {
		return c.Package
	}
	if i := strings.IndexByte(name, ' '); i >= 0 {
		name = name[:i]
	}
	return name
}

// ControlResult pairs one .deb path with its extracted control info or the
// error that prevented extraction. It mirrors Result's {input, output, err}
// shape so a caller already handling fetch batches can handle this one the
// same way.
type ControlResult struct {
	Path string
	Info ControlInfo
	Err  error
}

// ReadControlInfo opens the .deb at pathToDeb and extracts its control
// stanza: the package identity apt needs to index it as part of a staging
// repo, plus the raw relationship fields for evidence and warnings.
//
// This package first assumed dpkg-deb always gzips the control member
// regardless of the data member's compression, and shipped a hand-rolled
// gzip-only reader on that basis. That assumption was wrong: inspecting a
// live-downloaded Google Chrome .deb and a current Debian archive package
// both showed control.tar.xz, not control.tar.gz.
// ReadControlInfo therefore only handles the ar container and the control
// paragraph itself by hand; finding the right control.* member and
// decompressing it (gzip, xz, bzip2, lzma or zstd — whatever dpkg-deb
// actually used) is delegated to pault.ag/go/debian/deb, which already
// solves exactly that and is a real dependency of this module (see
// debsniff.go's package comment for why the rest of this package still does
// not depend on it).
func ReadControlInfo(pathToDeb string) (ControlInfo, error) {
	f, err := os.Open(pathToDeb)
	if err != nil {
		return ControlInfo{}, dferr.Wrap(dferr.Usage, err, "fetch: opening %s", pathToDeb)
	}
	defer f.Close()
	info, err := readControlInfo(f)
	if err != nil {
		return ControlInfo{}, fmt.Errorf("fetch: %s: %w", pathToDeb, err)
	}
	return info, nil
}

// ReadControlInfoBatch reads every path in paths — typically the
// Filename/StorePath values from a batch of Fetched results — and returns
// one ControlResult per input, in the same order. A bad file does not stop
// the batch: the caller can build the staging repo's package list from the
// good entries and report the rest, echoing the "resolve together, then
// report each unresolved name" shape apt resolution already uses.
func ReadControlInfoBatch(paths []string) []ControlResult {
	out := make([]ControlResult, len(paths))
	for i, p := range paths {
		info, err := ReadControlInfo(p)
		out[i] = ControlResult{Path: p, Info: info, Err: err}
	}
	return out
}

// readControlInfo takes an io.ReaderAt (a *os.File and a *bytes.Reader both
// qualify, which covers every caller in this package) because
// pault.ag/go/debian/deb.LoadAr needs random access: each ar member's data
// is exposed as an io.SectionReader at a computed absolute offset, rather
// than by tracking a running position through a single sequential stream.
func readControlInfo(r io.ReaderAt) (ControlInfo, error) {
	ar, err := pdeb.LoadAr(r)
	if err != nil {
		return ControlInfo{}, dferr.Wrap(dferr.Incomplete, err, "does not look like a .deb file")
	}
	first, err := ar.Next()
	if err != nil {
		return ControlInfo{}, dferr.Wrap(dferr.Incomplete, err, "does not look like a .deb file (empty archive)")
	}
	if first.Name != "debian-binary" {
		return ControlInfo{}, dferr.New(dferr.Incomplete,
			"does not look like a .deb file (first ar member is %q, expected \"debian-binary\")", first.Name)
	}

	for {
		m, err := ar.Next()
		if err == io.EOF {
			return ControlInfo{}, dferr.New(dferr.Incomplete,
				"missing control member (checked every ar member, found none named control.*)")
		}
		if err != nil {
			return ControlInfo{}, dferr.Wrap(dferr.Incomplete, err, "reading ar members")
		}
		if !strings.HasPrefix(m.Name, "control.") {
			continue // most likely data.*; pault.ag's random-access Data field means we do not need to skip it ourselves
		}
		return readControlMember(m)
	}
}

// maxControlSize bounds how many decompressed bytes readControlMember will
// read for one .deb's control file. A real control file is one deb822
// paragraph — a handful of kilobytes even with a long Description; this is
// set roughly three orders of magnitude above that, so no real .deb is ever
// rejected, while a maliciously crafted, highly compressed control.tar.*
// (a decompression bomb) can no longer exhaust builder memory: F6,
// docs/security/review-findings.md.
const maxControlSize = 8 << 20 // 8 MiB

// readControlMember decompresses one control.* ar member (gzip, xz, bzip2,
// lzma or zstd — ArEntry.Tarfile dispatches on the member's own extension,
// falling back to "assume uncompressed" for anything it does not recognise
// rather than refusing outright) and hands the "control" file inside it, as
// plain text, to this package's own deb822 reader. The relationship-field
// values this returns are therefore exactly as written in the control file
// either way: nothing in this path ever goes through a typed
// parse-then-restringify step that could reformat them.
func readControlMember(m *pdeb.ArEntry) (ControlInfo, error) {
	if !m.IsTarfile() {
		return ControlInfo{}, dferr.New(dferr.Incomplete, "%s is not a tar archive", m.Name)
	}
	tr, closer, err := m.Tarfile()
	if err != nil {
		return ControlInfo{}, dferr.Wrap(dferr.Incomplete, err, "decompressing %s", m.Name)
	}
	defer closer.Close()

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return ControlInfo{}, dferr.New(dferr.Incomplete, "%s has no control file inside it", m.Name)
		}
		if err != nil {
			return ControlInfo{}, dferr.Wrap(dferr.Incomplete, err, "reading %s", m.Name)
		}
		if path.Clean("/"+hdr.Name) != "/control" {
			continue
		}
		// io.LimitedReader, not a bare io.ReadAll: a real control file never
		// comes close to maxControlSize, so if the limit is ever actually
		// reached that is decisive evidence of a decompression bomb, not a
		// legitimate file that happened to be large. Checking limited.N == 0
		// afterwards is what tells "read exactly the limit, then stopped"
		// apart from "the file really was that short" — a truncated control
		// file that then silently parses (perhaps dropping the very field
		// that would have made it look wrong) would be worse than refusing
		// outright.
		limited := &io.LimitedReader{R: tr, N: maxControlSize + 1}
		body, err := io.ReadAll(limited)
		if err != nil {
			return ControlInfo{}, dferr.Wrap(dferr.Incomplete, err, "reading control file inside %s", m.Name)
		}
		if limited.N == 0 {
			return ControlInfo{}, dferr.New(dferr.Incomplete,
				"control file inside %s exceeds the %d-byte limit (decompresses to far more data than a real control file ever does; refusing rather than silently truncating it)",
				m.Name, maxControlSize)
		}
		return parseControlStanza(string(body))
	}
}

// parseControlStanza is a minimal deb822 reader for exactly what a binary
// control file is: one paragraph of "Key: Value" fields, where a
// continuation line (starting with a space or tab) folds into the previous
// field, and a literal " ." continuation marks a blank line within a
// multi-line value (used by Description; irrelevant to the fields read
// here, but handled so it cannot corrupt whatever field precedes one).
// Field names are matched case-insensitively, per the format.
func parseControlStanza(text string) (ControlInfo, error) {
	fields := map[string]string{}
	curKey := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break // end of the control file's one and only paragraph
		}
		if line[0] == ' ' || line[0] == '\t' {
			if curKey == "" {
				continue // continuation before any field: malformed, ignore
			}
			cont := strings.TrimLeft(line, " \t")
			if cont == "." {
				continue
			}
			if fields[curKey] != "" {
				fields[curKey] += " " + cont
			} else {
				fields[curKey] = cont
			}
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue // malformed line outside any field: ignore, do not fail the whole file over it
		}
		key := strings.ToLower(strings.TrimSpace(line[:idx]))
		fields[key] = strings.TrimSpace(line[idx+1:])
		curKey = key
	}

	info := ControlInfo{
		Package:      fields["package"],
		Source:       fields["source"],
		Version:      fields["version"],
		Architecture: fields["architecture"],
		MultiArch:    fields["multi-arch"],
		Depends:      fields["depends"],
		PreDepends:   fields["pre-depends"],
		Recommends:   fields["recommends"],
		Suggests:     fields["suggests"],
		Breaks:       fields["breaks"],
		Conflicts:    fields["conflicts"],
		Replaces:     fields["replaces"],
		Provides:     fields["provides"],
	}

	var missing []string
	if info.Package == "" {
		missing = append(missing, "Package")
	}
	if info.Version == "" {
		missing = append(missing, "Version")
	}
	if info.Architecture == "" {
		missing = append(missing, "Architecture")
	}
	if len(missing) > 0 {
		return ControlInfo{}, dferr.New(dferr.Incomplete,
			"control file is missing required field(s): %s", strings.Join(missing, ", "))
	}
	return info, nil
}
