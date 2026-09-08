package export

// Free space at one path, measured directly.
//
// # Why this exists, and why ListVolumes was the wrong answer
//
// Plan refuses an export that will not fit, and downgrades that refusal to a
// warning when DestFreeBytes is FreeSpaceUnknown — "a copy that might fail is
// still better than refusing to try". So how the caller obtains the number
// decides whether the refusal ever fires.
//
// internal/app derived it by matching the destination against ListVolumes()
// by longest mount-point prefix, and that is wrong in a way its own comment
// says it was written to prevent. ListVolumes is a DRIVE CHOOSER, not a
// filesystem table: selectMounts deliberately drops filesystems that are not
// destinations, so a path on one of them matches nothing, free space comes
// back unknown, and the hard "this will not fit" becomes a soft "could not be
// measured". The operator then starts a copy that cannot finish and finds out
// partway through, on the drive they were about to carry to a machine with no
// network.
//
// The gap is not exotic. Wherever "/" is rejected, nothing covers /tmp/... or
// a path under the operator's home directory:
//
//   - In any container, ListVolumes returns only the bind mounts. Everything
//     else — including the whole root filesystem — is unknown.
//   - A live-USB session and an overlayroot install reject "/" by the same
//     pseudo-filesystem rule, on a real desktop, with no container anywhere.
//
// The fix is to ask the destination rather than look it up in a list of
// something else. That is also simply more correct: the chooser's number
// describes a mount point, and a bind mount, a subvolume or a nested mount
// under it can have entirely different free space.
//
// # The budget is not optional
//
// statfs(2) on a hung NFS or SMB mount blocks in uninterruptible sleep for as
// long as the server's timeout, which is minutes; GetDiskFreeSpaceEx on a
// disconnected Windows network drive behaves the same way. PlanExport is a
// bound method and must not block for longer than a frame.
//
// So the syscall runs in its own goroutine under freeSpaceBudget, exactly as
// enumeration's do under statfsBudget, and a destination that misses it has no
// answer:
// FreeSpaceUnknown, and Plan warns instead of refusing. That is the honest
// outcome rather than a defeat — nothing was measured, so nothing may be
// claimed — and it is the same treatment an unresponsive volume already gets
// in the chooser. The abandoned goroutine writes into a buffered channel
// nobody reads, cannot block, and exits when the kernel releases it.

import (
	"math"
	"os"
	"path/filepath"
	"time"
)

// freeSpaceBudget bounds one measurement.
//
// It is the same 1500 ms enumeration allows each of its statfs calls. The
// number is spelled out here rather than taken from statfsBudget because that
// constant is Linux-only and this file is not, and it is spelled out rather
// than picked afresh because one measurement should not be allowed to take
// longer than the whole chooser does. TestFreeSpaceBudgetMatchesEnumeration
// fails on Linux if the two ever drift.
const freeSpaceBudget = 1500 * time.Millisecond

// maxInt64 is the ceiling an unsigned byte count is clamped to on its way
// into an int64.
//
// A free-space figure above this cannot come off a real filesystem, and one
// that did would arrive as a NEGATIVE int64 — which reads as FreeSpaceUnknown
// and downgrades the refusal to a warning, the exact failure this whole file
// exists to prevent. So it is clamped rather than converted straight, and the
// clamp is a branch that RETURNS rather than an assignment, so the conversion
// below is provably in range at the point it happens.
const maxInt64 = uint64(math.MaxInt64)

// FreeSpaceAt returns the bytes available to an unprivileged writer on the
// filesystem that will hold path, for Request.DestFreeBytes.
//
// path need not exist. The nearest ancestor that does is measured instead,
// which is the normal case rather than an edge one: an export's DestDir is a
// subdirectory that has not been created yet, and it will be created on its
// parent's filesystem.
//
// Returns FreeSpaceUnknown when nothing could be measured — the platform has
// no implementation, no ancestor exists, the syscall failed, or it did not
// answer within freeSpaceBudget. It never returns a negative value for any
// other reason, and never blocks longer than the budget.
//
// The number is what a normal user may actually write. On Linux that is
// f_bavail rather than f_bfree, so the root-reserved blocks — 5% of a default
// ext4 — are not promised to a copy that cannot have them.
func FreeSpaceAt(path string) int64 {
	base := nearestExisting(path)
	if base == "" {
		return FreeSpaceUnknown
	}

	// The ok flag travels with the number because the two zero cases are not
	// the same answer and must not collapse into one. A measured zero is a
	// FULL filesystem and Plan refuses it; a failed syscall measured nothing
	// and Plan must warn instead. Sending a bare 0 for both would turn every
	// unmeasurable destination into a hard refusal — the opposite defect to
	// the one this file fixes, and just as wrong.
	//
	// Buffered so a goroutine that outlives the budget can still finish and
	// exit rather than blocking on a send nobody will receive.
	type answer struct {
		bytes uint64
		ok    bool
	}
	ch := make(chan answer, 1)
	go func(p string) {
		n, ok := freeSpaceOne(p)
		ch <- answer{bytes: n, ok: ok}
	}(base)

	timer := time.NewTimer(freeSpaceBudget)
	defer timer.Stop()

	select {
	case a := <-ch:
		if !a.ok {
			return FreeSpaceUnknown
		}
		if a.bytes > maxInt64 {
			return math.MaxInt64
		}
		return int64(a.bytes) //nolint:gosec // G115: the branch above returns for everything above MaxInt64, so this conversion cannot be negative -- and a negative would read as FreeSpaceUnknown and downgrade the refusal, which is the defect this file fixes
	case <-timer.C:
		return FreeSpaceUnknown
	}
}

// nearestExisting walks up from path to the first component that exists,
// returning "" if nothing on the way to the root does.
//
// A destination whose parent does not exist is refused by checkDestination
// anyway, so this loop normally takes one or two steps. It is written as a
// loop rather than "just use the parent" because a caller is entitled to pass
// a DestDir several levels below anything that exists, and answering
// FreeSpaceUnknown for that would put us back where we started.
//
// It terminates on every platform: filepath.Dir is idempotent at a filesystem
// root ("/" on Unix, `C:\` on Windows), so the loop stops when the path stops
// changing rather than assuming a particular root shape.
func nearestExisting(path string) string {
	p := filepath.Clean(path)
	if p == "" || p == "." {
		abs, err := filepath.Abs(".")
		if err != nil {
			return ""
		}
		p = abs
	}
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
		p = parent
	}
}
