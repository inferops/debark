package fetch

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
)

// TestRedactMessage covers the half of the redaction problem RedactURL
// could not see: a credentialed URL that arrives inside a message this
// package did not format. Every case is a real shape, not an invented one —
// the first three are what net/http, crypto/tls and net actually produce.
func TestRedactMessage(t *testing.T) {
	const (
		secret = "s3cr3t-P4ssw0rd"
		token  = "QUERY-TOKEN-ABC123"
	)
	cred := "https://vendoruser:" + secret + "@vendor.example/pkg/a.deb?token=" + token

	cases := []struct {
		name string
		in   string
		// unchanged asserts the input comes back byte-identical: this
		// function must never "improve" a message it had no reason to
		// touch.
		unchanged      bool
		mustNotContain []string
		mustContain    []string
	}{
		{
			name:           "url.Error from a refused dial",
			in:             `fetch: https://REDACTED@vendor.example/pkg/a.deb?REDACTED: request failed: Get "` + cred + `": dial tcp 10.0.0.1:443: connect: connection refused`,
			mustNotContain: []string{secret, token, "vendoruser:" + secret},
			mustContain:    []string{"connection refused", "vendor.example", "REDACTED"},
		},
		{
			name:           "url.Error from an untrusted certificate",
			in:             `Get "` + cred + `": tls: failed to verify certificate: x509: certificate signed by unknown authority`,
			mustNotContain: []string{secret, token},
			mustContain:    []string{"x509: certificate signed by unknown authority"},
		},
		{
			name:           "two credentialed URLs in one message",
			in:             "refusing redirect from " + cred + " to http://other.example/x?sig=" + token,
			mustNotContain: []string{secret, token},
			mustContain:    []string{"vendor.example", "other.example"},
		},
		{
			name:      "no URL at all",
			in:        "fetch: sha256 mismatch: expected aaaa, got bbbb",
			unchanged: true,
		},
		{
			name:      "URL with nothing to redact",
			in:        `Get "https://deb.debian.org/debian/pool/main/h/hello_2.10-3_amd64.deb": EOF`,
			unchanged: true,
		},
		{
			name:      "a bare scheme separator that is not a URL",
			in:        "parse error at ://",
			unchanged: true,
		},
		{
			// The boundary set deliberately excludes "," and ";" — ending a
			// token early is the one mistake that leaks, because the
			// remainder would then be copied through untouched.
			name:           "comma inside the query string",
			in:             "failed: " + "https://u:" + secret + "@h.example/a.deb?ids=1,2,3&sig=" + token + " after 0 bytes",
			mustNotContain: []string{secret, token, "1,2,3"},
			mustContain:    []string{"h.example", "after 0 bytes"},
		},
		{
			name:           "credential in a message ending at a newline",
			in:             "line one\n" + cred + "\nline three",
			mustNotContain: []string{secret, token},
			mustContain:    []string{"line one", "line three"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactMessage(tc.in)
			if tc.unchanged && got != tc.in {
				t.Errorf("RedactMessage rewrote a message it had no reason to touch:\n in: %s\nout: %s", tc.in, got)
			}
			for _, s := range tc.mustNotContain {
				if strings.Contains(got, s) {
					t.Errorf("RedactMessage leaked %q:\n%s", s, got)
				}
			}
			for _, s := range tc.mustContain {
				if !strings.Contains(got, s) {
					t.Errorf("RedactMessage dropped %q, which the reader needs:\n%s", s, got)
				}
			}
		})
	}
}

// TestFetchErrorCarriesNoCredential drives the real fetcher at a port
// nothing is listening on, with a credentialed, token-bearing URL, and
// asserts the error it returns is safe to print, log and record.
//
// This is the case the existing engine-level test could not reach: its fake
// fetcher returned errors.New("connection refused"), a bare string with no
// URL in it, so it could not see that the real error is a *url.Error whose
// Error() prints the request URL verbatim.
func TestFetchErrorCarriesNoCredential(t *testing.T) {
	const (
		secret = "s3cr3t-P4ssw0rd"
		token  = "QUERY-TOKEN-ABC123"
	)

	// A listener opened and immediately closed gives an address that is
	// certainly closed and certainly not somebody else's service.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()
	if cerr := ln.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}

	credURL := "http://vendoruser:" + secret + "@" + addr + "/pkg/a.deb?token=" + token

	f := New(Options{Store: newTestStore(t), Retries: 1})
	_, err = f.Fetch(context.Background(), buildjob.URLInput{URL: credURL})
	if err == nil {
		t.Fatal("Fetch succeeded against a closed port; the test cannot say anything")
	}

	msg := err.Error()
	for _, s := range []string{secret, token, "vendoruser:" + secret} {
		if strings.Contains(msg, s) {
			t.Errorf("the error a failed fetch returns leaks %q:\n%s", s, msg)
		}
	}
	if !strings.Contains(msg, addr) {
		t.Errorf("the error dropped the host:port, which is what makes it actionable:\n%s", msg)
	}

	// Redaction must not cost the class: dferr.ClassOf looks for the
	// outermost *dferr.Error in the chain, and the wrapper has to let it
	// through or every failed download becomes exit 1.
	if got := dferr.ClassOf(err); got != dferr.Incomplete {
		t.Errorf("dferr.ClassOf = %v, want %v: the redaction wrapper broke the error chain", got, dferr.Incomplete)
	}
	// And errors.As must still reach the cause, for the same reason.
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Errorf("errors.As could not find the *url.Error cause through the redaction wrapper")
	}
}
