package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// This file implements the corrupted-bundle fixtures' mutations (the
// tamper matrix: "modified deb, added file, removed file, edited
// manifest, swapped signature, wrong key" — "each tamper case must fail
// before apt runs"). Every mutation operates on a bundle already copied out
// to the host as a plain directory, which is why the harness always builds
// with `--out DIR` rather than `--tar FILE`: flipping a byte in a directory
// tree needs no container and no tar/zstd round trip.
//
// Field names below (schema_version, bundle_id, files[].path/size/sha256,
// signatures[].signature) are the debark.manifest/v1 and
// debark.signature/v1 JSON shapes from core/manifest/types.go. That file
// is frozen (docs/dev/contract-brief.md), but the surrounding core/manifest
// package is not, and importing a package to reach one frozen file in it
// would make this harness's own buildability hostage to a sibling file
// someone else in the tree is mid-editing (observed directly during
// development: core/snapshot briefly failed to build this way). Generic
// JSON manipulation keeps this package's
// dependency surface to core/dferr and core/distro only, both single-file
// and frozen outright.

// TamperExtra carries the extra host-side bytes a couple of tamper kinds
// need beyond the bundle directory itself.
type TamperExtra struct {
	// DecoySigBytes is a debark.manifest.sig file from a *different*,
	// separately built bundle signed with the *same* trusted key — used by
	// TamperSwappedSignature to prove a structurally valid signature for the
	// wrong content is rejected, distinctly from TamperWrongKey (a
	// structurally-would-be-valid-if-trusted signature checked against an
	// untrusted key).
	DecoySigBytes []byte
}

// ApplyTamper mutates the bundle at dir according to spec. TamperWrongKey is
// intentionally a no-op here: it is applied by the caller choosing which
// public key to hand to `debark verify`, not by touching the bundle.
func ApplyTamper(spec TamperSpec, dir string, extra TamperExtra) error {
	switch spec.Kind {
	case TamperModifiedDeb:
		return tamperModifiedDeb(dir)
	case TamperAddedFile:
		return tamperAddedFile(dir)
	case TamperRemovedFile:
		return tamperRemovedFile(dir)
	case TamperEditedManifest:
		return tamperEditedManifest(dir)
	case TamperSwappedSignature:
		return tamperSwappedSignature(dir, extra.DecoySigBytes)
	case TamperWrongKey:
		return nil // handled by the caller at verify-invocation time
	case TamperSymlinkedFile:
		return nil // applied inside the fresh target; see ApplyTamperInTarget
	default:
		return fmt.Errorf("tamper: unknown kind %q", spec.Kind)
	}
}

// tamperSymlinkedFileScript replaces the first pool .deb with a SYMLINK to a
// byte-identical copy of itself parked outside the bundle.
//
// Every byte the link resolves to still matches the signed manifest exactly,
// so no digest check can tell the difference: the only thing wrong with the
// bundle is what the entry IS. That is the whole point — the refusal under
// test has to happen because of the entry type, not because of its contents,
// or the fixture would merely be re-testing the modified-deb case.
//
// It is run inside the fresh target rather than on the host, which is the
// one exception to this file's "every mutation operates on the bundle
// already copied out to the host" rule, for two reasons that both matter.
// First, os.Symlink needs SeCreateSymbolicLinkPrivilege on Windows, where
// this harness is routinely developed and run, so a host-side version would
// be blocked on exactly the machine most likely to run it — and a fixture
// that is blocked on the developer's own host is a fixture nobody ever
// watches fail. Second, `docker cp` does not transfer a symlink as a
// symlink, so even a host that could create one could not deliver it.
//
// The pool layout is repository.PoolPath's: repo/pool/<prefix>/<name>/<file>,
// so the three-level glob is exact rather than a guess, and POSIX sh expands
// a glob in sorted order, which makes the choice of .deb deterministic
// across a repeated run for the same reason firstPoolDeb sorts.
const tamperSymlinkedFileScript = `
set -e
target=""
for f in %s/repo/pool/*/*/*.deb; do
	[ -f "$f" ] || continue
	target="$f"
	break
done
if [ -z "$target" ]; then
	echo "tamper: no pool .deb found under %s/repo/pool" >&2
	exit 1
fi
outside=/var/tmp/dfe2e-tamper-outside-bundle
mkdir -p "$outside"
cp "$target" "$outside/blob"
rm "$target"
ln -s "$outside/blob" "$target"
[ -L "$target" ] || { echo "tamper: $target is not a symlink after ln -s" >&2; exit 1; }
`

// ApplyTamperInTarget applies the tamper kinds that have to be made inside
// the container holding the bundle rather than on the host copy. It is a
// no-op for every kind ApplyTamper handles, so the two are safe to call in
// sequence.
func ApplyTamperInTarget(ctx context.Context, c *Container, kind TamperKind, bundlePath string) error {
	if kind != TamperSymlinkedFile {
		return nil
	}
	script := fmt.Sprintf(tamperSymlinkedFileScript, bundlePath, bundlePath)
	if _, err := c.ShellMust(ctx, ExecOpts{}, script); err != nil {
		return fmt.Errorf("tamper %s in %s: %w", kind, c.Name, err)
	}
	return nil
}

// firstPoolDeb finds the first .deb file under dir/repo/pool, sorted, so the
// choice is deterministic across a repeated run.
func firstPoolDeb(dir string) (string, error) {
	poolDir := filepath.Join(dir, "repo", "pool")
	var found string
	err := filepath.WalkDir(poolDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || found != "" || d.IsDir() {
			return err
		}
		if filepath.Ext(p) == ".deb" {
			found = p
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk %s: %w", poolDir, err)
	}
	if found == "" {
		return "", fmt.Errorf("no .deb file found under %s", poolDir)
	}
	return found, nil
}

func tamperModifiedDeb(dir string) error {
	debPath, err := firstPoolDeb(dir)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(debPath)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("tamper: %s is empty", debPath)
	}
	mid := len(data) / 2
	data[mid] ^= 0xFF
	// debPath came from firstPoolDeb(dir), i.e. a walk of the bundle the
	// harness itself just built in its own temp directory; corrupting it in
	// place is the whole point of this function (it is how the e2e suite
	// proves `verify` rejects a modified .deb). Writing back to the exact
	// path we read from cannot traverse anywhere the read did not already go.
	// #nosec G703 -- writes back to the path just read, inside the harness's own temp bundle
	return os.WriteFile(debPath, data, 0o644)
}

func tamperAddedFile(dir string) error {
	poolDir := filepath.Join(dir, "repo", "pool")
	extra := filepath.Join(poolDir, "dfe2e-unexpected-extra-file.bin")
	return os.WriteFile(extra, []byte("this file is not in the manifest\n"), 0o644)
}

func tamperRemovedFile(dir string) error {
	debPath, err := firstPoolDeb(dir)
	if err != nil {
		return err
	}
	return os.Remove(debPath)
}

// tamperEditedManifest changes bundle_id, a field present on every manifest
// and untouched by any file-content check, so the mutation unambiguously
// exercises the "signature's manifest_sha256 no longer matches the
// manifest's canonical bytes" path (verify.ProblemManifestDigest) rather
// than a file-digest mismatch.
func tamperEditedManifest(dir string) error {
	manifestPath := filepath.Join(dir, "debark.manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", manifestPath, err)
	}
	id, _ := doc["bundle_id"].(string)
	doc["bundle_id"] = id + "-tampered-by-dfe2e-harness"
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, out, 0o644)
}

func tamperSwappedSignature(dir string, decoySig []byte) error {
	if len(decoySig) == 0 {
		return fmt.Errorf("tamper: swapped-signature requires a decoy signature file")
	}
	sigPath := filepath.Join(dir, "debark.manifest.sig")
	return os.WriteFile(sigPath, decoySig, 0o644)
}
