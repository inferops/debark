package snapshot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/digest"
)

// fuzzKeyringArchivePath is the one member FuzzOpenSnapshotDocument
// materialises with real OpenPGP key material instead of zero bytes, so a
// fuzzed document can reach verifyKeyringFingerprints' success path (and the
// re-derivation --approved-keys now depends on) rather than only its
// "carries no such keyring" refusal. Its bytes, digest and fingerprints are
// all real, taken from the genuine Debian 12 capture.
const fuzzKeyringArchivePath = "usr/share/keyrings/debian-archive-bookworm-stable.gpg"

// maxFuzzMaterialisedFiles and maxFuzzArchivePathLen bound what one fuzzed
// document can make the harness create on disk. A document is free to declare
// a hundred thousand files with thousand-byte paths; honouring that would
// turn each execution into a filesystem benchmark and starve the fuzzer of
// executions, which is the resource that actually finds things. Neither bound
// hides anything -- a traversal needs one entry, not a thousand.
const (
	maxFuzzMaterialisedFiles = 64
	maxFuzzArchivePathLen    = 200
)

// FuzzOpenSnapshotDocument fuzzes the snapshot document across a complete
// Open cycle, in both delivery forms Open accepts.
//
// This is the package's primary untrusted surface. A snapshot is captured on
// the offline target -- the untrusted side of the air gap -- and carried on
// removable media to the online builder, which is the one host in the
// pipeline holding a signing key (docs/threat-model.md §3.2). Anyone who can
// hand the operator a snapshot file is therefore an input to the build, and
// snapshot.json is the part of it that Open parses, validates and then acts
// on: it decides what gets extracted where, which digests are checked, which
// keys are believed, and what the operator is shown.
//
// Each execution builds both shapes Open accepts around the fuzzed document
// -- an already-extracted directory and a real snapshot.tar.zst -- and opens
// each. Four invariants:
//
//  1. NOTHING IS WRITTEN OUTSIDE THE EXTRACTION DIRECTORY. Open's own temp
//     root is redirected into a sandbox for the duration, and the sandbox,
//     its parent and its grandparent are diffed around the call. As in
//     FuzzExtractTar, this deliberately does not trust Open's return value:
//     a traversal shows up as an unexpected new entry beside the extraction
//     tree even when Open reported an error. This is the invariant that
//     covers the taint gosec flags in extractRegularFile and copyFilesDir,
//     and it covers copyFilesDir specifically -- the directory branch, which
//     FuzzExtractTar does not reach at all.
//
//  2. A DOCUMENT OPEN ACCEPTS SATISFIES WHAT doValidate CLAIMS TO ENFORCE.
//     Checked against the document Open *returns*, not the one it validated:
//     Open goes on to re-apply redactions and to append a warning when the
//     document disagreed with itself, so the returned document is not the
//     one that passed validation. A snapshot that Open hands back but would
//     refuse on the next load is a bypass, and it would show up here as a
//     property violation rather than as a crash. Alongside it, the digest
//     Open reports must be well formed (it is what the lock and manifest
//     record as snapshot_digest), and every archive path in the accepted
//     document must resolve, on this host's own path rules, inside
//     FilesDir() -- the bridge from "safeArchivePath returned true" to "the
//     operating system agrees", which is exactly where a lexical check can
//     be right and still wrong.
//
//  3. THE TWO ENTRY POINTS AGREE. Whatever Open accepts, OpenDocument must
//     accept: OpenDocument is the weaker of the two (it never touches a
//     files/ tree), core/bundle reads a bundle's snapshot.json copy through
//     it, and a document the builder opened but the bundle reader refuses
//     would split the pipeline in half.
//
//  4. THE TWO DELIVERY FORMS AGREE. A directory and an archive carrying
//     byte-identical content must produce the same accept-or-reject decision
//     and the same digest. extractTar and copyFilesDir are separate
//     implementations of "get the files onto disk", and drift between them
//     would mean a snapshot's identity depended on how it was handed over.
func FuzzOpenSnapshotDocument(f *testing.F) {
	for _, seed := range fuzzOpenSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		ctx := context.Background()

		// Laid out before TMP is redirected, so t.TempDir() itself still
		// lands in the real temp directory and the sandbox nests inside it.
		outer := t.TempDir()
		grandparent := filepath.Dir(outer)
		grandparentBefore := dirEntryNames(t, grandparent)

		srcDir := filepath.Join(outer, "src")
		arDir := filepath.Join(outer, "ar")
		sandbox := filepath.Join(outer, "tmp")
		for _, d := range []string{srcDir, arDir, sandbox} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatalf("preparing %s: %v", d, err)
			}
		}

		members := fuzzArchiveMembers(t, data, srcDir)

		// Open creates its extraction directory with os.MkdirTemp(""), so
		// this is what puts that directory somewhere the invariant can watch.
		// TMP/TEMP are what os.TempDir consults on Windows, TMPDIR on Unix.
		t.Setenv("TMP", sandbox)
		t.Setenv("TEMP", sandbox)
		t.Setenv("TMPDIR", sandbox)

		fromDir, dirErr := Open(ctx, srcDir)
		if fromDir != nil {
			defer func() { _ = fromDir.Close() }()
		}

		arPath := filepath.Join(arDir, "snapshot.tar.zst")
		compressed, cerr := zstdCompress(buildFuzzTarBytes(members))
		if cerr != nil {
			t.Fatalf("compressing the fuzz archive: %v", cerr)
		}
		if err := os.WriteFile(arPath, compressed, 0o644); err != nil {
			t.Fatalf("writing the fuzz archive: %v", err)
		}
		fromTar, tarErr := Open(ctx, arPath)
		if fromTar != nil {
			defer func() { _ = fromTar.Close() }()
		}

		// --- 1. containment -------------------------------------------------
		//
		// Checked before anything else and regardless of the errors above: an
		// escape is an escape whether or not Open admitted to one.
		assertOnly(t, outer, "src", "ar", "tmp")
		for name := range dirEntryNames(t, sandbox) {
			if !strings.HasPrefix(name, "debark-snapshot-") {
				t.Fatalf("Open created %q beside its own extraction directory, in %s: "+
					"an archive member escaped one level out of the extraction root", name, sandbox)
			}
		}
		for name := range dirEntryNames(t, grandparent) {
			if !grandparentBefore[name] {
				t.Fatalf("Open caused a new entry %q to appear in %s, outside the whole sandbox", name, grandparent)
			}
		}

		// --- 2. what Open accepts, doValidate still accepts ------------------
		for _, a := range []*Archive{fromDir, fromTar} {
			if a == nil {
				continue
			}
			if err := doValidate(a.Snapshot); err != nil {
				t.Fatalf("Open accepted a document it would refuse on the next load: %v", err)
			}
			if !digest.Valid(a.Digest) {
				t.Fatalf("Open reported snapshot digest %q, which is not a well-formed digest", a.Digest)
			}
			filesDir := a.FilesDir()
			for _, file := range a.Snapshot.Files() {
				if file.Path == "" && file.ArchivePath == "" {
					continue // doValidate's own skip: a wholly absent optional entry
				}
				p := filepath.Join(filesDir, filepath.FromSlash(file.ArchivePath))
				if !withinDir(filesDir, p) {
					t.Fatalf("Open accepted archive_path %q, which resolves to %q -- outside FilesDir() %q",
						file.ArchivePath, p, filesDir)
				}
			}
		}

		// --- 3. Open and OpenDocument agree ---------------------------------
		if dirErr == nil {
			if _, err := OpenDocument(ctx, filepath.Join(srcDir, DocumentName)); err != nil {
				t.Fatalf("Open accepted this document but OpenDocument refused it: %v", err)
			}
		}

		// --- 4. the two delivery forms agree --------------------------------
		//
		// Bounded by the document cap: extractTar reads snapshot.json through
		// readCappedDocument while Open's directory branch reads it with a
		// plain os.ReadFile, so above that size the two forms are not
		// comparable. No fuzzer-generated input comes near it; the guard is
		// here so that if one ever does, this reports the real asymmetry
		// rather than a spurious disagreement.
		if len(data) <= maxSnapshotDocumentSize {
			if (dirErr == nil) != (tarErr == nil) {
				t.Fatalf("the same snapshot was accepted in one delivery form and refused in the other:\n"+
					"  as a directory: %v\n  as an archive:  %v", dirErr, tarErr)
			}
			if fromDir != nil && fromTar != nil && fromDir.Digest != fromTar.Digest {
				t.Fatalf("the same snapshot has two identities depending on how it was delivered: "+
					"directory %s, archive %s", fromDir.Digest, fromTar.Digest)
			}
		}
	})
}

// fuzzArchiveMembers writes the files/ tree that accompanies one fuzzed
// document into srcDir, and returns the same content as tar member names so
// the archive form of the same snapshot can be built from it.
//
// A zero-byte file is materialised for every archive path the document
// declares, because the empty digest is a value a seed document can record
// and therefore a way for a mutated document to keep reaching
// verifyExtractedFiles' success path instead of stopping at "missing from
// archive". The one exception is fuzzKeyringArchivePath, which gets real key
// material so the keyring re-derivation is reachable too.
//
// Every path the document supplies is run through safeArchivePath -- the same
// gate extractTar applies -- and then re-checked for containment against the
// files directory before anything is created. That is deliberate belt and
// braces: this harness is the one place in the test that takes a
// path out of fuzzer-controlled bytes and hands it to the filesystem, so a
// bug here would let the fuzzer write outside its own sandbox and the
// invariant it exists to check would never fire.
func fuzzArchiveMembers(t *testing.T, doc []byte, srcDir string) map[string][]byte {
	t.Helper()

	members := map[string][]byte{DocumentName: doc}
	content := map[string][]byte{}
	if key := fuzzRealKeyring(); len(key) > 0 {
		content[fuzzKeyringArchivePath] = key
	}

	// Best effort: an undecodable document simply declares no files, which is
	// itself a case worth running (Open must refuse it, not panic).
	var declared Snapshot
	if err := json.Unmarshal(doc, &declared); err == nil {
		for _, file := range declared.Files() {
			if len(content) >= maxFuzzMaterialisedFiles {
				break
			}
			ap := file.ArchivePath
			if len(ap) > maxFuzzArchivePathLen || !safeArchivePath(ap) {
				continue
			}
			if _, ok := content[ap]; !ok {
				content[ap] = nil
			}
		}
	}

	filesDir := filepath.Join(srcDir, FilesDir)
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		t.Fatalf("preparing %s: %v", filesDir, err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, DocumentName), doc, 0o644); err != nil {
		t.Fatalf("writing the fuzzed document: %v", err)
	}
	for ap, data := range content {
		dest := filepath.Join(filesDir, filepath.FromSlash(ap))
		if !withinDir(filesDir, dest) {
			// safeArchivePath said yes and the host disagreed. That is
			// invariant 2's whole subject, so fail here rather than write it.
			t.Fatalf("harness refused to materialise %q: it resolves to %q, outside %q", ap, dest, filesDir)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatalf("preparing %s: %v", filepath.Dir(dest), err)
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			t.Fatalf("writing %s: %v", dest, err)
		}
		members[FilesDir+"/"+ap] = data
	}
	return members
}

// withinDir reports whether p is dir itself or lies underneath it, according
// to the host's own path rules rather than to a string comparison on the
// forward-slash form. filepath.Rel is what makes this a real containment
// test: it resolves the ".." and separator handling that differ between
// Windows and Unix, which is the difference a lexical check can miss.
func withinDir(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// dirEntryNames lists dir's immediate entries as a set, for the before/after
// diff the containment invariant is built on.
func dirEntryNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

// assertOnly fails unless dir contains exactly the named entries.
func assertOnly(t *testing.T, dir string, want ...string) {
	t.Helper()
	allowed := make(map[string]bool, len(want))
	for _, w := range want {
		allowed[w] = true
	}
	for name := range dirEntryNames(t, dir) {
		if !allowed[name] {
			t.Fatalf("an unexpected entry %q appeared in %s: something wrote outside the extraction directory", name, dir)
		}
	}
}

// fuzzOpenSeeds returns the seed corpus: real captured documents first, then
// the hostile document shapes prior reviews of this package found
// interesting. Everything here is small and cheap to run, because a seed
// corpus is executed as an ordinary unit test on every CI run.
func fuzzOpenSeeds() [][]byte {
	base := func() *Snapshot {
		return &Snapshot{
			SchemaVersion: SchemaVersion,
			CreatedAt:     "2026-09-03T00:00:00Z",
			Tool:          Tool{Name: "debark", Version: "0.1.0"},
			Target: Target{
				DistroID: "debian", VersionID: "12", Codename: "bookworm",
				Arch: "amd64", APTVersion: "2.6.1", DpkgVersion: "1.21.22",
			},
			// The empty digest, matching the zero-byte file the harness
			// materialises: this is what lets a seed reach the end of Open
			// rather than stopping at the first digest mismatch.
			DpkgStatus: File{Path: "/var/lib/dpkg/status", ArchivePath: "var/lib/dpkg/status", SHA256: digest.Bytes(nil)},
		}
	}
	marshal := func(s *Snapshot) []byte {
		doc, err := canonical.MarshalIndent(s)
		if err != nil {
			return nil
		}
		return doc
	}

	var seeds [][]byte
	add := func(b []byte) {
		if len(b) > 0 {
			seeds = append(seeds, b)
		}
	}

	add(marshal(base()))

	// A document that reaches the keyring re-derivation with real key
	// material: fingerprints, key ids and user ids all taken from the bytes
	// the harness actually materialises, so this seed opens successfully all
	// the way through verifyKeyringFingerprints.
	if key := fuzzRealKeyring(); len(key) > 0 {
		if parsed, err := parseKeyringFile(key); err == nil && len(parsed) > 0 {
			s := base()
			s.APT.Keyrings = []File{{
				Path:        "/" + fuzzKeyringArchivePath,
				ArchivePath: fuzzKeyringArchivePath,
				Size:        int64(len(key)),
				SHA256:      digest.Bytes(key),
			}}
			for _, k := range parsed {
				s.KeyringFingerprints = append(s.KeyringFingerprints, KeyFingerprint{
					Fingerprint: k.Fingerprint,
					KeyID:       k.KeyID,
					UserIDs:     k.UserIDs,
					Keyring:     s.APT.Keyrings[0].Path,
				})
			}
			sortKeyFingerprints(s.KeyringFingerprints)
			add(marshal(s))

			// The same document with one fingerprint hex digit changed: the
			// "claims a key its bytes do not contain" refusal, which is the
			// tampering --approved-keys exists to catch.
			bad := marshal(s)
			add([]byte(strings.Replace(string(bad), parsed[0].Fingerprint,
				"0000000000000000000000000000000000000000", 1)))
		}
	}

	// Claimed redactions, so reapplyRedactions and the "claimed redactions it
	// had not applied" path are both reachable from the seed corpus.
	{
		s := base()
		s.Target.MachineID = "deadbeefdeadbeefdeadbeefdeadbeef"
		s.Labels = map[string]string{"site": "hq"}
		s.Redactions = []string{RedactLabels, RedactMachineID, RedactProxies}
		add(marshal(s))
	}
	// A document sitting exactly on the warning bound that *also* disagrees
	// with its own redactions list, so Open has to append its tamper warning
	// to a list that is already full. This is the case invariant 2 caught:
	// before archive.go made room for it, Open returned a 257-warning
	// document that Open itself would refuse on the next load.
	{
		s := base()
		s.Target.MachineID = "deadbeefdeadbeefdeadbeefdeadbeef"
		s.Redactions = []string{RedactMachineID}
		for i := 0; i < maxWarnings; i++ {
			s.Warnings = append(s.Warnings, "captured warning "+strconv.Itoa(i))
		}
		add(marshal(s))
	}
	{
		s := base()
		s.APT.Conf = []File{{
			Path: "/etc/apt/apt.conf.d/99proxy", ArchivePath: "etc/apt/apt.conf.d/99proxy",
			SHA256: digest.Bytes(nil),
		}}
		s.Redactions = []string{RedactProxies}
		add(marshal(s))
	}

	// Hostile archive_path shapes, written as raw JSON rather than through
	// the struct so the exact bytes on the wire are the thing being fuzzed.
	// Each is a traversal Validate must refuse before extraction runs.
	for _, ap := range []string{
		"../../../etc/passwd",
		"/etc/passwd",
		"a/../../evil",
		"C:\\evil",
		"a\\b",
		"a:b",
		"./a",
		"a//b",
		"a/./b",
		"..",
		"",
		"a\u0000b",
		"a\u001b[2Kb",
		"a\u009bb",
		strings.Repeat("a/", 300) + "b",
	} {
		add(rawFuzzDocument(`"path":"/x","archive_path":` + mustJSONString(ap) + `,"sha256":"` + digest.Bytes(nil) + `"`))
	}

	// Two files claiming one archive path with different content: the
	// "which bytes are authoritative" ambiguity doValidate refuses.
	add([]byte(`{"schema_version":"` + SchemaVersion + `","created_at":"2026-09-03T00:00:00Z",` +
		`"target":{"distro_id":"debian","version_id":"12","arch":"amd64"},` +
		`"dpkg_status":{"path":"/a","archive_path":"same","sha256":"` + digest.Bytes(nil) + `"},` +
		`"apt":{"sources":[{"path":"/b","archive_path":"same","sha256":"` + digest.Bytes([]byte("x")) + `"}]}}`))

	// Display strings aimed at the operator's terminal, and the bounds
	// checkDisplayStrings puts on them.
	add(rawFuzzDocumentField(`"warnings":["\u001b[1A\u001b[2Kforged digest line"]`))
	add(rawFuzzDocumentField(`"warnings":["\u009b2K"]`))
	add(rawFuzzDocumentField(`"labels":{"\u001b[2K":"x"}`))
	add(rawFuzzDocumentField(`"warnings":[` + strings.TrimSuffix(strings.Repeat(`"w",`, maxWarnings), ",") + `]`))
	add(rawFuzzDocumentField(`"warnings":[` + strings.TrimSuffix(strings.Repeat(`"w",`, maxWarnings+1), ",") + `]`))

	// Numbers outside the range a JSON reader can carry losslessly: sizes and
	// counts are int64 in the schema, and 2^53 is where a JSON consumer that
	// is not Go stops agreeing with one that is.
	add(rawFuzzDocumentField(`"installed_count":9007199254740993`))
	add(rawFuzzDocumentField(`"installed_count":-9223372036854775808`))
	add(rawFuzzDocument(`"path":"/x","archive_path":"x","size":18446744073709551616,"sha256":"` + digest.Bytes(nil) + `"`))
	add(rawFuzzDocumentField(`"installed_count":1e400`))

	// Structurally hostile JSON: nesting depth, and the degenerate documents.
	add([]byte(strings.Repeat(`{"x":`, 1500) + "1" + strings.Repeat("}", 1500)))
	add([]byte(`{}`))
	add([]byte(`null`))
	add([]byte(`[]`))
	add([]byte(`"a string, not an object"`))
	add([]byte(``))
	add([]byte(`{"schema_version":"debark.snapshot/v2"}`))
	// Invalid UTF-8 in the raw bytes: Go's JSON decoder substitutes U+FFFD,
	// so the document that reaches doValidate is not the document on the wire.
	add(append([]byte(`{"schema_version":"`+SchemaVersion+`","created_at":"2026-09-03T00:00:00Z",`+
		`"target":{"distro_id":"`), append([]byte{0xff, 0xfe, 0xfd}, []byte(`","version_id":"12","arch":"amd64"},`+
		`"dpkg_status":{"path":"/a","archive_path":"a","sha256":"`+digest.Bytes(nil)+`"}}`)...)...))

	return seeds
}

// rawFuzzDocument builds an otherwise-valid document whose single apt source
// entry is the supplied raw JSON field text, so a seed can put an exact byte
// sequence into archive_path that the Go struct could not round-trip.
func rawFuzzDocument(sourceFields string) []byte {
	return []byte(`{"schema_version":"` + SchemaVersion + `","created_at":"2026-09-03T00:00:00Z",` +
		`"target":{"distro_id":"debian","version_id":"12","arch":"amd64"},` +
		`"dpkg_status":{"path":"/var/lib/dpkg/status","archive_path":"var/lib/dpkg/status","sha256":"` + digest.Bytes(nil) + `"},` +
		`"apt":{"sources":[{` + sourceFields + `}]}}`)
}

// rawFuzzDocumentField builds an otherwise-valid document carrying one extra
// raw top-level field.
func rawFuzzDocumentField(field string) []byte {
	return []byte(`{"schema_version":"` + SchemaVersion + `","created_at":"2026-09-03T00:00:00Z",` +
		`"target":{"distro_id":"debian","version_id":"12","arch":"amd64"},` +
		`"dpkg_status":{"path":"/var/lib/dpkg/status","archive_path":"var/lib/dpkg/status","sha256":"` + digest.Bytes(nil) + `"},` +
		field + `}`)
}

// mustJSONString encodes s as a JSON string literal, for seeds that build
// document text by concatenation.
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
