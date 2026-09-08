package snapshot

import "testing"

func TestParseDeb822Stanzas(t *testing.T) {
	data := []byte(`Types: deb
# a comment between fields, as real debian.sources files carry
URIs: http://deb.debian.org/debian
Suites: bookworm bookworm-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: http://deb.debian.org/debian-security
Suites: bookworm-security
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
`)
	stanzas := parseDeb822(data)
	if len(stanzas) != 2 {
		t.Fatalf("got %d stanzas, want 2", len(stanzas))
	}
	if v, ok := stanzas[0].Get("Types"); !ok || v != "deb" {
		t.Errorf("stanza 0 Types = %q, %v", v, ok)
	}
	if v, ok := stanzas[0].Get("uris"); !ok || v != "http://deb.debian.org/debian" {
		t.Errorf("stanza 0 URIs (case-insensitive lookup) = %q, %v", v, ok)
	}
	if v, ok := stanzas[1].Get("Suites"); !ok || v != "bookworm-security" {
		t.Errorf("stanza 1 Suites = %q, %v", v, ok)
	}
}

func TestParseDeb822ContinuationAndBlankMarker(t *testing.T) {
	data := []byte("Signed-By:\n" +
		" -----BEGIN PGP PUBLIC KEY BLOCK-----\n" +
		" .\n" +
		" abcd\n" +
		" -----END PGP PUBLIC KEY BLOCK-----\n")
	stanzas := parseDeb822(data)
	if len(stanzas) != 1 {
		t.Fatalf("got %d stanzas, want 1", len(stanzas))
	}
	v, ok := stanzas[0].Get("Signed-By")
	if !ok {
		t.Fatal("Signed-By not found")
	}
	want := "\n-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nabcd\n-----END PGP PUBLIC KEY BLOCK-----"
	if v != want {
		t.Errorf("Signed-By value = %q, want %q", v, want)
	}
}

func TestParseDeb822BlankLineSeparatesStanzas(t *testing.T) {
	data := []byte("A: 1\n\n\nB: 2\n")
	stanzas := parseDeb822(data)
	if len(stanzas) != 2 {
		t.Fatalf("got %d stanzas, want 2 (extra blank lines must not create empty stanzas)", len(stanzas))
	}
}

func TestParseDeb822Empty(t *testing.T) {
	if s := parseDeb822(nil); len(s) != 0 {
		t.Errorf("parseDeb822(nil) = %d stanzas, want 0", len(s))
	}
}
