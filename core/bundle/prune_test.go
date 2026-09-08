package bundle

import (
	"sort"
	"testing"
)

func names(entries []PoolEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	sort.Strings(out)
	return out
}

func TestPruneTableDriven(t *testing.T) {
	cases := []struct {
		name   string
		files  []PoolEntry
		remove []string // Path values expected in the removal set
	}{
		{
			name: "simple supersede keeps only the highest version",
			files: []PoolEntry{
				{Name: "vlc", Arch: "amd64", Version: "3.0.20", Path: "old"},
				{Name: "vlc", Arch: "amd64", Version: "3.0.21", Path: "new"},
			},
			remove: []string{"old"},
		},
		{
			name: "equal versions at different paths: neither is superseded",
			files: []PoolEntry{
				{Name: "vlc", Arch: "amd64", Version: "3.0.21", Path: "a"},
				{Name: "vlc", Arch: "amd64", Version: "3.0.21", Path: "b"},
			},
			remove: nil,
		},
		{
			name: "epoch dominates the upstream version entirely",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "2.0-1", Path: "no-epoch"},
				{Name: "foo", Arch: "amd64", Version: "1:1.0-1", Path: "epoch"},
			},
			remove: []string{"no-epoch"},
		},
		{
			name: "tilde pre-release sorts before the final release",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "1.0~beta1", Path: "beta1"},
				{Name: "foo", Arch: "amd64", Version: "1.0~beta2", Path: "beta2"},
				{Name: "foo", Arch: "amd64", Version: "1.0", Path: "final"},
			},
			remove: []string{"beta1", "beta2"},
		},
		{
			name: "tilde pre-release alone: highest tilde survives",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "1.0~beta1", Path: "beta1"},
				{Name: "foo", Arch: "amd64", Version: "1.0~beta2", Path: "beta2"},
			},
			remove: []string{"beta1"},
		},
		{
			name: "user-supplied older version alongside a newer archive one is protected",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "1.0", Path: "user-old", UserSupplied: true},
				{Name: "foo", Arch: "amd64", Version: "2.0", Path: "archive-new"},
			},
			remove: nil,
		},
		{
			name: "user-supplied newer version supersedes an older archive one",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "3.0", Path: "user-new", UserSupplied: true},
				{Name: "foo", Arch: "amd64", Version: "2.0", Path: "archive-old"},
			},
			remove: []string{"archive-old"},
		},
		{
			name: "two user-supplied versions: the older is still never removed",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "1.0", Path: "user-old", UserSupplied: true},
				{Name: "foo", Arch: "amd64", Version: "2.0", Path: "user-new", UserSupplied: true},
			},
			remove: nil,
		},
		{
			name: "different architectures are independent groups",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "1.0", Path: "amd64-old"},
				{Name: "foo", Arch: "amd64", Version: "2.0", Path: "amd64-new"},
				{Name: "foo", Arch: "arm64", Version: "1.0", Path: "arm64-only"},
			},
			remove: []string{"amd64-old"},
		},
		{
			name: "different package names are independent groups",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "1.0", Path: "foo-only"},
				{Name: "bar", Arch: "amd64", Version: "1.0", Path: "bar-only"},
			},
			remove: nil,
		},
		{
			name: "an unparseable version is left alone and ignored for comparison",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "not a version", Path: "garbage"},
				{Name: "foo", Arch: "amd64", Version: "1.0", Path: "good"},
			},
			remove: nil,
		},
		{
			name: "an unparseable version never protects a real older version either",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "not a version", Path: "garbage"},
				{Name: "foo", Arch: "amd64", Version: "1.0", Path: "old"},
				{Name: "foo", Arch: "amd64", Version: "2.0", Path: "new"},
			},
			remove: []string{"old"},
		},
		{
			name:   "empty input",
			files:  nil,
			remove: nil,
		},
		{
			name: "single file is never removed",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "1.0", Path: "only"},
			},
			remove: nil,
		},
		{
			name: "revision component breaks a version tie",
			files: []PoolEntry{
				{Name: "foo", Arch: "amd64", Version: "1.0-1", Path: "rev1"},
				{Name: "foo", Arch: "amd64", Version: "1.0-2build1", Path: "rev2"},
			},
			remove: []string{"rev1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := names(Prune(tc.files))
			want := append([]string(nil), tc.remove...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("Prune() removed %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("Prune() removed %v, want %v", got, want)
				}
			}
		})
	}
}

// TestPruneNeverReturnsUserSupplied is a focused property check across every
// case above plus a few more: no matter what else is going on, an entry
// marked UserSupplied never appears in Prune's return value.
func TestPruneNeverReturnsUserSupplied(t *testing.T) {
	files := []PoolEntry{
		{Name: "a", Arch: "amd64", Version: "1.0", Path: "a-old-user", UserSupplied: true},
		{Name: "a", Arch: "amd64", Version: "2.0", Path: "a-new-archive"},
		{Name: "a", Arch: "amd64", Version: "0.5", Path: "a-oldest-user", UserSupplied: true},
		{Name: "b", Arch: "amd64", Version: "5.0", Path: "b-user", UserSupplied: true},
	}
	removed := Prune(files)
	for _, e := range removed {
		if e.UserSupplied {
			t.Errorf("Prune returned a user-supplied entry for removal: %+v", e)
		}
	}
}

// TestPruneKeepsExactlyOnePerGroupWhenAllParse is a property check: with N
// distinct, all-parseable versions in one (name, arch) group and none
// user-supplied or lock-referenced, Prune removes exactly N-1 of them. Both
// of those flags are exemptions from the numeric rule, so the property is
// stated over entries carrying neither.
func TestPruneKeepsExactlyOnePerGroupWhenAllParse(t *testing.T) {
	files := []PoolEntry{
		{Name: "foo", Arch: "amd64", Version: "1.0", Path: "v1"},
		{Name: "foo", Arch: "amd64", Version: "1.1", Path: "v2"},
		{Name: "foo", Arch: "amd64", Version: "1.2", Path: "v3"},
		{Name: "foo", Arch: "amd64", Version: "1.3", Path: "v4"},
	}
	removed := Prune(files)
	if len(removed) != len(files)-1 {
		t.Fatalf("removed %d of %d, want %d", len(removed), len(files), len(files)-1)
	}
	for _, e := range removed {
		if e.Version == "1.3" {
			t.Errorf("Prune removed the highest version %q", e.Version)
		}
	}
}

// TestPruneNeverReturnsLockReferenced is the LockReferenced counterpart to
// TestPruneNeverReturnsUserSupplied. The interesting case is the third
// entry: "a" 0.5 is the current plan's selection and so the version the
// lock will name, while a numerically higher 2.0 is sitting in the pool
// from an earlier, non-pruning run. A rule that only compared versions
// would remove exactly the file the bundle is being built to ship.
func TestPruneNeverReturnsLockReferenced(t *testing.T) {
	files := []PoolEntry{
		{Name: "a", Arch: "amd64", Version: "2.0", Path: "a-leftover-higher"},
		{Name: "a", Arch: "amd64", Version: "1.0", Path: "a-leftover-middle"},
		{Name: "a", Arch: "amd64", Version: "0.5", Path: "a-planned", LockReferenced: true},
		{Name: "b", Arch: "amd64", Version: "1:0.1", Path: "b-planned", LockReferenced: true},
		{Name: "b", Arch: "amd64", Version: "9.9", Path: "b-leftover"},
	}
	for _, e := range Prune(files) {
		if e.LockReferenced {
			t.Errorf("Prune returned a lock-referenced entry for removal: %+v", e)
		}
	}
	// The leftovers that are neither highest nor referenced still go.
	// "b" is the epoch case: the plan's 1:0.1 outranks the 9.9 leftover, so
	// protecting a lock-referenced entry does not stop it superseding.
	want := []string{"a-leftover-middle", "b-leftover"}
	got := names(Prune(files))
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Prune removed %v, want %v", got, want)
	}
}

// TestPruneLockReferencedStillSupersedes checks the other half of the rule:
// being lock-referenced protects the entry itself, it does not stop it
// counting as the highest version and pruning an older archive sibling.
func TestPruneLockReferencedStillSupersedes(t *testing.T) {
	files := []PoolEntry{
		{Name: "foo", Arch: "amd64", Version: "1.0", Path: "old"},
		{Name: "foo", Arch: "amd64", Version: "2.0", Path: "planned", LockReferenced: true},
	}
	if got := names(Prune(files)); len(got) != 1 || got[0] != "old" {
		t.Errorf("Prune removed %v, want exactly [old]", got)
	}
}
