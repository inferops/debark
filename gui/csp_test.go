package main

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hashOf is the CSP source expression for a piece of inline script text, so a
// test can state its expectation as the script rather than as a base64 blob.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

func TestInlineScriptHashes(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "no scripts at all",
			doc:  "<!doctype html><html><head><title>x</title></head><body></body></html>",
			want: nil,
		},
		{
			name: "a src script is not inline and is not hashed",
			doc:  `<script type="module" src="./src/main.js"></script>`,
			want: nil,
		},
		{
			name: "an inline script is hashed over its exact child text",
			doc:  "<head><script>\n  var a = 1;\n</script></head>",
			want: []string{hashOf("\n  var a = 1;\n")},
		},
		{
			name: "both, in document order",
			doc:  `<script>A</script><script src="x.js"></script><script>B</script>`,
			want: []string{hashOf("A"), hashOf("B")},
		},
		{
			// The reason tagHasSrc walks attributes instead of searching for
			// the word: a substring match would call this one external and
			// leave the real inline script unhashed, which silently blocks it.
			name: "data-src is not src",
			doc:  `<script data-src="x.js">C</script>`,
			want: []string{hashOf("C")},
		},
		{
			name: "srcset-like attribute names are not src either",
			doc:  `<script srcdoc=x>D</script>`,
			want: []string{hashOf("D")},
		},
		{
			name: "an unquoted src value is still a src",
			doc:  `<script src=x.js></script>`,
			want: nil,
		},
		{
			name: "single-quoted src",
			doc:  `<script src='x.js'></script>`,
			want: nil,
		},
		{
			name: "attribute order does not matter",
			doc:  `<script defer type="module" src="x.js"></script>`,
			want: nil,
		},
		{
			name: "tag matching is case insensitive",
			doc:  "<SCRIPT>E</SCRIPT>",
			want: []string{hashOf("E")},
		},
		{
			name: "SRC is case insensitive too",
			doc:  `<SCRIPT SRC="x.js"></SCRIPT>`,
			want: nil,
		},
		{
			name: "an element whose name merely starts with script is not one",
			doc:  `<scriptish>F</scriptish>`,
			want: nil,
		},
		{
			name: "an empty inline script hashes the empty string",
			doc:  `<script></script>`,
			want: []string{hashOf("")},
		},
		{
			// Raw-text content: the "</div>" inside is script text, not markup,
			// and it must be inside the hash or the hash will not match what
			// the engine computes.
			name: "markup-looking text inside a script is part of the hash",
			doc:  `<script>var s = "</div>";</script>`,
			want: []string{hashOf(`var s = "</div>";`)},
		},
		{
			name: "an unterminated script contributes nothing",
			doc:  `<script>oops`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inlineScriptHashes([]byte(tc.doc))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d hashes %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("hash %d = %s, want %s", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestContentSecurityPolicyClauses pins the policy. It is the record of what
// shipped; docs/security-review.md §6.7 carries the argument for each line.
func TestContentSecurityPolicyClauses(t *testing.T) {
	policy := contentSecurityPolicy([]string{"'sha256-AAAA'"})

	want := map[string]string{
		"default-src":     "'none'",
		"script-src":      "'self' 'sha256-AAAA'",
		"style-src":       "'self' 'unsafe-inline'",
		"img-src":         "'self' data:",
		"font-src":        "'self'",
		"connect-src":     "'none'",
		"form-action":     "'none'",
		"frame-ancestors": "'none'",
		"base-uri":        "'none'",
		"object-src":      "'none'",
	}

	got := map[string]string{}
	for _, clause := range strings.Split(policy, ";") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		name, value, _ := strings.Cut(clause, " ")
		if _, dup := got[name]; dup {
			t.Errorf("directive %q appears twice; the second is ignored by the engine", name)
		}
		got[name] = value
	}

	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %q, want %q", name, got[name], value)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected directive %q in the policy", name)
		}
	}
}

// TestContentSecurityPolicyKeepsTheRulesTheProjectCannotRelax covers the three
// ways a plausible edit to this policy would break a non-negotiable rule.
func TestContentSecurityPolicyKeepsTheRulesTheProjectCannotRelax(t *testing.T) {
	policy := contentSecurityPolicy(inlineScriptHashes([]byte(`<script>x</script>`)))

	// Rule 4: a violation report is a network call, and there is no telemetry
	// of any kind in this application.
	for _, banned := range []string{"report-uri", "report-to", "report-sample"} {
		if strings.Contains(policy, banned) {
			t.Errorf("policy contains %q; a violation report is telemetry (rule 4)", banned)
		}
	}

	// Rule 3's frontend half is the reason this header is worth having.
	if !strings.Contains(policy, "connect-src 'none'") {
		t.Error("connect-src 'none' is missing; the policy no longer enforces rule 3 in the runtime")
	}

	// script-src must never gain either of these: 'unsafe-inline' gives back
	// everything the hash was for, and 'unsafe-eval' re-opens a sink the tree
	// does not otherwise contain.
	scriptSrc := ""
	for _, clause := range strings.Split(policy, ";") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(clause), "script-src "); ok {
			scriptSrc = s
		}
	}
	if scriptSrc == "" {
		t.Fatal("no script-src directive")
	}
	for _, banned := range []string{"'unsafe-inline'", "'unsafe-eval'", "'unsafe-hashes'", "*"} {
		if strings.Contains(scriptSrc, banned) {
			t.Errorf("script-src contains %q: %s", banned, scriptSrc)
		}
	}
}

// TestIndexHTMLHasExactlyOneInlineScript is the review-forcing half of §6.7.
//
// The hash itself is derived at startup, so a reworded theme bootstrap cannot
// silently disable the theme. What must not happen without someone looking is
// a SECOND inline script appearing in the document — that is the change that
// widens script-src — and this is the test that fails when it does.
func TestIndexHTMLHasExactlyOneInlineScript(t *testing.T) {
	doc, err := fs.ReadFile(assets, cspIndexPath)
	if err != nil {
		t.Fatalf("read %s: %v (run `make frontend`)", cspIndexPath, err)
	}

	scripts := inlineScripts(doc)
	if len(scripts) != 1 {
		t.Fatalf("%s carries %d inline scripts, want exactly 1.\n"+
			"Every inline script is allowed by hash in the Content-Security-Policy. "+
			"If a new one is genuinely needed, read it, then update this test and "+
			"docs/security-review.md §6.7.", cspIndexPath, len(scripts))
	}

	body := string(scripts[0])
	for _, marker := range []string{"localStorage", "debark.theme", "data-theme"} {
		if !strings.Contains(body, marker) {
			t.Errorf("the one inline script does not mention %q; it is no longer the theme bootstrap, "+
				"so re-read it before the policy keeps hashing it:\n%s", marker, body)
		}
	}
}

// TestCSPMiddlewareSetsTheHeader checks the seam, not the policy: every
// response that leaves the asset handler carries the header, whatever it is.
func TestCSPMiddlewareSetsTheHeader(t *testing.T) {
	const policy = "default-src 'none'"

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("hello"))
	})
	h := cspMiddleware(policy)(inner)

	for _, path := range []string{"/", "/index.html", "/src/main.js", "/missing"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rr.Header().Get(cspHeader); got != policy {
			t.Errorf("%s: %s = %q, want %q", path, cspHeader, got, policy)
		}
	}
}

// TestAssetContentSecurityPolicyUsesTheRealBundle ties the two halves
// together: the policy the app ships is built from the index.html the app
// embeds, and it names that document's inline script.
func TestAssetContentSecurityPolicyUsesTheRealBundle(t *testing.T) {
	doc, err := fs.ReadFile(assets, cspIndexPath)
	if err != nil {
		t.Fatalf("read %s: %v (run `make frontend`)", cspIndexPath, err)
	}
	scripts := inlineScripts(doc)
	if len(scripts) == 0 {
		t.Fatal("no inline script in the embedded index.html")
	}

	policy := assetContentSecurityPolicy(assets)
	want := hashOf(string(scripts[0]))
	if !strings.Contains(policy, want) {
		t.Errorf("the shipped policy does not carry the theme bootstrap's hash %s:\n%s", want, policy)
	}
}
