package apt

import (
	"regexp"
	"strings"

	debversion "pault.ag/go/debian/version"
)

// CompareVersions compares two Debian package version strings. It delegates
// to pault.ag/go/debian/version, which implements the algorithm in Debian
// Policy §5.6.12 (the same one dpkg --compare-versions uses): epoch, then
// upstream version, then debian revision, each compared by alternating runs
// of non-digits (ordered so that '~' sorts before everything, including the
// end of a string) and runs of digits (compared numerically).
//
// debark never runs a solver — apt alone decides what to install — but the
// tool still has to compare two version strings it already has in hand
// (matching an apt-reported Inst-line version against a Packages-index
// stanza, presenting a stable order in diagnostics), and getting that
// comparison wrong is exactly the kind of silent error the project exists to
// avoid.
//
// pault.ag's Parse rejects anything dpkg itself would also reject (embedded
// spaces, a version not starting with a digit, a disallowed character). Such
// input should never occur in anything apt produced, but a malformed
// third-party version string must not panic or abort a build, so that case
// falls back to a permissive, self-contained implementation of the same
// algorithm instead of propagating the parse error.
func CompareVersions(a, b string) int {
	av, aerr := debversion.Parse(a)
	bv, berr := debversion.Parse(b)
	if aerr == nil && berr == nil {
		return sign(debversion.Compare(av, bv))
	}
	return compareVersionsFallback(a, b)
}

// compareVersionsFallback is the same three-component comparison, used only
// when pault.ag/go/debian/version.Parse rejects one of the inputs.
func compareVersionsFallback(a, b string) int {
	aEpoch, aRest := splitEpoch(a)
	bEpoch, bRest := splitEpoch(b)
	if c := verrevcmp(aEpoch, bEpoch); c != 0 {
		return sign(c)
	}
	aUp, aRev := splitRevision(aRest)
	bUp, bRev := splitRevision(bRest)
	if c := verrevcmp(aUp, bUp); c != 0 {
		return sign(c)
	}
	return sign(verrevcmp(aRev, bRev))
}

// EqualVersions reports whether a and b are the same version per
// CompareVersions, without the caller needing to spell out == 0.
func EqualVersions(a, b string) bool { return CompareVersions(a, b) == 0 }

func splitEpoch(v string) (epoch, rest string) {
	if i := strings.IndexByte(v, ':'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return "0", v
}

func splitRevision(rest string) (upstream, revision string) {
	if i := strings.LastIndexByte(rest, '-'); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	// "the absence of a debian_revision is equivalent to a debian_revision
	// of 0" (Debian Policy §5.6.12). verrevcmp("", "0") == 0 by construction
	// (the leading-zero-skip step consumes the sole '0'), so either works;
	// "0" is used because it is what a reader expects to see.
	return rest, "0"
}

func verOrder(b byte) int {
	switch {
	case b == '~':
		return -1
	case b == 0:
		return 0
	case b >= '0' && b <= '9':
		return 0
	case (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z'):
		return int(b)
	default:
		return int(b) + 256
	}
}

func isDigitByte(b byte) bool { return b >= '0' && b <= '9' }

// verrevcmp implements dpkg's verrevcmp (lib/dpkg/version.c): compare two
// strings by alternating between runs of non-digit characters (ordered by
// verOrder) and runs of digit characters (compared numerically, ignoring
// leading zeros).
func verrevcmp(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for (i < len(a) && !isDigitByte(a[i])) || (j < len(b) && !isDigitByte(b[j])) {
			var ac, bc int
			if i < len(a) {
				ac = verOrder(a[i])
			}
			if j < len(b) {
				bc = verOrder(b[j])
			}
			if ac != bc {
				return ac - bc
			}
			if i < len(a) {
				i++
			}
			if j < len(b) {
				j++
			}
		}
		for i < len(a) && a[i] == '0' {
			i++
		}
		for j < len(b) && b[j] == '0' {
			j++
		}
		firstDiff := 0
		for i < len(a) && isDigitByte(a[i]) && j < len(b) && isDigitByte(b[j]) {
			if firstDiff == 0 {
				firstDiff = int(a[i]) - int(b[j])
			}
			i++
			j++
		}
		if i < len(a) && isDigitByte(a[i]) {
			return 1
		}
		if j < len(b) && isDigitByte(b[j]) {
			return -1
		}
		if firstDiff != 0 {
			return firstDiff
		}
	}
	return 0
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

var majorMinorRE = regexp.MustCompile(`^(\d+)\.(\d+)`)

// aptMajorMinor extracts the "major.minor" component pair from an apt or
// dpkg version string such as "2.7.14build2", "2.9.33" or "1.6~exp1+deb12u1".
// It returns ok=false when the string does not start with at least
// "<digits>.<digits>".
func aptMajorMinor(v string) (major, minor string, ok bool) {
	m := majorMinorRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}
