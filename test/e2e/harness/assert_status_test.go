package harness

import "testing"

// TestDpkgFullyInstalled pins the predicate both package assertions share.
//
// The case that matters is "hold ok installed". Both assertions previously
// tested for the literal "install ok installed", which folded dpkg's DESIRED
// field into a question that is only about the last two fields. A held package
// is installed and usable; only someone's request that apt leave it alone
// makes its first field differ.
//
// It stayed invisible while the harness installed onto a stock image where
// nothing had ever been held, so held-package asserted against a plain fresh
// install and never exercised the hold. It surfaced the moment the fresh
// target became the machine the fixture actually declares.
//
// The absent direction is the one with teeth: under the old test a held,
// installed package satisfied AssertPackagesAbsent, so a fixture proving a
// package was left out of a bundle would have passed with it installed.
func TestDpkgFullyInstalled(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
		why    string
	}{
		{"install ok installed", true, "the ordinary case"},
		{"hold ok installed", true, "held is still installed — the regression this exists for"},
		{"  install ok installed\n", true, "dpkg-query output arrives with surrounding whitespace"},
		{"deinstall ok installed", false, "installed but marked for removal is mid-transaction, not present"},
		{"purge ok installed", false, "as above"},
		{"install ok unpacked", false, "unpacked is not configured"},
		{"install ok half-configured", false, "a failed configure must not read as present"},
		{"install reinstreq installed", false, "the error flag is not ok"},
		{"deinstall ok config-files", false, "removed, config files left behind"},
		{"unknown ok not-installed", false, "never installed"},
		{"", false, "no answer at all"},
		{"install ok", false, "a truncated answer must not be generous"},
	} {
		if got := dpkgFullyInstalled(tc.status); got != tc.want {
			t.Errorf("dpkgFullyInstalled(%q) = %v, want %v — %s", tc.status, got, tc.want, tc.why)
		}
	}
}
