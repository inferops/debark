package cliadapter

// Tests for docs/security-review.md §6.1 — the command preview shows
// vendor-URL credentials next to a Copy button. Prefixed TestRedact...,
// because several packages write tests into this package.

import (
	"slices"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
)

const redactTestSHA = "2222222222222222222222222222222222222222222222222222222222222222"

// TestRedactCommandLeaksTheCredentialToday is the reproduction. It is the
// finding, stated as a test: the argv this application builds and shows under a
// "Copy the command" button carries the operator's password verbatim.
//
// It asserts the leak deliberately. Command must keep returning the exact argv
// — the real adapter execs it — so this is not a bug to fix in Command, and a
// test that quietly expected redaction there would be asking for a broken
// build. What the test pins is the reason RedactArgv has to exist.
func TestRedactCommandLeaksTheCredentialToday(t *testing.T) {
	const secret = "s3cr3t-deploy-token"
	spec := BuildSpec{
		BaseID: "ubuntu:24.04/minimal",
		URLs: []buildjob.URLInput{{
			URL:    "https://deploy:" + secret + "@vendor.example/pool/agent_2.1.0_amd64.deb",
			SHA256: redactTestSHA,
		}},
	}

	argv := BuildArgv(spec)
	if !slices.ContainsFunc(argv, func(a string) bool { return strings.Contains(a, secret) }) {
		t.Fatal("BuildArgv no longer carries the credential; §6.1 may already be fixed elsewhere, " +
			"but this test can no longer prove why RedactArgv exists")
	}

	// And the spec is legal, so nothing stops it reaching a preview.
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate refused a credentialed URL: %v — apt-style credentials in a URL are a real "+
			"deployment and debark is the thing that resolves them", err)
	}
}

// TestRedactArgvRemovesEveryCredential is the fix. The secret must not survive
// anywhere in the result, whichever argv position carried it.
func TestRedactArgvRemovesEveryCredential(t *testing.T) {
	const secret = "s3cr3t-deploy-token"
	const presigned = "AKIAsignaturevalue"

	spec := BuildSpec{
		BaseID:   "ubuntu:24.04/minimal",
		Packages: []string{"jq"},
		URLs: []buildjob.URLInput{
			{URL: "https://deploy:" + secret + "@vendor.example/pool/agent.deb", SHA256: redactTestSHA},
			{URL: "https://cdn.example/agent.deb?X-Amz-Signature=" + presigned},
			{URL: "https://plain.example/pool/tool.deb", SHA256: redactTestSHA},
		},
		LocalDebs: []string{"/home/op/vendor/thing.deb"},
		OutPath:   "/home/op/bundles/b1",
	}

	got := RedactArgv(BuildArgv(spec))

	for _, s := range []string{secret, presigned} {
		for i, a := range got {
			if strings.Contains(a, s) {
				t.Errorf("argv[%d] still carries %q: %s", i, s, a)
			}
		}
	}

	// The redaction has to be visible: a URL that silently lost its userinfo
	// reads as a URL that never had one, which is a weaker and wrong claim.
	if !slices.ContainsFunc(got, func(a string) bool { return strings.Contains(a, "REDACTED") }) {
		t.Error("nothing in the redacted argv says a redaction happened")
	}

	// Everything that is not a credential survives, or the preview stops
	// answering the question rule 8 asks it to answer.
	for _, want := range []string{
		"--base", "ubuntu:24.04/minimal",
		"--out", "/home/op/bundles/b1",
		"--json", "--",
		"apt:jq",
		"file:/home/op/vendor/thing.deb",
		"url:https://plain.example/pool/tool.deb",
		"url:https://REDACTED@vendor.example/pool/agent.deb",
		"https://plain.example/pool/tool.deb=" + redactTestSHA,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("the redacted argv lost %q:\n%v", want, got)
		}
	}
}

func TestRedactArgvCases(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{{
		name: "nothing to redact is left alone",
		in:   []string{"debark", "build", "--base", "ubuntu:24.04/minimal", "--", "apt:jq"},
		want: []string{"debark", "build", "--base", "ubuntu:24.04/minimal", "--", "apt:jq"},
	}, {
		name: "userinfo in a positional URL input",
		in:   []string{"--", "url:https://u:pw@vendor.example/a.deb"},
		want: []string{"--", "url:https://REDACTED@vendor.example/a.deb"},
	}, {
		// A password containing "@" must not shorten the region removed:
		// RFC 3986 §3.2 says userinfo runs to the LAST "@" in the authority.
		name: "a password containing an at-sign",
		in:   []string{"--", "url:https://u:p@ss@vendor.example/a.deb"},
		want: []string{"--", "url:https://REDACTED@vendor.example/a.deb"},
	}, {
		// The credential core's own review found riding into a bundle: a
		// presigned URL's token lives in a query parameter whose name is not
		// standardised, so the whole query goes.
		name: "a presigned query string",
		in:   []string{"--", "url:https://cdn.example/a.deb?token=abc&expires=99"},
		want: []string{"--", "url:https://cdn.example/a.deb?REDACTED"},
	}, {
		name: "the URL half of a --digest pair, digest kept",
		in:   []string{"--digest", "https://u:pw@vendor.example/a.deb=" + redactTestSHA},
		want: []string{"--digest", "https://REDACTED@vendor.example/a.deb=" + redactTestSHA},
	}, {
		// Validate refuses this pairing, so it cannot reach a build — but
		// PreviewCommand does not validate, so it can reach a screen.
		name: "a --digest pair whose URL has its own equals sign",
		in:   []string{"--digest", "https://u:pw@v.example/d?f=a.deb=" + redactTestSHA},
		want: []string{"--digest", "https://REDACTED@v.example/d?REDACTED=" + redactTestSHA},
	}, {
		name: "a --digest value that is not a pair at all is redacted whole",
		in:   []string{"--digest", "https://u:pw@vendor.example/a.deb"},
		want: []string{"--digest", "https://REDACTED@vendor.example/a.deb"},
	}, {
		// A URL that survives url.Parse comes back through url.String, which
		// normalises it: the space becomes %20. Worth pinning rather than
		// discovering — the previewed command is the redacted URL as the
		// parser re-emits it, not character-for-character what was typed.
		name: "a space in the path is percent-encoded on the way out",
		in:   []string{"--", "url:https://u:pw@vendor.example/a b.deb"},
		want: []string{"--", "url:https://REDACTED@vendor.example/a%20b.deb"},
	}, {
		// The shapes url.Parse reports Host == "" for, which still carry a
		// real credential. core's RedactURL falls back to a coarser textual
		// scrub rather than returning the string, and that fallback is the
		// behaviour being relied on here.
		name: "a credential url.Parse cannot take apart is still removed",
		in:   []string{"--", "url:https://u:pw@", "--", "url:https:/u:pw@vendor.example/a.deb"},
		want: []string{"--", "url:https://REDACTED@", "--", "url:https:REDACTED"},
	}, {
		// "--digest" as a VALUE, not a flag: the element after it is a URL
		// input, not a digest pair, and must be treated as one.
		name: "only the element after the flag is treated as a pair",
		in:   []string{"--out", "--digest", "--", "url:https://u:pw@v.example/a.deb"},
		want: []string{"--out", "--digest", "--", "url:https://REDACTED@v.example/a.deb"},
	}, {
		name: "a local file path is not a URL",
		in:   []string{"--", "file:/home/op/a=b.deb"},
		want: []string{"--", "file:/home/op/a=b.deb"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactArgv(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("RedactArgv(%v)\n = %v\nwant %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactArgvDoesNotMutateItsInput matters more than it looks: the caller's
// argv is the one that gets executed. A redactor that scrubbed in place would
// turn a display concern into a build that fetches "https://REDACTED@…".
func TestRedactArgvDoesNotMutateItsInput(t *testing.T) {
	in := []string{"--digest", "https://u:pw@v.example/a.deb=" + redactTestSHA, "--", "url:https://u:pw@v.example/a.deb"}
	before := slices.Clone(in)

	got := RedactArgv(in)

	if !slices.Equal(in, before) {
		t.Errorf("RedactArgv modified its argument:\n got %v\nwant %v", in, before)
	}
	if slices.Equal(got, in) {
		t.Error("RedactArgv returned the argument unchanged")
	}
	if len(got) > 0 && &got[0] == &in[0] {
		t.Error("RedactArgv returned the same backing array")
	}
}

func TestRedactArgvNil(t *testing.T) {
	if got := RedactArgv(nil); got != nil {
		t.Errorf("RedactArgv(nil) = %v, want nil", got)
	}
}
