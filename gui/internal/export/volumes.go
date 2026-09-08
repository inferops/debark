// Package export answers "where can I put this bundle?" and then puts it
// there.
//
// # The safety boundary
//
// This package NEVER performs raw block-device access. It does not partition,
// format, write bootable images, or require root or polkit. It enumerates
// filesystems the operating system has ALREADY MOUNTED and copies ordinary
// files onto them, exactly as a file manager would. Writing bootable USBs was
// deliberately cut from this product as its most dangerous component; see
// ../../docs/dev/contract-brief.md ("Scope discipline").
//
// Concretely, and permanently:
//
//   - No block device is ever opened, for reading or for writing.
//   - No ioctl that mutates state is ever issued.
//   - Nothing here shells out to mkfs, parted, sfdisk, fdisk, dd, diskpart,
//     wipefs, udisksctl or anything comparable. This package runs no external
//     commands at all.
//   - Device metadata is read only from files the kernel exposes read-only to
//     unprivileged users (/proc/self/mountinfo, /sys/block/*/removable) and
//     from statfs(2) and stat(2) on a mount point. All of it works as a
//     normal user.
//
// If a change to this package would need root, the change is wrong.
package export

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupportedPlatform is returned by ListVolumes on a GOOS with no
// implementation. Callers should treat it as "offer the operator a plain
// directory picker instead", not as a fatal error.
var ErrUnsupportedPlatform = errors.New("export: volume enumeration is not supported on this platform")

// VolumeKind is the coarse class of a destination, and is what the UI should
// group and colour by. It is deliberately coarser than the filesystem type:
// the operator cares about "is this the stick I just plugged in", not about
// exfat versus vfat.
type VolumeKind int

const (
	// KindUnknown means the platform could not classify the volume. Treat it
	// as fixed for safety: never auto-select it.
	KindUnknown VolumeKind = iota

	// KindRemovable is media the operator can physically pull out: a USB
	// stick, an SD card, an external drive on a removable bus. This is the
	// case the whole screen exists for.
	KindRemovable

	// KindFixed is an internal disk, including the machine's own root
	// filesystem. Shown, but never ranked first and never auto-selected.
	KindFixed

	// KindNetwork is an NFS/SMB/sshfs mount. A real destination — plenty of
	// operators stage bundles on a share — but capacity numbers from a server
	// are advisory and a copy can fail halfway for reasons no local check
	// predicts.
	KindNetwork
)

func (k VolumeKind) String() string {
	switch k {
	case KindRemovable:
		return "removable"
	case KindFixed:
		return "fixed"
	case KindNetwork:
		return "network"
	default:
		return "unknown"
	}
}

// Volume is one already-mounted filesystem the operator could copy a bundle
// to. Every field is derived from read-only kernel interfaces.
//
// The fields exist to make the chooser trustworthy. Picking the wrong
// destination is the failure that matters on this screen, so the UI must be
// able to show, without a second round-trip, enough for the operator to tell
// a 64 GB USB stick apart from the root filesystem at a glance: a name they
// recognise, a size they recognise, and an unambiguous removable/fixed badge.
type Volume struct {
	// Path is the directory to copy into: the mount point on Unix, the drive
	// root ("D:\\") on Windows. This is also the volume's identity — it is
	// what a caller passes back to the copier, and what Watch diffs on.
	Path string

	// Label is the name to show the operator, best-effort and never empty.
	// Preference order on Linux: the filesystem label from
	// /dev/disk/by-label, then the mount point's own basename when it was
	// mounted under a removable-media directory (udisks2 names those after
	// the label anyway), then the device basename, then the mount point.
	// On Windows it is the volume label, falling back to the drive letter.
	Label string

	// Device is the mount source as the kernel reports it: "/dev/sdb1",
	// "//fileserver/bundles", "UUID=..."-resolved paths, and so on. Display
	// and disambiguation only. Nothing in this package ever opens it.
	Device string

	// FSType is the filesystem type ("ext4", "vfat", "exfat", "ntfs3",
	// "cifs"). Shown because it is the difference between "this stick will
	// work on the offline box" and a support ticket — for example a 4 GiB
	// per-file limit on vfat.
	FSType string

	// TotalBytes and FreeBytes describe capacity. FreeBytes is space
	// available to THIS user (statfs f_bavail), not to root, so it is the
	// number that actually predicts whether the copy fits. Both are zero when
	// capacity could not be determined; see Note.
	TotalBytes uint64
	FreeBytes  uint64

	// Kind is the coarse class. Removable is the derived convenience for the
	// common test; it is exactly (Kind == KindRemovable).
	Kind      VolumeKind
	Removable bool

	// ReadOnly reports that the filesystem itself is mounted read-only — a
	// stick with its physical write-protect switch on, a read-only NFS
	// export. Distinct from Writable: a read-write filesystem the operator
	// simply has no permission on is Writable=false, ReadOnly=false, and the
	// two cases need different advice.
	ReadOnly bool

	// Writable is a cheap best-effort answer to "can this user create files
	// here", determined without writing anything. See the Writable section of
	// the package docs and ProbeWritable for the definitive test.
	Writable bool

	// Note is a short human explanation when something about this volume is
	// off — "mounted read-only", "no write permission", "capacity
	// unavailable (filesystem not responding)". Empty when the volume is an
	// unremarkable, usable destination.
	Note string
}

// Summary renders the one-line description used in logs and in the details
// drawer. Kept here rather than in the UI so that the exact wording is
// covered by tests.
func (v Volume) Summary() string {
	var b strings.Builder
	b.WriteString(v.Label)
	if v.Path != v.Label {
		b.WriteString(" (")
		b.WriteString(v.Path)
		b.WriteString(")")
	}
	if v.TotalBytes > 0 {
		fmt.Fprintf(&b, " — %s free of %s", formatVolumeSize(v.FreeBytes), formatVolumeSize(v.TotalBytes))
	}
	if v.FSType != "" {
		b.WriteString(", ")
		b.WriteString(v.FSType)
	}
	b.WriteString(", ")
	b.WriteString(v.Kind.String())
	if v.Note != "" {
		b.WriteString(" — ")
		b.WriteString(v.Note)
	}
	return b.String()
}

// formatVolumeSize renders a capacity, and is the uint64 doorway onto the one
// byte formatter this application has. See humanBytes in copy.go for which
// convention that is and why.
//
// It existed as a second implementation because copy.go was owned by a
// different package while this file was being written, and reaching across an
// ownership boundary for an unexported helper mid-flight is worse than
// duplicating twelve lines. Both packages have landed, so the duplicate is gone
// and — this is the part that mattered — the two no longer disagree about
// units.
//
// A capacity past 8 exabytes is not a drive, so the conversion is clamped
// rather than allowed to go negative.
func formatVolumeSize(n uint64) string {
	if n > math.MaxInt64 {
		n = math.MaxInt64
	}
	return humanBytes(int64(n))
}

// ListVolumes returns the mounted filesystems that are plausible copy
// destinations, best candidate first.
//
// # Refresh model: explicit, and cheap enough to poll
//
// There is no filesystem watcher and no background goroutine of its own. Media
// appears and disappears, so the caller refreshes by calling ListVolumes
// again; Watch is a convenience loop around exactly that.
//
// This is affordable because a call costs one read of a small procfs file plus
// one statfs(2) and one stat(2) per surviving candidate, issued concurrently
// under a shared deadline. There is no device open, no udev round-trip, no
// D-Bus, and no process spawn. Polling once or twice a second is fine and is
// the intended use; the deadline means a hung network mount degrades that
// volume's capacity readout instead of freezing the UI.
//
// Every path returned is a directory. Linux mounts at file granularity and
// the mount table does not say which entries are files, so that is enforced
// by the platform layer rather than by the parser; see listVolumes in
// volumes_linux.go for the case that made it necessary.
//
// Ordering is deterministic: removable media first, then everything else by
// mount path, with the machine's own root filesystem forced last so it is
// never what a keyboard-driven operator lands on by default.
//
// An error is returned only when enumeration as a whole failed. Individual
// volumes that vanish mid-enumeration — the stick pulled out between reading
// mountinfo and stat-ing it, which happens — are dropped silently, because
// the correct response is a shorter list, not a failed screen.
func ListVolumes() ([]Volume, error) {
	vols, err := listVolumes()
	if err != nil {
		return nil, err
	}
	sortVolumes(vols)
	return vols, nil
}

// ProbeWritable is the definitive writability test, for the moment the
// operator commits to a destination rather than for every poll.
//
// Volume.Writable is a permission check that never touches the disk, which is
// what makes it safe to run on a 1 s timer — but a permission check is a
// prediction. Quotas, a full filesystem, a read-only remount that raced the
// enumeration, SELinux or AppArmor policy, an SMB server that grants
// directory traversal but denies creation: all of those pass the cheap check
// and fail the copy. So the copier calls this once, immediately before it
// starts, and surfaces the real errno.
//
// It leaves nothing behind:
//
//   - On failure nothing was created, so there is nothing to clean up.
//   - On success the temporary file is closed and removed before returning,
//     including on every error path, via defer.
//   - The name is dot-prefixed and self-identifying, so that in the one case
//     removal can fail (the process is killed between create and remove) what
//     remains is a zero-length hidden file the operator can recognise.
//
// It writes a few bytes rather than only creating the file, because create
// succeeds and write fails on a full or over-quota filesystem. It does not
// fsync: durability is irrelevant to a file about to be deleted, and an fsync
// on slow removable media can block for seconds.
func ProbeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".debark-write-test-*")
	if err != nil {
		return fmt.Errorf("export: %s is not writable: %w", dir, err)
	}
	name := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(name)
	}()
	if _, err := f.Write([]byte("debark")); err != nil {
		return fmt.Errorf("export: %s accepted a new file but not data (full, or over quota): %w", dir, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("export: %s failed to flush a test file (full, or over quota): %w", dir, err)
	}
	return nil
}

// Fingerprint reduces a volume list to a short token that changes when the
// set of destinations meaningfully changes.
//
// It covers identity (path, label, filesystem, kind, capacity, permissions)
// and free space quantised to freeSpaceQuantum. The quantisation is the whole
// point: an unquantised free-byte count on the root filesystem changes
// several times a second as logs are written, which would make every
// change-detecting caller redraw continuously and detect nothing. At 16 MiB
// the token still moves when a copy makes real progress or a drive fills up,
// but not when syslog appends a line.
func Fingerprint(vols []Volume) string {
	h := fnv.New64a()
	for _, v := range vols {
		// hash.Hash documents that Write never returns an error, so the
		// discard is the whole handling there is: fnv has no failure mode
		// and no channel to report one on.
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%t\x00%t\x00",
			v.Path, v.Label, v.Device, v.FSType,
			v.Kind, v.TotalBytes, v.FreeBytes/freeSpaceQuantum,
			v.ReadOnly, v.Writable)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// freeSpaceQuantum is the granularity at which Fingerprint notices free-space
// changes. See Fingerprint.
const freeSpaceQuantum = 16 << 20

// Watch polls ListVolumes and invokes onChange whenever the result's
// Fingerprint differs from the previous one, starting with an immediate call
// so the caller never has to prime the UI itself.
//
// It blocks until ctx is cancelled, so run it in its own goroutine. onChange
// is called from that goroutine and must not block; a Wails binding should
// emit an event and return.
//
// Errors are delivered to onChange rather than terminating the loop, because
// the interesting failures here are transient — /proc unreadable inside a
// half-configured container, a drive yanked mid-scan — and a chooser that
// stops updating after one bad poll is worse than one that reports the
// problem and keeps trying. An error result is fingerprinted as its message,
// so a persistent failure is reported once, not once per tick.
func Watch(ctx context.Context, interval time.Duration, onChange func([]Volume, error)) {
	if interval <= 0 {
		interval = time.Second
	}
	var last string
	tick := func() {
		vols, err := ListVolumes()
		fp := "err:"
		if err != nil {
			fp += err.Error()
		} else {
			fp = Fingerprint(vols)
		}
		if fp == last {
			return
		}
		last = fp
		onChange(vols, err)
	}

	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// rank orders the chooser. Removable media first because it is the case this
// screen exists for; the root filesystem last because "I copied the bundle
// into my home directory again" is the most common wasted build. Everything
// else sits between, ordered by path so the list does not reshuffle under the
// operator's cursor between polls.
func rank(v Volume) int {
	switch {
	case v.Kind == KindRemovable:
		return 0
	case isSystemRoot(v.Path):
		return 3
	case v.Kind == KindNetwork:
		return 2
	default:
		return 1
	}
}

// isSystemRoot reports whether a path is the machine's own root volume, in
// either platform's spelling. Kept OS-independent so the ranking rule is
// testable everywhere.
func isSystemRoot(p string) bool {
	if p == "/" {
		return true
	}
	// Windows system drive. Comparing against %SystemDrive% would be more
	// precise, but C: is right on effectively every machine and being wrong
	// here only misorders one row.
	q := strings.ToUpper(strings.TrimSuffix(strings.ReplaceAll(p, "/", `\`), `\`))
	return q == "C:"
}

func sortVolumes(vols []Volume) {
	sort.SliceStable(vols, func(i, j int) bool {
		ri, rj := rank(vols[i]), rank(vols[j])
		if ri != rj {
			return ri < rj
		}
		return vols[i].Path < vols[j].Path
	})
}

// -----------------------------------------------------------------------------
// Linux mountinfo parsing and filtering.
//
// This lives in the OS-independent file on purpose. It is pure text
// processing — no syscalls, no filesystem access, no build-tagged types — and
// keeping it untagged means its table-driven tests compile and run on every
// developer machine and in CI regardless of GOOS. This project is developed
// on Windows; if these functions were tagged `linux` their tests would be
// silently skipped on the machine where they are being written, which is
// exactly how broken parsers ship. volumes_linux.go supplies the syscalls and
// nothing else.
// -----------------------------------------------------------------------------

// mountEntry is one parsed line of /proc/self/mountinfo.
//
// # Why mountinfo and not /proc/mounts
//
// /proc/mounts is a compatibility view rendered in fstab's five-column shape.
// It is missing three things this package needs and cannot reconstruct:
//
//  1. The device number (major:minor). Without it the only handle on a device
//     is the source string, which is unreliable — the same device appears as
//     "/dev/sdb1" here and "/dev/disk/by-uuid/..." there, network sources are
//     not devices at all, and there is no way to look a source string up in
//     sysfs. mountinfo hands over the kernel's own identifier, which maps
//     directly to /sys/dev/block/<major>:<minor> and to a stat(2) Rdev, so
//     labels and removability can be resolved without parsing device paths.
//
//  2. The mount root — which subtree of the filesystem is mounted. This is
//     the only way to tell a bind mount from the real thing, and bind mounts
//     are otherwise indistinguishable duplicates that would show the
//     operator the same 500 GB disk five times.
//
//  3. Mount IDs and parent IDs, which make over-mounting (two filesystems on
//     one mount point, the second hiding the first) visible.
//
// Both files octal-escape whitespace, so mountinfo costs nothing extra there.
//
// Line format (mountinfo(5)):
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime shared:1 - ext3 /dev/root rw,errors=continue
//	ID par maj:min root  point  opts   [optional...] - type source  super-opts
//
// The optional-field section is variable length and terminated by a single
// "-", which is why this cannot be parsed by fixed field index.
type mountEntry struct {
	ID         int
	ParentID   int
	Major      int
	Minor      int
	Root       string // subtree of the filesystem that is mounted; "/" for a whole-fs mount
	MountPoint string
	Options    string   // per-mount options (rw/ro, nosuid, ...)
	Optional   []string // propagation fields, before the "-"
	FSType     string
	Source     string
	SuperOpts  string // per-superblock options (rw/ro, ...)
}

// ReadOnly reports a read-only mount. Both option fields must be consulted:
// the superblock can be read-only while the mount says rw, and a read-write
// filesystem can be mounted ro. Either one means no writes.
func (e mountEntry) isReadOnly() bool {
	return hasOption(e.Options, "ro") || hasOption(e.SuperOpts, "ro")
}

func hasOption(opts, want string) bool {
	for _, o := range strings.Split(opts, ",") {
		if o == want {
			return true
		}
	}
	return false
}

// parseMountInfo parses the whole of /proc/self/mountinfo.
//
// Malformed lines are skipped rather than failing the parse: this file is
// written by the kernel and by every container runtime's mount namespace
// manipulation, and one line this parser does not understand must not cost
// the operator their drive list.
func parseMountInfo(data string) []mountEntry {
	var out []mountEntry
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		e, ok := parseMountInfoLine(line)
		if !ok {
			continue
		}
		out = append(out, e)
	}
	return out
}

func parseMountInfoLine(line string) (mountEntry, bool) {
	fields := strings.Fields(line)
	if len(fields) < 7 {
		return mountEntry{}, false
	}

	// Locate the "-" separator that ends the variable optional-field section.
	sep := -1
	for i := 6; i < len(fields); i++ {
		if fields[i] == "-" {
			sep = i
			break
		}
	}
	// After the separator: fstype, source, super options.
	if sep < 0 || len(fields) < sep+4 {
		return mountEntry{}, false
	}

	var e mountEntry
	var err error
	if e.ID, err = strconv.Atoi(fields[0]); err != nil {
		return mountEntry{}, false
	}
	if e.ParentID, err = strconv.Atoi(fields[1]); err != nil {
		return mountEntry{}, false
	}
	maj, mnr, ok := strings.Cut(fields[2], ":")
	if !ok {
		return mountEntry{}, false
	}
	if e.Major, err = strconv.Atoi(maj); err != nil {
		return mountEntry{}, false
	}
	if e.Minor, err = strconv.Atoi(mnr); err != nil {
		return mountEntry{}, false
	}

	e.Root = unescapeOctal(fields[3])
	e.MountPoint = unescapeOctal(fields[4])
	e.Options = fields[5]
	e.Optional = append([]string(nil), fields[6:sep]...)
	e.FSType = unescapeOctal(fields[sep+1])
	e.Source = unescapeOctal(fields[sep+2])
	e.SuperOpts = fields[sep+3]
	return e, true
}

// unescapeOctal reverses the kernel's mangling of the root, mount point and
// source fields, which are whitespace-separated and therefore cannot contain
// literal whitespace. The kernel escapes space (\040), tab (\011), newline
// (\012) and backslash (\134) as backslash plus exactly three octal digits.
//
// This is not a theoretical concern: udisks2 mounts a USB stick labelled
// "MY BACKUP" at /media/user/MY\040BACKUP, and a parser that does not undo
// this hands the caller a path that does not exist. Every mount point on a
// removable-media machine is a candidate for it.
//
// A backslash not followed by three octal digits is left alone, which is what
// the kernel's own unescaping does and keeps the function total.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+3 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		v, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String()
}

// unescapeUdev reverses udev's escaping of symlink names under
// /dev/disk/by-label. udev uses a different scheme from the kernel's mount
// table — backslash-x plus two hex digits, so a stick labelled "MY BACKUP"
// appears as /dev/disk/by-label/MY\x20BACKUP — which is why this cannot reuse
// unescapeOctal. Getting it wrong shows the operator "MY\x20BACKUP" as the
// name of the drive they are about to write to.
func unescapeUdev(s string) string {
	if !strings.Contains(s, `\x`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+3 >= len(s) || (s[i+1] != 'x' && s[i+1] != 'X') {
			b.WriteByte(s[i])
			continue
		}
		v, err := strconv.ParseUint(s[i+2:i+4], 16, 8)
		if err != nil {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String()
}

// pseudoFSTypes are filesystems that are never a place to put a bundle.
//
// The rule: a filesystem is excluded when copying a bundle onto it would be
// meaningless or actively wrong, not merely unusual.
//
//   - Kernel and API filesystems (proc, sysfs, cgroup, ...) hold no user
//     data and often reject ordinary files outright.
//   - RAM-backed filesystems (tmpfs, ramfs, devtmpfs) evaporate on reboot. A
//     bundle written to /run or /dev/shm is a bundle the operator loses,
//     and on a typical desktop tmpfs accounts for a dozen entries that would
//     bury the one row that matters.
//   - Read-only image formats (squashfs, erofs, iso9660) cannot be written
//     to by construction. squashfs alone contributes one loopback mount per
//     installed snap — routinely thirty or more rows of pure noise.
//   - Container and namespace plumbing (overlay, nsfs, fuse.lxcfs, autofs).
//     An overlay mount is some container's root filesystem; writing there
//     writes into a container layer that the next `docker rm` deletes.
//
// Everything not on this list is kept, including every real block filesystem
// and every network filesystem. Over-filtering is the more expensive mistake:
// an operator staging a bundle on an SMB share or a spare internal disk is a
// real, supported case, and a chooser that silently omits their destination
// is a chooser they stop trusting.
var pseudoFSTypes = map[string]bool{
	"autofs": true, "binder": true, "binfmt_misc": true, "bpf": true,
	"cgroup": true, "cgroup2": true, "configfs": true, "cpuset": true,
	"debugfs": true, "devpts": true, "devtmpfs": true, "efivarfs": true,
	"erofs": true, "fuse.gvfsd-fuse": true, "fuse.lxcfs": true,
	"fuse.portal": true, "fuse.snapfuse": true, "fusectl": true,
	"hugetlbfs": true, "iso9660": true, "mqueue": true, "nsfs": true,
	"overlay": true, "overlayfs": true, "pipefs": true, "proc": true,
	"pstore": true, "ramfs": true, "resctrl": true, "rootfs": true,
	"rpc_pipefs": true, "securityfs": true, "selinuxfs": true,
	"sockfs": true, "squashfs": true, "sysfs": true, "tmpfs": true,
	"tracefs": true,
}

// networkFSTypes classify a mount as KindNetwork. Note that 9p is
// deliberately absent from pseudoFSTypes and present here: under WSL2 it is
// how the Windows drives appear at /mnt/c, which is a genuinely useful
// destination on a builder machine.
var networkFSTypes = map[string]bool{
	"9p": true, "afs": true, "ceph": true, "cifs": true, "davfs": true,
	"fuse.davfs2": true, "fuse.rclone": true, "fuse.s3fs": true,
	"fuse.sshfs": true, "glusterfs": true, "lustre": true, "nfs": true,
	"nfs4": true, "smb2": true, "smb3": true, "smbfs": true, "virtiofs": true,
}

// systemPathPrefixes are mount points that are part of the machine's own
// machinery. These are excluded by path rather than by type because the
// filesystems underneath them are perfectly ordinary — /boot/efi is a real,
// writable vfat partition, and it is the single worst place in the tree to
// drop a multi-hundred-megabyte bundle, since filling the ESP is how a
// machine stops booting.
var systemPathPrefixes = []string{
	"/proc", "/sys", "/dev", "/run", "/boot",
	"/snap", "/var/snap", "/var/lib/snapd",
	"/var/lib/docker", "/var/lib/containers", "/var/lib/kubelet",
	"/var/lib/lxd", "/var/lib/lxc", "/var/lib/machines",
	"/nix/store",
	// WSL2 injects a read-only 9p mount of the host's GPU drivers at
	// /usr/lib/wsl/drivers and an overlay of the kernel modules at
	// /usr/lib/modules/<version>. Both are real, non-pseudo filesystems that
	// pass every other test and are pure noise in a drive chooser. Observed
	// on WSL2 Ubuntu, which is a plausible machine for a developer to run
	// this on even though the product ships on native Ubuntu.
	"/usr/lib/wsl", "/usr/lib/modules",
}

// hasPathPrefix is component-aware: "/booted" is not under "/boot".
func hasPathPrefix(p, prefix string) bool {
	if p == prefix {
		return true
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return strings.HasPrefix(p, prefix)
}

// removableMountDirs are where the desktop stack mounts removable media.
// Presence here is a strong, cheap signal that survives containers and odd
// sysfs layouts, and it also supplies a good label: udisks2 names the
// directory after the filesystem label.
var removableMountDirs = []string{"/media", "/run/media", "/Volumes"}

func isUnderRemovableMountDir(p string) bool {
	for _, d := range removableMountDirs {
		if hasPathPrefix(p, d) && p != d {
			return true
		}
	}
	return false
}

// rejectMount applies the type and path filters to a single entry. It returns
// the reason for rejection, or "" to keep the entry. Reasons are returned
// rather than a bare bool so that tests assert why a mount was dropped, which
// is what catches a rule that happens to produce the right answer for the
// wrong reason.
func rejectMount(e mountEntry) string {
	if e.MountPoint == "" || !strings.HasPrefix(e.MountPoint, "/") {
		return "not an absolute mount point"
	}
	if pseudoFSTypes[e.FSType] {
		return "pseudo-filesystem: " + e.FSType
	}
	// The removable-media directories are exempt from the system-path rule
	// below, and this ordering is load-bearing. udisks2 on Fedora, RHEL and
	// Arch mounts USB sticks at /run/media/<user>/<label>, which sits inside
	// the "/run" prefix; applying the system-path rule first would delete
	// every removable drive on those distributions — the exact failure this
	// package exists to prevent. Removable media must always appear.
	if isUnderRemovableMountDir(e.MountPoint) {
		return ""
	}
	// fuse.<something> covers a long tail of desktop helpers (gvfs, portals,
	// appimage loopbacks). Named ones are in pseudoFSTypes; the rest are
	// kept, because fuse.sshfs and fuse.rclone are real destinations.
	for _, prefix := range systemPathPrefixes {
		if hasPathPrefix(e.MountPoint, prefix) {
			return "system path: " + prefix
		}
	}
	return ""
}

// selectMounts turns raw mountinfo entries into the candidate destinations,
// applying the filters and then removing the duplicates that make a mount
// table unreadable. Input order is the kernel's; output order is by mount
// point, so the result is stable across polls.
func selectMounts(entries []mountEntry) []mountEntry {
	// 1. Type and path filters.
	kept := make([]mountEntry, 0, len(entries))
	for _, e := range entries {
		if rejectMount(e) == "" {
			kept = append(kept, e)
		}
	}

	// 2. Over-mounts. Two filesystems can occupy one mount point; only the
	// last one in mountinfo order is reachable, and the earlier one is
	// invisible to any copy. Keep the last.
	byPoint := make(map[string]mountEntry, len(kept))
	for _, e := range kept {
		byPoint[e.MountPoint] = e
	}
	kept = kept[:0]
	for _, e := range byPoint {
		kept = append(kept, e)
	}

	// 3. Bind mounts and duplicate mounts of one device.
	//
	// A naive "Root != \"/\" means bind mount" rule is wrong and would be
	// catastrophic: on a btrfs system the root filesystem itself has
	// Root="/@" and /home has Root="/@home", so that rule deletes the
	// machine's disks entirely. Subvolumes are siblings, not subtrees of one
	// another.
	//
	// The rule that works: within one device (major:minor), sort roots
	// shallowest-first and keep an entry only when its Root is not the same
	// as, or a descendant of, a root already kept for that device.
	//
	//   - "/@" and "/@home" on 0:33      -> both kept; neither contains the
	//                                       other. btrfs subvolumes survive.
	//   - "/" on 8:1 and "/home/x" on 8:1 -> the second is dropped; it is
	//                                       `mount --bind /home/x /mnt/y`,
	//                                       already reachable under "/".
	//   - "/" on 8:1 twice                -> the second is dropped as an
	//                                       exact duplicate; the shorter
	//                                       mount point wins.
	//
	// What this rule cannot do, and it matters: it only hides a bind mount
	// behind an entry for the same device that SURVIVED step 1. When the
	// device's whole-root mount was rejected there — "/" is an overlay
	// inside a container, or a disk is only ever mounted at a system path —
	// nothing is left to shadow its bind mounts and they all come through,
	// including the ones whose mount point is a single file. That is not
	// fixable here: mountinfo has no field saying whether a mount point is a
	// file, so the check belongs where a stat(2) is available. See
	// listVolumes in volumes_linux.go, and
	// TestSelectMountsKeepsFileBindMountsWhenTheRootFilesystemIsRejected,
	// which pins this exact behaviour so that a later reader does not
	// mistake it for the whole filter.
	sort.Slice(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.Major != b.Major {
			return a.Major < b.Major
		}
		if a.Minor != b.Minor {
			return a.Minor < b.Minor
		}
		if da, db := strings.Count(a.Root, "/"), strings.Count(b.Root, "/"); da != db {
			return da < db
		}
		if a.Root != b.Root {
			return a.Root < b.Root
		}
		if len(a.MountPoint) != len(b.MountPoint) {
			return len(a.MountPoint) < len(b.MountPoint)
		}
		return a.MountPoint < b.MountPoint
	})

	roots := make(map[devKey][]string)
	out := make([]mountEntry, 0, len(kept))
	for _, e := range kept {
		k := devKey{e.Major, e.Minor}
		dup := false
		for _, r := range roots[k] {
			if r == e.Root || hasPathPrefix(e.Root, r) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		roots[k] = append(roots[k], e.Root)
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].MountPoint < out[j].MountPoint })
	return out
}

// labelFor picks the name the operator will recognise, given whatever the
// platform managed to discover. fsLabel is the filesystem's own label (empty
// if unknown); the rest come from the mount entry.
func labelFor(fsLabel string, e mountEntry) string {
	if fsLabel != "" {
		return fsLabel
	}
	// udisks2 mounts removable media at /media/<user>/<label>, so the
	// basename is the label even when /dev/disk/by-label was unreadable
	// (which is the normal case inside a flatpak or a container).
	if isUnderRemovableMountDir(e.MountPoint) {
		if base := path.Base(e.MountPoint); base != "" && base != "/" && base != "." {
			return base
		}
	}
	if e.MountPoint == "/" {
		return "/"
	}
	if base := path.Base(e.MountPoint); base != "" && base != "/" && base != "." {
		return base
	}
	if e.Source != "" {
		return e.Source
	}
	return e.MountPoint
}

// kindFor classifies a mount. removable comes from the platform's device
// inspection; it is combined here rather than there so the precedence rule
// has one home.
func kindFor(e mountEntry, removable bool) VolumeKind {
	if networkFSTypes[e.FSType] || strings.HasPrefix(e.Source, "//") {
		return KindNetwork
	}
	if removable || isUnderRemovableMountDir(e.MountPoint) {
		return KindRemovable
	}
	return KindFixed
}

// devKey identifies a filesystem by the kernel's own device number, as
// reported by mountinfo and by stat(2). Used to join mount entries to the
// /dev/disk/by-label symlink farm without comparing device path strings,
// which do not match. See diskLabels in volumes_linux.go.
type devKey struct{ major, minor int }

// majorMinor decodes a Linux dev_t. The encoding is not the historic
// 8-bit-major/8-bit-minor one: since kernel 2.6 the major is 12 bits split
// across the word and the minor is 20 bits, so a naive (dev>>8, dev&0xff)
// misreads any device whose minor is above 255 — which on a busy machine
// includes ordinary NVMe namespaces and device-mapper nodes. Getting this
// wrong silently mislabels drives rather than failing — the operator sees the
// wrong name next to the wrong free-space figure and there is no error to
// investigate — which is why it lives here, unexported but untagged, with a
// round-trip test against glibc's encoder.
func majorMinor(dev uint64) (int, int) {
	// The uint32 truncation is not decoration: it is what glibc's
	// gnu_dev_major/gnu_dev_minor do by returning unsigned int, and without
	// it a major above 4095 has its high bits read back as part of the minor.
	major := uint32((dev>>8)&0xfff) | uint32((dev>>32)&^0xfff) //nolint:gosec // G115: dev>>32 of a uint64 is at most 32 bits, so &^0xfff cannot exceed MaxUint32
	minor := uint32(dev&0xff) | (uint32(dev>>12) &^ 0xff)      //nolint:gosec // G115: the truncation IS glibc's gnu_dev_minor, which casts (dev>>12) to unsigned int; without it a major above 4095 reads back as part of the minor
	return int(major), int(minor)
}

// hasUSBComponent reports whether any component of a resolved sysfs path is a
// USB controller ("usb1", "usb2"). Matching the whole component rather than a
// prefix avoids false hits on unrelated names under /sys/devices. See
// deviceRemovable in volumes_linux.go for why the bus matters.
func hasUSBComponent(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if len(seg) > 3 && strings.HasPrefix(seg, "usb") && seg[3] >= '0' && seg[3] <= '9' {
			return true
		}
	}
	return false
}

// noteFor produces the short human explanation attached to an unusual volume.
func noteFor(readOnly, writable, haveCapacity bool) string {
	switch {
	case readOnly:
		return "mounted read-only"
	case !writable:
		return "no write permission for this user"
	case !haveCapacity:
		return "capacity unavailable (filesystem not responding)"
	}
	return ""
}
