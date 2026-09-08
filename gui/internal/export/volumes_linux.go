//go:build linux

package export

// Linux volume enumeration.
//
// This file holds only the parts that must touch the operating system: the
// procfs read, statfs(2), access(2), and two read-only walks of sysfs and
// /dev/disk. All parsing, filtering and ranking lives in volumes.go so that
// it is testable on any GOOS — see the header comment there.
//
// Everything below runs as an unprivileged user. No block device is opened;
// /sys/block/<dev>/removable and /sys/dev/block/<maj>:<min> are text files
// and symlinks the kernel exposes world-readable, and stat(2) on a symlink
// under /dev/disk/by-label resolves a device number without opening the node.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// mountInfoPath is the kernel's authoritative mount table for this process's
// mount namespace. Using /proc/self rather than /proc/1 matters inside a
// container or a systemd unit with a private mount namespace: the mounts this
// process can actually write to are the ones it can see.
const mountInfoPath = "/proc/self/mountinfo"

// statfsBudget bounds how long enumeration will wait for capacity numbers.
//
// statfs(2) on a hung NFS or SMB mount blocks in uninterruptible sleep,
// potentially for the server's timeout — minutes. Since ListVolumes is
// designed to be polled once a second, one unreachable share would otherwise
// freeze the chooser permanently. Instead every statfs runs in its own
// goroutine under a shared deadline; a volume that misses it is still listed,
// with zero capacity and an explanatory Note, which is far more useful to the
// operator than a blank screen.
//
// A goroutine that misses the deadline is not cancellable — the syscall is in
// the kernel — so it is left to finish and write into a buffered channel
// nobody reads. It cannot block or leak memory beyond one small struct, and
// it exits when the server or the kernel timeout releases it.
const statfsBudget = 1500 * time.Millisecond

// accessWriteExec is access(2)'s W_OK|X_OK. Both are needed: creating a file
// in a directory requires write permission on it and search permission
// through it.
const accessWriteExec = 0x2 | 0x1

func listVolumes() ([]Volume, error) {
	data, err := os.ReadFile(mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("export: reading %s: %w", mountInfoPath, err)
	}

	mounts := selectMounts(parseMountInfo(string(data)))
	if len(mounts) == 0 {
		return nil, nil
	}

	caps := statfsAll(mounts, statfsBudget)
	labels := diskLabels()

	vols := make([]Volume, 0, len(mounts))
	for i, m := range mounts {
		c := caps[i]

		// A volume whose statfs failed outright (ENOENT, ESTALE) is one that
		// vanished between reading mountinfo and looking at it — the stick
		// was pulled, the automount expired. Drop it silently: the right
		// answer for the operator is a shorter list, not an error.
		if c.err != nil && !c.timedOut {
			continue
		}
		// A live filesystem with zero blocks is not a destination. This is
		// the belt-and-braces catch for pseudo-filesystems that slipped past
		// the type filter, which is likelier than it sounds given how freely
		// new ones appear.
		if c.ok && c.total == 0 {
			continue
		}
		// A mount point that is not a directory cannot receive a bundle.
		//
		// Linux mounts at file granularity, and the mount table cannot say
		// which entries those are — there is no field for it, which is why
		// this rule lives here and not in selectMounts. Every filter up to
		// this point has asked about the *filesystem*; this one asks about
		// the mount point itself, and it is the only one that can.
		//
		// The case that made this necessary: Docker bind-mounts
		// /etc/hostname, /etc/hosts and /etc/resolv.conf as single files
		// from the host's real disk. Their filesystem is ext4, so the type
		// filter keeps them, and selectMounts' per-device root-containment
		// pass cannot shadow them either, because "/" is an overlay mount
		// and was already rejected — so the device they live on has no
		// surviving whole-root entry to hide behind. All three reached the
		// chooser offering the host disk's capacity, and the export screen
		// would have offered the operator /etc/resolv.conf as a drive.
		//
		// That is the shape a container produces, but nothing about the
		// mechanism is container-specific: any device whose only whole-root
		// mount sits at a rejected path, plus one file bind-mounted off it,
		// reproduces it with no overlay anywhere. See
		// TestSelectMountsFileBindMountOffADeviceOnlyMountedAtASystemPath.
		//
		// Whether a shipping desktop can actually be in that state was NOT
		// established. Two real non-container Linux systems were swept —
		// every mount namespace on each, 65 and 137 of them, 909 surviving
		// candidates in total — and every mount point outside a container
		// namespace was a directory. No stock Ubuntu desktop, live-USB
		// session or overlayroot install was reachable to check, and all
		// three are configurations where "/" is not a real mount. So this is
		// a guard on an invariant the package has always relied on —
		// internal/export hands back somewhere a bundle can be copied into —
		// and not a fix for a bug reproduced off a container.
		//
		// The stat rides in the same budgeted goroutine as the statfs (see
		// statfsBudget): stat(2) on a hung NFS mount blocks exactly as
		// statfs(2) does, and doing it here in a loop would reintroduce the
		// freeze that budget exists to prevent. A mount that missed the
		// budget has no answer and is kept, which is the existing treatment
		// of an unresponsive volume — it is listed with no capacity and a
		// Note rather than silently dropped.
		if c.statOK && !c.isDir {
			continue
		}

		removable, _ := deviceRemovable(m.Major, m.Minor)
		readOnly := m.isReadOnly() || c.readOnly

		writable := !readOnly && syscall.Access(m.MountPoint, accessWriteExec) == nil

		v := Volume{
			Path:       m.MountPoint,
			Label:      labelFor(labels[devKey{m.Major, m.Minor}], m),
			Device:     m.Source,
			FSType:     m.FSType,
			TotalBytes: c.total,
			FreeBytes:  c.free,
			ReadOnly:   readOnly,
			Writable:   writable,
			Note:       noteFor(readOnly, writable, c.ok),
		}
		v.Kind = kindFor(m, removable)
		v.Removable = v.Kind == KindRemovable
		vols = append(vols, v)
	}
	return vols, nil
}

// capacity is everything one budgeted look at a candidate mount point
// learned. It carries two independent answers because both syscalls have to
// happen inside the same goroutine to stay under one deadline: statfs(2)
// describes the filesystem (ok, total, free, readOnly) and stat(2) describes
// the mount point's own inode (statOK, isDir).
//
// The two are genuinely independent. statfs(2) succeeds on a regular file —
// it reports the filesystem that file lives on — so capacity alone can never
// tell a destination from a single bind-mounted file.
type capacity struct {
	total    uint64
	free     uint64
	readOnly bool
	ok       bool
	timedOut bool

	// isDir reports that the mount point is a directory, and statOK that
	// stat(2) answered at all. An unanswered stat leaves both false and the
	// volume is kept: see listVolumes.
	isDir  bool
	statOK bool

	err error
}

// statfsAll stats every candidate concurrently under one deadline. See
// statfsBudget for why this is not a simple loop.
func statfsAll(mounts []mountEntry, budget time.Duration) []capacity {
	results := make([]chan capacity, len(mounts))
	for i, m := range mounts {
		ch := make(chan capacity, 1) // buffered: a late goroutine never blocks
		results[i] = ch
		go func(p string, ch chan<- capacity) {
			ch <- statfsOne(p)
		}(m.MountPoint, ch)
	}

	timer := time.NewTimer(budget)
	defer timer.Stop()

	out := make([]capacity, len(mounts))
	for i, ch := range results {
		select {
		case c := <-ch:
			out[i] = c
		case <-timer.C:
			// The budget is shared, so once it expires every remaining
			// volume is reported as unresponsive without further waiting.
			for j := i; j < len(results); j++ {
				select {
				case out[j] = <-results[j]:
				default:
					out[j] = capacity{timedOut: true}
				}
			}
			return out
		}
	}
	return out
}

func statfsOne(p string) capacity {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return capacity{err: err}
	}

	// Is the mount point a directory? Asked here rather than in listVolumes
	// so that it shares the caller's deadline; see capacity and statfsBudget.
	//
	// os.Stat, not os.Lstat: a mount point is never itself a symlink (the
	// kernel resolves the target before mounting), and following is what the
	// copier will do anyway. A failure — the mount vanished, or the process
	// cannot traverse to it — leaves statOK false and the volume is kept,
	// because refusing to list a destination on the strength of a syscall
	// that did not answer is the over-filtering this package's header warns
	// about.
	isDir, statOK := false, false
	if fi, err := os.Stat(p); err == nil {
		isDir, statOK = fi.IsDir(), true
	}

	// POSIX counts f_blocks in f_frsize units. Linux sets f_frsize equal to
	// f_bsize for every filesystem in practice, but preferring f_frsize when
	// it is set costs nothing and is the correct reading. The casts are
	// required because these fields are int32 on 32-bit ports and int64 on
	// 64-bit ones.
	unit := statfsUint(st.Frsize)
	if unit == 0 {
		unit = statfsUint(st.Bsize)
	}

	return capacity{
		total: statfsUint(st.Blocks) * unit,
		// f_bavail, not f_bfree: bfree includes the reserved blocks only root
		// may use, so quoting it would promise the operator space their copy
		// cannot actually have. On a default ext4 that is 5% of the disk.
		free:     statfsUint(st.Bavail) * unit,
		readOnly: statfsUint(st.Flags)&statRDONLY != 0,
		ok:       true,
		isDir:    isDir,
		statOK:   statOK,
	}
}

// statRDONLY is ST_RDONLY from statfs(2). Defined here rather than taken from
// a package because syscall does not export it on every Linux port.
const statRDONLY = 0x1

// deviceRemovable answers "can the operator pull this out", from sysfs alone.
//
// The primary source is /sys/block/<disk>/removable, reached from the mount's
// device number via /sys/dev/block/<major>:<minor>, which is a symlink into
// the device tree. Partitions carry a "partition" file, so the containing
// disk is that symlink's parent directory.
//
// The sysfs flag alone is not enough. It reports the *media* as removable,
// which is true for USB sticks and card readers but false for the external
// USB SSDs and spinning drives operators use constantly — those report
// removable=0 while being exactly the thing this screen is for. So the bus is
// consulted as well: a device reached through a USB or MMC controller is
// treated as removable regardless of the flag.
//
// The second return value reports whether sysfs could answer at all. It is
// false for network filesystems and anonymous devices (major 0, as used by
// btrfs, fuse and every network filesystem), and inside containers where
// /sys/dev/block is not mounted. Callers fall back to the mount-point
// heuristic in kindFor.
func deviceRemovable(major, minor int) (removable, known bool) {
	if major == 0 {
		return false, false // anonymous superblock: no backing block device
	}
	link := fmt.Sprintf("/sys/dev/block/%d:%d", major, minor)
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		return false, false
	}

	disk := resolved
	if _, err := os.Stat(filepath.Join(resolved, "partition")); err == nil {
		disk = filepath.Dir(resolved)
	}

	// Bus heuristic. The resolved path looks like
	// /sys/devices/pci0000:00/.../usb2/2-1/2-1:1.0/host6/.../block/sdb/sdb1
	// for a USB stick, so a "usbN" component identifies the transport
	// without opening anything.
	if hasUSBComponent(resolved) || strings.HasPrefix(filepath.Base(disk), "mmcblk") {
		return true, true
	}

	b, err := os.ReadFile(filepath.Join(disk, "removable"))
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(string(b)) == "1", true
}

// diskLabels maps device numbers to filesystem labels using the symlink farms
// udev maintains under /dev/disk.
//
// Matching is by device number, not by device path. That is the point: the
// mount source in mountinfo can be "/dev/sdb1", "/dev/disk/by-uuid/1234-ABCD"
// or a dm-crypt mapper path depending on how the volume was mounted, and
// string-comparing those against the by-label symlink targets fails in every
// interesting case. stat(2) on the symlink resolves it and yields st_rdev,
// which is the same identifier mountinfo reports, so the two always line up.
//
// A missing or unreadable /dev/disk is normal — inside a container, in a
// flatpak sandbox, on a minimal image — and simply yields no labels;
// labelFor then falls back to the mount point.
func diskLabels() map[devKey]string {
	out := make(map[devKey]string)
	// by-label is the filesystem's own label and is what a desktop shows.
	// by-partlabel is the GPT partition name, a reasonable second choice on
	// disks whose filesystems were created without a label.
	for _, dir := range []string{"/dev/disk/by-label", "/dev/disk/by-partlabel"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			// os.Stat follows the symlink; a dangling one (device removed
			// between ReadDir and now) simply fails and is skipped.
			fi, err := os.Stat(filepath.Join(dir, ent.Name()))
			if err != nil {
				continue
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			maj, mnr := majorMinor(uint64(st.Rdev))
			k := devKey{maj, mnr}
			if _, exists := out[k]; exists {
				continue // by-label wins over by-partlabel
			}
			out[k] = unescapeUdev(ent.Name())
		}
	}
	return out
}

// statfsField is every integer type a statfs field takes across the ports this
// builds for. The fields are unsigned on linux/amd64 and signed on others,
// which is why a fixed cast direction cannot work and why this is generic.
type statfsField interface {
	~int32 | ~int64 | ~uint32 | ~uint64
}

// statfsUint converts a statfs field to uint64, treating a negative value as
// zero.
//
// Negative is not a real filesystem answer, but a naive cast turns one into an
// enormous positive figure — and this value is multiplied into the free space
// the export screen decides a copy against. Reporting zero makes an
// unmeasurable filesystem look full, which refuses a copy; the inverse would
// start one that cannot finish.
func statfsUint[T statfsField](v T) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// freeSpaceOne is FreeSpaceAt's Linux half: bytes available to an
// unprivileged writer on the filesystem holding p, and whether statfs(2)
// answered at all.
//
// It is statfsOne, deliberately, rather than a second statfs call written
// beside it. statfsOne already resolves the f_frsize/f_bsize unit question
// and already prefers f_bavail over f_bfree so the root-reserved blocks are
// not promised to a copy that cannot have them — and a second copy of that
// reading would drift from the chooser's, which is how a screen comes to show
// one number and refuse against another.
//
// The stat(2) it also performs is not wasted here: p is the nearest EXISTING
// ancestor of the destination, so a false statOK means the path went away
// between the walk and the syscall, and the free space of a path that no
// longer exists is not a number worth returning.
//
// The caller runs this in its own goroutine under freeSpaceBudget, for the
// reason statfsBudget records: on a hung NFS or SMB mount this blocks in
// uninterruptible sleep for the server's timeout.
//
// A zero f_blocks is "no usable answer", not "a full disk", and the
// distinction is load-bearing rather than defensive. statfs(2) SUCCEEDS on a
// pseudo-filesystem and reports zero blocks and zero available: procfs,
// sysfs, cgroup2, debugfs. Returning that as a measured zero makes Plan refuse
// the export with "only 0 B is free" — the mirror image of the defect
// FreeSpaceAt exists to fix, and caught by
// TestFreeSpaceAtOnNothingAtAllIsUnknown on Linux, where the nearest existing
// ancestor of a nonexistent path under /proc is /proc itself. listVolumes
// takes the same reading two functions up, dropping any mount where c.ok &&
// c.total == 0 as "a live filesystem with zero blocks is not a destination".
//
// A genuinely full disk is unaffected: it has a non-zero total and zero
// available, and is still refused.
func freeSpaceOne(p string) (uint64, bool) {
	c := statfsOne(p)
	if c.err != nil || !c.ok || c.total == 0 {
		return 0, false
	}
	return c.free, true
}
