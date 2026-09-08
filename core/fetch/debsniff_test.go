package fetch

import (
	"bytes"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

func TestSniffDeb_Valid(t *testing.T) {
	b, err := BuildFixtureDeb(FixtureDeb{})
	if err != nil {
		t.Fatal(err)
	}
	if err := SniffDeb(bytes.NewReader(b)); err != nil {
		t.Fatalf("SniffDeb rejected a valid fixture: %v", err)
	}
}

func TestSniffDeb_NotAnArchive(t *testing.T) {
	// The classic failure this exists to catch: a 404 page saved as a .deb.
	err := SniffDeb(strings.NewReader("<html><body>404 Not Found</body></html>"))
	if err == nil {
		t.Fatal("want an error")
	}
	if dferr.ClassOf(err) != dferr.Incomplete {
		t.Errorf("class = %v, want Incomplete", dferr.ClassOf(err))
	}
	if !strings.Contains(err.Error(), "does not look like a .deb") {
		t.Errorf("error %q should say this does not look like a .deb", err.Error())
	}
}

func TestSniffDeb_EmptyInput(t *testing.T) {
	err := SniffDeb(bytes.NewReader(nil))
	if err == nil {
		t.Fatal("want an error for empty input")
	}
}

func TestSniffDeb_EmptyArchive(t *testing.T) {
	err := SniffDeb(strings.NewReader(arMagic))
	if err == nil {
		t.Fatal("want an error for an archive with no members")
	}
}

func TestSniffDeb_WrongFirstMember(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	writeArMember(&buf, "wrong-member", []byte("hello")) // must fit the 16-byte ar name field
	err := SniffDeb(&buf)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "debian-binary") {
		t.Errorf("error %q should name the expected member", err.Error())
	}
}

func TestSniffDeb_UnrecognisedVersion(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	writeArMember(&buf, "debian-binary", []byte("9.9\n"))
	err := SniffDeb(&buf)
	if err == nil {
		t.Fatal("want an error for an unrecognised debian-binary version")
	}
}

func TestSniffDeb_TruncatedHeader(t *testing.T) {
	// Magic present, but fewer than 60 bytes follow: must error, not panic.
	err := SniffDeb(strings.NewReader(arMagic + "short"))
	if err == nil {
		t.Fatal("want an error for a truncated header")
	}
}

func TestSniffDeb_TruncatedAfterMagicOnly(t *testing.T) {
	err := SniffDeb(strings.NewReader("!<arch"))
	if err == nil {
		t.Fatal("want an error for a truncated magic")
	}
}
