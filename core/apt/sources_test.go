package apt

import (
	"strings"
	"testing"
)

func TestRewriteSignedBy_Deb822_Rewrites(t *testing.T) {
	in := "Types: deb\nURIs: http://deb.debian.org/debian\nSuites: bookworm\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n"
	dest := map[string]string{
		"/usr/share/keyrings/debian-archive-keyring.gpg": "/priv/root/etc/apt/trusted.gpg.d/debian-archive-keyring.gpg",
	}
	out, records := rewriteSignedBy("etc/apt/sources.list.d/debian.sources", []byte(in), dest)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %+v", records)
	}
	r := records[0]
	if r.Stripped || r.Inline {
		t.Fatalf("expected a clean rewrite, got %+v", r)
	}
	if r.RewrittenTo != dest["/usr/share/keyrings/debian-archive-keyring.gpg"] {
		t.Errorf("RewrittenTo = %q", r.RewrittenTo)
	}
	got := string(out)
	if contains := "Signed-By: " + dest["/usr/share/keyrings/debian-archive-keyring.gpg"]; !strings.Contains(got, contains) {
		t.Errorf("output missing rewritten Signed-By line:\n%s", got)
	}
	if strings.Contains(got, "/usr/share/keyrings/debian-archive-keyring.gpg") {
		t.Errorf("original path leaked into output:\n%s", got)
	}
	if !strings.Contains(got, "Suites: bookworm") {
		t.Errorf("unrelated field lost:\n%s", got)
	}
}

func TestRewriteSignedBy_Deb822_StripsWhenKeyringUnknown(t *testing.T) {
	in := "Types: deb\nURIs: https://example.invalid/repo\nSuites: stable\nComponents: main\nSigned-By: /etc/apt/keyrings/unknown-vendor.gpg\n"
	out, records := rewriteSignedBy("etc/apt/sources.list.d/vendor.sources", []byte(in), map[string]string{})
	if len(records) != 1 || !records[0].Stripped {
		t.Fatalf("expected a stripped record, got %+v", records)
	}
	got := string(out)
	if strings.Contains(got, "Signed-By") {
		t.Errorf("Signed-By field should have been removed entirely:\n%s", got)
	}
	if !strings.Contains(got, "Suites: stable") {
		t.Errorf("unrelated field lost:\n%s", got)
	}
}

func TestRewriteSignedBy_Deb822_LeavesInlineArmourUntouched(t *testing.T) {
	in := "Types: deb\nURIs: https://example.invalid/repo\nSuites: stable\nComponents: main\nSigned-By:\n -----BEGIN PGP PUBLIC KEY BLOCK-----\n .\n mQINBF...fakekeydata...\n -----END PGP PUBLIC KEY BLOCK-----\n"
	out, records := rewriteSignedBy("etc/apt/sources.list.d/inline.sources", []byte(in), map[string]string{})
	if len(records) != 1 || !records[0].Inline {
		t.Fatalf("expected an inline record, got %+v", records)
	}
	if string(out) != in {
		t.Errorf("inline-armoured Signed-By must be left byte-identical:\ngot:  %q\nwant: %q", out, in)
	}
}

func TestRewriteSignedBy_Deb822_MultiStanza_OnlyMatchingOneRewritten(t *testing.T) {
	in := "Types: deb\nURIs: http://a.example/debian\nSuites: stable\nComponents: main\nSigned-By: /keys/a.gpg\n\n" +
		"Types: deb\nURIs: http://b.example/debian\nSuites: stable\nComponents: main\nSigned-By: /keys/b.gpg\n"
	dest := map[string]string{"/keys/a.gpg": "/root/trusted.gpg.d/a.gpg"}
	out, records := rewriteSignedBy("etc/apt/sources.list.d/two.sources", []byte(in), dest)
	if len(records) != 2 {
		t.Fatalf("expected 2 records (one per stanza), got %+v", records)
	}
	if records[0].RewrittenTo == "" || records[0].Stripped {
		t.Errorf("stanza 0 (known keyring) should have been rewritten: %+v", records[0])
	}
	if !records[1].Stripped {
		t.Errorf("stanza 1 (unknown keyring) should have been stripped: %+v", records[1])
	}
	got := string(out)
	if !strings.Contains(got, "/root/trusted.gpg.d/a.gpg") {
		t.Errorf("stanza 0 rewrite missing:\n%s", got)
	}
	if strings.Contains(got, "/keys/b.gpg") {
		t.Errorf("stanza 1's original path leaked:\n%s", got)
	}
	if !strings.Contains(got, "http://a.example/debian") || !strings.Contains(got, "http://b.example/debian") {
		t.Errorf("a URI line was lost:\n%s", got)
	}
}

func TestRewriteSignedBy_OneLine_Rewrites(t *testing.T) {
	in := "deb [arch=amd64 signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/ubuntu noble main\n"
	dest := map[string]string{
		"/usr/share/keyrings/tailscale-archive-keyring.gpg": "/root/trusted.gpg.d/tailscale-archive-keyring.gpg",
	}
	out, records := rewriteSignedBy("etc/apt/sources.list.d/tailscale.list", []byte(in), dest)
	if len(records) != 1 || records[0].Stripped {
		t.Fatalf("expected a rewrite, got %+v", records)
	}
	got := string(out)
	if !strings.Contains(got, "signed-by=/root/trusted.gpg.d/tailscale-archive-keyring.gpg") {
		t.Errorf("signed-by not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "arch=amd64") {
		t.Errorf("sibling option arch=amd64 was lost:\n%s", got)
	}
	if !strings.Contains(got, "https://pkgs.tailscale.com/stable/ubuntu noble main") {
		t.Errorf("URI/suite/component tail was lost:\n%s", got)
	}
}

func TestRewriteSignedBy_OneLine_StripsUnknownKeepsOtherOptions(t *testing.T) {
	in := "deb [arch=amd64 signed-by=/etc/apt/keyrings/unknown.gpg trusted=yes] https://example.invalid/repo stable main\n"
	out, records := rewriteSignedBy("etc/apt/sources.list.d/vendor.list", []byte(in), map[string]string{})
	if len(records) != 1 || !records[0].Stripped {
		t.Fatalf("expected a stripped record, got %+v", records)
	}
	got := string(out)
	if strings.Contains(got, "signed-by") {
		t.Errorf("signed-by option should have been removed:\n%s", got)
	}
	if !strings.Contains(got, "arch=amd64") || !strings.Contains(got, "trusted=yes") {
		t.Errorf("sibling options were lost:\n%s", got)
	}
}

func TestRewriteSignedBy_OneLine_NoSignedByIsUntouched(t *testing.T) {
	in := "deb http://archive.ubuntu.com/ubuntu noble main restricted\n# a comment\n\ndeb-src http://archive.ubuntu.com/ubuntu noble main\n"
	out, records := rewriteSignedBy("sources.list", []byte(in), map[string]string{})
	if len(records) != 0 {
		t.Fatalf("expected no records for lines without signed-by, got %+v", records)
	}
	if string(out) != in {
		t.Errorf("output changed with nothing to rewrite:\ngot:  %q\nwant: %q", out, in)
	}
}

func TestIsDeb822SourceFile(t *testing.T) {
	cases := map[string]bool{
		"etc/apt/sources.list.d/debian.sources": true,
		"etc/apt/sources.list.d/tailscale.list": false,
		"etc/apt/sources.list":                  false,
		"DEBIAN.SOURCES":                        true,
	}
	for name, want := range cases {
		if got := isDeb822SourceFile(name); got != want {
			t.Errorf("isDeb822SourceFile(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestRewriteSignedBy_OneLine_MultiKeyringStaysPinned is the multi-keyring
// de-pinning defect. Reproduced before the fix by printing the rewrite:
//
//	IN : deb [signed-by=/k.gpg,/k2.gpg] http://a/ b c
//	OUT: deb http://a/ b c                    <- the pin was removed
//
// A comma-separated signed-by is documented apt syntax on an entirely
// ordinary target, so nothing attacker-controlled is needed to reach it. The
// source silently stopped being pinned to its own keys and became verifiable
// by anything in the private root's combined trusted.gpg.d.
func TestRewriteSignedBy_OneLine_MultiKeyringStaysPinned(t *testing.T) {
	in := "deb [arch=amd64 signed-by=/keys/a.gpg,/keys/b.gpg] https://vendor.example/repo stable main\n"
	dest := map[string]string{
		"/keys/a.gpg": "/root/trusted.gpg.d/a.gpg",
		"/keys/b.gpg": "/root/trusted.gpg.d/b.gpg",
	}
	out, records := rewriteSignedBy("etc/apt/sources.list.d/vendor.list", []byte(in), dest)
	got := string(out)
	if !strings.Contains(got, "signed-by=") {
		t.Fatalf("the whole signed-by pin was dropped, widening this source's trust to every key in the private root:\n%s", got)
	}
	if !strings.Contains(got, "signed-by=/root/trusted.gpg.d/a.gpg,/root/trusted.gpg.d/b.gpg") {
		t.Errorf("both keyrings should be rewritten and kept as a list:\n%s", got)
	}
	if strings.Contains(got, "/keys/a.gpg") || strings.Contains(got, "/keys/b.gpg") {
		t.Errorf("an original target path leaked into the private root's source:\n%s", got)
	}
	if !strings.Contains(got, "arch=amd64") || !strings.Contains(got, "https://vendor.example/repo stable main") {
		t.Errorf("sibling option or the URI/suite tail was lost:\n%s", got)
	}
	if len(records) != 2 {
		t.Fatalf("expected one record per keyring, got %+v", records)
	}
	for _, r := range records {
		if r.Stripped || r.RewrittenTo == "" {
			t.Errorf("record %+v: both keyrings resolved, so neither is stripped", r)
		}
	}
}

// TestRewriteSignedBy_OneLine_QuotedValue covers the other spelling apt
// accepts. Before the fix the "(\S+)" capture took the quotes as part of the
// path, so it matched nothing and the pin was stripped.
func TestRewriteSignedBy_OneLine_QuotedValue(t *testing.T) {
	in := `deb [signed-by="/keys/a.gpg"] https://vendor.example/repo stable main` + "\n"
	dest := map[string]string{"/keys/a.gpg": "/root/trusted.gpg.d/a.gpg"}
	out, records := rewriteSignedBy("etc/apt/sources.list.d/vendor.list", []byte(in), dest)
	got := string(out)
	if !strings.Contains(got, "signed-by=/root/trusted.gpg.d/a.gpg") {
		t.Fatalf("a quoted signed-by value was not rewritten (and so was stripped):\n%s", got)
	}
	if len(records) != 1 || records[0].Stripped {
		t.Errorf("records = %+v, want one clean rewrite", records)
	}
}

// TestRewriteSignedBy_OneLine_PartiallyResolvableListKeepsThePin checks the
// half-and-half case: one keyring was captured, the other was not. The pin
// must survive with the key that is available -- and the entry that could not
// be traced must NOT be reported Stripped, because root.go turns Stripped
// into a warning that says this source is no longer pinned at all, which
// would be untrue here.
func TestRewriteSignedBy_OneLine_PartiallyResolvableListKeepsThePin(t *testing.T) {
	in := "deb [signed-by=/keys/a.gpg,/keys/missing.gpg] https://vendor.example/repo stable main\n"
	dest := map[string]string{"/keys/a.gpg": "/root/trusted.gpg.d/a.gpg"}
	out, records := rewriteSignedBy("etc/apt/sources.list.d/vendor.list", []byte(in), dest)
	got := string(out)
	if !strings.Contains(got, "signed-by=/root/trusted.gpg.d/a.gpg") {
		t.Fatalf("the resolvable half of the pin was lost:\n%s", got)
	}
	if strings.Contains(got, "missing.gpg") {
		t.Errorf("an unresolvable keyring path was left pointing at the target's own filesystem:\n%s", got)
	}
	if len(records) != 2 {
		t.Fatalf("expected one record per entry, got %+v", records)
	}
	for _, r := range records {
		if r.Stripped {
			t.Errorf("record %+v must not be Stripped: the source is still pinned, one key smaller", r)
		}
	}
}

// TestRewriteSignedBy_OneLine_FingerprintFormIsKept covers apt's other
// documented Signed-By form. A bare fingerprint needs no rewriting to keep
// its meaning inside the private root, so dropping it would widen trust for
// no reason at all.
func TestRewriteSignedBy_OneLine_FingerprintFormIsKept(t *testing.T) {
	in := "deb [signed-by=DEADBEEFCAFE0123456789ABCDEF0123456789AB] https://vendor.example/repo stable main\n"
	out, records := rewriteSignedBy("etc/apt/sources.list.d/vendor.list", []byte(in), map[string]string{})
	got := string(out)
	if !strings.Contains(got, "signed-by=DEADBEEFCAFE0123456789ABCDEF0123456789AB") {
		t.Fatalf("a fingerprint-form signed-by was stripped:\n%s", got)
	}
	if len(records) != 1 || !records[0].Inline || records[0].Stripped {
		t.Errorf("records = %+v, want one Inline (nothing to rewrite) record", records)
	}
}

// TestRewriteSignedBy_Deb822_MultiKeyringStaysPinned is the same defect in
// the deb822 grammar, where the entries are whitespace-separated.
func TestRewriteSignedBy_Deb822_MultiKeyringStaysPinned(t *testing.T) {
	in := "Types: deb\nURIs: https://vendor.example/repo\nSuites: stable\nComponents: main\nSigned-By: /keys/a.gpg /keys/b.gpg\n"
	dest := map[string]string{
		"/keys/a.gpg": "/root/trusted.gpg.d/a.gpg",
		"/keys/b.gpg": "/root/trusted.gpg.d/b.gpg",
	}
	out, records := rewriteSignedBy("etc/apt/sources.list.d/vendor.sources", []byte(in), dest)
	got := string(out)
	if !strings.Contains(got, "Signed-By: /root/trusted.gpg.d/a.gpg,/root/trusted.gpg.d/b.gpg") {
		t.Fatalf("a multi-keyring deb822 Signed-By was not rewritten (so the whole field was dropped):\n%s", got)
	}
	if len(records) != 2 {
		t.Fatalf("expected one record per keyring, got %+v", records)
	}
}

func TestParseSourceEntries_Trusted(t *testing.T) {
	oneLine := []byte(
		"# a comment\n" +
			"deb [trusted=yes] http://attacker.example/ stable main\n" +
			"deb [arch=amd64] http://deb.debian.org/debian bookworm main\n" +
			"deb [arch=amd64 trusted=\"yes\"] http://quoted.example/ stable main\n" +
			"deb http://untrusted-looking.example/trusted=yes stable main\n",
	)
	got := parseSourceEntries("sources.list", oneLine)
	if len(got) != 4 {
		t.Fatalf("parsed %d entries, want 4: %+v", len(got), got)
	}
	want := []struct {
		uri     string
		trusted bool
	}{
		{"http://attacker.example/", true},
		{"http://deb.debian.org/debian", false},
		{"http://quoted.example/", true},
		// "trusted=yes" inside a URI path is not an option.
		{"http://untrusted-looking.example/trusted=yes", false},
	}
	for i, w := range want {
		if len(got[i].URIs) != 1 || got[i].URIs[0] != w.uri || got[i].Trusted != w.trusted {
			t.Errorf("entry %d = %+v, want uri=%q trusted=%v", i, got[i], w.uri, w.trusted)
		}
	}

	deb822 := []byte("Types: deb\nURIs: http://a.example/x http://b.example/y\nSuites: stable\nTrusted: yes\n\n" +
		"Types: deb\nURIs: http://c.example/z\nSuites: stable\n")
	entries := parseSourceEntries("vendor.sources", deb822)
	if len(entries) != 2 {
		t.Fatalf("parsed %d deb822 entries, want 2: %+v", len(entries), entries)
	}
	if len(entries[0].URIs) != 2 || !entries[0].Trusted {
		t.Errorf("stanza 0 = %+v, want both URIs and Trusted", entries[0])
	}
	if entries[1].Trusted {
		t.Errorf("stanza 1 = %+v, want Trusted=false (Trusted: yes must not leak across the blank line)", entries[1])
	}
}
