package repository

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
)

// This file is the regression suite for the three ways an attacker-supplied
// .deb, or an attacker-influenced Input field, used to reach the generated
// apt index unfiltered. Everything here is written from the attacker's side:
// each fixture is the smallest hostile .deb or Input that produced a
// confirmed bad Packages or Release file before the corresponding check
// existed.

// writeHostileDeb builds a .deb whose control stanza is exactly controlText
// and returns its pool-relative path. It is writeTestDeb under a name that
// says what the fixture is for.
func writeHostileDeb(t *testing.T, dir, poolRelPath, controlText string) string {
	t.Helper()
	return writeTestDeb(t, dir, poolRelPath, controlText)
}

// writeOne runs the writer over a single .deb and returns the Packages text.
func writeOne(t *testing.T, dir, rel string) (string, error) {
	t.Helper()
	_, err := NewWriter().Write(context.Background(), Input{
		Dir:      dir,
		Files:    []PoolFile{{Path: rel}},
		Release:  testRelease(),
		Compress: false,
	})
	if err != nil {
		return "", err
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "Packages"))
	if rerr != nil {
		t.Fatalf("read Packages: %v", rerr)
	}
	return string(got), nil
}

// fieldLines returns every "Name: value" line in text whose field name folds
// to lower, so a case-insensitive duplicate is visible as more than one hit.
func fieldLines(text, lower string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue // continuation line, not a field
		}
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		if strings.EqualFold(line[:i], lower) {
			out = append(out, line)
		}
	}
	return out
}

// TestWriteDropsAptOwnedControlFields is the regression test for the
// control-field passthrough: every spelling of a field apt reads as a fact
// about the pool file must come from the writer, never from the .deb.
//
// The SHA512 case is the one that needs no argument about which duplicate a
// parser keeps: debark never writes SHA512, so before this check apt saw
// exactly one - the attacker's - and refused to install the package with a
// hash mismatch on the far side of the air gap, after verify had passed.
func TestWriteDropsAptOwnedControlFields(t *testing.T) {
	dir := t.TempDir()
	control := "Package: evil\n" +
		"Version: 1.0\n" +
		"Architecture: amd64\n" +
		// Never written by debark: apt would see only this one.
		"SHA512: " + strings.Repeat("0", 128) + "\n" +
		// Case-variant spellings of fields debark does write.
		"filename: pool/e/evil/somewhere-else.deb\n" +
		"size: 1\n" +
		"sha256: " + strings.Repeat("1", 64) + "\n" +
		"md5sum: " + strings.Repeat("2", 32) + "\n" +
		"sha1: " + strings.Repeat("3", 40) + "\n" +
		// A legitimate field whose name merely contains a digest algorithm:
		// it describes the description, not the file, and must survive.
		"Description-md5: " + strings.Repeat("4", 32) + "\n" +
		"Description: hostile\n"
	rel := writeHostileDeb(t, dir, "pool/e/evil/evil_1.0_amd64.deb", control)

	text, err := writeOne(t, dir, rel)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := fieldLines(text, "sha512"); len(got) != 0 {
		t.Errorf("SHA512 survived from the .deb's control stanza: %q\nPackages:\n%s", got, text)
	}
	for _, name := range []string{"Filename", "Size", "MD5sum", "SHA1", "SHA256"} {
		got := fieldLines(text, name)
		if len(got) != 1 {
			t.Errorf("%s appears %d times (case-insensitively) %q, want exactly 1\nPackages:\n%s", name, len(got), got, text)
		}
	}

	// The one surviving copy of each is the writer's own value, not the
	// attacker's.
	fh, err := digest.AllFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("hash fixture: %v", err)
	}
	for _, want := range []string{
		"Filename: " + rel,
		"MD5sum: " + fh.MD5,
		"SHA1: " + fh.SHA1,
		"SHA256: " + fh.SHA256,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("Packages does not carry the writer's own %q\nPackages:\n%s", want, text)
		}
	}
	if strings.Contains(text, "somewhere-else.deb") {
		t.Errorf("the attacker's Filename value reached Packages\n%s", text)
	}

	// A field that only looks like a hash field is untouched.
	if !strings.Contains(text, "Description-md5: "+strings.Repeat("4", 32)+"\n") {
		t.Errorf("Description-md5 was wrongly dropped; it describes the description, not the file\n%s", text)
	}
}

// TestWriteRefusesIllegalControlFieldNames covers the names that must never
// be echoed into an index at all, including the zero-length name a control
// line beginning with ":" produces.
func TestWriteRefusesIllegalControlFieldNames(t *testing.T) {
	cases := []struct{ name, control string }{
		{"empty field name", "Package: bad\nVersion: 1\nArchitecture: amd64\n: empty name\n"},
		{"escape byte in name", "Package: bad\nVersion: 1\nArchitecture: amd64\nWeird\x1bKey: x\n"},
		{"nul byte in name", "Package: bad\nVersion: 1\nArchitecture: amd64\nWeird\x00Key: x\n"},
		{"leading hyphen", "Package: bad\nVersion: 1\nArchitecture: amd64\n-Dash: x\n"},
		{"underscore", "Package: bad\nVersion: 1\nArchitecture: amd64\nX_Weird: x\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			rel := writeHostileDeb(t, dir, "pool/b/bad/bad_1_amd64.deb", tc.control)
			_, err := writeOne(t, dir, rel)
			if err == nil {
				t.Fatalf("Write accepted a control stanza with an illegal field name")
			}
			if !dferr.Is(err, dferr.Verification) {
				t.Errorf("error class = %s, want verification (hostile input): %v", dferr.ClassOf(err), err)
			}
		})
	}
}

// TestWriteDropsControlComments records where the "#" case is handled: a
// deb822 comment line never becomes a field, so the control parser drops it
// before validFieldName ever sees it. The property that matters downstream is
// the same either way - it must not appear in Packages.
func TestWriteDropsControlComments(t *testing.T) {
	dir := t.TempDir()
	control := "Package: good\nVersion: 1\nArchitecture: amd64\n#Comment: smuggled\nDescription: ok\n"
	rel := writeHostileDeb(t, dir, "pool/g/good/good_1_amd64.deb", control)
	text, err := writeOne(t, dir, rel)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if strings.Contains(text, "smuggled") || strings.Contains(text, "#Comment") {
		t.Fatalf("a control comment line reached Packages:\n%s", text)
	}
}

// TestWriteRefusesControlBytesInFieldValues keeps NUL and ESC out of the
// index: a NUL truncates the field for any C consumer reading it with str*
// functions, and an ESC turns an operator's terminal into the attacker's.
func TestWriteRefusesControlBytesInFieldValues(t *testing.T) {
	cases := []struct{ name, control string }{
		{"nul", "Package: bad\nVersion: 1\nArchitecture: amd64\nMaintainer: a\x00b\n"},
		{"escape", "Package: bad\nVersion: 1\nArchitecture: amd64\nMaintainer: a\x1b[2Jb\n"},
		{"carriage return", "Package: bad\nVersion: 1\nArchitecture: amd64\nMaintainer: a\rb\n"},
		{"bell", "Package: bad\nVersion: 1\nArchitecture: amd64\nMaintainer: a\x07b\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			rel := writeHostileDeb(t, dir, "pool/b/bad/bad_1_amd64.deb", tc.control)
			_, err := writeOne(t, dir, rel)
			if err == nil {
				t.Fatalf("Write accepted a control value containing a control byte")
			}
			if !dferr.Is(err, dferr.Verification) {
				t.Errorf("error class = %s, want verification: %v", dferr.ClassOf(err), err)
			}
		})
	}
}

// TestWriteRefusesCaseCollidingControlFields: two field names that differ
// only in case are one field to apt, so a stanza carrying both has no
// unambiguous meaning. Refusing is the only answer that neither discards data
// nor guesses.
func TestWriteRefusesCaseCollidingControlFields(t *testing.T) {
	dir := t.TempDir()
	control := "Package: bad\nVersion: 1\nArchitecture: amd64\nfilename: a.deb\nFILENAME: ../../etc/evil.deb\n"
	rel := writeHostileDeb(t, dir, "pool/b/bad/bad_1_amd64.deb", control)
	_, err := writeOne(t, dir, rel)
	if err == nil {
		t.Fatalf("Write accepted a control stanza declaring one field twice under different cases")
	}
	if !dferr.Is(err, dferr.Verification) {
		t.Errorf("error class = %s, want verification: %v", dferr.ClassOf(err), err)
	}
}

// TestWriteRefusesUnsafePoolPath is the regression test for PoolFile.Path
// reaching Filename unvalidated.
//
// Filename is the URI apt resolves against the repository root, and install
// mounts that repository with "Trusted: yes"; a ".." there names a file
// outside the manifest-covered tree. The writer also used to open a different
// string from the one it wrote - filepath.Join cleans, the Filename field did
// not - so an uncleaned path indexed one file under the name of another.
//
// Each case plants a real .deb at the location the hostile path resolves to,
// so a refusal cannot be mistaken for "the file was not there anyway".
func TestWriteRefusesUnsafePoolPath(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"parent traversal", "../outside_1_amd64.deb"},
		{"deep traversal", "../../outside_1_amd64.deb"},
		{"embedded parent component", "pool/x/../g/good/good_1.0_amd64.deb"},
		{"dot component", "pool/./g/good/good_1.0_amd64.deb"},
		{"double slash", "pool//g/good/good_1.0_amd64.deb"},
		{"leading dot slash", "./pool/g/good/good_1.0_amd64.deb"},
		{"absolute posix", "/etc/cron.d/evil.deb"},
		{"windows drive", `C:/Windows/evil.deb`},
		{"backslash separator", `pool\g\good\good_1.0_amd64.deb`},
		{"backslash traversal", `..\outside_1_amd64.deb`},
		{"unc path", `\\server\share\evil.deb`},
		{"trailing slash", "pool/g/good/"},
		{"dot", "."},
		{"dotdot", ".."},
		{"newline", "pool/g/good/good\n_1.0_amd64.deb"},
		{"nul byte", "pool/g/good/good_1.0_amd64.deb\x00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// A real, valid .deb one level above the repository root, so a
			// traversal that "worked" would find something. It lives inside
			// this test's own t.TempDir(), so an escape is contained.
			outside := filepath.Join(filepath.Dir(dir), "outside_1_amd64.deb")
			if err := os.WriteFile(outside, buildDebBytes(t, simpleControl("outside", "1", "amd64")), 0o644); err != nil {
				t.Fatalf("plant outside fixture: %v", err)
			}
			// And a real one inside, for the uncleaned-but-resolvable cases.
			writeHostileDeb(t, dir, "pool/g/good/good_1.0_amd64.deb", simpleControl("good", "1.0", "amd64"))

			_, err := NewWriter().Write(context.Background(), Input{
				Dir:      dir,
				Files:    []PoolFile{{Path: tc.path}},
				Release:  testRelease(),
				Compress: false,
			})
			if err == nil {
				got, rerr := os.ReadFile(filepath.Join(dir, "Packages"))
				if rerr != nil {
					t.Fatalf("Write accepted %q and Packages is unreadable: %v", tc.path, rerr)
				}
				t.Fatalf("Write accepted the unsafe pool path %q; Packages:\n%s", tc.path, got)
			}
			if !dferr.Is(err, dferr.Usage) {
				t.Errorf("error class = %s, want usage (a malformed caller argument): %v", dferr.ClassOf(err), err)
			}
		})
	}
}

// TestWriteFilenameIsExactlyThePathOpened pins the invariant the uncleaned
// cases above violated: the string written into Filename is byte-for-byte the
// string the writer resolved and opened.
func TestWriteFilenameIsExactlyThePathOpened(t *testing.T) {
	dir := t.TempDir()
	rel := writeHostileDeb(t, dir, "pool/g/good/good_1.0_amd64.deb", simpleControl("good", "1.0", "amd64"))
	text, err := writeOne(t, dir, rel)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	lines := fieldLines(text, "filename")
	if len(lines) != 1 || lines[0] != "Filename: "+rel {
		t.Fatalf("Filename lines = %q, want exactly [%q]", lines, "Filename: "+rel)
	}
	// And the named file really is the one under Dir.
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("Filename %q does not resolve to a file under Dir: %v", rel, err)
	}
}

// TestWriteRefusesReleaseFieldInjection is the regression test for Release
// field injection: a Codename of "noble\nSHA256:\n <digest> <size> Packages"
// produced a Release carrying a second, attacker-chosen SHA256 block ahead of
// the writer's real one.
func TestWriteRefusesReleaseFieldInjection(t *testing.T) {
	forged := "SHA256:\n " + strings.Repeat("0", 64) + " 1 Packages"

	cases := []struct {
		name   string
		mutate func(f *ReleaseFields)
	}{
		{"newline in Codename", func(f *ReleaseFields) { f.Codename = "noble\n" + forged }},
		{"carriage return in Codename", func(f *ReleaseFields) { f.Codename = "noble\r" + forged }},
		{"newline in Origin", func(f *ReleaseFields) { f.Origin = "debark\nSuite: other" }},
		{"newline in Description", func(f *ReleaseFields) { f.Description = "x\n" + forged }},
		{"newline in Date", func(f *ReleaseFields) { f.Date = "Thu, 03 Sep 2026 00:00:00 UTC\n" + forged }},
		{"newline in an Architectures element", func(f *ReleaseFields) { f.Architectures = []string{"amd64\nSuite: other"} }},
		{"newline in a Components element", func(f *ReleaseFields) { f.Components = []string{"main\nSuite: other"} }},
		{"newline in an ExtraFields value", func(f *ReleaseFields) {
			f.ExtraFields = map[string]string{"Extra": "x\n" + forged}
		}},
		{"newline in an ExtraFields key", func(f *ReleaseFields) {
			f.ExtraFields = map[string]string{"Extra\nSuite": "x"}
		}},
		{"colon in an ExtraFields key", func(f *ReleaseFields) {
			f.ExtraFields = map[string]string{"Extra: forged\nSuite": "x"}
		}},
		{"leading space in a value", func(f *ReleaseFields) {
			f.ExtraFields = map[string]string{"Extra": " continuation"}
		}},
		{"escape byte in a value", func(f *ReleaseFields) { f.Label = "debark\x1b[2J bundle" }},
		{"nul byte in a value", func(f *ReleaseFields) { f.Label = "debark\x00 bundle" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			rel := writeHostileDeb(t, dir, "pool/g/good/good_1.0_amd64.deb", simpleControl("good", "1.0", "amd64"))
			fields := testRelease()
			tc.mutate(&fields)

			_, err := NewWriter().Write(context.Background(), Input{
				Dir:      dir,
				Files:    []PoolFile{{Path: rel}},
				Release:  fields,
				Compress: false,
			})
			if err == nil {
				got, _ := os.ReadFile(filepath.Join(dir, "Release"))
				t.Fatalf("Write accepted an injected Release field; Release:\n%s", got)
			}
			if !dferr.Is(err, dferr.Usage) {
				t.Errorf("error class = %s, want usage (a malformed caller argument): %v", dferr.ClassOf(err), err)
			}
			// Nothing forged reached the file, if one was written at all.
			if got, rerr := os.ReadFile(filepath.Join(dir, "Release")); rerr == nil {
				if strings.Count(string(got), "\nSHA256:\n") > 1 {
					t.Fatalf("Release carries more than one SHA256 block:\n%s", got)
				}
			}
		})
	}
}

// TestWriteAcceptsRealisticControlFields is the over-blocking guard: the new
// checks must not reject anything a real Debian or vendor .deb carries.
func TestWriteAcceptsRealisticControlFields(t *testing.T) {
	dir := t.TempDir()
	control := "Package: libjq1\n" +
		"Source: jq\n" +
		"Version: 1.6-2.1+deb12u2\n" +
		"Architecture: amd64\n" +
		"Maintainer: Debian Test <test@example.com>\n" +
		"Original-Maintainer: Upstream Test <up@example.com>\n" +
		"Installed-Size: 111\n" +
		"Depends: libc6 (>= 2.34)\n" +
		"Pre-Depends: libc6\n" +
		"Built-Using: gcc-12 (= 12.2.0-3)\n" +
		"Section: libs\n" +
		"Priority: optional\n" +
		"Multi-Arch: same\n" +
		"Homepage: https://example.invalid/jq\n" +
		"Description-md5: " + strings.Repeat("a", 32) + "\n" +
		"Python-Version: 3.11\n" +
		"Gstreamer-Version: 1.0\n" +
		"X-Original-Vendor: Example\n" +
		"Tag: role::shared-lib\n" +
		"Description: JSON processor library\n" +
		" Contains a tab\there and a UTF-8 name: Björn Müller.\n" +
		" .\n" +
		" Second paragraph.\n"
	rel := writeHostileDeb(t, dir, PoolPath("libjq1", "libjq1_1.6-2.1+deb12u2_amd64.deb"), control)

	text, err := writeOne(t, dir, rel)
	if err != nil {
		t.Fatalf("Write rejected a realistic control stanza: %v", err)
	}
	for _, want := range []string{
		"Package: libjq1\n", "Source: jq\n", "Original-Maintainer: ", "Built-Using: ",
		"Description-md5: ", "Python-Version: 3.11\n", "X-Original-Vendor: Example\n",
		"Tag: role::shared-lib\n", "Björn Müller",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("realistic field %q was dropped\nPackages:\n%s", want, text)
		}
	}
	if !strings.HasPrefix(text, "Package: libjq1\n") {
		t.Errorf("Package is no longer the first field:\n%s", text)
	}
}
