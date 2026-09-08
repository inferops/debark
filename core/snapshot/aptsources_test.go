package snapshot

import "testing"

func TestExtractOneLineSignedBy(t *testing.T) {
	data := []byte(`# Tailscale packages for ubuntu noble
deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/ubuntu noble main
deb http://deb.debian.org/debian bookworm main
`)
	refs := extractSignedBy(data)
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1: %+v", len(refs), refs)
	}
	if refs[0].Path != "/usr/share/keyrings/tailscale-archive-keyring.gpg" || refs[0].Armored != "" {
		t.Errorf("got %+v", refs[0])
	}
}

func TestExtractOneLineSignedByCommentedOut(t *testing.T) {
	data := []byte("# deb [signed-by=/should/not/appear.gpg] http://example.invalid stable main\n")
	if refs := extractSignedBy(data); len(refs) != 0 {
		t.Errorf("a commented-out line was parsed: %+v", refs)
	}
}

func TestExtractDeb822SignedByPath(t *testing.T) {
	data := []byte(`Types: deb
URIs: http://deb.debian.org/debian
Suites: bookworm
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
`)
	refs := extractSignedBy(data)
	if len(refs) != 1 || refs[0].Path != "/usr/share/keyrings/debian-archive-keyring.gpg" {
		t.Fatalf("got %+v", refs)
	}
}

func TestExtractDeb822SignedByMultiStanza(t *testing.T) {
	data := []byte(`Types: deb
URIs: http://a
Suites: s
Components: main
Signed-By: /k/a.gpg

Types: deb
URIs: http://b
Suites: s
Components: main
Signed-By: /k/b.gpg
`)
	refs := extractSignedBy(data)
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2: %+v", len(refs), refs)
	}
}

func TestExtractDeb822SignedByInlineArmored(t *testing.T) {
	data := []byte("Types: deb\n" +
		"URIs: http://example.invalid\n" +
		"Suites: stable\n" +
		"Components: main\n" +
		"Signed-By:\n" +
		" -----BEGIN PGP PUBLIC KEY BLOCK-----\n" +
		" .\n" +
		" abcd1234\n" +
		" -----END PGP PUBLIC KEY BLOCK-----\n")
	refs := extractSignedBy(data)
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1: %+v", len(refs), refs)
	}
	if refs[0].Path != "" || refs[0].Armored == "" {
		t.Fatalf("expected an Armored ref with no Path, got %+v", refs[0])
	}
	if got := refs[0].Armored; got[:5] != "-----" {
		t.Errorf("Armored value does not start with the PGP header: %q", got)
	}
}

func TestExtractSignedByRunsBothExtractorsSafely(t *testing.T) {
	// A one-line file must never trip the deb822 extractor into finding a
	// spurious "signed-by" field (its URIs contain "://", which could
	// otherwise confuse a naive colon-based field splitter).
	data := []byte("deb [signed-by=/k.gpg] https://example.invalid/repo noble main\n")
	refs := extractSignedBy(data)
	if len(refs) != 1 || refs[0].Path != "/k.gpg" {
		t.Fatalf("got %+v", refs)
	}
}

func TestExtractSignedByNone(t *testing.T) {
	data := []byte("deb http://deb.debian.org/debian bookworm main\n")
	if refs := extractSignedBy(data); len(refs) != 0 {
		t.Errorf("got %+v, want none", refs)
	}
}
