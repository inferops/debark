package repository

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestPoolPathRefusesTraversal is the regression test for the arbitrary-write
// bug the second security review found: a vendor .deb's Package: field reaches
// PoolPath unvalidated, PoolPath interpolated it straight into a path, and
// filepath.Join then resolved the parent references, writing outside the
// bundle on the builder host.
func TestPoolPathRefusesTraversal(t *testing.T) {
	hostile := []struct {
		name, pkg, filename string
	}{
		{"parent refs in name", "../../../../tmp/evil", "x_1_amd64.deb"},
		{"absolute name", "/etc/cron.d/evil", "x_1_amd64.deb"},
		{"separator in name", "a/b", "x_1_amd64.deb"},
		{"backslash in name", `a\b`, "x_1_amd64.deb"},
		{"parent refs in filename", "pkg", "../../../../tmp/evil.deb"},
		{"separator in filename", "pkg", "sub/evil.deb"},
		{"windows drive in filename", "pkg", `C:evil.deb`},
		{"newline in filename", "pkg", "evil\n_1_amd64.deb"},
		{"nul in name", "pk\x00g", "x_1_amd64.deb"},
		{"dotdot filename", "pkg", ".."},
		{"uppercase name", "Evil", "x_1_amd64.deb"},
		{"leading dash name", "-evil", "x_1_amd64.deb"},
	}

	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			got := PoolPath(tc.pkg, tc.filename)
			if got != "" {
				t.Fatalf("PoolPath(%q, %q) = %q, want \"\" (hostile input must be refused)", tc.pkg, tc.filename, got)
			}
			// Belt and braces: prove that had it returned a path, joining it
			// would in fact have escaped — i.e. this test is guarding a real
			// hazard, not an imaginary one.
			escaping := filepath.Join("/bundle/repo", filepath.FromSlash("pool/x/"+tc.pkg+"/"+tc.filename))
			if strings.Contains(tc.pkg, "..") && strings.HasPrefix(escaping, filepath.Clean("/bundle/repo")+string(filepath.Separator)) {
				t.Fatalf("expected %q to escape /bundle/repo, but it did not; the test fixture is wrong", escaping)
			}
		})
	}
}

func TestPoolPathAcceptsRealPackages(t *testing.T) {
	ok := []struct{ pkg, filename, want string }{
		{"jq", "jq_1.6-2.1+deb12u2_amd64.deb", "pool/j/jq/jq_1.6-2.1+deb12u2_amd64.deb"},
		{"libonig5", "libonig5_6.9.8-1_amd64.deb", "pool/libo/libonig5/libonig5_6.9.8-1_amd64.deb"},
		{"g++", "g++_4%3a12.2.0-3_amd64.deb", "pool/g/g++/g++_4%3a12.2.0-3_amd64.deb"},
		{"linux-headers-6.1.0-13-common", "linux-headers-6.1.0-13-common_6.1.55-1_all.deb",
			"pool/l/linux-headers-6.1.0-13-common/linux-headers-6.1.0-13-common_6.1.55-1_all.deb"},
	}
	for _, tc := range ok {
		if got := PoolPath(tc.pkg, tc.filename); got != tc.want {
			t.Errorf("PoolPath(%q, %q) = %q, want %q", tc.pkg, tc.filename, got, tc.want)
		}
	}
}
