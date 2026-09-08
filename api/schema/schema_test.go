// Package schema embeds and tests debark's published JSON Schemas (draft
// 2020-12), one per on-disk or wire document defined by the frozen Go types
// in core/ and api/. The schemas are the deliverable; this file is the proof
// that they match the Go types exactly, and stays that way as the types
// evolve.
//
// Validation is done by the real thing, github.com/santhosh-tekuri/jsonschema/v6
// (a genuine draft 2020-12 implementation), now that it is a go.mod
// dependency. Each schema is registered with the compiler under its own
// declared $id — the URL it is actually published at,
// https://debark.dev/schema/<name>/v1 (see TestSchemasAreValidJSON) — and
// compiled either as a whole document or, via a "#/$defs/Name"
// JSON-pointer fragment, as one named subschema standalone; see compile and
// mustValidate below.
//
// This replaces an earlier hand-rolled subset validator that a previous
// version of this file carried while the dependency was still missing from
// go.mod. That validator only implemented the keywords its author had
// noticed these schemas use, so it could pass a schema the real,
// spec-complete validator rejects — exactly the risk of hand-rolling
// validation for schemas published to external consumers. The
// reflection-based drift checker in the second half of this file is
// unrelated to which validator runs — it walks the raw schema JSON and Go
// struct tags directly — and is unchanged.
package schema

import (
	"bytes"
	"embed"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	buildjobv1 "github.com/inferops/debark/api/buildjob/v1"
	pluginv1 "github.com/inferops/debark/api/plugin/v1"
	"github.com/inferops/debark/core/base"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/doctor"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/install"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/manifest"
	"github.com/inferops/debark/core/policy"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/core/store"
	"github.com/inferops/debark/core/verify"
)

//go:embed *.schema.json
var schemaFiles embed.FS

// ---------------------------------------------------------------------
// Schema loading
// ---------------------------------------------------------------------

// schemaDoc is one parsed schema file: the root JSON object plus its
// filename, for error messages.
type schemaDoc struct {
	file string
	root map[string]any
}

func load(t *testing.T, file string) schemaDoc {
	t.Helper()
	b, err := schemaFiles.ReadFile(file)
	if err != nil {
		t.Fatalf("embed: read %s: %v", file, err)
	}
	var root map[string]any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("%s: parse schema: %v", file, err)
	}
	// Re-decode without UseNumber for the validator/drift checker, which
	// compare against plain float64 instances produced by ordinary
	// json.Unmarshal of test values. UseNumber above was only to catch
	// malformed JSON early with a precise decoder; the plain form is what
	// every helper below actually consumes.
	var plain map[string]any
	if err := json.Unmarshal(b, &plain); err != nil {
		t.Fatalf("%s: parse schema: %v", file, err)
	}
	return schemaDoc{file: file, root: plain}
}

// def returns the named entry from the schema's $defs table.
func (d schemaDoc) def(name string) map[string]any {
	defs, _ := d.root["$defs"].(map[string]any)
	n, _ := defs[name].(map[string]any)
	return n
}

// allSchemaFiles is every schema this package publishes, keyed by the short
// name used as a testdata/ fixture prefix.
var allSchemaFiles = map[string]string{
	"snapshot":      "snapshot.v1.schema.json",
	"lock":          "lock.v1.schema.json",
	"manifest":      "manifest.v1.schema.json",
	"signature":     "signature.v1.schema.json",
	"events":        "events.v1.schema.json",
	"buildjob":      "buildjob.v1.schema.json",
	"plugin":        "plugin.v1.schema.json",
	"verifyreport":  "verifyreport.v1.schema.json",
	"installreport": "installreport.v1.schema.json",
	"doctorreport":  "doctorreport.v1.schema.json",
	"policy":        "policy.v1.schema.json",
	"storeindex":    "storeindex.v1.schema.json",
	"baselist":      "baselist.v1.schema.json",
}

func TestSchemasAreValidJSON(t *testing.T) {
	for prefix, file := range allSchemaFiles {
		t.Run(prefix, func(t *testing.T) {
			d := load(t, file)
			if d.root["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
				t.Errorf("%s: missing or wrong $schema", file)
			}
			id, _ := d.root["$id"].(string)
			want := "https://debark.dev/schema/" + prefix + "/v1"
			if id != want {
				t.Errorf("%s: $id = %q, want %q", file, id, want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// $ref resolution and small schema-JSON helpers, shared by the real-library
// compilation below and by the reflection-based drift checker further down
// — the drift checker walks raw schema JSON directly, independently of
// whichever validator is compiling it.
// ---------------------------------------------------------------------

// resolve follows a local "#/$defs/Name" $ref to its target, if node is one.
// Sibling keywords next to $ref (e.g. a description) are valid in 2020-12
// but carry no validation meaning here, so they are simply not read.
func resolve(root map[string]any, node map[string]any) map[string]any {
	for i := 0; i < 32; i++ { // bounded: these schemas never chain refs deeply
		ref, ok := node["$ref"].(string)
		if !ok {
			return node
		}
		const prefix = "#/$defs/"
		if !strings.HasPrefix(ref, prefix) {
			return node
		}
		defs, _ := root["$defs"].(map[string]any)
		next, ok := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
		if !ok {
			return node
		}
		node = next
	}
	return node
}

func asStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// compiledSchemas caches one compiled *jsonschema.Schema per (file, ptr)
// pair. Tests in this package run sequentially (nothing here calls
// t.Parallel), so a plain map is enough; this exists only to avoid
// recompiling the same tiny schema dozens of times over a run of "go test
// -v ./...", not for correctness.
var compiledSchemas = map[string]*jsonschema.Schema{}

// compile compiles the schema in d, starting from ptr: "" for the whole
// document's root schema, or a "#/$defs/Name" JSON-pointer fragment to
// compile one named subschema standalone (which is exactly what draft
// 2020-12 $defs entries are for). The document is registered with a fresh
// compiler under its own declared $id — the URL it is actually published at
// (https://debark.dev/schema/<name>/v1; see TestSchemasAreValidJSON) — so
// compiling and validating here exercises the schema the same way an
// external consumer fetching it from that URL would.
//
// The schema bytes are decoded with jsonschema.UnmarshalJSON rather than
// encoding/json directly, because that is what preserves number precision
// (json.Number instead of float64) the way the library expects; d.root,
// decoded separately by load() with the standard library, is used only for
// its $id string here — the drift checker's own use of d.root is untouched.
func compile(t *testing.T, d schemaDoc, ptr string) *jsonschema.Schema {
	t.Helper()
	key := d.file + ptr
	if sch, ok := compiledSchemas[key]; ok {
		return sch
	}
	id, _ := d.root["$id"].(string)
	if id == "" {
		t.Fatalf("%s: schema has no $id to compile against", d.file)
	}
	b, err := schemaFiles.ReadFile(d.file)
	if err != nil {
		t.Fatalf("embed: read %s: %v", d.file, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: parse schema: %v", d.file, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(id, doc); err != nil {
		t.Fatalf("%s: register resource %s: %v", d.file, id, err)
	}
	sch, err := c.Compile(id + ptr)
	if err != nil {
		t.Fatalf("%s: compile %s%s: %v", d.file, id, ptr, err)
	}
	compiledSchemas[key] = sch
	return sch
}

// toInstance marshals v with the standard library encoder and decodes it
// back with jsonschema.UnmarshalJSON (map[string]any / []any / string /
// json.Number / bool / nil, number precision preserved), i.e. exactly what
// an external consumer validating the wire bytes would see. Schema
// validation is canonicalisation-independent, so the standard
// (non-canonical) encoder is used deliberately here.
func toInstance(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("unmarshal %T: %v", v, err)
	}
	return instance
}

// mustValidate compiles d's schema starting at ptr (see compile) and asserts
// that v, marshalled to JSON, validates against it.
func mustValidate(t *testing.T, d schemaDoc, ptr string, v any) {
	t.Helper()
	sch := compile(t, d, ptr)
	instance := toInstance(t, v)
	if err := sch.Validate(instance); err != nil {
		b, _ := json.MarshalIndent(v, "", "  ")
		t.Errorf("%s: expected %T value to validate against %s, got:\n%v\n\njson was:\n%s", d.file, v, sch.Location, err, b)
	}
}

// ---------------------------------------------------------------------
// Reflection-based drift checker.
//
// Walks a Go struct's json tags and asserts every one appears in the
// schema's properties (and vice versa), that required-ness agrees with
// `omitempty`, and recurses into nested structs, slices and typed maps. This
// is what stops the schemas rotting when someone edits a type.
// ---------------------------------------------------------------------

type drift struct {
	t    *testing.T
	file string
	root map[string]any
}

func jsonType(rt reflect.Type) string {
	switch rt.Kind() {
	case reflect.Struct, reflect.Map:
		return "object"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	default:
		return "" // interface{} / any: deliberately unconstrained
	}
}

func (d *drift) check(rt reflect.Type, node map[string]any, path string) {
	for rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	node = resolve(d.root, node)

	if want := jsonType(rt); want != "" {
		if got, ok := node["type"].(string); ok && got != want {
			d.t.Errorf("drift [%s]: %s at %s: schema type %q does not match Go kind %q", d.file, rt.String(), path, got, want)
		}
	}

	switch rt.Kind() {
	case reflect.Struct:
		d.checkStruct(rt, node, path)
	case reflect.Slice, reflect.Array:
		items, ok := node["items"].(map[string]any)
		if !ok {
			d.t.Errorf("drift [%s]: %s at %s: Go slice/array type has no schema \"items\" object", d.file, rt.String(), path)
			return
		}
		d.check(rt.Elem(), items, path+"[]")
	case reflect.Map:
		if rt.Elem().Kind() == reflect.Interface {
			return // free-form value type (map[string]any): nothing further to check
		}
		ap, ok := node["additionalProperties"].(map[string]any)
		if !ok {
			d.t.Errorf("drift [%s]: %s at %s: Go map type has no schema \"additionalProperties\" object", d.file, rt.String(), path)
			return
		}
		d.check(rt.Elem(), ap, path+".*")
	default:
		// string / bool / numeric / named-string / interface{}: leaf, nothing to recurse into.
	}
}

func (d *drift) checkStruct(rt reflect.Type, node map[string]any, path string) {
	props, _ := node["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	required := map[string]bool{}
	for _, r := range asStringSlice(node["required"]) {
		required[r] = true
	}

	seen := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		tag, ok := f.Tag.Lookup("json")
		if !ok {
			d.t.Errorf("drift [%s]: %s.%s at %s: struct field has no json tag", d.file, rt.Name(), f.Name, path)
			continue
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		omitempty := false
		for _, p := range parts[1:] {
			if p == "omitempty" {
				omitempty = true
			}
		}
		// encoding/json's isEmptyValue only treats Array/Map/Slice/String
		// length, Bool, numeric zero, and nil Ptr/Interface as "empty" - a
		// non-pointer struct is never considered empty, so `omitempty` on a
		// struct-typed (not *struct) field is silently inert: the field is
		// always emitted. Schemas must mark such a field required to match
		// actual wire behaviour (see api/buildjob/v1.Output.Sign, documented
		// in docs/formats.md §5), so the required-ness check below must
		// judge the tag's real effect, not its literal text.
		effectiveOmitempty := omitempty && f.Type.Kind() != reflect.Struct

		seen[name] = true
		subAny, exists := props[name]
		sub, subOK := subAny.(map[string]any)
		if !exists || !subOK {
			d.t.Errorf("drift [%s]: Go field %s.%s (json %q) at %s has no matching schema property", d.file, rt.Name(), f.Name, name, path)
			continue
		}
		switch {
		case !effectiveOmitempty && !required[name]:
			d.t.Errorf("drift [%s]: Go field %s.%s (json %q) at %s has no effective `omitempty`, so the schema must list it as required, but it does not", d.file, rt.Name(), f.Name, name, path)
		case effectiveOmitempty && required[name]:
			d.t.Errorf("drift [%s]: Go field %s.%s (json %q) at %s has effective `omitempty`, so the schema must not list it as required, but it does", d.file, rt.Name(), f.Name, name, path)
		}
		d.check(f.Type, sub, path+"."+name)
	}
	for name := range props {
		if !seen[name] {
			d.t.Errorf("drift [%s]: schema property %q at %s has no matching field on Go type %s", d.file, name, path, rt.Name())
		}
	}
}

func mustNoDrift(t *testing.T, d schemaDoc, node map[string]any, v any) {
	t.Helper()
	(&drift{t: t, file: d.file, root: d.root}).check(reflect.TypeOf(v), node, "$")
}

// ---------------------------------------------------------------------
// Property coverage: does any document this package validates actually
// *carry* each property?
//
// The drift checker above walks Go types, not values, so it can only ever say
// that a struct field and a schema property agree about existence. Every
// optional field is `omitempty`, so a property can be added to the schema,
// added to the Go type, pass drift, and never once appear in a document that
// is put in front of the compiled validator - which means its type, its
// pattern and its place in the object are asserted by nobody. origin's
// assumed_installed arrived exactly that way: schema, Go type and drift check
// all in agreement over a field no fixture set.
//
// This is applied to the snapshot and base-listing documents, and that is a
// judgement rather than an oversight. "Every property is populated somewhere"
// is true of fullSnapshot and fullBaseList by design and false, on purpose, of
// several other values here: fullVerifyReport reports OK, so it must carry no
// problems, and fullLock uses the local backend, so it must carry neither
// image nor image_digest (TestResolverImageDigestPattern covers those
// separately). Asserting it tree-wide would force those values to describe
// states that cannot occur.
//
// A base listing is the easy case and is held to the rule for that reason:
// every property of it is unconditionally derivable from a Definition, so
// there is no state a real listing can be in that the fixture cannot also be
// in. A property nobody can populate is a property nobody should have added.
// ---------------------------------------------------------------------

// coverage walks a schema and an instance of it side by side, collecting the
// properties the schema defines and the subset the instance sets.
//
// Paths are named from the schema's own structure, and a $ref restarts the
// path at the $defs entry it names, so the many places a File appears
// collapse into one slot: File.mode is "exercised" once any file in the
// document sets it, which is the honest reading of what a fixture proves.
type coverage struct {
	root    map[string]any
	defined map[string]bool
	present map[string]bool
}

// defineFrom registers every property reachable from node. It is deliberately
// separate from the instance walk: a property nested under an absent parent
// must still count as unexercised, and an instance walk alone would never
// reach it to say so. seen guards against a $defs entry that refers to itself
// (none does today) and stops the same entry being re-walked.
func (c *coverage) defineFrom(node map[string]any, path string, seen map[string]bool) {
	if ref, ok := node["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
		if seen[ref] {
			return
		}
		seen[ref] = true
		path = ref
	}
	node = resolve(c.root, node)
	if props, ok := node["properties"].(map[string]any); ok {
		for name, sub := range props {
			subNode, ok := sub.(map[string]any)
			if !ok {
				continue
			}
			c.defined[path+"."+name] = true
			c.defineFrom(subNode, path+"."+name, seen)
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		c.defineFrom(items, path+"[]", seen)
	}
	if ap, ok := node["additionalProperties"].(map[string]any); ok {
		c.defineFrom(ap, path+".*", seen)
	}
}

func (c *coverage) walk(node map[string]any, instance any, path string) {
	if ref, ok := node["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
		path = ref
	}
	node = resolve(c.root, node)
	if props, ok := node["properties"].(map[string]any); ok {
		obj, _ := instance.(map[string]any)
		for name, sub := range props {
			subNode, ok := sub.(map[string]any)
			if !ok {
				continue
			}
			v, ok := obj[name]
			if !ok {
				continue
			}
			c.present[path+"."+name] = true
			c.walk(subNode, v, path+"."+name)
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		arr, _ := instance.([]any)
		for _, el := range arr {
			c.walk(items, el, path+"[]")
		}
	}
	if ap, ok := node["additionalProperties"].(map[string]any); ok {
		obj, _ := instance.(map[string]any)
		for _, v := range obj {
			c.walk(ap, v, path+".*")
		}
	}
}

// mustCoverEveryProperty asserts that v, marshalled, sets every property the
// schema at node defines.
func mustCoverEveryProperty(t *testing.T, d schemaDoc, node map[string]any, v any) {
	t.Helper()
	c := &coverage{root: d.root, defined: map[string]bool{}, present: map[string]bool{}}
	c.defineFrom(node, "$", map[string]bool{})
	c.walk(node, toInstance(t, v), "$")

	var missing []string
	for p := range c.defined {
		if !c.present[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	for _, p := range missing {
		t.Errorf("coverage [%s]: schema property %s is set by no %T fixture, so nothing validates it", d.file, p, v)
	}
}

// ---------------------------------------------------------------------
// Shared test fixture values. Digests below are real SHA-256 sums of short
// literal strings (computed once with sha256sum, not hand-typed), so every
// pattern check below exercises the real regexp, not a guessed constant.
// ---------------------------------------------------------------------

const (
	digSnapshotA    = "94a1a6bd87442a401b30634b46577a677f31a4ad066a943fb646ff6899054df0"
	digSnapshotB    = "93b01c58ab034ef261f31762c5246f5ddebb1c7c3db60cde6e6956487b6ff066"
	digLockA        = "80be0631f286fe7e12719ab399568f969c31d98f4bf6321666f58cc15e21ba4f"
	digRequestA     = "80d51bb829a6e379a6f43309aa6b28d206ff39953816b748267b16bed58be497"
	digManifestA    = "b6fd458a7205924b78c0b1bcc6730fc69ee72e093dd49f56e106479057f9441d"
	digRepoPackages = "4a7c25efce77741075c75312ff07c5a6033ad5d5f6b5705ba4aad8284091456a"
	digRepoRelease  = "48c6dc7efba94dc886451717a6d7077271646ac6be4b90e561d6ce86daf87645"
	digLockB        = "5ca54ae935ebe6a895ac8f31606e4db256628778d35f562ff44ee0aba658c349"
	digBinaryA      = "353797a12e4fbdb499f56f11b2b24ab86179b605b65b259358c00aed8b53541d"
	digInstallA     = "0b160a1edbc6429c7cedf0faa0c4360c3f09eba20ac9ef1f9c0522119d3e4d0b"
	digStoreA       = "19c29e0fbf72ec4bf6d83c5bf7f54efb97921f94480e70af8d1c94b49b7ce4b4"
	digAptOutputA   = "ea08eeadaaf778f1c36d5b880e677b8277fddaff3704cde3ea029b85fbd9a34f"
	digBaseA        = "ee693260532f32e2be17cae1b5e81d8c47f8c888a2f9cfd28d5a291aab467e62"
	digBaseB        = "9d58c2c95545778e059c39e3c212fc8d0cf0e04293da986680c42fdb8d9f9e5f"

	fingerprintA = "CAE265BE4F5710A1C6714384E04F6BB6DC1AE916"
	keyIDA       = "E04F6BB6DC1AE916"
	fingerprintB = "75C83A893514F80FF134A58C6E5385BD3174138D"

	sigBase64A    = "ZGViZmVycnktdGVzdC1zaWduYXR1cmUtYnl0ZXMtMDE="
	sigBase64B    = "ZGViZmVycnktdGVzdC1zaWduYXR1cmUtYnl0ZXMtMDItcGx1Z2lu"
	pubKeyBase64A = "ZGViZmVycnktdGVzdC1wdWJsaWMta2V5LWJ5dGVz"
	payloadA      = "ZGViZmVycnktdGVzdC1jYW5vbmljYWwtcGF5bG9hZA=="

	imageDigestA = "sha256:d1b7bb6aa2134003463e30ed0872d6746e275d4a3128315af7fbd5d61e05f311"
)

// ---------------------------------------------------------------------
// Fully-populated Go values, one constructor per document/message shape.
// ---------------------------------------------------------------------

func fullSnapshot() snapshot.Snapshot {
	return snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		CreatedAt:     "2026-09-03T12:00:00Z",
		Tool:          snapshot.Tool{Name: "debark", Version: "1.0.0", Edition: manifest.EditionCommunity},
		// Synthesized here so this fully-populated value exercises every
		// Origin field; the captured shape (kind only) is covered by
		// testdata/valid-snapshot.json.
		Origin: snapshot.Origin{
			Kind:         snapshot.OriginSynthesized,
			BaseID:       "debian:12/minimal",
			Source:       snapshot.OriginSourceBuiltin,
			SourceDigest: digSnapshotB,
			// Three names, not a real closure. A base's assumed set runs to a
			// couple of thousand entries, and none of the ones after the third
			// would exercise anything the third does not: what this value has
			// to prove is that the property appears in a document the
			// published schema is actually run against. The shape is the
			// contract though - name:arch, sorted - because that is what the
			// target compares against its own dpkg status.
			AssumedInstalled: []string{"base-files:amd64", "bash:amd64", "libc6:amd64"},
		},
		Target: snapshot.Target{
			DistroID: "debian", VersionID: "12", Codename: "bookworm",
			PrettyName:   "Debian GNU/Linux 12 (bookworm)",
			Arch:         "amd64",
			ForeignArchs: []string{"i386"},
			APTVersion:   "2.6.1",
			DpkgVersion:  "1.21.22",
			MachineID:    "0123456789abcdef0123456789abcdef",
			OSRelease: &snapshot.File{
				Path: "/etc/os-release", ArchivePath: "etc/os-release",
				Size: 385, SHA256: digSnapshotA, Mode: "0644",
			},
		},
		APT: snapshot.APT{
			Sources: []snapshot.File{{
				Path: "/etc/apt/sources.list", ArchivePath: "etc/apt/sources.list",
				Size: 1200, SHA256: digSnapshotB, Mode: "0644",
			}},
			Preferences: []snapshot.File{{
				Path: "/etc/apt/preferences", ArchivePath: "etc/apt/preferences",
				Size: 42, SHA256: digLockA, Mode: "0644",
			}},
			Conf: []snapshot.File{{
				Path: "/etc/apt/apt.conf.d/99recommends", ArchivePath: "etc/apt/apt.conf.d/99recommends",
				Size: 33, SHA256: digRequestA, Mode: "0644",
				// The only File here with redacted set, and set on an
				// apt.conf file because that is the shape redaction really
				// produces: Redactions below says "proxies" was applied, and
				// redactProxies marks exactly the apt.conf members whose bytes
				// it rewrote. Until this was set, no document validated in
				// this package carried the property at all.
				Redacted: true,
			}},
			Trusted: []snapshot.File{{
				Path:        "/etc/apt/trusted.gpg.d/debian-archive-bookworm-stable.gpg",
				ArchivePath: "etc/apt/trusted.gpg.d/debian-archive-bookworm-stable.gpg",
				Size:        2400, SHA256: digManifestA,
			}},
			Keyrings: []snapshot.File{{
				Path:        "/usr/share/keyrings/debian-archive-keyring.gpg",
				ArchivePath: "usr/share/keyrings/debian-archive-keyring.gpg",
				Size:        5200, SHA256: digRepoPackages,
			}},
		},
		DpkgStatus: snapshot.File{
			Path: "/var/lib/dpkg/status", ArchivePath: "var/lib/dpkg/status",
			Size: 892000, SHA256: digRepoRelease, Mode: "0644",
		},
		InstalledCount: 742,
		KeyringFingerprints: []snapshot.KeyFingerprint{{
			Fingerprint: fingerprintA, KeyID: keyIDA,
			UserIDs:  []string{"Debian Archive Automatic Signing Key (12/bookworm) <ftpmaster@debian.org>"},
			Keyring:  "etc/apt/trusted.gpg.d/debian-archive-bookworm-stable.gpg",
			SignedBy: []string{"etc/apt/sources.list.d/debian.sources"},
		}},
		Labels:     map[string]string{"site": "plant-4", "ticket": "CHG-00123"},
		Redactions: []string{snapshot.RedactProxies},
		Warnings:   []string{"preferences.d/50-local: unreadable, skipped"},
	}
}

func TestSnapshotSchema(t *testing.T) {
	d := load(t, "snapshot.v1.schema.json")
	v := fullSnapshot()
	t.Run("validates", func(t *testing.T) { mustValidate(t, d, "", v) })
	t.Run("drift", func(t *testing.T) { mustNoDrift(t, d, d.root, v) })
	t.Run("coverage", func(t *testing.T) { mustCoverEveryProperty(t, d, d.root, v) })
}

// TestSnapshotSchemaConstrainsOriginByKind holds the two validators a
// snapshot passes through to the same answer about origin.
//
// It replaced a test that pinned the gap between them: $defs/Origin used to
// carry no if/then, so the published schema accepted a CAPTURED origin
// declaring base_id, source, source_digest and - the one that matters -
// assumed_installed, and only core/snapshot.Validate refused it. That gap had
// teeth. A captured snapshot's installed set is the inventory of one real
// machine, and assumed_installed is the single part of this document that
// travels onward inside a bundle (docs/formats.md section 2), so a captured
// origin carrying it is precisely the leak D8 exists to prevent - and an
// external consumer validating snapshot.json against the published URL and
// nothing else would have seen a valid document.
//
// Both directions are asserted, because the point is that they agree: a
// change that relaxes either one alone fails here.
func TestSnapshotSchemaConstrainsOriginByKind(t *testing.T) {
	d := load(t, "snapshot.v1.schema.json")
	v := fullSnapshot()
	v.Origin.Kind = snapshot.OriginCaptured // everything else left in place

	if err := compile(t, d, "").Validate(toInstance(t, v)); err == nil {
		t.Error("the published schema accepted a captured origin carrying synthesized-only fields, " +
			"including assumed_installed: a real machine's installed set would reach a bundle " +
			"and pass validation for anyone not running the Go validator")
	}
	if err := snapshot.Validate(&v); err == nil {
		t.Error("core/snapshot.Validate accepted a captured origin carrying assumed_installed")
	}

	// And the positive direction, so the rule cannot be satisfied by a schema
	// that simply rejects everything.
	ok := fullSnapshot()
	if err := compile(t, d, "").Validate(toInstance(t, ok)); err != nil {
		t.Errorf("the published schema rejected a well-formed synthesized origin: %v", err)
	}
}

func fullLock() lock.Lock {
	return lock.Lock{
		SchemaVersion:  lock.SchemaVersion,
		CreatedAt:      "2026-09-03T12:05:00Z",
		SnapshotDigest: digSnapshotA,
		RequestDigest:  digRequestA,
		Target: lock.Target{
			DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64",
			ForeignArchs: []string{"i386"}, APTVersion: "2.6.1", DpkgVersion: "1.21.22",
		},
		Resolver: lock.Resolver{
			Backend: lock.BackendLocal, APTVersion: "2.6.1", DpkgVersion: "1.21.22",
			APTOptions:        []string{"APT::Sandbox::User=root", "Debug::NoLocking=1"},
			PhasedUpdates:     string(snapshot.PhasedTargetMachineID),
			InstallRecommends: true,
		},
		Packages: []lock.Package{
			{
				Name: "vlc", Arch: "amd64", Version: "3.0.21-1build1", SourcePackage: "vlc",
				Filename: "pool/v/vlc/vlc_3.0.21-1build1_amd64.deb", Size: 1957888, SHA256: digManifestA,
				Origin: lock.Origin{
					URI: "http://deb.debian.org/debian", Suite: "bookworm", Component: "main",
					ReleaseDigest: digRepoRelease, KeyFingerprint: fingerprintA,
				},
				Reason:                lock.ReasonRequested,
				PublisherVerification: lock.VerifiedAPTSigned,
			},
			{
				Name: "libvlccore9", Arch: "amd64", Version: "3.0.21-1build1", SourcePackage: "vlc",
				Filename: "pool/libv/libvlccore9/libvlccore9_3.0.21-1build1_amd64.deb", Size: 462200, SHA256: digRepoPackages,
				Origin: lock.Origin{
					URI: "http://deb.debian.org/debian", Suite: "bookworm", Component: "main",
					ReleaseDigest: digRepoRelease, KeyFingerprint: fingerprintA,
				},
				Reason:                lock.ReasonDependencyOfPrefix + "vlc",
				PublisherVerification: lock.VerifiedAPTSigned,
				Essential:             true,
			},
			{
				Name: "acme-agent", Arch: "amd64", Version: "2.1.0",
				Filename: "pool/a/acme-agent/acme-agent_2.1.0_amd64.deb", Size: 88000, SHA256: digLockB,
				Origin:                lock.Origin{URI: "https://vendor.example.com/acme-agent_2.1.0_amd64.deb"},
				Reason:                lock.ReasonExternal,
				PublisherVerification: lock.VerifiedUserDigest,
				Flags:                 []string{lock.FlagNetworkPostinst},
			},
		},
		Install: []string{"acme-agent=2.1.0", "vlc=3.0.21-1build1"},
		ClosedWorld: lock.ClosedWorld{
			Result: lock.ClosedWorldOK, CommandDigest: digStoreA, OutputDigest: digAptOutputA,
		},
		Warnings: []lock.Warning{
			{Code: "redistribution.multiverse", Message: "1 package carries redistribution terms.", Packages: []string{"acme-agent"}},
		},
		Unresolved: []lock.Unresolved{
			{Input: "no-such-pkg", Kind: "package", Detail: "Unable to locate package no-such-pkg", Missing: []string{"no-such-pkg"}},
		},
		Stats: lock.Stats{Added: 3, Removed: 0, Unchanged: 0, Bytes: 2508088, DownloadedBytes: 2508088},
	}
}

func TestLockSchema(t *testing.T) {
	d := load(t, "lock.v1.schema.json")
	v := fullLock()
	t.Run("validates", func(t *testing.T) { mustValidate(t, d, "", v) })
	t.Run("drift", func(t *testing.T) { mustNoDrift(t, d, d.root, v) })
}

func fullManifest() manifest.Manifest {
	return manifest.Manifest{
		SchemaVersion:  manifest.SchemaVersion,
		BundleID:       "bd-" + digLockA[:16],
		CreatedAt:      "2026-09-03T12:10:00Z",
		FormatVersion:  manifest.CurrentFormatVersion,
		Tool:           manifest.Tool{Name: "debark", Version: "1.0.0", BuildDigest: digBinaryA, Edition: manifest.EditionOfficial},
		SnapshotDigest: digSnapshotA,
		LockDigest:     digLockA,
		Repository: manifest.Repository{
			PackagesSHA256: digRepoPackages, PackagesGzSHA256: digRepoRelease,
			ReleaseSHA256: digManifestA, InReleaseSHA256: digLockB, ReleaseGPGSHA256: digRequestA,
			PackageCount: 3, PoolBytes: 2508088,
		},
		Files: []manifest.File{
			{Path: "lock.json", Size: 4096, SHA256: digLockA},
			{Path: "snapshot.json", Size: 8192, SHA256: digSnapshotA},
			{Path: "repo/Packages", Size: 12000, SHA256: digRepoPackages},
			{Path: "repo/Release", Size: 900, SHA256: digManifestA},
			{Path: "repo/pool/v/vlc/vlc_3.0.21-1build1_amd64.deb", Size: 1957888, SHA256: digInstallA},
		},
		Target:      manifest.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		SBOMRef:     "sbom.cdx.json",
		EvidenceRef: "evidence.json",
	}
}

func TestManifestSchema(t *testing.T) {
	d := load(t, "manifest.v1.schema.json")
	v := fullManifest()
	t.Run("validates", func(t *testing.T) { mustValidate(t, d, "", v) })
	t.Run("drift", func(t *testing.T) { mustNoDrift(t, d, d.root, v) })
}

func fullSignatureFile() manifest.SignatureFile {
	return manifest.SignatureFile{
		Schema:         manifest.SignatureSchemaVersion,
		ManifestSHA256: digManifestA,
		Signatures: []manifest.Signature{
			{
				SignerKind: manifest.SignerEd25519File, KeyID: "dGVzdC1rZXktaWQ", Algorithm: "ed25519",
				CreatedAt: "2026-09-03T12:11:00Z", Signature: sigBase64A, Comment: "release engineering key, 2026",
			},
			{
				SignerKind: manifest.SignerPluginPrefix + "acme-hsm", KeyID: fingerprintB, Algorithm: "ecdsa-p256-sha256",
				CreatedAt: "2026-09-03T12:11:05Z", Signature: sigBase64B,
			},
		},
	}
}

func TestSignatureSchema(t *testing.T) {
	d := load(t, "signature.v1.schema.json")
	v := fullSignatureFile()
	t.Run("validates", func(t *testing.T) { mustValidate(t, d, "", v) })
	t.Run("drift", func(t *testing.T) { mustNoDrift(t, d, d.root, v) })
}

func fullEvidenceDocument() evidence.Document {
	return evidence.Document{
		Schema:    evidence.SchemaVersion,
		CreatedAt: "2026-09-03T12:12:00Z",
		Context: map[string]any{
			"tool_version": "1.0.0", "edition": "community", "backend": "local", "build_host": "linux/amd64",
		},
		Events: []evidence.Event{
			{
				Schema: evidence.SchemaVersion, TS: "2026-09-03T12:00:01Z", Type: evidence.TypeSnapshotLoaded,
				Level: evidence.LevelInfo, Msg: "snapshot loaded", Attrs: map[string]any{"snapshot_digest": digSnapshotA},
			},
			{
				Schema: evidence.SchemaVersion, TS: "2026-09-03T12:00:05Z", Type: evidence.TypeFetchFile,
				Level: evidence.LevelInfo, Msg: "fetched vlc_3.0.21-1build1_amd64.deb",
				Attrs: map[string]any{"bytes": float64(1957888), "uri": "http://deb.debian.org/debian/pool/v/vlc/vlc_3.0.21-1build1_amd64.deb"},
			},
			{
				Schema: evidence.SchemaVersion, TS: "2026-09-03T12:00:09Z", Type: evidence.TypeWarning,
				Level: evidence.LevelWarn, Msg: "external package acme-agent is url-unverified",
			},
		},
	}
}

func TestEventsSchema(t *testing.T) {
	d := load(t, "events.v1.schema.json")
	v := fullEvidenceDocument()
	t.Run("document validates", func(t *testing.T) { mustValidate(t, d, "", v) })
	t.Run("document drift", func(t *testing.T) { mustNoDrift(t, d, d.root, v) })
	t.Run("event validates standalone", func(t *testing.T) { mustValidate(t, d, "#/$defs/Event", v.Events[0]) })
	t.Run("event drift", func(t *testing.T) { mustNoDrift(t, d, d.def("Event"), v.Events[0]) })
}

func fullBuildRequest() buildjobv1.BuildRequest {
	recommends := true
	closedWorld := true
	return buildjobv1.BuildRequest{
		SchemaVersion: buildjobv1.SchemaVersion,
		SnapshotRef:   "/var/lib/debark/snapshots/plant4.snapshot.tar.zst",
		Inputs: buildjobv1.Inputs{
			Packages:  []string{"vlc", "acme-agent=2.1.0"},
			URLs:      []buildjobv1.URLInput{{URL: "https://vendor.example.com/acme-agent_2.1.0_amd64.deb", SHA256: digInstallA}},
			Files:     []string{"./local-debs/zoom.deb"},
			LocalDirs: []string{"./local-debs"},
			ListFiles: []string{"packages.txt"},
		},
		Options: buildjobv1.Options{
			Recommends:                &recommends,
			Upgrades:                  true,
			UpdateMode:                buildjobv1.UpdateRefresh,
			Prune:                     true,
			ArchOverride:              "arm64",
			MirrorOverrides:           map[string]string{"http://deb.debian.org/debian": "http://mirror.internal/debian"},
			Backend:                   "auto",
			Image:                     "docker.io/library/debian:bookworm-slim",
			PolicyRef:                 "policy.yaml",
			ApprovedKeysRef:           "approved-keys.txt",
			AcknowledgeRedistribution: true,
			EmbedBinary:               "bin/debark-linux-arm64",
			SBOM:                      true,
			StoreDir:                  "/var/lib/debark/store",
			ClosedWorldCheck:          &closedWorld,
		},
		Output: buildjobv1.Output{
			Path:   "/mnt/media/plant4-bundle",
			Format: buildjobv1.FormatDir,
			Sign:   buildjobv1.SignOptions{SignerRef: "/etc/debark/keys/org.key", Required: true, RepoSignerRef: "gpg:ABCDEF0123456789"},
		},
	}
}

func fullBuildResult() buildjobv1.BuildResult {
	return buildjobv1.BuildResult{
		SchemaVersion: buildjobv1.SchemaVersion,
		LockRef:       "lock.json",
		ManifestRef:   "debark.manifest.json",
		BundlePath:    "/mnt/media/plant4-bundle",
		BundleID:      "bd-" + digLockA[:16],
		Signed:        true,
		Stats: buildjobv1.Stats{
			Added: 3, Removed: 0, Unchanged: 1, Bytes: 2508088, DownloadedBytes: 2508088,
			PackageCount: 3, DurationSeconds: 47,
		},
		Warnings:    []string{"1 package carries redistribution terms."},
		Unresolved:  []string{"no-such-pkg"},
		FetchFailed: []string{"https://vendor.example.com/broken.deb"},
		FetchFailures: []buildjobv1.FetchFailure{{
			Input:  "https://vendor.example.com/broken.deb",
			Reason: buildjobv1.ReasonTLSUntrusted,
			Detail: "fetch: https://vendor.example.com/broken.deb: request failed: tls: failed to verify certificate: x509: certificate signed by unknown authority",
		}},
		ExitClass: buildjobv1.ExitIncomplete,
	}
}

func TestBuildjobSchema(t *testing.T) {
	d := load(t, "buildjob.v1.schema.json")
	req, res := fullBuildRequest(), fullBuildResult()
	t.Run("request validates via oneOf", func(t *testing.T) { mustValidate(t, d, "", req) })
	t.Run("request drift", func(t *testing.T) { mustNoDrift(t, d, d.def("BuildRequest"), req) })
	t.Run("result validates via oneOf", func(t *testing.T) { mustValidate(t, d, "", res) })
	t.Run("result drift", func(t *testing.T) { mustNoDrift(t, d, d.def("BuildResult"), res) })
}

func fullHandshake() pluginv1.Handshake {
	return pluginv1.Handshake{Protocol: pluginv1.Protocol, Name: "acme-hsm", Version: "1.4.0", Capabilities: []string{pluginv1.CapabilitySign}}
}

func fullSignParams() pluginv1.SignParams {
	return pluginv1.SignParams{Purpose: manifest.SignPurpose, Payload: payloadA, KeyRef: "acme-hsm:org-key-1"}
}

func fullSignRequest() pluginv1.Request {
	return pluginv1.Request{ID: "1", Method: pluginv1.MethodSign, Params: fullSignParams()}
}

func fullSignResult() pluginv1.SignResult {
	return pluginv1.SignResult{Signature: sigBase64B, Algorithm: "ecdsa-p256-sha256", KeyID: fingerprintB, SignerKind: manifest.SignerPluginPrefix + "acme-hsm"}
}

func fullSignResponse() pluginv1.Response {
	return pluginv1.Response{ID: "1", Result: fullSignResult()}
}

func fullErrorResponse() pluginv1.Response {
	return pluginv1.Response{ID: "2", Error: &pluginv1.Error{Code: pluginv1.ErrKeyUnavailable, Message: "HSM session expired"}}
}

func fullKeyInfoResult() pluginv1.KeyInfoResult {
	return pluginv1.KeyInfoResult{KeyID: fingerprintB, Algorithm: "ecdsa-p256-sha256", PublicKey: pubKeyBase64A, Comment: "org signing key"}
}

func TestPluginSchema(t *testing.T) {
	d := load(t, "plugin.v1.schema.json")
	hs, req, res, errRes := fullHandshake(), fullSignRequest(), fullSignResponse(), fullErrorResponse()

	t.Run("handshake validates via oneOf", func(t *testing.T) { mustValidate(t, d, "", hs) })
	t.Run("handshake drift", func(t *testing.T) { mustNoDrift(t, d, d.def("Handshake"), hs) })

	t.Run("request validates via oneOf", func(t *testing.T) { mustValidate(t, d, "", req) })
	t.Run("request drift", func(t *testing.T) { mustNoDrift(t, d, d.def("Request"), req) })

	t.Run("sign result response validates via oneOf", func(t *testing.T) { mustValidate(t, d, "", res) })
	t.Run("sign result response drift", func(t *testing.T) { mustNoDrift(t, d, d.def("Response"), res) })

	t.Run("error response validates via oneOf", func(t *testing.T) { mustValidate(t, d, "", errRes) })
	t.Run("error response drift", func(t *testing.T) { mustNoDrift(t, d, d.def("Response"), errRes) })

	t.Run("sign params drift", func(t *testing.T) { mustNoDrift(t, d, d.def("SignParams"), fullSignParams()) })
	t.Run("sign result drift", func(t *testing.T) { mustNoDrift(t, d, d.def("SignResult"), fullSignResult()) })
	t.Run("key info result drift", func(t *testing.T) { mustNoDrift(t, d, d.def("KeyInfoResult"), fullKeyInfoResult()) })
}

func fullVerifyReport() verify.Report {
	return verify.Report{
		SchemaVersion: verify.SchemaVersion,
		CheckedAt:     "2026-09-03T13:00:00Z",
		BundlePath:    "/mnt/media/plant4-bundle",
		BundleID:      "bd-" + digLockA[:16],
		OK:            true,
		Signed:        true,
		Signatures: []verify.SignatureResult{
			{SignerKind: manifest.SignerEd25519File, KeyID: "dGVzdC1rZXktaWQ", Algorithm: "ed25519", Valid: true, Trusted: true},
			{SignerKind: manifest.SignerGPG, KeyID: fingerprintA, Algorithm: "rsa4096", Valid: true, Trusted: false, Detail: "signature valid but key not in the operator's trust store"},
		},
		FilesChecked: 5,
		BytesChecked: 2508088,
		Warnings:     []string{"1 package carries redistribution terms."},
		Target:       verify.ReportTarget{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		CreatedAt:    "2026-09-03T12:10:00Z",
		ToolVersion:  "1.0.0",
		Edition:      manifest.EditionOfficial,
	}
}

func TestVerifyReportSchema(t *testing.T) {
	d := load(t, "verifyreport.v1.schema.json")
	v := fullVerifyReport()
	t.Run("validates", func(t *testing.T) { mustValidate(t, d, "", v) })
	t.Run("drift", func(t *testing.T) { mustNoDrift(t, d, d.root, v) })
}

func fullInstallReport() install.Report {
	vr := fullVerifyReport()
	return install.Report{
		SchemaVersion:  install.SchemaVersion,
		StartedAt:      "2026-09-03T14:00:00Z",
		FinishedAt:     "2026-09-03T14:00:07Z",
		BundlePath:     "/mnt/media/plant4-bundle",
		BundleID:       "bd-" + digLockA[:16],
		Verify:         &vr,
		Applied:        true,
		OK:             true,
		ToInstall:      []string{"vlc=3.0.21-1build1", "acme-agent=2.1.0"},
		ToUpgrade:      []string{"libc6=2.36-9+deb12u7"},
		AlreadyCurrent: 1,
		TargetExpected: lock.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		TargetActual:   lock.Target{DistroID: "debian", VersionID: "12", Codename: "bookworm", Arch: "amd64"},
		// Set even though this report is otherwise the ordinary captured case,
		// because a *Go* fixture that leaves it nil validates none of it: the
		// drift check compares types, so base_divergence could carry a wrong
		// pattern or a wrong nesting and nothing here would know. The
		// name:arch pattern in particular has no other Go-side exercise.
		BaseDivergence: &install.BaseDivergence{
			BaseID:  "debian:12/minimal",
			Assumed: 412,
			Missing: []string{"ifupdown:amd64", "nano:amd64"},
		},
		Warnings:        []string{"codename mismatch tolerated: bookworm vs bookworm"},
		AptOutputDigest: digAptOutputA,
	}
}

func TestInstallReportSchema(t *testing.T) {
	d := load(t, "installreport.v1.schema.json")
	v := fullInstallReport()
	t.Run("validates", func(t *testing.T) { mustValidate(t, d, "", v) })
	t.Run("drift", func(t *testing.T) { mustNoDrift(t, d, d.root, v) })
	// installreport.v1 keeps its own local copy of the verify report shape so
	// the file validates standalone (see schema description); prove that
	// copy is byte-for-byte the same shape as verify.Report itself.
	t.Run("embedded verify report drift", func(t *testing.T) { mustNoDrift(t, d, d.def("VerifyReport"), fullVerifyReport()) })
}

func fullDoctorReport() doctor.Report {
	return doctor.Report{
		SchemaVersion: doctor.SchemaVersion,
		CheckedAt:     "2026-09-03T12:20:00Z",
		Findings: []doctor.Finding{
			{
				Check: doctor.CheckNetworkPostinst, Severity: doctor.SeverityWarn, Package: "acme-agent", Version: "2.1.0",
				Message: "postinst script references curl", Evidence: "curl -fsSL https://acme.example.com/register | sh",
				Flag: lock.FlagNetworkPostinst,
			},
			{
				Check: doctor.CheckSnapShim, Severity: doctor.SeverityNote, Package: "chromium", Version: "128.0-1",
				Message: "chromium is a transitional snap shim on this release", Evidence: "Depends: chromium-browser (snap shim)",
				Flag: lock.FlagSnapShim,
			},
			{
				Check: doctor.CheckDKMSHeaders, Severity: doctor.SeverityWarn, Package: "nvidia-dkms", Version: "550.90.07-1",
				Message: "no matching linux-headers package found in the bundle", Evidence: "linux-headers-6.1.0-25-amd64",
				Flag: lock.FlagDKMS,
			},
		},
		Scanned: 4,
		Summary: map[string]int{"note": 1, "warn": 2},
	}
}

func TestDoctorReportSchema(t *testing.T) {
	d := load(t, "doctorreport.v1.schema.json")
	v := fullDoctorReport()
	t.Run("validates", func(t *testing.T) { mustValidate(t, d, "", v) })
	t.Run("drift", func(t *testing.T) { mustNoDrift(t, d, d.root, v) })
}

func fullPolicy() policy.Policy {
	allowURL := false
	return policy.Policy{
		SchemaVersion:          policy.SchemaVersion,
		AllowComponents:        []string{"main", "contrib"},
		DenyComponents:         []string{"non-free", "non-free-firmware"},
		AllowPackages:          []string{"*"},
		DenyPackages:           []string{"telnet", "rsh-*"},
		RequireSignedPublisher: true,
		AllowURLInputs:         &allowURL,
		ApprovedKeys:           []string{fingerprintA, fingerprintB},
		DenyFlags:              []string{lock.FlagDKMS},
		DefaultSeverity:        policy.SeverityWarn,
	}
}

func TestPolicySchema(t *testing.T) {
	d := load(t, "policy.v1.schema.json")
	v := fullPolicy()
	t.Run("validates", func(t *testing.T) { mustValidate(t, d, "", v) })
	t.Run("drift", func(t *testing.T) { mustNoDrift(t, d, d.root, v) })
}

func fullStoreIndex() store.Index {
	return store.Index{
		SchemaVersion: store.IndexSchemaVersion,
		Entries: []store.Entry{
			{Digest: digInstallA, Name: "vlc", Version: "3.0.21-1build1", Arch: "amd64", Size: 1957888,
				Filename: "vlc_3.0.21-1build1_amd64.deb", AddedAt: "2026-09-01T09:00:00Z"},
			{Digest: digLockB, Name: "acme-agent", Version: "2.1.0", Arch: "amd64", Size: 88000,
				Filename: "acme-agent_2.1.0_amd64.deb", AddedAt: "2026-09-03T12:00:00Z", UserSupplied: true},
		},
	}
}

func TestStoreIndexSchema(t *testing.T) {
	d := load(t, "storeindex.v1.schema.json")
	v := fullStoreIndex()
	t.Run("validates", func(t *testing.T) { mustValidate(t, d, "", v) })
	t.Run("drift", func(t *testing.T) { mustNoDrift(t, d, d.root, v) })
}

// fullBaseList is one `snapshot list-bases --json` document, and sets every
// property baselist.v1 defines - see TestBaseListSchema's coverage subtest for
// why that is achievable here and not everywhere.
//
// The digests are the shared literal constants above, not the real digests of
// these two builtin bases. Pinning those here would tie this file to the
// builtin table and rot on every measured change to it, and asserting them is
// core/base's job in any case: what this file tests is the published shape,
// and for that a value of the right form is exactly as good.
//
// The two entries differ on purpose: one excludes nothing, so the optional
// field's absence is exercised alongside its presence.
func fullBaseList() base.List {
	return base.List{
		SchemaVersion: base.ListSchemaVersion,
		Arch:          "amd64",
		Bases: []base.ListEntry{
			{
				ID:          "debian:13/minimal",
				Description: "Debian 13 (trixie) — a minimal install: the base system and apt, nothing chosen by an installer profile",
				DistroID:    "debian", VersionID: "13", Codename: "trixie", Variant: "minimal", Arch: "amd64",
				Seeds: []string{"apt", "init"}, Recommends: false,
				Digest: digBaseA,
			},
			{
				ID:          "ubuntu:26.04/desktop",
				Description: "Ubuntu 26.04 (resolute) — a desktop install: the minimal system plus the default desktop environment",
				DistroID:    "ubuntu", VersionID: "26.04", Codename: "resolute", Variant: "desktop", Arch: "amd64",
				Seeds: []string{"ubuntu-desktop-minimal"}, Excludes: []string{"lsb-base"}, Recommends: false,
				Digest: digBaseB,
			},
		},
	}
}

// baseListOmits names the Definition fields a listing deliberately does not
// show, with the reason, and is the whole content of the projection subtest
// below: anything not here must appear in the listing.
//
// The rule it encodes is stated on base.ListEntry - a listing carries
// everything that decides what the base CLAIMS, and nothing that is merely
// how it is configured. Excludes is the field that proved the rule needed
// writing down: it landed on Definition after the listing had been designed,
// it changes the claim, and nothing existing would have noticed it was
// missing from the published shape.
var baseListOmits = map[string]string{
	"schema_version": "the listing has a schema_version of its own; a per-entry one would version the wrong document",
	"sources":        "a whole deb822 document; an archive URI per row would bury the fields a person is scanning for",
	"keyrings":       "paths on the resolving machine, not a property of the baseline itself",
}

// TestBaseListSchema holds `snapshot list-bases --json` to a published schema
// like every other --json output (ADR-012).
//
// The Go types deliberately live in core/base rather than internal/cli, where
// they were first written. Two reasons, one of which is absolute: they were
// unexported, so no test outside that package could name them at all - and
// api/schema, which is published API, should not import the CLI even where
// the language would allow it (it does: internal/ here sits at the module
// root, so every package in the module may import it).
func TestBaseListSchema(t *testing.T) {
	d := load(t, "baselist.v1.schema.json")
	v := fullBaseList()
	t.Run("validates", func(t *testing.T) { mustValidate(t, d, "", v) })
	t.Run("drift", func(t *testing.T) { mustNoDrift(t, d, d.root, v) })
	t.Run("coverage", func(t *testing.T) { mustCoverEveryProperty(t, d, d.root, v) })

	// The drift and coverage checks above compare the listing to its own
	// schema, which says nothing about whether the listing still shows what a
	// base is. A projection has a second way to rot that a mirrored type does
	// not: the thing it projects FROM grows a field, both sides of the
	// contract stay perfectly consistent, and the published document quietly
	// stops answering the question it exists to answer. This is the only
	// check in this file that looks at a type the schema does not describe.
	t.Run("projection", func(t *testing.T) {
		shown := map[string]bool{}
		re := reflect.TypeOf(base.ListEntry{})
		for i := 0; i < re.NumField(); i++ {
			name := strings.Split(re.Field(i).Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				shown[name] = true
			}
		}
		rd := reflect.TypeOf(base.Definition{})
		for i := 0; i < rd.NumField(); i++ {
			f := rd.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			why, omitted := baseListOmits[name]
			switch {
			case shown[name] && omitted:
				t.Errorf("baselist projection: base.Definition.%s (%q) is listed in baseListOmits (%q) but the listing shows it anyway",
					f.Name, name, why)
			case !shown[name] && !omitted:
				t.Errorf("baselist projection: base.Definition.%s serialises as %q, which `snapshot list-bases --json` does not show. "+
					"Add it to base.ListEntry and baselist.v1.schema.json if it changes what the base claims, or to baseListOmits with the reason if it does not.",
					f.Name, name)
			}
		}
	})
}

// imageDigestSanityCheck exercises the "sha256:" + 64 hex char pattern used
// by lock.Resolver.ImageDigest, independent of the fixtures above.
func TestResolverImageDigestPattern(t *testing.T) {
	d := load(t, "lock.v1.schema.json")
	l := fullLock()
	l.Resolver.Backend = lock.BackendContainer
	l.Resolver.Image = "docker.io/library/debian:bookworm-slim"
	l.Resolver.ImageDigest = imageDigestA
	mustValidate(t, d, "", l)
}

// ---------------------------------------------------------------------
// Golden fixtures under testdata/.
// ---------------------------------------------------------------------

func fixturePrefix(name string) string {
	base := strings.TrimSuffix(name, ".json")
	base = strings.TrimPrefix(base, "valid-")
	base = strings.TrimPrefix(base, "invalid-")
	if i := strings.IndexByte(base, '-'); i >= 0 {
		return base[:i]
	}
	return base
}

func TestGoldenFixtures(t *testing.T) {
	docs := map[string]schemaDoc{}
	for prefix, file := range allSchemaFiles {
		docs[prefix] = load(t, file)
	}

	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("testdata: no fixtures found")
	}

	seenValid := map[string]bool{}
	seenInvalid := map[string]int{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		prefix := fixturePrefix(name)
		doc, ok := docs[prefix]
		if !ok {
			t.Errorf("testdata/%s: filename prefix %q does not match any known schema", name, prefix)
			continue
		}

		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			verr := compile(t, doc, "").Validate(instance)
			switch {
			case strings.HasPrefix(name, "valid-"):
				if verr != nil {
					t.Errorf("expected valid, got error:\n%v", verr)
				} else {
					seenValid[prefix] = true
				}
			case strings.HasPrefix(name, "invalid-"):
				if verr == nil {
					t.Error("expected validation to fail, but it passed")
				} else {
					seenInvalid[prefix]++
				}
			default:
				t.Error("fixture name must start with valid- or invalid-")
			}
		})
	}

	for prefix := range allSchemaFiles {
		if !seenValid[prefix] {
			t.Errorf("testdata: no passing valid-%s*.json fixture found", prefix)
		}
		if seenInvalid[prefix] < 2 {
			t.Errorf("testdata: need at least 2 failing invalid-%s*.json fixtures, found %d", prefix, seenInvalid[prefix])
		}
	}
}

// ---------------------------------------------------------------------
// Enum/const drift: the schema's enumerated values must be the exact set
// the Go constants define, for the fields the design calls out by name -
// backends, reasons, publisher verification, severities, problem kinds,
// event types.
// ---------------------------------------------------------------------

func enumOf(t *testing.T, node map[string]any) []string {
	t.Helper()
	raw, ok := node["enum"].([]any)
	if !ok {
		t.Fatalf("schema node has no \"enum\" array: %#v", node)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("enum value %v is not a string", v)
		}
		out = append(out, s)
	}
	return out
}

func assertStringSetEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s: enum mismatch\n  schema: %v\n  go:     %v", label, g, w)
	}
}

func TestEnumConsistency(t *testing.T) {
	t.Run("lock resolver backend excludes auto", func(t *testing.T) {
		d := load(t, "lock.v1.schema.json")
		got := enumOf(t, d.def("Resolver")["properties"].(map[string]any)["backend"].(map[string]any))
		assertStringSetEqual(t, "lock.v1 resolver.backend", got, []string{string(lock.BackendLocal), string(lock.BackendContainer)})
	})
	t.Run("buildjob options backend includes auto", func(t *testing.T) {
		d := load(t, "buildjob.v1.schema.json")
		got := enumOf(t, d.def("Options")["properties"].(map[string]any)["backend"].(map[string]any))
		assertStringSetEqual(t, "buildjob.v1 options.backend", got, []string{string(lock.BackendLocal), string(lock.BackendContainer), string(lock.BackendAuto)})
	})
	t.Run("publisher verification", func(t *testing.T) {
		d := load(t, "lock.v1.schema.json")
		got := enumOf(t, d.def("Package")["properties"].(map[string]any)["publisher_verification"].(map[string]any))
		want := []string{string(lock.VerifiedAPTSigned), string(lock.VerifiedURLUnverified), string(lock.VerifiedUserDigest), string(lock.VerifiedUserSignature)}
		assertStringSetEqual(t, "lock.v1 package.publisher_verification", got, want)
	})
	t.Run("lock package flags", func(t *testing.T) {
		d := load(t, "lock.v1.schema.json")
		got := enumOf(t, d.def("Package")["properties"].(map[string]any)["flags"].(map[string]any)["items"].(map[string]any))
		want := []string{
			lock.FlagMultiverse, lock.FlagRestricted, lock.FlagNonFree, lock.FlagNonFreeFirmware,
			lock.FlagNetworkPostinst, lock.FlagSnapShim, lock.FlagDKMS, lock.FlagUserSupplied,
		}
		assertStringSetEqual(t, "lock.v1 package.flags items", got, want)
	})
	t.Run("verify problem kinds", func(t *testing.T) {
		d := load(t, "verifyreport.v1.schema.json")
		got := enumOf(t, d.def("Problem")["properties"].(map[string]any)["kind"].(map[string]any))
		want := []string{
			verify.ProblemManifestMissing, verify.ProblemManifestMalformed, verify.ProblemSchemaUnknown,
			verify.ProblemSignatureMissing, verify.ProblemSignatureInvalid, verify.ProblemSignatureUntrusted,
			verify.ProblemManifestDigest, verify.ProblemFileMissing, verify.ProblemFileDigest,
			verify.ProblemFileSize, verify.ProblemFileUnexpected, verify.ProblemFileNotRegular,
			verify.ProblemRepoDigest, verify.ProblemLockDigest, verify.ProblemSnapshotDigest,
			verify.ProblemSameMediaKey,
		}
		assertStringSetEqual(t, "verifyreport.v1 problem.kind", got, want)

		// installreport.v1 carries its own copy of this enum (see
		// TestInstallReportSchema) so that file validates standalone. A copy
		// is only as good as the check that it is still a copy: hold it to
		// the same Go constants, or widening the set - as file-not-regular
		// did - can land in one file and not the other.
		di := load(t, "installreport.v1.schema.json")
		gotInstall := enumOf(t, di.def("VerifyProblem")["properties"].(map[string]any)["kind"].(map[string]any))
		assertStringSetEqual(t, "installreport.v1 verifyProblem.kind", gotInstall, want)
	})
	t.Run("doctor severities", func(t *testing.T) {
		d := load(t, "doctorreport.v1.schema.json")
		got := enumOf(t, d.def("Finding")["properties"].(map[string]any)["severity"].(map[string]any))
		assertStringSetEqual(t, "doctorreport.v1 finding.severity", got, []string{string(doctor.SeverityNote), string(doctor.SeverityWarn)})
	})
	t.Run("policy severities", func(t *testing.T) {
		d := load(t, "policy.v1.schema.json")
		got := enumOf(t, d.root["properties"].(map[string]any)["default_severity"].(map[string]any))
		want := []string{string(policy.SeverityInfo), string(policy.SeverityWarn), string(policy.SeverityDeny)}
		assertStringSetEqual(t, "policy.v1 default_severity", got, want)
	})
	t.Run("event types", func(t *testing.T) {
		d := load(t, "events.v1.schema.json")
		got := enumOf(t, d.def("Event")["properties"].(map[string]any)["type"].(map[string]any))
		want := []string{
			evidence.TypeSnapshotCreated, evidence.TypeSnapshotLoaded, evidence.TypeBackendSelected,
			evidence.TypeAPTUpdate, evidence.TypeAPTResolve, evidence.TypeFetchFile, evidence.TypeInputExternal,
			evidence.TypeStoreHit, evidence.TypePolicyFinding, evidence.TypeDoctorFinding, evidence.TypeRepoIndexed,
			evidence.TypeClosedWorld, evidence.TypeManifestSigned, evidence.TypeBundleAssembled, evidence.TypePruned,
			evidence.TypeVerifyResult, evidence.TypeInstallPlan, evidence.TypeInstallResult, evidence.TypeWarning,
			evidence.TypeBuildStarted, evidence.TypeBuildFinished, evidence.TypeProgress,
		}
		assertStringSetEqual(t, "events.v1 event.type", got, want)
	})
	t.Run("buildjob fetch-failure reason matches FetchFailureReasons", func(t *testing.T) {
		d := load(t, "buildjob.v1.schema.json")
		got := enumOf(t, d.def("FetchFailure")["properties"].(map[string]any)["reason"].(map[string]any))
		var want []string
		for _, r := range buildjobv1.FetchFailureReasons() {
			want = append(want, string(r))
		}
		assertStringSetEqual(t, "buildjob.v1 FetchFailure.reason", got, want)
	})
	t.Run("buildjob exit class matches dferr.Classes", func(t *testing.T) {
		d := load(t, "buildjob.v1.schema.json")
		got := enumOf(t, d.def("BuildResult")["properties"].(map[string]any)["exit_class"].(map[string]any))
		var want []string
		for _, c := range dferr.Classes() {
			want = append(want, c.String())
		}
		assertStringSetEqual(t, "buildjob.v1 BuildResult.exit_class", got, want)
	})
}
