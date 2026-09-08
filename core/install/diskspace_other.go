//go:build !linux && !darwin && !windows

package install

// diskFree is not implemented on this platform. install runs for real only
// on Linux; the disk-space precondition degrades to "could not
// determine" here rather than failing the build.
func diskFree(string) (uint64, bool) { return 0, false }
