package lock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/inferops/debark/core/dferr"
)

// This file holds the checks Validate applies to one Package, plus the one
// check Load applies before it hands a lock to anybody at all.
//
// The split matters. Load is reached by tools whose whole job is to look at a
// bundle that may be wrong — `debark inspect`, `debark doctor` — and
// making those refuse a merely inaccurate lock would take away the tool an
// operator reaches for precisely when a bundle is suspect. So Load enforces
// only what is unsafe to carry forward rather than what is untidy, and
// Validate (build time, and install time once verify has passed) enforces the
// rest: everything api/schema/lock.v1.schema.json states and this package can
// see without opening the pool.

// maxLockBytes bounds what Load will read.
//
// lock.json arrives on removable media and is read BEFORE anything has
// verified it: core/bundle.Open loads the lock first and reaches the signed
// manifest several steps later, so `debark doctor BUNDLE` and `debark
// inspect BUNDLE` both parse an attacker-chosen document with no signature
// behind it. os.ReadFile has no ceiling, so a lock.json the size of the medium
// was read into memory in one allocation. 64 MiB is far past any real lock —
// the largest plausible one, a full desktop closure of a few thousand
// packages at roughly 400 bytes each, is under 2 MiB — and small enough that
// refusing costs nothing an honest bundle needs.
const maxLockBytes = 64 << 20

// validPublisherVerification is publisher_verification's enum from
// lock.v1.schema.json. An out-of-enum value is not cosmetic: core/policy's
// require-signed-publisher rule denies exactly VerifiedURLUnverified, so any
// other spelling — "", "unverified", "totally-fine" — is a file that reads as
// having a provenance claim it does not have.
var validPublisherVerification = []PublisherVerification{
	VerifiedAPTSigned,
	VerifiedURLUnverified,
	VerifiedUserDigest,
	VerifiedUserSignature,
}

// validClosedWorldResults is closed_world_check.result's enum.
var validClosedWorldResults = []string{ClosedWorldOK, ClosedWorldFailed, ClosedWorldSkipped}

// checkFilename reports why filename is not safe to resolve against a bundle
// directory, or nil when it is.
//
// This is the one Package field a reader turns into a filesystem path, and
// until now nothing checked it at all. core/doctor does
// filepath.Join(bundleDir, "repo", filepath.FromSlash(pkg.Filename)) and
// opens the result; filepath.Join cleans the path but cannot undo a "..",
// so a lock naming "../../../../etc/shadow" reads a file outside the bundle
// entirely — from an unverified document, on the path where verification has
// not run yet. An absolute path escapes the same way with no "..".
//
// The rule is the strict one: a relative, forward-slash, already-clean path
// with no parent reference, no empty segment and no byte that has no business
// in a pool path. Every filename debark itself writes comes from
// repository.PoolPath, which produces exactly that shape, so nothing an
// honest build emits is affected.
func checkFilename(filename string) error {
	switch {
	case filename == "":
		return dferr.New(dferr.Usage, "empty filename")
	case strings.ContainsRune(filename, 0):
		return dferr.New(dferr.Usage, "filename contains a NUL byte")
	case strings.ContainsRune(filename, '\\'):
		// Rejected rather than normalised: on Windows a backslash is a
		// separator and on Linux it is an ordinary character, so the same
		// lock would name two different files on the builder and the target.
		return dferr.New(dferr.Usage, "filename contains a backslash; bundle paths are always forward-slashed")
	case strings.HasPrefix(filename, "/"):
		return dferr.New(dferr.Usage, "filename is absolute")
	case len(filename) >= 2 && filename[1] == ':':
		return dferr.New(dferr.Usage, "filename carries a drive letter")
	}
	for _, c := range filename {
		if c < 0x20 || c == 0x7f {
			return dferr.New(dferr.Usage, "filename contains a control character")
		}
	}
	for _, seg := range strings.Split(filename, "/") {
		switch seg {
		case "":
			return dferr.New(dferr.Usage, "filename has an empty path segment")
		case ".", "..":
			return dferr.New(dferr.Usage, "filename contains a %q path segment, which would resolve outside the bundle", seg)
		}
	}
	return nil
}

// checkLoadSafety is what Load enforces: the subset of Validate whose failure
// is dangerous rather than merely wrong. Two rules, both about a Package's
// filename rather than the accuracy of its contents.
//
//  1. Every Filename resolves inside the bundle (checkFilename). Every caller
//     of Load may go on to open these paths and none of them re-derives the
//     filename itself.
//
//  2. No two entries name the same Filename. This one was Validate-only, and
//     that placement was the bug: bundle.Open calls Load and NEVER Validate,
//     so `debark doctor BUNDLE` — the command an operator runs FIRST on
//     media that just arrived, before anything is verified — did its work
//     once per lock ENTRY rather than once per file on the medium. A single
//     .deb named by every entry in the lock is parsed, decompressed and
//     script-scanned once per entry, and each pass appends findings that are
//     held until the report is returned. Measured against core/doctor with
//     one 948-byte fixture .deb named N times: 16 ms at N=10, 74 ms at N=50,
//     125 ms at N=100, 256 ms at N=200 — flat at ~1.3 ms per entry, with the
//     constant set by the size of the .deb, which the same attacker chooses.
//     maxLockBytes (64 MiB) over a ~145-byte minimal entry extrapolates to a
//     few hundred thousand entries, so the ceiling is an out-of-memory kill
//     rather than a slow command. Uniqueness is what ties a reader's work
//     back to what is actually on the medium.
//
//     Bounding the report inside core/doctor instead was considered and
//     rejected: truncating an operator's findings is a contract change, and
//     it treats the symptom. "One file, one entry" is a property of the
//     document, so the lock is where it belongs.
//
// The duplicate-(name, arch) rule deliberately stays in Validate rather than
// moving here with its sibling. It reads like the same kind of rule, and it
// is the same kind of AMBIGUITY — Find returns whichever entry comes first
// and InstallSet drops the one naming the other — but it is not load-bearing
// in the same way and it is not free:
//
//   - It cannot amplify anything. Two entries at distinct filenames need two
//     real files on the medium, so a reader's work already tracks what is
//     there. Only the shared-filename case breaks that link.
//   - No Load-only consumer reaches the ambiguity today. Find and InstallSet
//     have exactly one caller between them outside this package,
//     core/install/select.go, which runs Validate first (core/install/
//     bundle.go). bundle.Open's own consumers — doctor, inspect, store add —
//     iterate Packages directly.
//   - It costs something real. Moving it made Save refuse a lock
//     core/bundle's TestAssembleRejectsTwoSelectionsClaimingOnePoolPath
//     builds on purpose (two versions of one package at distinct pool
//     paths), which is a change to a package this review does not own, for a
//     shape lock.Validate already refuses a few steps later in the engine.
//
// So it stays where it is, and the asymmetry is recorded rather than papered
// over. What is in Validate is everything about whether a lock is ACCURATE —
// field grammar, enums, digests, install-set consistency, one version per
// package. Load must still open a bundle that is merely wrong, or it takes
// the diagnostic tool away exactly when an operator needs it.
func checkLoadSafety(l *Lock) error {
	byFilename := make(map[string]string, len(l.Packages))
	for i, p := range l.Packages {
		if err := checkFilename(p.Filename); err != nil {
			return dferr.New(dferr.Usage, "lock: packages[%d] (%s): %v", i, p.Name, err)
		}
		if prior, dup := byFilename[p.Filename]; dup {
			return dferr.New(dferr.Usage, "lock: %s and %s:%s both claim the file %s", prior, p.Name, p.Arch, p.Filename)
		}
		byFilename[p.Filename] = p.Name + ":" + p.Arch
	}
	return nil
}

// checkDocumentShape refuses a lock.json whose KEYS are not exactly the ones
// debark.lock/v1 declares, before the document is decoded into a Lock.
//
// json.Decoder.DisallowUnknownFields, which Load already sets, is not enough
// on its own, and the two gaps it leaves both matter for a document that
// arrives on removable media and is read before anything has verified it:
//
//   - encoding/json matches field names CASE-INSENSITIVELY. "FILENAME",
//     "Schema_Version" and "PaCkAgEs" all decode into the real fields and
//     DisallowUnknownFields raises nothing, because it only complains about
//     keys that match no field at all. lock.v1.schema.json sets
//     "additionalProperties": false, so an auditor validating the same bytes
//     against the published schema REJECTS a document debark accepts —
//     two readers of one file disagreeing about whether it is a lock.
//   - a repeated key is not an error either: encoding/json keeps the LAST
//     occurrence and says nothing. This project already refuses that in
//     core/policy ("a reader that keeps the first would disagree about what
//     the very same file means") and, more pointedly, in its own
//     canonicaliser: canonical.Transform — the function core/verify runs over
//     these exact bytes to derive lock.json's digest — fails outright on a
//     duplicate key. So Load accepted documents the rest of debark cannot
//     even read, and `debark inspect`/`doctor`, which never verify, then
//     reported the last-wins reading of a lock nothing else agrees with.
//
// Trailing content after the object is refused for the same reason:
// json.Decoder.Decode stops at the end of the first value, so a second
// document appended to lock.json was silently ignored here and rejected by
// canonical.Transform there.
//
// The declared key set is taken from the struct tags by reflection rather
// than restated as a list, so it cannot drift from the types the way a
// hand-maintained table would.
func checkDocumentShape(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var doc json.RawMessage
	if err := dec.Decode(&doc); err != nil {
		return err
	}
	if err := checkJSONValue(doc, reflect.TypeOf(Lock{}), ""); err != nil {
		return err
	}
	// Exactly one JSON value, nothing after it.
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing content after the lock document; a lock.json holds exactly one JSON object")
	}
	return nil
}

// checkJSONValue walks one raw JSON value against the Go type it will be
// decoded into, recursing through structs and slices. Scalars are left to
// encoding/json, which type-checks them during the real decode.
func checkJSONValue(raw json.RawMessage, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		if trimmed[0] != '{' {
			return nil // a type error encoding/json will report precisely
		}
		return checkJSONObject(trimmed, t, path)
	case reflect.Slice, reflect.Array:
		if trimmed[0] != '[' {
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return err
		}
		for i, item := range items {
			if err := checkJSONValue(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

// checkJSONObject enforces the key rules for one JSON object: every key is
// declared by t, exactly as spelled, and appears exactly once.
func checkJSONObject(raw json.RawMessage, t reflect.Type, path string) error {
	fields := jsonFieldsOf(t)
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil { // the opening '{'
		return err
	}
	seen := make(map[string]bool, len(fields))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := tok.(string)
		if !ok {
			return fmt.Errorf("%sexpected an object key, found %v", prefix(path), tok)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
		ft, declared := fields[name]
		if !declared {
			return fmt.Errorf("%sunknown field %q (debark.lock/v1 declares only: %s)",
				prefix(path), name, strings.Join(sortedFieldNames(fields), ", "))
		}
		if seen[name] {
			return fmt.Errorf("%sfield %q given more than once; encoding/json would keep the last one silently and canonical JSON refuses the document outright", prefix(path), name)
		}
		seen[name] = true
		if err := checkJSONValue(value, ft, joinPath(path, name)); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return err
	}
	return nil
}

// jsonFieldsOf maps a struct's JSON key names to the types behind them, from
// the `json` tags alone — the same names encoding/json will look for, minus
// its case-insensitive fallback.
func jsonFieldsOf(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name := f.Name
		if tag, ok := f.Tag.Lookup("json"); ok {
			if tag == "-" {
				continue
			}
			if base, _, _ := strings.Cut(tag, ","); base != "" {
				name = base
			}
		}
		out[name] = f.Type
	}
	return out
}

func sortedFieldNames(fields map[string]reflect.Type) []string {
	out := make([]string, 0, len(fields))
	for k := range fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func prefix(path string) string {
	if path == "" {
		return ""
	}
	return path + ": "
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// checkPackageFields applies lock.v1.schema.json's per-package constraints.
func checkPackageFields(i int, p Package) error {
	if p.Name == "" {
		return dferr.New(dferr.Usage, "lock: packages[%d]: empty name", i)
	}
	// The schema's name pattern is ^[a-z0-9][a-z0-9+.-]*$ (Debian Policy
	// 5.6.1). It is worth enforcing here and not only for tidiness: the name
	// is interpolated into the pool path and handed to apt as part of
	// "name:arch=version", and a grammar with no "/" and no bare ".." is what
	// makes both of those safe.
	if !validPackageName(p.Name) {
		return dferr.New(dferr.Usage, "lock: packages[%d]: %q is not a legal Debian package name", i, p.Name)
	}
	if p.Arch == "" {
		return dferr.New(dferr.Usage, "lock: packages[%d] (%s): empty arch", i, p.Name)
	}
	// An arch containing ":" or "=" makes "name:arch=version" ambiguous —
	// the same string could be split more than one way, so two readers of one
	// install entry can disagree about which package it names.
	if strings.ContainsAny(p.Arch, ":=") || hasSpaceOrControl(p.Arch) {
		return dferr.New(dferr.Usage, "lock: packages[%d] (%s): malformed arch %q", i, p.Name, p.Arch)
	}
	if p.Version == "" {
		return dferr.New(dferr.Usage, "lock: packages[%d] (%s): empty version", i, p.Name)
	}
	// A version may legitimately contain ":" (the epoch), which is why
	// parseInstallEntry splits on "=" first and looks for ":" only to the
	// left of it. "=" itself never appears in a Debian version and would
	// break that split.
	if strings.ContainsRune(p.Version, '=') || hasSpaceOrControl(p.Version) {
		return dferr.New(dferr.Usage, "lock: packages[%d] (%s): malformed version %q", i, p.Name, p.Version)
	}
	if err := checkFilename(p.Filename); err != nil {
		return dferr.New(dferr.Usage, "lock: packages[%d] (%s): %v", i, p.Name, err)
	}
	// The schema says minimum 0. A negative size is not a harmless oddity:
	// core/install sums these to decide whether the target has room, so one
	// negative entry can make an install that will not fit look as though it
	// will.
	if p.Size < 0 {
		return dferr.New(dferr.Usage, "lock: packages[%d] (%s): negative size %d", i, p.Name, p.Size)
	}
	if !validReason(p.Reason) {
		return dferr.New(dferr.Usage,
			"lock: packages[%d] (%s): reason %q is not one of %s, %s, %s or %s<pkg>",
			i, p.Name, p.Reason, ReasonRequested, ReasonUpgrade, ReasonExternal, ReasonDependencyOfPrefix)
	}
	if !containsPV(validPublisherVerification, p.PublisherVerification) {
		return dferr.New(dferr.Usage,
			"lock: packages[%d] (%s): publisher_verification %q is not a recognised provenance claim",
			i, p.Name, p.PublisherVerification)
	}
	return nil
}

// validReason applies the schema's reason pattern
// ^(requested|upgrade|external|dependency-of:.+)$.
func validReason(s string) bool {
	switch s {
	case ReasonRequested, ReasonUpgrade, ReasonExternal:
		return true
	}
	return strings.HasPrefix(s, ReasonDependencyOfPrefix) && len(s) > len(ReasonDependencyOfPrefix)
}

// validPackageName applies the schema's ^[a-z0-9][a-z0-9+.-]*$.
func validPackageName(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		lowerAlnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if i == 0 {
			if !lowerAlnum {
				return false
			}
			continue
		}
		if !lowerAlnum && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func hasSpaceOrControl(s string) bool {
	for _, c := range s {
		if c <= ' ' || c == 0x7f {
			return true
		}
	}
	return false
}

func containsPV(list []PublisherVerification, v PublisherVerification) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
