package apt

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// readGoldenForFuzz mirrors parse_test.go's loadGolden, but reports failure
// by a bool instead of t.Fatalf, so it can run at FuzzXxx(f *testing.F) seed
// -registration time, before any per-iteration *testing.T exists.
func readGoldenForFuzz(name string) (Output, bool) {
	data, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if err != nil {
		return Output{}, false
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "###") {
		return Output{}, false
	}
	m := goldenExitRE.FindStringSubmatch(lines[len(lines)-1])
	if m == nil {
		return Output{}, false
	}
	code, _ := strconv.Atoi(m[1])
	body := strings.Join(lines[1:len(lines)-1], "\n")
	return Output{Stdout: []byte(body), ExitCode: code}, true
}

func addGoldenSeeds(f *testing.F, names ...string) {
	for _, n := range names {
		if out, ok := readGoldenForFuzz(n); ok {
			f.Add(out.Stdout, out.ExitCode)
		}
	}
}

// FuzzParseUpdate fuzzes "apt-get update" output parsing with every real
// captured golden fixture in the tree (testdata/golden, §"real apt-get
// output, not hand-written" per parse_test.go's own comment) as seed
// material, plus the raw exit code apt reported alongside each. The
// invariant is ParseUpdate's own explicit rule, directly visible in its
// body: a non-zero process exit code always means Failed. A parser that
// ever silently reported Failed=false for a process that actually exited
// non-zero would let the engine treat a broken "apt-get update" as if
// nothing were wrong.
func FuzzParseUpdate(f *testing.F) {
	addGoldenSeeds(f, "02_update_verbose.txt", "12_update_wrongkey.txt")
	f.Add([]byte(""), 0)
	f.Add([]byte("Hit:1 http://deb.debian.org/debian bookworm InRelease\n"), 0)
	f.Add([]byte("Err:1 http://deb.debian.org/debian bookworm InRelease\n  404  Not Found\n"), 100)
	f.Add([]byte("W: GPG error: repo: NO_PUBKEY DEADBEEF\nE: broken\n"), 100)
	f.Add([]byte("\x00\x01garbage\xffnot utf8\xfe\n"), 1)
	f.Add([]byte(strings.Repeat("Hit:1 x\n", 500)), 0)

	f.Fuzz(func(t *testing.T, stdout []byte, exitCode int) {
		res := ParseUpdate(Output{Stdout: stdout, ExitCode: exitCode})
		if exitCode != 0 && !res.Failed {
			t.Fatalf("ParseUpdate(ExitCode=%d) returned Failed=false; a non-zero apt-get exit must always be reported as failed (stdout=%q)", exitCode, stdout)
		}
	})
}

// FuzzParseSimulate fuzzes "apt-get -s install/full-upgrade" output parsing
// with every real captured golden fixture as seed material. The invariant
// is instConfRE's own capture group, `(Inst|Conf)`: every InstAction this
// function ever returns must have exactly one of those two Kind values —
// there is no third possibility the regex could produce, so any other
// value appearing would mean a code path invented an InstAction some other
// way.
func FuzzParseSimulate(f *testing.F) {
	addGoldenSeeds(f, "03_sim_install_jq_tree.txt", "05_sim_missing_pkg.txt", "06_sim_bad_version.txt", "07_sim_full_upgrade.txt", "11_sim_unmet_deps.txt")
	f.Add([]byte(""), 0)
	f.Add([]byte("Inst pkg (1.0 Debian:12/stable [amd64])\n"), 0)
	f.Add([]byte("Inst pkg [0.9] (1.0 Debian:12/stable [amd64])\n"), 0)
	f.Add([]byte("The following packages have unmet dependencies:\n pkg : Depends: libfoo but it is not installable\nE: Unable to correct problems\n"), 100)
	f.Add([]byte("E: Unable to locate package doesnotexist\n"), 100)
	f.Add([]byte("E: Version 'bad' for 'pkg' was not found\n"), 100)
	f.Add([]byte("Inst \n"), 0) // malformed: no name
	f.Add([]byte(strings.Repeat("Inst p (1 D:1/s [amd64])\n", 500)), 0)

	f.Fuzz(func(t *testing.T, stdout []byte, exitCode int) {
		res := ParseSimulate(Output{Stdout: stdout, ExitCode: exitCode})
		for _, a := range res.Actions {
			if a.Kind != "Inst" && a.Kind != "Conf" {
				t.Fatalf("ParseSimulate returned an InstAction with Kind %q, want Inst or Conf (stdout=%q)", a.Kind, stdout)
			}
		}
	})
}

// FuzzParsePrintURIs fuzzes "apt-get install --print-uris" output parsing.
// The invariant is printURIRE's own size capture group, `(\d+)`: every
// URIEntry returned must have a non-negative Size, since the regex can
// never match a negative number for that field — a caller (fetchAndBuild
// Selections, byFilename lookups) is entitled to assume apt's own reported
// size is never negative.
func FuzzParsePrintURIs(f *testing.F) {
	addGoldenSeeds(f, "04_printuris_jq_tree.txt")
	f.Add([]byte(""), 0)
	f.Add([]byte("'http://deb.debian.org/debian/pool/main/j/jq/jq_1.6-2.1_amd64.deb' jq_1.6-2.1_amd64.deb 50184 SHA256:abcdef0123456789\n"), 0)
	f.Add([]byte("'http://host/x with spaces.deb' 'x.deb' 0 SHA256:00\n"), 0)
	f.Add([]byte("not a print-uris line at all\n"), 0)
	f.Add([]byte("'' '' 999999999999999999999999999999 MD5Sum:00\n"), 0) // size overflows int64
	f.Add([]byte(strings.Repeat("'http://x/y.deb' y.deb 1 SHA256:aa\n", 500)), 0)

	f.Fuzz(func(t *testing.T, stdout []byte, exitCode int) {
		entries := ParsePrintURIs(Output{Stdout: stdout, ExitCode: exitCode})
		for _, e := range entries {
			if e.Size < 0 {
				t.Fatalf("ParsePrintURIs returned a negative Size %d for %+v (stdout=%q)", e.Size, e, stdout)
			}
		}
	})
}
