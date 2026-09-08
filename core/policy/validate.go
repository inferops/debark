package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/inferops/debark/core/lock"
)

// This file is the loader's fail-closed half. Everything it enforces is
// already frozen in api/schema/policy.v1.schema.json — the published field
// reference docs/formats.md §3.11 points operators at — and none of it was
// enforced by Load, which decoded a policy file with plain encoding/json and
// asked no further questions.
//
// For a security control that asymmetry is the whole problem. Policy is the
// operator's only guardrail over what crosses the gap, and every way of
// getting the file slightly wrong failed OPEN: an unknown key (a typo'd
// rule name, or a rule from a schema this binary does not know) was
// silently ignored, so the rule the operator wrote simply did not run; a
// default_severity outside the enum — "Deny" with a capital D, "block",
// "critical" — produced findings at a severity nothing downstream treats as
// a denial, so the build passed AND the finding never even reached the lock
// as a warning (core/engine/policy.go records only SeverityWarn there); and
// a schema_version naming a future schema loaded as an empty v1 policy that
// finds nothing at all. In each case the operator sees a build succeed and
// has no way to tell that their policy was not applied.
//
// So Load now refuses a policy file it cannot apply exactly as written,
// with dferr.Usage (exit 1, ADR-012's "usage or configuration error:
// malformed config") — loud, immediate, and impossible to mistake for
// approval.

// policyFields is every key debark.policy/v1 declares, taken field for
// field from policy.v1.schema.json's "properties" (and matching Policy's own
// json/yaml tags in iface.go). The schema sets "additionalProperties": false,
// so anything else makes the document invalid.
var policyFields = map[string]bool{
	"schema_version":           true,
	"allow_components":         true,
	"deny_components":          true,
	"allow_packages":           true,
	"deny_packages":            true,
	"require_signed_publisher": true,
	"allow_url_inputs":         true,
	"approved_keys":            true,
	"deny_flags":               true,
	"default_severity":         true,
}

// knownFlags is deny_flags' enum from the same schema, expressed through the
// core/lock constants that define the vocabulary so the two cannot drift.
// A deny_flags entry outside it can never match any package's flags, which
// makes a typo ("dksm", "non_free") a rule that silently protects nothing.
var knownFlags = []string{
	lock.FlagMultiverse,
	lock.FlagRestricted,
	lock.FlagNonFree,
	lock.FlagNonFreeFirmware,
	lock.FlagNetworkPostinst,
	lock.FlagSnapShim,
	lock.FlagDKMS,
	lock.FlagUserSupplied,
}

// checkPolicyKeys reports the first key that debark.policy/v1 does not
// declare, or the first one given twice. Duplicates matter as much as
// unknown keys: encoding/json and this package's YAML reader both keep the
// LAST occurrence, so "deny_flags: [dkms]" followed later in the same file by
// "deny_flags: []" silently disarms the rule, and a reader that keeps the
// first would disagree about what the very same file means.
func checkPolicyKeys(keys []string) error {
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if !policyFields[k] {
			return fmt.Errorf("unknown field %q (debark.policy/v1 declares only: %s)", k, strings.Join(sortedFields(), ", "))
		}
		if seen[k] {
			return fmt.Errorf("field %q given more than once; a policy file must say exactly one thing about each rule", k)
		}
		seen[k] = true
	}
	return nil
}

func sortedFields() []string {
	out := make([]string, 0, len(policyFields))
	for k := range policyFields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// jsonObjectKeys returns the keys of a top-level JSON object in file order,
// duplicates included — encoding/json's own decoder silently collapses both,
// which is exactly what checkPolicyKeys has to see. It also enforces that the
// document IS an object: "null" and "[]" both unmarshal into a Policy struct
// without complaint and yield an empty policy that finds nothing.
func jsonObjectKeys(data []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected a JSON object at the top level, found %v", tok)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := k.(string)
		if !ok {
			return nil, fmt.Errorf("expected an object key, found %v", k)
		}
		keys = append(keys, name)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return nil, err
	}
	return keys, nil
}

// validatePolicy enforces every constraint policy.v1.schema.json states about
// a decoded policy that encoding/json's type checking does not already cover.
func validatePolicy(p *Policy) error {
	// schema_version is the schema's one required field. Without this check a
	// policy written against a future (or misspelt) schema decodes into a v1
	// Policy whose every field is zero — an empty policy, which "is valid and
	// finds nothing". Refusing is the only answer that cannot be mistaken for
	// a clean build.
	if p.SchemaVersion == "" {
		return fmt.Errorf("missing schema_version (want %q); it is the one field every policy file must set", SchemaVersion)
	}
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %q (want %q)", p.SchemaVersion, SchemaVersion)
	}

	// An out-of-enum default_severity is the single most dangerous value in
	// the file: it is not a parse error, so the policy loads, every rule runs
	// and produces findings — and then Deniable() (and core/engine's own
	// check) compares against SeverityDeny exactly, does not match, and the
	// build passes. "Deny" and "DENY" are rejected rather than folded to
	// "deny" deliberately: the schema's enum is lowercase, and a control
	// whose meaning depends on how a value is spelt should say so out loud
	// once rather than guess quietly on every run.
	switch p.DefaultSeverity {
	case "", SeverityInfo, SeverityWarn, SeverityDeny:
	default:
		return fmt.Errorf("default_severity %q is not one of %q, %q, %q",
			p.DefaultSeverity, SeverityInfo, SeverityWarn, SeverityDeny)
	}

	for _, f := range p.DenyFlags {
		if !matchesAnyFold(knownFlags, f) {
			return fmt.Errorf("deny_flags: %q is not a lock flag (want one of: %s)", f, strings.Join(knownFlags, ", "))
		}
	}

	// A malformed glob is not an error for path.Match's caller, it is a
	// pattern that never matches — which turns a deny_packages entry into a
	// rule that protects nothing while looking like it does. Evaluate still
	// tolerates one (it has no channel to report a policy-file problem, only
	// findings about packages); Load is where it can be said properly.
	for field, pats := range map[string][]string{"allow_packages": p.AllowPackages, "deny_packages": p.DenyPackages} {
		for _, pat := range pats {
			if _, err := path.Match(pat, ""); err != nil {
				return fmt.Errorf("%s: %q is not a valid glob pattern: %w", field, pat, err)
			}
		}
	}

	for i, k := range p.ApprovedKeys {
		norm, ok := normalizeFingerprint(k)
		if !ok {
			return fmt.Errorf("approved_keys[%d]: %q is not a full OpenPGP key fingerprint; %s", i, k, fingerprintRule)
		}
		p.ApprovedKeys[i] = norm
	}

	return nil
}

// fingerprintRule is the one-line explanation attached to every rejection, so
// an operator who pasted the wrong thing is told what to paste instead.
const fingerprintRule = "want the full 40-hex-digit (v4) or 64-hex-digit (v5) fingerprint, " +
	"as printed by `gpg --with-colons --fingerprint` — not a short key ID"

// normalizeFingerprint puts an OpenPGP fingerprint into the one form this
// package compares, and reports whether what it was given is a fingerprint at
// all.
//
// Normalisation is deliberately limited to presentation: internal whitespace
// (gpg's own `--fingerprint` output prints a v4 fingerprint as ten
// space-separated groups) and an optional 0x prefix are removed, and the hex
// is upper-cased. None of that can make one key's fingerprint equal another's
// — it only stops a correctly-copied fingerprint from being silently
// unmatchable, which is its own hazard: an approved-keys list where nothing
// ever matches denies every package, and the operator's likely next move is
// to switch the rule off.
//
// The length floor is the security-carrying half. approvedKeySet used to
// accept any non-empty string as an approved "key", so a list holding a short
// key ID — what `gpg --list-keys` prints by default, and the most natural
// thing for an operator to paste — became a matchable token. Short and long
// key IDs are the low 32 or 64 bits of the fingerprint and are cheap to
// collide on purpose, so an allow-list built from them is one an attacker can
// join by generating a key. Only a full fingerprint is an identity.
func normalizeFingerprint(s string) (string, bool) {
	// The 0x prefix goes first, before the hex scan: "x" is not a hex digit,
	// so a scan-then-strip order rejects "0xCAE2..." outright instead of
	// accepting the form operators most often copy out of a key-listing tool.
	s = strings.TrimSpace(s)
	if len(s) > 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s = s[2:]
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			continue
		case r >= '0' && r <= '9', r >= 'A' && r <= 'F':
			b.WriteRune(r)
		case r >= 'a' && r <= 'f':
			b.WriteRune(r - 'a' + 'A')
		default:
			return "", false
		}
	}
	out := b.String()
	if len(out) != 40 && len(out) != 64 {
		return "", false
	}
	return out, true
}
