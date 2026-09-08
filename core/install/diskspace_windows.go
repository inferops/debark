//go:build windows

package install

import (
	"syscall"
	"unsafe"
)

// GetDiskFreeSpaceExW via the stdlib syscall package's DLL-loading
// facilities — no new module dependency, just the standard library reaching
// one function further into kernel32.dll than its typed wrappers happen to
// go. install never installs anything on Windows, but the unit test
// suite runs here, and this makes the disk-space precondition exercisable
// end to end on the development machine rather than only on Linux.
var (
	modkernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceExW = modkernel32.NewProc("GetDiskFreeSpaceExW")
)

// diskFree returns the bytes available to the caller on the volume holding
// path, and whether the query succeeded.
func diskFree(path string) (uint64, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	var freeAvailable, totalBytes, totalFree uint64
	// The four uintptr(unsafe.Pointer(...)) conversions below are the ONLY way
	// to pass pointers to a LazyProc.Call, whose signature is ...uintptr, and
	// they are the shape the unsafe package documents as valid: "Conversion of
	// a Pointer to a uintptr when calling syscall.Syscall" (unsafe.Pointer
	// rule 4). syscall.(*LazyProc).Call carries //go:uintptrescapes precisely
	// so that this works -- the compiler keeps p, freeAvailable, totalBytes
	// and totalFree alive and unmoved for the duration of the call, which is
	// what makes the pointers valid on the callee side. The conversions are
	// written inline in the argument list, never stored in a variable first,
	// because storing one is what breaks the guarantee.
	//
	// gosec's G103 is an "audit this" rule, not a defect report: it flags
	// every use of unsafe so a human looks. This is that look, written down.
	// #nosec G103 -- documented syscall pointer-passing; see the note above
	ret, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ret == 0 {
		return 0, false
	}
	return freeAvailable, true
}
