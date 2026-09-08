package bundle

import "pault.ag/go/debian/version"

// pruneKey groups pool entries the way prune decides supersession: only the
// (name, arch) pair matters, versions within it are compared against each
// other.
type pruneKey struct{ name, arch string }

// pruneDecide is the actual pure logic behind Prune, factored out so it can
// be exercised with hand-built []PoolEntry values independent of any
// filesystem state.
//
// Per (name, arch), it keeps the dpkg-highest version (pault.ag/go/debian/
// version.Compare, so epochs and the "~" pre-release ordering are exactly
// what dpkg itself would decide) and returns every other entry in that group
// as removable. UserSupplied and LockReferenced entries both count toward
// what "highest" means - either can supersede an archive version and cause
// it to be pruned - but neither is ever itself returned for removal, however
// low its version compares.
//
// LockReferenced protects the version the caller's own plan selected for
// that (name, arch): the pool may still carry a numerically higher sibling
// left behind by an earlier, non-pruning run (the default additive mode),
// and a target pin can legitimately make the plan's own choice the lower of
// the two (docs/experiments/E3-pin-fidelity.md). Without this, a pure
// numeric rule would remove the file the lock this build writes is about to
// name, before install is ever consulted - the same class of failure E3's
// Finding 1 identified on the upgrade path, resolved there by the same
// principle (ADR-007: never let a free re-resolution override what the lock
// already decided).
//
// An entry whose Version does not parse is left alone entirely: it is never
// removed, and it never influences what counts as the highest version for
// its group. Pruning must never guess at data it cannot understand.
func pruneDecide(files []PoolEntry) []PoolEntry {
	parsed := make([]version.Version, len(files))
	valid := make([]bool, len(files))
	best := make(map[pruneKey]version.Version, len(files))

	for i, f := range files {
		v, err := version.Parse(f.Version)
		if err != nil {
			continue
		}
		parsed[i] = v
		valid[i] = true

		k := pruneKey{f.Name, f.Arch}
		if cur, ok := best[k]; !ok || version.Compare(v, cur) > 0 {
			best[k] = v
		}
	}

	var remove []PoolEntry
	for i, f := range files {
		if !valid[i] || f.UserSupplied || f.LockReferenced {
			continue
		}
		k := pruneKey{f.Name, f.Arch}
		if version.Compare(parsed[i], best[k]) < 0 {
			remove = append(remove, f)
		}
	}
	return remove
}
