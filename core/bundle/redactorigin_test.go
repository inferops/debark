package bundle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
)

// theSecret is the credential every case below plants. It is deliberately a
// single distinctive literal so a failure can grep the whole bundle for it,
// which is exactly how this defect was reproduced by hand: build once, then
// grep every file the bundle contains.
const theSecret = "S3CR3T-MUST-NOT-SHIP"

// TestAssembleRedactsOriginURIInLock is the regression test for the defect
// the vendor-url-fetch e2e fixture found on its first run: a vendor URL's
// credential written VERBATIM into lock.json's packages[].origin.uri, inside
// the bundle, hashed by the manifest and covered by the signature.
//
// It drives the real Assemble, reads the real lock.json off disk, and
// asserts BOTH halves of the claim for every URL shape a credential can hide
// in — the secret is absent, and the redacted form is present. Either half
// alone is vacuous: "the secret is absent" is satisfied by a build that
// wrote no origin at all, and "the redacted form is present" is satisfied by
// a value that was never credentialed. The fixture's own comment makes the
// same argument about its evidence.json assertions.
//
// It checks the bytes on disk rather than the returned struct, because the
// bytes are what the manifest hashes; and it then checks the returned struct
// as well, because core/engine adopts that very object (finalize.go's
// b.lockDoc = res.Lock) and writes it a second time.
func TestAssembleRedactsOriginURIInLock(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want string // the exact origin.uri expected in lock.json
	}{
		{
			name: "query credential",
			uri:  "https://vendor.example/acme_1.0_amd64.deb?token=" + theSecret,
			want: "https://vendor.example/acme_1.0_amd64.deb?REDACTED",
		},
		{
			name: "userinfo credential",
			uri:  "https://alice:" + theSecret + "@vendor.example/acme_1.0_amd64.deb",
			want: "https://REDACTED@vendor.example/acme_1.0_amd64.deb",
		},
		{
			name: "userinfo and query together",
			uri:  "https://alice:" + theSecret + "@vendor.example/acme_1.0_amd64.deb?sig=" + theSecret,
			want: "https://REDACTED@vendor.example/acme_1.0_amd64.deb?REDACTED",
		},
		{
			name: "credential in the fragment",
			uri:  "https://vendor.example/acme_1.0_amd64.deb#" + theSecret,
			want: "https://vendor.example/acme_1.0_amd64.deb",
		},
		{
			name: "http is honoured and still redacted",
			uri:  "http://127.0.0.1:9410/acme_1.0_amd64.deb?token=" + theSecret,
			want: "http://127.0.0.1:9410/acme_1.0_amd64.deb?REDACTED",
		},
		{
			// url.Parse reports Host=="" for this, so it takes RedactURL's
			// textual fallback. It is here because that is the shape someone
			// reaches for once they notice a redactor keying on a parsed URL.
			name: "unparseable authority still scrubbed",
			uri:  "https://alice:" + theSecret + "@",
			want: "https://REDACTED@",
		},
		{
			// Idempotence matters for more than tidiness: core/engine writes
			// lock.json a second time from the object this package returns,
			// so a redaction that changed its own output would produce two
			// different files for one build and break determinism.
			name: "already redacted is unchanged",
			uri:  "https://vendor.example/acme_1.0_amd64.deb?REDACTED",
			want: "https://vendor.example/acme_1.0_amd64.deb?REDACTED",
		},
		{
			name: "a plain archive uri is not damaged",
			uri:  "https://deb.debian.org/debian/pool/main/j/jq/jq_1.6-2.1_amd64.deb",
			want: "https://deb.debian.org/debian/pool/main/j/jq/jq_1.6-2.1_amd64.deb",
		},
		{
			name: "empty stays empty",
			uri:  "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			staging := t.TempDir()

			sel := selectionFor(t, staging, "acme", "1.0", "amd64", lock.ReasonExternal)
			// Exactly what core/engine's attributeExternalOrigins does
			// (inputs.go): the operator's literal URL, credential and all,
			// on the selection this package is handed.
			sel.URI = tc.uri
			sel.Origin = lock.Origin{URI: tc.uri}

			in := baseAssembleInput(t, root, []resolve.Selection{sel})
			res, err := Assemble(context.Background(), in)
			if err != nil {
				t.Fatalf("Assemble: %v", err)
			}

			raw, err := os.ReadFile(filepath.Join(in.Dir, lock.FileName))
			if err != nil {
				t.Fatalf("read %s: %v", lock.FileName, err)
			}
			if tc.uri != tc.want && strings.Contains(string(raw), theSecret) {
				t.Errorf("%s contains the credential %q; origin.uri was written verbatim into the signed bundle", lock.FileName, theSecret)
			}

			l, err := lock.Load(in.Dir)
			if err != nil {
				t.Fatalf("lock.Load: %v", err)
			}
			if len(l.Packages) != 1 {
				t.Fatalf("lock has %d packages, want 1", len(l.Packages))
			}
			if got := l.Packages[0].Origin.URI; got != tc.want {
				t.Errorf("lock.json origin.uri = %q, want %q", got, tc.want)
			}

			// The object core/engine adopts as its own lock document and
			// writes again at finalize must carry the same redacted value,
			// or the second write puts the credential back.
			if res.Lock == nil || len(res.Lock.Packages) != 1 {
				t.Fatalf("Result.Lock = %+v, want one package", res.Lock)
			}
			if got := res.Lock.Packages[0].Origin.URI; got != tc.want {
				t.Errorf("Result.Lock origin.uri = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAssembleLeaksNoCredentialIntoAnyBundleFile is the by-hand reproduction
// turned into a test: build a bundle whose only input carries a credential,
// then grep every file the bundle contains, rather than only the one file
// the defect was found in. A fix that closes lock.json and leaves another
// carrier is worse than no fix, because it retires the alarm.
func TestAssembleLeaksNoCredentialIntoAnyBundleFile(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()

	sel := selectionFor(t, staging, "acme", "1.0", "amd64", lock.ReasonExternal)
	sel.URI = "https://alice:" + theSecret + "@vendor.example/acme_1.0_amd64.deb?token=" + theSecret
	sel.Origin = lock.Origin{URI: sel.URI}

	in := baseAssembleInput(t, root, []resolve.Selection{sel})
	if _, err := Assemble(context.Background(), in); err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var leaked []string
	err := filepath.WalkDir(in.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), theSecret) {
			rel, _ := filepath.Rel(in.Dir, path)
			leaked = append(leaked, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk bundle: %v", err)
	}
	if len(leaked) != 0 {
		t.Errorf("the credential reached %d bundle file(s): %v", len(leaked), leaked)
	}
}

// TestRedactLockOriginsScrubsEveryPackage pins the gate itself, on a
// document this package did not build from selections. buildLock happens to
// rebuild Packages from scratch today, so an end-to-end test alone would
// pass for the wrong reason if that ever changed; this asserts the property
// the gate actually promises — every package in the document it is given,
// whatever put it there.
func TestRedactLockOriginsScrubsEveryPackage(t *testing.T) {
	l := &lock.Lock{
		Packages: []lock.Package{
			{Name: "a", Origin: lock.Origin{URI: "https://v.example/a.deb?token=" + theSecret}},
			{Name: "b", Origin: lock.Origin{URI: "https://bob:" + theSecret + "@v.example/b.deb"}},
			{Name: "c", Origin: lock.Origin{LocalPath: "/home/alice/c.deb"}},
			{Name: "d", Origin: lock.Origin{
				URI:            "https://v.example/d.deb?sig=" + theSecret,
				Suite:          "bookworm",
				Component:      "main",
				ReleaseDigest:  "sha256:abc",
				KeyFingerprint: "DEADBEEF",
			}},
		},
	}

	redactLockOrigins(l)

	want := []string{
		"https://v.example/a.deb?REDACTED",
		"https://REDACTED@v.example/b.deb",
		"",
		"https://v.example/d.deb?REDACTED",
	}
	for i, w := range want {
		if got := l.Packages[i].Origin.URI; got != w {
			t.Errorf("package %s: origin.uri = %q, want %q", l.Packages[i].Name, got, w)
		}
	}
	// LocalPath is not a URL and RedactURL must not touch it, or a local
	// --file input's record silently changes meaning.
	if got := l.Packages[2].Origin.LocalPath; got != "/home/alice/c.deb" {
		t.Errorf("origin.local_path = %q, want it untouched", got)
	}
	// The provenance fields the record exists for must survive: redaction
	// that also erased "which suite, which key" would trade one loss for
	// another.
	d := l.Packages[3].Origin
	if d.Suite != "bookworm" || d.Component != "main" || d.ReleaseDigest != "sha256:abc" || d.KeyFingerprint != "DEADBEEF" {
		t.Errorf("provenance fields damaged by redaction: %+v", d)
	}
}
