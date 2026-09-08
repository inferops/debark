package engine

import (
	"os"
	"path/filepath"

	"github.com/inferops/debark/core/bundle"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/sbom"
)

// writeSBOM renders the bundle's software bill of materials from the
// finished lock (Options.SBOM; defect 5 in the implementation notes) and writes it
// as bundle.SBOMFile ("sbom.cdx.json"), returning the path to record as
// manifest.BuildInput.SBOMRef. Like README.txt and evidence.json, it must
// exist on disk in its final form before buildManifest walks the tree
// (finalizeBundle's own ordering rule), so this is called from there,
// before that call, and never from bundle.Assemble's own, earlier pass.
//
// bundleID is the id finalizeBundle already derived for the README, and
// that manifest.Build will independently re-derive for the real manifest
// (manifest.NewBundleID is a pure function of lockDigest+createdAt, so both
// computations necessarily agree) — not whatever id bundle.Assemble's own
// internal, first-cut, discarded manifest build may have used earlier, from
// a lock digest computed before the doctor and closed-world folds landed in
// b.lockDoc. bundle.Input does carry its own SBOM field for exactly this
// purpose, but Assemble runs too early in the pipeline to use it correctly
// here: at that point the lock is not final yet (see finalizeBundle's doc
// comment), so an SBOM generated there would embed a bundle id that the
// bundle's own, final manifest then disagrees with. This mirrors
// evidence.json, which for the identical reason is also written directly in
// finalizeBundle rather than through bundle.Input.Evidence.
func (b *build) writeSBOM(bundleID string) (string, error) {
	doc, err := sbom.FromLock(b.lockDoc, bundleID, b.createdAt)
	if err != nil {
		return "", dferr.Wrap(dferr.Environment, err, "engine: build sbom")
	}
	out, err := doc.JSON()
	if err != nil {
		return "", dferr.Wrap(dferr.Environment, err, "engine: encode sbom")
	}
	if err := os.WriteFile(filepath.Join(b.bundleDir, bundle.SBOMFile), out, 0o644); err != nil {
		return "", dferr.Wrap(dferr.Environment, err, "engine: write %s", bundle.SBOMFile)
	}
	return bundle.SBOMFile, nil
}
