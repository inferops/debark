package store

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

func openTestStore(t *testing.T) Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st
}

func TestOpenCreatesLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "store")
	st, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if st.Root() != root {
		t.Errorf("Root() = %q, want %q", st.Root(), root)
	}
	if _, err := os.Stat(filepath.Join(root, "sha256")); err != nil {
		t.Errorf("sha256 dir not created: %v", err)
	}
}

func TestOpenEmptyDirUsesDefaultRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEBARK_STORE", dir)
	st, err := Open("")
	if err != nil {
		t.Fatalf("Open(\"\"): %v", err)
	}
	if st.Root() != dir {
		t.Errorf("Root() = %q, want %q (from DEBARK_STORE)", st.Root(), dir)
	}
}

func TestPutAndOpen(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	content := []byte("hello, debark")

	digest, size, err := st.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}
	const wantDigest = "a3a8278caf7cfa47cf851c8d2ca2ead5b86a39cefcb984cb68c62d470d13cf0c"
	if digest != wantDigest {
		t.Errorf("digest = %s, want %s", digest, wantDigest)
	}
	if !st.Has(digest) {
		t.Fatalf("Has(%s) = false after Put", digest)
	}

	rc, err := st.Open(digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got := new(bytes.Buffer)
	if _, err := got.ReadFrom(rc); err != nil {
		t.Fatalf("read object: %v", err)
	}
	if !bytes.Equal(got.Bytes(), content) {
		t.Errorf("object content = %q, want %q", got.Bytes(), content)
	}

	wantPath := filepath.Join(st.Root(), "sha256", digest[:2], digest)
	if st.Path(digest) != wantPath {
		t.Errorf("Path = %s, want %s", st.Path(digest), wantPath)
	}
}

func TestPutSameContentTwiceIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	content := []byte("same bytes, twice")

	d1, _, err := st.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	d2, _, err := st.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if d1 != d2 {
		t.Errorf("digests differ: %s vs %s", d1, d2)
	}
}

func TestPutNoPartialObjectVisible(t *testing.T) {
	// Simulate a writer that is interrupted after creating its temp file but
	// before the rename: the object must not exist at its final digest path
	// under any name a reader could stumble on.
	st := openTestStore(t).(*fsStore)
	ctx := context.Background()
	content := []byte("this object must never appear half-written")

	digest, _, err := st.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// After a successful Put, nothing should be left in the staging area.
	entries, err := os.ReadDir(st.tmpDir())
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("staging dir not empty after Put: %v", entries)
	}

	// The final object is exactly the content, never a truncated prefix.
	got, err := os.ReadFile(st.Path(digest))
	if err != nil {
		t.Fatalf("read final object: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("final object corrupted: got %q", got)
	}
}

func TestPutFileMoveOK(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "pkg_1.0_amd64.deb")
	content := []byte("deb file content")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	digest, size, err := st.PutFile(ctx, src, true)
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}
	if !st.Has(digest) {
		t.Fatalf("object missing after PutFile")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source file still exists after moveOK PutFile: err=%v", err)
	}
}

func TestPutFileNoMoveLeavesSource(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "pkg_1.0_amd64.deb")
	content := []byte("deb file content, kept")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	digest, _, err := st.PutFile(ctx, src, false)
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if !st.Has(digest) {
		t.Fatalf("object missing after PutFile")
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source file removed despite moveOK=false: %v", err)
	}
}

func TestPutFileDedupRemovesSourceWhenMoveOK(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	content := []byte("duplicate content")

	dir := t.TempDir()
	first := filepath.Join(dir, "first.deb")
	second := filepath.Join(dir, "second.deb")
	os.WriteFile(first, content, 0o644)
	os.WriteFile(second, content, 0o644)

	d1, _, err := st.PutFile(ctx, first, true)
	if err != nil {
		t.Fatalf("first PutFile: %v", err)
	}
	d2, _, err := st.PutFile(ctx, second, true)
	if err != nil {
		t.Fatalf("second PutFile: %v", err)
	}
	if d1 != d2 {
		t.Errorf("digests differ: %s vs %s", d1, d2)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Errorf("duplicate source not removed on dedup: err=%v", err)
	}
}

func TestMaterialiseHardlinksOnSameVolume(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	content := []byte("materialise me")
	digest, _, err := st.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "pool", "p", "pkg", "pkg_1.0_amd64.deb")
	if err := st.Materialise(digest, dest); err != nil {
		t.Fatalf("Materialise: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read materialised file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("materialised content = %q, want %q", got, content)
	}

	if runtime.GOOS != "windows" {
		srcInfo, err := os.Stat(st.Path(digest))
		if err != nil {
			t.Fatalf("stat source: %v", err)
		}
		destInfo, err := os.Stat(dest)
		if err != nil {
			t.Fatalf("stat dest: %v", err)
		}
		if !os.SameFile(srcInfo, destInfo) {
			t.Errorf("Materialise did not hardlink on same volume (same-file check failed)")
		}
	}

	// Never a symlink, on any platform.
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("Materialise produced a symlink at %s", dest)
	}
}

func TestMaterialiseFallsBackToCopyWhenLinkFails(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	content := []byte("copy fallback content")
	digest, _, err := st.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "materialised.deb")
	// Exercise the copy path directly: this is exactly what Materialise falls
	// back to when os.Link fails (cross-device, unsupported filesystem, a
	// Windows-specific restriction), so this test proves that path produces a
	// correct, complete, non-symlink file without needing to fabricate an
	// os.Link failure, which is not reliably inducible across platforms.
	if err := copyToPath(st.Path(digest), dest); err != nil {
		t.Fatalf("copyToPath: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("copied content = %q, want %q", got, content)
	}
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("copy fallback produced a symlink at %s", dest)
	}

	// Materialise itself must also succeed via its own (possibly hardlink)
	// path and be idempotent when called twice, e.g. across incremental runs.
	if err := st.Materialise(digest, dest); err != nil {
		t.Fatalf("Materialise: %v", err)
	}
	if err := st.Materialise(digest, dest); err != nil {
		t.Fatalf("second Materialise: %v", err)
	}
}

func TestMaterialiseMissingObject(t *testing.T) {
	st := openTestStore(t)
	err := st.Materialise("0000000000000000000000000000000000000000000000000000000000000000", filepath.Join(t.TempDir(), "x"))
	if err == nil {
		t.Fatalf("Materialise of a missing object: want error, got nil")
	}
}

func TestConcurrentPutsOfSameContent(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	content := bytes.Repeat([]byte("concurrent-content-"), 1000)

	const n = 24
	var wg sync.WaitGroup
	digests := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			digests[i], _, errs[i] = st.Put(ctx, bytes.NewReader(content))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Put[%d]: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if digests[i] != digests[0] {
			t.Errorf("digest[%d] = %s, want %s", i, digests[i], digests[0])
		}
	}
	got, err := os.ReadFile(st.Path(digests[0]))
	if err != nil {
		t.Fatalf("read final object: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("final object corrupted after concurrent Puts")
	}
}

func TestConcurrentPutsOfDistinctContent(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	const n = 16
	var wg sync.WaitGroup
	digests := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := bytes.Repeat([]byte{byte(i)}, 4096+i)
			digests[i], _, errs[i] = st.Put(ctx, bytes.NewReader(content))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Put[%d]: %v", i, err)
		}
		if !st.Has(digests[i]) {
			t.Errorf("Has(%s) = false for distinct content %d", digests[i], i)
		}
	}
}

func TestConcurrentRecordSurvivesIndex(t *testing.T) {
	st := openTestStore(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = st.Record(Entry{
				Digest:  digestFor(i),
				Name:    "pkg",
				Version: "1.0",
				Arch:    "amd64",
				Size:    int64(i),
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Record[%d]: %v", i, err)
		}
	}

	idx, err := st.Index()
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(idx.Entries) != n {
		t.Fatalf("index has %d entries, want %d (lost writes under concurrency)", len(idx.Entries), n)
	}
	if idx.SchemaVersion != IndexSchemaVersion {
		t.Errorf("index schema_version = %q, want %q", idx.SchemaVersion, IndexSchemaVersion)
	}
	seen := map[string]bool{}
	for _, e := range idx.Entries {
		if seen[e.Digest] {
			t.Errorf("duplicate entry for digest %s", e.Digest)
		}
		seen[e.Digest] = true
	}
}

func TestRecordUpdatesExistingEntry(t *testing.T) {
	st := openTestStore(t)
	if err := st.Record(Entry{Digest: digestFor(1), Name: "pkg", Version: "1.0", Arch: "amd64"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := st.Record(Entry{Digest: digestFor(1), Name: "pkg", Version: "2.0", Arch: "amd64", UserSupplied: true}); err != nil {
		t.Fatalf("Record (update): %v", err)
	}
	idx, err := st.Index()
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(idx.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1 (update should replace, not append)", len(idx.Entries))
	}
	if idx.Entries[0].Version != "2.0" || !idx.Entries[0].UserSupplied {
		t.Errorf("entry not updated: %+v", idx.Entries[0])
	}
}

func TestGC(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	keepDigest, _, err := st.Put(ctx, bytes.NewReader([]byte("keep me")))
	if err != nil {
		t.Fatalf("Put keep: %v", err)
	}
	dropDigest, _, err := st.Put(ctx, bytes.NewReader([]byte("drop me, a fair bit longer")))
	if err != nil {
		t.Fatalf("Put drop: %v", err)
	}
	if err := st.Record(Entry{Digest: keepDigest, Name: "keep", Version: "1.0", Arch: "amd64"}); err != nil {
		t.Fatalf("Record keep: %v", err)
	}
	if err := st.Record(Entry{Digest: dropDigest, Name: "drop", Version: "1.0", Arch: "amd64"}); err != nil {
		t.Fatalf("Record drop: %v", err)
	}

	stats, err := st.GC(ctx, func(digest string) bool { return digest == keepDigest })
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if stats.Removed != 1 {
		t.Errorf("Removed = %d, want 1", stats.Removed)
	}
	if stats.Kept != 1 {
		t.Errorf("Kept = %d, want 1", stats.Kept)
	}
	if stats.BytesFreed != int64(len("drop me, a fair bit longer")) {
		t.Errorf("BytesFreed = %d, want %d", stats.BytesFreed, len("drop me, a fair bit longer"))
	}
	if stats.BytesRemaining != int64(len("keep me")) {
		t.Errorf("BytesRemaining = %d, want %d", stats.BytesRemaining, len("keep me"))
	}

	if !st.Has(keepDigest) {
		t.Errorf("kept object was removed")
	}
	if st.Has(dropDigest) {
		t.Errorf("dropped object still present")
	}

	idx, err := st.Index()
	if err != nil {
		t.Fatalf("Index after GC: %v", err)
	}
	if len(idx.Entries) != 1 || idx.Entries[0].Digest != keepDigest {
		t.Errorf("index after GC = %+v, want only %s", idx.Entries, keepDigest)
	}
}

func TestGCEmptyStore(t *testing.T) {
	st := openTestStore(t)
	stats, err := st.GC(context.Background(), func(string) bool { return true })
	if err != nil {
		t.Fatalf("GC on empty store: %v", err)
	}
	if stats.Removed != 0 || stats.Kept != 0 {
		t.Errorf("GC on empty store = %+v, want all zero", stats)
	}
}

func TestDefaultRootPrecedence(t *testing.T) {
	t.Setenv("DEBARK_STORE", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("LOCALAPPDATA", "")

	t.Setenv("DEBARK_STORE", filepath.Join("Z", "explicit"))
	if got := DefaultRoot(); got != filepath.Join("Z", "explicit") {
		t.Errorf("DEBARK_STORE not honoured: got %q", got)
	}
	t.Setenv("DEBARK_STORE", "")

	t.Setenv("XDG_DATA_HOME", filepath.Join("Z", "xdg"))
	want := filepath.Join("Z", "xdg", "debark", "store")
	if got := DefaultRoot(); got != want {
		t.Errorf("XDG_DATA_HOME not honoured: got %q, want %q", got, want)
	}
}

// digestFor returns a syntactically valid, distinct 64-hex-char digest for
// index-only tests that never touch the object tree.
func digestFor(i int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 64)
	for j := range b {
		b[j] = hex[0]
	}
	s := []byte(bytes.ToLower([]byte(itoaHex(i))))
	copy(b[64-len(s):], s)
	return string(b)
}

func itoaHex(i int) string {
	if i == 0 {
		return "0"
	}
	const hex = "0123456789abcdef"
	var buf [16]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = hex[i%16]
		i /= 16
	}
	return string(buf[pos:])
}

// ---------------------------------------------------------------------------
// Content addressing is a claim about content, not about file names.
//
// The object tree is ordinary files under <root>/sha256/<hh>/<digest>. Anyone
// who can write one of them can park chosen bytes behind a digest apt vouched
// for, and nothing downstream would notice: the pool file, the Packages index
// and the signed manifest are all re-derived from whatever the store handed
// over. These tests pin the store's side of that - it never hands out, keeps
// or dedups against content it has not checked against its address.
// ---------------------------------------------------------------------------

// poison replaces the object filed under dg with content that does not hash
// to dg, and returns those bytes.
func poison(t *testing.T, st Store, dg string) []byte {
	t.Helper()
	bad := []byte("attacker-authored bytes parked at someone else's address")
	if err := os.WriteFile(st.Path(dg), bad, 0o644); err != nil {
		t.Fatalf("plant poisoned object: %v", err)
	}
	if got, _, err := sha256File(st.Path(dg)); err != nil || got == dg {
		t.Fatalf("poison did not change the object's digest (got %s, err %v)", got, err)
	}
	return bad
}

func TestMaterialiseRefusesAnObjectThatDoesNotMatchItsAddress(t *testing.T) {
	st := openTestStore(t)
	dg, _, err := st.Put(context.Background(), bytes.NewReader([]byte("the genuine package")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	bad := poison(t, st, dg)

	dest := filepath.Join(t.TempDir(), "pool", "p", "pkg", "pkg_1.0_amd64.deb")
	err = st.Materialise(dg, dest)
	if err == nil {
		t.Fatalf("Materialise placed an object whose content does not hash to its address")
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("error class = %v, want Verification: %v", got, err)
	}
	if b, rerr := os.ReadFile(dest); rerr == nil {
		t.Errorf("Materialise left %d bytes at %s after refusing (equal to the planted content: %v)",
			len(b), dest, bytes.Equal(b, bad))
	}
}

func TestMaterialisePlacesGenuineContent(t *testing.T) {
	// Guards the test above against passing because Materialise refuses
	// everything: the identical call on an intact object must still succeed.
	st := openTestStore(t)
	content := []byte("the genuine package")
	dg, _, err := st.Put(context.Background(), bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "pool", "p", "pkg", "pkg_1.0_amd64.deb")
	if err := st.Materialise(dg, dest); err != nil {
		t.Fatalf("Materialise of an intact object: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read materialised file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("materialised content = %q, want %q", got, content)
	}
}

func TestOpenFailsAtEOFOnContentThatDoesNotMatchItsAddress(t *testing.T) {
	st := openTestStore(t)
	dg, _, err := st.Put(context.Background(), bytes.NewReader([]byte("the genuine package")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	poison(t, st, dg)

	rc, err := st.Open(dg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatalf("reading a poisoned object to EOF returned no error")
	} else if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("error class = %v, want Verification: %v", got, err)
	}
}

func TestPutFileMoveKeepsGenuineContentWhenTheAddressIsAlreadyTaken(t *testing.T) {
	// Something was already filed at this address. Believing it meant PutFile
	// deleted the genuine download as a duplicate and left the planted bytes
	// for the next build to hardlink into a pool.
	st := openTestStore(t)
	ctx := context.Background()
	genuine := []byte("the genuine download, freshly fetched")

	seed, _, err := st.Put(ctx, bytes.NewReader(genuine))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	poison(t, st, seed)

	src := filepath.Join(t.TempDir(), "pkg_1.0_amd64.deb")
	if err := os.WriteFile(src, genuine, 0o644); err != nil {
		t.Fatalf("write download: %v", err)
	}
	dg, size, err := st.PutFile(ctx, src, true)
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if dg != seed {
		t.Fatalf("digest = %s, want %s", dg, seed)
	}
	if size != int64(len(genuine)) {
		t.Errorf("size = %d, want %d", size, len(genuine))
	}
	held, err := os.ReadFile(st.Path(dg))
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	if !bytes.Equal(held, genuine) {
		t.Errorf("store holds %q at %s, want the genuine download", held, dg)
	}
}

func TestPutFileCopyKeepsGenuineContentWhenTheAddressIsAlreadyTaken(t *testing.T) {
	// The same on the copying path. With moveOK false the source survives, so
	// this also pins that the store did not dedup against the planted object
	// and skip the ingest entirely.
	st := openTestStore(t)
	ctx := context.Background()
	genuine := []byte("the genuine download, kept on disk")

	seed, _, err := st.Put(ctx, bytes.NewReader(genuine))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	poison(t, st, seed)

	src := filepath.Join(t.TempDir(), "pkg_1.0_amd64.deb")
	if err := os.WriteFile(src, genuine, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	dg, _, err := st.PutFile(ctx, src, false)
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	held, err := os.ReadFile(st.Path(dg))
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	if !bytes.Equal(held, genuine) {
		t.Errorf("store holds %q at %s, want the genuine source content", held, dg)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source removed despite moveOK=false: %v", err)
	}
}

func TestPutFileStoresOnlyBytesItHashed(t *testing.T) {
	// Hashing the source and then re-reading the source to copy it is two
	// reads of a name the caller can still write to. A source that changes in
	// between lands at an address it does not hash to - the wrong-address
	// state a planted object produces, reached with no write into the store at
	// all. A concurrent rewriter makes that window observable.
	st := openTestStore(t)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "racy.deb")
	a := bytes.Repeat([]byte("AAAA"), 20000)
	b := bytes.Repeat([]byte("BBBB"), 20000)
	if err := os.WriteFile(src, a, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; !stop.Load(); i++ {
			if i%2 == 0 {
				os.WriteFile(src, a, 0o644)
			} else {
				os.WriteFile(src, b, 0o644)
			}
		}
	}()
	defer func() {
		stop.Store(true)
		<-done
	}()

	seen := map[string]bool{}
	for i := 0; i < 120; i++ {
		dg, _, err := st.PutFile(ctx, src, false)
		if err != nil {
			continue // a torn read may fail; storing wrong bytes may not
		}
		seen[dg] = true
		onDisk, _, herr := sha256File(st.Path(dg))
		if herr != nil {
			t.Fatalf("hash stored object: %v", herr)
		}
		if onDisk != dg {
			t.Fatalf("PutFile stored content hashing to %s under address %s", onDisk, dg)
		}
	}
	// Without this the test would pass on a machine where the rewriter never
	// interleaved, having asserted nothing about the window it exists to
	// close.
	if len(seen) < 2 {
		t.Fatalf("the concurrent rewrite never interleaved (%d distinct digests ingested); this test asserted nothing", len(seen))
	}
}

// ---------------------------------------------------------------------------
// A store address is a lowercase hex SHA-256 and nothing else. Anything else
// is an unvalidated path fragment joined onto the store root: ".." walks out
// of it, and Materialise would then copy any readable file on the builder into
// a bundle the operator signs.
// ---------------------------------------------------------------------------

func TestStoreRefusesAddressesThatAreNotDigests(t *testing.T) {
	arena := t.TempDir()
	st, err := Open(filepath.Join(arena, "store"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The payload lives inside this test's own temp tree, so a successful
	// escape stays contained and observable rather than aimed at the real
	// filesystem.
	secretDir := filepath.Join(arena, "outside")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("private key material"), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	for _, bad := range []string{
		"../outside/secret.txt",
		"..",
		"",
		"z" + strings.Repeat("0", 63),       // right length, not hex
		strings.Repeat("0", 63),             // one short
		strings.Repeat("0", 65),             // one long
		"sha256/" + strings.Repeat("0", 57), // an embedded separator
		strings.Repeat("0", 30) + "/../../etc/passwd", // separator plus traversal
	} {
		t.Run(bad, func(t *testing.T) {
			if p := st.Path(bad); p != "" {
				t.Errorf("Path(%q) = %q, want the empty string", bad, p)
			}
			if st.Has(bad) {
				t.Errorf("Has(%q) = true", bad)
			}
			if rc, err := st.Open(bad); err == nil {
				rc.Close()
				t.Errorf("Open(%q) succeeded", bad)
			} else if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("Open(%q) class = %v, want Usage", bad, got)
			}
			dest := filepath.Join(arena, "exfil", "leaked.deb")
			if err := st.Materialise(bad, dest); err == nil {
				t.Errorf("Materialise(%q) succeeded", bad)
			} else if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("Materialise(%q) class = %v, want Usage", bad, got)
			}
			if _, err := os.Stat(dest); err == nil {
				t.Errorf("Materialise(%q) wrote %s", bad, dest)
			}
		})
	}

	// The payload is still exactly where it was, and still not reachable
	// through the store.
	if b, err := os.ReadFile(secret); err != nil || string(b) != "private key material" {
		t.Errorf("payload disturbed: %q, %v", b, err)
	}
}

func TestAPoolFileRewrittenInPlaceCannotPoisonTheNextBundle(t *testing.T) {
	// Materialise hardlinks, so a bundle's pool file and the store object are
	// one inode, and an in-place write through the bundle rewrites the object.
	// A bundle directory is far more exposed than the store - staged for
	// removable media, left in CI workspaces, written to shares - so that
	// write is a realistic way in. The store cannot stop it happening; what it
	// must stop is the result reaching the next bundle.
	st := openTestStore(t)
	genuine := []byte("the genuine package")
	dg, _, err := st.Put(context.Background(), bytes.NewReader(genuine))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	first := filepath.Join(t.TempDir(), "pool", "pkg_1.0_amd64.deb")
	if err := st.Materialise(dg, first); err != nil {
		t.Fatalf("Materialise: %v", err)
	}
	f, err := os.OpenFile(first, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open pool file for writing: %v", err)
	}
	if _, err := f.Write([]byte("rewritten through the bundle directory")); err != nil {
		f.Close()
		t.Fatalf("rewrite pool file: %v", err)
	}
	f.Close()

	held, err := os.ReadFile(st.Path(dg))
	if err != nil {
		t.Fatalf("read store object: %v", err)
	}
	if bytes.Equal(held, genuine) {
		// No hardlink was made here (a filesystem that cannot, or a Windows
		// restriction), so the object was never reachable and there is nothing
		// for this test to catch. Say so rather than pass silently.
		t.Skip("Materialise copied rather than hard-linked; the rewrite did not reach the store object")
	}

	second := filepath.Join(t.TempDir(), "pool", "pkg_1.0_amd64.deb")
	err = st.Materialise(dg, second)
	if err == nil {
		t.Fatalf("the rewritten object was materialised into a second bundle")
	}
	if got := dferr.ClassOf(err); got != dferr.Verification {
		t.Errorf("error class = %v, want Verification: %v", got, err)
	}
	if _, serr := os.Stat(second); serr == nil {
		t.Errorf("Materialise left a file at %s after refusing", second)
	}
}
