package engine

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
)

// TestBuild_RedactsCredentialsFromArtifacts is the full-build assertion the
// security fix (F3, docs/security/review-findings.md) calls for: a build
// that touches a credentialed, query-token-bearing vendor URL must never
// let that credential material reach lock.json or evidence.json — the two
// files that ship inside the bundle and cross the air gap.
//
// It runs the real engine pipeline (New(...).Build(...)) against the
// package's own test fakes, but reinstates the real lock.Save (the harness
// normally fakes it to write "{}") so lock.json has real, inspectable
// content, and evidence.json is already real: the harness wires a genuine
// evidence.Collector, and finalizeBundle's write of evidence.json is
// production code, not faked.
func TestBuild_RedactsCredentialsFromArtifacts(t *testing.T) {
	const (
		user   = "vendoruser"
		secret = "s3cr3t-P4ssw0rd"
		token  = "QUERY-TOKEN-ABC123"
	)
	credURL := "https://" + user + ":" + secret + "@vendor.example.invalid/pkg/acme-agent_2.1.0_amd64.deb?token=" + token

	assertClean := func(t *testing.T, dir string) {
		t.Helper()
		for _, name := range []string{lock.FileName, "evidence.json"} {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			content := string(b)
			for _, secretMaterial := range []string{secret, token, user + ":" + secret} {
				if strings.Contains(content, secretMaterial) {
					t.Errorf("%s contains credential material %q:\n%s", name, secretMaterial, content)
				}
			}
		}
	}

	t.Run("successful fetch", func(t *testing.T) {
		h := newHarness(t)
		saveLock = lock.Save // real marshaller: lock.json gets real content to inspect

		fakeDebBytes := []byte("pretend .deb bytes for the redaction test, not a real archive")
		digestHex := digest.Bytes(fakeDebBytes)
		h.store.objects[digestHex] = fakeDebBytes

		h.req.Inputs.URLs = []buildjob.URLInput{{URL: credURL}}
		newFetcher = func(opts fetch.Options) fetch.Fetcher {
			return &fakeFetcher{results: []fetch.Result{{
				Input: buildjob.URLInput{URL: credURL},
				Fetched: &fetch.Fetched{
					// A real fetcher would already have redacted this (see
					// core/fetch's own fix); the fake mirrors that so this
					// test exercises the engine-side redaction
					// independently of core/fetch's.
					URL:          fetch.RedactURL(credURL),
					Filename:     "acme-agent_2.1.0_amd64.deb",
					Digest:       digestHex,
					Size:         int64(len(fakeDebBytes)),
					StorePath:    h.store.Path(digestHex),
					Verification: lock.VerifiedURLUnverified,
				},
			}}}
		}

		eng, err := New(h.deps)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		res, err := eng.Build(context.Background(), h.req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if res == nil {
			t.Fatal("Build returned a nil result")
		}

		assertClean(t, h.outDir)
	})

	t.Run("failed fetch", func(t *testing.T) {
		h := newHarness(t)
		saveLock = lock.Save // real marshaller: lock.json gets real content to inspect

		h.req.Inputs.URLs = []buildjob.URLInput{{URL: credURL}}
		newFetcher = func(opts fetch.Options) fetch.Fetcher {
			return &fakeFetcher{results: []fetch.Result{{
				Input: buildjob.URLInput{URL: credURL},
				Err:   errors.New("connection refused"),
			}}}
		}

		eng, err := New(h.deps)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		res, err := eng.Build(context.Background(), h.req)
		if err != nil {
			t.Fatalf("Build: %v (a fetch failure should still produce a BuildResult, not an error)", err)
		}
		if res.ExitClass != buildjob.ExitIncomplete {
			t.Errorf("ExitClass = %q, want %q", res.ExitClass, buildjob.ExitIncomplete)
		}
		// FetchFailed is operator-facing summary data, not a persisted
		// artefact, and is deliberately left un-redacted : the operator typed this URL themselves and needs to see
		// exactly what failed to retry it. It must still carry the URL.
		if !containsString(res.FetchFailed, credURL) {
			t.Errorf("FetchFailed = %v, want it to contain the original URL", res.FetchFailed)
		}

		assertClean(t, h.outDir)
	})

	// The subtest above returns errors.New("connection refused") — a bare
	// string with no URL in it. That is not the error a failed download
	// actually produces, and the difference is the whole defect: net/http
	// wraps every transport failure in a *url.Error whose Error() prints
	// the request URL verbatim, credentials and query string included, and
	// that string went straight into the "error" attribute of the warn
	// event, hence into evidence.json, hence inside the signed bundle.
	//
	// A fake that cannot produce the shape production produces cannot see
	// the bug. This one produces it.
	t.Run("failed fetch, with the error shape net/http really returns", func(t *testing.T) {
		h := newHarness(t)
		saveLock = lock.Save

		h.req.Inputs.URLs = []buildjob.URLInput{{URL: credURL}}
		transportErr := &url.Error{Op: "Get", URL: credURL, Err: &net.OpError{
			Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused"),
		}}
		newFetcher = func(opts fetch.Options) fetch.Fetcher {
			return &fakeFetcher{results: []fetch.Result{{
				Input: buildjob.URLInput{URL: credURL},
				Err:   transportErr,
			}}}
		}

		eng, err := New(h.deps)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		res, err := eng.Build(context.Background(), h.req)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if res.ExitClass != buildjob.ExitIncomplete {
			t.Errorf("ExitClass = %q, want %q", res.ExitClass, buildjob.ExitIncomplete)
		}

		// The reason must survive the redaction: an operator who cannot
		// read "connection refused" out of the event has gained nothing.
		ev, err := os.ReadFile(filepath.Join(h.outDir, "evidence.json"))
		if err != nil {
			t.Fatalf("reading evidence.json: %v", err)
		}
		if !strings.Contains(string(ev), "connection refused") {
			t.Errorf("evidence.json lost the reason the download failed:\n%s", ev)
		}
		if !strings.Contains(string(ev), "vendor.example.invalid") {
			t.Errorf("evidence.json lost the host, which is what makes the event actionable")
		}

		assertClean(t, h.outDir)
	})
}
