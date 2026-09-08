package snapshot

import (
	"strconv"
	"strings"
	"testing"
)

func validMinimalSnapshot() *Snapshot {
	return &Snapshot{
		SchemaVersion: SchemaVersion,
		CreatedAt:     "2026-09-03T00:00:00Z",
		Origin:        Origin{Kind: OriginCaptured},
		Target:        Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		DpkgStatus:    File{Path: "/var/lib/dpkg/status", ArchivePath: "var/lib/dpkg/status", SHA256: sha256Hex([]byte("x"))},
	}
}

func TestValidateValidDocument(t *testing.T) {
	if err := Validate(validMinimalSnapshot()); err != nil {
		t.Errorf("a well-formed minimal document must validate: %v", err)
	}
}

func TestValidateNil(t *testing.T) {
	if err := Validate(nil); err == nil {
		t.Error("expected an error for a nil document")
	}
}

func TestValidateWrongSchemaVersion(t *testing.T) {
	s := validMinimalSnapshot()
	s.SchemaVersion = "debark.snapshot/v2"
	assertVerificationError(t, s)
}

func TestValidateBadCreatedAt(t *testing.T) {
	s := validMinimalSnapshot()
	s.CreatedAt = "not a timestamp"
	assertVerificationError(t, s)
}

func TestValidateMissingDistroID(t *testing.T) {
	s := validMinimalSnapshot()
	s.Target.DistroID = ""
	assertVerificationError(t, s)
}

func TestValidateMissingVersionID(t *testing.T) {
	s := validMinimalSnapshot()
	s.Target.VersionID = ""
	assertVerificationError(t, s)
}

func TestValidateImplausibleArch(t *testing.T) {
	cases := []string{"", "AMD64", "amd 64", "amd/64", "../etc"}
	for _, a := range cases {
		s := validMinimalSnapshot()
		s.Target.Arch = a
		if err := Validate(s); err == nil {
			t.Errorf("arch %q should be rejected", a)
		}
	}
}

func TestValidatePlausibleArchAccepted(t *testing.T) {
	// Including a real but debark-unsupported architecture: Validate is a
	// format check, not a "can debark build for this" check.
	for _, a := range []string{"amd64", "i386", "arm64", "armhf", "riscv64", "kfreebsd-amd64", "hppa"} {
		s := validMinimalSnapshot()
		s.Target.Arch = a
		if err := Validate(s); err != nil {
			t.Errorf("arch %q should be accepted: %v", a, err)
		}
	}
}

func TestValidateImplausibleForeignArch(t *testing.T) {
	s := validMinimalSnapshot()
	s.Target.ForeignArchs = []string{"i386", "not valid!"}
	assertVerificationError(t, s)
}

func TestValidateMissingDpkgStatus(t *testing.T) {
	s := validMinimalSnapshot()
	s.DpkgStatus = File{}
	assertVerificationError(t, s)
}

// TestValidateOriginAssumedInstalled covers the one field of this document
// that travels beyond it: a bundle carries snapshot.json but not the
// snapshot's files, so origin.assumed_installed is the only copy of the base's
// installed set the target ever sees, and install reads it back to say which
// assumed packages the machine in front of it does not have.
//
// Two unrelated rules meet on that field, and this table holds both. The
// first is a disclosure rule rather than a bound: a captured snapshot's
// installed set is the inventory of a real machine, which D8 keeps out of
// artefacts, so a captured origin carrying the field is refused outright
// instead of being quietly emptied. The rest are the ordinary display-string
// rules from displaystrings.go, applied here because a package name that
// install prints is as good a carrier for a cursor-movement escape as a
// warning is.
func TestValidateOriginAssumedInstalled(t *testing.T) {
	synthesized := func(names ...string) func(*Snapshot) {
		return func(s *Snapshot) {
			s.Origin = Origin{Kind: OriginSynthesized, BaseID: "debian:12/minimal", AssumedInstalled: names}
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Snapshot)
		wantErr bool
	}{
		{
			// The one that matters. Everything else here bounds a string; this
			// refuses a claim -- that a measurement of a real machine may
			// carry its package inventory into a field built to be published.
			name: "captured carrying an assumed set",
			mutate: func(s *Snapshot) {
				s.Origin = Origin{Kind: OriginCaptured, AssumedInstalled: []string{"bash:amd64"}}
			},
			wantErr: true,
		},
		{
			name:    "synthesized with a raw escape in an entry",
			mutate:  synthesized("bash:amd64", "libc6:amd64"+cursorUpEraseLine),
			wantErr: true,
		},
		{
			// The same attack spelled as C1 CSI (U+009B) rather than ESC-[.
			// hasControlChars tests C0 per byte but C1 per rune, deliberately:
			// a lone 0x9b byte is not valid UTF-8 and a UTF-8 terminal will not
			// act on it, while the properly encoded two-byte form is one it
			// will. A per-byte check alone would let this through.
			name:    "synthesized with an encoded C1 CSI in an entry",
			mutate:  synthesized("bash:amd64", "libc6:amd64\u009b1A\u009b2K"),
			wantErr: true,
		},
		{
			name:    "synthesized with an over-long entry",
			mutate:  synthesized(strings.Repeat("x", maxAssumedInstalledLength+1)),
			wantErr: true,
		},
		{
			// The boundary from the accepting side, so the limit stays
			// inclusive: without it, an off-by-one that started refusing
			// exactly-256-byte names would pass every other case here.
			name:    "synthesized with an entry at exactly the length limit",
			mutate:  synthesized(strings.Repeat("x", maxAssumedInstalledLength)),
			wantErr: false,
		},
		{
			// Generated rather than checked in: the whole content of this case
			// is the size of the number, and a fixture big enough to state it
			// is not something the tree should have to carry.
			name: "synthesized with more entries than the cap allows",
			mutate: func(s *Snapshot) {
				names := make([]string, maxAssumedInstalled+1)
				for i := range names {
					names[i] = "pkg" + strconv.Itoa(i) + ":amd64"
				}
				synthesized(names...)(s)
			},
			wantErr: true,
		},
		{
			// What from-base actually writes: name:arch, sorted, foreign
			// architecture included. If this ever fails, the rules above have
			// stopped bounding the absurd and started rejecting the real.
			name:    "synthesized with an ordinary sorted list",
			mutate:  synthesized("base-files:amd64", "bash:amd64", "libc6:amd64", "libc6:i386"),
			wantErr: false,
		},
	}
	for _, tc := range cases {
		s := validMinimalSnapshot()
		tc.mutate(s)
		err := Validate(s)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("%s: Validate accepted it", tc.name)
		case tc.wantErr && errClass(err) != "verification":
			t.Errorf("%s: class = %s, want verification: %v", tc.name, errClass(err), err)
		case !tc.wantErr && err != nil:
			t.Errorf("%s: Validate refused it: %v", tc.name, err)
		}
	}
}

func TestValidateMalformedDigest(t *testing.T) {
	s := validMinimalSnapshot()
	s.DpkgStatus.SHA256 = "not-hex"
	assertVerificationError(t, s)

	s = validMinimalSnapshot()
	s.DpkgStatus.SHA256 = "abcd" // too short
	assertVerificationError(t, s)
}

func TestValidateUnsafeArchivePath(t *testing.T) {
	cases := []string{"../../etc/passwd", "/etc/passwd", `..\evil`, "a/../../b", ""}
	for _, p := range cases {
		s := validMinimalSnapshot()
		s.DpkgStatus.ArchivePath = p
		if err := Validate(s); err == nil {
			t.Errorf("archive_path %q should be rejected", p)
		}
	}
}

func TestValidateConflictingDuplicateArchivePath(t *testing.T) {
	s := validMinimalSnapshot()
	s.APT.Sources = []File{{
		Path:        "/etc/apt/sources.list.d/other",
		ArchivePath: s.DpkgStatus.ArchivePath, // same archive_path as dpkg_status
		SHA256:      sha256Hex([]byte("different content")),
	}}
	assertVerificationError(t, s)
}

func TestValidateSameArchivePathSameContentIsFine(t *testing.T) {
	// Not a real scenario Capture produces, but Validate should not reject
	// two File entries that happen to agree on both path and digest.
	s := validMinimalSnapshot()
	s.APT.Sources = []File{{
		Path:        s.DpkgStatus.Path,
		ArchivePath: s.DpkgStatus.ArchivePath,
		SHA256:      s.DpkgStatus.SHA256,
	}}
	if err := Validate(s); err != nil {
		t.Errorf("identical duplicate entries should not be rejected: %v", err)
	}
}

func TestValidateImplausibleFingerprint(t *testing.T) {
	s := validMinimalSnapshot()
	s.KeyringFingerprints = []KeyFingerprint{{Fingerprint: "not-hex", Keyring: "/x"}}
	assertVerificationError(t, s)

	s = validMinimalSnapshot()
	s.KeyringFingerprints = []KeyFingerprint{{Fingerprint: "abc123", Keyring: "/x"}} // wrong length
	assertVerificationError(t, s)
}

func TestValidatePlausibleFingerprintLengths(t *testing.T) {
	s := validMinimalSnapshot()
	s.KeyringFingerprints = []KeyFingerprint{
		{Fingerprint: "4D64FEC119C2029067D6E791F8D2585B8783D481", Keyring: "/x"}, // v4, 40 hex
	}
	if err := Validate(s); err != nil {
		t.Errorf("a well-formed v4 fingerprint should be accepted: %v", err)
	}
}

func assertVerificationError(t *testing.T, s *Snapshot) {
	t.Helper()
	err := Validate(s)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := errClass(err); got != "verification" {
		t.Errorf("class = %s, want verification", got)
	}
}
