// Package digest holds the file digest helpers shared by the store, the
// repository writer, the manifest builder and verify.
package digest

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// Prefix is the algorithm label used in store paths and log lines.
const Prefix = "sha256"

// SHA256Reader returns the lowercase hex SHA-256 of everything r yields, and
// the number of bytes read.
func SHA256Reader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, fmt.Errorf("digest: read: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// SHA256File returns the lowercase hex SHA-256 of the file at path and its size.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("digest: open %s: %w", path, err)
	}
	defer f.Close()
	return SHA256Reader(f)
}

// Bytes returns the lowercase hex SHA-256 of b.
func Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FileHashes is the hash set an apt Packages stanza carries for one file.
type FileHashes struct {
	Size   int64
	MD5    string
	SHA1   string
	SHA256 string
}

// AllFile computes MD5, SHA1 and SHA256 of a file in a single pass. MD5 and
// SHA1 are required by apt's index format for compatibility; debark never
// makes a trust decision on them.
func AllFile(path string) (FileHashes, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileHashes{}, fmt.Errorf("digest: open %s: %w", path, err)
	}
	defer f.Close()
	return AllReader(f)
}

// AllReader computes MD5, SHA1 and SHA256 of r in a single pass.
func AllReader(r io.Reader) (FileHashes, error) {
	var (
		m  = md5.New()
		s1 = sha1.New()
		s2 = sha256.New()
	)
	n, err := io.Copy(io.MultiWriter(m, s1, s2), r)
	if err != nil {
		return FileHashes{}, fmt.Errorf("digest: read: %w", err)
	}
	return FileHashes{
		Size:   n,
		MD5:    hex.EncodeToString(m.Sum(nil)),
		SHA1:   hex.EncodeToString(s1.Sum(nil)),
		SHA256: hex.EncodeToString(s2.Sum(nil)),
	}, nil
}

// Valid reports whether s looks like a lowercase hex SHA-256.
func Valid(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// Equal compares two digests case-insensitively.
func Equal(a, b string) bool { return strings.EqualFold(a, b) }
