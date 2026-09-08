//go:build windows

package export

// Windows volume enumeration.
//
// Windows is built but unsupported: the product ships on Ubuntu, and this
// exists so that the tree compiles and runs for developers on Windows
// machines. Correctness and not-panicking matter here; polish does not.
//
// The same safety boundary applies as everywhere else in this package. These
// are the informational Win32 calls a file manager makes —
// GetLogicalDriveStrings, GetDriveType, GetVolumeInformation,
// GetDiskFreeSpaceEx. Nothing opens \\.\PhysicalDrive, issues an
// IOCTL_DISK_* control code, or goes near diskpart. There is deliberately no
// WMI and no COM: the plain kernel32 entry points supply everything the
// chooser needs, and a COM dependency would be a new dependency to review.

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// On golang.org/x/sys: it is already in the module graph, pulled in by Wails,
// so importing it adds no new module and downloads nothing. It is preferred
// over hand-rolled syscall.NewLazyDLL wrappers because those require writing
// the uintptr and unsafe.Pointer marshalling by hand for every call, and this
// package would be doing that on the one platform that is not shipped and so
// gets the least scrutiny. x/sys's wrappers are generated and widely
// exercised.
//
// One consequence to be aware of: this is the repository's first direct use
// of golang.org/x/sys, so `go mod tidy` moves it out of the indirect block in
// go.mod. That is a truthful record of a real direct import rather than a new
// dependency, but go.mod is a frozen file, so the move belongs to whoever
// owns it. Nothing here depends on which block the requirement sits in.

func listVolumes() ([]Volume, error) {
	// Suppress the kernel's hard-error UI for the duration of enumeration.
	// Calling GetVolumeInformation on an empty card reader or a disconnected
	// network drive otherwise pops a modal "There is no disk in the drive"
	// box, in front of a GUI that never asked for one. Restored before
	// returning so the rest of the process is unaffected.
	prev := windows.SetErrorMode(windows.SEM_FAILCRITICALERRORS)
	defer windows.SetErrorMode(prev)

	roots, err := logicalDriveRoots()
	if err != nil {
		return nil, fmt.Errorf("export: enumerating logical drives: %w", err)
	}

	vols := make([]Volume, 0, len(roots))
	for _, root := range roots {
		v, ok := describeDrive(root)
		if !ok {
			continue
		}
		vols = append(vols, v)
	}
	return vols, nil
}

// logicalDriveRoots returns the mounted drive roots ("C:\\", "D:\\"). The API
// returns a double-NUL-terminated block of NUL-separated strings, and it is
// called twice: once to size the buffer, once to fill it.
func logicalDriveRoots() ([]string, error) {
	n, err := windows.GetLogicalDriveStrings(0, nil)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	// size, not len(buf): the length is already a uint32 here, so the
	// buffer and the API call cannot disagree about it and there is no
	// int in the middle to narrow.
	size := n + 1
	buf := make([]uint16, size)
	n, err = windows.GetLogicalDriveStrings(size, &buf[0])
	if err != nil {
		return nil, err
	}
	if n > size {
		n = size
	}
	return splitNulUTF16(buf[:n]), nil
}

// splitNulUTF16 splits a NUL-separated, double-NUL-terminated UTF-16 block.
// Kept separate and pure so that the parsing is unit-testable without a
// machine that happens to have the right drives attached.
func splitNulUTF16(buf []uint16) []string {
	var out []string
	start := 0
	for i, c := range buf {
		if c != 0 {
			continue
		}
		if i > start {
			out = append(out, windows.UTF16ToString(buf[start:i]))
		}
		start = i + 1
	}
	if start < len(buf) {
		if s := windows.UTF16ToString(buf[start:]); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func describeDrive(root string) (Volume, bool) {
	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return Volume{}, false
	}

	driveType := windows.GetDriveType(rootPtr)
	kind, keep := kindForDriveType(driveType)
	if !keep {
		return Volume{}, false
	}

	v := Volume{
		Path:      root,
		Device:    root,
		Kind:      kind,
		Removable: kind == KindRemovable,
	}

	// GetVolumeInformation fails on a drive letter with no media in it (an
	// empty card reader, an ejected optical drive). That is exactly the
	// "device vanished between listing and inspecting" case, and the answer
	// is to drop the row rather than show an unusable destination.
	var (
		nameBuf [windows.MAX_PATH + 1]uint16
		fsBuf   [windows.MAX_PATH + 1]uint16
		serial  uint32
		compLen uint32
		fsFlags uint32
	)
	if err := windows.GetVolumeInformation(rootPtr,
		&nameBuf[0], uint32(len(nameBuf)),
		&serial, &compLen, &fsFlags,
		&fsBuf[0], uint32(len(fsBuf))); err != nil {
		return Volume{}, false
	}
	v.Label = windows.UTF16ToString(nameBuf[:])
	v.FSType = windows.UTF16ToString(fsBuf[:])
	v.ReadOnly = fsFlags&windows.FILE_READ_ONLY_VOLUME != 0
	if v.Label == "" {
		v.Label = strings.TrimSuffix(root, `\`) // "D:"
	}

	// freeToCaller honours per-user quotas, so it is the number that predicts
	// whether the copy fits; totalFree is the volume-wide figure and is
	// deliberately discarded.
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(rootPtr, &freeToCaller, &total, &totalFree); err == nil {
		v.TotalBytes = total
		v.FreeBytes = freeToCaller
	}

	// There is no access(2) here. Windows effective permissions require
	// building and evaluating a security descriptor against the thread token,
	// which is far more machinery than an unsupported platform justifies, so
	// the cheap check is only the volume's read-only flag. ProbeWritable is
	// the honest answer and the copier calls it regardless of platform.
	v.Writable = !v.ReadOnly
	v.Note = noteFor(v.ReadOnly, v.Writable, v.TotalBytes > 0)
	return v, true
}

// kindForDriveType maps GetDriveType's result onto the chooser's classes and
// decides whether the drive is offered at all.
//
// The filter mirrors the Linux one in spirit: exclude what cannot hold a
// bundle, keep everything else.
//
//   - DRIVE_NO_ROOT_DIR and DRIVE_UNKNOWN: not a usable filesystem.
//   - DRIVE_CDROM: read-only by construction, the analogue of squashfs.
//   - DRIVE_RAMDISK: evaporates on reboot, the analogue of tmpfs.
//
// Known and accepted limitation: Windows reports external USB hard drives and
// SSDs as DRIVE_FIXED, so they are listed as fixed rather than removable.
// Linux gets this right via the bus check in deviceRemovable; matching it here
// would mean SetupAPI device enumeration, which is not worth it on a platform
// that is not shipped.
func kindForDriveType(t uint32) (VolumeKind, bool) {
	switch t {
	case windows.DRIVE_REMOVABLE:
		return KindRemovable, true
	case windows.DRIVE_FIXED:
		return KindFixed, true
	case windows.DRIVE_REMOTE:
		return KindNetwork, true
	default:
		// DRIVE_UNKNOWN, DRIVE_NO_ROOT_DIR, DRIVE_CDROM, DRIVE_RAMDISK.
		return KindUnknown, false
	}
}

// freeSpaceOne is FreeSpaceAt's Windows half.
//
// GetDiskFreeSpaceEx accepts any path on the volume, not only a drive root,
// so the destination is asked about directly. Its first out-parameter —
// lpFreeBytesAvailableToCaller — is the one that respects a disk quota, which
// is what the copy will actually be allowed to write; lpTotalNumberOfFreeBytes
// is the volume-wide figure and would over-promise under a quota. describeDrive
// makes the same choice for the chooser's FreeBytes.
//
// SetErrorMode for the same reason enumeration uses it: on an empty card
// reader or a disconnected network drive this call otherwise pops a modal
// "There is no disk in the drive" box in front of a GUI that never asked for
// one. Restored before returning.
//
// The caller runs this under freeSpaceBudget: a disconnected network drive
// blocks here exactly as a hung NFS mount blocks statfs(2) on Linux.
func freeSpaceOne(p string) (uint64, bool) {
	ptr, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, false
	}

	prev := windows.SetErrorMode(windows.SEM_FAILCRITICALERRORS)
	defer windows.SetErrorMode(prev)

	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &freeToCaller, &total, &totalFree); err != nil {
		return 0, false
	}
	// A zero total is "no usable answer", not "a full disk" — the same
	// reading the Linux half takes for a pseudo-filesystem, and the same one
	// describeDrive takes for the chooser. Refusing an export with "only 0 B
	// is free" on the strength of a call that told us nothing is the mirror
	// image of the defect FreeSpaceAt exists to fix. A genuinely full volume
	// has a non-zero total and is still refused.
	if total == 0 {
		return 0, false
	}
	return freeToCaller, true
}
