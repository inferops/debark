// Package canonical implements the project's canonical JSON serialisation
// (RFC 8785 JSON Canonicalization Scheme) and the digest rules that every
// hashed or signed debark object depends on.
//
// Rules, frozen by ADR-004:
//
//   - Any object that is hashed or signed is first marshalled to JSON, then
//     canonicalised with JCS: object keys sorted by UTF-16 code unit, no
//     insignificant whitespace, ES6 number formatting, UTF-8 output.
//   - A digest is the lowercase hex SHA-256 of those canonical bytes.
//   - Timestamps are RFC 3339 in UTC with second precision ("2006-01-02T15:04:05Z").
//   - Absent optional fields are omitted, never emitted as null.
//
// # The representability rule
//
// These bytes are what a signature covers, so the serialisation has to be
// injective: two documents that differ must never canonicalise to the same
// bytes, or one signature silently covers both and the manifest a target
// verifies is not the manifest the builder approved. JCS gets most of the way
// there — it sorts keys, fixes number formatting and refuses duplicate keys,
// which is what closes the classic "canonicaliser and JSON parser disagree"
// attack — but two kinds of value survive the transform without surviving it
// intact, and both are refused here rather than encoded:
//
//   - Strings that are not valid UTF-8. encoding/json replaces every
//     undecodable byte with U+FFFD, so "bad\xffname" and "bad\xfename" — two
//     different, perfectly legal ext4 filenames — become the same JSON string
//     and the same digest before JCS is ever reached.
//   - Integers outside ±MaxSafeInteger. RFC 8785 defines numbers as IEEE 754
//     binary64, so 9007199254740993 and 9007199254740992 canonicalise
//     identically, and 9223372036854775807 canonicalises to
//     9223372036854776000, which no int64 can read back.
//
// Refusal, not repair, is the choice. A document that cannot be represented
// exactly is one debark must not sign, and the builder — which has the
// document, the field names and an operator watching — is a far better place
// to say so than the air-gapped target, which would only see a bundle whose
// manifest walk disagrees with its manifest and no explanation of why.
// Refusing costs nothing real: every number debark writes is a size or a
// count (±MaxSafeInteger is nine petabytes), and no captured target fixture
// carries a non-UTF-8 string.
//
// What is deliberately NOT refused here is control characters. They are not a
// collision source — JSON escapes them and JCS mandates one escaping, so
// distinct strings keep distinct bytes — and refusing them is a judgement
// about what a *field* may hold, which needs to be made where the field's
// meaning is known: core/manifest rejects a control byte in a bundle path,
// core/snapshot rejects one in a string bound for a terminal. This package
// canonicalises documents; it does not police their contents.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gowebpki/jcs"

	"github.com/inferops/debark/core/dferr"
)

// TimeFormat is the only timestamp format debark writes.
const TimeFormat = "2006-01-02T15:04:05Z"

// MaxSafeInteger is the largest magnitude an integer may have in a canonical
// debark document: 2^53-1, ECMAScript's Number.MAX_SAFE_INTEGER. RFC 8785
// serialises numbers with the ES6 Number::toString algorithm over IEEE 754
// binary64, so every integer round-trips through a float64; beyond this
// bound that round trip stops being injective and, further out, stops being
// reversible into an int64 at all.
//
// It is exported so that a package which knows what a number means can reject
// an out-of-range one where the field is named and the message can say so -
// lock.Validate bounds a package size from below already - rather than
// leaving the refusal to this package, which only ever sees a literal.
const MaxSafeInteger = 1<<53 - 1

// errClass is the dferr class every refusal in this package carries.
//
// Usage (1), not Verification (4): nothing has been verified and found to
// disagree. A document that cannot be represented is malformed input, caught
// on the builder before a signature exists — the same family as a bad flag or
// an unreadable config. Reporting Verification would tell an operator their
// bundle had been tampered with when the truth is that debark declined to
// serialise it. Every caller already wraps this package's errors as Usage
// (manifest, lock, snapshot all do), so classifying here is what those
// wrappers were already assuming, and it holds for the two call sites that
// discard the error instead of wrapping it.
const errClass = dferr.Usage

// replacementForms are the ways U+FFFD - the rune encoding/json substitutes
// for every byte of a string it cannot decode - can appear in encoder output.
// Its presence there is the only trace the substitution leaves behind.
//
// All of them are looked for because which one appears is an implementation
// detail of encoding/json, not part of its contract: today it writes the
// six-character escape for a byte it replaced and the raw encoding for a
// U+FFFD that was really in the string, which is very nearly a decision
// procedure on its own. Depending on that would be depending on a detail that
// can change under a Go upgrade without notice, so it is used only to decide
// whether the question is worth asking; findInvalidUTF8 answers it.
var replacementForms = [][]byte{
	[]byte(string(utf8.RuneError)),
	[]byte("\\ufffd"),
	[]byte("\\uFFFD"),
}

// Time renders t as a canonical debark timestamp (UTC, second precision).
func Time(t time.Time) string { return t.UTC().Truncate(time.Second).Format(TimeFormat) }

// SourceDateEpochVar is the environment variable debark honours for a
// reproducible artefact clock: https://reproducible-builds.org/specs/source-date-epoch/,
// the same convention the project's release tooling already uses
// (.goreleaser.yaml, hack/reproducible-check.sh). One name, one meaning,
// everywhere debark needs a reproducible timestamp.
const SourceDateEpochVar = "SOURCE_DATE_EPOCH"

// EffectiveTime is the single place in debark that decides what "now" means
// for anything written into an artefact. now supplies the real clock, so a
// caller can substitute a fixed one in a test.
//
// It lives here, beside Time and ParseTime, rather than in whichever package
// happens to need it, because there is now more than one producer of hashed
// artefacts: core/engine writes a bundle, and core/base writes a synthesized
// snapshot whose digest a bundle then records. Two implementations of this
// rule would be two chances for one of them to quietly stop honouring the
// variable, and "a reproducible build that quietly is not reproducible" is
// the failure the rule exists to prevent.
//
//   - Unset or empty: now(), the real wall clock.
//   - A valid Unix timestamp: that instant. Every timestamp the caller writes
//     is derived from this one value.
//   - Anything else (non-numeric, negative, overflowing): a dferr.Usage
//     error, never a silent fallback. A caller who set the variable got its
//     name right and asked for one specific, reproducible instant; treating a
//     malformed value as "use whatever time it is now" would silently defeat
//     the only guarantee they asked for.
func EffectiveTime(now func() time.Time) (string, error) {
	raw := os.Getenv(SourceDateEpochVar)
	if raw == "" {
		return Time(now()), nil
	}
	sec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || sec < 0 {
		return "", dferr.New(dferr.Usage,
			"%s=%q is not a valid Unix timestamp (want a non-negative base-10 integer number of "+
				"seconds since 1970-01-01T00:00:00Z; see https://reproducible-builds.org/specs/source-date-epoch/)",
			SourceDateEpochVar, raw)
	}
	return Time(time.Unix(sec, 0)), nil
}

// ParseTime parses a canonical debark timestamp.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("canonical: parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// Marshal renders v as canonical JSON bytes, refusing any value the
// serialisation cannot carry losslessly (see the package comment).
func Marshal(v any) ([]byte, error) {
	// json.Marshal runs first, and is also what rejects the values that have
	// no JSON form at all — a channel, a func, a reference cycle. Checking v
	// only after it has succeeded is what keeps checkUTF8's walk safe to
	// write as a plain recursion: a cyclic value never reaches it.
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical: marshal: %w", err)
	}
	if err := checkUTF8(v, raw); err != nil {
		return nil, err
	}
	return Transform(raw)
}

// Transform canonicalises already-encoded JSON.
//
// It is the path Load takes on bytes read from disk — verify canonicalises
// the manifest file itself rather than a re-marshalled struct — so its checks
// have to hold for JSON this process did not write.
func Transform(raw []byte) ([]byte, error) {
	// JSON text is UTF-8 (RFC 8259 §8.1) and RFC 8785 output is UTF-8, but
	// jcs.Transform copies undecodable bytes through untouched, which would
	// let Transform emit "canonical" bytes that are not canonical JSON at
	// all - and that no re-marshalling of the same document could reproduce,
	// because encoding/json would have replaced those bytes with U+FFFD.
	if !utf8.Valid(raw) {
		return nil, dferr.New(errClass,
			"canonical: input is not valid UTF-8, so it has no canonical JSON form")
	}
	if err := checkNumbers(raw); err != nil {
		return nil, err
	}
	out, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonical: canonicalise: %w", err)
	}
	return out, nil
}

// MarshalIndent renders v as human-facing JSON with a trailing newline. It is
// used for files that are read by people and validated by schema, but whose
// digest is always computed over the canonical form, never over this one.
//
// It applies the same representability checks even though it produces no
// digest: this is the form Save writes, and manifest.Save writes the manifest
// through it alone. A document whose canonical form would be refused must not
// reach disk looking well-formed. Indentation changes no string and no
// number, so the checks read the indented bytes directly rather than
// encoding a second time.
func MarshalIndent(v any) ([]byte, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("canonical: marshal indent: %w", err)
	}
	if err := checkUTF8(v, out); err != nil {
		return nil, err
	}
	if err := checkNumbers(out); err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// Digest returns the canonical digest of v: lowercase hex SHA-256 over its
// canonical JSON encoding.
func Digest(v any) (string, error) {
	b, err := Marshal(v)
	if err != nil {
		return "", err
	}
	return DigestBytes(b), nil
}

// DigestBytes returns the lowercase hex SHA-256 of b.
func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// checkUTF8 refuses v when encoding it silently replaced an undecodable byte
// with U+FFFD, taking raw as the encoding json.Marshal already produced.
//
// The byte scan comes first and almost always answers on its own: every
// substituted byte becomes a U+FFFD in the output, so output without one
// cannot have lost anything, and no reflection is needed on the path every
// real document takes. Only when a U+FFFD is present does v have to be walked
// to settle the one question the output can no longer answer — whether that
// character was in the document to begin with.
//
// A string that genuinely contains U+FFFD is accepted, and that is not a
// loophole: it is precisely because the undecodable bytes are refused that
// U+FFFD is left with a single preimage and the encoding stays injective.
func checkUTF8(v any, raw []byte) error {
	if !containsReplacement(raw) {
		return nil
	}
	if field, found := findInvalidUTF8(reflect.ValueOf(v), ""); found {
		return dferr.New(errClass,
			"canonical: %s is not valid UTF-8; encoding it would replace the undecodable bytes "+
				"with U+FFFD, so a different value would canonicalise to the same bytes and the "+
				"same signature", fieldLabel(field))
	}
	return nil
}

// findInvalidUTF8 returns the path of the first string in v that is not valid
// UTF-8. It mirrors what encoding/json encodes, so that it neither misses a
// string that reaches the output nor reports one that never would.
func findInvalidUTF8(v reflect.Value, path string) (string, bool) {
	if !v.IsValid() {
		return "", false
	}
	// A nil pointer or interface encodes as null whatever its type promises,
	// so it has no strings and must not have its marshaller called:
	// encoding/json would not have called it either, and a marshaller with a
	// pointer receiver is entitled to assume it is never nil.
	isNilRef := (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil()
	// A type that encodes itself owns the bytes it produces, and its fields
	// need not appear in them at all. Validate the output. No debark
	// document type does this today; the branch exists so that adding one
	// cannot quietly open a hole behind this check.
	if b, ok := ownEncoding(v, isNilRef); ok {
		if !utf8.Valid(b) {
			return path, true
		}
		return "", false
	}
	switch v.Kind() {
	case reflect.String:
		return path, !utf8.ValidString(v.String())
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "", false
		}
		return findInvalidUTF8(v.Elem(), path)
	case reflect.Slice:
		// encoding/json writes a byte slice as base64, which is ASCII: its
		// contents never reach the output as text.
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return "", false
		}
		fallthrough
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if p, found := findInvalidUTF8(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); found {
				return p, true
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if p, found := findInvalidUTF8(k, path+"[key]"); found {
				return p, true
			}
			if p, found := findInvalidUTF8(v.MapIndex(k), fmt.Sprintf("%s[%v]", path, k)); found {
				return p, true
			}
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			// Unexported and json:"-" fields are never encoded, so a bad
			// string in one is not the U+FFFD we are explaining.
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if name == "" {
				name = f.Name
			}
			if p, found := findInvalidUTF8(v.Field(i), path+"."+name); found {
				return p, true
			}
		}
	}
	return "", false
}

// ownEncoding returns the bytes a type that encodes itself produces, if it is
// one. json.Marshal has already run without error by the time this is called,
// so a marshaller that fails here would have failed there: treat it as having
// no own encoding and let the ordinary walk continue.
func ownEncoding(v reflect.Value, isNilRef bool) ([]byte, bool) {
	if isNilRef {
		return nil, false
	}
	if m, ok := v.Interface().(json.Marshaler); ok {
		b, err := m.MarshalJSON()
		return b, err == nil
	}
	if m, ok := v.Interface().(interface{ MarshalText() ([]byte, error) }); ok {
		b, err := m.MarshalText()
		return b, err == nil
	}
	return nil, false
}

// fieldLabel names the offending value for an operator. A struct walk starts
// at the document root, so the path opens with a separator that is noise on
// its own and a "the value" that is clearer than an empty string.
func fieldLabel(path string) string {
	if path == "" {
		return "the value"
	}
	return strings.TrimPrefix(path, ".")
}

// checkNumbers refuses integer literals that canonicalisation could not carry
// back out unchanged.
//
// It reads raw rather than the Go value so that it also covers Transform, and
// therefore the manifest bytes verify reads off a bundle: without it a
// manifest whose size field was edited from 9007199254740992 to
// 9007199254740993 canonicalises to the byte-identical document, and the
// digest verify compares against the signature would not move.
func checkNumbers(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err != nil {
			// End of input, or input this scanner cannot tokenise. Malformed
			// JSON is not this function's to report: jcs.Transform is about
			// to read the same bytes and gives the better message, and it is
			// also the one that refuses duplicate keys.
			return nil
		}
		n, ok := tok.(json.Number)
		if !ok {
			continue
		}
		lit := n.String()
		if strings.ContainsAny(lit, ".eE") {
			// Not an integer literal. RFC 8785 *defines* such a number as the
			// binary64 nearest to it, so it has no identity beyond that
			// double to lose, and encoding/json only ever writes the shortest
			// literal that round-trips.
			continue
		}
		i, perr := strconv.ParseInt(lit, 10, 64)
		if perr != nil || i > MaxSafeInteger || i < -MaxSafeInteger {
			return dferr.New(errClass,
				"canonical: integer %s is outside ±%d, the range RFC 8785 number formatting "+
					"preserves exactly; canonicalising it would change its value",
				lit, MaxSafeInteger)
		}
	}
}

// containsReplacement reports whether raw carries U+FFFD in any of the forms
// an encoder may write it in.
func containsReplacement(raw []byte) bool {
	for _, form := range replacementForms {
		if bytes.Contains(raw, form) {
			return true
		}
	}
	return false
}
