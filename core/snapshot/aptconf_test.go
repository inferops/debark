package snapshot

import "testing"

func TestParseAPTBool(t *testing.T) {
	truthy := []string{"true", "TRUE", "yes", "Yes", "on", "1"}
	for _, s := range truthy {
		if v, ok := parseAPTBool(s); !ok || !v {
			t.Errorf("parseAPTBool(%q) = %v, %v; want true, true", s, v, ok)
		}
	}
	falsy := []string{"false", "FALSE", "no", "off", "0"}
	for _, s := range falsy {
		if v, ok := parseAPTBool(s); !ok || v {
			t.Errorf("parseAPTBool(%q) = %v, %v; want false, true", s, v, ok)
		}
	}
	if _, ok := parseAPTBool("maybe"); ok {
		t.Error(`parseAPTBool("maybe") should report ok=false`)
	}
}

func TestParseAPTConfIntoDottedForm(t *testing.T) {
	kv := map[string]string{}
	parseAPTConfInto([]byte(`APT::Install-Recommends "false";`+"\n"), kv)
	if kv["apt::install-recommends"] != "false" {
		t.Errorf("got %+v", kv)
	}
}

func TestParseAPTConfIntoNestedBlockForm(t *testing.T) {
	kv := map[string]string{}
	parseAPTConfInto([]byte(`
APT
{
  Install-Recommends "0";
  Get { Assume-Yes "true"; };
};
`), kv)
	if kv["apt::install-recommends"] != "0" {
		t.Errorf("apt::install-recommends = %q, want 0 (kv=%+v)", kv["apt::install-recommends"], kv)
	}
	if kv["apt::get::assume-yes"] != "true" {
		t.Errorf("apt::get::assume-yes = %q, want true (kv=%+v)", kv["apt::get::assume-yes"], kv)
	}
}

func TestParseAPTConfIntoCommentsAndProxyURLNotMistakenForComment(t *testing.T) {
	kv := map[string]string{}
	parseAPTConfInto([]byte(`
// a line comment
/* a block
   comment */
Acquire::http::Proxy "http://user:pass@proxy.example.com:3128/"; // trailing comment
`), kv)
	if kv["acquire::http::proxy"] != "http://user:pass@proxy.example.com:3128/" {
		t.Errorf("proxy value corrupted by comment stripping: %+v", kv)
	}
}

func TestParseAPTConfIntoListValuedKeyIsIgnoredNotCorrupting(t *testing.T) {
	kv := map[string]string{}
	parseAPTConfInto([]byte(`
DPkg::Options {
   "--force-confdef";
   "--force-confold";
};
APT::Install-Recommends "true";
`), kv)
	if kv["apt::install-recommends"] != "true" {
		t.Errorf("a list-valued block corrupted parsing of a later scalar key: %+v", kv)
	}
	if _, ok := kv["dpkg::options"]; ok {
		t.Errorf("a list-valued key should not appear as a scalar assignment: %+v", kv)
	}
}

func TestParseAPTConfIntoClear(t *testing.T) {
	kv := map[string]string{"apt::install-recommends": "false"}
	parseAPTConfInto([]byte(`#clear APT::Install-Recommends;`+"\n"), kv)
	if _, ok := kv["apt::install-recommends"]; ok {
		t.Errorf("#clear did not remove the key: %+v", kv)
	}
}

func TestDoInstallRecommends(t *testing.T) {
	mk := func(files map[string]string) (*Snapshot, *FileSet) {
		s := &Snapshot{}
		fs := &FileSet{Bytes: map[string][]byte{}}
		for path, content := range files {
			s.APT.Conf = append(s.APT.Conf, File{Path: path, ArchivePath: toArchivePath(path)})
			fs.Bytes[toArchivePath(path)] = []byte(content)
		}
		sortFilesByPath(s.APT.Conf)
		return s, fs
	}

	t.Run("default when unset", func(t *testing.T) {
		s, fs := mk(nil)
		v, explicit := doInstallRecommends(s, fs)
		if !v || explicit {
			t.Errorf("got value=%v explicit=%v, want true,false", v, explicit)
		}
	})

	t.Run("explicit false", func(t *testing.T) {
		s, fs := mk(map[string]string{
			"/etc/apt/apt.conf.d/99recommends": `APT::Install-Recommends "false";`,
		})
		v, explicit := doInstallRecommends(s, fs)
		if v || !explicit {
			t.Errorf("got value=%v explicit=%v, want false,true", v, explicit)
		}
	})

	t.Run("later file wins over earlier apt.conf", func(t *testing.T) {
		s, fs := mk(map[string]string{
			"/etc/apt/apt.conf":                `APT::Install-Recommends "true";`,
			"/etc/apt/apt.conf.d/99recommends": `APT::Install-Recommends "false";`,
		})
		v, explicit := doInstallRecommends(s, fs)
		if v || !explicit {
			t.Errorf("last-wins order not respected: value=%v explicit=%v", v, explicit)
		}
	})

	t.Run("later filename within apt.conf.d wins", func(t *testing.T) {
		s, fs := mk(map[string]string{
			"/etc/apt/apt.conf.d/10first":  `APT::Install-Recommends "true";`,
			"/etc/apt/apt.conf.d/99second": `APT::Install-Recommends "false";`,
		})
		v, _ := doInstallRecommends(s, fs)
		if v {
			t.Error("expected the alphabetically later file (99second) to win")
		}
	})

	t.Run("run-parts-invalid filename in apt.conf.d is ignored", func(t *testing.T) {
		s, fs := mk(map[string]string{
			"/etc/apt/apt.conf.d/99recommends":          `APT::Install-Recommends "false";`,
			"/etc/apt/apt.conf.d/99recommends.dpkg-new": `APT::Install-Recommends "true";`,
		})
		v, _ := doInstallRecommends(s, fs)
		if v {
			t.Error("a .dpkg-new backup file must not override the real apt.conf.d entry (apt itself would never read it)")
		}
	})

	t.Run("malformed value falls back to default", func(t *testing.T) {
		s, fs := mk(map[string]string{
			"/etc/apt/apt.conf.d/99recommends": `APT::Install-Recommends "sort-of";`,
		})
		v, explicit := doInstallRecommends(s, fs)
		if !v || explicit {
			t.Errorf("got value=%v explicit=%v, want default true,false", v, explicit)
		}
	})

	t.Run("nested block form end to end", func(t *testing.T) {
		s, fs := mk(map[string]string{
			"/etc/apt/apt.conf.d/99recommends": "APT\n{\n  Install-Recommends \"0\";\n};\n",
		})
		v, explicit := doInstallRecommends(s, fs)
		if v || !explicit {
			t.Errorf("got value=%v explicit=%v, want false,true", v, explicit)
		}
	})

	t.Run("nil snapshot or file set", func(t *testing.T) {
		if v, explicit := doInstallRecommends(nil, nil); !v || explicit {
			t.Errorf("nil inputs should return the safe default, got %v,%v", v, explicit)
		}
	})
}

func TestSolverMajorVersion(t *testing.T) {
	cases := []struct {
		in    string
		major int
		ok    bool
	}{
		{"2.8.3", 2, true},
		{"3.1.6ubuntu2", 3, true},
		{"3.2.0", 3, true},
		{"  2.6.1  ", 2, true},
		{"", 0, false},
		{"abc", 0, false},
		{"v2.6.1", 0, false},
	}
	for _, c := range cases {
		major, ok := solverMajorVersion(c.in)
		if ok != c.ok || (ok && major != c.major) {
			t.Errorf("solverMajorVersion(%q) = (%d, %v), want (%d, %v)", c.in, major, ok, c.major, c.ok)
		}
	}
}

// TestDefaultSolverForAPTVersion checks the three MEASURED data points from
// hack/experiments/out/e2/e2-run.log verbatim, plus the INFERRED
// generalisation for versions E2 did not test -- see
// defaultSolverForAPTVersion's doc for exactly which is which.
func TestDefaultSolverForAPTVersion(t *testing.T) {
	cases := []struct {
		version string
		want    string
		wantOK  bool
	}{
		// MEASURED (hack/experiments/out/e2/e2-run.log, 2026-09-03 run):
		{"2.8.3", solverInternal, true}, // Ubuntu 24.04/noble: no APT::Solver key at all
		{"3.1.6ubuntu2", solver3, true}, // Ubuntu 25.10/questing: binary::apt-get::APT::Solver "3.0"
		{"3.2.0", solver3, true},        // Ubuntu 26.04/resolute: bare APT::Solver "3.0"
		// INFERRED generalisation (major < 3 -> internal, major >= 3 -> 3.0):
		{"1.6~exp1+deb12u1", solverInternal, true},
		{"2.6.1", solverInternal, true},
		{"4.0.0", solver3, true},
		// unparseable:
		{"", "", false},
		{"not-a-version", "", false},
	}
	for _, c := range cases {
		got, ok := defaultSolverForAPTVersion(c.version)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("defaultSolverForAPTVersion(%q) = (%q, %v), want (%q, %v)", c.version, got, ok, c.want, c.wantOK)
		}
	}
}

func TestDoEffectiveSolver(t *testing.T) {
	mk := func(aptVersion string, files map[string]string) (*Snapshot, *FileSet) {
		s := &Snapshot{Target: Target{APTVersion: aptVersion}}
		fs := &FileSet{Bytes: map[string][]byte{}}
		for path, content := range files {
			s.APT.Conf = append(s.APT.Conf, File{Path: path, ArchivePath: toArchivePath(path)})
			fs.Bytes[toArchivePath(path)] = []byte(content)
		}
		sortFilesByPath(s.APT.Conf)
		return s, fs
	}

	t.Run("unset falls back to the target apt version's own default", func(t *testing.T) {
		s, fs := mk("2.8.3", nil)
		v, explicit := doEffectiveSolver(s, fs)
		if v != solverInternal || explicit {
			t.Errorf("got value=%q explicit=%v, want %q,false (apt 2.8.3 has no solver3)", v, explicit, solverInternal)
		}
	})

	t.Run("unset on a solver3-default apt version falls back to 3.0", func(t *testing.T) {
		s, fs := mk("3.2.0", nil)
		v, explicit := doEffectiveSolver(s, fs)
		if v != solver3 || explicit {
			t.Errorf("got value=%q explicit=%v, want %q,false", v, explicit, solver3)
		}
	})

	t.Run("explicit override beats the version default", func(t *testing.T) {
		s, fs := mk("3.2.0", map[string]string{
			"/etc/apt/apt.conf.d/99solver": `APT::Solver "internal";`,
		})
		v, explicit := doEffectiveSolver(s, fs)
		if v != solverInternal || !explicit {
			t.Errorf("got value=%q explicit=%v, want %q,true (explicit override must win even though 3.2.0 defaults to 3.0)", v, explicit, solverInternal)
		}
	})

	t.Run("later file within apt.conf.d wins", func(t *testing.T) {
		s, fs := mk("2.8.3", map[string]string{
			"/etc/apt/apt.conf.d/10first":  `APT::Solver "3.0";`,
			"/etc/apt/apt.conf.d/99second": `APT::Solver "internal";`,
		})
		v, explicit := doEffectiveSolver(s, fs)
		if v != solverInternal || !explicit {
			t.Errorf("expected the alphabetically later file (99second) to win: value=%q explicit=%v", v, explicit)
		}
	})

	t.Run("nested block form end to end", func(t *testing.T) {
		s, fs := mk("2.8.3", map[string]string{
			"/etc/apt/apt.conf.d/99solver": "APT\n{\n  Solver \"3.0\";\n};\n",
		})
		v, explicit := doEffectiveSolver(s, fs)
		if v != solver3 || !explicit {
			t.Errorf("got value=%q explicit=%v, want %q,true", v, explicit, solver3)
		}
	})

	t.Run("blank explicit value is treated as unset, not as an empty solver", func(t *testing.T) {
		s, fs := mk("3.2.0", map[string]string{
			"/etc/apt/apt.conf.d/99solver": `APT::Solver "";`,
		})
		v, explicit := doEffectiveSolver(s, fs)
		if v != solver3 || explicit {
			t.Errorf("got value=%q explicit=%v, want %q,false (blank APT::Solver falls back to the version default, matching questing's own apt-config dump shape)", v, explicit, solver3)
		}
	})

	t.Run("run-parts-invalid filename in apt.conf.d is ignored", func(t *testing.T) {
		s, fs := mk("2.8.3", map[string]string{
			"/etc/apt/apt.conf.d/99solver":          `APT::Solver "3.0";`,
			"/etc/apt/apt.conf.d/99solver.dpkg-new": `APT::Solver "internal";`,
		})
		v, _ := doEffectiveSolver(s, fs)
		if v != solver3 {
			t.Error("a .dpkg-new backup file must not override the real apt.conf.d entry (apt itself would never read it)")
		}
	})

	t.Run("unrecognised target apt version and no override is unknown", func(t *testing.T) {
		s, fs := mk("not-a-version", nil)
		v, explicit := doEffectiveSolver(s, fs)
		if v != "" || explicit {
			t.Errorf("got value=%q explicit=%v, want \"\",false", v, explicit)
		}
	})

	t.Run("nil snapshot or file set", func(t *testing.T) {
		if v, explicit := doEffectiveSolver(nil, nil); v != "" || explicit {
			t.Errorf("nil inputs should report unknown, got %q,%v", v, explicit)
		}
	})
}
