package policy

import (
	"context"
	"reflect"
	"testing"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
)

func sel(name string, mods ...func(*resolve.Selection)) resolve.Selection {
	s := resolve.Selection{
		Name:                  name,
		Arch:                  "amd64",
		Version:               "1.0",
		PublisherVerification: lock.VerifiedAPTSigned,
		Reason:                lock.ReasonRequested,
	}
	for _, m := range mods {
		m(&s)
	}
	return s
}

func withComponent(c string) func(*resolve.Selection) {
	return func(s *resolve.Selection) { s.Origin.Component = c }
}
func withFlags(f ...string) func(*resolve.Selection) {
	return func(s *resolve.Selection) { s.Flags = f }
}
func withVerification(v lock.PublisherVerification) func(*resolve.Selection) {
	return func(s *resolve.Selection) { s.PublisherVerification = v }
}
func withReasonExternal(uri string) func(*resolve.Selection) {
	return func(s *resolve.Selection) { s.Reason = lock.ReasonExternal; s.URI = uri }
}
func withFingerprint(fp string) func(*resolve.Selection) {
	return func(s *resolve.Selection) { s.Origin.KeyFingerprint = fp }
}

func rulesOf(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Rule)
	}
	return out
}

func TestEvaluate_EveryRule(t *testing.T) {
	cases := []struct {
		name      string
		policy    Policy
		input     Input
		wantRules map[string]int // rule -> expected finding count
		wantNone  bool
	}{
		{
			name:   "empty policy finds nothing",
			policy: Policy{},
			input: Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
				sel("vlc", withComponent("multiverse")),
			}}},
			wantNone: true,
		},
		{
			name:   "deny-components flags a denied component",
			policy: Policy{DenyComponents: []string{"multiverse", "non-free"}},
			input: Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
				sel("vlc", withComponent("multiverse")),
				sel("bash", withComponent("main")),
			}}},
			wantRules: map[string]int{"component-deny": 1},
		},
		{
			name:   "allow-components flags anything outside the list",
			policy: Policy{AllowComponents: []string{"main"}},
			input: Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
				sel("vlc", withComponent("universe")),
				sel("bash", withComponent("main")),
			}}},
			wantRules: map[string]int{"component-allow": 1},
		},
		{
			name:   "deny-packages matches a glob",
			policy: Policy{DenyPackages: []string{"lib*-dbg"}},
			input: Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
				sel("libfoo-dbg"),
				sel("bash"),
			}}},
			wantRules: map[string]int{"package-deny": 1},
		},
		{
			name:   "allow-packages requires a glob match",
			policy: Policy{AllowPackages: []string{"lib*"}},
			input: Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
				sel("libfoo"),
				sel("bash"),
			}}},
			wantRules: map[string]int{"package-allow": 1},
		},
		{
			name:   "require-signed-publisher flags url-unverified only",
			policy: Policy{RequireSignedPublisher: true},
			input: Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
				sel("vendor-tool", withVerification(lock.VerifiedURLUnverified)),
				sel("vendor-tool2", withVerification(lock.VerifiedUserDigest)),
				sel("archive-pkg", withVerification(lock.VerifiedAPTSigned)),
			}}},
			wantRules: map[string]int{"require-signed-publisher": 1},
		},
		{
			name:   "allow-url-inputs=false flags external URL packages",
			policy: Policy{AllowURLInputs: boolPtr(false)},
			input: Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
				sel("vendor-tool", withReasonExternal("https://vendor.example/tool.deb")),
				sel("local-tool", withReasonExternal("")), // local file input, no URL
				sel("bash"),
			}}},
			wantRules: map[string]int{"allow-url-inputs": 1},
		},
		{
			name:   "allow-url-inputs defaults to permitted when nil",
			policy: Policy{AllowURLInputs: nil},
			input: Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
				sel("vendor-tool", withReasonExternal("https://vendor.example/tool.deb")),
			}}},
			wantNone: true,
		},
		{
			name:   "deny-flags matches any carried flag",
			policy: Policy{DenyFlags: []string{lock.FlagNetworkPostinst, lock.FlagDKMS}},
			input: Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
				sel("curly", withFlags(lock.FlagNetworkPostinst)),
				sel("clean-pkg", withFlags(lock.FlagSnapShim)),
			}}},
			wantRules: map[string]int{"deny-flags": 1},
		},
		{
			name:   "approved-keys: no constraint when the list is empty",
			policy: Policy{},
			input: Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
				sel("bash"), // no fingerprint at all
			}}},
			wantNone: true,
		},
		{
			name:   "approved-keys: missing fingerprint and unapproved fingerprint both fire",
			policy: Policy{ApprovedKeys: []string{"AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555"}},
			input: Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
				sel("no-fp"),
				sel("bad-fp", withFingerprint("0000000000000000000000000000000000FFFF")),
				sel("good-fp", withFingerprint("aaaa1111bbbb2222cccc3333dddd4444eeee5555")), // lowercase, must still match
			}}},
			wantRules: map[string]int{"approved-keys": 2},
		},
		{
			name:   "approved-keys: Input.ApprovedKeys and Policy.ApprovedKeys union",
			policy: Policy{ApprovedKeys: []string{"AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555"}},
			input: Input{
				ApprovedKeys: []string{"1111222233334444555566667777888899990000"},
				Plan: &resolve.Plan{Selections: []resolve.Selection{
					sel("from-policy-list", withFingerprint("AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555")),
					sel("from-input-list", withFingerprint("1111222233334444555566667777888899990000")),
					sel("from-neither", withFingerprint("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF")),
				}},
			},
			wantRules: map[string]int{"approved-keys": 1},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, err := loadFromPolicy(c.policy)
			if err != nil {
				t.Fatalf("build evaluator: %v", err)
			}
			findings, err := ev.Evaluate(context.Background(), c.input)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if c.wantNone {
				if len(findings) != 0 {
					t.Fatalf("want no findings, got %d: %+v", len(findings), findings)
				}
				return
			}
			got := map[string]int{}
			for _, r := range rulesOf(findings) {
				got[r]++
			}
			if !reflect.DeepEqual(got, c.wantRules) {
				t.Fatalf("finding counts by rule = %v, want %v\nfull findings: %+v", got, c.wantRules, findings)
			}
		})
	}
}

func TestEvaluate_DefaultSeverity(t *testing.T) {
	ev, err := loadFromPolicy(Policy{DenyPackages: []string{"bad*"}})
	if err != nil {
		t.Fatal(err)
	}
	findings, err := ev.Evaluate(context.Background(), Input{Plan: &resolve.Plan{Selections: []resolve.Selection{sel("badtool")}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Severity != SeverityWarn {
		t.Fatalf("default severity should be warn, got %+v", findings)
	}

	ev2, err := loadFromPolicy(Policy{DenyPackages: []string{"bad*"}, DefaultSeverity: SeverityDeny})
	if err != nil {
		t.Fatal(err)
	}
	findings2, err := ev2.Evaluate(context.Background(), Input{Plan: &resolve.Plan{Selections: []resolve.Selection{sel("badtool")}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings2) != 1 || findings2[0].Severity != SeverityDeny {
		t.Fatalf("explicit default_severity=deny should propagate, got %+v", findings2)
	}
	if !Deniable(findings2) {
		t.Fatal("Deniable(findings2) should be true")
	}
	if Deniable(findings) {
		t.Fatal("Deniable(findings) (warn only) should be false")
	}
}

// TestEvaluate_Deterministic proves repeated evaluation of the same input
// produces byte-for-byte the same finding order, and that the order does not
// depend on the order Selections were given in (resolve.SortSelections order
// is authoritative).
func TestEvaluate_Deterministic(t *testing.T) {
	forward := []resolve.Selection{
		sel("alpha", withComponent("multiverse")),
		sel("mid", withComponent("multiverse")),
		sel("zeta", withComponent("multiverse")),
	}
	reversed := []resolve.Selection{forward[2], forward[1], forward[0]}

	ev, err := loadFromPolicy(Policy{DenyComponents: []string{"multiverse"}})
	if err != nil {
		t.Fatal(err)
	}

	var first []Finding
	for i := 0; i < 4; i++ {
		selections := forward
		if i%2 == 1 {
			selections = reversed
		}
		findings, err := ev.Evaluate(context.Background(), Input{Plan: &resolve.Plan{Selections: selections}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = findings
			continue
		}
		if !reflect.DeepEqual(first, findings) {
			t.Fatalf("run %d produced a different result:\n first: %+v\n this: %+v", i, first, findings)
		}
	}
	names := rulesOf(first)
	if len(names) != 3 {
		t.Fatalf("want 3 findings, got %d", len(names))
	}
}

// TestEvaluate_DoesNotMutateInput proves Evaluate does not sort the caller's
// Plan.Selections slice in place.
func TestEvaluate_DoesNotMutateInput(t *testing.T) {
	selections := []resolve.Selection{sel("zeta"), sel("alpha")}
	plan := &resolve.Plan{Selections: selections}
	ev, err := loadFromPolicy(Policy{DenyPackages: []string{"nonexistent"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ev.Evaluate(context.Background(), Input{Plan: plan}); err != nil {
		t.Fatal(err)
	}
	if plan.Selections[0].Name != "zeta" || plan.Selections[1].Name != "alpha" {
		t.Fatalf("Evaluate mutated the caller's Selections order: %+v", plan.Selections)
	}
}

func TestEvaluate_NilPlanOrEmptyPolicy(t *testing.T) {
	ev, err := loadFromPolicy(Policy{DenyComponents: []string{"multiverse"}})
	if err != nil {
		t.Fatal(err)
	}
	findings, err := ev.Evaluate(context.Background(), Input{Plan: nil})
	if err != nil || findings != nil {
		t.Fatalf("nil Plan should produce (nil, nil), got (%+v, %v)", findings, err)
	}

	findings, err = Empty().Evaluate(context.Background(), Input{Plan: &resolve.Plan{Selections: []resolve.Selection{sel("x", withComponent("multiverse"))}}})
	if err != nil || findings != nil {
		t.Fatalf("Empty() should always find nothing, got (%+v, %v)", findings, err)
	}
}

func TestGlobMatchesAny(t *testing.T) {
	cases := []struct {
		patterns []string
		name     string
		want     bool
	}{
		{[]string{"lib*"}, "libfoo", true},
		{[]string{"lib*"}, "notlib", false},
		{[]string{"*-dbg"}, "foo-dbg", true},
		{[]string{"linux-image-*"}, "linux-image-amd64", true},
		{[]string{"[a-c]*"}, "bash", true},
		{[]string{"[a-c]*"}, "zsh", false},
		{[]string{"["}, "anything", false}, // malformed pattern: no match, no panic
	}
	for _, c := range cases {
		if got := globMatchesAny(c.patterns, c.name); got != c.want {
			t.Errorf("globMatchesAny(%v, %q) = %v, want %v", c.patterns, c.name, got, c.want)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

// loadFromPolicy builds an Evaluator directly from an in-memory Policy value,
// bypassing Load/file parsing, since most of these tests want to exercise
// Evaluate's rule logic, not the file format.
func loadFromPolicy(p Policy) (Evaluator, error) {
	cp := p
	return evaluator{p: &cp}, nil
}
