package export

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These tests are deliberately untagged. The parsing, filtering and ranking
// they cover is pure, lives in volumes.go, and therefore runs on every GOOS —
// which matters because this package is developed on Windows, where the Linux
// implementation cannot be executed at all. If these tests only ran on Linux
// they would not be run by the person writing the parser, and a mountinfo
// parser nobody has run is a mountinfo parser that is wrong.

// ubuntuDesktopMountInfo is /proc/self/mountinfo from an Ubuntu 24.04 desktop
// with snaps installed, Docker running, a USB stick whose label contains a
// space, an SD card, an NFS mount and an SMB mount. Reproduced with the
// kernel's own escaping intact.
const ubuntuDesktopMountInfo = `24 30 0:22 / /proc rw,nosuid,nodev,noexec,relatime shared:13 - proc proc rw
25 30 0:23 / /sys rw,nosuid,nodev,noexec,relatime shared:2 - sysfs sysfs rw
26 30 0:5 / /dev rw,nosuid,relatime shared:8 - devtmpfs udev rw,size=8110448k,nr_inodes=2027612,mode=755,inode64
27 26 0:24 / /dev/pts rw,nosuid,noexec,relatime shared:9 - devpts devpts rw,gid=5,mode=620,ptmxmode=000
28 30 0:25 / /run rw,nosuid,nodev,noexec,relatime shared:5 - tmpfs tmpfs rw,size=1631428k,mode=755,inode64
30 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw,errors=remount-ro
31 25 0:7 / /sys/kernel/security rw,nosuid,nodev,noexec,relatime shared:3 - securityfs securityfs rw
32 26 0:27 / /dev/shm rw,nosuid,nodev shared:10 - tmpfs tmpfs rw,inode64
33 28 0:28 / /run/lock rw,nosuid,nodev,noexec,relatime shared:6 - tmpfs tmpfs rw,size=5120k,inode64
34 25 0:29 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:4 - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot
35 25 0:30 / /sys/fs/pstore rw,nosuid,nodev,noexec,relatime shared:16 - pstore pstore rw
36 25 0:31 / /sys/firmware/efi/efivars rw,nosuid,nodev,noexec,relatime shared:17 - efivarfs efivarfs rw
37 25 0:32 / /sys/fs/bpf rw,nosuid,nodev,noexec,relatime shared:18 - bpf bpf rw,mode=700
38 24 0:33 / /proc/sys/fs/binfmt_misc rw,relatime shared:26 - binfmt_misc binfmt_misc rw
39 25 0:20 / /sys/kernel/debug rw,nosuid,nodev,noexec,relatime shared:19 - debugfs debugfs rw
40 25 0:12 / /sys/kernel/tracing rw,nosuid,nodev,noexec,relatime shared:20 - tracefs tracefs rw
41 26 0:35 / /dev/hugepages rw,relatime shared:11 - hugetlbfs hugetlbfs rw,pagesize=2M
42 26 0:19 / /dev/mqueue rw,nosuid,nodev,noexec,relatime shared:12 - mqueue mqueue rw
43 25 0:8 / /sys/kernel/config rw,nosuid,nodev,noexec,relatime shared:21 - configfs configfs rw
44 25 0:36 / /sys/fs/fuse/connections rw,nosuid,nodev,noexec,relatime shared:22 - fusectl fusectl rw
45 30 8:1 / /boot/efi rw,relatime shared:38 - vfat /dev/sda1 rw,fmask=0077,dmask=0077,codepage=437,iocharset=iso8859-1,shortname=mixed,errors=remount-ro
120 30 7:0 / /snap/bare/5 ro,nodev,relatime shared:63 - squashfs /dev/loop0 ro,errors=continue,threads=single,x-gdu.hide
122 30 7:1 / /snap/core22/1380 ro,nodev,relatime shared:66 - squashfs /dev/loop1 ro,errors=continue,threads=single,x-gdu.hide
124 30 7:2 / /snap/firefox/4396 ro,nodev,relatime shared:69 - squashfs /dev/loop2 ro,errors=continue,threads=single,x-gdu.hide
126 30 7:3 / /snap/gnome-42-2204/141 ro,nodev,relatime shared:72 - squashfs /dev/loop3 ro,errors=continue,threads=single,x-gdu.hide
128 30 7:4 / /snap/snapd/21465 ro,nodev,relatime shared:75 - squashfs /dev/loop4 ro,errors=continue,threads=single,x-gdu.hide
200 28 0:50 / /run/user/1000 rw,nosuid,nodev,relatime shared:110 - tmpfs tmpfs rw,size=1631424k,nr_inodes=407856,mode=700,uid=1000,gid=1000,inode64
202 200 0:51 / /run/user/1000/gvfs rw,nosuid,nodev,relatime shared:113 - fuse.gvfsd-fuse gvfsd-fuse rw,user_id=1000,group_id=1000
204 200 0:52 / /run/user/1000/doc rw,nosuid,nodev,relatime shared:116 - fuse.portal portal rw,user_id=1000,group_id=1000
250 30 0:57 / /var/lib/docker/overlay2/9f2c1a/merged rw,relatime shared:120 - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/AAA,upperdir=/var/lib/docker/overlay2/9f2c1a/diff,workdir=/var/lib/docker/overlay2/9f2c1a/work
252 30 0:58 / /var/lib/docker/containers/9f2c1a/mounts/shm rw,nosuid,nodev,noexec,relatime shared:122 - tmpfs shm rw,size=65536k,inode64
300 30 8:17 / /media/operator/MY\040BACKUP rw,nosuid,nodev,relatime shared:130 - vfat /dev/sdb1 rw,uid=1000,gid=1000,fmask=0022,dmask=0022,codepage=437,iocharset=iso8859-1,shortname=mixed,errors=remount-ro
302 30 179:1 / /media/operator/FIELD\040KIT rw,nosuid,nodev,relatime shared:133 - exfat /dev/mmcblk0p1 rw,uid=1000,gid=1000,fmask=0022,dmask=0022
310 30 0:60 / /mnt/bundles rw,relatime shared:140 - nfs4 fileserver:/export/bundles rw,vers=4.2,rsize=1048576,wsize=1048576,namlen=255,hard,proto=tcp
312 30 0:61 / /srv/incoming rw,relatime shared:142 - cifs //winbox/My\040Share rw,vers=3.1.1,cache=strict,username=operator
320 30 8:33 / /mnt/spare rw,relatime shared:150 - xfs /dev/sdc1 rw,attr2,inode64,logbufs=8,logbsize=32k,noquota`

func mountPoints(entries []mountEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.MountPoint
	}
	return out
}

func TestSelectMountsUbuntuDesktop(t *testing.T) {
	got := mountPoints(selectMounts(parseMountInfo(ubuntuDesktopMountInfo)))

	// Everything the operator could plausibly copy a bundle to, and nothing
	// else. Note what survives: the root filesystem, a spare internal disk,
	// an NFS share and an SMB share are all real destinations and are kept.
	// Note what does not: 5 snap loopbacks, a Docker overlay, the ESP, and
	// 14 kernel/RAM filesystems.
	want := []string{
		"/",
		"/media/operator/FIELD KIT",
		"/media/operator/MY BACKUP",
		"/mnt/bundles",
		"/mnt/spare",
		"/srv/incoming",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("selectMounts:\n got %q\nwant %q", got, want)
	}
}

// wsl2MountInfo is a verbatim /proc/self/mountinfo from a real WSL2 Ubuntu
// instance. It is here because it is the gnarliest genuine sample available:
//
//   - Several lines carry no optional fields at all, so the "-" separator is
//     at index 6 rather than 7.
//   - The mount sources are "C:\134", the kernel's escaping of "C:\", which
//     is a backslash escape in a field that is not a path.
//   - / and /mnt/wslg/distro are the same device (8:80) with the same root,
//     mounted twice — a duplicate that must collapse to the shorter path.
//   - /mnt/wslg/versions.txt and /mnt/wslg/doc are overlay mounts of single
//     subtrees (Root=/etc/versions.txt), the shape a bind mount takes.
//     /mnt/wslg/versions.txt and /init are mounts of single FILES, which is
//     worth noting because it shows that file-granularity mounts are not a
//     container artefact — this is an ordinary WSL2 Ubuntu. Both are dropped
//     here by filesystem type (overlay, rootfs) and by nothing else, which
//     is luck rather than a rule; the rule that a destination must be a
//     directory is enforced in listVolumes, where a stat(2) is available.
//   - /usr/lib/wsl/drivers is a read-only 9p mount that passes the
//     pseudo-filesystem test and must be excluded by path.
//   - /run/shm and /dev/shm share one device (0:179) at two paths.
const wsl2MountInfo = `686 691 0:29 / /usr/lib/modules/6.6.87.2-microsoft-standard-WSL2 rw,nosuid,nodev,noatime - overlay none rw,lowerdir=/modules,upperdir=/lib/modules/6.6.87.2-microsoft-standard-WSL2/rw/upper,workdir=/lib/modules/6.6.87.2-microsoft-standard-WSL2/rw/work,uuid=on
687 691 0:32 / /mnt/wsl rw,relatime shared:1 - tmpfs none rw
688 691 0:34 / /usr/lib/wsl/drivers ro,nosuid,nodev,noatime - 9p drivers ro,aname=drivers;fmask=222;dmask=222,cache=5,access=client,msize=65536,trans=fd,rfd=8,wfd=8
691 676 8:80 / / rw,relatime - ext4 /dev/sdf rw,discard,errors=remount-ro,data=ordered
692 691 0:111 / /mnt/wslg rw,relatime shared:37 - tmpfs none rw
693 692 8:80 / /mnt/wslg/distro ro,relatime shared:42 - ext4 /dev/sdf rw,discard,errors=remount-ro,data=ordered
731 691 0:171 / /usr/lib/wsl/lib rw,nosuid,nodev,noatime - overlay none rw,lowerdir=/gpu_lib_packaged:/gpu_lib_inbox,upperdir=/gpu_lib/rw/upper,workdir=/gpu_lib/rw/work,uuid=on
732 691 0:2 /init /init ro - rootfs rootfs rw,size=49263748k,nr_inodes=12315937
733 691 0:5 / /dev rw,nosuid,relatime shared:55 - devtmpfs none rw,size=49263748k,nr_inodes=12315937,mode=755
734 691 0:22 / /sys rw,nosuid,nodev,noexec,noatime shared:56 - sysfs sysfs rw
735 691 0:175 / /proc rw,nosuid,nodev,noexec,noatime shared:57 - proc proc rw
736 733 0:176 / /dev/pts rw,nosuid,noexec,noatime shared:58 - devpts devpts rw,gid=5,mode=620,ptmxmode=000
737 691 0:177 / /run rw,nosuid,nodev shared:59 - tmpfs none rw,mode=755
738 737 0:178 / /run/lock rw,nosuid,nodev,noexec,noatime shared:60 - tmpfs none rw
739 737 0:179 / /run/shm rw,nosuid,nodev,noatime shared:61 - tmpfs none rw
740 733 0:179 / /dev/shm rw,nosuid,nodev,noatime shared:61 - tmpfs none rw
743 737 0:180 / /run/user rw,nosuid,nodev,noexec,noatime shared:62 - tmpfs none rw,mode=755
754 735 0:33 / /proc/sys/fs/binfmt_misc rw,relatime shared:63 - binfmt_misc binfmt_misc rw
755 734 0:23 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:64 - cgroup2 cgroup2 rw,nsdelegate
767 692 0:118 /etc/versions.txt /mnt/wslg/versions.txt rw,relatime shared:65 - overlay none rw,lowerdir=/systemvhd,upperdir=/system/rw/upper,workdir=/system/rw/work,uuid=on
769 692 0:118 /usr/share/doc /mnt/wslg/doc rw,relatime shared:66 - overlay none rw,lowerdir=/systemvhd,upperdir=/system/rw/upper,workdir=/system/rw/work,uuid=on
771 691 0:111 /.X11-unix /tmp/.X11-unix ro,relatime shared:37 - tmpfs none rw
772 691 0:185 / /mnt/c rw,noatime - 9p C:\134 rw,aname=drvfs;path=C:\;uid=1000;gid=1000;symlinkroot=/mnt/,cache=5,access=client,msize=65536,trans=fd,rfd=6,wfd=6
773 691 0:186 / /mnt/d rw,noatime - 9p D:\134 rw,aname=drvfs;path=D:\;uid=1000;gid=1000;symlinkroot=/mnt/,cache=5,access=client,msize=65536,trans=fd,rfd=6,wfd=6
774 691 0:187 / /mnt/e rw,noatime - 9p E:\134 rw,aname=drvfs;path=E:\;uid=1000;gid=1000;symlinkroot=/mnt/,cache=5,access=client,msize=65536,trans=fd,rfd=6,wfd=6
775 691 0:188 / /mnt/f rw,noatime - 9p F:\134 rw,aname=drvfs;path=F:\;uid=1000;gid=1000;symlinkroot=/mnt/,cache=5,access=client,msize=65536,trans=fd,rfd=6,wfd=6
776 743 0:111 /run/user /run/user rw,relatime shared:37 - tmpfs none rw
777 733 0:189 / /dev/hugepages rw,nosuid,nodev,relatime shared:67 - hugetlbfs hugetlbfs rw,pagesize=2M
778 733 0:109 / /dev/mqueue rw,nosuid,nodev,noexec,relatime shared:68 - mqueue mqueue rw
779 734 0:7 / /sys/kernel/debug rw,nosuid,nodev,noexec,relatime shared:69 - debugfs debugfs rw
780 734 0:12 / /sys/kernel/tracing rw,nosuid,nodev,noexec,relatime shared:70 - tracefs tracefs rw
781 734 0:84 / /sys/fs/fuse/connections rw,nosuid,nodev,noexec,relatime shared:71 - fusectl fusectl rw
782 734 0:190 / /sys/kernel/config rw,nosuid,nodev,noexec,relatime shared:72 - configfs configfs rw
979 776 0:200 / /run/user/1000 rw,nosuid,nodev,relatime shared:340 - tmpfs tmpfs rw,size=9853756k,nr_inodes=2463439,mode=700,uid=1000,gid=1000
996 692 0:200 / /mnt/wslg/run/user/1000 rw,nosuid,nodev,relatime shared:340 - tmpfs tmpfs rw,size=9853756k,nr_inodes=2463439,mode=700,uid=1000,gid=1000`

func TestSelectMountsWSL2(t *testing.T) {
	entries := parseMountInfo(wsl2MountInfo)

	// Every line of a real file must parse. A line the parser drops silently
	// is a drive the operator silently cannot see.
	if got, want := len(entries), strings.Count(strings.TrimSpace(wsl2MountInfo), "\n")+1; got != want {
		t.Errorf("parsed %d of %d lines", got, want)
	}

	got := mountPoints(selectMounts(entries))
	want := []string{"/", "/mnt/c", "/mnt/d", "/mnt/e", "/mnt/f"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("selectMounts:\n got %q\nwant %q", got, want)
	}
}

func TestWSL2SourcesAreUnescaped(t *testing.T) {
	// The Windows drive letters arrive as "C:\134" — the kernel escaping a
	// backslash in a field that is not a path at all.
	for _, e := range parseMountInfo(wsl2MountInfo) {
		if e.MountPoint == "/mnt/c" {
			if e.Source != `C:\` {
				t.Errorf("Source = %q, want %q", e.Source, `C:\`)
			}
			return
		}
	}
	t.Fatal("/mnt/c not found in the sample")
}

func TestWSL2DuplicateRootMountCollapses(t *testing.T) {
	// / and /mnt/wslg/distro are device 8:80 root=/ mounted twice. Listing
	// both would show the operator the same disk under two names with
	// identical free space.
	for _, e := range selectMounts(parseMountInfo(wsl2MountInfo)) {
		if e.MountPoint == "/mnt/wslg/distro" {
			t.Error("/mnt/wslg/distro was kept alongside /, which is the same filesystem")
		}
	}
}

func TestSelectMountsIsStable(t *testing.T) {
	// The chooser is polled once a second. If the order shifted between
	// polls the operator's selection would jump under their cursor, so the
	// output must be identical across runs despite the internal map.
	first := mountPoints(selectMounts(parseMountInfo(ubuntuDesktopMountInfo)))
	for i := 0; i < 25; i++ {
		got := mountPoints(selectMounts(parseMountInfo(ubuntuDesktopMountInfo)))
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("iteration %d differs:\n got %q\nwant %q", i, got, first)
		}
	}
}

func TestRejectMount(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string // "" means keep
	}{
		{
			name: "root ext4 is kept",
			line: `30 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw,errors=remount-ro`,
		},
		{
			name: "snap squashfs loopback",
			line: `122 30 7:1 / /snap/core22/1380 ro,nodev,relatime shared:66 - squashfs /dev/loop1 ro,errors=continue`,
			want: "pseudo-filesystem: squashfs",
		},
		{
			name: "docker overlay",
			line: `250 30 0:57 / /var/lib/docker/overlay2/9f2c1a/merged rw,relatime shared:120 - overlay overlay rw,lowerdir=/a`,
			want: "pseudo-filesystem: overlay",
		},
		{
			name: "tmpfs is RAM and vanishes on reboot",
			line: `28 30 0:25 / /run rw,nosuid,nodev,noexec,relatime shared:5 - tmpfs tmpfs rw,size=1631428k,mode=755`,
			want: "pseudo-filesystem: tmpfs",
		},
		{
			name: "the ESP is a real writable vfat partition and is still refused",
			line: `45 30 8:1 / /boot/efi rw,relatime shared:38 - vfat /dev/sda1 rw,fmask=0077`,
			want: "system path: /boot",
		},
		{
			name: "a real filesystem under /var/lib/docker is still container plumbing",
			line: `260 30 8:5 / /var/lib/docker/volumes/data/_data rw,relatime shared:99 - ext4 /dev/sdd1 rw`,
			want: "system path: /var/lib/docker",
		},
		{
			name: "udisks2 removable media under /run/media survives the /run rule",
			line: `300 30 8:17 / /run/media/operator/FIELD\040KIT rw,nosuid,nodev,relatime shared:130 - vfat /dev/sdb1 rw,uid=1000`,
		},
		{
			name: "removable media under /media",
			line: `300 30 8:17 / /media/operator/MY\040BACKUP rw,nosuid,nodev,relatime shared:130 - vfat /dev/sdb1 rw,uid=1000`,
		},
		{
			name: "nfs is a legitimate destination",
			line: `310 30 0:60 / /mnt/bundles rw,relatime shared:140 - nfs4 fileserver:/export/bundles rw,vers=4.2`,
		},
		{
			name: "sshfs is a legitimate destination",
			line: `311 30 0:62 / /home/operator/remote rw,nosuid,nodev,relatime shared:141 - fuse.sshfs operator@box:/srv rw,user_id=1000`,
		},
		{
			name: "9p is how WSL2 exposes the Windows drives and is kept",
			line: `35 28 0:32 / /mnt/c rw,noatime shared:20 - 9p C:\134 rw,dirsync,aname=drvfs;path=C:\134;uid=1000`,
		},
		{
			name: "an iso9660 loop mount cannot be written to",
			line: `400 30 11:0 / /mnt/iso ro,relatime shared:160 - iso9660 /dev/sr0 ro,nojoliet,check=s`,
			want: "pseudo-filesystem: iso9660",
		},
		{
			name: "a nix store is not a destination",
			line: `410 30 8:2 /nix/store /nix/store ro,relatime shared:170 - ext4 /dev/sda2 ro`,
			want: "system path: /nix/store",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := parseMountInfoLine(tc.line)
			if !ok {
				t.Fatalf("line did not parse: %s", tc.line)
			}
			if got := rejectMount(e); got != tc.want {
				t.Errorf("rejectMount() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelectMountsBtrfsSubvolumesAreNotBindMounts(t *testing.T) {
	// The trap this guards. All btrfs subvolumes of one filesystem share a
	// single major:minor and none of them has Root == "/", so the obvious
	// "Root != / means bind mount" and "one device means one volume" rules
	// both delete the machine's entire storage on a default Fedora, openSUSE
	// or Ubuntu-with-btrfs install.
	const in = `30 1 0:33 /@ / rw,relatime shared:1 - btrfs /dev/nvme0n1p3 rw,ssd,space_cache=v2,subvolid=256,subvol=/@
48 30 0:33 /@home /home rw,relatime shared:30 - btrfs /dev/nvme0n1p3 rw,ssd,space_cache=v2,subvolid=257,subvol=/@home
50 30 0:33 /@srv /srv rw,relatime shared:32 - btrfs /dev/nvme0n1p3 rw,ssd,space_cache=v2,subvolid=259,subvol=/@srv`

	got := mountPoints(selectMounts(parseMountInfo(in)))
	want := []string{"/", "/home", "/srv"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("btrfs subvolumes:\n got %q\nwant %q", got, want)
	}
}

func TestSelectMountsDropsBindMounts(t *testing.T) {
	// A bind mount is the same bytes at a second path. Listing it would show
	// the operator one 500 GB disk twice with identical free space, which is
	// precisely the confusion the chooser must not create.
	const in = `30 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
60 30 8:2 /home/operator/work /mnt/work rw,relatime shared:40 - ext4 /dev/sda2 rw
62 30 8:2 /home/operator/work /mnt/work2 rw,relatime shared:41 - ext4 /dev/sda2 rw`

	got := mountPoints(selectMounts(parseMountInfo(in)))
	want := []string{"/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bind mounts:\n got %q\nwant %q", got, want)
	}
}

func TestSelectMountsCollapsesDuplicateWholeDeviceMounts(t *testing.T) {
	// One device mounted at two places with Root == "/". Keep the shorter
	// path: it is the canonical one and the one the operator recognises.
	const in = `70 1 8:33 / /mnt/backup rw,relatime shared:50 - xfs /dev/sdc1 rw
72 1 8:33 / /mnt/some/deeply/nested/second/mount rw,relatime shared:51 - xfs /dev/sdc1 rw`

	got := mountPoints(selectMounts(parseMountInfo(in)))
	want := []string{"/mnt/backup"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("duplicate device mounts:\n got %q\nwant %q", got, want)
	}
}

func TestSelectMountsOvermountKeepsTheReachableFilesystem(t *testing.T) {
	// Two filesystems on one mount point: only the last is reachable, and a
	// copy would land on it. Showing the hidden one, with the hidden one's
	// free space, would be a lie.
	const in = `80 1 8:33 / /mnt/data rw,relatime shared:60 - xfs /dev/sdc1 rw
82 1 8:49 / /mnt/data rw,relatime shared:61 - ext4 /dev/sdd1 rw`

	kept := selectMounts(parseMountInfo(in))
	if len(kept) != 1 {
		t.Fatalf("got %d entries, want 1: %q", len(kept), mountPoints(kept))
	}
	if kept[0].Source != "/dev/sdd1" {
		t.Errorf("kept the shadowed filesystem: Source = %q, want /dev/sdd1", kept[0].Source)
	}
}

// dockerContainerMountInfo is /proc/self/mountinfo from inside a plain
// `docker run` of this project's own test image (debark-shots:go126, Docker
// Desktop 29.6.2 on Windows), captured 2026-09-07. It is verbatim apart from
// the overlay line's lowerdir list, which is nine snapshot paths of no
// interest here and has been elided to keep the fixture readable, and two
// -v mounts of the working tree.
//
// It is here because it is the only sample available in which a mount point
// is NOT a directory. Docker bind-mounts /etc/hostname, /etc/hosts and
// /etc/resolv.conf as single FILES off the host's real ext4 disk (8:48).
const dockerContainerMountInfo = `1020 680 0:113 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/desktop-containerd/daemon/io.containerd.snapshotter.v1.overlayfs/snapshots/26191/fs,upperdir=/var/lib/desktop-containerd/daemon/io.containerd.snapshotter.v1.overlayfs/snapshots/26192/fs,workdir=/var/lib/desktop-containerd/daemon/io.containerd.snapshotter.v1.overlayfs/snapshots/26192/work
1022 1020 0:162 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
1023 1020 0:164 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755
1024 1023 0:166 / /dev/pts rw,nosuid,noexec,relatime - devpts devpts rw,gid=5,mode=620,ptmxmode=666
1025 1020 0:168 / /sys ro,nosuid,nodev,noexec,relatime - sysfs sysfs ro
1026 1025 0:23 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw,nsdelegate
1027 1023 0:158 / /dev/mqueue rw,nosuid,nodev,noexec,relatime - mqueue mqueue rw
1028 1023 0:169 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k
1029 1020 0:70 /Users/operator/scratch /sp rw,noatime - 9p C:\134 rw,aname=drvfs;path=C:\;uid=0;gid=0;metadata,cache=5,access=client,msize=65536,trans=fd,rfd=5,wfd=5
1030 1020 8:48 /data/docker/containers/027d4562ccce/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/sdd rw
1031 1020 8:48 /data/docker/containers/027d4562ccce/hostname /etc/hostname rw,relatime - ext4 /dev/sdd rw
1032 1020 8:48 /data/docker/containers/027d4562ccce/hosts /etc/hosts rw,relatime - ext4 /dev/sdd rw
693 1022 0:162 /bus /proc/bus ro,nosuid,nodev,noexec,relatime - proc proc rw
696 1022 0:162 /sys /proc/sys ro,nosuid,nodev,noexec,relatime - proc proc rw
698 1022 0:170 / /proc/acpi ro,relatime - tmpfs tmpfs ro,size=4k,nr_inodes=1
699 1022 0:164 /null /proc/interrupts rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755
732 1025 0:170 / /sys/firmware ro,relatime - tmpfs tmpfs ro,size=4k,nr_inodes=1`

func TestSelectMountsRejectsAnOverlayRootFilesystem(t *testing.T) {
	// "/" is not sacred. Inside a container it is an overlay mount, which is
	// some container's layer: a bundle written there is deleted by the next
	// `docker rm`, so rejecting it is correct and deliberate. The live tests
	// in volumes_linux_test.go switch their expectation on exactly this.
	entries := parseMountInfo(dockerContainerMountInfo)
	var root mountEntry
	for _, e := range entries {
		if e.MountPoint == "/" {
			root = e
		}
	}
	if root.FSType != "overlay" {
		t.Fatalf("fixture no longer has an overlay root: %q", root.FSType)
	}
	if got, want := rejectMount(root), "pseudo-filesystem: overlay"; got != want {
		t.Errorf("rejectMount(overlay /) = %q, want %q", got, want)
	}
}

func TestSelectMountsKeepsFileBindMountsWhenTheRootFilesystemIsRejected(t *testing.T) {
	// This test pins a LIMITATION, not a desired behaviour, so that a reader
	// of selectMounts is not left thinking the filter is complete.
	//
	// Step 3 hides a bind mount behind an entry for the same device whose
	// Root contains it. Here there is no such entry: "/" on this machine is
	// the overlay above, rejected in step 1, so device 8:48 has no surviving
	// whole-root mount and all three of its file bind mounts come through.
	//
	// selectMounts cannot fix that — mountinfo has no field saying whether a
	// mount point is a file — so listVolumes stats each candidate and drops
	// the ones that are not directories. Everything below is what reaches
	// that stat.
	got := mountPoints(selectMounts(parseMountInfo(dockerContainerMountInfo)))
	want := []string{"/etc/hostname", "/etc/hosts", "/etc/resolv.conf", "/sp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("container mounts:\n got %q\nwant %q", got, want)
	}
}

func TestSelectMountsShadowsFileBindMountsWhenTheDeviceRootSurvives(t *testing.T) {
	// The same three Docker file bind mounts, on a machine whose "/" is a
	// real ext4 mount of the very device they come from — a stock desktop.
	// Now step 3 has something to shadow them with and they all disappear,
	// which is why this defect is invisible on a normal Ubuntu install.
	const in = `30 1 8:48 / / rw,relatime shared:1 - ext4 /dev/sdd rw
1030 30 8:48 /data/docker/containers/027d4562ccce/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/sdd rw
1031 30 8:48 /data/docker/containers/027d4562ccce/hostname /etc/hostname rw,relatime - ext4 /dev/sdd rw
1032 30 8:48 /data/docker/containers/027d4562ccce/hosts /etc/hosts rw,relatime - ext4 /dev/sdd rw`

	got := mountPoints(selectMounts(parseMountInfo(in)))
	want := []string{"/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("file bind mounts under a live root:\n got %q\nwant %q", got, want)
	}
}

func TestSelectMountsFileBindMountOffADeviceOnlyMountedAtASystemPath(t *testing.T) {
	// The mechanism is not container-specific, which is the reason the guard
	// in listVolumes is not written as a container check. Here "/" is an
	// ordinary ext4 disk and nothing is an overlay; the second disk (8:16)
	// is mounted only at /boot, which rejectMount drops by path. So 8:16 has
	// no surviving whole-root entry either, and a single file bind-mounted
	// off it reaches the chooser exactly as it does inside a container.
	//
	// This input is CONSTRUCTED, not captured: no machine reachable from
	// this project was found in this state. It is here to show that the
	// survival condition is "the device has no surviving whole-root entry"
	// and not "we are in a container", so that nobody replaces the stat with
	// a container check.
	const in = `30 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
45 30 8:16 / /boot rw,relatime shared:38 - ext4 /dev/sdb rw
60 30 8:16 /config-6.8.0-generic /srv/kernel-config rw,relatime shared:40 - ext4 /dev/sdb rw`

	got := mountPoints(selectMounts(parseMountInfo(in)))
	want := []string{"/", "/srv/kernel-config"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bind off a system-path-only device:\n got %q\nwant %q", got, want)
	}
}

func TestParseMountInfoLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		ok   bool
		want mountEntry
	}{
		{
			name: "no optional fields at all",
			line: `30 1 8:2 / / rw,relatime - ext4 /dev/sda2 rw,errors=remount-ro`,
			ok:   true,
			want: mountEntry{
				ID: 30, ParentID: 1, Major: 8, Minor: 2,
				Root: "/", MountPoint: "/", Options: "rw,relatime",
				Optional: nil, FSType: "ext4", Source: "/dev/sda2",
				SuperOpts: "rw,errors=remount-ro",
			},
		},
		{
			name: "several optional fields",
			line: `36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 propagate_from:2 unbindable - ext3 /dev/root rw,errors=continue`,
			ok:   true,
			want: mountEntry{
				ID: 36, ParentID: 35, Major: 98, Minor: 0,
				Root: "/mnt1", MountPoint: "/mnt2", Options: "rw,noatime",
				Optional:  []string{"master:1", "propagate_from:2", "unbindable"},
				FSType:    "ext3",
				Source:    "/dev/root",
				SuperOpts: "rw,errors=continue",
			},
		},
		{
			name: "escaped space in the mount point, which every labelled USB stick has",
			line: `300 30 8:17 / /media/operator/MY\040BACKUP rw,nosuid shared:130 - vfat /dev/sdb1 rw,uid=1000`,
			ok:   true,
			want: mountEntry{
				ID: 300, ParentID: 30, Major: 8, Minor: 17,
				Root: "/", MountPoint: "/media/operator/MY BACKUP", Options: "rw,nosuid",
				Optional: []string{"shared:130"}, FSType: "vfat",
				Source: "/dev/sdb1", SuperOpts: "rw,uid=1000",
			},
		},
		{
			name: "escaped space in the source, as an SMB share name",
			line: `312 30 0:61 / /srv/incoming rw,relatime shared:142 - cifs //winbox/My\040Share rw,vers=3.1.1`,
			ok:   true,
			want: mountEntry{
				ID: 312, ParentID: 30, Major: 0, Minor: 61,
				Root: "/", MountPoint: "/srv/incoming", Options: "rw,relatime",
				Optional: []string{"shared:142"}, FSType: "cifs",
				Source: "//winbox/My Share", SuperOpts: "rw,vers=3.1.1",
			},
		},
		{
			name: "large minor number, above the 8-bit range",
			line: `90 1 259:7 / /mnt/nvme rw,relatime shared:70 - ext4 /dev/nvme0n1p7 rw`,
			ok:   true,
			want: mountEntry{
				ID: 90, ParentID: 1, Major: 259, Minor: 7,
				Root: "/", MountPoint: "/mnt/nvme", Options: "rw,relatime",
				Optional: []string{"shared:70"}, FSType: "ext4",
				Source: "/dev/nvme0n1p7", SuperOpts: "rw",
			},
		},
		{name: "empty line", line: ``},
		{name: "truncated line", line: `30 1 8:2 / /`},
		{name: "missing separator", line: `30 1 8:2 / / rw,relatime shared:1 ext4 /dev/sda2 rw`},
		{name: "nothing after the separator", line: `30 1 8:2 / / rw,relatime shared:1 - ext4`},
		{name: "non-numeric device", line: `30 1 sda:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw`},
		{name: "non-numeric mount id", line: `x 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw`},
		{name: "device without a colon", line: `30 1 82 / / rw,relatime shared:1 - ext4 /dev/sda2 rw`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseMountInfoLine(tc.line)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("\n got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestParseMountInfoSkipsGarbageWithoutLosingGoodLines(t *testing.T) {
	// A container runtime writing something this parser does not understand
	// must not cost the operator their drive list.
	const in = "30 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw\n" +
		"this is not a mountinfo line\n" +
		"\n" +
		"  \n" +
		"320 30 8:33 / /mnt/spare rw,relatime shared:150 - xfs /dev/sdc1 rw\r\n"

	got := parseMountInfo(in)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %q", len(got), mountPoints(got))
	}
	if got[1].SuperOpts != "rw" {
		t.Errorf("CRLF was not trimmed: SuperOpts = %q", got[1].SuperOpts)
	}
}

func TestUnescapeOctal(t *testing.T) {
	tests := []struct{ in, want string }{
		{`/media/operator/MY\040BACKUP`, "/media/operator/MY BACKUP"},
		{`/mnt/tab\011here`, "/mnt/tab\there"},
		{`/mnt/new\012line`, "/mnt/new\nline"},
		{`/mnt/back\134slash`, `/mnt/back\slash`},
		{`/media/a\040b\040c`, "/media/a b c"},
		{"/no/escapes/here", "/no/escapes/here"},
		{"", ""},
		// A trailing or malformed escape must not panic or eat characters.
		{`/mnt/trailing\`, `/mnt/trailing\`},
		{`/mnt/short\04`, `/mnt/short\04`},
		{`/mnt/nonoctal\09a`, `/mnt/nonoctal\09a`},
		{`/mnt/hex\x20style`, `/mnt/hex\x20style`}, // udev's scheme, not the kernel's
	}
	for _, tc := range tests {
		if got := unescapeOctal(tc.in); got != tc.want {
			t.Errorf("unescapeOctal(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUnescapeUdev(t *testing.T) {
	tests := []struct{ in, want string }{
		{`MY\x20BACKUP`, "MY BACKUP"},
		{`FIELD\x20KIT\x20v2`, "FIELD KIT v2"},
		{"PLAIN", "PLAIN"},
		{"", ""},
		{`trailing\x2`, `trailing\x2`},
		{`bad\xZZhex`, `bad\xZZhex`},
		{`octal\040style`, `octal\040style`}, // the kernel's scheme, not udev's
	}
	for _, tc := range tests {
		if got := unescapeUdev(tc.in); got != tc.want {
			t.Errorf("unescapeUdev(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHasPathPrefix(t *testing.T) {
	tests := []struct {
		p, prefix string
		want      bool
	}{
		{"/boot", "/boot", true},
		{"/boot/efi", "/boot", true},
		{"/booted", "/boot", false}, // the reason this is not strings.HasPrefix
		{"/boot-backup", "/boot", false},
		{"/var/lib/docker/volumes/x", "/var/lib/docker", true},
		{"/var/lib/dockerish", "/var/lib/docker", false},
		{"/media/operator/USB", "/media", true},
		{"/mnt", "/media", false},
		{"/", "/", true},
	}
	for _, tc := range tests {
		if got := hasPathPrefix(tc.p, tc.prefix); got != tc.want {
			t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", tc.p, tc.prefix, got, tc.want)
		}
	}
}

func TestIsUnderRemovableMountDir(t *testing.T) {
	tests := []struct {
		p    string
		want bool
	}{
		{"/media/operator/MY BACKUP", true},
		{"/run/media/operator/FIELD KIT", true},
		{"/media", false}, // the directory itself is not media
		{"/run/media", false},
		{"/mnt/spare", false},
		{"/", false},
	}
	for _, tc := range tests {
		if got := isUnderRemovableMountDir(tc.p); got != tc.want {
			t.Errorf("isUnderRemovableMountDir(%q) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestIsReadOnly(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "read-write",
			line: `30 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw`,
			want: false,
		},
		{
			name: "mounted read-only",
			line: `30 1 8:2 / / ro,relatime shared:1 - ext4 /dev/sda2 rw`,
			want: true,
		},
		{
			name: "superblock read-only, as a write-protect switch produces",
			line: `30 1 8:17 / /media/operator/LOCKED rw,relatime shared:1 - vfat /dev/sdb1 ro,uid=1000`,
			want: true,
		},
		{
			name: "an option merely containing ro must not match",
			line: `30 1 8:2 / / rw,relatime,errors=remount-ro shared:1 - ext4 /dev/sda2 rw,rootcontext=x`,
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := parseMountInfoLine(tc.line)
			if !ok {
				t.Fatal("line did not parse")
			}
			if got := e.isReadOnly(); got != tc.want {
				t.Errorf("isReadOnly() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLabelFor(t *testing.T) {
	tests := []struct {
		name    string
		fsLabel string
		e       mountEntry
		want    string
	}{
		{
			name:    "the filesystem label wins",
			fsLabel: "FIELD KIT",
			e:       mountEntry{MountPoint: "/media/operator/disk", Source: "/dev/sdb1"},
			want:    "FIELD KIT",
		},
		{
			name: "udisks2 names the directory after the label, so use it",
			e:    mountEntry{MountPoint: "/media/operator/MY BACKUP", Source: "/dev/sdb1"},
			want: "MY BACKUP",
		},
		{
			name: "and on distributions that mount under /run/media",
			e:    mountEntry{MountPoint: "/run/media/operator/FIELD KIT", Source: "/dev/sdb1"},
			want: "FIELD KIT",
		},
		{
			name: "the root filesystem is called what it is",
			e:    mountEntry{MountPoint: "/", Source: "/dev/sda2"},
			want: "/",
		},
		{
			name: "an unlabelled internal disk falls back to its mount point",
			e:    mountEntry{MountPoint: "/mnt/spare", Source: "/dev/sdc1"},
			want: "spare",
		},
		{
			name: "a network share falls back to its mount point too",
			e:    mountEntry{MountPoint: "/mnt/bundles", Source: "fileserver:/export/bundles"},
			want: "bundles",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := labelFor(tc.fsLabel, tc.e); got != tc.want {
				t.Errorf("labelFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKindFor(t *testing.T) {
	tests := []struct {
		name      string
		e         mountEntry
		removable bool
		want      VolumeKind
	}{
		{
			name: "nfs is network",
			e:    mountEntry{FSType: "nfs4", MountPoint: "/mnt/bundles", Source: "fileserver:/export"},
			want: KindNetwork,
		},
		{
			name: "cifs is network",
			e:    mountEntry{FSType: "cifs", MountPoint: "/srv/incoming", Source: "//winbox/share"},
			want: KindNetwork,
		},
		{
			name: "a UNC source is network even with an unfamiliar type",
			e:    mountEntry{FSType: "smbfuse", MountPoint: "/srv/x", Source: "//winbox/share"},
			want: KindNetwork,
		},
		{
			name:      "sysfs said removable",
			e:         mountEntry{FSType: "vfat", MountPoint: "/mnt/stick", Source: "/dev/sdb1"},
			removable: true,
			want:      KindRemovable,
		},
		{
			name: "sysfs was unreadable but the desktop mounted it as media",
			e:    mountEntry{FSType: "exfat", MountPoint: "/media/operator/FIELD KIT", Source: "/dev/sdb1"},
			want: KindRemovable,
		},
		{
			name: "network wins over the media directory, since a share can be mounted there",
			e:    mountEntry{FSType: "cifs", MountPoint: "/media/operator/share", Source: "//winbox/share"},
			want: KindNetwork,
		},
		{
			name: "an internal disk is fixed",
			e:    mountEntry{FSType: "xfs", MountPoint: "/mnt/spare", Source: "/dev/sdc1"},
			want: KindFixed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := kindFor(tc.e, tc.removable); got != tc.want {
				t.Errorf("kindFor() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSortVolumesPutsRemovableFirstAndRootLast(t *testing.T) {
	in := []Volume{
		{Path: "/", Kind: KindFixed},
		{Path: "/mnt/bundles", Kind: KindNetwork},
		{Path: "/mnt/spare", Kind: KindFixed},
		{Path: "/media/operator/MY BACKUP", Kind: KindRemovable},
		{Path: "/home", Kind: KindFixed},
		{Path: "/media/operator/FIELD KIT", Kind: KindRemovable},
	}
	sortVolumes(in)

	want := []string{
		"/media/operator/FIELD KIT",
		"/media/operator/MY BACKUP",
		"/home",
		"/mnt/spare",
		"/mnt/bundles",
		"/",
	}
	got := make([]string, len(in))
	for i, v := range in {
		got[i] = v.Path
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortVolumes:\n got %q\nwant %q", got, want)
	}
}

func TestIsSystemRoot(t *testing.T) {
	tests := []struct {
		p    string
		want bool
	}{
		{"/", true},
		{"/home", false},
		{`C:\`, true},
		{"C:", true},
		{"c:/", true},
		{`D:\`, false},
		{"/media/operator/USB", false},
	}
	for _, tc := range tests {
		if got := isSystemRoot(tc.p); got != tc.want {
			t.Errorf("isSystemRoot(%q) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

// makedev is glibc's gnu_dev_makedev, the inverse of majorMinor. Encoding the
// test's own inputs with it rather than by hand is deliberate: hand-written
// dev_t constants are exactly as easy to get wrong as the decoder, and a test
// that shares the decoder's mistake proves nothing.
func makedev(major, minor uint64) uint64 {
	return (minor & 0xff) |
		((major & 0xfff) << 8) |
		((minor &^ 0xff) << 12) |
		((major &^ 0xfff) << 32)
}

func TestMajorMinor(t *testing.T) {
	// The encoding packs a 12-bit major and a 20-bit minor across a 64-bit
	// word. The naive (dev>>8, dev&0xff) reading is right for /dev/sda2 and
	// wrong for anything with a minor above 255, which on a busy machine
	// includes ordinary NVMe namespaces and device-mapper nodes — hence the
	// large cases below.
	tests := []struct {
		name     string
		maj, min uint64
	}{
		{"/dev/sda2", 8, 2},
		{"/dev/sdb1", 8, 17},
		{"/dev/mmcblk0p1", 179, 1},
		{"device-mapper", 253, 0},
		{"minor above the 8-bit range", 259, 256},
		{"minor at the 12-bit boundary", 259, 4095},
		{"largest minor", 259, 0xfffff},
		{"major above the 12-bit range", 4097, 1},
		{"zero", 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dev := makedev(tc.maj, tc.min)
			maj, min := majorMinor(dev)
			if uint64(maj) != tc.maj || uint64(min) != tc.min {
				t.Errorf("majorMinor(%#x) = (%d, %d), want (%d, %d)", dev, maj, min, tc.maj, tc.min)
			}
		})
	}
}

func TestMajorMinorMatchesKnownEncodings(t *testing.T) {
	// A handful of literal dev_t values as the kernel actually reports them,
	// so the round-trip test above cannot pass by agreeing with a wrong
	// makedev.
	tests := []struct {
		dev              uint64
		wantMaj, wantMin int
	}{
		{0x0802, 8, 2},       // /dev/sda2
		{0x0811, 8, 17},      // /dev/sdb1
		{0xb301, 179, 1},     // /dev/mmcblk0p1
		{0x110300, 259, 256}, // an NVMe namespace with a minor past 255
	}
	for _, tc := range tests {
		maj, min := majorMinor(tc.dev)
		if maj != tc.wantMaj || min != tc.wantMin {
			t.Errorf("majorMinor(%#x) = (%d, %d), want (%d, %d)", tc.dev, maj, min, tc.wantMaj, tc.wantMin)
		}
	}
}

func TestHasUSBComponent(t *testing.T) {
	tests := []struct {
		p    string
		want bool
	}{
		{"/sys/devices/pci0000:00/0000:00:14.0/usb2/2-1/2-1:1.0/host6/target6:0:0/6:0:0:0/block/sdb/sdb1", true},
		{"/sys/devices/pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0/block/sda/sda2", false},
		{"/sys/devices/pci0000:00/0000:00:1d.0/usb1/1-1/block/sdc", true},
		{"/sys/devices/virtual/block/dm-0", false},
		{"/sys/devices/platform/usbcore/block/sdz", false}, // "usbcore" is not "usbN"
		{"/sys/devices/usb/block/sdz", false},              // bare "usb" is not "usbN"
		{"", false},
	}
	for _, tc := range tests {
		if got := hasUSBComponent(tc.p); got != tc.want {
			t.Errorf("hasUSBComponent(%q) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestNoteFor(t *testing.T) {
	tests := []struct {
		readOnly, writable, haveCapacity bool
		want                             string
	}{
		{false, true, true, ""},
		{true, false, true, "mounted read-only"},
		{false, false, true, "no write permission for this user"},
		{false, true, false, "capacity unavailable (filesystem not responding)"},
	}
	for _, tc := range tests {
		if got := noteFor(tc.readOnly, tc.writable, tc.haveCapacity); got != tc.want {
			t.Errorf("noteFor(%v,%v,%v) = %q, want %q", tc.readOnly, tc.writable, tc.haveCapacity, got, tc.want)
		}
	}
}

func TestFormatVolumeSize(t *testing.T) {
	tests := []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1000, "1000 B"},
		{1024, "1 KiB"},
		{57_300_000_000, "53.4 GiB"},
		{64_000_000_000, "59.6 GiB"},
		{2_000_000_000_000, "1.8 TiB"},
	}
	for _, tc := range tests {
		if got := formatVolumeSize(tc.n); got != tc.want {
			t.Errorf("formatVolumeSize(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestVolumeSummary(t *testing.T) {
	tests := []struct {
		name string
		v    Volume
		want string
	}{
		{
			name: "a USB stick",
			v: Volume{
				Path: "/media/operator/MY BACKUP", Label: "MY BACKUP", FSType: "vfat",
				TotalBytes: 64_000_000_000, FreeBytes: 57_300_000_000,
				Kind: KindRemovable, Removable: true, Writable: true,
			},
			want: "MY BACKUP (/media/operator/MY BACKUP) — 53.4 GiB free of 59.6 GiB, vfat, removable",
		},
		{
			name: "the root filesystem",
			v: Volume{
				Path: "/", Label: "/", FSType: "ext4",
				TotalBytes: 500_000_000_000, FreeBytes: 120_000_000_000,
				Kind: KindFixed, Writable: true,
			},
			want: "/ — 112 GiB free of 466 GiB, ext4, fixed",
		},
		{
			name: "a write-protected stick explains itself",
			v: Volume{
				Path: "/media/operator/LOCKED", Label: "LOCKED", FSType: "vfat",
				TotalBytes: 8_000_000_000, FreeBytes: 8_000_000_000,
				Kind: KindRemovable, Removable: true, ReadOnly: true,
				Note: "mounted read-only",
			},
			want: "LOCKED (/media/operator/LOCKED) — 7.5 GiB free of 7.5 GiB, vfat, removable — mounted read-only",
		},
		{
			name: "an unresponsive share omits the numbers it does not have",
			v: Volume{
				Path: "/mnt/bundles", Label: "bundles", FSType: "nfs4",
				Kind: KindNetwork, Note: "capacity unavailable (filesystem not responding)",
			},
			want: "bundles (/mnt/bundles), nfs4, network — capacity unavailable (filesystem not responding)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.Summary(); got != tc.want {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestVolumeKindString(t *testing.T) {
	for k, want := range map[VolumeKind]string{
		KindRemovable:  "removable",
		KindFixed:      "fixed",
		KindNetwork:    "network",
		KindUnknown:    "unknown",
		VolumeKind(99): "unknown",
	} {
		if got := k.String(); got != want {
			t.Errorf("VolumeKind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
}

func TestFingerprint(t *testing.T) {
	base := []Volume{
		{Path: "/", Label: "/", FSType: "ext4", Kind: KindFixed, TotalBytes: 500e9, FreeBytes: 120e9, Writable: true},
		{Path: "/media/operator/USB", Label: "USB", FSType: "vfat", Kind: KindRemovable, TotalBytes: 64e9, FreeBytes: 57e9, Writable: true},
	}
	fp := Fingerprint(base)

	t.Run("identical input", func(t *testing.T) {
		if got := Fingerprint(base); got != fp {
			t.Error("Fingerprint is not deterministic")
		}
	})

	t.Run("free-space drift below the quantum does not churn", func(t *testing.T) {
		// The root filesystem's free space changes several times a second as
		// logs are written. If that moved the fingerprint, a change-detecting
		// caller would redraw continuously and detect nothing.
		drift := append([]Volume(nil), base...)
		drift[0].FreeBytes -= 4096
		if got := Fingerprint(drift); got != fp {
			t.Error("a 4 KiB write to the root filesystem changed the fingerprint")
		}
	})

	t.Run("a real change in free space is noticed", func(t *testing.T) {
		big := append([]Volume(nil), base...)
		big[1].FreeBytes -= 4 * freeSpaceQuantum
		if got := Fingerprint(big); got == fp {
			t.Error("a 64 MiB change was not noticed")
		}
	})

	t.Run("unplugging is noticed", func(t *testing.T) {
		if got := Fingerprint(base[:1]); got == fp {
			t.Error("removing a volume did not change the fingerprint")
		}
	})

	t.Run("plugging in is noticed", func(t *testing.T) {
		plus := append([]Volume(nil), base...)
		plus = append(plus, Volume{Path: "/media/operator/SD", Label: "SD", Kind: KindRemovable})
		if got := Fingerprint(plus); got == fp {
			t.Error("adding a volume did not change the fingerprint")
		}
	})

	t.Run("a remount to read-only is noticed", func(t *testing.T) {
		ro := append([]Volume(nil), base...)
		ro[1].Writable = false
		if got := Fingerprint(ro); got == fp {
			t.Error("losing writability did not change the fingerprint")
		}
	})

	t.Run("a relabelled drive is noticed", func(t *testing.T) {
		rel := append([]Volume(nil), base...)
		rel[1].Label = "FIELD KIT"
		if got := Fingerprint(rel); got == fp {
			t.Error("a label change did not change the fingerprint")
		}
	})

	t.Run("empty and nil agree", func(t *testing.T) {
		if Fingerprint(nil) != Fingerprint([]Volume{}) {
			t.Error("nil and empty fingerprints differ")
		}
	})
}

func TestProbeWritable(t *testing.T) {
	dir := t.TempDir()

	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProbeWritable(dir); err != nil {
		t.Fatalf("ProbeWritable on a temp dir: %v", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The whole point: no litter, not even on success.
	if len(after) != len(before) {
		names := make([]string, len(after))
		for i, e := range after {
			names[i] = e.Name()
		}
		t.Errorf("ProbeWritable left %d entries behind: %q", len(after)-len(before), names)
	}

	t.Run("a directory that does not exist fails and creates nothing", func(t *testing.T) {
		missing := filepath.Join(dir, "no-such-directory")
		err := ProbeWritable(missing)
		if err == nil {
			t.Fatal("ProbeWritable succeeded on a missing directory")
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error does not name the directory: %v", err)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Error("ProbeWritable created the directory it was asked about")
		}
	})

	t.Run("a file is not a directory", func(t *testing.T) {
		f := filepath.Join(dir, "regular-file")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ProbeWritable(f); err == nil {
			t.Error("ProbeWritable succeeded on a regular file")
		}
	})
}

func TestWatchDeliversOnceThenStops(t *testing.T) {
	// Watch must prime the caller immediately — a chooser that shows nothing
	// for the first tick looks broken — and must return when the context is
	// cancelled rather than leaking a goroutine for the process's lifetime.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(ctx, 20*time.Millisecond, func([]Volume, error) {
			select {
			case calls <- struct{}{}:
			default:
			}
		})
	}()

	select {
	case <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not deliver an initial result")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after its context was cancelled")
	}
}

func TestWatchDoesNotRepeatAnUnchangedResult(t *testing.T) {
	// A steady machine must produce one callback, not one per tick, or every
	// caller has to de-duplicate for itself.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	var n int
	Watch(ctx, 10*time.Millisecond, func([]Volume, error) { n++ })

	if n == 0 {
		t.Fatal("no callbacks at all")
	}
	// Free space on a developer machine genuinely moves, so allow a few
	// legitimate changes; the failure being caught is one call per tick
	// (which would be roughly 25).
	if n > 8 {
		t.Errorf("got %d callbacks in 250ms across ~25 ticks; unchanged results are not being suppressed", n)
	}
}

// TestListVolumesDoesNotPanic exercises the real platform path on whatever
// GOOS the tests are running on. It asserts almost nothing about the content —
// what is attached to the machine is not the test's business — only that
// enumeration completes, respects its own invariants, and never panics.
func TestListVolumesDoesNotPanic(t *testing.T) {
	vols, err := ListVolumes()
	if err != nil {
		// The !linux && !windows fallback is a legitimate outcome.
		if err == ErrUnsupportedPlatform {
			t.Skipf("no implementation for this platform: %v", err)
		}
		t.Fatalf("ListVolumes: %v", err)
	}

	seen := map[string]bool{}
	for _, v := range vols {
		if v.Path == "" {
			t.Error("a volume has no path")
		}
		if v.Label == "" {
			t.Errorf("%s has no label; the chooser would render a blank row", v.Path)
		}
		if seen[v.Path] {
			t.Errorf("%s listed twice", v.Path)
		}
		seen[v.Path] = true
		if v.Removable != (v.Kind == KindRemovable) {
			t.Errorf("%s: Removable=%v disagrees with Kind=%v", v.Path, v.Removable, v.Kind)
		}
		if v.ReadOnly && v.Writable {
			t.Errorf("%s is both read-only and writable", v.Path)
		}
		if v.FreeBytes > v.TotalBytes {
			t.Errorf("%s: %d free of %d total", v.Path, v.FreeBytes, v.TotalBytes)
		}
		t.Log(v.Summary())
	}

	// The ordering contract the UI relies on.
	for i := 1; i < len(vols); i++ {
		if rank(vols[i-1]) > rank(vols[i]) {
			t.Errorf("ordering violated at %d: %s (rank %d) before %s (rank %d)",
				i, vols[i-1].Path, rank(vols[i-1]), vols[i].Path, rank(vols[i]))
		}
	}
}

// TestListVolumesIsCheapEnoughToPoll backs the documented refresh model. The
// contract is that a caller may poll once or twice a second, which is only
// true if a call is far quicker than that.
func TestListVolumesIsCheapEnoughToPoll(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	if _, err := ListVolumes(); err == ErrUnsupportedPlatform {
		t.Skip("no implementation for this platform")
	}

	start := time.Now()
	const n = 5
	for i := 0; i < n; i++ {
		if _, err := ListVolumes(); err != nil {
			t.Fatalf("ListVolumes: %v", err)
		}
	}
	per := time.Since(start) / n
	t.Logf("ListVolumes: %v per call", per)

	// Deliberately loose. A hung network mount costs a full statfsBudget on
	// Linux, and CI runners are slow; this is a smoke test against an
	// implementation that accidentally starts a process or blocks on udev,
	// not a benchmark.
	if per > 2*time.Second {
		t.Errorf("ListVolumes took %v per call; it is documented as safe to poll every second", per)
	}
}
