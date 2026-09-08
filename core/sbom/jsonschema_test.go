package sbom

// A small draft-07 JSON Schema validator, scoped to exactly the keywords the
// published CycloneDX 1.6 schema uses (surveyed by grepping
// testdata/cyclonedx/bom-1.6.schema.json while writing this package): $ref
// (internal "#/..." and whole-external-document, e.g. "spdx.schema.json"),
// type, enum, required, properties, additionalProperties (boolean form only
// — the schema never uses the sub-schema form), items (single schema form),
// minItems, maxItems, uniqueItems, minLength, maxLength, pattern, minimum,
// maximum, oneOf, anyOf. "format" is parsed as a no-op (draft-07 leaves
// format as annotation-only unless a validator opts into enforcing it, and
// this one does not).
//
// This is deliberately not a general-purpose validator — no "not", "if/then/
// else", "patternProperties", "propertyNames", "contains", "multipleOf" or
// "$dynamicRef", because the schema this test validates against never uses
// them (see that work report for the keyword-frequency survey that
// justified the cut). It exists only so cyclonedx_schema_test.go can check
// this package's own output against the real, published schema without a new
// dependency and without a network call — it is test-only code, excluded
// from the production binary.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

type schemaValidator struct {
	docs map[string]any // "" -> root document; "spdx.schema.json" etc -> external documents, keyed exactly as $ref spells them
}

func loadSchemaValidator(t *testing.T, dir string) *schemaValidator {
	t.Helper()
	root := loadJSONDoc(t, dir+"/bom-1.6.schema.json")
	spdx := loadJSONDoc(t, dir+"/spdx.schema.json")
	jsf := loadJSONDoc(t, dir+"/jsf-0.82.schema.json")
	return &schemaValidator{docs: map[string]any{
		"":                     root,
		"spdx.schema.json":     spdx,
		"jsf-0.82.schema.json": jsf,
	}}
}

func loadJSONDoc(t *testing.T, path string) any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return v
}

// Validate checks instance (already decoded via encoding/json into
// any/map[string]any/[]any/...) against the root schema and returns every
// violation found, each prefixed with a JSON-Pointer-ish path to where it
// occurred.
func (v *schemaValidator) Validate(instance any) []string {
	return v.validate(v.docs[""], instance, "$")
}

func (v *schemaValidator) validate(schema, instance any, path string) []string {
	if b, ok := schema.(bool); ok {
		if b {
			return nil
		}
		return []string{path + ": schema is `false`, no value is valid here"}
	}
	sm, ok := schema.(map[string]any)
	if !ok {
		return nil
	}

	if ref, ok := sm["$ref"].(string); ok {
		resolved, err := v.resolveRef(ref)
		if err != nil {
			return []string{fmt.Sprintf("%s: %v", path, err)}
		}
		return v.validate(resolved, instance, path)
	}

	var errs []string

	if t, ok := sm["type"]; ok && !matchesType(t, instance) {
		return append(errs, fmt.Sprintf("%s: type mismatch: schema wants %v, value is %s", path, t, jsonTypeName(instance)))
	}
	if enumVals, ok := sm["enum"].([]any); ok && !jsonMemberOf(enumVals, instance) {
		errs = append(errs, fmt.Sprintf("%s: %v is not one of the allowed enum values", path, instance))
	}
	if pat, ok := sm["pattern"].(string); ok {
		if s, ok := instance.(string); ok {
			if re, err := regexp.Compile(pat); err == nil && !re.MatchString(s) {
				errs = append(errs, fmt.Sprintf("%s: %q does not match pattern %q", path, s, pat))
			}
		}
	}
	if s, ok := instance.(string); ok {
		if ml, ok := sm["minLength"].(float64); ok && float64(len([]rune(s))) < ml {
			errs = append(errs, fmt.Sprintf("%s: string shorter than minLength %v", path, ml))
		}
		if ml, ok := sm["maxLength"].(float64); ok && float64(len([]rune(s))) > ml {
			errs = append(errs, fmt.Sprintf("%s: string longer than maxLength %v", path, ml))
		}
	}
	if n, ok := instance.(float64); ok {
		if mn, ok := sm["minimum"].(float64); ok && n < mn {
			errs = append(errs, fmt.Sprintf("%s: %v is less than minimum %v", path, n, mn))
		}
		if mx, ok := sm["maximum"].(float64); ok && n > mx {
			errs = append(errs, fmt.Sprintf("%s: %v is greater than maximum %v", path, n, mx))
		}
	}

	switch inst := instance.(type) {
	case map[string]any:
		if reqs, ok := sm["required"].([]any); ok {
			for _, r := range reqs {
				key, _ := r.(string)
				if _, present := inst[key]; !present {
					errs = append(errs, fmt.Sprintf("%s: missing required field %q", path, key))
				}
			}
		}
		propsSchema, _ := sm["properties"].(map[string]any)
		addlAllowed := true
		if b, ok := sm["additionalProperties"].(bool); ok {
			addlAllowed = b
		}
		keys := make([]string, 0, len(inst))
		for k := range inst {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := propsSchema[k]; ok {
				errs = append(errs, v.validate(ps, inst[k], path+"/"+k)...)
			} else if !addlAllowed {
				errs = append(errs, fmt.Sprintf("%s: additional property %q is not allowed by this schema", path, k))
			}
		}
	case []any:
		if mi, ok := sm["minItems"].(float64); ok && float64(len(inst)) < mi {
			errs = append(errs, fmt.Sprintf("%s: array has fewer than minItems=%v elements", path, mi))
		}
		if mi, ok := sm["maxItems"].(float64); ok && float64(len(inst)) > mi {
			errs = append(errs, fmt.Sprintf("%s: array has more than maxItems=%v elements", path, mi))
		}
		if u, ok := sm["uniqueItems"].(bool); ok && u && !jsonAllUnique(inst) {
			errs = append(errs, fmt.Sprintf("%s: array elements are not all unique", path))
		}
		if itemSchema, ok := sm["items"]; ok {
			for i, el := range inst {
				errs = append(errs, v.validate(itemSchema, el, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	}

	if branches, ok := sm["oneOf"].([]any); ok {
		matched := 0
		for _, b := range branches {
			if len(v.validate(b, instance, path)) == 0 {
				matched++
			}
		}
		if matched != 1 {
			errs = append(errs, fmt.Sprintf("%s: oneOf matched %d of %d branches, want exactly 1", path, matched, len(branches)))
		}
	}
	if branches, ok := sm["anyOf"].([]any); ok {
		matched := false
		for _, b := range branches {
			if len(v.validate(b, instance, path)) == 0 {
				matched = true
				break
			}
		}
		if !matched {
			errs = append(errs, fmt.Sprintf("%s: anyOf matched none of %d branches", path, len(branches)))
		}
	}

	return errs
}

// resolveRef resolves a $ref string against the loaded document set: a bare
// fragment ("#/definitions/component") resolves within the root document; a
// bare filename ("spdx.schema.json") resolves to that whole external
// document; this schema never combines the two (no "file.json#/frag" refs
// appear in it), so that combination is not implemented.
func (v *schemaValidator) resolveRef(ref string) (any, error) {
	if strings.HasPrefix(ref, "#") {
		return jsonPointer(v.docs[""], strings.TrimPrefix(ref, "#"))
	}
	doc, ok := v.docs[ref]
	if !ok {
		return nil, fmt.Errorf("unresolved $ref %q (not preloaded)", ref)
	}
	return doc, nil
}

func jsonPointer(doc any, ptr string) (any, error) {
	if ptr == "" || ptr == "/" {
		return doc, nil
	}
	cur := doc
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		tok = strings.ReplaceAll(tok, "~1", "/")
		tok = strings.ReplaceAll(tok, "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("json pointer %q: not an object at %q", ptr, tok)
		}
		next, ok := m[tok]
		if !ok {
			return nil, fmt.Errorf("json pointer %q: no such key %q", ptr, tok)
		}
		cur = next
	}
	return cur, nil
}

func matchesType(t, instance any) bool {
	var types []string
	switch tt := t.(type) {
	case string:
		types = []string{tt}
	case []any:
		for _, x := range tt {
			if s, ok := x.(string); ok {
				types = append(types, s)
			}
		}
	}
	for _, ty := range types {
		switch ty {
		case "object":
			if _, ok := instance.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := instance.([]any); ok {
				return true
			}
		case "string":
			if _, ok := instance.(string); ok {
				return true
			}
		case "boolean":
			if _, ok := instance.(bool); ok {
				return true
			}
		case "null":
			if instance == nil {
				return true
			}
		case "number":
			if _, ok := instance.(float64); ok {
				return true
			}
		case "integer":
			if f, ok := instance.(float64); ok && f == math.Trunc(f) {
				return true
			}
		}
	}
	return false
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func jsonMemberOf(vals []any, instance any) bool {
	for _, v := range vals {
		if reflect.DeepEqual(v, instance) {
			return true
		}
	}
	return false
}

func jsonAllUnique(items []any) bool {
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if reflect.DeepEqual(items[i], items[j]) {
				return false
			}
		}
	}
	return true
}
