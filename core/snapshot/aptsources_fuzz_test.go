package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzExtractSignedBy fuzzes both sources-file extractors (the one-line
// sources.list(5) format and the deb822 .sources format — extractSignedBy
// deliberately runs both unconditionally, see its own doc) against every
// real captured sources fixture in the tree, plus hand-picked edge cases:
// an inline armoured key block, a broken/missing Signed-By, mixed one-line
// and deb822 content in the same file. The invariant is the two result
// shapes' own mutual exclusivity, encoded directly in
// extractDeb822SignedBy: a signedByRef is either a bare keyring Path or an
// inline Armored block, never both — a parser that ever set both would
// leave a caller unable to tell which one to honour.
func FuzzExtractSignedBy(f *testing.F) {
	for _, path := range []string{
		filepath.Join("testdata", "debian12-list", "etc", "apt", "sources.list"),
		filepath.Join("testdata", "ubuntu2404-deb822", "etc", "apt", "sources.list.d", "ubuntu.sources"),
		filepath.Join("testdata", "inline-armored", "etc", "apt", "sources.list.d", "inline.sources"),
		filepath.Join("testdata", "missing-keyring", "etc", "apt", "sources.list.d", "broken.sources"),
		filepath.Join("testdata", "mixed", "etc", "apt", "sources.list.d", "classic.list"),
		filepath.Join("testdata", "mixed", "etc", "apt", "sources.list.d", "modern.sources"),
	} {
		if data, err := os.ReadFile(path); err == nil {
			f.Add(data)
		}
	}
	f.Add([]byte("deb [signed-by=/usr/share/keyrings/x.gpg] http://host/debian bookworm main\n"))
	f.Add([]byte("deb [ signed-by = /x.gpg trusted=yes ] http://host/debian bookworm main\n"))
	f.Add([]byte("deb http://host/debian bookworm main\n# deb [signed-by=/x.gpg] http://host2/debian bookworm main\n"))
	f.Add([]byte("Types: deb\nURIs: http://host/debian\nSuites: bookworm\nSigned-By: /x.gpg\n"))
	f.Add([]byte("Types: deb\nSigned-By:\n -----BEGIN PGP PUBLIC KEY BLOCK-----\n .\n abcd\n -----END PGP PUBLIC KEY BLOCK-----\n"))
	f.Add([]byte("Signed-By:\n"))
	f.Add([]byte(""))
	f.Add([]byte("deb [signed-by=] http://host/debian bookworm main\n"))
	f.Add([]byte("garbage\x00with\x00nuls\nand\xffbad\xfeutf8\n"))
	f.Add([]byte("deb [signed-by=/a.gpg] http://x bookworm main\nTypes: deb\nSigned-By: /b.gpg\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, ref := range extractSignedBy(data) {
			if ref.Path != "" && ref.Armored != "" {
				t.Fatalf("signedByRef has both Path (%q) and Armored set for input %q", ref.Path, data)
			}
		}
	})
}

// FuzzParseDeb822 fuzzes the underlying deb822 stanza splitter directly.
// The invariant is flush()'s own gate: a stanza is only ever appended when
// it has at least one field, so parseDeb822 must never return an empty
// deb822Stanza — a caller (extractDeb822SignedBy, doInstallRecommends' apt
// .conf sibling reader in aptconf.go and every other deb822 consumer in
// this package) is entitled to assume every returned stanza has something
// in it.
func FuzzParseDeb822(f *testing.F) {
	if data, err := os.ReadFile(filepath.Join("testdata", "ubuntu2404-deb822", "etc", "apt", "sources.list.d", "ubuntu.sources")); err == nil {
		f.Add(data)
	}
	f.Add([]byte("Key: Value\n\nKey2: Value2\n"))
	f.Add([]byte("Key: Value\n Continuation\n .\n More\n"))
	f.Add([]byte("# comment\nKey: Value\n"))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte(""))
	f.Add([]byte(" leading space with no prior field\nKey: Value\n"))
	f.Add([]byte("NoColonHere\nKey: Value\n"))
	f.Add([]byte(":EmptyKey\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, stz := range parseDeb822(data) {
			if len(stz) == 0 {
				t.Fatalf("parseDeb822 returned an empty stanza for input %q", data)
			}
		}
	})
}
