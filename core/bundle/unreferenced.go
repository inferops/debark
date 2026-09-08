package bundle

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/repository"
	"github.com/inferops/debark/core/resolve"
)

// Reporting for pool files that cross the air gap without being installable.
//
// A run is additive by default, and even under --update prune never
// removes the highest version of a (name, arch) group. Two entirely ordinary
// sequences therefore leave a .deb in repo/pool that this run's repo/Packages
// does not index:
//
//   - the target's pins select a LOWER version than a leftover already in the
//     pool. The plan's own file is protected by PoolEntry.LockReferenced and
//     the higher leftover is its group's numeric best, so pruneDecide keeps
//     both, deliberately (docs/experiments/E3-pin-fidelity.md, "Prune
//     resolution").
//   - an earlier run's request named a package this run's does not. Nothing
//     supersedes it, so it survives whatever the prune flag says.
//
// Both produce the same shape, and it is a shape with two different answers
// to "what crossed the gap": the file is on the media and covered by the
// manifest (manifest.Build walks the whole bundle directory) and therefore by
// the operator's signature, but it is absent from repo/Packages, which is
// written from this run's selections alone, and absent from
// last-run-removed.txt, because it was not removed. The target's apt cannot
// see or select it - that is the property that actually matters, and it holds
// - but an auditor reading Packages or lock.json is told the media holds
// strictly less than it does.
//
// This file closes that reporting gap and only that gap. It changes nothing
// about what prune removes; the LockReferenced rule and its reasoning are
// settled.

// maxUnreferencedNamed caps how many files the lock warning's message
// enumerates. The message is a sentence an operator reads in build output and
// in README.txt, while last-run-unreferenced.txt is where the complete list
// lives - so the cap costs an auditor nothing, and keeps a bundle that
// accumulated a hundred leftovers from turning one warning into a listing.
const maxUnreferencedNamed = 5

// unreferencedPoolFiles lists every file left in the pool that repo/Packages
// does not index, repo-relative and sorted, exactly like the paths in
// last-run-added.txt and last-run-removed.txt.
//
// indexed is the same []repository.PoolFile handed to the repository writer,
// so "not in this set" is by construction "not in Packages" rather than a
// second, independently derived opinion about what got indexed.
//
// It must run after prune: a file prune deleted is gone from the pool and
// already accounted for in last-run-removed.txt, so the walk simply does not
// see it. And it deliberately reports every file, not only *.deb - anything
// sitting in the pool is on the media and covered by the manifest, whatever
// its extension, and the whole point here is that the pool and Packages
// disagree.
func unreferencedPoolFiles(repoDir string, indexed []repository.PoolFile) ([]string, error) {
	inIndex := make(map[string]bool, len(indexed))
	for _, f := range indexed {
		inIndex[f.Path] = true
	}

	poolRoot := filepath.Join(repoDir, "pool")
	if _, err := os.Stat(poolRoot); os.IsNotExist(err) {
		return nil, nil
	}

	var out []string
	walkErr := filepath.WalkDir(poolRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(repoDir, p)
		if rerr != nil {
			return rerr
		}
		relSlash := filepath.ToSlash(rel)
		if !inIndex[relSlash] {
			out = append(out, relSlash)
		}
		return nil
	})
	if walkErr != nil {
		return nil, dferr.Wrap(dferr.Environment, walkErr, "bundle: scan %s", poolRoot)
	}
	sort.Strings(out)
	return out, nil
}

// unreferencedWarning renders the lock.Warning for a non-empty unreferenced
// set, and nil when there is nothing to report.
//
// It exists in addition to last-run-unreferenced.txt, not instead of it,
// because the two reach different readers. The file is the complete,
// diffable record, and it sits in the bundle root next to its
// last-run-added.txt / last-run-removed.txt siblings - where nobody looks
// unless they already suspect something is there. A lock.Warning is carried
// into lock.json, rendered into README.txt's Warnings section (readme.go),
// printed by `debark build` and listed by `debark inspect` - every
// surface an operator or auditor actually reads, without having to know this
// failure mode exists first. Neither alone answers both "was I told?" and
// "told what, exactly?".
//
// What it claims about each file is limited to what is provably true. The
// pool path is authoritative: it is how the manifest and both existing
// last-run reports already name a file. Whether the same package is indexed
// at some OTHER version is decided by pool directory, because
// repository.PoolPath derives that directory from the package name alone, so
// a leftover sharing a directory with one of this run's selections is the
// same package by construction. The name and version quoted for the indexed
// side therefore come from that selection - never from the leftover's own
// control data, which this path deliberately never opens. Reading it would
// add a failure mode (an unparseable leftover aborting a run that is only
// trying to describe itself) to a change whose entire remit is to alter
// nothing a build does.
func unreferencedWarning(unreferenced []string, selections []resolve.Selection) *lock.Warning {
	if len(unreferenced) == 0 {
		return nil
	}

	indexedByDir := make(map[string][]resolve.Selection, len(selections))
	for _, sel := range selections {
		p := repository.PoolPath(sel.Name, sel.Filename)
		if p == "" {
			continue
		}
		d := path.Dir(p)
		indexedByDir[d] = append(indexedByDir[d], sel)
	}

	// Names are collected over the whole set, not just the enumerated head,
	// so Warning.Packages stays complete however long the list gets.
	nameSet := map[string]bool{}
	for _, rel := range unreferenced {
		for _, sel := range indexedByDir[path.Dir(rel)] {
			nameSet[sel.Name] = true
		}
	}
	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, n)
	}
	sort.Strings(names)

	parts := make([]string, 0, maxUnreferencedNamed+1)
	for i, rel := range unreferenced {
		if i == maxUnreferencedNamed {
			parts = append(parts, fmt.Sprintf("and %d more", len(unreferenced)-i))
			break
		}
		siblings := indexedByDir[path.Dir(rel)]
		if len(siblings) == 0 {
			// Nothing of this package is indexed at all: an earlier request
			// named it and this one does not. Saying only what is certain -
			// the path - is better than guessing a name from the filename.
			parts = append(parts, rel+" (Packages indexes no version of this package)")
			continue
		}
		versions := make([]string, 0, len(siblings))
		for _, sel := range siblings {
			versions = append(versions, sel.Version)
		}
		sort.Strings(versions)
		parts = append(parts, fmt.Sprintf("%s (Packages indexes %s %s)",
			rel, siblings[0].Name, strings.Join(versions, ", ")))
	}

	return &lock.Warning{
		Code: WarnUnreferenced,
		Message: fmt.Sprintf(
			"%d file(s) in repo/pool are not indexed by repo/Packages: they are on the media and covered by the manifest and its signature, but apt on the target cannot see or install them. %s. The complete list is in %s.",
			len(unreferenced), strings.Join(parts, "; "), UnreferencedFile),
		Packages: names,
	}
}
