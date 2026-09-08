package apt

import "testing"

func TestCompareVersions(t *testing.T) {
	// less holds ordered chains: each entry must compare strictly less than
	// the next. Chosen from Debian Policy §5.6.12's own worked example
	// ("1.0~beta1~svn20101010 < 1.0~beta1 < 1.0") plus the properties the
	// algorithm is defined by: epoch dominates everything, digit runs compare
	// numerically (not lexicographically), and '~' sorts before anything,
	// including the end of a string.
	chain := []string{
		"1.0~beta1~svn20101010",
		"1.0~beta1",
		"1.0~rc1",
		"1.0",
		"1.0-1",
		"1.0-2",
		"1.0-10",
		"1.0a",
		"1.0b",
		"1.1",
		"1.9",
		"1.10",
		"1.10.1",
		"2.0",
		"1:0.0.1",
		"1:2.0",
		"2:0.0.1",
	}
	for i := 0; i+1 < len(chain); i++ {
		a, b := chain[i], chain[i+1]
		if c := CompareVersions(a, b); c >= 0 {
			t.Errorf("CompareVersions(%q, %q) = %d, want < 0", a, b, c)
		}
		if c := CompareVersions(b, a); c <= 0 {
			t.Errorf("CompareVersions(%q, %q) = %d, want > 0", b, a, c)
		}
	}

	equal := [][2]string{
		{"1.0", "1.0"},
		{"1.0", "1.0-0"}, // absence of a revision == revision 0
		{"1.0-1", "1.0-1"},
		{"0:1.0", "1.0"},    // explicit epoch 0 == no epoch
		{"1.0-01", "1.0-1"}, // leading zeros in a digit run don't matter
	}
	for _, e := range equal {
		if c := CompareVersions(e[0], e[1]); c != 0 {
			t.Errorf("CompareVersions(%q, %q) = %d, want 0", e[0], e[1], c)
		}
		if !EqualVersions(e[0], e[1]) {
			t.Errorf("EqualVersions(%q, %q) = false, want true", e[0], e[1])
		}
	}
}

func TestCompareVersions_MalformedFallsBack(t *testing.T) {
	// pault.ag/go/debian/version.Parse rejects embedded spaces and other
	// input real dpkg would also reject; CompareVersions must still return a
	// sane, non-panicking answer via its own fallback rather than erroring.
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"1.0", "1.0", 0},
		{"1.0", "2.0", -1},
	}
	for _, c := range cases {
		if got := sign(CompareVersions(c.a, c.b)); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAptMajorMinor(t *testing.T) {
	cases := []struct {
		in         string
		major, min string
		ok         bool
	}{
		{"2.6.1", "2", "6", true},
		{"2.7.14build2", "2", "7", true},
		{"1.6~exp1+deb12u1", "1", "6", true},
		{"2.9.33", "2", "9", true},
		{"  2.4.13  ", "2", "4", true},
		{"2", "", "", false},
		{"", "", "", false},
		{"abc", "", "", false},
		{"v2.6.1", "", "", false},
	}
	for _, c := range cases {
		major, minor, ok := aptMajorMinor(c.in)
		if ok != c.ok || (ok && (major != c.major || minor != c.min)) {
			t.Errorf("aptMajorMinor(%q) = (%q, %q, %v), want (%q, %q, %v)", c.in, major, minor, ok, c.major, c.min, c.ok)
		}
	}
}
