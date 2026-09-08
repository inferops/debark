package evidence

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestKnownTypeCoversEveryDeclaredType reads types.go and requires that every
// Type* constant declared there is in the eventTypes set KnownType answers
// from.
//
// A hand-maintained set beside a hand-maintained const block drifts on the
// first addition, and the drift is silent in the direction that matters: a new
// event type would be emitted normally by everything in this repository and
// refused by the one reader that checks -- the container backend, reading a
// stream from another debark process -- so the symptom would be "the newest
// event type is the only one that never crosses a container boundary", which
// nobody would connect to a missing map entry.
//
// Parsing the source rather than reflecting: Go constants are not enumerable
// at run time, and the alternative (a []string the constants are defined from)
// would make the block itself less readable to buy the same property.
func TestKnownTypeCoversEveryDeclaredType(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "types.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing types.go: %v", err)
	}

	var declared []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if !strings.HasPrefix(name.Name, "Type") || i >= len(vs.Values) {
				continue
			}
			lit, ok := vs.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			declared = append(declared, v)
		}
		return true
	})

	if len(declared) == 0 {
		t.Fatal("found no Type* constants in types.go; this test is not looking where it thinks it is")
	}
	for _, typ := range declared {
		if !KnownType(typ) {
			t.Errorf("KnownType(%q) is false, but types.go declares it: add it to eventTypes", typ)
		}
	}
	if len(eventTypes) != len(declared) {
		t.Errorf("eventTypes has %d entries and types.go declares %d Type* constants; "+
			"an entry with no constant is as much a drift as a constant with no entry", len(eventTypes), len(declared))
	}

	// And the negative, so the test cannot pass by KnownType returning true
	// for everything.
	for _, typ := range []string{"", "apt.resolve.v2", "backend.selected ", "arbitrary.attacker.type"} {
		if KnownType(typ) {
			t.Errorf("KnownType(%q) is true, but no constant declares it", typ)
		}
	}
}
