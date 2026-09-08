package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
)

// eventCollector is a thread-safe evidence.Sink so fetcher tests (which
// download concurrently in FetchAll) can inspect what was emitted.
type eventCollector struct {
	mu     sync.Mutex
	events []evidence.Event
}

func (c *eventCollector) Emit(e evidence.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}
func (c *eventCollector) Close() error { return nil }

func (c *eventCollector) all() []evidence.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]evidence.Event, len(c.events))
	copy(out, c.events)
	return out
}

func (c *eventCollector) hasWarningContaining(substr string) bool {
	for _, e := range c.all() {
		if e.Level == evidence.LevelWarn && strings.Contains(e.Msg, substr) {
			return true
		}
	}
	return false
}

func (c *eventCollector) hasType(typ string) bool {
	for _, e := range c.all() {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func TestFetch_Success(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "vlc", Version: "3.0.21-1build1"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(deb)
	}))
	defer srv.Close()

	st := newTestStore(t)
	ev := &eventCollector{}
	f := New(Options{Client: srv.Client(), Store: st, Events: ev, Retries: 2})

	fetched, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/vlc_3.0.21-1build1_amd64.deb"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fetched.Filename != "vlc_3.0.21-1build1_amd64.deb" {
		t.Errorf("Filename = %q, want vlc_3.0.21-1build1_amd64.deb", fetched.Filename)
	}
	if fetched.Verification != lock.VerifiedURLUnverified {
		t.Errorf("Verification = %q, want %q", fetched.Verification, lock.VerifiedURLUnverified)
	}
	if fetched.Size != int64(len(deb)) {
		t.Errorf("Size = %d, want %d", fetched.Size, len(deb))
	}
	if fetched.Digest != digest.Bytes(deb) {
		t.Errorf("Digest = %s, want %s", fetched.Digest, digest.Bytes(deb))
	}
	if !st.Has(fetched.Digest) {
		t.Errorf("store does not have the fetched digest")
	}
	if ev.hasWarningContaining("plaintext") {
		t.Errorf("unexpected plaintext warning for an https fetch")
	}
	if !ev.hasType(evidence.TypeFetchFile) {
		t.Errorf("no %s event emitted", evidence.TypeFetchFile)
	}
	if !ev.hasType(evidence.TypeProgress) {
		t.Errorf("no %s event emitted", evidence.TypeProgress)
	}
}

func TestFetch_404_NotRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	st := newTestStore(t)
	f := New(Options{Client: srv.Client(), Store: st, Retries: 3})
	_, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/x.deb"})
	if err == nil {
		t.Fatal("want an error")
	}
	if dferr.ClassOf(err) != dferr.Incomplete {
		t.Errorf("class = %v, want Incomplete; err=%v", dferr.ClassOf(err), err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server hit %d times, want 1 (a 404 must not be retried)", got)
	}
}

func TestFetch_500ThenSuccess_Retries(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "retry-me"})
	if err != nil {
		t.Fatal(err)
	}
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write(deb)
	}))
	defer srv.Close()

	st := newTestStore(t)
	f := New(Options{Client: srv.Client(), Store: st, Retries: 3})
	fetched, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/x.deb"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("server hit %d times, want 2 (fail once, then succeed)", got)
	}
	if fetched.Size != int64(len(deb)) {
		t.Errorf("Size = %d, want %d", fetched.Size, len(deb))
	}
}

func TestFetch_PersistentFailure_ExhaustsRetries(t *testing.T) {
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	st := newTestStore(t)
	f := New(Options{Client: srv.Client(), Store: st, Retries: 3})
	_, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/x.deb"})
	if err == nil {
		t.Fatal("want an error")
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("server hit %d times, want exactly Retries=3", got)
	}
}

func TestFetch_DigestMismatch_NoRetry(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(deb)
	}))
	defer srv.Close()

	st := newTestStore(t)
	f := New(Options{Client: srv.Client(), Store: st, Retries: 3})
	wrong := strings.Repeat("a", 64)
	_, err = f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/x.deb", SHA256: wrong})
	if err == nil {
		t.Fatal("want an error")
	}
	if dferr.ClassOf(err) != dferr.Verification {
		t.Errorf("class = %v, want Verification (exit 4); err=%v", dferr.ClassOf(err), err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server hit %d times, want 1 (a digest mismatch must not retry)", got)
	}
}

func TestFetch_DigestMatch_IsUserDigest(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "x"})
	if err != nil {
		t.Fatal(err)
	}
	want := digest.Bytes(deb)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(deb)
	}))
	defer srv.Close()

	st := newTestStore(t)
	f := New(Options{Client: srv.Client(), Store: st, Retries: 1})
	fetched, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/x.deb", SHA256: want})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fetched.Verification != lock.VerifiedUserDigest {
		t.Errorf("Verification = %q, want %q", fetched.Verification, lock.VerifiedUserDigest)
	}
}

func TestFetch_MalformedDigest_RejectedUpfront(t *testing.T) {
	st := newTestStore(t)
	f := New(Options{Store: st})
	_, err := f.Fetch(context.Background(), buildjob.URLInput{URL: "https://host.example/x.deb", SHA256: "not-hex"})
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
	}
}

func TestFetch_HTMLErrorPageAsDeb(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK) // the confusing case: 200 OK but the body is garbage
		w.Write([]byte("<html><body>Not Found</body></html>"))
	}))
	defer srv.Close()

	st := newTestStore(t)
	f := New(Options{Client: srv.Client(), Store: st, Retries: 1})
	_, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/x.deb"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "does not look like a .deb") {
		t.Errorf("error %q should clearly say this is not a real .deb", err.Error())
	}
	if dferr.ClassOf(err) != dferr.Incomplete {
		t.Errorf("class = %v, want Incomplete; err=%v", dferr.ClassOf(err), err)
	}
}

func TestFetch_HostileContentDisposition_FallsBackToURLName(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "x"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		`attachment; filename="../../etc/passwd"`,
		`attachment; filename="/etc/passwd"`,
		`attachment; filename="..\..\windows\system32\evil.deb"`,
		`attachment; filename*=UTF-8''evil%0d%0aSet-Cookie%3a-x`,
	}
	for _, cd := range cases {
		t.Run(cd, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Disposition", cd)
				w.Write(deb)
			}))
			defer srv.Close()

			st := newTestStore(t)
			f := New(Options{Client: srv.Client(), Store: st, Retries: 1})
			fetched, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/safe-name.deb"})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if strings.ContainsAny(fetched.Filename, "/\\") || strings.Contains(fetched.Filename, "..") {
				t.Fatalf("Filename = %q leaked path traversal from a hostile header", fetched.Filename)
			}
			if fetched.Filename != "safe-name.deb" {
				t.Errorf("Filename = %q, want fallback to the URL-derived name safe-name.deb", fetched.Filename)
			}
		})
	}
}

func TestFetch_SafeContentDisposition_IsUsed(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "x"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="renamed-by-server.deb"`)
		w.Write(deb)
	}))
	defer srv.Close()

	st := newTestStore(t)
	f := New(Options{Client: srv.Client(), Store: st, Retries: 1})
	fetched, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/original-name.deb"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fetched.Filename != "renamed-by-server.deb" {
		t.Errorf("Filename = %q, want renamed-by-server.deb (a safe Content-Disposition name)", fetched.Filename)
	}
}

func TestFetch_PlaintextHTTPAllowedWithWarning(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "x"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // plain http
		w.Write(deb)
	}))
	defer srv.Close()

	st := newTestStore(t)
	ev := &eventCollector{}
	f := New(Options{Store: st, Events: ev, Retries: 1})
	fetched, err := f.Fetch(context.Background(), buildjob.URLInput{URL: srv.URL + "/x.deb"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fetched.Verification != lock.VerifiedURLUnverified {
		t.Errorf("Verification = %q", fetched.Verification)
	}
	if !ev.hasWarningContaining("plaintext") {
		t.Errorf("expected a plaintext-HTTP warning; events=%+v", ev.all())
	}
}

func TestFetch_RefusesNonHTTPScheme(t *testing.T) {
	st := newTestStore(t)
	f := New(Options{Store: st})
	_, err := f.Fetch(context.Background(), buildjob.URLInput{URL: "ftp://host.example/x.deb"})
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
	}
}

func TestFetch_RefusesInvalidURL(t *testing.T) {
	st := newTestStore(t)
	f := New(Options{Store: st})
	_, err := f.Fetch(context.Background(), buildjob.URLInput{URL: "://not-a-url"})
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("class = %v, want Usage; err=%v", dferr.ClassOf(err), err)
	}
}

// TestFetch_RefusesSchemeDowngradeRedirect proves an https request whose
// redirect chain would continue over plaintext is refused outright, never
// silently followed.
func TestFetch_RefusesSchemeDowngradeRedirect(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var plainHit int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&plainHit, 1)
		w.Write(deb)
	}))
	defer plain.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x.deb", http.StatusFound)
	}))
	defer secure.Close()

	st := newTestStore(t)
	f := New(Options{Client: secure.Client(), Store: st, Retries: 1})
	_, err = f.Fetch(context.Background(), buildjob.URLInput{URL: secure.URL + "/x.deb"})
	if err == nil {
		t.Fatal("want an error: a redirect from https to http must be refused")
	}
	if !strings.Contains(err.Error(), "downgrade") {
		t.Errorf("error %q should mention the scheme downgrade", err.Error())
	}
	if got := atomic.LoadInt32(&plainHit); got != 0 {
		t.Errorf("the plaintext server was contacted %d times; the redirect should never have been followed", got)
	}
}

// TestFetch_CrossHostRedirectWarns proves a same-scheme redirect to a
// different host is followed (vendor downloads routinely bounce through a
// CDN) but never silently: it must show up as a warning.
func TestFetch_CrossHostRedirectWarns(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "x"})
	if err != nil {
		t.Fatal(err)
	}
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(deb)
	}))
	defer target.Close()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x.deb", http.StatusFound)
	}))
	defer origin.Close()

	// The redirect chain touches two different test servers, each with its
	// own self-signed certificate; trust both.
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	pool.AddCert(target.Certificate())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}

	st := newTestStore(t)
	ev := &eventCollector{}
	f := New(Options{Client: client, Store: st, Events: ev, Retries: 1})
	fetched, err := f.Fetch(context.Background(), buildjob.URLInput{URL: origin.URL + "/x.deb"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fetched == nil {
		t.Fatal("Fetched is nil")
	}
	if !ev.hasWarningContaining("different host") {
		t.Errorf("expected a warning about the cross-host redirect; events=%+v", ev.all())
	}
}

func TestFetch_ContextCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() {
		close(block)
		srv.Close()
	}()

	st := newTestStore(t)
	f := New(Options{Client: srv.Client(), Store: st, Retries: 1, Timeout: time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := f.Fetch(ctx, buildjob.URLInput{URL: srv.URL + "/x.deb"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want an error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Fetch took %s to respect context cancellation", elapsed)
	}
}

func TestFetchAll_OrderPreservedAndPartialFailureDoesNotAbort(t *testing.T) {
	debA, err := BuildFixtureDeb(FixtureDeb{Package: "a"})
	if err != nil {
		t.Fatal(err)
	}
	debC, err := BuildFixtureDeb(FixtureDeb{Package: "c"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.deb":
			w.Write(debA)
		case "/b.deb":
			http.Error(w, "not found", http.StatusNotFound)
		case "/c.deb":
			w.Write(debC)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	st := newTestStore(t)
	f := New(Options{Client: srv.Client(), Store: st, Retries: 1})
	in := []buildjob.URLInput{
		{URL: srv.URL + "/a.deb"},
		{URL: srv.URL + "/b.deb"},
		{URL: srv.URL + "/c.deb"},
	}
	results := f.FetchAll(context.Background(), in)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if results[0].Err != nil || results[0].Fetched == nil {
		t.Errorf("results[0] = %+v", results[0])
	}
	if results[1].Err == nil {
		t.Errorf("results[1] should have failed (404)")
	}
	if results[2].Err != nil || results[2].Fetched == nil {
		t.Errorf("results[2] = %+v", results[2])
	}
	for i, r := range results {
		if r.Input.URL != in[i].URL {
			t.Errorf("results[%d].Input.URL = %q, want %q (order must be preserved)", i, r.Input.URL, in[i].URL)
		}
	}
}

func TestFetchAll_Empty(t *testing.T) {
	st := newTestStore(t)
	f := New(Options{Store: st})
	results := f.FetchAll(context.Background(), nil)
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

// --- pure function unit tests: filename derivation and sanitization ---

func TestSanitizeFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"report.deb", "report.deb"},
		{"", ""},
		{".", ""},
		{"..", ""},
		{"../etc/passwd", ""},
		{"/etc/passwd", ""},
		{`..\windows\evil.deb`, ""},
		{`C:\evil.deb`, ""},
		{"C:evil.deb", ""}, // bare drive-letter prefix, no backslash needed to be dangerous
		{"café.deb", "café.deb"},
		{"evil\r\nSet-Cookie:-x", ""},
		{"evil\x00null.deb", ""},
		{"  spaced.deb  ", "spaced.deb"},
	}
	for _, c := range cases {
		if got := sanitizeFilename(c.in); got != c.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDecodeExtValue exercises the RFC 5987 ext-value decoder directly. In
// the Go version this was written against, mime.ParseMediaType already
// folds a decoded filename* into the plain "filename" key (verified by
// hand:), so safeFilenameFromContentDisposition never
// reaches this function's still-encoded branch through a real header in
// this test binary's Go version — but the function exists precisely so a
// different Go version, where that is not true, is still handled correctly,
// so it gets its own direct test rather than depending on stdlib behaviour
// this package does not control.
func TestDecodeExtValue(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"UTF-8''caf%C3%A9.deb", "café.deb", true},
		{"UTF-8''evil%0d%0aInjected", "evil\r\nInjected", true},
		{"UTF-8'en'plain.deb", "plain.deb", true},
		{"already-decoded-no-quotes.deb", "already-decoded-no-quotes.deb", true},
		{"UTF-8''bad%zz", "", false},
	}
	for _, c := range cases {
		got, ok := decodeExtValue(c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("decodeExtValue(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestSleepBackoff_RespectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepBackoff(ctx, 1) {
		t.Error("sleepBackoff on an already-cancelled context should return false immediately")
	}
}

func TestSafeFilenameFromContentDisposition(t *testing.T) {
	cases := []struct{ header, want string }{
		{`attachment; filename="report.deb"`, "report.deb"},
		{`attachment; filename="../../etc/passwd"`, ""},
		{`attachment; filename="/etc/passwd"`, ""},
		{`attachment; filename*=UTF-8''caf%C3%A9.deb`, "café.deb"},
		{`attachment; filename*=UTF-8''evil%0d%0aSet-Cookie%3a-x`, ""},
		{`attachment; filename="."`, ""},
		{`attachment; filename=".."`, ""},
		{``, ""},
		{`garbled ;;; not even a media type`, ""},
	}
	for _, c := range cases {
		t.Run(c.header, func(t *testing.T) {
			if got := safeFilenameFromContentDisposition(c.header); got != c.want {
				t.Errorf("safeFilenameFromContentDisposition(%q) = %q, want %q", c.header, got, c.want)
			}
		})
	}
}
