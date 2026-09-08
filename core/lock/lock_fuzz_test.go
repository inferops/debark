package lock

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/canonical"
)

// FuzzLoad fuzzes lock.json document loading with a real, valid lock
// (validLock, the same fixture this package's other tests use) as a seed,
// plus hand-picked malformed variants. The invariant: whenever Load
// succeeds, the parsed *Lock must still be well-formed enough for the rest
// of this package's own API to operate on it without panicking —
// Digest(l) (what Save actually calls to produce the value that ends up in
// the manifest's lock_digest) must not error, and Validate(l) must run to
// completion (returning either nil or a real *dferr.Error, never a panic).
// A parser that produced a *Lock so malformed that the package's own
// Digest/Validate functions could not even run over it would be a parser
// that silently handed a broken value to its own callers.
func FuzzLoad(f *testing.F) {
	dir := f.TempDir()
	if d, err := Save(dir, validLock()); err == nil {
		_ = d
		if real, rerr := os.ReadFile(filepath.Join(dir, FileName)); rerr == nil {
			f.Add(real)
		}
	}

	f.Add([]byte(`{"schema_version":"debark.lock/v1"}`))
	f.Add([]byte(`{"schema_version":"debark.lock/v0-does-not-exist"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(``))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(`{"schema_version":"debark.lock/v1","packages":[{"name":"","arch":"amd64","version":"1"}]}`))
	f.Add([]byte(`{"schema_version":"debark.lock/v1","install":["not:a=valid=entry"]}`))
	f.Add([]byte(`{"schema_version":"debark.lock/v1","resolver":{"backend":"auto"}}`))
	f.Add([]byte(`[1,2,3]`))
	f.Add([]byte(`{"schema_version":"debark.lock/v1","packages":[{"sha256":"not-hex-at-all"}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), data, 0o644); err != nil {
			t.Fatalf("writing fuzz input: %v", err)
		}

		l, err := Load(dir)
		if err != nil {
			return
		}
		if l.SchemaVersion != SchemaVersion {
			t.Fatalf("Load succeeded with SchemaVersion %q, want %q", l.SchemaVersion, SchemaVersion)
		}
		if _, derr := canonical.Digest(l); derr != nil {
			t.Fatalf("Digest(l) failed on a Lock Load itself accepted: %v", derr)
		}
		_ = Validate(l) // must not panic; nil or a real error are both fine
	})
}
