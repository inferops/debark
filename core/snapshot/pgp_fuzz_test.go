package snapshot

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// FuzzParseKeyringFile fuzzes the captured-keyring reader, which is a
// third-party OpenPGP packet parser (ProtonMail/go-crypto) fed bytes the
// builder does not control: a keyring travels inside the snapshot, and the
// snapshot crosses the air gap on removable media (docs/threat-model.md
// §3.2). Two things now depend on what this function returns, not merely on
// what the document claims -- verifyKeyringFingerprints refuses a document
// whose keyring_fingerprints disagrees with the bytes beside it, and
// --approved-keys re-derives fingerprints through FingerprintsIn rather than
// trusting the snapshot's self-asserted claim -- so this parser sits
// directly underneath a security decision.
//
// Four invariants, none of them "it did not crash":
//
//  1. Every fingerprint this parser produces is one doValidate would accept.
//     plausibleFingerprint is the validator's own predicate for
//     keyring_fingerprints, and capture writes exactly these values into that
//     field. A shape the parser can emit but the validator rejects (a v3
//     key's 16-byte MD5 fingerprint, say, which is 32 hex characters rather
//     than 40 or 64) would make an honest capture produce a document that can
//     never be opened again -- and, worse, would make an --approved-keys
//     comparison compare against something the schema says cannot exist.
//
//  2. Parsing is deterministic. identityNames sorts because Entity.Identities
//     is a map, and map order is not a thing this project writes into an
//     artefact; UserIDs order is load-bearing beyond the digest, because
//     verifyKeyringFingerprints compares recorded against derived user ids
//     with sameStrings, which is order-sensitive. Unsorted or unstable output
//     would make a snapshot intermittently fail to verify against itself.
//
//  3. FingerprintsIn -- the exported entry point core/apt uses to derive the
//     trusted set for a private apt root -- agrees with parseKeyringFile.
//     The whole reason FingerprintsIn is exported (see doFingerprintsIn's
//     doc) is so the two cannot drift; this is that promise, checked.
//
//  4. An error means zero keys. keyringverify.go's fail-closed rule is
//     "unparseable means zero keys, never trust the document's word for it",
//     and a parser that returned an error *and* some keys would let a caller
//     that only checks one of the two act on half-parsed material.
func FuzzParseKeyringFile(f *testing.F) {
	debian := filepath.Join("..", "..", "testdata", "real-targets", "debian-12-state.tar.gz")
	ubuntu := filepath.Join("..", "..", "testdata", "real-targets", "ubuntu-2404-state.tar.gz")

	// Real captured key material, both encodings parseKeyringFile detects by
	// content: a binary keyring and an ASCII-armored one. Small on purpose --
	// a seed corpus runs as an ordinary unit test on every CI run.
	for _, seed := range [][]byte{
		fuzzRealKeyring(),
		realTargetMember(debian, "target-state/apt/trusted.gpg.d/debian-archive-bookworm-stable.asc"),
		realTargetMember(ubuntu, "target-state/apt/trusted.gpg.d/ubuntu-keyring-2018-archive.gpg"),
	} {
		if len(seed) > 0 {
			f.Add(seed)
		}
	}

	// Derived hostile shapes, built from the real material rather than from
	// nothing: a fuzzer that has to discover valid OpenPGP packet framing on
	// its own never gets past the first byte, so the interesting mutations
	// start from something that already parses.
	if base := fuzzRealKeyring(); len(base) > 8 {
		f.Add(base[:len(base)/2]) // truncated mid-packet
		f.Add(append(append([]byte(nil), base...), base...))
		flipped := append([]byte(nil), base...)
		flipped[len(flipped)/2] ^= 0xFF
		f.Add(flipped)
		// A trailing NUL: apt reads keyrings as opaque bytes, and a keyring
		// that parses only because the parser stopped early is a keyring
		// whose recorded fingerprints do not describe all of its content.
		f.Add(append(append([]byte(nil), base...), 0x00))
	}

	f.Add([]byte{}) // a real 0-byte keyring exists in the Ubuntu fixture
	f.Add([]byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nnot base64 at all\n-----END PGP PUBLIC KEY BLOCK-----\n"))
	f.Add([]byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQENBF")) // armor header, never closed
	f.Add([]byte("   \t\r\n-----BEGIN PGP"))                        // looksArmored's TrimLeft boundary
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})
	// Old-format public-key packet (tag 6) declaring a 65535-byte body that
	// is not there, and a new-format packet declaring a 4 GiB one: both are
	// the "trust the declared length" shape that turns a parser into an
	// allocator.
	f.Add([]byte{0x99, 0xFF, 0xFF})
	f.Add([]byte{0xC6, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, data []byte) {
		keys, err := parseKeyringFile(data)

		if err != nil {
			if len(keys) != 0 {
				t.Fatalf("parseKeyringFile returned %d keys alongside error %v: "+
					"a fail-closed caller that only checks the error would still act on them", len(keys), err)
			}
			// FingerprintsIn must fail closed on the same bytes, or core/apt
			// would build a trusted set out of material this package refused.
			if fps, ferr := doFingerprintsIn(data); ferr == nil {
				t.Fatalf("parseKeyringFile failed (%v) but FingerprintsIn accepted the same bytes, returning %v", err, fps)
			}
			return
		}

		for i, k := range keys {
			if !plausibleFingerprint(k.Fingerprint) {
				t.Fatalf("key %d: parseKeyringFile derived fingerprint %q, which doValidate's own "+
					"plausibleFingerprint rejects: capture would write a document that can never be opened again", i, k.Fingerprint)
			}
			if k.KeyID != "" && !isUpperHex(k.KeyID, 16) {
				t.Fatalf("key %d: key id %q is not 16 uppercase hex characters (types.go: \"the long key id\")", i, k.KeyID)
			}
			if !sortedAscending(k.UserIDs) {
				t.Fatalf("key %d: user ids %v are not sorted; verifyKeyringFingerprints compares them "+
					"with sameStrings, which is order-sensitive, so this snapshot would not verify against itself", i, k.UserIDs)
			}
		}

		// Determinism: same bytes, same keys, every time.
		again, againErr := parseKeyringFile(data)
		if againErr != nil {
			t.Fatalf("parseKeyringFile succeeded then failed on identical bytes: %v", againErr)
		}
		if !reflect.DeepEqual(keys, again) {
			t.Fatalf("parseKeyringFile is not deterministic:\nfirst:  %+v\nsecond: %+v", keys, again)
		}

		// The exported derivation must see exactly the same key set.
		fps, ferr := doFingerprintsIn(data)
		if ferr != nil {
			t.Fatalf("parseKeyringFile accepted these bytes but FingerprintsIn rejected them: %v", ferr)
		}
		want := map[string]bool{}
		for _, k := range keys {
			want[k.Fingerprint] = true
		}
		got := map[string]bool{}
		for _, fp := range fps {
			got[fp] = true
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("FingerprintsIn and parseKeyringFile disagree about the key material:\nparse: %v\nFingerprintsIn: %v", want, fps)
		}
		if !sortedAscending(fps) {
			t.Fatalf("FingerprintsIn returned %v, which its own doc says is sorted", fps)
		}
	})
}

// isUpperHex reports whether s is exactly n uppercase hexadecimal digits.
func isUpperHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// sortedAscending reports whether ss is in non-decreasing order. Used instead
// of sorting a copy and comparing, so a failure message can show the original
// order the caller actually produced.
func sortedAscending(ss []string) bool {
	for i := 1; i < len(ss); i++ {
		if strings.Compare(ss[i-1], ss[i]) > 0 {
			return false
		}
	}
	return true
}
