package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
)

// TestRedactURL is the table of hostile URLs the security fix (F3,
// docs/security/review-findings.md) asks for: every case must come back
// with no credential material, and every case that had nothing to redact
// must come back byte-identical (RedactURL must never "improve" or reformat
// a URL it had no reason to touch).
func TestRedactURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// want, when non-empty, is the exact expected output. When empty,
		// the test instead checks in == out (nothing should have changed).
		want string
		// mustNotContain lists substrings (credential material) that must
		// never appear in the output, regardless of the exact form chosen.
		mustNotContain []string
		// mustContain lists substrings the redacted form must still carry
		// (scheme/host/path survive redaction).
		mustContain []string
	}{
		{
			name:           "userinfo with password",
			in:             "https://user:hunter2@host.example/pkg.deb",
			mustNotContain: []string{"hunter2", "user:hunter2"},
			mustContain:    []string{"https://", "host.example", "/pkg.deb", "REDACTED"},
		},
		{
			name:           "userinfo, username only",
			in:             "https://svcaccount@host.example/pkg.deb",
			mustNotContain: []string{"svcaccount"},
			mustContain:    []string{"https://", "host.example", "/pkg.deb", "REDACTED"},
		},
		{
			name:           "userinfo with percent-encoded password",
			in:             "https://user:p%40ss@host.example:8443/a/b/pkg.deb",
			mustNotContain: []string{"p%40ss", "p@ss"},
			mustContain:    []string{"host.example:8443", "/a/b/pkg.deb", "REDACTED"},
		},
		{
			name:           "presigned query string (S3-style)",
			in:             "https://bucket.s3.amazonaws.com/pkg.deb?X-Amz-Signature=deadbeefcafe&X-Amz-Credential=AKIAEXAMPLE",
			mustNotContain: []string{"deadbeefcafe", "AKIAEXAMPLE"},
			mustContain:    []string{"https://", "bucket.s3.amazonaws.com", "/pkg.deb", "REDACTED"},
		},
		{
			name:           "bare token query parameter",
			in:             "https://internal-artifactory.example/repo/pkg.deb?token=s3cr3t",
			mustNotContain: []string{"s3cr3t"},
			mustContain:    []string{"/repo/pkg.deb"},
		},
		{
			name:           "both userinfo and query credential",
			in:             "https://deploy:tok123@host.example/pkg.deb?sig=abcXYZ",
			mustNotContain: []string{"tok123", "abcXYZ", "deploy:tok123"},
			mustContain:    []string{"host.example", "/pkg.deb"},
		},
		{
			name: "no credential at all: byte-identical",
			in:   "https://deb.debian.org/debian/pool/main/v/vlc/vlc_3.0.21-1build1_amd64.deb",
			want: "https://deb.debian.org/debian/pool/main/v/vlc/vlc_3.0.21-1build1_amd64.deb",
		},
		{
			name: "http, no credential: byte-identical",
			in:   "http://mirror.internal.example/pkg.deb",
			want: "http://mirror.internal.example/pkg.deb",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "not a URL at all (local path), left untouched",
			in:   `C:\Users\operator\Downloads\pkg.deb`,
			want: `C:\Users\operator\Downloads\pkg.deb`,
		},
		{
			name: "local absolute path, left untouched",
			in:   "/home/operator/local-debs/pkg.deb",
			want: "/home/operator/local-debs/pkg.deb",
		},
		{
			name:           "fragment carrying something sensitive",
			in:             "https://host.example/pkg.deb#token=abc123",
			mustNotContain: []string{"abc123"},
			mustContain:    []string{"host.example", "/pkg.deb"},
		},
		// A scheme containing a DIGIT, in the opaque (no "//") form. This
		// reaches RedactURL's redactUnparsed fallback, because url.Parse
		// succeeds but reports Host == "" for an opaque URL, and there the
		// only thing standing between the credential and the artefact is
		// schemePrefixLen accepting a digit as a legal scheme character
		// (RFC 3986: scheme = ALPHA *( ALPHA / DIGIT / "+" / "-" / "." )).
		// Without the digit class, schemePrefixLen returns 0, the "@" check
		// never runs, and the string is returned verbatim -- a password
		// written straight into evidence.json. "s3:" is not hypothetical:
		// this function's own doc comment is about presigned object-store
		// URLs.
		{
			name:           "opaque URL whose scheme contains a digit",
			in:             "s3:deploy:hunter2@bucket/pkg.deb",
			mustNotContain: []string{"hunter2", "deploy:hunter2"},
			mustContain:    []string{"s3:", "REDACTED"},
		},
		{
			name:           "opaque URL with a digit and a plus in the scheme",
			in:             "git+ssh2:deploy:hunter2@host/pkg.deb",
			mustNotContain: []string{"hunter2"},
			mustContain:    []string{"git+ssh2:", "REDACTED"},
		},
		{
			name: "digit-bearing scheme with no credential is left untouched",
			in:   "s3:bucket/pkg.deb",
			want: "s3:bucket/pkg.deb",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			if tc.want != "" || tc.in == "" {
				if got != tc.want {
					t.Errorf("RedactURL(%q) = %q, want %q (unchanged)", tc.in, got, tc.want)
				}
			}
			for _, s := range tc.mustNotContain {
				if strings.Contains(got, s) {
					t.Errorf("RedactURL(%q) = %q, still contains credential material %q", tc.in, got, s)
				}
			}
			for _, s := range tc.mustContain {
				if !strings.Contains(got, s) {
					t.Errorf("RedactURL(%q) = %q, want it to still contain %q", tc.in, got, s)
				}
			}
		})
	}
}

// TestRedactURL_Idempotent checks that redacting an already-redacted URL is
// a no-op — a caller that redacts defensively at more than one layer (as
// this fix does: core/fetch at the source, core/engine again at the lock/
// evidence boundary) must never end up with double markers or mangled
// output.
func TestRedactURL_Idempotent(t *testing.T) {
	in := "https://user:hunter2@host.example/pkg.deb?token=abc"
	once := RedactURL(in)
	twice := RedactURL(once)
	if once != twice {
		t.Errorf("RedactURL is not idempotent: RedactURL(x)=%q, RedactURL(RedactURL(x))=%q", once, twice)
	}
}

// TestFetch_RedactsCredentialsFromEventsAndResult exercises the real
// fetcher end to end (a real HTTP round trip against an httptest server) and
// checks that every evidence event it emits, and the Fetched.URL it
// returns, are free of the credential and query-string secret embedded in
// the operator's original URL — the actual request must still have used the
// real, credentialed URL (checked via the Authorization header the Go HTTP
// client derives from URL userinfo).
func TestFetch_RedactsCredentialsFromEventsAndResult(t *testing.T) {
	const (
		user   = "vendoruser"
		secret = "hunter2-P4ssw0rd"
		token  = "QUERY-TOKEN-SECRET-XYZ"
	)

	deb, err := BuildFixtureDeb(FixtureDeb{Package: "acme-agent", Version: "2.1.0"})
	if err != nil {
		t.Fatal(err)
	}

	var gotAuthHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		w.Write(deb)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	credURL := "http://" + user + ":" + secret + "@" + host + "/pkg/acme-agent_2.1.0_amd64.deb?token=" + token

	st := newTestStore(t)
	ev := &eventCollector{}
	f := New(Options{Store: st, Events: ev, Retries: 1})

	fetched, err := f.Fetch(context.Background(), buildjob.URLInput{URL: credURL})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// The real request must have carried the real credential (proving this
	// fix does not also break the actual download) — Go's http.Client
	// derives HTTP Basic auth from URL userinfo automatically.
	if gotAuthHeader == "" {
		t.Errorf("server saw no Authorization header; the real request should have used the real credential")
	}

	assertNoSecret := func(where, s string) {
		t.Helper()
		if strings.Contains(s, secret) {
			t.Errorf("%s contains the password: %q", where, s)
		}
		if strings.Contains(s, token) {
			t.Errorf("%s contains the query token: %q", where, s)
		}
		if strings.Contains(s, user+":"+secret) {
			t.Errorf("%s contains the raw userinfo: %q", where, s)
		}
	}

	assertNoSecret("Fetched.URL", fetched.URL)

	for _, e := range ev.all() {
		assertNoSecret("event.msg", e.Msg)
		for k, v := range e.Attrs {
			if s, ok := v.(string); ok {
				assertNoSecret("event.attrs["+k+"]", s)
			}
		}
	}
}

// TestFetch_RedactsCredentialsOnFailure covers the error path: a fetch that
// fails (a 404, here) must still never leak the credential, either through
// the evidence event core/fetch itself emits (the plaintext-HTTP warning)
// or through the returned error's message — which is exactly the string
// core/engine's stageExternals folds into an evidence "error" attribute
// (core/engine/inputs.go), so this is also a regression test for that
// downstream path.
func TestFetch_RedactsCredentialsOnFailure(t *testing.T) {
	const (
		user   = "vendoruser"
		secret = "another-secret-99"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	credURL := "http://" + user + ":" + secret + "@" + host + "/pkg.deb"

	st := newTestStore(t)
	ev := &eventCollector{}
	f := New(Options{Store: st, Events: ev, Retries: 1})

	_, err := f.Fetch(context.Background(), buildjob.URLInput{URL: credURL})
	if err == nil {
		t.Fatal("want an error for a 404")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message contains the credential: %q", err.Error())
	}
	if strings.Contains(err.Error(), user+":"+secret) {
		t.Errorf("error message contains the raw userinfo: %q", err.Error())
	}

	for _, e := range ev.all() {
		if strings.Contains(e.Msg, secret) {
			t.Errorf("event.msg contains the credential: %q", e.Msg)
		}
		for k, v := range e.Attrs {
			if s, ok := v.(string); ok && strings.Contains(s, secret) {
				t.Errorf("event.attrs[%s] contains the credential: %q", k, s)
			}
		}
	}
}
