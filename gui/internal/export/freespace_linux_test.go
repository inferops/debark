//go:build linux

package export

import "testing"

// TestFreeSpaceBudgetMatchesEnumeration. freeSpaceBudget is spelled out as a
// literal in freespace.go because that file is portable and statfsBudget is
// Linux-only. This is what stops the two drifting: one measurement must not
// be allowed to take longer than the whole chooser allows for each of its
// own, and a change to either number should be a deliberate choice about
// both.
func TestFreeSpaceBudgetMatchesEnumeration(t *testing.T) {
	if freeSpaceBudget != statfsBudget {
		t.Fatalf("freeSpaceBudget is %v and statfsBudget is %v; they have drifted. "+
			"Decide which is right and change both, or say in freespace.go why one "+
			"measurement is allowed longer than the chooser allows each of its own.",
			freeSpaceBudget, statfsBudget)
	}
}

// TestFreeSpaceOneIsTheChoosersOwnReading. freeSpaceOne returns statfsOne's
// number rather than a second statfs call written beside it, so a plan-time
// refusal and the capacity shown in the chooser cannot disagree. A future
// edit that reimplemented it — and reached for f_bfree, or forgot the
// f_frsize/f_bsize unit — would show one number on screen and refuse against
// another.
func TestFreeSpaceOneIsTheChoosersOwnReading(t *testing.T) {
	const p = "/"
	c := statfsOne(p)
	if c.err != nil || !c.ok {
		t.Skipf("statfs(%q) did not answer: %v", p, c.err)
	}
	got, ok := freeSpaceOne(p)
	if !ok {
		t.Fatalf("freeSpaceOne(%q) reported no answer while statfsOne did", p)
	}
	if got != c.free {
		t.Fatalf("freeSpaceOne(%q) = %d but the chooser reads %d from the same syscall; "+
			"the two are no longer one reading", p, got, c.free)
	}
}
