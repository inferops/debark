package apt

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Goldens under testdata/golden/ are real apt-get output, not hand-written
// fixtures. They were captured 2026-09-03 from apt 2.6.1 in a
// debian:bookworm-slim container, resolving inside a private root built from
// the checked-in testdata/real-targets/debian-12-state.tar.gz fixture (the
// same private-root construction this package's root.go implements),
// against the real Debian archive. See docs/dev/prototype-baseline.md for
// the companion end-to-end run this matches (jq + tree -> exactly jq,
// libjq1, libonig5, tree).
//
// Each file has a "### apt-get <args> ###" header line this test strips, and
// a trailing "EXIT:<code>" line this test reads back as Output.ExitCode;
// everything between is apt's real, unmodified stdout+stderr.

var goldenExitRE = regexp.MustCompile(`^EXIT:(-?\d+)$`)

func loadGolden(t *testing.T, name string) Output {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "###") {
		t.Fatalf("golden %s: missing header line", name)
	}
	last := lines[len(lines)-1]
	m := goldenExitRE.FindStringSubmatch(last)
	if m == nil {
		t.Fatalf("golden %s: missing trailing EXIT: line", name)
	}
	code, _ := strconv.Atoi(m[1])
	body := strings.Join(lines[1:len(lines)-1], "\n")
	return Output{Stdout: []byte(body), ExitCode: code}
}

func TestParseUpdate_Verbose(t *testing.T) {
	res := ParseUpdate(loadGolden(t, "02_update_verbose.txt"))
	if res.Failed {
		t.Fatalf("expected success, got Failed=true (entries=%+v)", res.Entries)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(res.Entries), res.Entries)
	}
	wantSuites := []string{"bookworm", "bookworm-updates", "bookworm-security"}
	for i, want := range wantSuites {
		if res.Entries[i].Status != "hit" {
			t.Errorf("entry %d: status = %q, want hit", i, res.Entries[i].Status)
		}
		if res.Entries[i].Suite != want {
			t.Errorf("entry %d: suite = %q, want %q", i, res.Entries[i].Suite, want)
		}
	}
	if res.Entries[0].URI != "http://deb.debian.org/debian" {
		t.Errorf("entry 0: URI = %q", res.Entries[0].URI)
	}
}

func TestParseUpdate_GPGError(t *testing.T) {
	res := ParseUpdate(loadGolden(t, "12_update_wrongkey.txt"))
	if !res.Failed {
		t.Fatal("expected Failed=true for a GPG error")
	}
	if len(res.GPGErrors) != 1 {
		t.Fatalf("expected 1 GPGError, got %d: %+v", len(res.GPGErrors), res.GPGErrors)
	}
	ge := res.GPGErrors[0]
	wantKeys := []string{"6ED0E7B82643E131", "78DBA3BC47EF2265"}
	if len(ge.NoPubkeys) != len(wantKeys) {
		t.Fatalf("NoPubkeys = %v, want %v", ge.NoPubkeys, wantKeys)
	}
	for i, k := range wantKeys {
		if ge.NoPubkeys[i] != k {
			t.Errorf("NoPubkeys[%d] = %q, want %q", i, ge.NoPubkeys[i], k)
		}
	}
	if !strings.Contains(res.Summary, "is not signed") {
		t.Errorf("Summary = %q, want it to mention 'is not signed'", res.Summary)
	}
	if !strings.Contains(res.Error(), "6ED0E7B82643E131") {
		t.Errorf("Error() = %q, want it to mention the missing key", res.Error())
	}
}

func TestParseSimulate_InstallJqTree(t *testing.T) {
	res := ParseSimulate(loadGolden(t, "03_sim_install_jq_tree.txt"))
	if res.Failed {
		t.Fatalf("expected success, got Failed=true: %+v", res)
	}
	insts := map[string]InstAction{}
	confs := 0
	for _, a := range res.Actions {
		switch a.Kind {
		case "Inst":
			insts[a.Name] = a
		case "Conf":
			confs++
		}
	}
	if len(insts) != 4 {
		t.Fatalf("expected 4 Inst actions, got %d: %+v", len(insts), res.Actions)
	}
	if confs != 4 {
		t.Fatalf("expected 4 Conf actions, got %d", confs)
	}
	jq, ok := insts["jq"]
	if !ok {
		t.Fatal("no Inst action for jq")
	}
	if jq.Version != "1.6-2.1+deb12u2" {
		t.Errorf("jq version = %q", jq.Version)
	}
	if jq.Arch != "amd64" {
		t.Errorf("jq arch = %q", jq.Arch)
	}
	if jq.OldVersion != "" {
		t.Errorf("jq OldVersion = %q, want empty (new install)", jq.OldVersion)
	}
	wantOrigins := []string{"Debian:12.15/oldstable", "Debian-Security:12/oldstable-security"}
	if len(jq.Origins) != len(wantOrigins) {
		t.Fatalf("jq origins = %v, want %v", jq.Origins, wantOrigins)
	}
	for i, o := range wantOrigins {
		if jq.Origins[i] != o {
			t.Errorf("jq origin[%d] = %q, want %q", i, jq.Origins[i], o)
		}
	}
	for _, name := range []string{"libjq1", "libonig5", "tree"} {
		if _, ok := insts[name]; !ok {
			t.Errorf("no Inst action for %s", name)
		}
	}
}

func TestParseSimulate_MissingPackage(t *testing.T) {
	res := ParseSimulate(loadGolden(t, "05_sim_missing_pkg.txt"))
	if !res.Failed {
		t.Fatal("expected Failed=true")
	}
	if len(res.Unresolved) != 1 {
		t.Fatalf("expected 1 Unresolved, got %+v", res.Unresolved)
	}
	if res.Unresolved[0].Input != "nonexistent-package-xyz123" {
		t.Errorf("Unresolved[0].Input = %q", res.Unresolved[0].Input)
	}
}

func TestParseSimulate_BadVersion(t *testing.T) {
	res := ParseSimulate(loadGolden(t, "06_sim_bad_version.txt"))
	if !res.Failed {
		t.Fatal("expected Failed=true")
	}
	if len(res.Unresolved) != 1 || res.Unresolved[0].Input != "jq" {
		t.Fatalf("Unresolved = %+v, want one entry for jq", res.Unresolved)
	}
	if !strings.Contains(res.Unresolved[0].Detail, "99.99.99-doesnotexist") {
		t.Errorf("Detail = %q, want it to mention the requested version", res.Unresolved[0].Detail)
	}
}

func TestParseSimulate_FullUpgrade(t *testing.T) {
	res := ParseSimulate(loadGolden(t, "07_sim_full_upgrade.txt"))
	if res.Failed {
		t.Fatalf("expected success: %+v", res)
	}
	var insts []InstAction
	for _, a := range res.Actions {
		if a.Kind == "Inst" {
			insts = append(insts, a)
		}
	}
	if len(insts) != 2 {
		t.Fatalf("expected 2 Inst actions, got %d: %+v", len(insts), insts)
	}
	byName := map[string]InstAction{}
	for _, a := range insts {
		byName[a.Name] = a
	}
	bf, ok := byName["base-files"]
	if !ok {
		t.Fatal("no Inst action for base-files")
	}
	if bf.OldVersion != "12.4+deb12u14" {
		t.Errorf("base-files OldVersion = %q", bf.OldVersion)
	}
	if bf.Version != "12.4+deb12u15" {
		t.Errorf("base-files Version = %q", bf.Version)
	}
}

func TestParseSimulate_UnmetDependencies(t *testing.T) {
	res := ParseSimulate(loadGolden(t, "11_sim_unmet_deps.txt"))
	if !res.Failed {
		t.Fatal("expected Failed=true")
	}
	if len(res.Unresolved) != 1 {
		t.Fatalf("expected 1 Unresolved, got %+v", res.Unresolved)
	}
	u := res.Unresolved[0]
	if u.Input != "zzz-fake-unmet-pkg" {
		t.Errorf("Input = %q", u.Input)
	}
	if len(u.Missing) != 1 || u.Missing[0] != "this-package-does-not-exist-anywhere-xyz" {
		t.Errorf("Missing = %v", u.Missing)
	}
	if !strings.Contains(res.Summary, "Unable to correct problems") {
		t.Errorf("Summary = %q", res.Summary)
	}
}

func TestParsePrintURIs(t *testing.T) {
	entries := ParsePrintURIs(loadGolden(t, "04_printuris_jq_tree.txt"))
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d: %+v", len(entries), entries)
	}
	byFile := map[string]URIEntry{}
	for _, e := range entries {
		byFile[e.Filename] = e
	}
	jq, ok := byFile["jq_1.6-2.1+deb12u2_amd64.deb"]
	if !ok {
		t.Fatalf("no entry for jq's filename; got %+v", entries)
	}
	wantURI := "http://deb.debian.org/debian/pool/main/j/jq/jq_1.6-2.1%2bdeb12u2_amd64.deb"
	if jq.URI != wantURI {
		t.Errorf("jq URI = %q, want %q", jq.URI, wantURI)
	}
	if jq.Size != 63984 {
		t.Errorf("jq Size = %d, want 63984", jq.Size)
	}
	if jq.HashAlgo != "MD5Sum" {
		t.Errorf("jq HashAlgo = %q, want MD5Sum (apt's default for --print-uris)", jq.HashAlgo)
	}
	if jq.Hash != "b9aca00e056b5365d65597df4b338cee" {
		t.Errorf("jq Hash = %q", jq.Hash)
	}
}

func TestParseAptGetVersion(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "apt_version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := ParseAptGetVersion(data)
	if !ok || v != "2.6.1" {
		t.Fatalf("ParseAptGetVersion = %q, %v; want 2.6.1, true", v, ok)
	}
}

func TestParseDpkgVersion(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "dpkg_version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := ParseDpkgVersion(data)
	if !ok || v != "1.21.23" {
		t.Fatalf("ParseDpkgVersion = %q, %v; want 1.21.23, true", v, ok)
	}
}

func TestDeriveSuiteComponent(t *testing.T) {
	cases := []struct {
		file          string
		suite, compon string
	}{
		{"deb.debian.org_debian_dists_bookworm_main_binary-amd64_Packages", "bookworm", "main"},
		{"deb.debian.org_debian-security_dists_bookworm-security_main_binary-amd64_Packages", "bookworm-security", "main"},
		{"deb.debian.org_debian_dists_bookworm-updates_main_binary-amd64_Packages", "bookworm-updates", "main"},
		{"archive.ubuntu.com_ubuntu_dists_noble-backports_universe_binary-arm64_Packages", "noble-backports", "universe"},
		{"not-a-packages-index.txt", "", ""},
	}
	for _, c := range cases {
		suite, comp, ok := deriveSuiteComponent(c.file)
		if c.suite == "" {
			if ok {
				t.Errorf("%s: expected no match, got suite=%q component=%q", c.file, suite, comp)
			}
			continue
		}
		if !ok || suite != c.suite || comp != c.compon {
			t.Errorf("%s: got suite=%q component=%q ok=%v, want %q %q", c.file, suite, comp, ok, c.suite, c.compon)
		}
	}
}
