package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/store"
)

// The regression tests for the security review of this package. Each one
// names the thing it prevents, not the code it calls: the point of the file
// is that a later refactor that reopens one of these holes fails here rather
// than in a bundle that has already crossed the air gap.

// TestRedactURL_CredentialSurvivesNothing is the regression test for the
// leak this review found: RedactURL used to hand back its input unchanged
// whenever url.Parse refused it or reported no host, and every one of those
// inputs can still carry a userinfo credential. The string then reaches
// Fetch's "invalid URL %q" error, which the engine copies into an evidence
// attribute, which ships inside the signed bundle.
func TestRedactURL_CredentialSurvivesNothing(t *testing.T) {
	// Forms url.Parse rejects outright (control character, space, bad
	// percent-escape, invalid port) or parses with an empty Host.
	unparseable := []string{
		"https://user:s3cr3t@host/" + string(rune(0x7f)) + ".deb",
		"https://user:s3cr3t@host/a.deb\n",
		"https://user:s3cr3t@ho st/a.deb",
		"https://user:s3cr3t@host:notaport/a.deb",
		"https://user:s3cr3t@host/a.deb%zz",
		"http://user:s3cr3t@/path",
		"https://user:s3cr3t@",
		// A password containing "@": userinfo runs to the LAST "@", so a
		// naive first-"@" split would leave half the password behind.
		"https://user:p@ss:s3cr3t@ho st/a.deb",
		// The credential in a query string rather than in userinfo.
		"https://host/a.deb?token=s3cr3t&x=%zz#s3cr3t",
		// A scheme with one slash instead of two. url.Parse accepts it and
		// reports no host, so it lands in the same fallback — but it has
		// no "://" for that fallback to find an authority by, which is
		// exactly why someone routing a password past a redactor would
		// choose this shape.
		"https:/user:s3cr3t@host/a.deb",
	}
	for _, raw := range unparseable {
		got := RedactURL(raw)
		if strings.Contains(got, "s3cr3t") {
			t.Errorf("RedactURL(%q) = %q: the credential survived", raw, got)
		}
		if !strings.Contains(got, "REDACTED") {
			t.Errorf("RedactURL(%q) = %q: redaction must be visible, not silent", raw, got)
		}
	}

	// Well-formed URLs keep working exactly as before, including IPv6
	// literals and percent-encoded userinfo.
	parseable := []struct{ in, want string }{
		{"https://user:s3cr3t@host/a.deb", "https://REDACTED@host/a.deb"},
		{"https://us%3Aer:p%40ss@host/a.deb", "https://REDACTED@host/a.deb"},
		{"https://user:s3cr3t@[2001:db8::1]:8443/a.deb", "https://REDACTED@[2001:db8::1]:8443/a.deb"},
		{"https://host/a.deb?sig=s3cr3t", "https://host/a.deb?REDACTED"},
		{"https://host/pool/a.deb", "https://host/pool/a.deb"},
	}
	for _, c := range parseable {
		if got := RedactURL(c.in); got != c.want {
			t.Errorf("RedactURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Fetched.URL carries a local path, not a URL, for a FromLocalFile
	// result (see its doc comment): those must come back untouched. The
	// drive-letter and UNC forms are what keep the scheme fallback above
	// honest — a one-letter "C:" must stay a drive, never a scheme — and
	// the last one is a POSIX path with an "@" in it, which is a directory
	// name and not a credential.
	for _, p := range []string{`C:\vendor\pkg.deb`, "/home/op/local-debs/pkg.deb", "./rel/pkg.deb", `\\server\share\pkg.deb`, "/mnt/op@corp/pkg.deb", ""} {
		if got := RedactURL(p); got != p {
			t.Errorf("RedactURL(%q) = %q, want it unchanged (a local path is not a URL)", p, got)
		}
	}
}

// TestFetch_InvalidURLErrorCarriesNoCredential is the end-to-end half of the
// test above: the leak was reachable, not theoretical, through the one error
// Fetch raises before it has a parsed URL to work with.
func TestFetch_InvalidURLErrorCarriesNoCredential(t *testing.T) {
	f := New(Options{Store: newTestStore(t)})
	_, err := f.Fetch(context.Background(), buildjob.URLInput{URL: "https://user:s3cr3t@ho st/a.deb"})
	if err == nil {
		t.Fatal("want an error for an unparseable URL")
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("the error message carries the password: %v", err)
	}
}

// TestFetch_OversizedResponseIsRefused is the regression test for the
// missing response cap. A chunked response declares no length, so before
// maxDownloadSize existed io.Copy ran until the server stopped or the disk
// did — on the one host that holds the signing key.
//
// maxDownloadSize is lowered here rather than served past: at its real 8 GiB
// this test would have to write 8 GiB.
func TestFetch_OversizedResponseIsRefused(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "x"})
	if err != nil {
		t.Fatal(err)
	}
	restore := maxDownloadSize
	maxDownloadSize = int64(len(deb)) + 1024
	defer func() { maxDownloadSize = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Length: chunked, so the client cannot know how much is
		// coming until it has already taken all of it. The prefix is a real
		// .deb so SniffDeb is not what rejects this.
		w.WriteHeader(http.StatusOK)
		w.Write(deb)
		flusher, _ := w.(http.Flusher)
		junk := make([]byte, 4096)
		for i := 0; i < 64; i++ {
			if _, err := w.Write(junk); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	var hits int32
	countingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, srv.URL+"/x.deb", http.StatusFound)
	}))
	defer countingSrv.Close()

	st := newTestStore(t)
	ev := &eventCollector{}
	f := New(Options{Store: st, Events: ev, Retries: 3})
	got, err := f.Fetch(context.Background(), buildjob.URLInput{URL: countingSrv.URL + "/x.deb"})
	if err == nil {
		t.Fatalf("an over-limit response was accepted: %d bytes stored", got.Size)
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error %q should say which limit was hit", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("server hit %d times, want 1: an over-limit response must not be retried", n)
	}

	// Refusing afterwards is not the property worth having: the whole point
	// is that the flood never reaches the disk in the first place. The
	// progress events carry the running byte count, so they are the record
	// of how much was actually streamed — the server here offers roughly
	// 160x the limit, so a copy that ran to EOF and only then complained
	// would show up plainly.
	var streamed int64
	for _, e := range ev.all() {
		if b, ok := e.Attrs["bytes"].(int64); ok && b > streamed {
			streamed = b
		}
	}
	if streamed > maxDownloadSize+1 {
		t.Errorf("streamed %d bytes with a %d-byte limit: the download must stop at the limit, not be measured after the fact",
			streamed, maxDownloadSize)
	}
}

// TestFetch_LyingContentLengthIsRefusedBeforeTheBody covers the cheap half
// of the same limit. Content-Length is a claim by the server the limit
// exists to distrust, so it is never the enforcement — but when it already
// admits to being over the limit there is no reason to take the body first.
func TestFetch_LyingContentLengthIsRefusedBeforeTheBody(t *testing.T) {
	restore := maxDownloadSize
	maxDownloadSize = 4096
	defer func() { maxDownloadSize = restore }()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// The header declares four times the limit and then no body is
		// sent at all. That is what makes this test about the header
		// check and nothing else: with the header check gone there are no
		// over-limit bytes for the streaming check to catch, so the fetch
		// fails somewhere else, with a different error, or not at all.
		w.Header().Set("Content-Length", fmt.Sprint(maxDownloadSize*4))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := New(Options{Store: newTestStore(t), Retries: 3})
	_, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/x.deb"})
	if err == nil {
		t.Fatal("a response declaring more than the limit was accepted")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error %q should say which limit was hit", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("server hit %d times, want 1: a server that declares an over-limit body will declare it again", n)
	}
}

// TestFetch_RedirectToNonHTTPSchemeIsRefusedByPolicy covers the gap the
// downgrade rule leaves: it only fires for a chain that STARTED at https, so
// an operator who opted into a plaintext http:// download could be sent
// anywhere. Go's default transport happens to refuse an unknown scheme, so
// the assertion here is specifically that the refusal is this package's own
// policy — the error must be a schemeDowngradeError, non-retryable, not the
// transport's generic "unsupported protocol scheme".
func TestFetch_RedirectToNonHTTPSchemeIsRefusedByPolicy(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	defer srv.Close()

	f := New(Options{Store: newTestStore(t), Retries: 3})
	_, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/x.deb"})
	if err == nil {
		t.Fatal("a redirect to file:// was followed")
	}
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("error %q is not this package's redirect refusal", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("server hit %d times, want 1: a refused redirect is not retryable", n)
	}
}

// TestFetch_RedirectChainIsBounded proves a server that redirects forever
// terminates instead of looping. Installing a CheckRedirect of our own
// switches off net/http's built-in ten-redirect stop, so the bound has to be
// ours.
func TestFetch_RedirectChainIsBounded(t *testing.T) {
	var hits int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, srv.URL+r.URL.Path+"x", http.StatusFound)
	}))
	defer srv.Close()

	f := New(Options{Store: newTestStore(t), Retries: 1})
	_, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/a.deb"})
	if err == nil {
		t.Fatal("an endless redirect chain was followed to a result")
	}
	if n := atomic.LoadInt32(&hits); n > 12 {
		t.Errorf("followed %d redirects; the chain is meant to stop at 10", n)
	}
}

// TestSanitizeFilename_HostileWindowsNames covers the names a server can put
// in Content-Disposition that are harmless on Linux and are not on Windows,
// where the builder may equally well be running. Each is checked on every
// platform on purpose: builds are deterministic, so the same header must be
// accepted or refused identically everywhere.
//
// The device cases assert the strict reading — "nul.deb.deb" is refused,
// not accepted — and that is the deliberate decision, not an artefact of
// how the matcher happens to be written. Which names Windows resolves to a
// device varies by Windows build (isReservedDeviceName records what was
// measured), and the pool entry is opened again on a target host whose
// Windows this package never sees, so the only defensible rule is the
// widest one. Getting it wrong in the other direction is silent: the write
// succeeds, the file never exists.
func TestSanitizeFilename_HostileWindowsNames(t *testing.T) {
	cases := []struct {
		in, want, why string
	}{
		{"NUL.deb", "", "Win32 resolves NUL.deb to the null device: the download would be silently discarded"},
		{"nul", "", "device names are matched case-insensitively"},
		{"CON.deb", "", "console device"},
		{"com1.deb", "", "serial port device"},
		{"LPT9.deb", "", "printer port device"},
		{"nul.deb.deb", "", "a second extension does not rescue it: the stem before the FIRST dot is what Win32 matches, and it ignores everything after"},
		{"nul .deb", "", "Win32 discards trailing spaces before it compares the stem, so this is the same device"},
		{"x.deb:evil", "", "everything after a colon on NTFS names an alternate data stream"},
		{"C:evil.deb", "", "bare drive-letter reference"},
		{"vlc.deb.", "", "Win32 strips a trailing dot, so this collides with vlc.deb"},
		{strings.Repeat("a", 256) + ".deb", "", "longer than any filesystem's per-component limit"},
		// Names that merely look alarming but are perfectly legal. The
		// rule is a whole-stem match, not a prefix match, so it must not
		// spread to every filename that happens to start with "nul".
		{"console.deb", "console.deb", "not a device name"},
		{"nulls_1.0_amd64.deb", "nulls_1.0_amd64.deb", "a stem beginning with a device name is not one"},
		{"nul_1.0_amd64.deb", "nul_1.0_amd64.deb", "the stem is \"nul_1\", not \"nul\": an underscore does not end it"},
		{"libfoo1.2.3-4_amd64.deb", "libfoo1.2.3-4_amd64.deb", "an ordinary Debian filename"},
		{strings.Repeat("a", 251) + ".deb", strings.Repeat("a", 251) + ".deb", "exactly at the 255-byte limit"},
	}
	for _, c := range cases {
		if got := sanitizeFilename(c.in); got != c.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q (%s)", c.in, got, c.want, c.why)
		}
	}
}

// TestFetch_DeviceNameContentDispositionFallsBackToURL is the end-to-end
// form: a hostile Content-Disposition never decides the pool filename by
// itself, it only ever gets to be ignored.
func TestFetch_DeviceNameContentDispositionFallsBackToURL(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "x"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="NUL.deb"`)
		w.Write(deb)
	}))
	defer srv.Close()

	f := New(Options{Client: srv.Client(), Store: newTestStore(t), Retries: 1})
	got, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/vlc_1.0_amd64.deb"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Filename != "vlc_1.0_amd64.deb" {
		t.Errorf("Filename = %q, want the URL's own last path element", got.Filename)
	}
}

// TestPoolFilenameForLocal_RefusesUnusableNames is the local-input half of
// the filename story. Until this review the URL path was sanitised and the
// local path was a bare filepath.Base, and that name is not a display
// string: core/engine/inputs.go joins it onto a directory and hands the same
// string to repository.PoolFile.Path, where it becomes the Filename field of
// a deb822 stanza.
func TestPoolFilenameForLocal_RefusesUnusableNames(t *testing.T) {
	bad := []struct{ path, why string }{
		{"/drop/vlc\n Package: evil.deb", "a POSIX filename may hold a newline, which ends a deb822 field"},
		{"/drop/vlc\x1b[2J.deb", "a control character reaching an evidence string"},
		{"/drop/..", "filepath.Base of a path ending in .. is \"..\""},
		{"/drop/NUL.deb", "a Win32 device name"},
		{"/drop/x.deb:stream", "an NTFS alternate data stream"},
		{"/drop/" + strings.Repeat("a", 300) + ".deb", "longer than any filesystem allows"},
	}
	for _, c := range bad {
		got, err := poolFilenameForLocal(c.path)
		if err == nil {
			t.Errorf("poolFilenameForLocal(%q) = %q with no error (%s)", c.path, got, c.why)
			continue
		}
		if dferr.ClassOf(err) != dferr.Usage {
			t.Errorf("poolFilenameForLocal(%q): class = %v, want Usage", c.path, dferr.ClassOf(err))
		}
	}
	for _, c := range []struct{ path, want string }{
		{filepath.Join("drop", "vlc_3.0.21-1build1_amd64.deb"), "vlc_3.0.21-1build1_amd64.deb"},
		{"café.deb", "café.deb"},
	} {
		got, err := poolFilenameForLocal(c.path)
		if err != nil || got != c.want {
			t.Errorf("poolFilenameForLocal(%q) = (%q, %v), want (%q, nil)", c.path, got, err, c.want)
		}
	}
}

// swapStore rewrites the source file just before delegating to the real
// store: the concrete shape of the window between "hash this path" and
// "ingest this path", which are two separate reads of a name the caller does
// not hold open.
type swapStore struct {
	store.Store
	swap []byte
}

func (s *swapStore) PutFile(ctx context.Context, path string, moveOK bool) (string, int64, error) {
	if s.swap != nil {
		if err := os.WriteFile(path, s.swap, 0o600); err != nil {
			return "", 0, err
		}
		s.swap = nil
	}
	return s.Store.PutFile(ctx, path, moveOK)
}

// TestFromLocalFileWithDigest_VerifiesTheBytesTheStoreKept is the regression
// test for the sharpest finding of this review. The digest was checked
// against the operator's path, then PutFile read that same path again to
// ingest it. A file swapped in that window was recorded as
// lock.VerifiedUserDigest — debark's strongest provenance claim — for
// content whose digest was never checked, and the mismatch was invisible
// because nothing compared the store's own digest with the expected one.
func TestFromLocalFileWithDigest_VerifiesTheBytesTheStoreKept(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vendor.deb")
	if err := WriteFixtureDeb(p, FixtureDeb{Package: "vendor", Version: "1.0"}); err != nil {
		t.Fatal(err)
	}
	want, _, err := digest.SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	evil, err := BuildFixtureDeb(FixtureDeb{Package: "evil", Version: "9.9"})
	if err != nil {
		t.Fatal(err)
	}

	st := &swapStore{Store: newTestStore(t), swap: evil}
	got, err := FromLocalFileWithDigest(context.Background(), st, p, want)
	if err == nil {
		t.Fatalf("content swapped during ingestion was accepted as %q with digest %s (expected %s)",
			got.Verification, got.Digest, want)
	}
	if dferr.ClassOf(err) != dferr.Verification {
		t.Errorf("class = %v, want Verification (exit 4); err=%v", dferr.ClassOf(err), err)
	}
}

// TestFetch_VerifiesTheBytesTheStoreKept is the same invariant on the
// download path: what the lock records must be the digest of the bytes the
// store actually holds, not of a scratch file that was hashed and then
// handed on.
func TestFetch_VerifiesTheBytesTheStoreKept(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "x"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(deb)
	}))
	defer srv.Close()

	evil, err := BuildFixtureDeb(FixtureDeb{Package: "evil", Version: "9.9"})
	if err != nil {
		t.Fatal(err)
	}
	st := &swapStore{Store: newTestStore(t), swap: evil}
	f := New(Options{Client: srv.Client(), Store: st, Retries: 1})
	got, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/x.deb", SHA256: digest.Bytes(deb)})
	if err == nil {
		t.Fatalf("content swapped during ingestion was accepted as %q with digest %s", got.Verification, got.Digest)
	}
	if dferr.ClassOf(err) != dferr.Verification {
		t.Errorf("class = %v, want Verification (exit 4); err=%v", dferr.ClassOf(err), err)
	}
}

// TestFromLocalFile_DigestlessIngestionStillWorks guards the fix above from
// over-reaching: a plain FromLocalFile has no expected digest to compare,
// and must go on working.
func TestFromLocalFile_DigestlessIngestionStillWorks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vendor.deb")
	if err := WriteFixtureDeb(p, FixtureDeb{Package: "vendor", Version: "1.0"}); err != nil {
		t.Fatal(err)
	}
	got, err := FromLocalFile(context.Background(), newTestStore(t), p)
	if err != nil {
		t.Fatalf("FromLocalFile: %v", err)
	}
	if got.Verification != lock.VerifiedURLUnverified || got.Filename != "vendor.deb" {
		t.Errorf("got %q / %q, want url-unverified / vendor.deb", got.Verification, got.Filename)
	}
}

// TestParseListFile_RefusesOptionShapedPackageName is this package's half of
// the argv-injection fix. classifyEntry's final arm calls anything it does
// not recognise an apt package name, and that name becomes an operand in
// apt's argv, where a leading "-" makes it an option instead — including the
// options that switch off the signature checking that makes apt a
// trustworthy oracle. The resolver's own argv construction is hardened
// separately; both halves stand alone.
func TestParseListFile_RefusesOptionShapedPackageName(t *testing.T) {
	refused := []string{
		"-o",
		"--allow-unauthenticated",
		"apt:--allow-unauthenticated",
		"apt:-o",
		"-",
		"vlc\x1b[2J",
	}
	for _, entry := range refused {
		dir := t.TempDir()
		p := filepath.Join(dir, "packages.txt")
		if err := os.WriteFile(p, []byte(entry+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		in, err := ParseListFile(p)
		if err == nil {
			t.Errorf("ParseListFile accepted %q as packages %q", entry, in.Packages)
			continue
		}
		if dferr.ClassOf(err) != dferr.Usage {
			t.Errorf("%q: class = %v, want Usage; err=%v", entry, dferr.ClassOf(err), err)
		}
	}

	// The documented syntax keeps working: an option-shaped string is the
	// only thing newly refused.
	accepted := "vlc\nvlc=3.0.21-1build1\napt:libc6\nlibfoo:amd64\nbar/noble\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "packages.txt")
	if err := os.WriteFile(p, []byte(accepted), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := ParseListFile(p)
	if err != nil {
		t.Fatalf("ParseListFile refused ordinary package lines: %v", err)
	}
	if len(in.Packages) != 5 {
		t.Errorf("got %d packages %q, want 5", len(in.Packages), in.Packages)
	}
}
