package snapshot

import (
	"os"
	"sort"

	"github.com/inferops/debark/core/dferr"
)

// captureKeyrings resolves every Signed-By (deb822) and signed-by= (one-line)
// reference found in the already-captured sources files, plus the wholesale
// trusted.gpg.d capture, into Snapshot.APT.Keyrings and
// Snapshot.KeyringFingerprints.
//
// Only *referenced* keyrings are captured here -- unlike trusted.gpg.d, which
// is captured wholesale by the caller before this runs. /usr/share/keyrings
// and /etc/apt/keyrings otherwise commonly hold keys for archives the target
// does not even use; capturing the whole directory (as the bash prototype
// does) would both bloat the snapshot and record trust material broader than
// the target's actual captured policy.
//
// An inline armoured key (deb822 "Signed-By:" continuation block) has no
// separate file: it is already present verbatim inside the .sources file
// that carries it, so it contributes fingerprints but never an APT.Keyrings
// entry. Its KeyFingerprint.Keyring is that source file's own Path.
func captureKeyrings(root string, sourceFiles, trustedFiles []File, fs *FileSet, warn func(string, ...any)) (keyrings []File, fingerprints []KeyFingerprint) {
	referencedBy := map[string]map[string]bool{} // keyring target path -> set of source file paths
	var order []string

	addKey := func(k parsedKey, keyring string, signedBy []string) {
		fingerprints = append(fingerprints, KeyFingerprint{
			Fingerprint: k.Fingerprint,
			KeyID:       k.KeyID,
			UserIDs:     k.UserIDs,
			Keyring:     keyring,
			SignedBy:    signedBy,
		})
	}

	for _, sf := range sourceFiles {
		data, ok := fs.Bytes[sf.ArchivePath]
		if !ok {
			continue
		}
		for _, ref := range extractSignedBy(data) {
			switch {
			case ref.Armored != "":
				keys, err := parseInlineArmoredKey(ref.Armored)
				if err != nil {
					warn("snapshot: %s: inline Signed-By key: %v", sf.Path, err)
					continue
				}
				for _, k := range keys {
					addKey(k, sf.Path, []string{sf.Path})
				}
			case ref.Path != "":
				if referencedBy[ref.Path] == nil {
					referencedBy[ref.Path] = map[string]bool{}
					order = append(order, ref.Path)
				}
				referencedBy[ref.Path][sf.Path] = true
			}
		}
	}

	for _, kp := range order {
		rec, data, err := readKeyringAt(root, kp)
		if err != nil {
			warn("snapshot: keyring %s (referenced by Signed-By in %s): %v",
				kp, firstOf(sortedSetKeys(referencedBy[kp])), describeErr(err))
			continue
		}
		fs.Bytes[rec.ArchivePath] = data
		keyrings = append(keyrings, rec)

		signedBy := sortedSetKeys(referencedBy[kp])
		keys, perr := parseKeyringFile(data)
		if perr != nil {
			warn("snapshot: keyring %s: %v", kp, perr)
			continue
		}
		for _, k := range keys {
			addKey(k, kp, signedBy)
		}
	}

	for _, tf := range trustedFiles {
		data, ok := fs.Bytes[tf.ArchivePath]
		if !ok {
			continue
		}
		keys, err := parseKeyringFile(data)
		if err != nil {
			warn("snapshot: trusted keyring %s: %v", tf.Path, err)
			continue
		}
		for _, k := range keys {
			addKey(k, tf.Path, nil)
		}
	}

	sortFilesByPath(keyrings)
	sortKeyFingerprints(fingerprints)
	return keyrings, fingerprints
}

// readKeyringAt reads a keyring by its target-absolute path directly (not
// via captureDir, since a referenced keyring may live anywhere -- e.g.
// /usr/share/keyrings -- unlike the fixed directories a straight listing
// walks). It uses a scratch FileSet because captureFile's signature always
// records into one; the caller merges the single resulting entry itself once
// it knows the file is worth keeping.
func readKeyringAt(root, targetPath string) (File, []byte, error) {
	scratch := &FileSet{Bytes: map[string][]byte{}}
	rec, err := captureFile(root, targetPath, scratch)
	if err != nil {
		return File{}, nil, err
	}
	return rec, scratch.Bytes[rec.ArchivePath], nil
}

// describeErr renders a read failure the way a human wants to see it: a
// missing file and a permission-denied file are both just "why didn't this
// referenced keyring exist", not a Go error stack.
func describeErr(err error) string {
	if os.IsNotExist(err) {
		return "no such file"
	}
	if os.IsPermission(err) {
		return "permission denied"
	}
	return err.Error()
}

func firstOf(ss []string) string {
	if len(ss) == 0 {
		return "?"
	}
	return ss[0]
}

func sortedSetKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortKeyFingerprints sorts KeyringFingerprints by (Fingerprint, Keyring) so
// the document is stable regardless of directory-listing or map order, even
// when the same key appears in more than one captured keyring (e.g. an
// individual release key that is also present inside the combined
// *-archive-keyring.gpg). sort.SliceStable makes the rare remaining tie
// (identical fingerprint *and* keyring path) fall back to encounter order,
// which is itself deterministic.
func sortKeyFingerprints(ks []KeyFingerprint) {
	sort.SliceStable(ks, func(i, j int) bool {
		if ks[i].Fingerprint != ks[j].Fingerprint {
			return ks[i].Fingerprint < ks[j].Fingerprint
		}
		return ks[i].Keyring < ks[j].Keyring
	})
}

// doKeyMaterial is api.go's KeyMaterial body. See that doc comment for why
// this derivation is exported at all.
func doKeyMaterial(keyrings []File, fs *FileSet) ([]KeyFingerprint, error) {
	if fs == nil || fs.Bytes == nil {
		return nil, dferr.New(dferr.Usage, "snapshot: KeyMaterial: no file set")
	}
	var out []KeyFingerprint
	for _, kr := range keyrings {
		data, ok := fs.Bytes[kr.ArchivePath]
		if !ok {
			continue
		}
		keys, err := parseKeyringFile(data)
		if err != nil {
			return nil, dferr.Wrap(dferr.Verification, err,
				"snapshot: keyring %s is not a parseable OpenPGP keyring", kr.Path)
		}
		for _, k := range keys {
			out = append(out, KeyFingerprint{
				Fingerprint: k.Fingerprint,
				KeyID:       k.KeyID,
				UserIDs:     k.UserIDs,
				// Keyring is the file's own recorded target path, which is
				// the key deriveKeyMaterial looks these up by. SignedBy is
				// left empty: it records which captured *source files* named
				// this keyring, and that is a fact about a captured machine's
				// sources.list.d, which a caller of this function does not
				// have and must not invent.
				Keyring: kr.Path,
			})
		}
	}
	sortKeyFingerprints(out)
	return out, nil
}
