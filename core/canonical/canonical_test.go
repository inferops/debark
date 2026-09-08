package canonical

import (
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

// manifestish mirrors the shape of the documents this package actually
// serialises - a manifest's files[] with a path and a size, and a target
// identity - so the collision tests below run against the real field layout
// rather than a bare string.
type manifestish struct {
	SchemaVersion string    `json:"schema_version"`
	Target        targetish `json:"target"`
	Files         []fileish `json:"files"`
	Labels        labelsish `json:"labels,omitempty"`
	// Ignored and unexported are never encoded. They are here so a test can
	// prove the walk mirrors encoding/json rather than blaming a field that
	// never reaches the output.
	Ignored    string `json:"-"`
	unexported string
}

type targetish struct {
	Codename string `json:"codename"`
}

type fileish struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type labelsish map[string]string

type inner struct {
	S string `json:"s"`
}

type outer struct {
	Inner inner `json:"inner"`
}

func manifestWith(path1, path2, codename string) manifestish {
	return manifestish{
		SchemaVersion: "debark.manifest/v1",
		Target:        targetish{Codename: codename},
		Files: []fileish{
			{Path: path1, Size: 1, SHA256: strings.Repeat("a", 64)},
			{Path: path2, Size: 2, SHA256: strings.Repeat("b", 64)},
		},
	}
}

// TestMarshalRefusesInvalidUTF8Collision is the regression test for the
// non-injectivity that made this package unable to keep its own promise:
// json.Marshal runs before JCS and replaces every undecodable byte with
// U+FFFD, so four different byte strings became two identical documents.
//
// The two inputs are genuinely different - four distinct paths and, in the
// second case, two distinct codenames - which is the point. Comparing a
// document to itself would prove nothing at all.
func TestMarshalRefusesInvalidUTF8Collision(t *testing.T) {
	cases := []struct {
		name string
		a, b manifestish
	}{
		{
			// The confirmed case: "bad\xffname"/"bad\xfename" against
			// "bad\xfdname"/"bad\xfcname". Every one of the four is a legal
			// ext4 filename, and all four encode to "bad�name".
			name: "files[].path",
			a:    manifestWith("bad\xffname", "bad\xfename", "noble"),
			b:    manifestWith("bad\xfdname", "bad\xfcname", "noble"),
		},
		{
			name: "target.codename",
			a:    manifestWith("a", "b", "nobl\xffe"),
			b:    manifestWith("a", "b", "nobl\xfee"),
		},
		{
			name: "map value",
			a:    manifestish{SchemaVersion: "v1", Labels: labelsish{"site": "h\xffq"}},
			b:    manifestish{SchemaVersion: "v1", Labels: labelsish{"site": "h\xfeq"}},
		},
		{
			name: "map key",
			a:    manifestish{SchemaVersion: "v1", Labels: labelsish{"s\xffte": "hq"}},
			b:    manifestish{SchemaVersion: "v1", Labels: labelsish{"s\xfete": "hq"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Guard against a vacuous test: the two inputs must really differ.
			ja, _ := json.Marshal(rawStringsOf(tc.a))
			jb, _ := json.Marshal(rawStringsOf(tc.b))
			if string(ja) == string(jb) {
				t.Fatalf("test is vacuous: both inputs hold the same bytes %s", ja)
			}

			da, errA := Digest(tc.a)
			db, errB := Digest(tc.b)
			if errA == nil || errB == nil {
				t.Fatalf("Digest accepted a document with invalid UTF-8: %q(err=%v) / %q(err=%v); "+
					"if it accepts both it must at least not collide (collided=%v)",
					da, errA, db, errB, da == db)
			}
			for _, err := range []error{errA, errB} {
				if got := dferr.ClassOf(err); got != dferr.Usage {
					t.Errorf("error class = %v, want %v (%v)", got, dferr.Usage, err)
				}
				if !strings.Contains(err.Error(), "not valid UTF-8") {
					t.Errorf("error does not say what is wrong: %v", err)
				}
			}
			// The field name has to reach the operator, or the refusal is
			// unactionable on a manifest with thousands of paths.
			if !strings.Contains(errA.Error(), "canonical: ") {
				t.Errorf("error is not attributed to this package: %v", errA)
			}
		})
	}
}

// rawStringsOf lists a document's strings as hex byte sequences, so the
// vacuity guard above compares the actual input bytes. It cannot go through
// json or fmt %q: both render an undecodable byte as U+FFFD, which is exactly
// the lossy step under test, and the guard would then pass on two inputs that
// really were identical.
func rawStringsOf(m manifestish) []string {
	strs := []string{m.SchemaVersion, m.Target.Codename}
	for _, f := range m.Files {
		strs = append(strs, f.Path, f.SHA256)
	}
	keys := make([]string, 0, len(m.Labels))
	for k := range m.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		strs = append(strs, "key="+k, "val="+m.Labels[k])
	}
	out := make([]string, len(strs))
	for i, s := range strs {
		out[i] = hex.EncodeToString([]byte(s))
	}
	return out
}

// TestMarshalAcceptsGenuineReplacementCharacter pins the other half of the
// rule. Refusing the undecodable bytes is exactly what leaves U+FFFD with a
// single preimage, so a document that really does contain one is still
// representable and must keep working - and must keep its existing digest,
// because rejecting it would break every bundle that already carries one.
func TestMarshalAcceptsGenuineReplacementCharacter(t *testing.T) {
	a := manifestWith("lit�eral", "b", "noble")
	b := manifestWith("lit�eral", "c", "noble")

	da, err := Digest(a)
	if err != nil {
		t.Fatalf("Digest refused a document whose U+FFFD is genuine: %v", err)
	}
	db, err := Digest(b)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if da == db {
		t.Errorf("two different documents share digest %s", da)
	}
}

// TestUnrepresentableStringsAreRefusedNotEncoded checks the values that are
// legitimate and must keep working, so the refusal cannot creep outwards into
// ordinary text.
func TestUnrepresentableStringsAreRefusedNotEncoded(t *testing.T) {
	accepted := []struct {
		name string
		in   any
	}{
		{"ascii", map[string]string{"a": "plain"}},
		{"non-ascii utf-8", map[string]string{"a": "café — ünïcodé ✓"}},
		{"astral plane", map[string]string{"a": "\U0001F680 rocket"}},
		{"literal U+FFFD", map[string]string{"a": "x�y"}},
		{"control characters", map[string]string{"a": "tab\there\nnewline\x00nul\x1b[1m"}},
		{"empty", map[string]string{}},
		{"byte slice (base64)", map[string][]byte{"a": {0xff, 0xfe, 0xfd}}},
		{"nil pointer", struct {
			P *string `json:"p,omitempty"`
		}{}},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			if _, err := Digest(tc.in); err != nil {
				t.Errorf("Digest refused a representable document: %v", err)
			}
		})
	}

	refused := []struct {
		name string
		in   any
	}{
		{"bare string", "bad\xffname"},
		{"slice element", []string{"ok", "bad\xffname"}},
		{"nested struct", outer{Inner: inner{S: "bad\xffname"}}},
		{"pointer target", &inner{S: "bad\xffname"}},
		{"interface value", map[string]any{"a": "bad\xffname"}},
		{"pointer inside a slice", []*inner{{S: "ok"}, {S: "bad\xffname"}}},
	}
	for _, tc := range refused {
		t.Run("refuse/"+tc.name, func(t *testing.T) {
			if _, err := Digest(tc.in); err == nil {
				t.Error("Digest accepted a string that is not valid UTF-8")
			} else if dferr.ClassOf(err) != dferr.Usage {
				t.Errorf("error class = %v, want %v", dferr.ClassOf(err), dferr.Usage)
			}
		})
	}
}

// TestSkippedFieldsAreNotBlamed makes sure the walk mirrors what
// encoding/json encodes. A json:"-" or unexported field never reaches the
// output, so a bad string in one must not turn a representable document into
// a refused one.
func TestSkippedFieldsAreNotBlamed(t *testing.T) {
	m := manifestWith("ok", "fine", "noble")
	m.Ignored = "bad\xffname"
	m.unexported = "bad\xfename"
	if _, err := Digest(m); err != nil {
		t.Errorf("Digest refused a document whose only bad strings are never encoded: %v", err)
	}
}

// TestMarshalRefusesIntegerCollision is the regression test for the second
// non-injectivity: RFC 8785 renders numbers as IEEE 754 doubles, so two
// different int64 values canonicalise identically above 2^53.
func TestMarshalRefusesIntegerCollision(t *testing.T) {
	a := manifestish{SchemaVersion: "v1", Files: []fileish{{Path: "p", Size: 9007199254740993, SHA256: "x"}}}
	b := manifestish{SchemaVersion: "v1", Files: []fileish{{Path: "p", Size: 9007199254740992, SHA256: "x"}}}
	if a.Files[0].Size == b.Files[0].Size {
		t.Fatal("test is vacuous: both sizes are the same int64")
	}

	da, errA := Digest(a)
	db, errB := Digest(b)
	if errA == nil && errB == nil && da == db {
		t.Fatalf("two different int64 sizes share digest %s", da)
	}
	for i, err := range []error{errA, errB} {
		if err == nil {
			t.Errorf("Digest accepted size %d; it and its neighbour canonicalise to the same bytes",
				[]int64{a.Files[0].Size, b.Files[0].Size}[i])
			continue
		}
		if dferr.ClassOf(err) != dferr.Usage {
			t.Errorf("error class = %v, want %v (%v)", dferr.ClassOf(err), dferr.Usage, err)
		}
	}
	_, _ = da, db
}

// TestAcceptedIntegersAreInjective is the property the fix exists to restore,
// asserted directly: no two distinct integers this package accepts may share
// a digest. It is the non-vacuous form of the test above - it compares many
// genuinely different values against each other rather than one hand-picked
// pair - and it is what would catch a future loosening of the bound.
func TestAcceptedIntegersAreInjective(t *testing.T) {
	values := []int64{
		0, 1, -1, 42, 1 << 20, 1 << 40, 12_000_000_000,
		MaxSafeInteger - 2, MaxSafeInteger - 1, MaxSafeInteger,
		-MaxSafeInteger, -MaxSafeInteger + 1,
		MaxSafeInteger + 1, MaxSafeInteger + 2, MaxSafeInteger + 3,
		1 << 60, 1<<63 - 1, -1 << 63,
	}
	seen := make(map[string]int64, len(values))
	accepted := 0
	for _, v := range values {
		d, err := Digest(map[string]int64{"size": v})
		if err != nil {
			continue
		}
		accepted++
		if prev, dup := seen[d]; dup {
			t.Errorf("%d and %d are different int64 values sharing digest %s", prev, v, d)
		}
		seen[d] = v
	}
	if accepted < 10 {
		t.Fatalf("only %d of %d values were accepted; the bound has become so strict "+
			"that this test no longer proves anything", accepted, len(values))
	}
}

// TestMarshalRefusesUnreadableInteger covers the worse half of the same
// defect: MaxInt64 does not merely collide, it canonicalises to a literal no
// int64 can parse back, so a signed manifest would carry a size the target
// cannot read.
func TestMarshalRefusesUnreadableInteger(t *testing.T) {
	v := map[string]int64{"size": 9223372036854775807}
	b, err := Marshal(v)
	if err == nil {
		var back map[string]int64
		uerr := json.Unmarshal(b, &back)
		t.Fatalf("Marshal accepted MaxInt64 and produced %s (parses back into int64: %v)", b, uerr == nil)
	}
	if dferr.ClassOf(err) != dferr.Usage {
		t.Errorf("error class = %v, want %v", dferr.ClassOf(err), dferr.Usage)
	}
	if !strings.Contains(err.Error(), "9223372036854775807") {
		t.Errorf("error does not name the offending value: %v", err)
	}
}

// TestIntegerBoundary pins the edge of the accepted range in both directions.
func TestIntegerBoundary(t *testing.T) {
	cases := []struct {
		name   string
		v      int64
		accept bool
	}{
		{"zero", 0, true},
		{"one", 1, true},
		{"max safe", MaxSafeInteger, true},
		{"min safe", -MaxSafeInteger, true},
		{"max safe + 1", MaxSafeInteger + 1, false},
		{"min safe - 1", -MaxSafeInteger - 1, false},
		{"typical pool size", 12_000_000_000, true},
		{"max int64", 1<<63 - 1, false},
		{"min int64", -1 << 63, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Marshal(map[string]int64{"n": tc.v})
			if tc.accept && err != nil {
				t.Errorf("Marshal(%d) = %v, want accepted", tc.v, err)
			}
			if !tc.accept && err == nil {
				t.Errorf("Marshal(%d) accepted; it does not survive canonicalisation", tc.v)
			}
		})
	}
}

// TestFractionalNumbersAreLeftToTheSpec pins where the integer rule stops, so
// that the boundary reads as a decision rather than an oversight.
//
// RFC 8785 *defines* a number with a fraction or an exponent as the binary64
// nearest to it, so "1.0" and "1" are the same JSON number by the standard and
// there is no identity there to lose. The rule only covers integer literals,
// which are the only numbers debark writes: every one comes from an int or
// an int64 field, and Go's encoder renders those exactly. A document that
// reaches Transform with "9007199254740993.0" in a size field is refused a
// step later by the loader, whose int64 field will not take a fractional
// literal at all.
func TestFractionalNumbersAreLeftToTheSpec(t *testing.T) {
	for _, in := range []string{
		`{"a":1.5}`, `{"a":0.1}`, `{"a":1e21}`, `{"a":-0.0}`, `{"a":1.0}`,
		`{"a":1e-7}`, `{"a":9007199254740993.0}`,
	} {
		if _, err := Transform([]byte(in)); err != nil {
			t.Errorf("Transform(%s) = %v, want accepted", in, err)
		}
	}
	// The loader is what refuses the fractional form, and it must keep doing
	// so, or the integer rule would have a way around it.
	var into struct {
		A int64 `json:"a"`
	}
	if err := json.Unmarshal([]byte(`{"a":9007199254740993.0}`), &into); err == nil {
		t.Errorf("a fractional literal parsed into an int64 field as %d; "+
			"the integer rule can then be sidestepped by writing the same value with a decimal point", into.A)
	}
}

// TestTransformChecksBytesFromDisk covers the path verify actually takes:
// manifest.Load canonicalises the bytes read off the bundle, never a
// re-marshalled struct, so the same rules have to hold for JSON this process
// did not write.
func TestTransformChecksBytesFromDisk(t *testing.T) {
	t.Run("integer above the safe range", func(t *testing.T) {
		// Two different on-disk manifests, differing only in the last digit
		// of a size. Before the check they canonicalised to the same bytes,
		// so editing one into the other left the digest verify compares
		// against the signature exactly where it was.
		hi := []byte(`{"files":[{"path":"p","size":9007199254740993}]}`)
		lo := []byte(`{"files":[{"path":"p","size":9007199254740992}]}`)
		a, errA := Transform(hi)
		b, errB := Transform(lo)
		if errA == nil && errB == nil && string(a) == string(b) {
			t.Fatalf("two different on-disk manifests canonicalise identically: %s", a)
		}
		if errA == nil {
			t.Error("Transform accepted an on-disk size that canonicalisation changes")
		}
		if errB == nil {
			t.Error("Transform accepted the size that value collides with")
		}
		// The largest size a real bundle could carry must still go through.
		if _, err := Transform([]byte(`{"files":[{"path":"p","size":9007199254740991}]}`)); err != nil {
			t.Errorf("Transform refused a size within the exactly representable range: %v", err)
		}
	})

	t.Run("invalid UTF-8 in the file", func(t *testing.T) {
		// jcs copies undecodable bytes through untouched, so without this
		// check Transform would hand back "canonical" bytes that are not
		// valid JSON text and that no re-marshalling could reproduce.
		raw := []byte("{\"path\":\"bad\xffname\"}")
		out, err := Transform(raw)
		if err == nil {
			t.Fatalf("Transform accepted invalid UTF-8 and returned %q", out)
		}
		if dferr.ClassOf(err) != dferr.Usage {
			t.Errorf("error class = %v, want %v", dferr.ClassOf(err), dferr.Usage)
		}
	})
}

// TestTransformStillRefusesDuplicateKeys is the property that must survive
// this change: JCS refusing duplicate keys is what closes the classic
// "canonicaliser and JSON parser disagree" attack, where the two pick
// different values for the same key and the signature covers the one the
// verifier did not read. The new checks run before jcs.Transform, so this
// asserts they do not shadow it.
func TestTransformStillRefusesDuplicateKeys(t *testing.T) {
	for _, in := range []string{
		`{"a":1,"a":2}`,
		`{"o":{"a":1,"a":2}}`,
		`{"a":"x","a":"y"}`,
		`[{"a":1,"a":2}]`,
	} {
		out, err := Transform([]byte(in))
		if err == nil {
			t.Errorf("Transform(%s) accepted duplicate keys and returned %q", in, out)
			continue
		}
		if !strings.Contains(err.Error(), "Duplicate key") {
			t.Errorf("Transform(%s) failed for the wrong reason: %v", in, err)
		}
	}
}

// TestCanonicalDigestsAreUnchanged pins the bytes and digests of documents
// that were already valid. Every one of these was produced by this package
// before the representability checks existed; if any moves, every signature
// debark has ever written is invalid.
func TestCanonicalDigestsAreUnchanged(t *testing.T) {
	cases := []struct {
		name       string
		in         any
		wantBytes  string
		wantDigest string
	}{
		{
			"key sorting",
			map[string]any{"b": 1, "a": "x"},
			`{"a":"x","b":1}`,
			"cdab067e9f3beb32d1252cfd63e492592fecbf591b0d08cadb24bb17f3864246",
		},
		{
			"safe integers",
			map[string]any{"n": 9007199254740991, "neg": -9007199254740991},
			`{"n":9007199254740991,"neg":-9007199254740991}`,
			"a83f5599a74ecc0cb2845491b128ee7a547ba5c131d11926289474792a97917e",
		},
		{
			"unicode, escapes and a genuine U+FFFD",
			map[string]any{"unicode": "café — ünïcodé ✓", "esc": "tab\there\nnewline", "fffd": "lit�eral"},
			"{\"esc\":\"tab\\there\\nnewline\",\"fffd\":\"lit�eral\",\"unicode\":\"café — ünïcodé ✓\"}",
			"e28acf8da6ccf33c2e68b1a278e42eeee5761bebe91e19ac4e02d2e4d2db6af9",
		},
		{
			"array order is preserved",
			[]string{"z", "a", "m"},
			`["z","a","m"]`,
			"4cb43e0105d5006bb4e80147e822141c0c0b49385c7c49e9702835a39c4c0535",
		},
		{
			"floats",
			map[string]any{"f": 1.5, "g": 0.1, "h": 1e21, "i": 0},
			`{"f":1.5,"g":0.1,"h":1e+21,"i":0}`,
			"3a14f89172142f41d2cb6fd4916840d99ea72b84ed853357cc04e4801d8cc427",
		},
		{
			"empty object",
			struct{}{},
			`{}`,
			"44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
		},
		{
			"empty array",
			[]int{},
			`[]`,
			"4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(b) != tc.wantBytes {
				t.Errorf("canonical bytes changed:\n got %q\nwant %q", b, tc.wantBytes)
			}
			d, err := Digest(tc.in)
			if err != nil {
				t.Fatalf("Digest: %v", err)
			}
			if d != tc.wantDigest {
				t.Errorf("digest changed: got %s, want %s", d, tc.wantDigest)
			}
		})
	}
}

// TestMarshalIndentAppliesTheSameRules: manifest.Save writes the manifest
// through MarshalIndent alone, so a document the canonical form would refuse
// must not reach disk looking well-formed.
func TestMarshalIndentAppliesTheSameRules(t *testing.T) {
	if _, err := MarshalIndent(map[string]string{"path": "bad\xffname"}); err == nil {
		t.Error("MarshalIndent wrote a document whose canonical form would be refused")
	}
	if _, err := MarshalIndent(map[string]int64{"size": 1 << 60}); err == nil {
		t.Error("MarshalIndent wrote an integer canonicalisation would change")
	}
	out, err := MarshalIndent(map[string]string{"path": "ok"})
	if err != nil {
		t.Fatalf("MarshalIndent refused a valid document: %v", err)
	}
	if string(out) != "{\n  \"path\": \"ok\"\n}\n" {
		t.Errorf("MarshalIndent output changed: %q", out)
	}
}

// TestMarshalStillRejectsUnencodableValues: the UTF-8 walk runs only after
// json.Marshal has succeeded, which is what keeps it a plain recursion. A
// value json cannot encode at all must still fail the way it always did,
// rather than sending the walk into a cycle.
func TestMarshalStillRejectsUnencodableValues(t *testing.T) {
	type node struct {
		Name string `json:"name"`
		Next *node  `json:"next,omitempty"`
	}
	n := &node{Name: "a"}
	n.Next = n
	if _, err := Marshal(n); err == nil {
		t.Error("Marshal accepted a reference cycle")
	}
	if _, err := Marshal(make(chan int)); err == nil {
		t.Error("Marshal accepted a channel")
	}
}

// TestTimeRoundTrip guards the timestamp helpers this change did not touch.
func TestTimeRoundTrip(t *testing.T) {
	const stamp = "2026-09-04T10:30:00Z"
	parsed, err := ParseTime(stamp)
	if err != nil {
		t.Fatalf("ParseTime: %v", err)
	}
	if got := Time(parsed); got != stamp {
		t.Errorf("Time(ParseTime(%q)) = %q", stamp, got)
	}
	if _, err := ParseTime("not a time"); err == nil {
		t.Error("ParseTime accepted a non-timestamp")
	}
}
