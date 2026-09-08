package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/digest"
)

// What this file answers.
//
// The store re-verifies content against the address it is stored under: every
// object placed into a bundle's pool is read back in full and re-hashed before
// the placement is allowed to stand. That check was added on security grounds -
// the object tree is ordinary files, so anything able to write one of them can
// park its own bytes behind a digest apt vouched for, and every consumer
// downstream (the pool, the Packages index, the signed manifest) would then
// agree with each other about the attacker's bytes. It was accepted without a
// number attached. These benchmarks supply the number, because the failure they
// guard against is not a wrong answer but a build that quietly stops being
// usable: an operator on slow storage discovering, on a five-gigabyte bundle,
// that assembly now takes minutes it did not take before.
//
// The question, precisely: how much wall clock does a build spend on
// verification that it did not spend before the check existed, at the sizes
// debark is for - the reference bundle of 74 packages and about 40 MB, and
// production bundles running to several gigabytes?
//
// Where the cost is paid, per object, per build:
//
//	Materialise          one full read and SHA-256 of the file it just placed
//	                     (verifyFileDigest). On the hardlink path this is pure
//	                     addition: before the check Materialise moved no bytes
//	                     at all, it created a directory entry. On the copy
//	                     fallback the bytes were being read anyway, so the check
//	                     adds a second pass over data the write left warm.
//	Open                 SHA-256 of everything read, folded into the caller's
//	                     own read (verifyingReader). No extra I/O; CPU only, and
//	                     only for a reader taken to EOF.
//	PutFile, dedup hit   a second full read: the source is hashed, and then the
//	                     object already filed at that address is read back and
//	                     checked before the copy is skipped (putByDedup).
//	Put, PutFile by move unchanged. The hashing there computes the address; it
//	                     is not verification, and there is nothing to compare
//	                     against.
//	Has, Path, GC        free. They never open an object.
//
// So the cost is per object and per build - not per byte stored, and not once
// per store. A rebuild that changes nothing still pays it in full for every
// selection, because every selection is materialised into the pool again.
//
// core/bundle then stacks three of these on one object: ensureInStore reads it
// back through Open, Materialise re-reads what it placed, and
// materialiseSelections hashes the pool file once more so the property does not
// depend on which Store implementation was passed in. The figure a build feels
// is three passes over the pool, not one, which is why
// BenchmarkBundleVerificationCost times that whole sequence and not just
// Materialise.
//
// Reading the numbers: the page cache caveat, which matters more than anything
// else here. Every object below was written moments earlier by the same
// process, so every read is served by the OS page cache and nothing here
// touches the storage device on the read path. That is deliberate, and it is
// the only honest thing a portable benchmark can do - there is no supported way
// to evict a file from the Windows or Linux page cache from inside a test, and
// a benchmark that pretended otherwise would be reporting whatever the cache
// happened to be doing that minute. What it means is that these are the
// CPU-bound floor of the verification cost: the best case, the freshly built
// bundle whose bytes are still in RAM. The floor is pinned down on its own by
// BenchmarkVerificationFloor, which times SHA-256 over a buffer that never goes
// near a filesystem alongside the same hash over a warm file; when those two
// agree, the warm read is free and the entire cost is hashing. On a cold store
// - the incremental rebuild a week later, which is the case that actually hurts
// - each pass additionally pays the device, and the real cost is the pool size
// divided by the smaller of the hash rate measured here and the storage read
// rate, which has to be measured for the storage in question rather than
// guessed.
//
// The data is incompressible for the same reason: a pool of zeroes would let
// NTFS compression, a sparse file, or an SSD's own dedupe make the very reads
// being measured disappear, and the benchmark would report a verification cost
// of nearly nothing that no real .deb would ever see.

// objSize is one point on the size sweep.
type objSize struct {
	name string
	size int64
}

// debSizes spans the range of .deb sizes a real bundle contains, from the
// shell-script-and-a-changelog packages that dominate by count to the kernel
// images, debug symbols and language packs that dominate by bytes. It is here
// to show where the verification cost stops being a fixed per-call overhead and
// becomes pure throughput: below a hundred kilobytes or so the open, stat and
// unlink syscalls are the whole story, and a bundle of many tiny packages pays
// a cost that does not follow from its size at all; above a megabyte the
// SHA-256 is everything and the cost is simply bytes divided by hash rate.
var debSizes = []objSize{
	{"16KiB", 16 << 10},
	{"64KiB", 64 << 10},
	{"200KiB", 200 << 10},
	{"1MiB", 1 << 20},
	{"8MiB", 8 << 20},
	{"32MiB", 32 << 20},
	{"100MiB", 100 << 20},
}

// probeSizes are the three points used where a full sweep would only repeat
// what the sweep already showed: one object small enough for syscall overhead
// to dominate, one at the reference bundle's mean object size, and one large
// enough to be pure throughput.
var probeSizes = []objSize{
	{"64KiB", 64 << 10},
	{"1MiB", 1 << 20},
	{"32MiB", 32 << 20},
}

// hardlinkOnly is Materialise with the content check taken out: MkdirAll, the
// idempotent unlink, os.Link, and nothing else. It is the baseline every
// verification figure here is measured against, because it is exactly what
// Materialise did before the check was added.
//
// The baseline has to be reconstructed rather than switched on, because the
// store deliberately offers no way to bypass verification: a flag that skips
// the check is a flag that reintroduces the vulnerability, and it would end up
// set in the one build where it mattered. Nothing outside this file may call
// this function.
func hardlinkOnly(st *fsStore, dg, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Link(st.Path(dg), dest)
}

// drain reads r to EOF through a fixed buffer. Both sides of the Open
// comparison use it so that neither picks up an io.Copy fast path the other
// does not: *os.File and the store's verifying reader do not implement the same
// optional interfaces, and letting io.Copy choose would make the comparison
// about interface dispatch rather than about hashing.
func drain(r io.Reader, buf []byte) error {
	for {
		_, err := r.Read(buf)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// sizedObject puts one object of the given size into a fresh store and returns
// the store, its digest, a pool directory under the same root - and so on the
// same volume, which is what keeps os.Link succeeding and keeps the benchmark
// on the hardlink path a real build takes - and the bytes themselves, for the
// comparisons that must not touch a filesystem at all.
func sizedObject(b *testing.B, size int64) (st *fsStore, dg, poolDir string, buf []byte) {
	b.Helper()
	root := b.TempDir()
	s, err := Open(filepath.Join(root, "store"))
	if err != nil {
		b.Fatalf("Open store: %v", err)
	}
	st = s.(*fsStore)
	poolDir = filepath.Join(root, "pool")
	if err := os.MkdirAll(poolDir, 0o755); err != nil {
		b.Fatalf("create pool dir: %v", err)
	}
	buf = make([]byte, size)
	fillIncompressible(buf, uint64(size)|1)
	dg, _, err = st.Put(context.Background(), bytes.NewReader(buf))
	if err != nil {
		b.Fatalf("Put %d-byte object: %v", size, err)
	}
	return st, dg, poolDir, buf
}

// destRingSize is how many distinct pool paths a placement benchmark rotates
// through.
//
// Placing the same object at the same path over and over is not what a build
// does - a pool has one path per package - and on NTFS it is actively
// misleading. Deleting a name and immediately recreating it serialises against
// the delete still being pending, and with an on-access virus scanner in the
// path as well the measured cost of a single unlink-and-relink came out at
// milliseconds, an order of magnitude above the placement itself and varying by
// a factor of twenty between sizes that should all have cost the same. That
// artefact would have been reported as the cost of verification. Rotating over
// a ring of names removes it without pretending the unlink is free, since each
// path in the ring is still overwritten once per lap, exactly as a rebuild
// overwrites a pool left by the previous run.
const destRingSize = 64

func destRing(dir string) []string {
	out := make([]string, destRingSize)
	for i := range out {
		out[i] = filepath.Join(dir, fmt.Sprintf("obj-%02d.deb", i))
	}
	return out
}

// timeOver runs op b.N times and reports the result as throughput over size.
// The timer is reset after the caller's setup has run, so neither the test
// data's allocation nor the Put that placed it is counted; ResetTimer clears
// the allocation counters at the same point, which is what makes the allocs/op
// column describe the operation rather than the fixture.
func timeOver(b *testing.B, size int64, op func() error) {
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := op(); err != nil {
			b.Fatalf("op: %v", err)
		}
	}
	b.StopTimer()
}

// timePlacement is timeOver for the operations that write a pool file, rotating
// the destination over destRing and clearing the ring afterwards so the next
// sub-benchmark starts from the same state this one did.
func timePlacement(b *testing.B, size int64, dests []string, place func(dest string) error) {
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := place(dests[i%len(dests)]); err != nil {
			b.Fatalf("place: %v", err)
		}
	}
	b.StopTimer()
	for _, d := range dests {
		if err := os.Remove(d); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.Fatalf("clear pool file %s: %v", d, err)
		}
	}
}

// BenchmarkMaterialiseBySize is the per-object question: what one placement
// costs with the check, against what the same placement cost without it, across
// the whole spread of .deb sizes. The gap between the two rows at each size is
// the verification cost, and the shape of that gap across the sweep is the
// interesting part - a flat overhead at the small end that a bundle of many
// tiny packages pays once per package, turning into a straight
// bytes-over-hash-rate line once objects are large.
func BenchmarkMaterialiseBySize(b *testing.B) {
	for _, sz := range debSizes {
		b.Run(sz.name, func(b *testing.B) {
			st, dg, poolDir, _ := sizedObject(b, sz.size)
			dests := destRing(poolDir)
			b.Run("link-only", func(b *testing.B) {
				timePlacement(b, sz.size, dests, func(dest string) error {
					return hardlinkOnly(st, dg, dest)
				})
			})
			b.Run("link+verify", func(b *testing.B) {
				timePlacement(b, sz.size, dests, func(dest string) error {
					return st.Materialise(dg, dest)
				})
			})
		})
	}
}

// BenchmarkVerificationFloor separates the two things a verification pass does:
// move the bytes, and hash them. hash-only never opens a file; read+hash is the
// store's own verifyFileDigest over a file the page cache is holding. The gap
// between them is what a warm read costs, and it is the measurement that says
// how far every other number here can be trusted - if the gap is small, these
// are hash-rate figures with the storage device absent from them, which is
// precisely the caveat a reader has to apply before believing them about a cold
// store.
func BenchmarkVerificationFloor(b *testing.B) {
	for _, sz := range debSizes {
		b.Run(sz.name, func(b *testing.B) {
			st, dg, _, buf := sizedObject(b, sz.size)
			objPath := st.Path(dg)
			b.Run("read+hash", func(b *testing.B) {
				timeOver(b, sz.size, func() error { return verifyFileDigest(objPath, dg) })
			})
			b.Run("hash-only", func(b *testing.B) {
				timeOver(b, sz.size, func() error {
					_, _, err := digest.SHA256Reader(bytes.NewReader(buf))
					return err
				})
			})
		})
	}
}

// BenchmarkOpenVerification prices the other verification site: Store.Open,
// which hashes on the way past rather than reading the object a second time.
// raw-read is the same file read with os.Open, which is what a caller who
// trusted the address would have done. The difference is the entire cost of
// verifying a read, and it should be CPU only - if it is not, the verifying
// reader is doing something to the read pattern that it should not be.
func BenchmarkOpenVerification(b *testing.B) {
	for _, sz := range probeSizes {
		b.Run(sz.name, func(b *testing.B) {
			st, dg, _, _ := sizedObject(b, sz.size)
			objPath := st.Path(dg)
			buf := make([]byte, 64<<10)
			b.Run("raw-read", func(b *testing.B) {
				timeOver(b, sz.size, func() error {
					f, err := os.Open(objPath)
					if err != nil {
						return err
					}
					defer f.Close()
					return drain(f, buf)
				})
			})
			b.Run("open+verify", func(b *testing.B) {
				timeOver(b, sz.size, func() error {
					rc, err := st.Open(dg)
					if err != nil {
						return err
					}
					defer rc.Close()
					return drain(rc, buf)
				})
			})
		})
	}
}

// BenchmarkPutFileAlreadyHeld prices the third site, the one that is easy to
// forget: re-ingesting a staged file the store already holds. Every rebuild
// does this for every package that arrived as a staged download, and putByDedup
// now reads the held object back before agreeing that the copy can be skipped,
// so the dedup shortcut costs two full passes over the object where it used to
// cost one. hash-source-only is that older shortcut - hash the source, see the
// address is occupied, stop.
func BenchmarkPutFileAlreadyHeld(b *testing.B) {
	ctx := context.Background()
	for _, sz := range probeSizes {
		b.Run(sz.name, func(b *testing.B) {
			st, dg, _, buf := sizedObject(b, sz.size)
			staged := filepath.Join(b.TempDir(), "staged.deb")
			if err := os.WriteFile(staged, buf, 0o644); err != nil {
				b.Fatalf("write staged file: %v", err)
			}
			b.Run("hash-source-only", func(b *testing.B) {
				timeOver(b, sz.size, func() error {
					got, _, err := sha256File(staged)
					if err != nil {
						return err
					}
					if got != dg || !st.Has(got) {
						return fmt.Errorf("staged file no longer matches %s", dg)
					}
					return nil
				})
			})
			b.Run("putfile+recheck", func(b *testing.B) {
				timeOver(b, sz.size, func() error {
					got, _, err := st.PutFile(ctx, staged, false)
					if err != nil {
						return err
					}
					if got != dg {
						return fmt.Errorf("PutFile returned %s, want %s", got, dg)
					}
					return nil
				})
			})
		})
	}
}

// debSizeSpread is one cycle of a realistic .deb size distribution, in KiB.
// Sixteen of the seventeen entries are under two megabytes, which is where the
// overwhelming majority of a Debian archive's packages sit by count, and the
// mean works out at about 534 KiB - which is the project's reference bundle, 74
// packages in roughly 40 MB. Cycling a fixed table rather than drawing from a
// distribution keeps the pool identical between runs, so two benchmark runs are
// comparable with each other.
var debSizeSpread = []int64{28, 44, 52, 68, 76, 96, 112, 140, 168, 196, 240, 320, 448, 700, 1100, 1900, 3400}

// largeDebs stand for the handful of packages that carry most of a large
// bundle's bytes - kernel images, debug symbols, texlive, a browser. They are
// what makes a multi-gigabyte bundle multi-gigabyte, and they are where a
// per-byte cost is actually felt, so a bundle shape without them would
// understate the verification cost badly.
var largeDebs = []int64{20 << 20, 45 << 20, 100 << 20}

// bundleShape describes a synthetic pool. count fixes the object count, which
// is how the reference bundle is reproduced exactly; when count is zero,
// objects are added until totalAtLeast bytes have been placed, and every
// largeEvery-th object is drawn from largeDebs instead of from the spread.
type bundleShape struct {
	name         string
	count        int
	totalAtLeast int64
	largeEvery   int
}

// bundleShapes are the two measured points the gigabyte figures rest on. The
// first is the project's own reference bundle, which is small and made of small
// objects, so it shows the per-object overhead at its worst. The second is a
// real gigabyte of pool including 100 MB packages - a measured point, not an
// extrapolated one, because the whole purpose of building it is that the answer
// for a 1 GB bundle should not have to be inferred. Only the step from there to
// five gigabytes is extrapolation, and by that size the cost is linear in bytes
// and the step is safe.
var bundleShapes = []bundleShape{
	{name: "ref-74deb-36MiB", count: 74},
	{name: "large-1GiB", totalAtLeast: 1 << 30, largeEvery: 25},
}

func (sh bundleShape) sizes() []int64 {
	var out []int64
	var sum int64
	for i := 0; ; i++ {
		if sh.count > 0 {
			if len(out) == sh.count {
				break
			}
		} else if sum >= sh.totalAtLeast {
			break
		}
		sz := debSizeSpread[i%len(debSizeSpread)] << 10
		if sh.largeEvery > 0 && i > 0 && i%sh.largeEvery == 0 {
			sz = largeDebs[(i/sh.largeEvery-1)%len(largeDebs)]
		}
		out = append(out, sz)
		sum += sz
	}
	return out
}

// verifyBundle is a store pre-loaded with a pool-shaped set of objects, plus
// destination paths for them under the same root, so that placements take the
// hardlink path a real build takes.
type verifyBundle struct {
	st      *fsStore
	digests []string
	dests   []string
	total   int64
}

func newVerifyBundle(b *testing.B, sh bundleShape) *verifyBundle {
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
	vb := &verifyBundle{st: s.(*fsStore)}
	// One buffer, refilled per object. Every object must have distinct content,
	// or two of them would share a digest and collapse into a single stored
	// file, quietly shrinking the pool being measured; holding a gigabyte of
	// distinct pool in memory to achieve that would measure the allocator.
	var buf []byte
	for i, size := range sh.sizes() {
		if int64(cap(buf)) < size {
			buf = make([]byte, size)
		}
		buf = buf[:size]
		fillIncompressible(buf, uint64(i)+1)
		dg, n, err := vb.st.Put(context.Background(), bytes.NewReader(buf))
		if err != nil {
			b.Fatalf("Put object %d (%d bytes): %v", i, size, err)
		}
		vb.digests = append(vb.digests, dg)
		vb.dests = append(vb.dests, filepath.Join(poolDir, fmt.Sprintf("obj-%04d.deb", i)))
		vb.total += n
	}
	return vb
}

// run times one placement strategy over the whole pool and reports three
// figures, because ns/op over an entire pool is not a number anybody can act
// on. SetBytes gives the pool's throughput; ns/object gives the per-package
// cost a bundle of many small packages is really paying; and s/GiB is the
// figure to multiply by a real bundle's size to predict what that bundle will
// cost, which is the question this whole file exists to answer.
func (vb *verifyBundle) run(b *testing.B, place func(dg, dest string) error) {
	b.SetBytes(vb.total)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j, dg := range vb.digests {
			if err := place(dg, vb.dests[j]); err != nil {
				b.Fatalf("place %s: %v", dg, err)
			}
		}
	}
	b.StopTimer()
	objects := float64(b.N) * float64(len(vb.digests))
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/objects, "ns/object")
	gib := float64(b.N) * float64(vb.total) / float64(1<<30)
	b.ReportMetric(b.Elapsed().Seconds()/gib, "s/GiB")
	vb.clear(b)
}

func (vb *verifyBundle) clear(b *testing.B) {
	b.Helper()
	for _, d := range vb.dests {
		if err := os.Remove(d); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.Fatalf("clear pool file %s: %v", d, err)
		}
	}
}

func (vb *verifyBundle) placeUnverified(dg, dest string) error {
	return hardlinkOnly(vb.st, dg, dest)
}

func (vb *verifyBundle) placeVerified(dg, dest string) error {
	return vb.st.Materialise(dg, dest)
}

// placeAsShipped is the full per-object sequence core/bundle's
// materialiseSelections runs for a selection already held in the store: read
// the object back out and hash it (ensureInStore), Materialise it, which
// re-hashes what it wrote, then hash the placed file once more so the property
// does not depend on which Store implementation was passed in. Three full
// passes over every byte. It mirrors benchPool.assembleAfter deliberately -
// same sequence, run over a pool with a realistic size distribution rather than
// uniform objects - and if materialiseSelections changes shape, both have to
// follow it or they stop describing the real code.
func (vb *verifyBundle) placeAsShipped(dg, dest string) error {
	if err := verifyStoreObjectLikeBundle(vb.st, dg); err != nil {
		return err
	}
	if err := vb.st.Materialise(dg, dest); err != nil {
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

// BenchmarkBundleVerificationCost is the whole point of the file: a realistic
// pool, placed three ways. no-verification is what a build cost before the
// check existed; store-verify is what the store alone now charges;
// as-shipped-3-passes is what a build actually pays, because core/bundle adds
// two more passes of its own on top. The s/GiB metric from the large shape
// multiplied by a bundle's size answers "what will this cost me", and the
// ns/object metric from the reference shape answers it for a bundle whose
// packages are all small.
func BenchmarkBundleVerificationCost(b *testing.B) {
	for _, sh := range bundleShapes {
		b.Run(sh.name, func(b *testing.B) {
			vb := newVerifyBundle(b, sh)
			b.Logf("pool: %d objects, %.1f MiB total", len(vb.digests), float64(vb.total)/(1<<20))
			b.Run("no-verification", func(b *testing.B) { vb.run(b, vb.placeUnverified) })
			b.Run("store-verify", func(b *testing.B) { vb.run(b, vb.placeVerified) })
			b.Run("as-shipped-3-passes", func(b *testing.B) { vb.run(b, vb.placeAsShipped) })
		})
	}
}
