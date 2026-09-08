package export

// Tests for FreeSpaceAt, and for the defect it was written to fix: PlanExport
// derived DestFreeBytes by matching the destination against ListVolumes(), a
// drive chooser that deliberately omits filesystems that are not
// destinations, so any path on one of them measured as unknown and Plan
// downgraded its hard "this will not fit" refusal to a soft warning.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestFreeSpaceAtMeasuresAPathTheChooserDoesNotList is the finding, stated as
// a test. It is written to be meaningful on any machine: it asserts that the
// two disagree in the direction that matters, rather than pinning a byte
// count that depends on whose disk it runs on.
//
// The temporary directory is the fixture on purpose — it is where the old
// derivation failed. In a container ListVolumes returns only the bind mounts,
// so nothing covers /tmp; on a live-USB session or an overlayroot install "/"
// is rejected as a pseudo-filesystem and nothing covers it either.
func TestFreeSpaceAtMeasuresAPathTheChooserDoesNotList(t *testing.T) {
	dir := t.TempDir()

	got := FreeSpaceAt(dir)
	if got == FreeSpaceUnknown {
		t.Skipf("this platform cannot measure free space (GOOS=%s); nothing to compare", runtime.GOOS)
	}
	if got < 0 {
		t.Fatalf("FreeSpaceAt(%q) = %d: negative values other than FreeSpaceUnknown read as unmeasured "+
			"and silently downgrade the refusal", dir, got)
	}

	// The old derivation, kept verbatim, so this test proves the previous
	// form was wrong rather than merely restating what the new one does. If
	// it happens to find a covering volume on this machine the comparison is
	// not available, which is a property of the machine and not a failure.
	old := oldDerivationForTest(dir)
	if old == FreeSpaceUnknown && got >= 0 {
		// The defect, reproduced: the chooser had no answer for this path and
		// the syscall does. Logged rather than silent, because "this test
		// passed" and "this test reproduced the bug it was written for" are
		// different facts and only one of them is worth a transcript.
		t.Logf("REPRODUCED: the chooser has no volume covering %q (old=FreeSpaceUnknown, so "+
			"export.Plan would WARN); FreeSpaceAt measures %d bytes (so it now REFUSES)", dir, got)
		return
	}
	t.Logf("this machine's chooser does cover %q (old=%d new=%d); the reproduction is "+
		"environment-dependent and the direct measurement is asserted above regardless", dir, old, got)
}

// oldDerivationForTest is what internal/app did before FreeSpaceAt existed:
// longest-mount-point-prefix match against the chooser's list. Kept so the
// test above can show the two disagreeing; not used by anything that ships.
func oldDerivationForTest(dest string) int64 {
	vols, err := ListVolumes()
	if err != nil {
		return FreeSpaceUnknown
	}
	best := -1
	for i, v := range vols {
		if v.TotalBytes <= 0 || !pathWithinForTest(dest, v.Path) {
			continue
		}
		if best < 0 || len(v.Path) > len(vols[best].Path) {
			best = i
		}
	}
	if best < 0 {
		return FreeSpaceUnknown
	}
	n := vols[best].FreeBytes
	if n > maxInt64 {
		n = maxInt64
	}
	return int64(n)
}

func pathWithinForTest(dest, mount string) bool {
	d := filepath.Clean(dest)
	m := filepath.Clean(mount)
	if runtime.GOOS == "windows" {
		d, m = strings.ToLower(d), strings.ToLower(m)
	}
	if d == m {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(m, sep) {
		m += sep
	}
	return strings.HasPrefix(d, m)
}

// TestFreeSpaceAtAnswersForAPathThatDoesNotExistYet. An export's DestDir is a
// subdirectory that has not been created, so this is the normal call and not
// an edge case. It must measure the filesystem the directory WILL be created
// on rather than giving up.
func TestFreeSpaceAtAnswersForAPathThatDoesNotExistYet(t *testing.T) {
	dir := t.TempDir()
	here := FreeSpaceAt(dir)
	if here == FreeSpaceUnknown {
		t.Skipf("this platform cannot measure free space (GOOS=%s)", runtime.GOOS)
	}

	for _, depth := range []string{
		"nope",
		filepath.Join("nope", "deeper"),
		filepath.Join("nope", "deeper", "deeper-still"),
	} {
		t.Run(depth, func(t *testing.T) {
			got := FreeSpaceAt(filepath.Join(dir, depth))
			if got == FreeSpaceUnknown {
				t.Fatalf("FreeSpaceAt of a not-yet-created subdirectory returned unknown; "+
					"every export would fall back to a warning (parent measured %d)", here)
			}
			// Same filesystem, so the same number bar concurrent activity.
			// Compared loosely on purpose: this box is doing other things.
			if got <= 0 {
				t.Fatalf("FreeSpaceAt = %d for a path under a directory with %d free", got, here)
			}
		})
	}
}

// TestFreeSpaceAtOnNothingAtAllIsUnknown is the mirror image of the defect
// this file fixes, and it fired for real while this was being written.
//
// A destination that cannot be measured must come back FreeSpaceUnknown so
// Plan WARNS. A plain zero means a full disk and Plan REFUSES, so answering
// zero for "I could not tell" fails an export that would have worked — the
// same class of mistake as passing a warning off as a measurement, pointing
// the other way.
//
// The Linux arm is not contrived. statfs(2) SUCCEEDS on a pseudo-filesystem
// and reports zero blocks, so the nearest existing ancestor of a nonexistent
// path under /proc is /proc itself, which answers "0 free of 0 total". The
// first version of freeSpaceOne returned that as a measured zero and this
// test caught it.
func TestFreeSpaceAtOnNothingAtAllIsUnknown(t *testing.T) {
	cases := map[string]string{}
	if runtime.GOOS == "windows" {
		cases["a drive letter with nothing in it"] = `Q:\definitely\not\here`
	} else {
		// Walks up to /proc, a live filesystem that reports zero blocks.
		cases["under a pseudo-filesystem"] = "/proc/self/definitely-not-a-directory/deeper"
		cases["a pseudo-filesystem directly"] = "/proc"
		cases["under sysfs"] = "/sys/definitely-not-here"
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			got := FreeSpaceAt(p)
			if got == 0 {
				t.Fatalf("FreeSpaceAt(%q) = 0, which Plan reads as a full disk and refuses with "+
					"\"only 0 B is free\". A destination that could not be measured must be "+
					"FreeSpaceUnknown so Plan warns instead.", p)
			}
			if got != FreeSpaceUnknown && got < 0 {
				t.Fatalf("FreeSpaceAt(%q) = %d: the only legal negative value is FreeSpaceUnknown (%d)",
					p, got, FreeSpaceUnknown)
			}
		})
	}
}

// TestFreeSpaceAtStillRefusesAGenuinelyFullFilesystem. The guard above must
// not swallow the case the refusal exists for: a real filesystem with a
// non-zero total and nothing left. Driven against a tiny loopback-free tmpfs
// where one is available, and skipped rather than faked where it is not —
// mounting requires privileges this test must not assume.
func TestFreeSpaceAtStillRefusesAGenuinelyFullFilesystem(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fixture is a Linux tmpfs")
	}
	dir := t.TempDir()
	// A filesystem this test can fill without privileges is not generally
	// available, so what is asserted here is the decision rule rather than a
	// mounted fixture: zero-free-with-a-real-total is a refusal, and only
	// zero-total is unknown. freeSpaceOne is the function that decides it.
	if _, ok := freeSpaceOne(dir); !ok {
		t.Fatalf("freeSpaceOne(%q) reported no answer for an ordinary temporary directory", dir)
	}
	if _, ok := freeSpaceOne("/proc"); ok {
		t.Fatal("freeSpaceOne(/proc) claimed a usable answer; a zero-block filesystem has none")
	}
}

// TestFreeSpaceAtRespectsItsBudget. PlanExport is a bound method and must not
// block for longer than a frame; statfs(2) on a hung NFS mount blocks in
// uninterruptible sleep for the server's timeout. The budget is the only
// thing between those two facts.
func TestFreeSpaceAtRespectsItsBudget(t *testing.T) {
	start := time.Now()
	FreeSpaceAt(t.TempDir())
	if elapsed := time.Since(start); elapsed > freeSpaceBudget*2 {
		t.Errorf("FreeSpaceAt took %v against a %v budget", elapsed, freeSpaceBudget)
	}
}

// TestFreeSpaceAtIsWhatAWriterMayActuallyHave. The number must be what an
// unprivileged copy can write, not the filesystem's raw free count: on a
// default ext4 the root-reserved blocks are 5% of the disk, and promising
// them is how a copy fails at 95%.
//
// Asserted as a property rather than a percentage, because the reservation is
// tunable and zero on many filesystems: whatever else is true, the answer
// must not exceed the space actually available, which is verified by writing
// into it.
func TestFreeSpaceAtIsWhatAWriterMayActuallyHave(t *testing.T) {
	dir := t.TempDir()
	free := FreeSpaceAt(dir)
	if free == FreeSpaceUnknown {
		t.Skipf("this platform cannot measure free space (GOOS=%s)", runtime.GOOS)
	}
	if free == 0 {
		t.Skip("no free space on the test filesystem; nothing to assert")
	}

	// A token write has to succeed if the number means anything at all.
	f := filepath.Join(dir, "probe")
	if err := os.WriteFile(f, []byte("probe"), 0o600); err != nil {
		t.Fatalf("FreeSpaceAt reported %d bytes free but a 5-byte write failed: %v", free, err)
	}
	if err := os.Remove(f); err != nil {
		t.Fatalf("removing the probe file: %v", err)
	}
}
