package apt

import "testing"

// TestParseSimulateReadsRepeatedReasonBrackets pins a line shape that made
// `--upgrades` unusable on Ubuntu 24.04.
//
// apt appends one bracketed group per reason it has for touching a package,
// and instConfRE allowed at most one. A real `apt-get -s full-upgrade` emits
// two for libpam-modules-bin, so ParseSimulate could not read the line,
// recorded it in UnparsedActions, and the build refused with exit 5.
//
// The refusal itself was right and is deliberately left alone: an action line
// this package cannot read must never be skipped, because skipping it drops a
// package from the closure silently — "apt is the oracle; output whose
// meaning we do not know cannot certify a closed world". The defect was only
// that the parser could not read output apt legitimately produces.
//
// The line below is verbatim from the 2026-09-06 matrix run, not
// reconstructed, so the test keeps working against what apt really prints.
func TestParseSimulateReadsRepeatedReasonBrackets(t *testing.T) {
	const line = "Inst libpam-modules-bin [1.5.3-5ubuntu5.6] " +
		"(1.5.3-5ubuntu5.7 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64]) " +
		"[libpam-modules:amd64 on libpam-modules-bin:amd64] [libpam-modules:amd64 ]"

	res := ParseSimulate(Output{Stdout: []byte(line + "\n")})

	if len(res.UnparsedActions) != 0 {
		t.Fatalf("apt's own output was recorded as unreadable: %q", res.UnparsedActions)
	}
	if len(res.Actions) != 1 {
		t.Fatalf("Actions = %+v, want exactly one Inst action", res.Actions)
	}
	got := res.Actions[0]
	if got.Name != "libpam-modules-bin" {
		t.Errorf("Name = %q, want libpam-modules-bin", got.Name)
	}
	if got.Version != "1.5.3-5ubuntu5.7" {
		t.Errorf("Version = %q, want the NEW version 1.5.3-5ubuntu5.7, not the installed one", got.Version)
	}
	if got.Arch != "amd64" {
		t.Errorf("Arch = %q, want amd64", got.Arch)
	}

	// A single trailing group and none at all must keep working — the repeat
	// is a widening, and a regex change is an easy place to lose a case that
	// used to match.
	for _, l := range []string{
		"Inst tree (2.1.0-1 Debian:12/stable [amd64]) [something:amd64]",
		"Inst tree (2.1.0-1 Debian:12/stable [amd64])",
		"Conf tree (2.1.0-1 Debian:12/stable [amd64])",
	} {
		r := ParseSimulate(Output{Stdout: []byte(l + "\n")})
		if len(r.UnparsedActions) != 0 || len(r.Actions) != 1 {
			t.Errorf("%q: Actions=%+v Unparsed=%q, want one readable action", l, r.Actions, r.UnparsedActions)
		}
	}
}
