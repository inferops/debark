package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
)

// sha256Hex is a short alias for digest.Bytes, used throughout the test
// files to build or check expected File.SHA256 values.
func sha256Hex(b []byte) string { return digest.Bytes(b) }

// errClass returns the dferr class name of err ("usage", "environment",
// "verification", ...), for asserting which exit-code bucket an error maps
// to without importing the numeric Class value into every test.
func errClass(err error) string { return dferr.ClassOf(err).String() }

// writeFixtureFile writes content to root/rel (rel using forward slashes,
// converted to the host's own separator), creating parent directories as
// needed. Used to build a throwaway fixture tree in t.TempDir() for a test
// that needs a root not worth checking into testdata/.
func writeFixtureFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// realTargetMember returns one member's bytes from a testdata/real-targets
// prototype capture (the same archives extractPrototypeCapture unpacks), or
// nil when that large, separately-owned fixture tree is absent from this
// checkout.
//
// It reports no error, deliberately: its callers are fuzz seed registrations,
// which run at FuzzXxx(f *testing.F) time against genuine captured material,
// and a missing fixture must degrade to "one fewer seed" rather than to a
// broken test binary -- the same skip-don't-fail rule extractPrototypeCapture
// already follows for the tests built on the same archives.
func realTargetMember(tarGzPath, member string) []byte {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err != nil {
			return nil
		}
		if hdr.Name != member || hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return nil
		}
		return data
	}
}

// fuzzRealKeyring is genuine, single-key OpenPGP material out of the real
// Debian 12 capture: trusted.gpg.d's bookworm stable release key, at 280
// bytes the smallest real keyring in either fixture. Fuzz seeds want real
// packet structure to mutate away from -- a hand-written blob would only
// ever exercise the "not a keyring at all" rejection -- and they want it
// small, because a seed corpus runs as an ordinary unit test on every CI run.
//
// sync.OnceValue, not a plain var: the fixture is read out of a gzip stream,
// and each fuzz worker is its own process, so this pays that cost once per
// process instead of once per seed registration. It is nil when the fixture
// tree is absent; every caller must handle that.
var fuzzRealKeyring = sync.OnceValue(func() []byte {
	return realTargetMember(
		filepath.Join("..", "..", "testdata", "real-targets", "debian-12-state.tar.gz"),
		"target-state/keyrings/usr/share/keyrings/debian-archive-bookworm-stable.gpg")
})
