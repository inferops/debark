package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
)

// fsStore is the filesystem-backed Store implementation.
//
// # Concurrency discipline
//
// The content-addressed objects under <root>/sha256/<hh>/<digest> need no
// locking: every write goes to a private temp file first (fsync'd) and is
// published with a single rename, so two writers racing to add the same
// digest either both succeed (content-addressed, so whichever rename wins
// last is byte-identical to the other) or one simply finds the object
// already there. There is nothing to coordinate.
//
// index.json is different: it is one shared file that several processes may
// want to update (Record, GC). It is protected by a lock file,
// <root>/index.lock, acquired with an atomic O_CREATE|O_EXCL create and
// released by removing it. A lock older than lockStaleAfter is assumed to
// belong to a crashed process and is stolen. This is a single-writer
// discipline: readers (Index) never take the lock, because the same
// temp-file-then-rename publication used for index.json itself guarantees a
// reader only ever observes a complete file.
type fsStore struct {
	root string
}

const (
	lockFileName   = "index.lock"
	lockStaleAfter = 30 * time.Second
	lockRetryDelay = 20 * time.Millisecond
	lockWaitBudget = 10 * time.Second
)

// shardDir returns the two-hex-character shard directory a digest lives
// under. dg must already have passed digest.Valid, which is what keeps the
// shard name (and the file name below it) a pair of hex characters rather
// than an arbitrary path fragment.
func shardDir(root, dg string) string {
	return filepath.Join(root, "sha256", dg[:2])
}

// address normalises a caller-supplied digest into a store address, or
// reports that it is not one at all. Every entry point runs it first: a store
// address is always a lowercase hex SHA-256, and without that check "digest"
// is just an unvalidated path fragment appended to the store root, so
// "../../etc/shadow" would name a real file that Has confirms, Open reads and
// Materialise copies into the bundle the operator then signs.
func address(dg string) (string, bool) {
	dg = strings.ToLower(dg)
	return dg, digest.Valid(dg)
}

func (s *fsStore) Root() string { return s.root }

// Path returns "" for anything that is not a store address, so a malformed
// digest can never resolve to a path at all, let alone one outside the root.
func (s *fsStore) Path(dg string) string {
	dg, ok := address(dg)
	if !ok {
		return ""
	}
	return filepath.Join(shardDir(s.root, dg), dg)
}

func (s *fsStore) Has(dg string) bool {
	p := s.Path(dg)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func (s *fsStore) Open(dg string) (io.ReadCloser, error) {
	dg, ok := address(dg)
	if !ok {
		return nil, dferr.New(dferr.Usage, "store: open object: %q is not a sha256 digest", dg)
	}
	f, err := os.Open(s.Path(dg))
	if err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "store: open object %s", dg)
	}
	return &verifyingReader{f: f, want: dg, h: sha256.New()}, nil
}

// verifyingReader hashes an object on its way out and refuses to report a
// clean EOF unless the bytes really hash to the address they were stored
// under. Verifying in the same pass costs no extra read; the cost is that a
// caller who stops early gets no verification, which is why Materialise
// checks the file it wrote instead of relying on this.
type verifyingReader struct {
	f       *os.File
	want    string
	h       hash.Hash
	checked bool
}

func (r *verifyingReader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	if n > 0 {
		r.h.Write(p[:n])
	}
	if errors.Is(err, io.EOF) && !r.checked {
		r.checked = true
		if got := hex.EncodeToString(r.h.Sum(nil)); got != r.want {
			return n, dferr.New(dferr.Verification,
				"store: object %s holds content that hashes to %s", r.want, got)
		}
	}
	return n, err
}

func (r *verifyingReader) Close() error { return r.f.Close() }

// verifyFileDigest re-reads path and reports whether its bytes hash to want.
// It is the check that makes this store content-addressed rather than
// content-addressed-by-filename-convention: the object tree is ordinary
// files, so anything able to write one of them can park its own bytes behind
// a digest apt vouched for, and every consumer downstream - the pool, the
// Packages index, the signed manifest - would then agree with each other
// about the attacker's bytes.
func verifyFileDigest(path, want string) error {
	got, _, err := sha256File(path)
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "store: hash %s", path)
	}
	if got != want {
		return dferr.New(dferr.Verification,
			"store: %s holds content that hashes to %s, not %s", path, got, want)
	}
	return nil
}

func (s *fsStore) tmpDir() string { return filepath.Join(s.root, "sha256", "tmp") }

// Put streams r into the store while hashing it, so the digest is only known
// once every byte has been written. The bytes land in a temp file under
// <root>/sha256/tmp first (same volume as the sharded directories, so the
// final rename is a same-filesystem, atomic publish); fsync runs before the
// rename so a partial object is never observable, and a crash mid-write
// leaves only an orphaned temp file, never a corrupt object at digest's
// path.
func (s *fsStore) Put(ctx context.Context, r io.Reader) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(s.tmpDir(), 0o755); err != nil {
		return "", 0, dferr.Wrap(dferr.Environment, err, "store: create staging directory")
	}
	tmp, err := os.CreateTemp(s.tmpDir(), "obj-*")
	if err != nil {
		return "", 0, dferr.Wrap(dferr.Environment, err, "store: create temp file")
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		tmp.Close()
		return "", 0, dferr.Wrap(dferr.Environment, err, "store: write object")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", 0, dferr.Wrap(dferr.Environment, err, "store: sync object")
	}
	if err := tmp.Close(); err != nil {
		return "", 0, dferr.Wrap(dferr.Environment, err, "store: close object")
	}

	dg := hex.EncodeToString(h.Sum(nil))
	final := s.Path(dg)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", 0, dferr.Wrap(dferr.Environment, err, "store: create shard directory")
	}
	if err := os.Rename(tmpName, final); err != nil {
		if s.Has(dg) {
			// Another writer published the identical content first: content
			// addressing means this is success, not a conflict.
			return dg, n, nil
		}
		return "", 0, dferr.Wrap(dferr.Environment, err, "store: publish object %s", dg)
	}
	removeTmp = false
	return dg, n, nil
}

// PutFile ingests an existing file.
//
// It never publishes bytes it has not itself hashed. Hashing the caller's
// path and then re-reading that same path to copy it - two separate reads of
// a name the caller (and anyone else) can still write to - leaves a window in
// which the source changes between the two, and the store ends up holding
// content at an address it does not hash to. That is exactly the state a
// planted object produces, reached without any write into the store at all.
//
// So the copy path hashes while it writes (Put), and the move path renames
// the source into the store's own staging area before hashing it, which keeps
// the no-copy fast path for a same-filesystem source while making the hash
// describe the bytes that actually get published.
func (s *fsStore) PutFile(ctx context.Context, path string, moveOK bool) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if moveOK {
		dg, size, moved, err := s.putByMove(path)
		if err != nil {
			return "", 0, err
		}
		if moved {
			return dg, size, nil
		}
		// Cross-device or otherwise unrenameable (common on Windows when the
		// source and store live on different drives): fall through to copy.
	} else if dg, size, ok := s.putByDedup(path); ok {
		return dg, size, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", 0, dferr.Wrap(dferr.Environment, err, "store: open %s", path)
	}
	defer f.Close()
	dg, size, err := s.Put(ctx, f)
	if err != nil {
		return "", 0, err
	}
	if moveOK {
		// The source has already been copied into the store and the digest
		// returned describes the bytes that were written, so a failed unlink
		// costs a stray file on the caller's side and nothing else. Reporting
		// it would turn a completed ingest into a failed one.
		_ = os.Remove(path)
	}
	return dg, size, nil
}

// putByDedup is the "we already hold this" shortcut for the copying path: it
// hashes the source, and if an object is already filed under that address it
// re-reads that object and checks it before agreeing to skip the copy.
// Presence at an address is not evidence about content, so skipping the copy
// on os.Stat alone would let a planted object stand in for a genuine
// download - and, when the caller passed moveOK, see the genuine download
// deleted as a redundant duplicate of it.
func (s *fsStore) putByDedup(path string) (string, int64, bool) {
	dg, size, err := sha256File(path)
	if err != nil {
		return "", 0, false // let the copying path report the real error
	}
	if !s.Has(dg) {
		return "", 0, false
	}
	if err := verifyFileDigest(s.Path(dg), dg); err != nil {
		// Held content is not what its address says. Ingest for real; content
		// addressing means the bytes that hash to dg are the only bytes
		// entitled to that name, so the copy repairs the object.
		return "", 0, false
	}
	return dg, size, true
}

// putByMove is PutFile's no-copy fast path. It renames the source into the
// staging area first and hashes it there: once the file is under a name only
// this call knows, nothing can swap its content between the hash and the
// publish. (An attacker who can already write inside the store root can still
// interfere, but such an attacker owns the store either way.)
//
// moved is false with a nil error when the source simply cannot be renamed
// into the store - a different filesystem, the usual case on Windows when the
// download and the store live on different drives - which tells PutFile to
// copy instead. Once the source has been renamed away, any later failure is
// returned rather than swallowed, so a caller is never told to fall back to
// copying a file that is no longer there.
func (s *fsStore) putByMove(path string) (string, int64, bool, error) {
	if err := os.MkdirAll(s.tmpDir(), 0o755); err != nil {
		return "", 0, false, dferr.Wrap(dferr.Environment, err, "store: create staging directory")
	}
	tmp, err := os.CreateTemp(s.tmpDir(), "mv-*")
	if err != nil {
		return "", 0, false, dferr.Wrap(dferr.Environment, err, "store: create temp file")
	}
	tmpName := tmp.Name()
	tmp.Close()
	if err := os.Remove(tmpName); err != nil {
		return "", 0, false, dferr.Wrap(dferr.Environment, err, "store: clear temp file")
	}
	if err := os.Rename(path, tmpName); err != nil {
		return "", 0, false, nil
	}
	restore := func() {
		if err := os.Rename(tmpName, path); err != nil {
			_ = os.Remove(tmpName)
		}
	}

	dg, size, err := sha256File(tmpName)
	if err != nil {
		restore()
		return "", 0, false, dferr.Wrap(dferr.Environment, err, "store: hash %s", path)
	}
	final := s.Path(dg)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		restore()
		return "", 0, false, dferr.Wrap(dferr.Environment, err, "store: create shard directory")
	}
	if err := os.Rename(tmpName, final); err != nil {
		restore()
		return "", 0, false, dferr.Wrap(dferr.Environment, err, "store: publish object %s", dg)
	}
	return dg, size, true, nil
}

// Materialise hardlinks the object into dest, falling back to a copy when a
// hardlink is impossible (different filesystem, a filesystem that does not
// support hardlinks, or a Windows-specific restriction). It never symlinks:
// a bundle must keep working after being copied wholesale to other media,
// which a symlink into this store would not survive.
//
// It then re-hashes what landed at dest. That is a full extra read of every
// object placed, and it is unconditional on purpose: this is the one moment
// the store's central claim - these bytes are the bytes that hash to this
// digest - is cheap to check and catastrophic to skip. Everything downstream
// re-derives its hashes from the file Materialise wrote, so a substitution
// here produces a bundle that is internally consistent about the wrong
// content and passes verify. Checking dest rather than the source object also
// covers the copy fallback, and covers a hard-linked object rewritten in place
// through a bundle directory, which is far more exposed than the store.
func (s *fsStore) Materialise(dg, dest string) error {
	dg, ok := address(dg)
	if !ok {
		return dferr.New(dferr.Usage, "store: materialise: %q is not a sha256 digest", dg)
	}
	src := s.Path(dg)
	if _, err := os.Stat(src); err != nil {
		return dferr.Wrap(dferr.Environment, err, "store: materialise %s: object not found", dg)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return dferr.Wrap(dferr.Environment, err, "store: materialise %s: create destination directory", dg)
	}
	// Idempotent overwrite: a previous run's file (or a stale link) never
	// lingers under whatever Materialise writes next.
	_ = os.Remove(dest)

	if err := os.Link(src, dest); err != nil {
		if err := copyToPath(src, dest); err != nil {
			return dferr.Wrap(dferr.Environment, err, "store: materialise %s", dg)
		}
	}
	if err := verifyFileDigest(dest, dg); err != nil {
		// Leave nothing behind for a caller that ignores the error: an
		// unverified file at a pool path is the whole problem.
		_ = os.Remove(dest)
		return dferr.Wrap(dferr.Verification, err, "store: materialise %s", dg)
	}
	return nil
}

func (s *fsStore) indexPath() string { return filepath.Join(s.root, IndexFileName) }

func (s *fsStore) Index() (Index, error) {
	b, err := os.ReadFile(s.indexPath())
	if errors.Is(err, os.ErrNotExist) {
		return Index{SchemaVersion: IndexSchemaVersion}, nil
	}
	if err != nil {
		return Index{}, dferr.Wrap(dferr.Environment, err, "store: read index")
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return Index{}, dferr.Wrap(dferr.Environment, err, "store: parse index")
	}
	if idx.SchemaVersion == "" {
		idx.SchemaVersion = IndexSchemaVersion
	}
	return idx, nil
}

// writeIndexLocked writes idx to index.json atomically. The caller must
// already hold the index lock.
func (s *fsStore) writeIndexLocked(idx Index) error {
	idx.SchemaVersion = IndexSchemaVersion
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Digest < idx.Entries[j].Digest })

	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "store: encode index")
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(s.root, ".index-*.json")
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "store: create temp index")
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return dferr.Wrap(dferr.Environment, err, "store: write temp index")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return dferr.Wrap(dferr.Environment, err, "store: sync temp index")
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return dferr.Wrap(dferr.Environment, err, "store: close temp index")
	}
	if err := os.Rename(tmpName, s.indexPath()); err != nil {
		_ = os.Remove(tmpName)
		return dferr.Wrap(dferr.Environment, err, "store: publish index")
	}
	return nil
}

func (s *fsStore) Record(entry Entry) error {
	if entry.Digest == "" {
		return dferr.New(dferr.Usage, "store: Record: empty digest")
	}
	unlock, err := s.lockIndex()
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "store: record %s", entry.Digest)
	}
	defer unlock()

	idx, err := s.Index()
	if err != nil {
		return err
	}
	if entry.AddedAt == "" {
		entry.AddedAt = canonical.Time(time.Now())
	}
	replaced := false
	for i := range idx.Entries {
		if idx.Entries[i].Digest == entry.Digest {
			idx.Entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		idx.Entries = append(idx.Entries, entry)
	}
	return s.writeIndexLocked(idx)
}

// GC visits every object once, keeps it or removes it according to keep, and
// drops any index entries for what it removed. It does not lock the object
// tree itself: running GC concurrently with a build that is still Putting
// objects is not supported, the same way `git gc` is not safe to run
// alongside another writer — keep should be computed from a stable snapshot
// of what is still referenced before GC starts.
func (s *fsStore) GC(ctx context.Context, keep func(dg string) bool) (GCStats, error) {
	var stats GCStats
	shaDir := filepath.Join(s.root, "sha256")
	if _, err := os.Stat(shaDir); errors.Is(err, os.ErrNotExist) {
		return stats, nil
	}

	shards, err := os.ReadDir(shaDir)
	if err != nil {
		return stats, dferr.Wrap(dferr.Environment, err, "store: gc: read %s", shaDir)
	}

	removedDigests := map[string]bool{}
	for _, shard := range shards {
		if !shard.IsDir() || shard.Name() == "tmp" {
			continue
		}
		shardPath := filepath.Join(shaDir, shard.Name())
		objs, err := os.ReadDir(shardPath)
		if err != nil {
			return stats, dferr.Wrap(dferr.Environment, err, "store: gc: read %s", shardPath)
		}
		for _, obj := range objs {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			if obj.IsDir() || strings.HasPrefix(obj.Name(), ".") {
				continue
			}
			dg := obj.Name()
			info, err := obj.Info()
			var size int64
			if err == nil {
				size = info.Size()
			}
			if keep(dg) {
				stats.Kept++
				stats.BytesRemaining += size
				continue
			}
			if err := os.Remove(filepath.Join(shardPath, dg)); err != nil {
				return stats, dferr.Wrap(dferr.Environment, err, "store: gc: remove %s", dg)
			}
			stats.Removed++
			stats.BytesFreed += size
			removedDigests[dg] = true
		}
	}

	if len(removedDigests) == 0 {
		return stats, nil
	}
	unlock, err := s.lockIndex()
	if err != nil {
		// The objects are already gone; a stale index entry is a cosmetic
		// problem, not a correctness one, so report the GC as done but
		// surface the index failure to the caller.
		return stats, dferr.Wrap(dferr.Environment, err, "store: gc: lock index")
	}
	defer unlock()
	idx, err := s.Index()
	if err != nil {
		return stats, err
	}
	kept := idx.Entries[:0]
	for _, e := range idx.Entries {
		if !removedDigests[e.Digest] {
			kept = append(kept, e)
		}
	}
	idx.Entries = kept
	if err := s.writeIndexLocked(idx); err != nil {
		return stats, err
	}
	return stats, nil
}

// lockIndex acquires the single-writer lock on index.json, stealing a stale
// lock (older than lockStaleAfter, presumed abandoned by a crashed process)
// if one is found. It gives up after lockWaitBudget.
func (s *fsStore) lockIndex() (unlock func(), err error) {
	lockPath := filepath.Join(s.root, lockFileName)
	deadline := time.Now().Add(lockWaitBudget)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			// Deliberately unchecked, and deliberately still reported by the
			// lint config rather than excluded by name: this is the only
			// fmt.Fprintf in core/ that writes to a real file, and the rule
			// that catches it is worth keeping for the next one. The reason
			// it is ignored is that the payload is diagnostic only - it
			// exists so an operator who finds a stale index.lock can see
			// which process left it. The lock is already held by the time
			// this line runs: mutual exclusion comes entirely from the
			// atomic O_CREATE|O_EXCL above and staleness from the file's
			// mtime, neither of which depends on a byte of the content, and
			// nothing in the tree ever reads it back. Propagating a failure
			// would mean releasing a lock we successfully took and retrying,
			// in order to report a condition - a failed 40-byte write to a
			// file created microseconds earlier - that the very next index
			// write reports with a better message anyway.
			_, _ = fmt.Fprintf(f, "pid=%d locked_at=%s\n", os.Getpid(), canonical.Time(time.Now()))
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		// Windows reports a lock file that is still in a delete-pending state
		// as "access is denied" rather than "already exists", so a lock
		// released a moment ago reads as a hard error on the next acquirer.
		// Both mean the same thing here - someone else has it, wait and
		// retry - and a genuine permission problem still surfaces, as the
		// wait-budget timeout below.
		if !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("store: create lock %s: %w", lockPath, err)
		}
		if fi, statErr := os.Stat(lockPath); statErr == nil {
			if time.Since(fi.ModTime()) > lockStaleAfter {
				_ = os.Remove(lockPath) // best-effort steal of an abandoned lock
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("store: index locked: timed out waiting for %s", lockPath)
		}
		time.Sleep(lockRetryDelay)
	}
}

// sha256File hashes a file without holding its full content in memory.
func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// copyToPath copies src to a temp file in dest's own directory, fsyncs it,
// and renames it into place, so a reader can never observe a partially
// written dest.
func copyToPath(src, dest string) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dferr.Wrap(dferr.Environment, err, "store: create %s", dir)
	}
	in, err := os.Open(src)
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "store: open %s", src)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return dferr.Wrap(dferr.Environment, err, "store: create temp file in %s", dir)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return dferr.Wrap(dferr.Environment, err, "store: copy to %s", tmpName)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return dferr.Wrap(dferr.Environment, err, "store: sync %s", tmpName)
	}
	if err := tmp.Close(); err != nil {
		return dferr.Wrap(dferr.Environment, err, "store: close %s", tmpName)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return dferr.Wrap(dferr.Environment, err, "store: publish %s", dest)
	}
	removeTmp = false
	return nil
}
