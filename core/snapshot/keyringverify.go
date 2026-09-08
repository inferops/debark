package snapshot

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/inferops/debark/core/dferr"
)

// Keyring verification on load.
//
// docs/threat-model.md §3.2 names --approved-keys as *the* control against a
// snapshot that carries both a malicious source and the key that
// authenticates it. That control is only worth anything if the fingerprints
// it decides on are a fact about the key material the snapshot actually
// carries, rather than a claim the document makes about itself:
// KeyringFingerprints is written by the target, and the target is the
// untrusted side of the air gap. Two tamperings are otherwise free -- swap
// the keyring file's bytes while leaving the recorded fingerprints naming
// the genuine archive key, or record a fingerprint for a keyring the
// snapshot does not carry at all -- and both produce a document that
// verifies against itself, because nothing re-derived anything.
//
// So Open re-derives every fingerprint from the extracted bytes and refuses
// a document that disagrees, exactly as verifyExtractedFiles refuses a file
// whose bytes do not match its recorded digest. This does not change the
// schema: keyring_fingerprints stays a recorded field with the same meaning
// (ADR-008: captured policy, never an independent root of trust). What
// changes is that the record must now be *true of the bytes beside it*.
//
// A keyring whose bytes are not a parseable OpenPGP keyring at all is not by
// itself a refusal -- capture already tolerates one, recording a warning and
// no fingerprints for it, and an honest snapshot can carry one -- but it
// contributes no fingerprints here either, so nothing can ever be approved
// on its account. That is the deliberate fail-closed choice: unparseable
// means zero keys, never "trust the document's word for it".

// derivedKeyring is what one captured keyring's bytes actually contain,
// keyed by fingerprint. parseErr is set when the bytes are not a keyring at
// all, in which case keys is empty -- deliberately, see the file comment.
type derivedKeyring struct {
	keys        map[string]parsedKey
	archivePath string
	parseErr    error
}

// verifyKeyringFingerprints re-derives every OpenPGP fingerprint from the
// extracted key material and refuses a document whose KeyringFingerprints
// disagrees with it, in either direction: a claimed fingerprint the bytes do
// not contain, and a key the bytes do contain that the document failed to
// record. Both directions matter -- the first is how a swapped keyring keeps
// claiming the genuine archive key, the second is how a keyring smuggles in
// an extra key that no audit of the document would ever show.
func verifyKeyringFingerprints(s *Snapshot, filesDir string) error {
	// --no-keyrings: no keyring files and no fingerprints were captured, so
	// there is nothing recorded to be true or false about. Inline armoured
	// keys inside the captured sources are deliberately not enumerated in
	// that mode either (doCapture skips captureKeyrings wholesale), so the
	// "every derived key must be recorded" direction below would refuse an
	// honest --no-keyrings snapshot. Skipping costs nothing: with no
	// fingerprints recorded at all, --approved-keys has nothing to approve
	// and fails closed downstream.
	if len(s.KeyringFingerprints) == 0 && len(s.APT.Keyrings) == 0 && len(s.APT.Trusted) == 0 {
		return nil
	}

	derived, err := deriveKeyMaterial(s, filesDir)
	if err != nil {
		return err
	}

	claimed := map[string]map[string]bool{}
	for _, kf := range s.KeyringFingerprints {
		dk, ok := derived[kf.Keyring]
		if !ok {
			return dferr.New(dferr.Verification,
				"snapshot: keyring_fingerprints records %s as coming from %q, but the snapshot carries no such keyring: a fingerprint may only be recorded for key material the snapshot actually contains",
				kf.Fingerprint, kf.Keyring)
		}
		key, ok := dk.keys[kf.Fingerprint]
		if !ok {
			if dk.parseErr != nil {
				return dferr.New(dferr.Verification,
					"snapshot: keyring_fingerprints records %s as coming from %q, but that file's bytes are not a parseable OpenPGP keyring (%v), so it contains no keys at all",
					kf.Fingerprint, kf.Keyring, dk.parseErr)
			}
			return dferr.New(dferr.Verification,
				"snapshot: keyring_fingerprints records %s as coming from %q, but that keyring's bytes contain no such key (they contain %s)",
				kf.Fingerprint, kf.Keyring, describeFingerprints(dk.keys))
		}
		if kf.KeyID != "" && kf.KeyID != key.KeyID {
			return dferr.New(dferr.Verification,
				"snapshot: keyring_fingerprints records key id %q for %s in %q, but the key material says %q",
				kf.KeyID, kf.Fingerprint, kf.Keyring, key.KeyID)
		}
		if !sameStrings(kf.UserIDs, key.UserIDs) {
			// User ids are "for human output" (types.go), and human output is
			// exactly what an auditor reads when the threat model tells them
			// to check which keys a snapshot trusts -- so a user id the key material
			// does not carry is a lie aimed at that reader, not cosmetic.
			return dferr.New(dferr.Verification,
				"snapshot: keyring_fingerprints records user ids %v for %s in %q, but the key material carries %v",
				kf.UserIDs, kf.Fingerprint, kf.Keyring, key.UserIDs)
		}
		if claimed[kf.Keyring] == nil {
			claimed[kf.Keyring] = map[string]bool{}
		}
		claimed[kf.Keyring][kf.Fingerprint] = true
	}

	for _, path := range sortedKeys(derived) {
		dk := derived[path]
		for _, fp := range sortedKeys(dk.keys) {
			if !claimed[path][fp] {
				return dferr.New(dferr.Verification,
					"snapshot: %s contains key %s, which keyring_fingerprints does not record: the recorded fingerprints must be the whole truth about the key material carried beside them",
					path, fp)
			}
		}
	}
	return nil
}

// deriveKeyMaterial parses every piece of key material the snapshot carries
// -- the captured keyring files, plus the inline armoured keys embedded in
// captured deb822 sources -- and returns it keyed by the same string
// KeyFingerprint.Keyring uses: the keyring's own recorded path for a file,
// and the *source file's* path for an inline key (captureKeyrings records
// them that way, because an inline key has no file of its own).
func deriveKeyMaterial(s *Snapshot, filesDir string) (map[string]*derivedKeyring, error) {
	out := map[string]*derivedKeyring{}

	add := func(path, archivePath string, keys []parsedKey, parseErr error) error {
		dk, ok := out[path]
		if !ok {
			dk = &derivedKeyring{keys: map[string]parsedKey{}, archivePath: archivePath}
			out[path] = dk
		} else if dk.archivePath != archivePath {
			// The same target path captured twice from two different archive
			// members is ambiguous about which bytes are authoritative, and
			// core/apt's private root would silently keep only one of them.
			// The legitimate duplicate -- a Signed-By pointing into
			// trusted.gpg.d, so the same file lands in both APT.Trusted and
			// APT.Keyrings -- shares one archive path and lands here as a
			// harmless merge.
			return dferr.New(dferr.Verification,
				"snapshot: two captured keyrings both claim target path %q (%s and %s) with different content",
				path, dk.archivePath, archivePath)
		}
		if parseErr != nil {
			dk.parseErr = parseErr
			return nil
		}
		for _, k := range keys {
			dk.keys[k.Fingerprint] = k
		}
		return nil
	}

	for _, f := range append(append([]File(nil), s.APT.Keyrings...), s.APT.Trusted...) {
		if f.Path == "" && f.ArchivePath == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(filesDir, filepath.FromSlash(f.ArchivePath)))
		if err != nil {
			return nil, dferr.Wrap(dferr.Verification, err, "snapshot: %s: re-reading keyring to re-derive its fingerprints", f.Path)
		}
		keys, perr := parseKeyringFile(data)
		if err := add(f.Path, f.ArchivePath, keys, perr); err != nil {
			return nil, err
		}
	}

	for _, f := range s.APT.Sources {
		if f.ArchivePath == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(filesDir, filepath.FromSlash(f.ArchivePath)))
		if err != nil {
			return nil, dferr.Wrap(dferr.Verification, err, "snapshot: %s: re-reading source to re-derive its inline keys", f.Path)
		}
		for _, ref := range extractSignedBy(data) {
			if ref.Armored == "" {
				continue
			}
			keys, perr := parseInlineArmoredKey(ref.Armored)
			if perr != nil {
				// Capture warns and records nothing for an unparseable inline
				// block; mirror that exactly, or an honest snapshot carrying
				// one would be refused here.
				continue
			}
			if err := add(f.Path, f.ArchivePath, keys, nil); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// doFingerprintsIn is the real implementation behind FingerprintsIn: the
// uppercase-hex OpenPGP fingerprint of every key -- primary and subkey alike
// -- in one keyring file's raw bytes, sorted. Binary and ASCII-armored
// keyrings are both accepted, detected by content rather than by file
// extension, since apt accepts both for Signed-By.
//
// It is exported because core/apt must derive the same fingerprints from the
// keyring bytes it copies into a private root's trusted.gpg.d, rather than
// from the snapshot document's claim about them (docs/threat-model.md §3.2),
// and duplicating the armoured-versus-binary detection there would be a
// second place for the two to drift apart. An error means the bytes are not
// an OpenPGP keyring at all; callers enforcing an approved-key list must
// treat that as zero fingerprints and fail closed, never as "unknown".
func doFingerprintsIn(data []byte) ([]string, error) {
	keys, err := parseKeyringFile(data)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if seen[k.Fingerprint] {
			continue
		}
		seen[k.Fingerprint] = true
		out = append(out, k.Fingerprint)
	}
	sort.Strings(out)
	return out, nil
}

func describeFingerprints(keys map[string]parsedKey) string {
	if len(keys) == 0 {
		return "no keys"
	}
	return strings.Join(sortedKeys(keys), ", ")
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
