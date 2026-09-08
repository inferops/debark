package policy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
)

// These tests all have the same shape, because every defect they cover had
// the same shape: a policy file that an operator wrote to REFUSE something,
// which the loader accepted and then did not apply. Each one therefore does
// more than assert "Load returns an error" — where the policy loads at all,
// it goes on to evaluate a plan the policy plainly forbids and fails if the
// build would have been allowed. That way the test still describes the
// security property (a rule that should refuse must refuse) if the loader's
// behaviour is ever loosened again, instead of only pinning today's error.

const svLine = "schema_version: " + SchemaVersion + "\n"

// forbiddenPlan is a plan that violates every rule these policies express: a
// multiverse package, from a URL input, unsigned, carrying dkms, under an
// archive key nobody approved.
func forbiddenPlan() *resolve.Plan {
	return &resolve.Plan{Selections: []resolve.Selection{{
		Name: "evil-pkg", Arch: "amd64", Version: "1.0",
		Section:               "multiverse/net",
		URI:                   "https://alice:hunter2@vendor.example/private/evil.deb?token=s3cret",
		Reason:                lock.ReasonExternal,
		Flags:                 []string{lock.FlagDKMS},
		PublisherVerification: lock.VerifiedURLUnverified,
		Origin: lock.Origin{
			Component:      "multiverse",
			KeyFingerprint: "DEADBEEF00000000000000000000000000000000",
		},
	}}}
}

// mustNotAllow loads body as a policy file and fails unless the build is
// stopped — either because the file was refused outright (fail closed at
// load) or because evaluating it denied the plan. Anything else is a
// fail-open: the operator wrote a rule and the build went through.
func mustNotAllow(t *testing.T, name, ext, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy"+ext)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, err := Load(path)
	if err != nil {
		if got := dferr.ClassOf(err); got != dferr.Usage {
			t.Errorf("%s: refused with class %s, want %s (ADR-012: malformed config is a usage error)", name, got, dferr.Usage)
		}
		return
	}
	findings, err := ev.Evaluate(context.Background(), Input{Plan: forbiddenPlan()})
	if err != nil {
		t.Fatalf("%s: Evaluate: %v", name, err)
	}
	if !Deniable(findings) {
		t.Errorf("FAIL-OPEN %s: policy loaded without complaint and allowed a plan it forbids; findings=%+v", name, findings)
	}
}

// TestLoad_OutOfEnumDefaultSeverityFailsClosed covers the worst of the set. An
// out-of-enum default_severity is not a parse error, so the policy loaded,
// every rule ran, findings were produced — and then Deniable compared them
// against SeverityDeny exactly, did not match, and the build passed. Worse,
// core/engine records only SeverityWarn findings as lock warnings, so the
// finding did not even survive as a warning: it appeared in evidence.json and
// nowhere else.
func TestLoad_OutOfEnumDefaultSeverityFailsClosed(t *testing.T) {
	for _, sev := range []string{"Deny", "DENY", "denied", "deny!", "block", "error", "fatal", "critical", "high", "0"} {
		t.Run(sev, func(t *testing.T) {
			mustNotAllow(t, "default_severity: "+sev, ".yaml", svLine+"default_severity: "+sev+"\ndeny_flags:\n  - dkms\n")
		})
	}
	// The three the schema does declare must keep working.
	for _, sev := range []Severity{SeverityInfo, SeverityWarn, SeverityDeny} {
		path := filepath.Join(t.TempDir(), "policy.yaml")
		body := svLine + "default_severity: " + string(sev) + "\ndeny_flags:\n  - dkms\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err != nil {
			t.Errorf("default_severity %q is in the schema's enum but was refused: %v", sev, err)
		}
	}
}

// TestLoad_UnknownFieldFailsClosed: policy.v1.schema.json sets
// "additionalProperties": false, but Load used to ignore anything it did not
// recognise — so a singular "deny_flag", a mistyped "allowed_components" or a
// rule from a schema this binary does not know was a rule that silently did
// not run.
func TestLoad_UnknownFieldFailsClosed(t *testing.T) {
	cases := map[string]string{
		"singular deny_flag":    "deny_flag:\n  - dkms\n",
		"denyflags no under":    "denyflags:\n  - dkms\n",
		"singular deny_package": "deny_package:\n  - 'evil-*'\n",
		"plural publishers":     "require_signed_publishers: true\n",
		"allowed_components":    "allowed_components:\n  - main\n",
		"a rule from v2":        "rules:\n  - deny-dkms\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			mustNotAllow(t, name, ".yaml", svLine+"default_severity: deny\n"+body)
		})
	}
}

// TestLoad_SchemaVersionFailsClosed: schema_version is the schema's one
// required field and Load never looked at it. A policy written against a
// future schema — whose keys this binary does not know — decoded into a v1
// Policy with every field zero, which "is valid and finds nothing".
func TestLoad_SchemaVersionFailsClosed(t *testing.T) {
	rules := "default_severity: deny\ndeny_flags:\n  - dkms\n"
	mustNotAllow(t, "absent", ".yaml", rules)
	mustNotAllow(t, "future", ".yaml", "schema_version: debark.policy/v2\n"+rules)
	mustNotAllow(t, "garbage", ".yaml", "schema_version: nonsense\n"+rules)
	mustNotAllow(t, "empty file", ".yaml", "")
	mustNotAllow(t, "comments only", ".yaml", "# default_severity: deny\n")
	mustNotAllow(t, "doc marker only", ".yaml", "---\n")
}

// TestLoad_NonObjectJSONFailsClosed: "null" and "[]" both unmarshal into a
// Policy struct without error and leave it zero — an empty policy that finds
// nothing, from a file that is not a policy at all.
func TestLoad_NonObjectJSONFailsClosed(t *testing.T) {
	mustNotAllow(t, "json null", ".json", `null`)
	mustNotAllow(t, "json array", ".json", `[]`)
	mustNotAllow(t, "json number", ".json", `7`)
	mustNotAllow(t, "json empty object", ".json", `{}`)
}

// TestLoad_JSONBodyInAYAMLFileFailsClosed: naming a JSON policy file .yaml
// routes it to the YAML reader (the extension wins over content sniffing),
// which used to parse it into one garbage key and hand back an empty policy.
func TestLoad_JSONBodyInAYAMLFileFailsClosed(t *testing.T) {
	body := `{"schema_version": "` + SchemaVersion + `", "default_severity": "deny", "deny_flags": ["dkms"]}`
	mustNotAllow(t, "json body, .yaml name", ".yaml", body)
	mustNotAllow(t, "yaml flow mapping", ".yaml", "{default_severity: deny, deny_flags: [dkms]}")
}

// TestLoad_DuplicateFieldFailsClosed: both readers keep the LAST occurrence,
// so a rule stated once and emptied later in the same file was silently
// disarmed — and a reader that kept the first would disagree about what the
// same bytes mean.
func TestLoad_DuplicateFieldFailsClosed(t *testing.T) {
	mustNotAllow(t, "yaml duplicate", ".yaml",
		svLine+"default_severity: deny\ndeny_flags:\n  - dkms\ndeny_flags: []\n")
	mustNotAllow(t, "json duplicate", ".json",
		`{"schema_version":"`+SchemaVersion+`","default_severity":"deny","deny_flags":["dkms"],"deny_flags":[]}`)
}

// TestLoad_MalformedGlobFailsClosed: path.Match reports ErrBadPattern, which
// globMatchesAny turns into "does not match" — so a malformed deny_packages
// entry is a rule that protects nothing while looking like it does. (In an
// allow_packages list the same typo fails closed on its own, which is exactly
// the asymmetry that made the deny side worth catching at load.)
func TestLoad_MalformedGlobFailsClosed(t *testing.T) {
	mustNotAllow(t, "deny_packages bad glob", ".yaml",
		svLine+"default_severity: deny\ndeny_packages:\n  - 'evil-[pkg'\n")
	mustNotAllow(t, "allow_packages bad glob", ".yaml",
		svLine+"default_severity: deny\nallow_packages:\n  - 'good-[pkg'\n")
}

// TestLoad_UnknownDenyFlagFailsClosed: deny_flags is an enum in the schema,
// and a value outside it can never match any package's flags — so "dksm" or
// "non_free" is a denial that never fires.
func TestLoad_UnknownDenyFlagFailsClosed(t *testing.T) {
	mustNotAllow(t, "typo dksm", ".yaml", svLine+"default_severity: deny\ndeny_flags:\n  - dksm\n")
	mustNotAllow(t, "underscore", ".yaml", svLine+"default_severity: deny\ndeny_flags:\n  - non_free\n")

	// The real vocabulary, in any case, must still load: evaluation compares
	// flags case-insensitively and validation has to agree with it.
	for _, f := range []string{"dkms", "DKMS", "network-postinst", "non-free-firmware"} {
		path := filepath.Join(t.TempDir(), "policy.yaml")
		if err := os.WriteFile(path, []byte(svLine+"deny_flags:\n  - "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err != nil {
			t.Errorf("deny_flags %q is a real lock flag but was refused: %v", f, err)
		}
	}
}

// TestLoadApprovedKeys_FailsClosed covers the --approved-keys file. An
// allow-list that names nothing must not mean "everything is allowed", and an
// entry that is not a full fingerprint must not become a matchable token.
func TestLoadApprovedKeys_FailsClosed(t *testing.T) {
	const fp = "CAE265BE4F5710A1C6714384E04F6BB6DC1AE916"
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "keys.txt")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	refused := map[string]string{
		"empty file":            "",
		"only comments":         "# " + fp + "\n\n# nothing live\n",
		"only blank lines":      "\n\n   \n",
		"short key id":          "DEADBEEF\n",
		"long key id":           "DEADBEEF00000000\n",
		"not hex":               "not-a-key\n",
		"one good one garbage":  fp + "\nnope\n",
		"fingerprint truncated": fp[:39] + "\n",
	}
	for name, body := range refused {
		t.Run(name, func(t *testing.T) {
			out, err := LoadApprovedKeys(write(t, body))
			if err == nil {
				t.Fatalf("FAIL-OPEN: accepted an approved-keys file that names no usable key: %v", out)
			}
			if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("class %s, want %s", got, dferr.Usage)
			}
		})
	}

	accepted := map[string]string{
		"plain":         fp + "\n",
		"lowercase":     strings.ToLower(fp) + "\n",
		"with comment":  fp + "  # the vendor archive key\n",
		"0x prefixed":   "0x" + fp + "\n",
		"gpg spaced":    "CAE2 65BE 4F57 10A1 C671  4384 E04F 6BB6 DC1A E916\n",
		"crlf line end": fp + "\r\n",
	}
	for name, body := range accepted {
		t.Run(name, func(t *testing.T) {
			out, err := LoadApprovedKeys(write(t, body))
			if err != nil {
				t.Fatalf("refused a valid fingerprint: %v", err)
			}
			if len(out) != 1 || out[0] != fp {
				t.Fatalf("got %v, want [%s] — every accepted spelling must normalise to one form", out, fp)
			}
		})
	}
}

// TestEvaluate_ApprovedKeysNeverTrustsAShortID is the rule's own half of the
// same problem. approvedKeySet used to admit any non-empty string, so a list
// holding a short key ID — what `gpg --list-keys` prints, and the most
// natural thing to paste — became a token a plan could match. Short and long
// key IDs are the low bits of a fingerprint and are cheap to collide on
// purpose, so an allow-list built from them is one an attacker can join.
func TestEvaluate_ApprovedKeysNeverTrustsAShortID(t *testing.T) {
	p := &Policy{SchemaVersion: SchemaVersion, DefaultSeverity: SeverityDeny}
	ev := evaluator{p: p}

	for _, claim := range []string{"DEADBEEF", "DEADBEEF00000000", "0xDEADBEEF"} {
		plan := forbiddenPlan()
		plan.Selections[0].Origin.KeyFingerprint = claim
		findings, err := ev.Evaluate(context.Background(), Input{Plan: plan, ApprovedKeys: []string{claim}})
		if err != nil {
			t.Fatal(err)
		}
		if !Deniable(findings) {
			t.Errorf("FAIL-OPEN: a plan claiming key %q was approved by an allow-list holding the same short id", claim)
		}
	}

	// A full fingerprint still matches, in any of its spellings, or the rule
	// would fail closed so hard that operators would switch it off.
	const fp = "CAE265BE4F5710A1C6714384E04F6BB6DC1AE916"
	for _, spelling := range []string{fp, strings.ToLower(fp), "0x" + fp, "CAE2 65BE 4F57 10A1 C671  4384 E04F 6BB6 DC1A E916"} {
		plan := forbiddenPlan()
		plan.Selections[0].Origin.KeyFingerprint = spelling
		findings, err := ev.Evaluate(context.Background(), Input{Plan: plan, ApprovedKeys: []string{fp}})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range findings {
			if f.Rule == "approved-keys" {
				t.Errorf("approved key spelled %q was reported as unapproved: %s", spelling, f.Message)
			}
		}
	}
}

// TestEvaluate_MalformedFingerprintIsReportedAsSuch: Origin.KeyFingerprint is
// a claim carried in the plan, not something this package can verify. What it
// must not do is compare an unvalidated string as an opaque token — and an
// auditor needs to tell "a real key nobody approved" apart from "this is not
// a fingerprint at all".
func TestEvaluate_MalformedFingerprintIsReportedAsSuch(t *testing.T) {
	ev := evaluator{p: &Policy{SchemaVersion: SchemaVersion, DefaultSeverity: SeverityDeny}}
	plan := forbiddenPlan()
	plan.Selections[0].Origin.KeyFingerprint = "not a fingerprint"
	findings, err := ev.Evaluate(context.Background(), Input{
		Plan:         plan,
		ApprovedKeys: []string{"CAE265BE4F5710A1C6714384E04F6BB6DC1AE916"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !Deniable(findings) {
		t.Fatal("FAIL-OPEN: a malformed archive key fingerprint was accepted")
	}
	var got string
	for _, f := range findings {
		if f.Rule == "approved-keys" {
			got = f.Message
		}
	}
	if !strings.Contains(got, "not a full archive key fingerprint") {
		t.Errorf("finding %q does not distinguish a malformed fingerprint from an unapproved one", got)
	}
}

// TestEvaluate_URLDetailIsRedacted: a vendor URL input legitimately carries
// userinfo credentials or a query token, and Finding.Detail is documented as
// carrying "the URL" for a consumer to record. Every other place this project
// puts a URL in front of a reader goes through fetch.RedactURL.
func TestEvaluate_URLDetailIsRedacted(t *testing.T) {
	no := false
	ev := evaluator{p: &Policy{SchemaVersion: SchemaVersion, DefaultSeverity: SeverityDeny, AllowURLInputs: &no}}
	findings, err := ev.Evaluate(context.Background(), Input{Plan: forbiddenPlan()})
	if err != nil {
		t.Fatal(err)
	}
	var url string
	for _, f := range findings {
		if f.Rule == "allow-url-inputs" {
			url = f.Detail["url"]
		}
	}
	if url == "" {
		t.Fatal("no allow-url-inputs finding produced")
	}
	for _, secret := range []string{"hunter2", "s3cret", "alice:"} {
		if strings.Contains(url, secret) {
			t.Errorf("Finding.Detail[url] = %q leaks %q", url, secret)
		}
	}
	if !strings.Contains(url, "vendor.example") {
		t.Errorf("Finding.Detail[url] = %q lost the host, which is what makes it useful", url)
	}
}

// TestDenialIsExitClassPolicy pins the one number ADR-012 froze for this
// package's whole reason to exist: a denial is exit 6, and nothing else in
// this package returns that class by accident.
func TestDenialIsExitClassPolicy(t *testing.T) {
	if int(dferr.Policy) != 6 {
		t.Fatalf("dferr.Policy = %d, want 6 (ADR-012, frozen)", int(dferr.Policy))
	}
	ev := evaluator{p: &Policy{SchemaVersion: SchemaVersion, DefaultSeverity: SeverityDeny, DenyFlags: []string{lock.FlagDKMS}}}
	findings, err := ev.Evaluate(context.Background(), Input{Plan: forbiddenPlan()})
	if err != nil {
		t.Fatal(err)
	}
	if !Deniable(findings) {
		t.Fatal("a deny-severity finding did not report as deniable")
	}

	// A cancelled context is NOT a policy violation, and must not be able to
	// masquerade as one: core/engine classifies an Evaluate error as
	// dferr.Policy only when the error carries no class of its own.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ev.Evaluate(ctx, Input{Plan: forbiddenPlan()}); err == nil {
		t.Fatal("a cancelled context produced no error")
	} else if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("cancelled Evaluate has class %s, want %s", got, dferr.Usage)
	}
}

// TestEvaluate_RequireSignedPublisherFailsClosedOnAnUnknownClaim is the
// rule-level twin of the loader defects above: nothing about the POLICY FILE
// is wrong here, but the rule read the plan and drew the permissive
// conclusion.
//
// require_signed_publisher used to deny a package only when
// publisher_verification held exactly "url-unverified". Every other spelling
// passed — including the empty string, a case variant, one with a trailing
// space, and any value from a schema this binary does not know. Each of those
// is a file whose provenance nothing established, which is the one thing this
// rule exists to refuse.
//
// The path is reachable. core/engine runs evaluatePolicy against the resolved
// PLAN, before buildInitialLock; lock.Validate — the only code that enforces
// publisher_verification's enum — sees the LOCK, several steps later. With
// the container backend the plan arrives as JSON from a separate process and
// containerCrossCheckEnvelope never looks at this field.
func TestEvaluate_RequireSignedPublisherFailsClosedOnAnUnknownClaim(t *testing.T) {
	ev := evaluator{p: &Policy{SchemaVersion: SchemaVersion, DefaultSeverity: SeverityDeny, RequireSignedPublisher: true}}

	// Values that are not an affirmative provenance claim. Every one of them
	// must stop the build.
	unrecognised := []lock.PublisherVerification{
		"",                 // a backend that never filled the field in
		"URL-UNVERIFIED",   // the enum is lowercase; this is not a member
		"url-unverified ",  // trailing space
		" url-unverified",  // leading space
		"unverified",       // plausible, and not a member
		"totally-fine",     // an attacker's own choice of word
		"apt-signed-ish",   // a near miss on a member that does pass
		"user-signature\n", // a member with a newline glued on
	}
	for _, pv := range unrecognised {
		t.Run("unrecognised/"+string(pv), func(t *testing.T) {
			plan := forbiddenPlan()
			plan.Selections[0].PublisherVerification = pv
			findings, err := ev.Evaluate(context.Background(), Input{Plan: plan})
			if err != nil {
				t.Fatal(err)
			}
			if !Deniable(findings) {
				t.Errorf("FAIL-OPEN: require_signed_publisher allowed a package whose publisher_verification is %q; findings=%+v", string(pv), findings)
			}
		})
	}

	// The four values debark.lock/v1 does define must keep exactly the
	// verdict they had, or the fix would be over-strict rather than
	// fail-closed: only url-unverified is a denial.
	verdict := map[lock.PublisherVerification]bool{
		lock.VerifiedURLUnverified: true,
		lock.VerifiedAPTSigned:     false,
		lock.VerifiedUserDigest:    false,
		lock.VerifiedUserSignature: false,
	}
	for pv, wantDeny := range verdict {
		t.Run("enum/"+string(pv), func(t *testing.T) {
			plan := forbiddenPlan()
			plan.Selections[0].PublisherVerification = pv
			findings, err := ev.Evaluate(context.Background(), Input{Plan: plan})
			if err != nil {
				t.Fatal(err)
			}
			if got := Deniable(findings); got != wantDeny {
				t.Errorf("publisher_verification %q: denied=%v, want %v", string(pv), got, wantDeny)
			}
		})
	}
}

// TestEvaluate_URLInputSchemeIsCaseInsensitive: RFC 3986 §3.1 makes a URL
// scheme case-insensitive and net/url lowercases it on parse, so core/fetch
// downloads "HTTPS://vendor.example/x.deb" exactly like the lowercase form.
// The allow-url-inputs rule compared a literal lowercase prefix, so that
// spelling walked straight past a policy that forbids URL inputs.
func TestEvaluate_URLInputSchemeIsCaseInsensitive(t *testing.T) {
	no := false
	ev := evaluator{p: &Policy{SchemaVersion: SchemaVersion, DefaultSeverity: SeverityDeny, AllowURLInputs: &no}}

	deny := []string{
		"https://vendor.example/x.deb",
		"HTTPS://vendor.example/x.deb",
		"HtTpS://vendor.example/x.deb",
		"http://vendor.example/x.deb",
		"HTTP://vendor.example/x.deb",
	}
	for _, uri := range deny {
		plan := forbiddenPlan()
		plan.Selections[0].URI = uri
		findings, err := ev.Evaluate(context.Background(), Input{Plan: plan})
		if err != nil {
			t.Fatal(err)
		}
		if !Deniable(findings) {
			t.Errorf("FAIL-OPEN: a URL input spelled %q was allowed by a policy with allow_url_inputs: false", uri)
		}
	}

	// A local file input carries no remote URL and must stay allowed: this
	// rule is about URL inputs specifically, not about external inputs in
	// general (see examples/policy.yaml, "as opposed to a local file").
	for _, uri := range []string{"", "file:///work/external/x.deb", "/work/external/x.deb"} {
		plan := forbiddenPlan()
		plan.Selections[0].URI = uri
		findings, err := ev.Evaluate(context.Background(), Input{Plan: plan})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range findings {
			if f.Rule == "allow-url-inputs" {
				t.Errorf("uri %q is not a URL input but produced %s: %s", uri, f.Rule, f.Message)
			}
		}
	}
}
