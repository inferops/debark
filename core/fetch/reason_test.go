package fetch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
)

// TestReasonOf drives the real fetcher at real servers and asserts that
// every failure comes back classified.
//
// The three the error catalogue calls out — an unreachable host, a digest
// that does not match and a certificate nothing trusts — are the reason this
// exists: all three exit 3, all three wrote one line to stderr differing
// only in the URL, and all three landed in fetch_failed[] with nothing to
// tell them apart, even though the operator's next step is different for
// each (fix the network, fix the digest, fix this machine's trust store).
func TestReasonOf(t *testing.T) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "acme-agent", Version: "2.1.0"})
	if err != nil {
		t.Fatal(err)
	}

	// A port opened and immediately closed: certainly closed, certainly not
	// somebody else's service.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	if cerr := ln.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	debServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(deb)
	}))
	defer debServer.Close()

	// TLS with a certificate signed by httptest's own throwaway CA. Fetched
	// with the DEFAULT client (no Options.Client), so the system trust store
	// has never heard of it — which is exactly the shape a corporate root
	// that was never installed, or an intercepting proxy, produces.
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(deb)
	}))
	defer tlsServer.Close()

	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such package", http.StatusNotFound)
	}))
	defer notFound.Close()

	// An HTML error page served with status 200 — the classic cause of
	// "the download succeeded and the file is not a package".
	htmlPage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body>Sign in to continue</body></html>"))
	}))
	defer htmlPage.Close()

	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+deadAddr+"/a.deb", http.StatusFound)
	}))
	defer redirector.Close()

	const wrongDigest = "0000000000000000000000000000000000000000000000000000000000000000"

	cases := []struct {
		name   string
		in     buildjob.URLInput
		client *http.Client
		want   buildjob.FetchFailureReason
		// mustSay is a fragment the operator must still be able to read out
		// of the message: a reason code that arrives with an unreadable
		// sentence has traded one problem for another.
		mustSay string
	}{
		{
			name:    "unreachable host",
			in:      buildjob.URLInput{URL: "http://" + deadAddr + "/acme.deb"},
			want:    buildjob.ReasonUnreachable,
			mustSay: "request failed",
		},
		{
			name:    "certificate nothing trusts",
			in:      buildjob.URLInput{URL: tlsServer.URL + "/acme.deb"},
			want:    buildjob.ReasonTLSUntrusted,
			mustSay: "x509",
		},
		{
			name:    "digest that does not match",
			in:      buildjob.URLInput{URL: debServer.URL + "/acme.deb", SHA256: wrongDigest},
			want:    buildjob.ReasonDigestMismatch,
			mustSay: "sha256 mismatch",
		},
		{
			name:    "server says no",
			in:      buildjob.URLInput{URL: notFound.URL + "/acme.deb"},
			want:    buildjob.ReasonHTTPStatus,
			mustSay: "404",
		},
		{
			name:    "200 with something that is not a package",
			in:      buildjob.URLInput{URL: htmlPage.URL + "/acme.deb"},
			want:    buildjob.ReasonNotADeb,
			mustSay: "does not look like a .deb file",
		},
		{
			name:    "https redirected to plaintext",
			in:      buildjob.URLInput{URL: redirector.URL + "/acme.deb"},
			client:  redirector.Client(),
			want:    buildjob.ReasonRefusedRedirect,
			mustSay: "refusing redirect",
		},
		{
			name:    "scheme that is not fetched",
			in:      buildjob.URLInput{URL: "ftp://vendor.example/acme.deb"},
			want:    buildjob.ReasonBadInput,
			mustSay: "unsupported URL scheme",
		},
		{
			name:    "digest that is not a digest",
			in:      buildjob.URLInput{URL: debServer.URL + "/acme.deb", SHA256: "not-a-sha"},
			want:    buildjob.ReasonBadInput,
			mustSay: "malformed sha256",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := New(Options{Client: tc.client, Store: newTestStore(t), Retries: 1})
			_, err := f.Fetch(context.Background(), tc.in)
			if err == nil {
				t.Fatal("Fetch succeeded; the case proves nothing")
			}
			if got := ReasonOf(err); got != tc.want {
				t.Errorf("ReasonOf = %q, want %q\nerror was: %v", got, tc.want, err)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("the message no longer says %q, so the reason code has replaced the explanation rather than adding to it:\n%v", tc.mustSay, err)
			}
		})
	}

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		f := New(Options{Store: newTestStore(t), Retries: 1})
		_, err := f.Fetch(ctx, buildjob.URLInput{URL: debServer.URL + "/acme.deb"})
		if err == nil {
			t.Fatal("Fetch succeeded against a cancelled context")
		}
		if got := ReasonOf(err); got != buildjob.ReasonCancelled {
			t.Errorf("ReasonOf = %q, want %q\nerror was: %v", got, buildjob.ReasonCancelled, err)
		}
	})

	t.Run("nil error has no reason", func(t *testing.T) {
		if got := ReasonOf(nil); got != "" {
			t.Errorf("ReasonOf(nil) = %q, want the empty string", got)
		}
	})

	t.Run("an error from outside this package is other, not a guess", func(t *testing.T) {
		if got := ReasonOf(errStub{}); got != buildjob.ReasonOther {
			t.Errorf("ReasonOf(unknown) = %q, want %q: an unclassifiable error must not be absorbed into a specific reason", got, buildjob.ReasonOther)
		}
	})
}

type errStub struct{}

func (errStub) Error() string { return "something a Fetcher implementation invented" }

// TestFromLocalFileReason covers the other half of the bucket: fetch_failed
// carries local .deb paths too, not only URLs, which is why the field is
// documented in terms of inputs rather than downloads.
func TestFromLocalFileReason(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()

	missing := dir + "/nothing-here.deb"
	_, err := FromLocalFile(context.Background(), st, missing)
	if err == nil {
		t.Fatal("FromLocalFile succeeded on a path that does not exist")
	}
	if got := ReasonOf(err); got != buildjob.ReasonUnreadable {
		t.Errorf("ReasonOf(missing file) = %q, want %q", got, buildjob.ReasonUnreadable)
	}

	if _, err := FromLocalFile(context.Background(), st, dir); err == nil {
		t.Error("FromLocalFile succeeded on a directory")
	} else if got := ReasonOf(err); got != buildjob.ReasonUnreadable {
		t.Errorf("ReasonOf(directory) = %q, want %q", got, buildjob.ReasonUnreadable)
	}
}
