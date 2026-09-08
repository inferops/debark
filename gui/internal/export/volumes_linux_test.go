//go:build linux

package export

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Everything in this file touches the running system, so it is guarded twice:
// by the linux build tag, and by a skip when the machine cannot support the
// assertion. The parsing and filtering these functions feed is covered
// exhaustively and portably in volumes_test.go; what is left here is only what
// cannot be tested without a kernel.

func skipIfNoProcfs(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(mountInfoPath); err != nil {
		t.Skipf("%s is unreadable (%v); not a machine this test can say anything about", mountInfoPath, err)
	}
}

// rootIsARealFilesystem reports whether "/" on this machine is a filesystem
// this package considers a destination at all.
//
// It exists because two assertions here used to be written as "the root
// filesystem is on every Linux machine, including inside a container" — and
// that is false. Inside a container "/" is an overlay mount, which is a
// container layer the next `docker rm` deletes, and pseudoFSTypes rejects it
// on purpose (see the comment there). The same is true of a live-USB session
// and an overlayroot install. So both tests failed in every container this
// project builds and tests in, and the code they were failing against was
// right.
//
// The tests do not skip on that. Skipping would retire coverage of the shape
// the product actually ships onto — a desktop with a real root filesystem —
// on any machine where the suite happens to run in a container, which for
// this project is most of them. Instead each assertion switches to the one
// that is true of THIS machine, so both environments verify something, and
// the property that holds in both is asserted separately below in
// TestLiveEveryEnumeratedVolumeIsAnExistingDirectory.
//
// The question is answered from the live mount table rather than by
// detecting a container: the rule that matters is "is / a filesystem worth
// copying to", which is exactly what rejectMount already decides.
func rootIsARealFilesystem(t *testing.T) (mountEntry, bool) {
	t.Helper()
	for _, e := range parseMountInfo(readMountInfo(t)) {
		if e.MountPoint != "/" {
			continue
		}
		if reason := rejectMount(e); reason != "" {
			t.Logf("/ is %s and is rejected (%s); asserting the container/live-image shape", e.FSType, reason)
			return e, false
		}
		return e, true
	}
	t.Fatalf("no entry for / in %s at all", mountInfoPath)
	return mountEntry{}, false
}

func TestLiveMountInfoParses(t *testing.T) {
	skipIfNoProcfs(t)

	data, err := os.ReadFile(mountInfoPath)
	if err != nil {
		t.Fatalf("reading %s: %v", mountInfoPath, err)
	}

	entries := parseMountInfo(string(data))
	if len(entries) == 0 {
		t.Fatalf("parsed no entries from %d bytes of live mountinfo", len(data))
	}

	// Every non-blank line of the real file must parse. A line the parser
	// silently drops is a mount the operator silently cannot see.
	var lines int
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}
	if len(entries) != lines {
		t.Errorf("parsed %d of %d lines of live mountinfo; some real line is not understood", len(entries), lines)
	}

	// Whether "/" survives the filter is not a universal truth; it depends on
	// what "/" is on this machine. Both answers are asserted, because both
	// are worth asserting — see rootIsARealFilesystem.
	root, realRoot := rootIsARealFilesystem(t)

	var haveRoot bool
	for _, e := range selectMounts(entries) {
		if e.MountPoint == "/" {
			haveRoot = true
		}
	}
	switch {
	case realRoot && !haveRoot:
		t.Errorf("/ is a real %s filesystem and did not survive selectMounts", root.FSType)
	case !realRoot && haveRoot:
		t.Errorf("/ is a %s mount, which is not a place to leave a bundle, and selectMounts kept it", root.FSType)
	}
}

func TestLiveStatfsRoot(t *testing.T) {
	skipIfNoProcfs(t)

	c := statfsOne("/")
	if c.err != nil {
		t.Fatalf("statfs(/): %v", c.err)
	}
	if !c.ok || c.total == 0 {
		t.Errorf("statfs(/) reported no capacity: %+v", c)
	}
	if c.free > c.total {
		t.Errorf("statfs(/) reported %d free of %d total", c.free, c.total)
	}
	t.Logf("/ = %s free of %s", formatVolumeSize(c.free), formatVolumeSize(c.total))
}

func TestLiveStatfsOnAVanishedPathFailsRatherThanHangs(t *testing.T) {
	// The "device vanished between listing and stat-ing" path. listVolumes
	// drops such volumes silently, which is only correct if statfs actually
	// returns an error here rather than blocking.
	done := make(chan capacity, 1)
	go func() { done <- statfsOne("/nonexistent-mount-point-for-debark-tests") }()
	select {
	case c := <-done:
		if c.err == nil {
			t.Error("statfs on a nonexistent path succeeded")
		}
		if c.ok {
			t.Error("a failed statfs was reported as ok")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("statfs on a nonexistent path did not return")
	}
}

func TestStatfsAllHonoursItsBudget(t *testing.T) {
	skipIfNoProcfs(t)

	mounts := selectMounts(parseMountInfo(readMountInfo(t)))
	if len(mounts) == 0 {
		t.Skip("no candidate mounts")
	}

	start := time.Now()
	caps := statfsAll(mounts, statfsBudget)
	elapsed := time.Since(start)

	if len(caps) != len(mounts) {
		t.Fatalf("got %d results for %d mounts", len(caps), len(mounts))
	}
	// The budget is the promise that one hung NFS server cannot freeze the
	// chooser. Allow generous slack for a loaded CI runner.
	if elapsed > statfsBudget*4 {
		t.Errorf("statfsAll took %v against a %v budget", elapsed, statfsBudget)
	}
	t.Logf("statfs of %d mounts in %v", len(mounts), elapsed)
}

func readMountInfo(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(mountInfoPath)
	if err != nil {
		t.Fatalf("reading %s: %v", mountInfoPath, err)
	}
	return string(data)
}

func TestLiveDeviceRemovable(t *testing.T) {
	skipIfNoProcfs(t)

	// An anonymous superblock has no backing block device and must be
	// reported as unknown rather than guessed at.
	if _, known := deviceRemovable(0, 42); known {
		t.Error("deviceRemovable claimed to know about an anonymous device")
	}

	// A device number that cannot exist must not panic and must not claim
	// knowledge.
	if removable, known := deviceRemovable(9998, 9999); known || removable {
		t.Errorf("deviceRemovable(9998, 9999) = (%v, %v), want (false, false)", removable, known)
	}

	for _, m := range selectMounts(parseMountInfo(readMountInfo(t))) {
		removable, known := deviceRemovable(m.Major, m.Minor)
		t.Logf("%-40s %d:%-4d removable=%v known=%v", m.MountPoint, m.Major, m.Minor, removable, known)
	}
}

func TestLiveDiskLabels(t *testing.T) {
	skipIfNoProcfs(t)

	// Never fails: /dev/disk is absent inside containers and minimal images,
	// and the correct behaviour there is an empty map, not an error.
	labels := diskLabels()
	for k, v := range labels {
		t.Logf("%d:%d = %q", k.major, k.minor, v)
		if v == "" {
			t.Errorf("device %d:%d mapped to an empty label", k.major, k.minor)
		}
		if strings.Contains(v, `\x`) {
			t.Errorf("label %q was not unescaped", v)
		}
	}
}

func TestLiveListVolumes(t *testing.T) {
	skipIfNoProcfs(t)

	vols, err := ListVolumes()
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	for _, v := range vols {
		t.Log(v.Summary())
	}

	entry, realRoot := rootIsARealFilesystem(t)

	var root *Volume
	for i := range vols {
		if vols[i].Path == "/" {
			root = &vols[i]
		}
	}

	if !realRoot {
		// A container, a live-image session, an overlayroot appliance. There
		// is no root filesystem here worth copying a bundle onto, so the
		// assertion is the inverse — and it is a real assertion, not a skip.
		if root != nil {
			t.Errorf("/ is a %s mount and was enumerated as a destination: %s", entry.FSType, root.Summary())
		}
		return
	}

	// On a machine with a real root filesystem it is the one row that can be
	// asserted regardless of what is plugged in.
	if root == nil {
		t.Fatalf("/ is a real %s filesystem and was not enumerated", entry.FSType)
	}
	if root.TotalBytes == 0 {
		t.Error("the root filesystem reports zero capacity")
	}
	if root.FSType == "" {
		t.Error("the root filesystem has no filesystem type")
	}
	if vols[len(vols)-1].Path != "/" && len(vols) > 1 {
		// Not an error in itself — another volume could tie — but the
		// ranking rule says the root filesystem sorts last.
		t.Errorf("the root filesystem is not last; order is %v", volumePaths(vols))
	}
}

// TestLiveEveryEnumeratedVolumeIsAnExistingDirectory is the property that
// holds on every Linux machine, in a container or not, and that the literal
// "/ is in the list" assertions above cannot express: whatever ListVolumes
// hands back, a caller can copy a bundle into it.
//
// It is the assertion that fails on the defect this file was written around.
// Docker bind-mounts /etc/hostname, /etc/hosts and /etc/resolv.conf as single
// FILES; with "/" rejected as an overlay there was nothing left to shadow
// them, so all three reached the chooser wearing the host disk's 1 TB
// capacity. The export screen would have offered an operator /etc/resolv.conf
// as a drive to copy a bundle onto.
func TestLiveEveryEnumeratedVolumeIsAnExistingDirectory(t *testing.T) {
	skipIfNoProcfs(t)

	vols, err := ListVolumes()
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(vols) == 0 {
		t.Skip("no volumes enumerated on this machine; nothing to assert")
	}
	for _, v := range vols {
		fi, err := os.Stat(v.Path)
		if err != nil {
			// A volume that vanished between enumeration and now is the
			// pulled-stick case and is not a defect. Anything else is.
			if os.IsNotExist(err) {
				t.Logf("%s vanished between ListVolumes and Stat; ignoring", v.Path)
				continue
			}
			t.Errorf("%s: stat: %v", v.Path, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory (mode %s) and cannot receive a bundle: %s",
				v.Path, fi.Mode(), v.Summary())
		}
	}
}

// TestStatfsOneDistinguishesAFileFromADirectory pins the mechanism the guard
// above rests on, without needing a container, a bind mount or root.
//
// The first half is the reason a directory check was needed at all: statfs(2)
// answers happily for a regular file, because it describes the filesystem the
// file is on and not the file. So a capacity readout can never tell a
// destination apart from one bind-mounted file — it will cheerfully report
// the whole disk's free space for /etc/resolv.conf, which is exactly what the
// export screen was showing.
func TestStatfsOneDistinguishesAFileFromADirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-destination")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing the sample file: %v", err)
	}

	c := statfsOne(file)
	if c.err != nil {
		t.Fatalf("statfs(a regular file): %v", c.err)
	}
	if !c.ok || c.total == 0 {
		t.Errorf("statfs on a regular file reported no capacity (%+v); the premise of this test is that it reports the FILESYSTEM's", c)
	}
	if !c.statOK {
		t.Fatal("stat of a regular file did not answer")
	}
	if c.isDir {
		t.Error("a regular file was reported as a directory")
	}

	d := statfsOne(dir)
	if d.err != nil {
		t.Fatalf("statfs(a directory): %v", d.err)
	}
	if !d.statOK || !d.isDir {
		t.Errorf("a directory was reported as statOK=%v isDir=%v, want true/true", d.statOK, d.isDir)
	}

	// Both live on the same filesystem, so the capacity halves must agree.
	// If they ever diverge, the two syscalls are no longer describing the
	// same thing and the guard is reading the wrong one.
	if c.total != d.total {
		t.Errorf("the file and its directory reported different capacities: %d vs %d", c.total, d.total)
	}

	// A path that does not exist leaves both stat answers false, which is the
	// "keep it" case in listVolumes rather than the "drop it" one.
	m := statfsOne(filepath.Join(dir, "no-such-entry"))
	if m.statOK || m.isDir {
		t.Errorf("a missing path reported statOK=%v isDir=%v, want false/false", m.statOK, m.isDir)
	}
}

func volumePaths(vols []Volume) []string {
	out := make([]string, len(vols))
	for i, v := range vols {
		out[i] = v.Path
	}
	return out
}
