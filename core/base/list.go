package base

// The `snapshot list-bases --json` document, debark.baselist/v1.
//
// It lives here rather than in internal/cli because everything in it is a
// fact about a Definition, and this package is where a Definition and its
// digest are defined. Two consequences follow, and both are the reason for
// the move: adding a field to Definition now has a visible, compiling
// neighbour that decides whether the listing should show it, and the
// published shape can be held to its JSON Schema by
// api/schema/schema_test.go — which cannot reference an unexported type
// inside internal/cli, and should not depend on the CLI even if it could.
//
// This is a *listing*, not an artefact: it is never written into a bundle,
// never hashed and never signed. It is versioned all the same, because
// ADR-012 makes every --json output a versioned, schema-published object
// rather than ad hoc JSON.

// ListSchemaVersion versions the `snapshot list-bases --json` shape.
const ListSchemaVersion = "debark.baselist/v1"

// List is one `snapshot list-bases --json` document: the bases this binary
// has compiled in, rendered for one architecture.
type List struct {
	// SchemaVersion is always ListSchemaVersion.
	SchemaVersion string `json:"schema_version"`
	// Arch is the dpkg architecture the listing was rendered for, which is
	// part of the answer rather than context around it: a base's sources and
	// therefore its digest differ per architecture (Ubuntu's ports archive
	// serves arm64 and the main archive serves amd64), so "which bases exist"
	// has no architecture-free answer.
	Arch string `json:"arch"`
	// Bases is one entry per definition, in the order the caller supplied
	// them — for the builtin table, Builtin's display order, which is the
	// same order the human table prints. Order is not sorted here: the two
	// renderings of one listing must agree row for row.
	Bases []ListEntry `json:"bases"`
}

// ListEntry is one base in a List.
//
// It is a projection of Definition, not Definition itself, and the rule for
// what survives the projection is: everything that decides what the base
// *claims*, and nothing that is merely how it is configured. So Seeds,
// Excludes and Recommends are here, and Sources and Keyrings are not — those
// are long, are the part an operator overrides, and an archive URI per row
// would bury the fields a person is actually scanning for. Digest is added,
// because it is the only thing in the listing that distinguishes two builds
// claiming the same id; see Digest's own comment.
//
// api/schema/schema_test.go holds that rule to the code: a new field on
// Definition fails there until someone decides which side of it the field
// falls on.
type ListEntry struct {
	// ID is the base id, "<distro>:<version>/<variant>".
	ID string `json:"id"`
	// Description is the one-line summary, when the definition carries one.
	Description string `json:"description,omitempty"`

	// DistroID, VersionID and Codename are the target identity the
	// synthesized snapshot would claim.
	DistroID  string `json:"distro_id"`
	VersionID string `json:"version_id"`
	Codename  string `json:"codename"`
	// Variant is desktop, server, minimal or an operator's own word.
	Variant string `json:"variant,omitempty"`
	// Arch is the architecture this row was materialised for; it equals the
	// enclosing List's Arch, restated so one entry is self-contained when a
	// consumer picks rows out of the array.
	Arch string `json:"arch"`

	// Seeds are the metapackages whose closure stands for a stock install.
	Seeds []string `json:"seeds"`
	// Excludes are packages that closure names which this base nonetheless
	// does not claim the target already has. Shown because it changes the
	// claim: two bases with identical seeds and different excludes assume
	// different machines, and only the digest would otherwise say so.
	Excludes []string `json:"excludes,omitempty"`
	// Recommends is APT::Install-Recommends for the seed resolution.
	Recommends bool `json:"recommends"`
	// Digest is the definition's canonical-JSON SHA-256 — the same value the
	// synthesized snapshot records in origin.source_digest, so an operator
	// can tell which listed base a snapshot in hand was built from.
	Digest string `json:"digest"`
}

// NewList projects defs into the published listing shape.
//
// It takes the definitions rather than looking them up from arch, so that the
// human table and the --json object are guaranteed to describe the same set:
// the caller resolves the bases once and renders that one answer both ways.
// Two lookups could disagree — quietly, and only on the machine where they
// did — which is exactly the class of bug a versioned contract is for.
func NewList(arch string, defs []Definition) (List, error) {
	entries := make([]ListEntry, 0, len(defs))
	for _, d := range defs {
		dg, err := Digest(d)
		if err != nil {
			return List{}, err
		}
		entries = append(entries, ListEntry{
			ID:          d.ID,
			Description: d.Description,
			DistroID:    d.DistroID,
			VersionID:   d.VersionID,
			Codename:    d.Codename,
			Variant:     d.Variant,
			Arch:        d.Arch,
			// Copied, not aliased: an entry handed to a caller must not be a
			// window onto the definition it was projected from.
			Seeds:      append([]string(nil), d.Seeds...),
			Excludes:   append([]string(nil), d.Excludes...),
			Recommends: d.Recommends,
			Digest:     dg,
		})
	}
	return List{SchemaVersion: ListSchemaVersion, Arch: arch, Bases: entries}, nil
}
