package policy

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/fetch"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
)

// evaluator is the Evaluator a loaded policy file produces.
type evaluator struct {
	p *Policy
}

// Evaluate is pure: no clock, no network, no filesystem — it only reads in
// and e's own already-parsed Policy. Two calls with equal arguments produce
// equal, equally-ordered findings.
//
// Rules run in a fixed order — component-deny, component-allow, package-
// deny, package-allow, require-signed-publisher, allow-url-inputs,
// deny-flags, approved-keys — and within each rule, packages are visited in
// resolve.SortSelections order (name, then arch, then version) over a local
// copy of in.Plan.Selections, so in.Plan itself is never mutated and the
// finding order never depends on map iteration or caller-supplied order.
func (e evaluator) Evaluate(ctx context.Context, in Input) ([]Finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "policy: evaluate")
	}
	if e.p == nil || in.Plan == nil || len(in.Plan.Selections) == 0 {
		return nil, nil
	}

	sels := make([]resolve.Selection, len(in.Plan.Selections))
	copy(sels, in.Plan.Selections)
	resolve.SortSelections(sels)

	sev := e.p.DefaultSeverity
	if sev == "" {
		sev = SeverityWarn
	}

	var findings []Finding

	if len(e.p.DenyComponents) > 0 {
		for _, s := range sels {
			comp := componentOf(s)
			if comp != "" && matchesAnyFold(e.p.DenyComponents, comp) {
				findings = append(findings, Finding{
					Rule: "component-deny", Severity: sev,
					Message:  fmt.Sprintf("component %q is denied by policy", comp),
					Packages: []string{s.Name},
					Detail:   map[string]string{"component": comp},
				})
			}
		}
	}
	if len(e.p.AllowComponents) > 0 {
		for _, s := range sels {
			comp := componentOf(s)
			if comp == "" || !matchesAnyFold(e.p.AllowComponents, comp) {
				findings = append(findings, Finding{
					Rule: "component-allow", Severity: sev,
					Message:  fmt.Sprintf("component %q is not in the allowed component list", comp),
					Packages: []string{s.Name},
					Detail:   map[string]string{"component": comp},
				})
			}
		}
	}
	if len(e.p.DenyPackages) > 0 {
		for _, s := range sels {
			if globMatchesAny(e.p.DenyPackages, s.Name) {
				findings = append(findings, Finding{
					Rule: "package-deny", Severity: sev,
					Message:  fmt.Sprintf("package %q matches a denied name pattern", s.Name),
					Packages: []string{s.Name},
				})
			}
		}
	}
	if len(e.p.AllowPackages) > 0 {
		for _, s := range sels {
			if !globMatchesAny(e.p.AllowPackages, s.Name) {
				findings = append(findings, Finding{
					Rule: "package-allow", Severity: sev,
					Message:  fmt.Sprintf("package %q does not match any allowed name pattern", s.Name),
					Packages: []string{s.Name},
				})
			}
		}
	}
	if e.p.RequireSignedPublisher {
		for _, s := range sels {
			// The test is an ALLOW-list over publisher_verification's enum,
			// not "is it the one bad value". It used to be the latter — a
			// package was denied only when the field held exactly
			// "url-unverified" — which made every other spelling pass:
			// "" (a plan whose backend never filled the field in),
			// "URL-UNVERIFIED", "url-unverified " with a trailing space,
			// "unverified", or any value from a schema this binary does not
			// know. Each of those is a file whose provenance nothing has
			// established, which is precisely what this rule exists to
			// refuse, and each of them made the rule pass instead.
			//
			// Nothing has checked the field by the time this runs: policy is
			// evaluated against the resolved PLAN (core/engine's run order
			// puts evaluatePolicy before buildInitialLock), and lock.Validate
			// — the only thing that enforces the enum — sees the LOCK, later.
			// For the container backend the value arrives inside a JSON
			// envelope produced by a separate process, and
			// containerCrossCheckEnvelope does not look at it at all. So the
			// unrecognised case is reachable, and a security control must
			// treat "I do not recognise this claim" as "no claim".
			if signedPublisherClaim[s.PublisherVerification] {
				continue
			}
			msg := fmt.Sprintf("package %q has no publisher signature (publisher_verification=%q)", s.Name, string(s.PublisherVerification))
			if s.PublisherVerification != lock.VerifiedURLUnverified {
				msg = fmt.Sprintf("package %q records publisher_verification %q, which is not a provenance claim debark.lock/v1 defines; treated as unsigned",
					s.Name, string(s.PublisherVerification))
			}
			findings = append(findings, Finding{
				Rule: "require-signed-publisher", Severity: sev,
				Message:  msg,
				Packages: []string{s.Name},
				Detail:   map[string]string{"publisher_verification": string(s.PublisherVerification)},
			})
		}
	}
	if e.p.AllowURLInputs != nil && !*e.p.AllowURLInputs {
		for _, s := range sels {
			if s.Reason == lock.ReasonExternal && isHTTPURL(s.URI) {
				findings = append(findings, Finding{
					Rule: "allow-url-inputs", Severity: sev,
					Message:  fmt.Sprintf("package %q came from a URL input, which this policy disallows", s.Name),
					Packages: []string{s.Name},
					// Redacted, not raw. A vendor URL input legitimately
					// carries userinfo credentials or a query token
					// ("https://user:token@vendor.example/private.deb"), and
					// a Finding is not a private value: iface.go's package
					// comment says a commercial edition "would consume
					// exactly these findings", and Detail is documented as
					// carrying "the URL". Every other place this project
					// puts a URL in front of a reader goes through
					// fetch.RedactURL — the one implementation, kept single
					// on purpose, since a second scrubber is how the two
					// drift apart and one of them stops covering a form the
					// other does.
					Detail: map[string]string{"url": fetch.RedactURL(s.URI)},
				})
			}
		}
	}
	if len(e.p.DenyFlags) > 0 {
		for _, s := range sels {
			var matched []string
			for _, f := range s.Flags {
				if containsFold(e.p.DenyFlags, f) {
					matched = append(matched, f)
				}
			}
			if len(matched) > 0 {
				sort.Strings(matched)
				findings = append(findings, Finding{
					Rule: "deny-flags", Severity: sev,
					Message:  fmt.Sprintf("package %q carries a denied flag: %s", s.Name, strings.Join(matched, ", ")),
					Packages: []string{s.Name},
					Detail:   map[string]string{"flags": strings.Join(matched, ",")},
				})
			}
		}
	}

	if approved, supplied := approvedKeySet(e.p.ApprovedKeys, in.ApprovedKeys); supplied > 0 {
		for _, s := range sels {
			// Origin.KeyFingerprint is a CLAIM, carried in the plan, about
			// which archive key signed the Release this file was listed in.
			// This rule cannot verify it: Evaluate is pure (no keyring, no
			// key material, no filesystem — see the method comment), so all
			// it can honestly do is decide whether the operator approved the
			// identity being claimed. Proving the claim belongs upstream,
			// where the keyring actually is.
			//
			// What it must NOT do is compare an unvalidated attacker-side
			// string against the allow-list as an opaque token. Both sides go
			// through the same normaliser, and a claim that is not a full
			// fingerprint is reported as malformed instead of silently
			// becoming a "not approved" that reads like an ordinary
			// wrong-key result — a truncated or invented fingerprint is a
			// different problem from a real key nobody approved, and an
			// auditor needs to be able to tell them apart.
			raw := strings.TrimSpace(s.Origin.KeyFingerprint)
			fp, wellFormed := normalizeFingerprint(raw)
			switch {
			case raw == "":
				findings = append(findings, Finding{
					Rule: "approved-keys", Severity: sev,
					Message:  fmt.Sprintf("package %q has no recorded archive key fingerprint", s.Name),
					Packages: []string{s.Name},
				})
			case !wellFormed:
				findings = append(findings, Finding{
					Rule: "approved-keys", Severity: sev,
					Message:  fmt.Sprintf("package %q records %q, which is not a full archive key fingerprint", s.Name, raw),
					Packages: []string{s.Name},
					Detail:   map[string]string{"fingerprint": raw},
				})
			case !approved[fp]:
				findings = append(findings, Finding{
					Rule: "approved-keys", Severity: sev,
					Message:  fmt.Sprintf("package %q's archive key is not in the approved-keys list", s.Name),
					Packages: []string{s.Name},
					Detail:   map[string]string{"fingerprint": fp},
				})
			}
		}
	}

	return findings, nil
}

// signedPublisherClaim is the set of publisher_verification values that
// amount to a positive statement about who produced a file:
//
//   - apt-signed: the Release listing it verified against a captured keyring.
//   - user-signature: a vendor signature the operator supplied verified.
//   - user-digest: the operator supplied --digest and it matched, which is
//     the operator vouching for the bytes by hand.
//
// It is deliberately expressed as the set that PASSES rather than the one
// value that fails, so a publisher_verification this binary does not
// recognise — a future enum member, a truncated or invented string, or the
// empty value a backend forgot to fill in — is denied by default instead of
// admitted by default. The three members are exactly the three that passed
// before, so no plan an honest build produces changes verdict.
var signedPublisherClaim = map[lock.PublisherVerification]bool{
	lock.VerifiedAPTSigned:     true,
	lock.VerifiedUserDigest:    true,
	lock.VerifiedUserSignature: true,
}

// componentOf reports the archive component a selection came from, preferring
// the resolved Origin.Component and falling back to the leading segment of
// Section (e.g. "universe/net" -> "universe") when Origin was not populated.
func componentOf(s resolve.Selection) string {
	if s.Origin.Component != "" {
		return s.Origin.Component
	}
	if s.Section == "" {
		return ""
	}
	if i := strings.IndexByte(s.Section, '/'); i >= 0 {
		return s.Section[:i]
	}
	return s.Section
}

// isHTTPURL reports whether uri is an http or https URL. The scheme is
// compared case-INSENSITIVELY, as RFC 3986 §3.1 requires ("schemes are case-
// insensitive"): net/url lowercases the scheme when it parses, so
// "HTTPS://vendor.example/x.deb" is fetched exactly like the lowercase form
// by core/fetch, and a literal prefix test — which is what this was — let
// that spelling walk past the allow-url-inputs rule while every other reader
// treated it as the same URL.
func isHTTPURL(uri string) bool {
	i := strings.IndexByte(uri, ':')
	if i <= 0 {
		return false
	}
	switch strings.ToLower(uri[:i]) {
	case "http", "https":
		return strings.HasPrefix(uri[i:], "://")
	}
	return false
}

func matchesAnyFold(list []string, val string) bool {
	for _, v := range list {
		if strings.EqualFold(v, val) {
			return true
		}
	}
	return false
}

func containsFold(list []string, val string) bool {
	return matchesAnyFold(list, val)
}

// globMatchesAny reports whether name matches any of patterns, using
// path.Match semantics: '*' matches any sequence of characters not containing
// '/', '?' matches any single non-'/' character, and '[...]' is a character
// class — the ordinary shell-glob rules for one path segment. Package names
// never contain '/', so in practice this is a plain glob over the whole name
// (e.g. "lib*", "*-dbg", "linux-image-*"). A malformed pattern
// (path.ErrBadPattern) is treated as "does not match" rather than an error,
// so one bad glob in a policy file cannot crash evaluation — Evaluate has no
// way to report a warning about the policy file itself, only findings about
// packages.
func globMatchesAny(patterns []string, name string) bool {
	for _, pat := range patterns {
		if ok, err := path.Match(pat, name); err == nil && ok {
			return true
		}
	}
	return false
}

// approvedKeySet builds the normalised union of a policy file's inline
// approved_keys and the caller-supplied --approved-keys list (Input.
// ApprovedKeys). It reports the set and how many entries were SUPPLIED,
// which is not the same number: entries that are not full fingerprints are
// dropped from the set rather than admitted as opaque tokens (see
// normalizeFingerprint for why a short key ID must never become one).
//
// The two counts have to be reported separately, because whether the rule
// runs at all depends on the second. "Nobody configured an approved-keys
// list" means no constraint, per both fields' doc comments in iface.go — but
// "somebody configured one and every entry in it was unusable" must NOT also
// mean no constraint, or the loosest possible allow-list would be the one
// that switches the rule off. Gating on the supplied count instead of the
// usable one makes such a list deny everything, which is what an allow-list
// that approves nothing should do.
//
// Load and LoadApprovedKeys already refuse a file with an unusable entry in
// it, so in the normal path nothing is dropped here; this is the same rule
// applied again at the point of use, for an Input.ApprovedKeys assembled by
// an embedder that came through neither loader.
func approvedKeySet(a, b []string) (set map[string]bool, supplied int) {
	set = map[string]bool{}
	for _, list := range [2][]string{a, b} {
		for _, k := range list {
			if strings.TrimSpace(k) == "" {
				continue
			}
			supplied++
			if fp, ok := normalizeFingerprint(k); ok {
				set[fp] = true
			}
		}
	}
	return set, supplied
}
