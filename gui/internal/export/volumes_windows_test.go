//go:build windows

package export

import (
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func utf16Block(ss ...string) []uint16 {
	var out []uint16
	for _, s := range ss {
		for _, r := range s {
			out = append(out, uint16(r))
		}
		out = append(out, 0)
	}
	return append(out, 0) // the second terminator
}

func TestSplitNulUTF16(t *testing.T) {
	tests := []struct {
		name string
		in   []uint16
		want []string
	}{
		{
			name: "a typical machine",
			in:   utf16Block(`C:\`, `D:\`, `Z:\`),
			want: []string{`C:\`, `D:\`, `Z:\`},
		},
		{
			name: "a single drive",
			in:   utf16Block(`C:\`),
			want: []string{`C:\`},
		},
		{
			name: "empty block",
			in:   []uint16{0, 0},
			want: nil,
		},
		{
			name: "nothing at all",
			in:   nil,
			want: nil,
		},
		{
			name: "an unterminated final entry is still returned",
			in:   []uint16{'C', ':', '\\'},
			want: []string{`C:\`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitNulUTF16(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitNulUTF16 = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKindForDriveType(t *testing.T) {
	tests := []struct {
		name     string
		t        uint32
		wantKind VolumeKind
		wantKeep bool
	}{
		{"removable", windows.DRIVE_REMOVABLE, KindRemovable, true},
		{"fixed", windows.DRIVE_FIXED, KindFixed, true},
		{"network", windows.DRIVE_REMOTE, KindNetwork, true},
		{"optical is read-only by construction", windows.DRIVE_CDROM, KindUnknown, false},
		{"a RAM disk evaporates on reboot", windows.DRIVE_RAMDISK, KindUnknown, false},
		{"no root directory", windows.DRIVE_NO_ROOT_DIR, KindUnknown, false},
		{"unknown", windows.DRIVE_UNKNOWN, KindUnknown, false},
		{"an out-of-range value must not be offered", 99, KindUnknown, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, keep := kindForDriveType(tc.t)
			if kind != tc.wantKind || keep != tc.wantKeep {
				t.Errorf("kindForDriveType(%d) = (%v, %v), want (%v, %v)", tc.t, kind, keep, tc.wantKind, tc.wantKeep)
			}
		})
	}
}

// TestLiveWindowsEnumeration runs against the machine's real drives. It is
// guarded because what is attached varies, and because CI runners have no
// removable media at all: it asserts only the invariants that must hold for
// every machine.
func TestLiveWindowsEnumeration(t *testing.T) {
	if testing.Short() {
		t.Skip("touches the live system")
	}

	roots, err := logicalDriveRoots()
	if err != nil {
		t.Fatalf("logicalDriveRoots: %v", err)
	}
	t.Logf("logical drives: %q", roots)
	if len(roots) == 0 {
		t.Skip("no logical drives reported; not a machine this test can say anything about")
	}

	vols, err := listVolumes()
	if err != nil {
		t.Fatalf("listVolumes: %v", err)
	}
	for _, v := range vols {
		t.Log(v.Summary())
		if v.Label == "" {
			t.Errorf("%s has an empty label", v.Path)
		}
		if v.FSType == "" {
			t.Errorf("%s has an empty filesystem type", v.Path)
		}
		if v.FreeBytes > v.TotalBytes {
			t.Errorf("%s: %d free of %d total", v.Path, v.FreeBytes, v.TotalBytes)
		}
	}

	// The system drive is always present and always a destination, so it is
	// the one row that can be asserted on any Windows machine.
	found := false
	for _, v := range vols {
		if isSystemRoot(v.Path) {
			found = true
			if v.Kind != KindFixed {
				t.Errorf("the system drive is classified %v, want fixed", v.Kind)
			}
			if v.TotalBytes == 0 {
				t.Error("the system drive reports zero capacity")
			}
		}
	}
	if !found {
		t.Error("the system drive was not enumerated")
	}
}
