package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzParseAPTConfInto fuzzes the apt.conf(5) tokeniser/parser with the real
// captured fixture already in the tree (testdata/recommends-false's
// 99recommends) as a seed, plus hand-picked edge cases for the grammar
// subset it handles (dotted and nested-block scalar assignment, quoted
// strings with escapes, // and /* */ comments, #include/#include-dir/
// #clear). The invariant is joinAPTKey's own explicit guarantee: every key
// this function writes into the output map is lower-cased apt::style,
// because doInstallRecommends looks up exactly "apt::install-recommends"
// and would silently stop working (not crash — just quietly return the
// wrong default) if a differently-cased key ever slipped through.
func FuzzParseAPTConfInto(f *testing.F) {
	if real, err := os.ReadFile(filepath.Join("testdata", "recommends-false", "etc", "apt", "apt.conf.d", "99recommends")); err == nil {
		f.Add(real)
	}
	f.Add([]byte(`APT::Install-Recommends "false";`))
	f.Add([]byte(`APT { Install-Recommends "0"; };`))
	f.Add([]byte("// comment\nAPT::Install-Recommends \"true\";\n"))
	f.Add([]byte("/* block\ncomment */APT::Install-Recommends \"true\";"))
	f.Add([]byte(`Acquire::http::Proxy "http://user:pass@proxy.example/";`))
	f.Add([]byte(`DPkg::Options { "--force-confdef"; "--force-confold"; };`))
	f.Add([]byte("#clear APT::Install-Recommends;"))
	f.Add([]byte(`#include "other.conf";`))
	f.Add([]byte(`#include-dir "conf.d";`))
	f.Add([]byte(`APT::Key1 "va\"lue with escaped quote";`))
	f.Add([]byte(`A::B::C "1"; A::B::C { D "2"; };`))
	f.Add([]byte(""))
	f.Add([]byte("}}}}{{{{;;;;"))
	f.Add([]byte(`"unterminated quote`))
	f.Add([]byte("/* unterminated block comment"))
	f.Add([]byte(`APT::Install-Recommends "true" "extra";`))

	f.Fuzz(func(t *testing.T, data []byte) {
		out := map[string]string{}
		parseAPTConfInto(data, out) // must not panic
		for k := range out {
			if k != strings.ToLower(k) {
				t.Fatalf("parseAPTConfInto produced a non-lowercase key %q from input %q", k, data)
			}
		}
	})
}
