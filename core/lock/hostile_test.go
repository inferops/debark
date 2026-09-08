package lock

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

const (
	digestA = "1111111111111111111111111111111111111111111111111111111111111111"
	digestB = "2222222222222222222222222222222222222222222222222222222222222222"
)

// goodLock is the smallest lock that should validate, so every test below can
// change exactly one thing and attribute the result to it.
func goodLock() *Lock {
	return &Lock{
		SchemaVersion: SchemaVersion,
		Target:        Target{DistroID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "amd64"},
		Resolver:      Resolver{Backend: BackendLocal, APTVersion: "2.7.14", DpkgVersion: "1.22.6"},
		ClosedWorld:   ClosedWorld{Result: ClosedWorldOK},
		Packages: []Package{{
			Name: "vlc", Arch: "amd64", Version: "1:3.0.21-1build1",
			Filename: "pool/v/vlc/vlc_3.0.21-1build1_amd64.deb",
			Size:     1024, SHA256: digestA,
			Reason:                ReasonRequested,
			PublisherVerification: VerifiedAPTSigned,
		}},
		Install: []string{"vlc:amd64=1:3.0.21-1build1"},
	}
}

func TestValidate_AcceptsAWellFormedLock(t *testing.T) {
	if err := Validate(goodLock()); err != nil {
		t.Fatalf("a well-formed lock was refused: %v", err)
	}
}

// TestValidate_FilenameIsNeverAPathOutsideTheBundle is the one with the
// sharpest edge. Filename is the only Package field a reader turns into a
// filesystem path — core/doctor joins it to the bundle directory and opens
// the result — and nothing checked it at all. filepath.Join cleans a path but
// cannot undo a "..", so a lock naming "../../../../etc/shadow" reads a file
// outside the bundle entirely.
func TestValidate_FilenameIsNeverAPathOutsideTheBundle(t *testing.T) {
	hostile := map[string]string{
		"parent traversal":   "../../../../etc/shadow",
		"embedded traversal": "pool/v/vlc/../../../../etc/shadow",
		"absolute posix":     "/etc/cron.d/pwn",
		"windows absolute":   `C:\Windows\System32\evil.deb`,
		"backslash sep":      `pool\v\vlc\evil.deb`,
		"drive relative":     "C:evil.deb",
		"nul byte":           "pool/v/vlc/vlc\x00.deb",
		"newline":            "pool/v/vlc/vlc\n.deb",
		"empty segment":      "pool//vlc/vlc.deb",
		"dot segment":        "pool/./vlc/vlc.deb",
		"bare dotdot":        "..",
		"empty":              "",
	}
	for name, fn := range hostile {
		t.Run(name, func(t *testing.T) {
			l := goodLock()
			l.Packages[0].Filename = fn
			if err := Validate(l); err == nil {
				t.Errorf("Validate accepted filename %q, which resolves outside the bundle", fn)
			}

			// Load is the half that matters for the unverified path:
			// core/bundle.Open reads the lock BEFORE the signed manifest is
			// checked, so `debark doctor BUNDLE` and `debark inspect
			// BUNDLE` parse an attacker's document with nothing behind it.
			dir := t.TempDir()
			raw, err := json.Marshal(l)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, FileName), raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir); err == nil {
				t.Errorf("Load accepted filename %q from an unverified bundle", fn)
			}
		})
	}

	// The shape debark itself writes (repository.PoolPath's output) must
	// keep working, or the rule is useless in practice.
	for _, ok := range []string{
		"pool/v/vlc/vlc_3.0.21-1build1_amd64.deb",
		"pool/libf/libfoo/libfoo_1.0_all.deb",
		"vlc.deb",
	} {
		l := goodLock()
		l.Packages[0].Filename = ok
		if err := Validate(l); err != nil {
			t.Errorf("Validate refused the legitimate pool path %q: %v", ok, err)
		}
	}
}

// TestValidate_OnePackagePerNameAndArch: the uniqueness key used to include
// the version, so a lock could name two versions of one package and still
// validate — and then mean different things to different readers, all of them
// silent. Find returns whichever comes first, InstallSet drops the entry
// naming the other, core/install's selectInstallSet refuses the whole lock,
// and `install --all` asks apt for two conflicting pins.
func TestValidate_OnePackagePerNameAndArch(t *testing.T) {
	l := goodLock()
	second := l.Packages[0]
	second.Version = "1:3.0.22-1"
	second.Filename = "pool/v/vlc/vlc_3.0.22-1_amd64.deb"
	second.SHA256 = digestB
	l.Packages = append(l.Packages, second)
	l.Install = []string{"vlc:amd64=1:3.0.22-1"}

	if err := Validate(l); err == nil {
		// Demonstrate the divergence the acceptance used to allow.
		got := l.InstallSet()
		found, _ := l.Find("vlc", "amd64")
		t.Fatalf("Validate accepted two versions of vlc:amd64; the lock names 1 package, "+
			"InstallSet returns %d, and Find resolves to version %q", len(got), found.Version)
	}

	// The same name at a DIFFERENT architecture is legitimate (Multi-Arch).
	l = goodLock()
	other := l.Packages[0]
	other.Arch = "arm64"
	other.Filename = "pool/v/vlc/vlc_3.0.21-1build1_arm64.deb"
	other.SHA256 = digestB
	l.Packages = append(l.Packages, other)
	l.Install = append(l.Install, "vlc:arm64=1:3.0.21-1build1")
	if err := Validate(l); err != nil {
		t.Errorf("Validate refused the same package at two architectures, which is legal: %v", err)
	}
}

// TestValidate_OneFileClaimedOnce is the collision core/bundle's
// materialiseSelections calls "silent corruption, not a harmless collision",
// noting there that lock.Validate could not see it because its uniqueness key
// was name/arch/version.
func TestValidate_OneFileClaimedOnce(t *testing.T) {
	l := goodLock()
	dup := l.Packages[0]
	dup.Name = "vlc-plugin-base"
	dup.SHA256 = digestB
	l.Packages = append(l.Packages, dup)
	l.Install = append(l.Install, "vlc-plugin-base:amd64=1:3.0.21-1build1")
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted two packages claiming one pool file")
	}
}

// TestValidate_InstallNamesEachPackageOnce: Install is handed to apt as an
// argument list, so a repeat — or two entries pinning one package to
// different versions — is a request apt cannot satisfy as written.
func TestValidate_InstallNamesEachPackageOnce(t *testing.T) {
	l := goodLock()
	l.Install = []string{"vlc:amd64=1:3.0.21-1build1", "vlc:amd64=1:3.0.21-1build1"}
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted a repeated install entry")
	}
}

// TestValidate_NegativeSize: core/install sums these to decide whether the
// target has room, so one negative entry makes an install that will not fit
// look as though it will. The schema says minimum 0.
func TestValidate_NegativeSize(t *testing.T) {
	l := goodLock()
	l.Packages[0].Size = -1 << 40
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted a negative package size")
	}
	l.Packages[0].Size = 0
	if err := Validate(l); err != nil {
		t.Errorf("Validate refused a zero size, which the schema permits: %v", err)
	}
}

// TestValidate_RequiresTargetArch: core/install guards its whole architecture
// check with `if lk.Target.Arch != ""`, so a lock that simply omits the field
// installs on ANY architecture with no exit-7 refusal at all. An absent value
// must not switch off the check that exists to catch it.
func TestValidate_RequiresTargetArch(t *testing.T) {
	l := goodLock()
	l.Target.Arch = ""
	if err := Validate(l); err == nil {
		t.Fatal("Validate accepted a lock with no target.arch; install would skip its architecture check entirely")
	}
	if int(dferr.TargetMismatch) != 7 {
		t.Fatalf("dferr.TargetMismatch = %d, want 7 (ADR-012, frozen)", int(dferr.TargetMismatch))
	}
}

// TestValidate_EnumFields: publisher_verification in particular is not
// cosmetic — core/policy's require-signed-publisher rule denies exactly
// VerifiedURLUnverified, so any other spelling is a file that reads as having
// a provenance claim it does not have.
func TestValidate_EnumFields(t *testing.T) {
	for _, pv := range []PublisherVerification{"", "unverified", "totally-fine", "APT-SIGNED"} {
		l := goodLock()
		l.Packages[0].PublisherVerification = pv
		if err := Validate(l); err == nil {
			t.Errorf("Validate accepted publisher_verification %q", pv)
		}
	}
	for _, pv := range []PublisherVerification{VerifiedAPTSigned, VerifiedURLUnverified, VerifiedUserDigest, VerifiedUserSignature} {
		l := goodLock()
		l.Packages[0].PublisherVerification = pv
		if err := Validate(l); err != nil {
			t.Errorf("Validate refused the real value %q: %v", pv, err)
		}
	}

	for _, r := range []string{"", "nonsense", "dependency-of:", "Requested"} {
		l := goodLock()
		l.Packages[0].Reason = r
		if err := Validate(l); err == nil {
			t.Errorf("Validate accepted reason %q", r)
		}
	}
	for _, r := range []string{ReasonRequested, ReasonUpgrade, ReasonExternal, ReasonDependencyOfPrefix + "vlc"} {
		l := goodLock()
		l.Packages[0].Reason = r
		if err := Validate(l); err != nil {
			t.Errorf("Validate refused the real reason %q: %v", r, err)
		}
	}

	for _, cw := range []string{"", "wat", "OK"} {
		l := goodLock()
		l.ClosedWorld.Result = cw
		if err := Validate(l); err == nil {
			t.Errorf("Validate accepted closed_world_check.result %q", cw)
		}
	}
}

// TestValidate_IdentityFieldsKeepInstallEntriesUnambiguous. An install entry
// is "name:arch=version"; a colon in the arch or an "=" in the version makes
// the same string splittable more than one way, so two readers can disagree
// about which package one entry names.
func TestValidate_IdentityFieldsKeepInstallEntriesUnambiguous(t *testing.T) {
	l := goodLock()
	l.Packages[0].Arch = "amd64:x"
	if err := Validate(l); err == nil {
		t.Error("Validate accepted an arch containing a colon")
	}
	l = goodLock()
	l.Packages[0].Version = "1.0=2.0"
	if err := Validate(l); err == nil {
		t.Error("Validate accepted a version containing '='")
	}
	// An epoch is a legitimate colon, to the RIGHT of the "=" separator.
	l = goodLock()
	if err := Validate(l); err != nil {
		t.Errorf("Validate refused an epoch version: %v", err)
	}
	for _, n := range []string{"VLC", "vlc/../etc", "-leading-dash", "vlc:amd64", "vlc name"} {
		l = goodLock()
		l.Packages[0].Name = n
		l.Install = nil
		if err := Validate(l); err == nil {
			t.Errorf("Validate accepted the illegal package name %q", n)
		}
	}
}

// TestLoad_RefusesAnUnknownField mirrors the concern core/verify already
// documents when it re-derives lock.json's digest from raw bytes rather than
// from a re-marshalled struct: "it would let an unknown field appended to
// lock.json survive an unmarshal/remarshal round trip undetected".
func TestLoad_RefusesAnUnknownField(t *testing.T) {
	dir := t.TempDir()
	raw, err := json.Marshal(goodLock())
	if err != nil {
		t.Fatal(err)
	}
	smuggled := strings.Replace(string(raw), `{"schema_version"`, `{"payload":"anything at all","schema_version"`, 1)
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(smuggled), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load accepted a lock.json carrying a field debark.lock/v1 does not declare")
	}
}

// TestLoad_BoundsTheFileItReads. lock.json arrives on removable media and is
// read before anything has verified it, and os.ReadFile has no ceiling.
func TestLoad_BoundsTheFileItReads(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	// Truncate rather than write: the size is what Load checks, and a sparse
	// file gets there without spending 64 MiB of disk or time.
	if err := f.Truncate(maxLockBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Load(dir)
	if err == nil {
		t.Fatal("Load read a lock.json larger than the maximum")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// TestSaveLoadRoundTrip: what Save writes must be exactly what Load reads
// back, or the lock means one thing to the builder and another to the target.
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := goodLock()
	if _, err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load could not read back what Save wrote: %v", err)
	}
	if err := Validate(got); err != nil {
		t.Fatalf("Validate refused a lock this package just wrote: %v", err)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Errorf("round trip changed the document:\n saved %s\n read  %s", a, b)
	}
}

// TestLoadErrorsAreUsageClass: every refusal above must carry a class, or
// dferr.ClassOf falls through to Usage by default and the exit code is right
// only by accident.
func TestLoadErrorsAreUsageClass(t *testing.T) {
	dir := t.TempDir()
	l := goodLock()
	l.Packages[0].Filename = "../../etc/shadow"
	raw, _ := json.Marshal(l)
	if err := os.WriteFile(filepath.Join(dir, FileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	var target *dferr.Error
	if !errors.As(err, &target) {
		t.Fatalf("Load's refusal carries no dferr class: %v", err)
	}
	if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("class %s, want %s", got, dferr.Usage)
	}
}

// writeLockJSON puts body at dir/lock.json and returns dir.
func writeLockJSON(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// goodLockJSON is goodLock as bytes, for tests that mutate the document's
// TEXT rather than the struct — which is the only way to express the defects
// below, because none of them survives a Go round trip.
func goodLockJSON(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(goodLock())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestLoad_RefusesAFieldNameThatDiffersOnlyInCase. json.Decoder's
// DisallowUnknownFields, which Load sets, only complains about keys that
// match NO field: encoding/json matches field names case-insensitively, so
// "FILENAME" decoded straight into Package.Filename and nothing objected.
//
// lock.v1.schema.json sets "additionalProperties": false, so an auditor
// validating the same bytes against the published schema rejects a document
// debark accepted — two readers of one file disagreeing about whether it is
// a lock at all, on the unverified read path (`debark inspect`, `debark
// doctor`, core/bundle.Open) where nothing else is watching.
func TestLoad_RefusesAFieldNameThatDiffersOnlyInCase(t *testing.T) {
	base := goodLockJSON(t)
	cases := map[string]string{
		"top-level SCHEMA_VERSION": strings.Replace(base, `"schema_version"`, `"Schema_Version"`, 1),
		"top-level PACKAGES":       strings.Replace(base, `"packages"`, `"PACKAGES"`, 1),
		"nested FILENAME":          strings.Replace(base, `"filename"`, `"FILENAME"`, 1),
		"nested SHA256":            strings.Replace(base, `"sha256"`, `"SHA256"`, 1),
		"deeply nested origin URI": strings.Replace(base, `"origin":{}`, `"origin":{"URI":"https://x"}`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if body == base {
				t.Fatal("test case did not actually change the document")
			}
			if _, err := Load(writeLockJSON(t, body)); err == nil {
				t.Error("Load accepted a key debark.lock/v1 does not declare, matched only by encoding/json's case-insensitive fallback")
			} else if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("class %s, want %s", got, dferr.Usage)
			}
		})
	}
}

// TestLoad_RefusesADuplicateKey. encoding/json keeps the LAST occurrence of a
// repeated key and says nothing, so one lock.json meant two different things
// depending on who read it — and this project's own canonicaliser is one of
// the readers that disagrees: canonical.Transform, which core/verify runs
// over these exact bytes to derive lock.json's digest, fails outright on a
// duplicate key. Load accepted documents the rest of debark cannot read.
func TestLoad_RefusesADuplicateKey(t *testing.T) {
	base := goodLockJSON(t)
	cases := map[string]string{
		"duplicate install (rules disarmed by the second)": base[:len(base)-1] + `,"install":["evil:amd64=1"]}`,
		"duplicate packages":       base[:len(base)-1] + `,"packages":[]}`,
		"duplicate schema_version": base[:len(base)-1] + `,"schema_version":"debark.lock/v1"}`,
		"duplicate nested filename": strings.Replace(base,
			`"filename":"pool/v/vlc/vlc_3.0.21-1build1_amd64.deb"`,
			`"filename":"pool/v/vlc/vlc_3.0.21-1build1_amd64.deb","filename":"pool/evil.deb"`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if body == base {
				t.Fatal("test case did not actually change the document")
			}
			if _, err := Load(writeLockJSON(t, body)); err == nil {
				t.Error("Load accepted a lock.json with a repeated key; encoding/json silently kept the last one")
			} else if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("class %s, want %s", got, dferr.Usage)
			}
		})
	}
}

// TestLoad_RefusesTrailingContent. json.Decoder.Decode stops at the end of
// the first value, so anything appended to lock.json was silently ignored
// here — and refused by canonical.Transform when core/verify hashed the same
// bytes. Same file, two answers.
func TestLoad_RefusesTrailingContent(t *testing.T) {
	base := goodLockJSON(t)
	for name, suffix := range map[string]string{
		"a second document": ` {"schema_version":"debark.lock/v1"}`,
		"loose text":        "\nnot json at all\n",
		"a stray bracket":   "]",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeLockJSON(t, base+suffix)); err == nil {
				t.Error("Load ignored content appended after the lock document")
			}
		})
	}
}

// TestLoad_StillAcceptsAnHonestLock guards the three tests above against
// over-strictness: the exact bytes Save writes must keep loading, or the
// refusals cost more than they buy.
func TestLoad_StillAcceptsAnHonestLock(t *testing.T) {
	dir := t.TempDir()
	if _, err := Save(dir, goodLock()); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load refused the bytes Save just wrote: %v", err)
	}

	// And an omitted optional field, or an explicit null in place of one,
	// must still be fine: both are how a real lock spells "nothing here".
	minimal := `{"schema_version":"` + SchemaVersion + `","packages":null,"install":null,"warnings":null}`
	if _, err := Load(writeLockJSON(t, minimal)); err != nil {
		t.Errorf("Load refused a lock whose optional arrays are null: %v", err)
	}
}

// TestSave_RefusesToWriteALockLoadWillNotRead. Save wrote whatever it was
// given: a Filename of "../../../../etc/shadow" went to disk, and Load then
// refused the bundle. A build that appears to succeed and leaves a bundle no
// debark can open is the worst shape for this failure, because the
// diagnostic arrives on the far side of the air gap from the person who could
// fix it.
func TestSave_RefusesToWriteALockLoadWillNotRead(t *testing.T) {
	for _, filename := range []string{
		"../../../../etc/shadow",
		"/etc/shadow",
		"pool/../../escape.deb",
		`C:\Windows\System32\evil.deb`,
		"",
	} {
		t.Run(filename, func(t *testing.T) {
			dir := t.TempDir()
			l := goodLock()
			l.Packages[0].Filename = filename
			if _, err := Save(dir, l); err == nil {
				t.Errorf("Save wrote a lock naming %q; Load will refuse it", filename)
			} else if got := dferr.ClassOf(err); got != dferr.Usage {
				t.Errorf("class %s, want %s", got, dferr.Usage)
			}
			if _, statErr := os.Stat(filepath.Join(dir, FileName)); statErr == nil {
				t.Error("Save left a lock.json behind after refusing")
			}
		})
	}
}

// TestSave_RefusesASchemaVersionLoadWillNotRead is the second half of the
// same asymmetry: Save wrote whatever schema_version the caller set, and Load
// accepts exactly one. A bundle written with any other value was a bundle no
// debark could open, discovered after the medium had crossed the gap.
func TestSave_RefusesASchemaVersionLoadWillNotRead(t *testing.T) {
	for _, sv := range []string{"debark.lock/v2", "debark.lock/v0", "", "DEBARK.LOCK/V1"} {
		t.Run(sv, func(t *testing.T) {
			dir := t.TempDir()
			l := goodLock()
			l.SchemaVersion = sv
			if _, err := Save(dir, l); err == nil {
				t.Errorf("Save wrote schema_version %q; Load refuses anything but %q", sv, SchemaVersion)
			}
			if _, statErr := os.Stat(filepath.Join(dir, FileName)); statErr == nil {
				t.Error("Save left a lock.json behind after refusing")
			}
		})
	}
}

// TestSave_WriteFailureIsEnvironmentClass. ADR-012 puts "no disk space, no
// permission" in the environment class (exit 2); Usage (exit 1) is "the
// operator passed something malformed", which a write failure is not — the
// document was already accepted by everything above it.
func TestSave_WriteFailureIsEnvironmentClass(t *testing.T) {
	// A directory that does not exist is the portable stand-in for an
	// unwritable one: os.WriteFile fails the same way on Linux and Windows,
	// and for the same class of reason.
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	_, err := Save(missing, goodLock())
	if err == nil {
		t.Fatal("Save succeeded into a directory that does not exist")
	}
	if got := dferr.ClassOf(err); got != dferr.Environment {
		t.Errorf("class %s, want %s (ADR-012: no disk, no permission)", got, dferr.Environment)
	}
}

// TestLoad_RefusesTwoEntriesNamingOneFile is the amplification defect, pinned
// where it is fixed. bundle.Open calls Load and never Validate, so `debark
// doctor BUNDLE` — the command an operator runs FIRST on media that just
// arrived, before anything is verified — used to do its work once per lock
// ENTRY instead of once per file on the medium. One .deb named by every entry
// is parsed, decompressed and script-scanned once per entry, and every pass
// appends findings held until the report is returned.
//
// Measured against core/doctor with a single 948-byte fixture .deb named N
// times: 16 ms at N=10, 74 ms at N=50, 125 ms at N=100, 256 ms at N=200 —
// flat at ~1.3 ms per entry, with the constant set by the size of the .deb,
// which the same attacker chooses. maxLockBytes is 64 MiB and a minimal entry
// is around 145 bytes, so the ceiling is an out-of-memory kill rather than a
// slow command.
//
// The test itself is a unit test of the rule, not of the timing: what makes
// the work finite is that one pool file cannot be named twice, and that is
// what is asserted.
func TestLoad_RefusesTwoEntriesNamingOneFile(t *testing.T) {
	l := goodLock()
	second := l.Packages[0]
	second.Name = "vlc-plugin-base" // a different package...
	second.SHA256 = digestB
	l.Packages = append(l.Packages, second) // ...at the same pool path
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writeLockJSON(t, string(raw))); err == nil {
		t.Fatal("Load accepted a lock naming one pool file twice; a reader's work is then bounded by the lock's entry count, not by the medium")
	} else if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("class %s, want %s", got, dferr.Usage)
	}

	// The amplifying shape itself: many entries, one file.
	many := goodLock()
	many.Packages = nil
	many.Install = nil
	for i := 0; i < 64; i++ {
		p := goodLock().Packages[0]
		p.Name = "pkg" + strconv.Itoa(i)
		many.Packages = append(many.Packages, p) // every one at the same Filename
	}
	raw, err = json.Marshal(many)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writeLockJSON(t, string(raw))); err == nil {
		t.Fatal("Load accepted 64 entries naming one pool file")
	}
}

// TestLoad_StillOpensALockWithTwoVersionsOfOnePackage records the OTHER half
// of the placement question, deliberately answered the other way.
//
// A duplicate (name, arch) is the same kind of ambiguity as a duplicate
// filename — Find returns whichever entry comes first and InstallSet drops
// the one naming the other — but it is not load-bearing in the same way, so
// it stays in Validate and Load keeps opening such a bundle. Three reasons,
// all recorded in checkLoadSafety's doc comment: it cannot amplify anything
// (two entries at distinct filenames need two real files on the medium); no
// Load-only consumer reaches Find or InstallSet today; and moving it made
// Save refuse a lock core/bundle's own test builds on purpose, which is a
// change to a package this review does not own.
//
// This test exists so that the asymmetry is a decision with a stated reason
// rather than an oversight someone silently "fixes" later.
func TestLoad_StillOpensALockWithTwoVersionsOfOnePackage(t *testing.T) {
	l := goodLock()
	second := l.Packages[0]
	second.Version = "1:3.0.22-1"
	second.Filename = "pool/v/vlc/vlc_3.0.22-1_amd64.deb" // a DISTINCT file
	second.SHA256 = digestB
	l.Packages = append(l.Packages, second)
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writeLockJSON(t, string(raw))); err != nil {
		t.Errorf("Load refused a lock with two versions of one package: %v. "+
			"That rule belongs to Validate; if it has deliberately moved to the load path, "+
			"checkLoadSafety's doc comment and core/bundle's assemble test both need updating with it.", err)
	}
	// Validate is still where it is refused.
	if err := Validate(l); err == nil {
		t.Error("Validate accepted two versions of one package")
	}
}

// TestValidateAndLoadAgreeAboutFilenames: for the rules checkLoadSafety owns,
// the two entry points must not be able to drift into disagreeing. Validate
// calls checkLoadSafety rather than repeating it, and this pins that.
func TestValidateAndLoadAgreeAboutFilenames(t *testing.T) {
	mutations := map[string]func(*Lock){
		"one file claimed twice": func(l *Lock) {
			second := l.Packages[0]
			second.Name = "vlc-plugin-base"
			second.SHA256 = digestB
			l.Packages = append(l.Packages, second)
		},
		"filename escapes the bundle": func(l *Lock) {
			l.Packages[0].Filename = "../../etc/shadow"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			l := goodLock()
			mutate(l)
			validateErr := Validate(l)
			raw, err := json.Marshal(l)
			if err != nil {
				t.Fatal(err)
			}
			_, loadErr := Load(writeLockJSON(t, string(raw)))
			if (validateErr == nil) != (loadErr == nil) {
				t.Errorf("Validate and Load disagree: Validate=%v, Load=%v", validateErr, loadErr)
			}
			if validateErr == nil {
				t.Error("neither entry point refused it")
			}
		})
	}
}
