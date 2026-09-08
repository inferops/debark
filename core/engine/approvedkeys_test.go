package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/resolve"
)

// Two full v4 fingerprints (40 hex), which is the only form
// policy.LoadApprovedKeys accepts. Digits sort before letters, so the
// expected merged order is flagKey then inlineKey.
const (
	flagKey   = "1111111111111111111111111111111111111111"
	inlineKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// fakePolicyWithInlineKeys is an Evaluator that also carries an inline
// approved_keys list — the shape core/policy's own evaluator needs to grow
// one additive method to have (see approvedKeysCarrier in policy.go).
type fakePolicyWithInlineKeys struct {
	*fakePolicy
	keys []string
}

func (f *fakePolicyWithInlineKeys) ApprovedKeys() []string { return f.keys }

func writeApprovedKeysFile(t *testing.T, keys ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approved-keys.txt")
	var body string
	for _, k := range keys {
		body += k + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// captureResolveInput makes the fake backend record what it was asked, while
// still returning a usable plan.
func captureResolveInput(t *testing.T, h *harness, got *apt.ResolveInput) {
	t.Helper()
	h.backend.resolveFn = func(ctx context.Context, in apt.ResolveInput) (*resolve.Plan, error) {
		*got = in
		return fixturePlan(t), nil
	}
}

// TestBuild_InlineApprovedKeysReachAptNotJustPolicy pins the equivalence
// policy.Policy.ApprovedKeys promises: an inline approved_keys list is
// "equivalent to --approved-keys".
//
// It was not. --approved-keys reaches apt's real, keyring-derived per-source
// check (apt.ResolveInput.ApprovedKeys -> core/apt/root.go's
// checkApprovedKeys, exit 6) as well as the policy evaluator's much weaker
// check of what the plan claims about itself. An inline list reached only the
// evaluator, so an operator who configured keys in their policy file got
// half the control, silently.
//
// The assertion that matters is the FIRST one: the inline key must appear in
// what the backend was asked, because that is the side that was missing.
func TestBuild_InlineApprovedKeysReachAptNotJustPolicy(t *testing.T) {
	h := newHarness(t)
	var got apt.ResolveInput
	captureResolveInput(t, h, &got)

	h.req.Options.ApprovedKeysRef = writeApprovedKeysFile(t, flagKey)
	h.deps.Policy = &fakePolicyWithInlineKeys{fakePolicy: h.policyEval, keys: []string{inlineKey}}

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Build(context.Background(), h.req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	want := []string{flagKey, inlineKey}
	if len(got.ApprovedKeys) != len(want) {
		t.Fatalf("apt.ResolveInput.ApprovedKeys = %v, want %v", got.ApprovedKeys, want)
	}
	for i := range want {
		if got.ApprovedKeys[i] != want[i] {
			t.Fatalf("apt.ResolveInput.ApprovedKeys = %v, want %v (sorted union)", got.ApprovedKeys, want)
		}
	}

	// The evaluator keeps seeing the same merged list, so the two consumers
	// can never disagree about what is approved.
	seen := h.policyEval.gotInput.ApprovedKeys
	if len(seen) != len(want) {
		t.Fatalf("policy.Input.ApprovedKeys = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("policy.Input.ApprovedKeys = %v, want %v", seen, want)
		}
	}
}

// TestBuild_ApprovedKeysUnionIsDeduplicatedAndOrderStable guards the two
// properties the merged list has to have because it reaches apt's private
// root and, through it, values recorded in the lock: the same request built
// twice must produce the same list, and naming one key in both places must
// not name it twice.
func TestBuild_ApprovedKeysUnionIsDeduplicatedAndOrderStable(t *testing.T) {
	h := newHarness(t)
	var got apt.ResolveInput
	captureResolveInput(t, h, &got)

	// The same key in both sources, plus one only the policy file has, given
	// in an order that is not the sorted one.
	h.req.Options.ApprovedKeysRef = writeApprovedKeysFile(t, inlineKey)
	h.deps.Policy = &fakePolicyWithInlineKeys{
		fakePolicy: h.policyEval, keys: []string{inlineKey, flagKey},
	}

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Build(context.Background(), h.req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	want := []string{flagKey, inlineKey}
	if len(got.ApprovedKeys) != len(want) {
		t.Fatalf("ApprovedKeys = %v, want %v (deduplicated, sorted)", got.ApprovedKeys, want)
	}
	for i := range want {
		if got.ApprovedKeys[i] != want[i] {
			t.Fatalf("ApprovedKeys = %v, want %v (deduplicated, sorted)", got.ApprovedKeys, want)
		}
	}
}

// TestBuild_ApprovedKeysUnchangedWithoutAnInlineList is the regression half:
// an evaluator that carries no inline list — which is every evaluator
// core/policy builds today — must leave the --approved-keys list exactly as
// it was loaded. The union is an addition to the existing behaviour, not a
// rewrite of it.
func TestBuild_ApprovedKeysUnchangedWithoutAnInlineList(t *testing.T) {
	h := newHarness(t)
	var got apt.ResolveInput
	captureResolveInput(t, h, &got)

	h.req.Options.ApprovedKeysRef = writeApprovedKeysFile(t, inlineKey, flagKey)

	eng, err := New(h.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Build(context.Background(), h.req); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// As loaded: LoadApprovedKeys preserves file order, and nothing reorders
	// it when there is nothing to merge.
	want := []string{inlineKey, flagKey}
	if len(got.ApprovedKeys) != len(want) {
		t.Fatalf("ApprovedKeys = %v, want %v", got.ApprovedKeys, want)
	}
	for i := range want {
		if got.ApprovedKeys[i] != want[i] {
			t.Fatalf("ApprovedKeys = %v, want %v (unchanged when no inline list exists)", got.ApprovedKeys, want)
		}
	}
}
