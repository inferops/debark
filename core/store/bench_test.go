package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/digest"
)

// These benchmarks measure one thing: what the store's content verification
// costs when a bundle's pool is assembled.
//
// Materialise re-hashes every object it places, and core/bundle's
// ensureInStore reads an already-held object back in full before agreeing to
// use it. Both are unconditional, and both are full passes over every byte of
// a pool that can run to gigabytes, so the cost is worth a number rather than
// an assumption. Nothing here is a test of the check - the Materialise and
// Open verification tests in store_test.go own that. These only time it.
//
// What is being compared, per object already held in the store:
//
//	link                what Materialise did before the check existed: one
//	                    os.Link, no bytes read.
//	link+verify         Materialise as it ships: os.Link, then a full read of
//	                    what landed, hashed and compared.
//	copy, copy+verify   the same pair on the fallback path taken when the
//	                    store and the bundle are on different filesystems,
//	                    where a full read is already unavoidable.
//
// and, at the level core/bundle actually works at,
// BenchmarkAssemblePoolFromStore compares the whole per-object sequence
// before and after the hardening commit, which added three separate passes,
// not one.
//
// Caveat that matters for reading the numbers: the pool is written
// immediately before it is read, so every read is served from the OS page
// cache. These are therefore the CPU-bound floor for the verification cost -
// the best case. On a store whose objects are cold each pass additionally
// pays the storage read, and the floor becomes the pool size divided by the
// smaller of hash throughput and sequential read throughput.

// poolShape is a synthetic bundle pool: count objects of size bytes each. The
// larger shape is sized on a real mid-size offline bundle - a few hundred
// megabytes of .deb, in files averaging a bit over a megabyte.
type poolShape struct {
	name  string
	count int
	size  int64
}

var benchPoolShapes = []poolShape{
	{"32MiB", 64, 512 << 10},
	{"320MiB", 256, 1280 << 10},
}

// benchPool is a store pre-loaded with objects, plus a destination directory
// under the same root - and so on the same volume, which is what lets os.Link
// succeed and keeps the benchmark on the hardlink path a real build takes.
type benchPool struct {
	st      *fsStore
	digests []string
	dests   []string
	total   int64
}

func newBenchPool(b *testing.B, sh poolShape) *benchPool {
	b.Helper()
	root := b.TempDir()
	s, err := Open(filepath.Join(root, "store"))
	if err != nil {
		b.Fatalf("Open store: %v", err)
	}
	poolDir := filepath.Join(root, "pool")
	if err := os.MkdirAll(poolDir, 0o755); err != nil {
		b.Fatalf("create pool dir: %v", err)
	}

	p := &benchPool{st: s.(*fsStore)}
	buf := make([]byte, sh.size)
	for i := 0; i < sh.count; i++ {
		fillIncompressible(buf, uint64(i)+1)
		dg, n, err := p.st.Put(context.Background(), bytes.NewReader(buf))
		if err != nil {
			b.Fatalf("Put object %d: %v", i, err)
		}
		p.digests = append(p.digests, dg)
		p.dests = append(p.dests, filepath.Join(poolDir, fmt.Sprintf("obj-%04d.deb", i)))
		p.total += n
	}
	return p
}

// fillIncompressible writes a deterministic xorshift64* stream into buf. A
// .deb is a compressed archive, so filling the pool with zeroes would let a
// filesystem or a device dedupe away exactly the reads being measured.
func fillIncompressible(buf []byte, seed uint64) {
	x := seed*0x9e3779b97f4a7c15 + 1
	for i := 0; i+8 <= len(buf); i += 8 {
		x ^= x >> 12
		x ^= x << 25
		x ^= x >> 27
		v := x * 0x2545f4914f6cdd1d
		buf[i] = byte(v)
		buf[i+1] = byte(v >> 8)
		buf[i+2] = byte(v >> 16)
		buf[i+3] = byte(v >> 24)
		buf[i+4] = byte(v >> 32)
		buf[i+5] = byte(v >> 40)
		buf[i+6] = byte(v >> 48)
		buf[i+7] = byte(v >> 56)
	}
}

// run times one placement strategy over the whole pool. SetBytes makes the
// reported figure the pool's own throughput, which is the number to compare:
// how fast this machine can turn a store into a bundle pool.
func (p *benchPool) run(b *testing.B, place func(dg, dest string) error) {
	b.SetBytes(p.total)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j, dg := range p.digests {
			if err := place(dg, p.dests[j]); err != nil {
				b.Fatalf("place %s: %v", dg, err)
			}
		}
	}
	b.StopTimer()
	p.clearPool(b)
}

func (p *benchPool) clearPool(b *testing.B) {
	b.Helper()
	for _, d := range p.dests {
		if err := os.Remove(d); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.Fatalf("clear pool file %s: %v", d, err)
		}
	}
}

func (p *benchPool) link(dg, dest string) error {
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Link(p.st.Path(dg), dest)
}

func (p *benchPool) linkVerify(dg, dest string) error {
	return p.st.Materialise(dg, dest)
}

func (p *benchPool) copyOnly(dg, dest string) error {
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return copyToPath(p.st.Path(dg), dest)
}

func (p *benchPool) copyVerify(dg, dest string) error {
	if err := p.copyOnly(dg, dest); err != nil {
		return err
	}
	return verifyFileDigest(dest, dg)
}

// BenchmarkMaterialisePool isolates the store's own verification: the same
// pool placed with and without the re-hash, on both the hardlink path and the
// copy fallback.
func BenchmarkMaterialisePool(b *testing.B) {
	for _, sh := range benchPoolShapes {
		b.Run(sh.name, func(b *testing.B) {
			p := newBenchPool(b, sh)
			b.Run("link", func(b *testing.B) { p.run(b, p.link) })
			b.Run("link+verify", func(b *testing.B) { p.run(b, p.linkVerify) })
			b.Run("copy", func(b *testing.B) { p.run(b, p.copyOnly) })
			b.Run("copy+verify", func(b *testing.B) { p.run(b, p.copyVerify) })
		})
	}
}

// verifyStoreObjectLikeBundle is core/bundle's verifyStoreObject, repeated
// here so the benchmark can time the whole sequence Assemble runs without
// core/store importing core/bundle. If that function changes shape this one
// has to follow it, or the comparison stops being about the real code.
func verifyStoreObjectLikeBundle(st Store, want string) error {
	rc, err := st.Open(want)
	if err != nil {
		return err
	}
	defer rc.Close()
	got, _, err := digest.SHA256Reader(rc)
	if err != nil {
		return err
	}
	if !digest.Equal(got, want) {
		return fmt.Errorf("store object %s holds content that hashes to %s", want, got)
	}
	return nil
}

// assembleAfter is what core/bundle does today for one selection already held
// in the store: read the object back and hash it (ensureInStore), Materialise
// it, which hashes what it wrote, and then hash the placed file once more so
// the check does not depend on which Store implementation was passed in.
// Three full passes over every byte.
func (p *benchPool) assembleAfter(dg, dest string) error {
	if err := verifyStoreObjectLikeBundle(p.st, dg); err != nil {
		return err
	}
	if err := p.st.Materialise(dg, dest); err != nil {
		return err
	}
	got, _, err := digest.SHA256File(dest)
	if err != nil {
		return err
	}
	if !digest.Equal(got, dg) {
		return fmt.Errorf("pool file does not match %s after materialising", dg)
	}
	return nil
}

// BenchmarkAssemblePoolFromStore is the number that matters to a build: the
// per-object work core/bundle does for a selection already held in the store,
// before and after the hardening commit that added the content checks.
func BenchmarkAssemblePoolFromStore(b *testing.B) {
	for _, sh := range benchPoolShapes {
		b.Run(sh.name, func(b *testing.B) {
			p := newBenchPool(b, sh)
			b.Run("before-no-verification", func(b *testing.B) { p.run(b, p.link) })
			b.Run("after-three-passes", func(b *testing.B) { p.run(b, p.assembleAfter) })
		})
	}
}

// BenchmarkVerifyFileDigest is the ceiling every one of those passes runs
// into: one open, one full read, one SHA-256, on a file the page cache is
// holding. Divide a pool's size by this throughput to predict what each extra
// pass costs on a warm store.
func BenchmarkVerifyFileDigest(b *testing.B) {
	const size = 4 << 20
	path := filepath.Join(b.TempDir(), "object")
	buf := make([]byte, size)
	fillIncompressible(buf, 7)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		b.Fatalf("write: %v", err)
	}
	dg := digest.Bytes(buf)

	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := verifyFileDigest(path, dg); err != nil {
			b.Fatalf("verifyFileDigest: %v", err)
		}
	}
}

// BenchmarkSHA256InMemory separates hashing from reading: the same work as
// BenchmarkVerifyFileDigest with the filesystem taken out. The gap between
// the two is what a warm read costs.
func BenchmarkSHA256InMemory(b *testing.B) {
	const size = 4 << 20
	buf := make([]byte, size)
	fillIncompressible(buf, 7)

	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := digest.SHA256Reader(bytes.NewReader(buf)); err != nil {
			b.Fatalf("SHA256Reader: %v", err)
		}
	}
}
