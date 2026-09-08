package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/canonical"
)

// FuzzLoad fuzzes debark.manifest.json document loading with a real,
// well-formed manifest (produced by this package's own Build/Save, exactly
// as buildInputFor's fixture is used elsewhere in this package's tests) as
// a seed, plus hand-picked malformed variants. The invariant is Load's own
// documented, load-bearing property: the canonical bytes it returns are
// produced by re-canonicalising the *exact bytes read from disk*, which
// must be idempotent under RFC 8785 JCS — transforming already-canonical
// bytes a second time must yield the identical result. Load.'s own comment
// warns explicitly against ever "simplifying" this to
// canonical.Marshal(&m), because that would silently drop an unrecognised
// field before it reaches a digest; this fuzz target is the regression
// guard for that promise staying true across arbitrary on-disk bytes, not
// just the one golden case a unit test would think to write by hand.
func FuzzLoad(f *testing.F) {
	dir := f.TempDir()
	m, err := Build(context.Background(), buildInputFor(dir))
	if err == nil {
		if err := Save(dir, m); err == nil {
			if real, rerr := os.ReadFile(filepath.Join(dir, FileName)); rerr == nil {
				f.Add(real)
			}
		}
	}

	f.Add([]byte(`{"schema_version":"debark.manifest/v1"}`))
	f.Add([]byte(`{"schema_version":"debark.manifest/v1","files":[]}`))
	f.Add([]byte(`{"schema_version":"debark.manifest/v0-does-not-exist"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(``))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(`{"schema_version":"debark.manifest/v1", "unknown_future_field": {"nested": [1,2,3]}}`))
	f.Add([]byte(`{"schema_version": "debark.manifest/v1", "z_field": 1, "a_field": 2}`)) // out-of-order keys
	f.Add([]byte("\xef\xbb\xbf" + `{"schema_version":"debark.manifest/v1"}`))             // UTF-8 BOM
	f.Add([]byte(`{"schema_version":"debark.manifest/v1","files":[{"path":"a","sha256":"` + "00000000000000000000000000000000000000000000000000000000000000" + `","size":0}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), data, 0o644); err != nil {
			t.Fatalf("writing fuzz input: %v", err)
		}

		_, canon, err := Load(dir)
		if err != nil {
			return
		}
		again, terr := canonical.Transform(canon)
		if terr != nil {
			t.Fatalf("Load returned canonical bytes that are not themselves valid canonical JSON: %v (canon=%q)", terr, canon)
		}
		if string(again) != string(canon) {
			t.Fatalf("canonicalisation is not idempotent: Transform(canon) != canon\ncanon:  %q\nagain:  %q", canon, again)
		}
	})
}
