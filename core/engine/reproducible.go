package engine

import (
	"reflect"
	"strings"

	"github.com/inferops/debark/core/lock"
)

// Placeholders written in place of the two host locations this build knows
// are not part of the request. They follow the idiom core/apt already
// established for the same job (sanitizeAPTOptionsForLock's "<PRIVATE-ROOT>",
// closedWorldMasks' "<CLOSED-WORLD-WORKDIR>"): keep the shape of the string,
// replace only the varying prefix, so what is left still reads like the value
// it stands in for.
const (
	workRootPlaceholder  = "<BUILD-WORKDIR>"
	bundleDirPlaceholder = "<BUNDLE-DIR>"
)

// makeLockReproducible is the engine's last, single act of ownership over
// lock.json before it is validated and written: it removes from the finished
// lock the two kinds of value that are properties of THIS MACHINE AT THIS
// MOMENT rather than of the request, and which therefore must not reach a
// file whose digest becomes the bundle id.
//
// It runs once, at the top of finalizeBundle, after doctor, the closed-world
// check and the unsigned-warning fold have all landed — i.e. after the last
// thing that can add to the lock, and before the single rewrite that makes
// it final. Everything downstream (README.txt, which is rendered from this
// lock; LockDigest; manifest.BundleID; the signature over that manifest)
// inherits the result for free.
//
// # 1. Host paths, scrubbed
//
// The engine is the only writer of lock.json (see finalizeBundle's own doc
// comment, which makes the same argument for lock.Validate), and the lock is
// assembled from values several collaborators produced: the backend's
// Resolver block, the backend's ClosedWorld result, warnings raised inside
// core/apt's private-root construction, apt's own text in Unresolved.Detail,
// and — on the path this fixes — err.Error() from a backend call that failed
// outright, folded in by runClosedWorld as ClosedWorld.Detail.
//
// Each of those collaborators is expected to keep host paths out of what it
// hands back (core/apt's sanitizeAPTOptionsForLock and closedWorldMasks both
// do exactly that), but "expected to" is a convention, and apt.Backend is an
// interface with more than one implementation and no way to enforce one. The
// path that is different on every single run is b.workRoot — an
// os.MkdirTemp("", "debark-build-*") directory the caller cannot even name
// — and it is the engine, not any backend, that created it and knows what it
// is. So the engine scrubs it, here, once, from everything in the lock. Same
// for b.bundleDir, which is Output.Path: a host location by the same rule
// that already keeps it out of RequestDigest (see requestDigestInput).
//
// This is defence in depth, not a replacement for the collaborator-side
// masking: a value that has already been masked upstream contains no
// occurrence of either path, so this pass is a no-op on it. What it buys is
// that a backend which forgets — or a new backend, or a new error path —
// cannot silently reintroduce the leak this project has already paid for
// twice (Resolver.APTOptions, then ClosedWorld.CommandDigest/OutputDigest).
//
// # 2. Stats.DownloadedBytes, dropped
//
// See stripCacheDependentStats.
func (b *build) makeLockReproducible() {
	if b.lockDoc == nil {
		return
	}
	scrubHostPaths(b.lockDoc, map[string]string{
		b.workRoot:  workRootPlaceholder,
		b.bundleDir: bundleDirPlaceholder,
	})
	b.stripCacheDependentStats()
}

// stripCacheDependentStats clears lock.Stats.DownloadedBytes.
//
// DownloadedBytes counts the bytes of this run's selections that the content
// store did not already hold (core/engine/resolve.go's ingestPlan, and
// core/bundle's own equivalent accounting). The store is a persistent,
// machine-level cache, shared by every build on the host and populated by
// every previous one; whether it happens to already hold a given .deb is a
// fact about the machine, never about the request. Recording it in lock.json
// made bundle identity depend on it: the same `debark build` run twice
// produced downloaded_bytes N and then 0, which is different lock.json bytes,
// a different LockDigest, a different BundleID and a different README.txt —
// design section 3.13's byte-identical-rebuild claim broken by nothing more
// than having built the bundle once before.
//
// This follows the precedent already set for the one other statistic with the
// same problem: Stats.DurationSeconds measures the wall clock this process
// took, is reported in buildjob.BuildResult, and was deliberately never given
// a home in lock.Stats. DownloadedBytes is reported the same way — the CLI
// prints it from BuildResult.Stats (internal/cli/cmd_build.go), where it is
// still exact — and is simply not a fact the bundle records about itself.
//
// Note the contrast with Added/Removed/Unchanged, which are NOT cleared and
// are not the same problem: those are computed against the previous contents
// of Output.Path, which an auditor rebuilding from scratch has by definition
// empty, so they are reproducible for the rebuild case the determinism claim
// is actually about. The store is the opposite: an auditor cannot empty it,
// cannot be asked to, and should not have to.
//
// SCHEMA NOTE: lock.json still carries the "downloaded_bytes" key (it has no
// omitempty and core/lock is not this package's to change), but its value is
// now always 0. docs/formats.md section 3.2 still describes stats as
// "added/removed/unchanged/bytes/downloaded_bytes for this run" and its
// example lock shows a non-zero value; that text needs a decision from
// whoever owns the format doc.
func (b *build) stripCacheDependentStats() {
	b.lockDoc.Stats.DownloadedBytes = 0
}

// scrubHostPaths replaces every occurrence of each key of replacements with
// its value, in every string reachable from l.
//
// It walks with reflection rather than naming the four fields that carry
// collaborator text today (Resolver.APTOptions, ClosedWorld.Detail,
// Warnings[].Message, Unresolved[].Detail) for the same reason
// requestDigestInput is its own named type: the failure mode this exists to
// prevent is a NEW field, added later by someone who never read this comment,
// quietly carrying a temporary path into the bundle id. A field list would
// have to be kept in sync by hand and would be wrong the first time it was
// not. A walk cannot be forgotten.
//
// An empty key is skipped: b.workRoot and b.bundleDir are both non-empty by
// the time finalizeBundle runs, but "" would otherwise match everywhere.
func scrubHostPaths(l *lock.Lock, replacements map[string]string) {
	repl := make([][2]string, 0, len(replacements))
	for from, to := range replacements {
		if from == "" {
			continue
		}
		repl = append(repl, [2]string{from, to})
	}
	if len(repl) == 0 {
		return
	}
	scrubStrings(reflect.ValueOf(l), func(s string) string {
		for _, r := range repl {
			s = strings.ReplaceAll(s, r[0], r[1])
		}
		return s
	})
}

// scrubStrings applies fn to every settable string reachable from v.
// Unexported fields are skipped rather than panicked on (CanSet is false for
// them); lock.Lock has none today, and a future one would simply be left
// alone rather than crashing a build.
func scrubStrings(v reflect.Value, fn func(string) string) {
	switch v.Kind() {
	case reflect.String:
		if !v.CanSet() {
			return
		}
		if s := fn(v.String()); s != v.String() {
			v.SetString(s)
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			scrubStrings(v.Elem(), fn)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			scrubStrings(v.Field(i), fn)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			scrubStrings(v.Index(i), fn)
		}
	case reflect.Map:
		// Map elements are never addressable, so a nested string has to be
		// rewritten through SetMapIndex. lock.Lock has no map fields today;
		// this arm exists so that adding one does not silently create a hole
		// in the walk.
		for _, k := range v.MapKeys() {
			mv := v.MapIndex(k)
			if mv.Kind() != reflect.String {
				continue
			}
			if s := fn(mv.String()); s != mv.String() {
				v.SetMapIndex(k, reflect.ValueOf(s).Convert(mv.Type()))
			}
		}
	}
}
