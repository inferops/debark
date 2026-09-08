package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/policy"
)

// approvedKeysCarrier is an OPTIONAL interface a policy.Evaluator may
// implement to report the inline approved_keys list its policy file declared.
//
// It is declared here, in the consumer, rather than in core/policy, for the
// usual Go reason: the engine is what needs the list, and an evaluator that
// has nothing to report simply does not implement the method. That also makes
// this work for a caller-supplied Deps.Policy — an embedder with its own
// evaluator gets the same equivalence without core/policy having to know it
// exists.
//
// STATUS, stated plainly so nobody reads this as done: core/policy's own
// evaluator does NOT implement this today. policy.Load returns the
// unexported evaluator{p: *Policy}, and the package exports no way to read
// the parsed Policy back, so an inline approved_keys list is still invisible
// to the engine and still reaches only the evaluator's own weak check. The
// missing half is one additive method in core/policy — a package this package
// does not own and must not edit:
//
//	func (e evaluator) ApprovedKeys() []string { return e.p.ApprovedKeys }
//
// With that in place the union below starts working with no further change
// here, and the equivalence policy.Policy.ApprovedKeys promises becomes true.
type approvedKeysCarrier interface {
	ApprovedKeys() []string
}

// inlineApprovedKeys reads the evaluator's inline list when it has one.
func inlineApprovedKeys(ev policy.Evaluator) []string {
	c, ok := ev.(approvedKeysCarrier)
	if !ok {
		return nil
	}
	return c.ApprovedKeys()
}

// unionApprovedKeys merges the --approved-keys list with a policy file's
// inline one, deduplicated and sorted. With nothing to merge it returns the
// flag's list untouched, so the single-source case behaves exactly as before.
//
// UNION, not intersection, and this is the one place in the engine where the
// permissive-looking choice is the safe one:
//
//   - The two sources are documented as two spellings of the same thing
//     ("equivalent to --approved-keys"), not as two independent filters to be
//     satisfied at once. An operator who lists their organisation's archive
//     key on the command line and a vendor's key in the project policy file
//     means "both of these are approved", which is exactly the union.
//   - Intersection FAILS OPEN, catastrophically, in the ordinary case. Two
//     disjoint lists intersect to the empty list, and an empty
//     Input.ApprovedKeys means "no constraint" (policy.Input.ApprovedKeys'
//     own doc, and core/apt's checkApprovedKeys skips entirely on an empty
//     list). The stricter-sounding rule would silently switch the whole key
//     check off precisely when the operator had configured it twice. That
//     asymmetry is decisive on its own.
//
// What the union does cost is honest and worth stating: adding a key can only
// ever let MORE archive sources pass checkApprovedKeys, so an operator who
// wants a narrower list must narrow it in both places. That is a visible,
// legible property of "these two settings mean the same thing"; a rule that
// could silently disarm itself is not.
//
// Sorted and deduplicated because this value reaches apt's private-root
// construction and, through it, values recorded in the lock: two runs of the
// same request must not differ by the order two lists happened to be
// concatenated in. Both loaders already normalise fingerprints to uppercase
// hex (policy.LoadApprovedKeys), so plain string equality is the right
// identity here.
func unionApprovedKeys(fromFlag, fromPolicy []string) []string {
	if len(fromPolicy) == 0 {
		return fromFlag
	}
	if len(fromFlag) == 0 {
		return sortedStrings(dedupeStrings(fromPolicy))
	}
	merged := make([]string, 0, len(fromFlag)+len(fromPolicy))
	merged = append(merged, fromFlag...)
	merged = append(merged, fromPolicy...)
	return sortedStrings(dedupeStrings(merged))
}

// evaluatePolicy runs the configured Evaluator over the resolved plan. A
// SeverityDeny finding fails the build with dferr.Policy (exit 6) before
// anything is written to Output.Path. Non-deny findings are recorded as
// structured lock warnings, folded in when the initial lock is built.
func (b *build) evaluatePolicy(ctx context.Context) error {
	findings, err := b.pol.Evaluate(ctx, policy.Input{
		Snapshot:     b.snap.Snapshot,
		Plan:         b.plan,
		ApprovedKeys: b.approvedKeys,
	})
	if err != nil {
		return classify(err, dferr.Policy, "engine: evaluate policy")
	}

	var denyMsgs []string
	for _, f := range findings {
		b.emit(evidence.TypePolicyFinding, f.Message, map[string]any{
			"rule": f.Rule, "severity": string(f.Severity), "packages": f.Packages,
		})
		if f.Severity == policy.SeverityDeny {
			denyMsgs = append(denyMsgs, fmt.Sprintf("%s: %s", f.Rule, f.Message))
			continue
		}
		if f.Severity != policy.SeverityWarn {
			continue // info findings are recorded in evidence.json only.
		}
		b.policyWarnings = append(b.policyWarnings, lock.Warning{
			Code:     "policy." + f.Rule,
			Message:  f.Message,
			Packages: f.Packages,
		})
	}
	if len(denyMsgs) > 0 {
		return dferr.New(dferr.Policy, "engine: policy denied: %s", strings.Join(denyMsgs, "; "))
	}
	return nil
}
