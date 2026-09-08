package snapshot

import (
	"bytes"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// Keyring parsing: turns a captured keyring's bytes -- binary OpenPGP
// packets, or ASCII-armored, detected by content rather than file extension,
// since apt accepts both for Signed-By -- into the keys it contains.
//
// The trust model is explicit that a
// snapshot's keyrings are captured policy, not an independent root of trust:
// nothing here evaluates a signature or a revocation, only enumerates what
// keys and identities the target's apt was configured to trust. That is
// exactly openpgp.ReadKeyRing's own contract too ("Unsupported keys are
// ignored as long as at least a single valid key is found"), which is why a
// whole-file failure is the only error this ever needs to turn into a
// Warning: an unsupported *individual* key inside an otherwise-good keyring
// is already handled for us.

// parsedKey is one public key -- primary or subkey -- found in a keyring.
type parsedKey struct {
	Fingerprint string   // uppercase hex
	KeyID       string   // long key id, uppercase hex
	UserIDs     []string // sorted, for a deterministic document; empty for a subkey
}

// parseKeyringFile decodes one captured keyring into every key -- primary and
// subkey alike -- it contains, so KeyringFingerprints reflects every key
// material a Signed-By reference actually covers.
func parseKeyringFile(data []byte) ([]parsedKey, error) {
	el, err := readEntityList(data)
	if err != nil {
		return nil, err
	}
	return keysFromEntities(el), nil
}

// parseInlineArmoredKey parses one deb822 inline "Signed-By:" block: the
// continuation-line text, already reconstructed with real newlines and no
// leading indentation. It is always armored, by construction of the field it
// came from.
func parseInlineArmoredKey(armored string) ([]parsedKey, error) {
	el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		return nil, err
	}
	return keysFromEntities(el), nil
}

func readEntityList(data []byte) (openpgp.EntityList, error) {
	if looksArmored(data) {
		return openpgp.ReadArmoredKeyRing(bytes.NewReader(data))
	}
	return openpgp.ReadKeyRing(bytes.NewReader(data))
}

func looksArmored(data []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte("-----BEGIN PGP"))
}

func keysFromEntities(el openpgp.EntityList) []parsedKey {
	var keys []parsedKey
	for _, e := range el {
		if e == nil || e.PrimaryKey == nil {
			continue
		}
		keys = append(keys, parsedKey{
			Fingerprint: fingerprintHex(e.PrimaryKey.Fingerprint),
			KeyID:       e.PrimaryKey.KeyIdString(),
			UserIDs:     identityNames(e),
		})
		for _, sk := range e.Subkeys {
			if sk.PublicKey == nil {
				continue
			}
			keys = append(keys, parsedKey{
				Fingerprint: fingerprintHex(sk.PublicKey.Fingerprint),
				KeyID:       sk.PublicKey.KeyIdString(),
			})
		}
	}
	return keys
}

// identityNames returns an Entity's claimed identities, sorted: Identities is
// a map (keyed by name), and map iteration order is not a thing this project
// ever writes into an artefact.
func identityNames(e *openpgp.Entity) []string {
	if len(e.Identities) == 0 {
		return nil
	}
	names := make([]string, 0, len(e.Identities))
	for name := range e.Identities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func fingerprintHex(fp []byte) string {
	return strings.ToUpper(hex.EncodeToString(fp))
}
