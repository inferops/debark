// Package sbom writes a minimal, native CycloneDX 1.6 JSON software bill of
// materials for a debark bundle: one component per package in the lock,
// plus the bundle itself as the root component.
//
// It is deliberately small — a native minimal CycloneDX writer, with an
// optional syft exec for richer output and no Syft dependency graph in the
// binary — and
// stays inside the do-not-build boundary: no vulnerability data, no
// licence adjudication, no dependency graph between components. An operator
// who wants a richer document with those things can install syft; Build
// (native) is always available as the fallback that needs nothing installed.
//
// Every function here is pure: no clock, no filesystem, no network. Callers
// supply the timestamp and bundle identity explicitly, which is also what
// makes the golden-file test in cyclonedx_test.go possible.
package sbom

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
)

// BOMFormat and SpecVersion are the two fields the CycloneDX schema requires
// at the top level, and the only two whose values are fixed.
const (
	BOMFormat   = "CycloneDX"
	SpecVersion = "1.6"
)

// SourcePackageProperty is the CycloneDX property name debark uses to
// record a component's Debian source package, namespaced per the CycloneDX
// property-taxonomy convention (vendor:name) since it is not one of the
// spec's built-in fields.
const SourcePackageProperty = "debark:source-package"

// ComponentType is the CycloneDX component "type" debark writes for every
// package component. "library" is the classification used for OS/distro
// packages by other SBOM tools (Syft, Trivy) in the absence of a more
// specific CycloneDX type for "an apt package".
const ComponentType = "library"

// RootComponentType is the type of the metadata.component that represents the
// bundle itself.
const RootComponentType = "application"

// Options controls Build. Every field that would otherwise require a clock or
// randomness is supplied here so the writer stays pure.
type Options struct {
	// BundleID identifies the bundle (e.g. manifest.Manifest.BundleID). Used
	// as the root component's name and bom-ref, and to derive SerialNumber
	// deterministically when SerialNumber is empty.
	BundleID string
	// BundleVersion is an optional version string for the root component
	// (e.g. the lock's target codename or the tool version).
	BundleVersion string
	// Timestamp is RFC 3339 UTC (canonical.Time), required. Build does not
	// call time.Now() itself.
	Timestamp string
	// SerialNumber is "urn:uuid:...". When empty, Build derives one
	// deterministically from BundleID, so the same bundle id always produces
	// the same serial number instead of a random one.
	SerialNumber string
	// ToolName and ToolVersion optionally identify the generator in
	// metadata.tools. Both empty omits metadata.tools entirely.
	ToolName    string
	ToolVersion string
}

// Component is one package this writer turns into a CycloneDX component. It
// is deliberately narrow: only the fields the document needs, so callers do
// not need to hand Build a full lock.Package.
type Component struct {
	Name          string
	Version       string
	Arch          string
	Distro        string // distro id for the purl, e.g. "debian", "ubuntu"
	SHA256        string
	SourcePackage string
}

// Document is a minimal CycloneDX 1.6 BOM. Field order matches the struct
// declaration order in Go's encoding/json, so JSON() output is deterministic
// without needing canonical.Marshal's key-sorting (this document is not
// signed or hashed with JCS; it is hashed as a plain bundle file, like every
// other file the manifest lists — see core/manifest).
type Document struct {
	BOMFormat    string         `json:"bomFormat"`
	SpecVersion  string         `json:"specVersion"`
	SerialNumber string         `json:"serialNumber"`
	Version      int            `json:"version"`
	Metadata     *Metadata      `json:"metadata,omitempty"`
	Components   []BOMComponent `json:"components"`
}

// Metadata is CycloneDX's metadata object, restricted to the fields this
// writer populates.
type Metadata struct {
	Timestamp string        `json:"timestamp,omitempty"`
	Tools     *Tools        `json:"tools,omitempty"`
	Component *BOMComponent `json:"component,omitempty"`
}

// Tools is the CycloneDX 1.5+ object form of metadata.tools (the bare-array
// form is deprecated).
type Tools struct {
	Components []BOMComponent `json:"components"`
}

// BOMComponent is a CycloneDX component. Only the fields this writer ever
// sets are present; every one of them is a valid key under
// definitions/component in the CycloneDX 1.6 schema (additionalProperties is
// false there, so an unrecognised key would fail validation).
type BOMComponent struct {
	Type       string     `json:"type"`
	BOMRef     string     `json:"bom-ref,omitempty"`
	Name       string     `json:"name"`
	Version    string     `json:"version,omitempty"`
	PURL       string     `json:"purl,omitempty"`
	Hashes     []Hash     `json:"hashes,omitempty"`
	Properties []Property `json:"properties,omitempty"`
}

// Hash is a CycloneDX hash object.
type Hash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

// Property is a CycloneDX name/value property.
type Property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HashAlgSHA256 is the CycloneDX hash-alg enum value for SHA-256, the only
// algorithm debark records (it is the only one the lock carries).
const HashAlgSHA256 = "SHA-256"

// Build renders a CycloneDX 1.6 document natively. Deterministic: the same
// options and components, in the same order, produce byte-identical JSON —
// sort components before calling (FromLock does this using the lock's own
// package order).
func Build(opts Options, components []Component) (*Document, error) {
	if opts.Timestamp == "" {
		return nil, fmt.Errorf("sbom: Options.Timestamp is required")
	}
	if opts.BundleID == "" {
		return nil, fmt.Errorf("sbom: Options.BundleID is required")
	}
	// The bundle identity is generated, not operator-supplied, but it lands in
	// the same document as the package metadata below and gets the same check
	// rather than a comment explaining why it is exempt.
	for _, f := range [][2]string{
		{"Options.BundleID", opts.BundleID},
		{"Options.BundleVersion", opts.BundleVersion},
		{"Options.Timestamp", opts.Timestamp},
		{"Options.SerialNumber", opts.SerialNumber},
		{"Options.ToolName", opts.ToolName},
		{"Options.ToolVersion", opts.ToolVersion},
	} {
		if err := checkDocumentText(f[0], f[1]); err != nil {
			return nil, fmt.Errorf("sbom: %w", err)
		}
	}

	serial := opts.SerialNumber
	if serial == "" {
		serial = deterministicSerialNumber(opts.BundleID)
	}

	doc := &Document{
		BOMFormat:    BOMFormat,
		SpecVersion:  SpecVersion,
		SerialNumber: serial,
		Version:      1,
		Metadata: &Metadata{
			Timestamp: opts.Timestamp,
			Component: &BOMComponent{
				Type:    RootComponentType,
				BOMRef:  "bundle:" + opts.BundleID,
				Name:    opts.BundleID,
				Version: opts.BundleVersion,
			},
		},
	}
	if opts.ToolName != "" || opts.ToolVersion != "" {
		name := opts.ToolName
		if name == "" {
			name = "debark"
		}
		doc.Metadata.Tools = &Tools{Components: []BOMComponent{{
			Type:    "application",
			Name:    name,
			Version: opts.ToolVersion,
		}}}
	}

	doc.Components = make([]BOMComponent, 0, len(components))
	// bom-ref must be unique within a BOM (CycloneDX definitions/refType), and
	// two components claiming one identity is precisely the misattribution an
	// SBOM exists to rule out. The map is a duplicate detector only; nothing
	// is ever iterated out of it, so it cannot affect output order.
	seenRef := make(map[string]int, len(components))
	for i, c := range components {
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("sbom: component %d (%q): %w", i, c.Name, err)
		}
		purl, err := PURL(c.Distro, c.Name, c.Version, c.Arch)
		if err != nil {
			return nil, fmt.Errorf("sbom: component %d: %w", i, err)
		}
		if prev, dup := seenRef[purl]; dup {
			return nil, fmt.Errorf("sbom: components %d and %d are both %s; a bom-ref must identify exactly one component", prev, i, purl)
		}
		seenRef[purl] = i
		bc := BOMComponent{
			Type:    ComponentType,
			BOMRef:  purl,
			Name:    c.Name,
			Version: c.Version,
			PURL:    purl,
		}
		if c.SHA256 != "" {
			bc.Hashes = []Hash{{Alg: HashAlgSHA256, Content: c.SHA256}}
		}
		if c.SourcePackage != "" {
			bc.Properties = []Property{{Name: SourcePackageProperty, Value: c.SourcePackage}}
		}
		doc.Components = append(doc.Components, bc)
	}
	return doc, nil
}

// validate rejects component metadata that would make the document lie about
// what is in the bundle. It is not a Debian Policy check -- apt and dpkg own
// that -- it is the narrower question of whether these bytes can be written
// into a CycloneDX document without changing their meaning.
//
// A .deb supplied with --file carries whatever its author put in its control
// stanza, and that text reaches here unfiltered, so this is untrusted input
// even though it usually comes from an archive index.
func (c Component) validate() error {
	if err := checkDocumentText("name", c.Name); err != nil {
		return err
	}
	if err := checkDocumentText("version", c.Version); err != nil {
		return err
	}
	if err := checkDocumentText("arch", c.Arch); err != nil {
		return err
	}
	if err := checkDocumentText("distro", c.Distro); err != nil {
		return err
	}
	if err := checkDocumentText("source package", c.SourcePackage); err != nil {
		return err
	}
	// An empty SHA256 means "the caller had no digest" and omits the hashes
	// block; a malformed one would be written out verbatim as a hash the
	// bundle does not actually have. CycloneDX's own hash-content pattern
	// rejects it too, so shipping it means the SBOM fails validation on the
	// far side of the gap, where nobody can fix it.
	if c.SHA256 != "" && !digest.Valid(c.SHA256) {
		return fmt.Errorf("sha256 %q is not a lowercase hex SHA-256", c.SHA256)
	}
	return nil
}

// checkDocumentText rejects text that JSON encoding would silently alter or
// that would let one document line impersonate another.
//
// Invalid UTF-8 is the one that actually changes identity: encoding/json
// replaces every bad byte with U+FFFD, so two packages whose names differ
// only in a byte that is not valid UTF-8 come out of the writer under one
// and the same name, while their purls stay distinct -- an SBOM naming a
// package that is not the one in the pool.
//
// Control characters cannot break the JSON structure, since the encoder
// escapes them, but they let a rendered SBOM line carry a newline or an
// ANSI/bidi sequence and read as something it is not. No Debian package
// name, version, architecture or source name may contain one.
func checkDocumentText(field, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return fmt.Errorf("%s contains the control character %U", field, r)
		}
	}
	return nil
}

// FromLockOption sets one of the values FromLock cannot read off the lock
// itself. It exists so this package can stay pure — see the package doc: no
// clock, no filesystem, no link-time globals — while still letting the
// caller state facts about the run that only the caller knows.
type FromLockOption func(*Options)

// WithToolVersion records ver as the version of debark in metadata.tools.
//
// It is an option rather than something FromLock works out for itself
// because the answer lives in core/version, whose value is set at link time
// and whose Get() reads the running executable off disk. Importing that here
// would make this package's output depend on how the binary was linked, and
// would break both the purity this package documents and the golden-file
// test that purity makes possible. The caller that already holds the build
// identity (core/engine, which computes version.Tool() for the manifest)
// passes it in, so the SBOM and the manifest beside it necessarily agree.
//
// Without this option metadata.tools still names debark, but with no
// version — an SBOM that cannot say which debark produced it.
func WithToolVersion(ver string) FromLockOption {
	return func(o *Options) { o.ToolVersion = ver }
}

// FromLock builds a Document directly from a resolved lock: one component per
// lock.Package, sorted the same way the lock itself is (name, arch, version —
// resolve.SortSelections' order, which is how lock.Packages is always
// written), plus the bundle as the root component.
//
// Pass WithToolVersion so metadata.tools says which debark produced the
// document; the lock carries no such field (lock.Resolver records apt and
// dpkg, not debark), so it cannot be derived here.
func FromLock(l *lock.Lock, bundleID, timestamp string, opts ...FromLockOption) (*Document, error) {
	if l == nil {
		return nil, fmt.Errorf("sbom: FromLock: nil lock")
	}
	components := make([]Component, 0, len(l.Packages))
	for _, p := range l.Packages {
		components = append(components, Component{
			Name:          p.Name,
			Version:       p.Version,
			Arch:          p.Arch,
			Distro:        l.Target.DistroID,
			SHA256:        p.SHA256,
			SourcePackage: p.SourcePackage,
		})
	}
	// lock.Packages is documented as "sorted by (name, arch)"; re-sort
	// defensively so Document is deterministic even if a caller hands
	// FromLock an unsorted or hand-built Lock (as tests do).
	sort.SliceStable(components, func(i, j int) bool {
		if components[i].Name != components[j].Name {
			return components[i].Name < components[j].Name
		}
		if components[i].Arch != components[j].Arch {
			return components[i].Arch < components[j].Arch
		}
		return components[i].Version < components[j].Version
	})
	o := Options{
		BundleID:  bundleID,
		Timestamp: timestamp,
		ToolName:  "debark",
	}
	for _, opt := range opts {
		opt(&o)
	}
	return Build(o, components)
}

// JSON renders d as indented JSON with a trailing newline, matching
// canonical.MarshalIndent's convention for human-facing, schema-validated
// bundle files (this file is hashed as bundle bytes, not JCS-canonicalised —
// see the Document doc comment).
func (d *Document) JSON() ([]byte, error) {
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("sbom: marshal: %w", err)
	}
	return append(out, '\n'), nil
}

// deterministicSerialNumber derives an RFC 4122-shaped (version 5, variant 1)
// UUID from seed by hashing it, so the same bundle id always yields the same
// serialNumber instead of debark needing a random source (which would break
// reproducibility — principle 2). This is not a general name-based UUID
// implementation (no namespace UUID input, unlike RFC 4122 UUIDv5 proper) —
// just a stable, spec-shaped identifier.
func deterministicSerialNumber(seed string) string {
	sum := sha256.Sum256([]byte("debark.sbom.serial/v1:" + seed))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("urn:uuid:%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
