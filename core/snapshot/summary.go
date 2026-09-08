package snapshot

// Summary is a compact, JSON-tagged rendering of a Snapshot for `snapshot
// inspect`: target identity, counts, the sources list, key fingerprints,
// redactions and warnings. Unlike Snapshot, Summary is a view, not a wire
// format -- its shape may change between releases without a schema bump.
type Summary struct {
	SchemaVersion string `json:"schema_version"`
	CreatedAt     string `json:"created_at"`
	Tool          string `json:"tool"`

	DistroID     string   `json:"distro_id"`
	VersionID    string   `json:"version_id"`
	Codename     string   `json:"codename,omitempty"`
	PrettyName   string   `json:"pretty_name,omitempty"`
	Arch         string   `json:"arch"`
	ForeignArchs []string `json:"foreign_archs,omitempty"`
	APTVersion   string   `json:"apt_version,omitempty"`
	DpkgVersion  string   `json:"dpkg_version,omitempty"`

	HasMachineID bool         `json:"has_machine_id"`
	PhasedPolicy PhasedPolicy `json:"phased_policy"`

	// OriginKind, BaseID and BaseSource say whether this document is a
	// measurement of a real machine or an assumption about a stock release.
	// A view that omitted them would show a target block that reads
	// identically either way, which is exactly the confusion origin was made
	// required to prevent (ADR-014).
	OriginKind   string `json:"origin_kind"`
	BaseID       string `json:"base_id,omitempty"`
	BaseSource   string `json:"base_source,omitempty"`
	BaseDigest   string `json:"base_digest,omitempty"`
	AssumedCount int    `json:"assumed_installed_count,omitempty"`

	InstalledCount int `json:"installed_count"`

	Sources         []string `json:"sources,omitempty"`
	Preferences     []string `json:"preferences,omitempty"`
	ConfFiles       []string `json:"conf_files,omitempty"`
	TrustedKeyrings []string `json:"trusted_keyrings,omitempty"`
	Keyrings        []string `json:"keyrings,omitempty"`

	KeyFingerprints []SummaryKey `json:"key_fingerprints,omitempty"`

	Labels     map[string]string `json:"labels,omitempty"`
	Redactions []string          `json:"redactions,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// SummaryKey is one entry of Summary.KeyFingerprints.
type SummaryKey struct {
	Fingerprint string   `json:"fingerprint"`
	KeyID       string   `json:"key_id,omitempty"`
	UserIDs     []string `json:"user_ids,omitempty"`
	Keyring     string   `json:"keyring"`
	SignedBy    []string `json:"signed_by,omitempty"`
}

// Summarise renders s as an inspection-friendly Summary. It is a pure,
// in-memory projection: no I/O, no errors, always defined for any non-nil
// Snapshot (even a partial one Capture produced with warnings).
func Summarise(s *Snapshot) Summary {
	if s == nil {
		return Summary{}
	}
	sum := Summary{
		SchemaVersion: s.SchemaVersion,
		CreatedAt:     s.CreatedAt,
		Tool:          s.Tool.Name + "/" + s.Tool.Version,

		DistroID:     s.Target.DistroID,
		VersionID:    s.Target.VersionID,
		Codename:     s.Target.Codename,
		PrettyName:   s.Target.PrettyName,
		Arch:         s.Target.Arch,
		ForeignArchs: s.Target.ForeignArchs,
		APTVersion:   s.Target.APTVersion,
		DpkgVersion:  s.Target.DpkgVersion,

		HasMachineID: s.Target.MachineID != "",
		PhasedPolicy: s.PhasedPolicyFor(),

		OriginKind:   s.Origin.Kind,
		BaseID:       s.Origin.BaseID,
		BaseSource:   s.Origin.Source,
		BaseDigest:   s.Origin.SourceDigest,
		AssumedCount: len(s.Origin.AssumedInstalled),

		InstalledCount: s.InstalledCount,

		Labels:     s.Labels,
		Redactions: s.Redactions,
		Warnings:   s.Warnings,
	}
	if s.Tool.Edition != "" {
		sum.Tool += " (" + s.Tool.Edition + ")"
	}

	sum.Sources = filePaths(s.APT.Sources)
	sum.Preferences = filePaths(s.APT.Preferences)
	sum.ConfFiles = filePaths(s.APT.Conf)
	sum.TrustedKeyrings = filePaths(s.APT.Trusted)
	sum.Keyrings = filePaths(s.APT.Keyrings)

	// SummaryKey and KeyFingerprint have identical fields today, so
	// staticcheck offers SummaryKey(kf) instead of this literal. Declined
	// on purpose: the two types are identical by coincidence, not by
	// contract. Summary is a projection meant for human inspection, and
	// the field list below is the statement of what a summary is allowed
	// to carry. With a conversion, adding a field to KeyFingerprint --
	// which is the captured, target-derived type, the one redaction
	// exists for -- would silently start publishing it here. With the
	// literal, someone has to decide.
	for _, kf := range s.KeyringFingerprints {
		sum.KeyFingerprints = append(sum.KeyFingerprints, SummaryKey{ //nolint:staticcheck // S1016: explicit projection, not an accidental type twin; see above
			Fingerprint: kf.Fingerprint,
			KeyID:       kf.KeyID,
			UserIDs:     kf.UserIDs,
			Keyring:     kf.Keyring,
			SignedBy:    kf.SignedBy,
		})
	}
	return sum
}

func filePaths(files []File) []string {
	if len(files) == 0 {
		return nil
	}
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}
