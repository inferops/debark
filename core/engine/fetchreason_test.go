package engine

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/fetch"
)

// TestBuild_FetchFailuresCarryTheirReason is the end of the road for the gap
// docs/dev/error-catalogue.md 3.3-3.5 records: an unreachable vendor URL, a
// digest that does not match and a certificate nothing trusts produced one
// indistinguishable line in BuildResult, and the only thing that told them
// apart was the English in a warn-level input.external event. A caller
// reading the result document had to say "a download failed" and stop.
//
// The errors here are not hand-written. Each one is produced by the REAL
// core/fetch.Fetcher against a real server (or a port that was opened and
// closed to guarantee it is dead), and only then handed to the engine's fake
// through fetch.Result. A fake that invented its own errors would be
// asserting against itself: it would pass whatever the real classification
// did, including nothing at all. That is the mistake the existing credential
// test made, and it cost a shipped bug.
func TestBuild_FetchFailuresCarryTheirReason(t *testing.T) {
	deb, err := fetch.BuildFixtureDeb(fetch.FixtureDeb{Package: "acme-agent", Version: "2.1.0"})
	if err != nil {
		t.Fatal(err)
	}

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
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(deb)
	}))
	defer tlsServer.Close()

	const wrongDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	unreachableURL := "http://" + deadAddr + "/unreachable.deb"
	untrustedURL := tlsServer.URL + "/untrusted.deb"
	mismatchURL := debServer.URL + "/mismatch.deb"
	missingFile := filepath.Join(t.TempDir(), "handed-to-me.deb")

	h := newHarness(t)

	// Real errors, from the real fetcher, captured once and replayed.
	realFetcher := fetch.New(fetch.Options{Store: h.store, Retries: 1})
	capture := func(in buildjob.URLInput) fetch.Result {
		t.Helper()
		_, ferr := realFetcher.Fetch(context.Background(), in)
		if ferr == nil {
			t.Fatalf("the real fetcher succeeded for %s; the case proves nothing", in.URL)
		}
		return fetch.Result{Input: in, Err: ferr}
	}
	results := []fetch.Result{
		capture(buildjob.URLInput{URL: unreachableURL}),
		capture(buildjob.URLInput{URL: untrustedURL}),
		capture(buildjob.URLInput{URL: mismatchURL, SHA256: wrongDigest}),
	}

	h.req.Inputs.URLs = []buildjob.URLInput{
		{URL: unreachableURL}, {URL: untrustedURL}, {URL: mismatchURL, SHA256: wrongDigest},
	}
	h.req.Inputs.Files = []string{missingFile}
	newFetcher = func(opts fetch.Options) fetch.Fetcher { return &fakeFetcher{results: results} }

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Build(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Build: %v (a fetch failure still produces a BuildResult)", err)
	}
	if res.ExitClass != buildjob.ExitIncomplete {
		t.Errorf("ExitClass = %q, want %q", res.ExitClass, buildjob.ExitIncomplete)
	}

	// The local file is attempted before the URLs, so it comes first.
	want := []buildjob.FetchFailure{
		{Input: missingFile, Reason: buildjob.ReasonUnreadable},
		{Input: unreachableURL, Reason: buildjob.ReasonUnreachable},
		{Input: untrustedURL, Reason: buildjob.ReasonTLSUntrusted},
		{Input: mismatchURL, Reason: buildjob.ReasonDigestMismatch},
	}
	if len(res.FetchFailures) != len(want) {
		t.Fatalf("FetchFailures has %d entries, want %d: %+v", len(res.FetchFailures), len(want), res.FetchFailures)
	}
	for i, w := range want {
		got := res.FetchFailures[i]
		if got.Input != w.Input {
			t.Errorf("FetchFailures[%d].Input = %q, want %q", i, got.Input, w.Input)
		}
		if got.Reason != w.Reason {
			t.Errorf("FetchFailures[%d].Reason = %q, want %q (detail: %s)", i, got.Reason, w.Reason, got.Detail)
		}
		if got.Detail == "" {
			t.Errorf("FetchFailures[%d] has no detail; the reason code is meant to add to the explanation, not replace it", i)
		}
	}

	// The three that used to be indistinguishable must now be three
	// different values. Stated separately from the table above because it
	// is the actual requirement, and a table can satisfy itself while
	// mapping every case onto one reason.
	seen := map[buildjob.FetchFailureReason]bool{}
	for _, f := range res.FetchFailures[1:] {
		if seen[f.Reason] {
			t.Errorf("two URL failures share the reason %q, which is the defect this change exists to fix", f.Reason)
		}
		seen[f.Reason] = true
	}

	// fetch_failed[] must remain exactly the inputs, in the same order: it
	// is what every existing consumer reads, and the two fields are
	// documented as joinable.
	if len(res.FetchFailed) != len(res.FetchFailures) {
		t.Fatalf("FetchFailed has %d entries and FetchFailures has %d; they are documented as one-to-one",
			len(res.FetchFailed), len(res.FetchFailures))
	}
	for i := range res.FetchFailed {
		if res.FetchFailed[i] != res.FetchFailures[i].Input {
			t.Errorf("FetchFailed[%d] = %q but FetchFailures[%d].Input = %q; the two lists have drifted",
				i, res.FetchFailed[i], i, res.FetchFailures[i].Input)
		}
	}

	// The reason has to be readable as well as switchable.
	for _, f := range res.FetchFailures {
		switch f.Reason {
		case buildjob.ReasonTLSUntrusted:
			if !strings.Contains(f.Detail, "x509") {
				t.Errorf("the untrusted-certificate detail no longer names the cause: %s", f.Detail)
			}
		case buildjob.ReasonDigestMismatch:
			if !strings.Contains(f.Detail, "sha256 mismatch") {
				t.Errorf("the digest-mismatch detail no longer names the cause: %s", f.Detail)
			}
		}
	}
}

// TestBuild_FetchFailureDetailIsRedacted pins the one asymmetry in
// FetchFailure: Input is the operator's literal string, credentials and all,
// because they typed it and have to recognise it; Detail can carry a cause
// this process did not compose, so it is redacted.
func TestBuild_FetchFailureDetailIsRedacted(t *testing.T) {
	const (
		secret = "s3cr3t-P4ssw0rd"
		token  = "QUERY-TOKEN-ABC123"
	)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	if cerr := ln.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	credURL := "http://vendoruser:" + secret + "@" + deadAddr + "/pkg/a.deb?token=" + token

	h := newHarness(t)
	realFetcher := fetch.New(fetch.Options{Store: h.store, Retries: 1})
	_, ferr := realFetcher.Fetch(context.Background(), buildjob.URLInput{URL: credURL})
	if ferr == nil {
		t.Fatal("the real fetcher succeeded against a dead port")
	}

	h.req.Inputs.URLs = []buildjob.URLInput{{URL: credURL}}
	newFetcher = func(opts fetch.Options) fetch.Fetcher {
		return &fakeFetcher{results: []fetch.Result{{Input: buildjob.URLInput{URL: credURL}, Err: ferr}}}
	}

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := eng.Build(context.Background(), h.req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.FetchFailures) != 1 {
		t.Fatalf("FetchFailures = %+v, want exactly one", res.FetchFailures)
	}
	f := res.FetchFailures[0]
	if f.Input != credURL {
		t.Errorf("Input = %q, want the operator's literal URL %q", f.Input, credURL)
	}
	for _, s := range []string{secret, token} {
		if strings.Contains(f.Detail, s) {
			t.Errorf("Detail leaks %q:\n%s", s, f.Detail)
		}
	}
	if !strings.Contains(f.Detail, deadAddr) {
		t.Errorf("Detail dropped the host, which is what makes it actionable:\n%s", f.Detail)
	}
}
