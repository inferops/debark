package fetch

// A minimal, dependency-free reader for the Unix `ar(1)` archive format that
// a .deb file is. The format is small and stable (Debian Policy §7.2): an
// 8-byte magic, then any number of 60-byte member headers each followed by
// that member's bytes (padded to an even length).
//
// go.mod did not carry pault.ag/go/debian when this package began, so this
// package verifies and reads .deb control data itself rather than depending
// on that library; the format is a public, frozen, decades-old standard,
// not a reimplementation of anyone's original logic. A dependency on
// pault.ag/go/debian was added to go.mod mid-session by a different,
// concurrently running package — see crossvalidate_test.go for why this
// package still reads and writes .deb files itself rather than switching
// over now that the dependency exists, and for how that library is used
// instead: as an independent check that this package's own reader and
// writer agree with a second, community-maintained implementation.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/inferops/debark/core/dferr"
)

// arMagic is the fixed 8-byte header every ar archive starts with.
const arMagic = "!<arch>\n"

// arHeaderLen is the fixed size of one ar member header.
const arHeaderLen = 60

// arMember is one member's header, as parsed from its 60-byte record.
type arMember struct {
	Name string
	Size int64
}

// arReader walks the members of an ar archive sequentially. It never seeks:
// callers that do not want a member's content must call Skip, which
// discards exactly that member's (padded) bytes so Next can continue.
type arReader struct {
	r    *bufio.Reader
	pos  int64 // bytes consumed of the current member's content
	size int64 // current member's declared size
}

// newArReader checks the magic and returns a reader positioned at the first
// member header.
func newArReader(r io.Reader) (*arReader, error) {
	br := bufio.NewReaderSize(r, 4096)
	magic := make([]byte, len(arMagic))
	if _, err := io.ReadFull(br, magic); err != nil {
		return nil, fmt.Errorf("not an ar archive (%w)", err)
	}
	if string(magic) != arMagic {
		return nil, errors.New("not an ar archive: bad magic")
	}
	return &arReader{r: br}, nil
}

// Next skips whatever remains of the previous member (including its pad
// byte) and parses the next member's header.
func (a *arReader) Next() (arMember, error) {
	if a.pos < a.size {
		if err := a.skip(a.size - a.pos); err != nil {
			return arMember{}, err
		}
	}
	if a.size%2 == 1 {
		// Odd-length members are padded with one byte so the next header
		// stays on an even offset.
		if _, err := io.CopyN(io.Discard, a.r, 1); err != nil {
			return arMember{}, fmt.Errorf("ar: reading pad byte: %w", err)
		}
	}

	header := make([]byte, arHeaderLen)
	if _, err := io.ReadFull(a.r, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return arMember{}, io.EOF
		}
		return arMember{}, fmt.Errorf("ar: reading member header: %w", err)
	}
	m, err := parseArHeader(header)
	if err != nil {
		return arMember{}, err
	}
	a.pos = 0
	a.size = m.Size
	return m, nil
}

// Read reads from the current member's content. It never reads past that
// member's declared size.
func (a *arReader) Read(p []byte) (int, error) {
	remaining := a.size - a.pos
	if remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := a.r.Read(p)
	a.pos += int64(n)
	return n, err
}

func (a *arReader) skip(n int64) error {
	written, err := io.CopyN(io.Discard, a.r, n)
	a.pos += written
	if err != nil {
		return fmt.Errorf("ar: skipping member content: %w", err)
	}
	return nil
}

// parseArHeader decodes one 60-byte ar member header. Layout (Debian Policy
// §7.2 / SysV ar):
//
//	0   16  name, space-padded, optional trailing "/"
//	16  12  mtime, decimal ASCII
//	28  6   uid, decimal ASCII
//	34  6   gid, decimal ASCII
//	40  8   mode, octal ASCII
//	48  10  size in bytes, decimal ASCII
//	58  2   magic 0x60 0x0A
func parseArHeader(h []byte) (arMember, error) {
	if len(h) != arHeaderLen {
		return arMember{}, errors.New("ar: short header")
	}
	if h[58] != 0x60 || h[59] != 0x0A {
		return arMember{}, errors.New("ar: bad header terminator")
	}
	name := strings.TrimSuffix(strings.TrimRight(string(h[0:16]), " "), "/")
	sizeField := strings.TrimSpace(string(h[48:58]))
	size, err := strconv.ParseInt(sizeField, 10, 64)
	if err != nil {
		return arMember{}, fmt.Errorf("ar: bad size field %q: %w", sizeField, err)
	}
	if size < 0 {
		return arMember{}, fmt.Errorf("ar: negative size field %q", sizeField)
	}
	return arMember{Name: name, Size: size}, nil
}

// SniffDeb reports whether r begins with a well-formed .deb: an ar archive
// whose first member is named "debian-binary" and whose content is a
// recognised format-version line. It reads only that first member (a few
// bytes) and returns as soon as it knows the answer, so a caller can reject
// a wrong download (a 404 page saved as foo.deb is the common case) without
// reading the rest of the body.
//
// A nil return means "this looks like a real .deb"; it is not a guarantee
// the archive is well-formed all the way through (Fetch and FromLocalFile
// still ask the store to record its own digest of exactly what is on disk).
func SniffDeb(r io.Reader) error {
	ar, err := newArReader(r)
	if err != nil {
		return dferr.Wrap(dferr.Incomplete, err,
			"does not look like a .deb file (not an ar archive; a download error page saved as a .deb is a common cause)")
	}
	m, err := ar.Next()
	if err != nil {
		return dferr.Wrap(dferr.Incomplete, err, "does not look like a .deb file (empty or truncated ar archive)")
	}
	if m.Name != "debian-binary" {
		return dferr.New(dferr.Incomplete,
			"does not look like a .deb file (first ar member is %q, expected \"debian-binary\")", m.Name)
	}
	// The debian-binary member is a few bytes ("2.0\n"); read a bounded
	// amount so a maliciously huge "size" field cannot make us buffer more
	// than a token amount before we notice something is wrong.
	buf := make([]byte, 32)
	n, _ := io.ReadFull(&io.LimitedReader{R: ar, N: int64(len(buf))}, buf)
	version := strings.TrimRight(string(buf[:n]), "\x00")
	if !strings.HasPrefix(version, "2.0\n") && !strings.HasPrefix(version, "2.0") {
		return dferr.New(dferr.Incomplete,
			"does not look like a .deb file (unrecognised debian-binary version %q)", strings.TrimSpace(version))
	}
	return nil
}
