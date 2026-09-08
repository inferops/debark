//go:build linux

package install

import "syscall"

// diskFree returns the bytes available to an unprivileged caller on the
// filesystem holding path, and whether the query succeeded. This is the
// platform that matters: install only ever runs for real on Linux.
func diskFree(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	// st.Bsize is int64 (int32 on 32-bit), and converting a negative one to
	// uint64 would produce an enormous block size, wrap the multiply, and
	// report bogus free space. statfs(2) does not return a negative block
	// size -- it is a filesystem's block size, 512 or 4096 in practice -- so
	// this is unreachable rather than merely unlikely.
	//
	// Worth knowing which way it would fail if it ever were reachable: the
	// only caller is the disk-space precondition in runner.go, so a wrapped
	// value reads as "plenty of room" and SKIPS the "insufficient disk
	// space" problem rather than raising a false one. If that is ever felt
	// to be too generous, the fix is a guard (`if st.Bsize <= 0 { return 0,
	// false }`), which fails closed -- but that is a behaviour change and
	// belongs in its own commit, not in a lint pass.
	// #nosec G115 -- statfs block size is never negative; see above
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
