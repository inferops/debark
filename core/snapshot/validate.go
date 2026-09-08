package snapshot

import (
	"strings"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
)

// doValidate checks a document against the schema rules: known schema
// version, required fields present, digests well formed, architectures
// plausible, and every ArchivePath a safe, relative, non-escaping member
// name -- the last because a snapshot is untrusted input that Open() is
// about to extract to disk, and a crafted ArchivePath is exactly how a "zip
// slip" attack works. All failures are dferr.Verification: this function
// exists to decide whether a document can be trusted at all, the same
// family of failure as a digest mismatch.
func doValidate(s *Snapshot) error {
	if s == nil {
		return dferr.New(dferr.Verification, "snapshot: nil document")
	}
	if s.SchemaVersion != SchemaVersion {
		return dferr.New(dferr.Verification, "snapshot: unknown schema_version %q (want %q)", s.SchemaVersion, SchemaVersion)
	}
	if _, err := canonical.ParseTime(s.CreatedAt); err != nil {
		return dferr.New(dferr.Verification, "snapshot: created_at: %v", err)
	}
	if strings.TrimSpace(s.Target.DistroID) == "" {
		return dferr.New(dferr.Verification, "snapshot: target.distro_id is required")
	}
	if strings.TrimSpace(s.Target.VersionID) == "" {
		return dferr.New(dferr.Verification, "snapshot: target.version_id is required")
	}
	if !plausibleArch(s.Target.Arch) {
		return dferr.New(dferr.Verification, "snapshot: target.arch %q is not a plausible dpkg architecture", s.Target.Arch)
	}
	for _, a := range s.Target.ForeignArchs {
		if !plausibleArch(a) {
			return dferr.New(dferr.Verification, "snapshot: target.foreign_archs contains %q, not a plausible dpkg architecture", a)
		}
	}
	if s.DpkgStatus.Path == "" || s.DpkgStatus.ArchivePath == "" {
		return dferr.New(dferr.Verification, "snapshot: dpkg_status is required")
	}

	// origin is required, and an absent one is an error rather than a default
	// (ADR-014). Defaulting absent to "captured" would let a synthesized
	// snapshot that lost the field read as a measurement of a real machine --
	// the strongest claim in the format asserted by the document that says
	// least. The safe direction is to refuse to guess.
	switch s.Origin.Kind {
	case OriginCaptured:
		if s.Origin.BaseID != "" || s.Origin.Source != "" || s.Origin.SourceDigest != "" {
			return dferr.New(dferr.Verification,
				"snapshot: origin.kind is %q but carries base definition fields; those belong only to a synthesized snapshot", OriginCaptured)
		}
		if len(s.Origin.AssumedInstalled) > 0 {
			// Checked separately from the three fields above, and worth its
			// own message, because this one is not merely out of place: the
			// installed-package list of a real machine is the sensitive
			// inventory D8 keeps out of artefacts, and origin.assumed_installed
			// is the one field in this format that travels into a bundle.
			return dferr.New(dferr.Verification,
				"snapshot: origin.kind is %q but carries origin.assumed_installed; a captured snapshot's installed set is a measurement of a real machine and never leaves it in this field", OriginCaptured)
		}
	case OriginSynthesized:
		if strings.TrimSpace(s.Origin.BaseID) == "" {
			return dferr.New(dferr.Verification, "snapshot: origin.kind is %q but origin.base_id is empty", OriginSynthesized)
		}
		if s.Origin.SourceDigest != "" && !digest.Valid(s.Origin.SourceDigest) {
			return dferr.New(dferr.Verification, "snapshot: origin.source_digest %q is not a well-formed digest", s.Origin.SourceDigest)
		}
		if err := checkAssumedInstalled(s.Origin.AssumedInstalled); err != nil {
			return err
		}
	case "":
		return dferr.New(dferr.Verification, "snapshot: origin.kind is required").
			WithHint("A snapshot must say whether it was captured from a real machine or synthesized from a base definition; debark will not assume the stronger of the two.")
	default:
		return dferr.New(dferr.Verification, "snapshot: origin.kind %q is not %q or %q", s.Origin.Kind, OriginCaptured, OriginSynthesized)
	}

	if err := checkDisplayStrings(s); err != nil {
		return err
	}

	seen := map[string]File{}
	for _, f := range s.Files() {
		if f.Path == "" && f.ArchivePath == "" {
			continue
		}
		if hasControlChars(f.Path) {
			return dferr.New(dferr.Verification, "snapshot: path %q contains control characters", f.Path)
		}
		if !digest.Valid(f.SHA256) {
			return dferr.New(dferr.Verification, "snapshot: %s: sha256 %q is not a well-formed digest", f.Path, f.SHA256)
		}
		if !safeArchivePath(f.ArchivePath) {
			return dferr.New(dferr.Verification, "snapshot: %s: archive_path %q is unsafe", f.Path, f.ArchivePath)
		}
		if prior, ok := seen[f.ArchivePath]; ok && (prior.Path != f.Path || prior.SHA256 != f.SHA256) {
			return dferr.New(dferr.Verification,
				"snapshot: archive_path %q is claimed by both %s and %s with different content",
				f.ArchivePath, prior.Path, f.Path)
		}
		seen[f.ArchivePath] = f
	}

	for _, kf := range s.KeyringFingerprints {
		if !plausibleFingerprint(kf.Fingerprint) {
			return dferr.New(dferr.Verification, "snapshot: keyring_fingerprints contains %q, not a plausible OpenPGP fingerprint", kf.Fingerprint)
		}
	}
	return nil
}

// plausibleArch is a format check, not a support check: it accepts anything
// shaped like a dpkg architecture tuple. distro.Architectures() is the much
// narrower list debark can actually build for; a valid snapshot may
// legitimately name an architecture debark cannot build (an obscure port,
// say) -- that is a resolution-time problem, not a malformed document.
func plausibleArch(a string) bool {
	if a == "" || len(a) > 32 {
		return false
	}
	for i, r := range a {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		case r == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// plausibleFingerprint accepts a v4 (40 hex chars, SHA-1) or v6/RFC 9580 (64
// hex chars, SHA-256) OpenPGP fingerprint, uppercase.
func plausibleFingerprint(fp string) bool {
	if len(fp) != 40 && len(fp) != 64 {
		return false
	}
	for _, r := range fp {
		if !((r >= '0' && r <= '9') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// doDigest returns the canonical digest of a snapshot document.
func doDigest(s *Snapshot) (string, error) {
	if s == nil {
		return "", dferr.New(dferr.Usage, "snapshot: cannot digest a nil document")
	}
	d, err := canonical.Digest(s)
	if err != nil {
		return "", dferr.Wrap(dferr.Usage, err, "snapshot: digest")
	}
	return d, nil
}
