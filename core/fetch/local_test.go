package fetch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/store"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st
}

func TestFromLocalFile_NoDigest_IsURLUnverified(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vendor.deb")
	if err := WriteFixtureDeb(p, FixtureDeb{Package: "vendor", Version: "2.0"}); err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)

	fetched, err := FromLocalFile(context.Background(), st, p)
	if err != nil {
		t.Fatalf("FromLocalFile: %v", err)
	}
	if fetched.Verification != lock.VerifiedURLUnverified {
		t.Errorf("Verification = %q, want %q (a bare local file makes no cryptographic provenance claim)",
			fetched.Verification, lock.VerifiedURLUnverified)
	}
	if fetched.Filename != "vendor.deb" {
		t.Errorf("Filename = %q, want vendor.deb", fetched.Filename)
	}
	if !st.Has(fetched.Digest) {
		t.Errorf("store does not have digest %s after FromLocalFile", fetched.Digest)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("original file was removed or moved: %v", err)
	}
}

func TestFromLocalFileWithDigest_Match(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vendor.deb")
	if err := WriteFixtureDeb(p, FixtureDeb{Package: "vendor", Version: "2.0"}); err != nil {
		t.Fatal(err)
	}
	want, _, err := digest.SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)

	fetched, err := FromLocalFileWithDigest(context.Background(), st, p, want)
	if err != nil {
		t.Fatalf("FromLocalFileWithDigest: %v", err)
	}
	if fetched.Verification != lock.VerifiedUserDigest {
		t.Errorf("Verification = %q, want %q", fetched.Verification, lock.VerifiedUserDigest)
	}
	if fetched.Digest != want {
		t.Errorf("Digest = %q, want %q", fetched.Digest, want)
	}
}

func TestFromLocalFileWithDigest_Mismatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vendor.deb")
	if err := WriteFixtureDeb(p, FixtureDeb{Package: "vendor"}); err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)

	wrong := strings.Repeat("0", 64)
	_, err := FromLocalFileWithDigest(context.Background(), st, p, wrong)
	if err == nil {
		t.Fatal("want an error for a digest mismatch")
	}
	if dferr.ClassOf(err) != dferr.Verification {
		t.Errorf("class = %v, want Verification (exit 4); err=%v", dferr.ClassOf(err), err)
	}
	// The original file must still be there and untouched even on failure.
	if _, statErr := os.Stat(p); statErr != nil {
		t.Errorf("original file missing after a rejected ingest: %v", statErr)
	}
}

func TestFromLocalFileWithDigest_Malformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vendor.deb")
	if err := WriteFixtureDeb(p, FixtureDeb{}); err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	_, err := FromLocalFileWithDigest(context.Background(), st, p, "not-hex")
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
	}
}

func TestFromLocalFile_NotFound(t *testing.T) {
	st := newTestStore(t)
	_, err := FromLocalFile(context.Background(), st, filepath.Join(t.TempDir(), "nope.deb"))
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
	}
}

func TestFromLocalFile_Directory(t *testing.T) {
	st := newTestStore(t)
	_, err := FromLocalFile(context.Background(), st, t.TempDir())
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
	}
}

func TestFromLocalFile_NotADeb(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fake.deb")
	if err := os.WriteFile(p, []byte("this is not a deb file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	_, err := FromLocalFile(context.Background(), st, p)
	if err == nil {
		t.Fatal("want an error")
	}
	if dferr.ClassOf(err) != dferr.Incomplete {
		t.Errorf("class = %v, want Incomplete; err=%v", dferr.ClassOf(err), err)
	}
}

func TestFromLocalFile_NoStore(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vendor.deb")
	if err := WriteFixtureDeb(p, FixtureDeb{}); err != nil {
		t.Fatal(err)
	}
	_, err := FromLocalFile(context.Background(), nil, p)
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage for a nil Store; err=%v", dferr.ClassOf(err), err)
	}
}
