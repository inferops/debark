package cliadapter

// Tests for docs/security-review.md §6.5 — a vendor URL containing "="
// misbinds its --digest entry. Prefixed TestDigest..., because several packages
// write tests into this package and a redeclaration breaks the build for all
// of them.
//
// The defect is in a parser this repository does not link: --digest is a pflag
// stringToString registered in the core repository, and github.com/spf13/pflag
// is not a dependency here (rule 6: no new Go dependency without reporting it
// first). So the algorithm is transcribed once, below, from
// pflag@v1.0.10/string_to_string.go — the version debark's own go.mod
// requires — and the transcription is checked against pflag's own documented
// examples so that a test built on it cannot pass because the transcription is
// wrong.
//
// The behaviour was also driven against a real binary. See
// TestDigestMisbindingWasObservedNotDeduced for the transcript.

import (
	"encoding/csv"
	"errors"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
)

const digestTestSHA = "1111111111111111111111111111111111111111111111111111111111111111"

// pflagStringToString is a transcription of pflag's stringToStringValue.Set
// (v1.0.10). It returns the map one --digest occurrence produces, or ok=false
// where pflag would return an error.
//
// Copied rather than imported deliberately: importing pflag to test a defect
// in pflag would add a dependency to this module to describe a parser that
// runs in another process. The transcription is 20 lines and is pinned by
// TestDigestTranscriptionMatchesPflagsOwnExamples.
func pflagStringToString(val string) (map[string]string, bool) {
	var ss []string
	switch n := strings.Count(val, "="); n {
	case 0:
		return nil, false
	case 1:
		ss = append(ss, strings.Trim(val, `"`))
	default:
		r := csv.NewReader(strings.NewReader(val))
		fields, err := r.Read()
		if err != nil {
			return nil, false
		}
		ss = fields
	}

	out := make(map[string]string, len(ss))
	for _, pair := range ss {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			return nil, false
		}
		out[kv[0]] = kv[1]
	}
	return out, true
}

// TestDigestTranscriptionMatchesPflagsOwnExamples checks the transcription
// against the format pflag documents ("a=1,b=2") and the cases its own tests
// cover, so the tests below are not built on a guess.
func TestDigestTranscriptionMatchesPflagsOwnExamples(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
		ok   bool
	}{
		{"a=1", map[string]string{"a": "1"}, true},
		{"a=1,b=2", map[string]string{"a": "1", "b": "2"}, true},
		{"a=1=2", map[string]string{"a": "1=2"}, true},               // csv branch, one field
		{"nope", nil, false},                                         // no "=" at all
		{`"a=1","b=2"`, map[string]string{"a": "1", "b": "2"}, true}, // quoted csv
	}
	for _, tc := range cases {
		got, ok := pflagStringToString(tc.in)
		if ok != tc.ok {
			t.Errorf("%q: ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("%q: got %v, want %v", tc.in, got, tc.want)
			continue
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%q: got[%q] = %q, want %q", tc.in, k, got[k], v)
			}
		}
	}
}

// digestArg returns the value BuildArgv emits after --digest for one URL, or
// "" if it emitted none. It reads the real argv rather than re-deriving it, so
// a change to the joining shows up here.
func digestArg(t *testing.T, u buildjob.URLInput) string {
	t.Helper()
	argv := BuildArgv(BuildSpec{
		BaseID: "ubuntu:24.04/minimal",
		URLs:   []buildjob.URLInput{u},
	})
	for i, a := range argv {
		if a == "--digest" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// TestDigestMisbindsWhenTheURLCarriesAnEqualsSign is the reproduction. It runs
// the argv this package really builds through the parser that really consumes
// it, and shows the digest arriving under a key nothing has.
func TestDigestMisbindsWhenTheURLCarriesAnEqualsSign(t *testing.T) {
	cases := []struct {
		name string
		url  string
		// boundTo is the key pflag ends up using. When it is not the URL, the
		// digest is lost: cmd_build.go's buildInputs reads digests[url] with
		// url the exact positional argument.
		boundTo string
		value   string
		// extra is any further entry the one pair produced. A pair that
		// silently becomes two map entries is a worse answer than a pair that
		// fails to parse, and it needs saying out loud.
		extra  map[string]string
		parses bool
	}{{
		name:    "a plain vendor URL binds to itself",
		url:     "https://vendor.example/pool/agent_2.1.0_amd64.deb",
		boundTo: "https://vendor.example/pool/agent_2.1.0_amd64.deb",
		value:   digestTestSHA,
		parses:  true,
	}, {
		name:    "a comma in the path is harmless",
		url:     "https://vendor.example/pool/agent,2.1.0_amd64.deb",
		boundTo: "https://vendor.example/pool/agent,2.1.0_amd64.deb",
		value:   digestTestSHA,
		parses:  true,
	}, {
		// The defect. An ordinary download URL, not a contrived one.
		name:    "a query string steals the key",
		url:     "https://vendor.example/download?file=agent_2.1.0_amd64.deb",
		boundTo: "https://vendor.example/download?file",
		value:   "agent_2.1.0_amd64.deb=" + digestTestSHA,
		parses:  true,
	}, {
		name:    "a signed-URL query steals it too, and takes the signature with it",
		url:     "https://cdn.example/agent.deb?token=abc&expires=99",
		boundTo: "https://cdn.example/agent.deb?token",
		value:   "abc&expires=99=" + digestTestSHA,
		parses:  true,
	}, {
		// Worse again, and quietly. Two "=" puts pflag on its csv branch, and
		// there a comma is a field separator: the single pair becomes two
		// entries, "…/download?f"="a" and "b"=<the digest>. Neither is the
		// URL, both are junk, and nothing errors.
		name:    "a comma inside a query string splits the pair in two",
		url:     "https://vendor.example/download?f=a,b",
		boundTo: "b",
		value:   digestTestSHA,
		extra:   map[string]string{"https://vendor.example/download?f": "a"},
		parses:  true,
	}, {
		// The only shape of the four that is loud. csv refuses a bare quote in
		// a non-quoted field, so pflag returns an error and cobra refuses the
		// command. Worth a row precisely because it is the exception: three of
		// the four go through silently.
		name:   "a quote inside a query string is the one loud failure",
		url:    `https://vendor.example/download?q="x"`,
		parses: false,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arg := digestArg(t, buildjob.URLInput{URL: tc.url, SHA256: digestTestSHA})
			if arg == "" {
				t.Fatal("BuildArgv emitted no --digest at all")
			}

			bound, ok := pflagStringToString(arg)
			if ok != tc.parses {
				t.Fatalf("pflag parse of %q: ok = %v, want %v", arg, ok, tc.parses)
			}
			if !ok {
				return
			}

			if got := bound[tc.url]; got != digestTestSHA {
				// This is the finding, not a broken test: for the query-string
				// rows the digest is NOT under the URL's own key.
				t.Logf("digest under the URL's own key = %q (want %q)", got, digestTestSHA)
			}
			if bound[tc.boundTo] != tc.value {
				t.Errorf("digest bound to %q = %q, want %q; the whole map is %v",
					tc.boundTo, bound[tc.boundTo], tc.value, bound)
			}
			if tc.boundTo != tc.url && bound[tc.url] != "" {
				t.Errorf("the URL's own key is populated after all: %q", bound[tc.url])
			}
			for k, v := range tc.extra {
				if bound[k] != v {
					t.Errorf("extra entry %q = %q, want %q; the whole map is %v", k, bound[k], v, bound)
				}
			}
			if want := 1 + len(tc.extra); len(bound) != want {
				t.Errorf("one --digest produced %d map entries, want %d: %v", len(bound), want, bound)
			}
		})
	}
}

// TestDigestValidateRefusesAnInexpressiblePair is the fix. A spec whose digest
// cannot survive the flag is refused where the operator can act on it, rather
// than run with the attestation silently dropped.
func TestDigestValidateRefusesAnInexpressiblePair(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		sha    string
		refuse bool
	}{
		{"plain URL with a digest", "https://vendor.example/pool/a.deb", digestTestSHA, false},
		{"comma in the path", "https://vendor.example/pool/a,b.deb", digestTestSHA, false},
		{"percent escape in the path", "https://vendor.example/pool/a%20b.deb", digestTestSHA, false},
		{"fragment", "https://vendor.example/pool/a.deb#x", digestTestSHA, false},
		{"query string, no digest asked for", "https://vendor.example/d?file=a.deb", "", false},
		{"query string with a digest", "https://vendor.example/d?file=a.deb", digestTestSHA, true},
		{"equals anywhere at all", "https://vendor.example/a=b.deb", digestTestSHA, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := BuildSpec{
				BaseID: "ubuntu:24.04/minimal",
				URLs:   []buildjob.URLInput{{URL: tc.url, SHA256: tc.sha}},
			}
			err := spec.Validate()
			if tc.refuse {
				if err == nil {
					t.Fatal("Validate accepted a digest that cannot be passed to debark")
				}
				var ce *Error
				if !errors.As(err, &ce) {
					t.Fatalf("Validate returned %T, want *cliadapter.Error", err)
				}
				if !strings.Contains(ce.Summary(), tc.url) {
					t.Errorf("the message does not name the URL: %q", ce.Summary())
				}
				if ce.Hint() == "" {
					t.Error("no hint: a refusal the operator cannot act on is worse than the defect")
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate refused a legal spec: %v", err)
			}
		})
	}
}

// TestDigestMisbindingWasObservedNotDeduced records the run that turned §6.5
// from reasoning into an observation, so the transcript is not only in a
// document. It asserts nothing about the binary — it cannot, there is none
// here — but it keeps the evidence next to the code it justifies.
//
//	Two builds, differing only in the vendor URL's query string, both given
//	--digest <url>=1111…1111 against a local server returning a real .deb:
//
//	A  url:http://127.0.0.1:9201/agent_1.0_amd64.deb
//	   exit 3, and input.external (warn):
//	     "fetch: …/agent_1.0_amd64.deb: sha256 mismatch: expected 1111…1111,
//	      got bcfb38a837b64a85cd70bfbef1d2129cb0e690b3b518368fb0eb3b75e3202768"
//	   The digest bound, and the wrong file was refused.
//
//	B  url:http://127.0.0.1:9201/download?file=agent_1.0_amd64.deb
//	   the same bytes, the same wrong digest, and input.external (info):
//	     {"filename":"download","publisher_verification":"url-unverified",
//	      "sha256":"bcfb38…2768","size":948}
//	   Accepted. The digest landed on the key ".../download?file" and nothing
//	   read it; the build carried on with the file unattested.
//
// Observed against a debark binary built from the engine as it stood at
// the time, in the debark-shots:go126 image, driven by a throwaway probe
// script.
func TestDigestMisbindingWasObservedNotDeduced(t *testing.T) {
	// The one thing this test can check is that the shape it describes is
	// still the shape this package emits.
	arg := digestArg(t, buildjob.URLInput{
		URL:    "http://127.0.0.1:9201/download?file=agent_1.0_amd64.deb",
		SHA256: digestTestSHA,
	})
	bound, ok := pflagStringToString(arg)
	if !ok {
		t.Fatalf("pflag rejects %q; the transcript above no longer describes this argv", arg)
	}
	if _, hit := bound["http://127.0.0.1:9201/download?file"]; !hit {
		t.Fatalf("the digest no longer lands on %q; the transcript above is stale: %v",
			"http://127.0.0.1:9201/download?file", bound)
	}
}
