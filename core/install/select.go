package install

import (
	"fmt"
	"sort"
	"strings"

	"github.com/inferops/debark/core/lock"
)

// selectInstallSet is where "the plan comes from the lock, not from apt's
// opinion" (ADR-007) actually happens. It never asks apt what the newest
// available version of anything is; it only ever returns the exact entries
// the online solve already chose (lk.Install), or — for --all — every
// package the bundle carries. --only narrows either set by package name.
//
// Every entry lk.Install carries (and every one this function builds for
// --all) is architecture-qualified: "name:arch=version", dpkg's own syntax
// (which apt-get install accepts directly) and the exact form
// core/lock.Validate now requires — a plain "name=version" is ambiguous for
// a Multi-Arch: same package, which legitimately appears at two
// architectures with the same name and the same version. names is exactly
// what gets handed to `apt-get install` (or simulated with
// `apt-get -s install`); pkgs is the matching subset of lk.Packages, in the
// same order, used for the disk-space precondition.
func selectInstallSet(lk *lock.Lock, opts Options) (names []string, pkgs []lock.Package, err error) {
	type selected struct {
		entry string
		pkg   lock.Package
	}
	var all []selected

	if opts.All {
		for _, p := range lk.Packages {
			all = append(all, selected{entry: archQualifiedEntry(p), pkg: p})
		}
	} else {
		for _, entry := range lk.Install {
			name, arch, version, ok := splitInstallEntry(entry)
			if !ok {
				return nil, nil, fmt.Errorf("lock: malformed install entry %q, want name:arch=version", entry)
			}
			p, found := lk.Find(name, arch)
			if !found || p.Version != version {
				return nil, nil, fmt.Errorf("lock: install entry %q names a package not present in packages at that version", entry)
			}
			all = append(all, selected{entry: entry, pkg: p})
		}
	}

	if len(opts.Only) > 0 {
		wanted := make(map[string]bool, len(opts.Only))
		for _, n := range opts.Only {
			if n = strings.TrimSpace(n); n != "" {
				wanted[n] = true
			}
		}
		found := make(map[string]bool, len(wanted))
		var filtered []selected
		for _, sp := range all {
			if wanted[sp.pkg.Name] {
				filtered = append(filtered, sp)
				found[sp.pkg.Name] = true
			}
		}
		var missing []string
		for n := range wanted {
			if !found[n] {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, nil, fmt.Errorf("--only: not in the bundle's lock: %s", strings.Join(missing, ", "))
		}
		all = filtered
	}

	sort.Slice(all, func(i, j int) bool { return all[i].entry < all[j].entry })
	names = make([]string, len(all))
	pkgs = make([]lock.Package, len(all))
	for i, sp := range all {
		names[i] = sp.entry
		pkgs[i] = sp.pkg
	}
	return names, pkgs, nil
}

// archQualifiedEntry renders a package as dpkg's architecture-qualified
// "name:arch=version", matching lock.Install's required form.
func archQualifiedEntry(p lock.Package) string {
	return p.Name + ":" + p.Arch + "=" + p.Version
}

// selectUpgradeSet returns the packages the online solve's own full-upgrade
// pass chose (lock.Package.Reason == lock.ReasonUpgrade), architecture-
// qualified exactly like selectInstallSet's entries, for install's --upgrade
// flag.
//
// E3 (docs/experiments/E3-pin-fidelity.md) found that --upgrade asking the
// target's own apt to run a free `full-upgrade` against the flat bundle
// escapes every pinning defence: the bundle's Release no longer carries the
// origins a target pin matched against online, so a second, unpinned
// resolution can silently reselect the exact version the target's pin was
// written to reject — and full-upgrade reports it as ordinary success. The
// online solve already ran full-upgrade --download-only at build time and
// recorded exactly what it chose (Reason: "upgrade", lock.ReasonUpgrade);
// applying ADR-007's own principle here means installing exactly that,
// never asking the target's apt to decide again. Concretely, runner.go folds
// this function's result into the same exact-version `apt-get install
// name:arch=version …` call the plain install path already uses (never a
// bare `full-upgrade`), so the upgrade path now gets defences 1 and 2 for
// free, exactly like every other install.
//
// baseSelection is the packages selectInstallSet already chose (the
// requested/dependency, or --all, set): an upgrade-reason entry sharing a
// (name, arch) with one of those is skipped rather than merged in, because
// the base selection's version comes from the request/dependency closure the
// online solve resolved together — a harder, more specific constraint than
// "also happened to be upgradeable" — and apt-get install cannot sensibly be
// asked to install two different versions of the same name:arch in one
// invocation anyway. This is the precedence rule for a package the lock
// records both ways (once in the dependency closure at one version, and
// separately, as its own pool entry, with Reason "upgrade" at a different
// version — a normal outcome of the additive bundles, the same shape E3's
// own bundle-accumulated fixture used): the dependency/request version wins.
//
// total is every Reason: upgrade entry the lock carries, before the
// baseSelection dedup, so a caller can distinguish "the lock recorded
// nothing to upgrade at all" (total == 0) from "everything the lock would
// have upgraded is already covered by the requested set" (total > 0,
// len(names) == 0).
func selectUpgradeSet(lk *lock.Lock, baseSelection []lock.Package) (names []string, pkgs []lock.Package, total int) {
	already := make(map[string]bool, len(baseSelection))
	for _, p := range baseSelection {
		already[p.Name+":"+p.Arch] = true
	}

	type selected struct {
		entry string
		pkg   lock.Package
	}
	var all []selected
	for _, p := range lk.Packages {
		if p.Reason != lock.ReasonUpgrade {
			continue
		}
		total++
		if already[p.Name+":"+p.Arch] {
			continue
		}
		all = append(all, selected{entry: archQualifiedEntry(p), pkg: p})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].entry < all[j].entry })

	names = make([]string, len(all))
	pkgs = make([]lock.Package, len(all))
	for i, sp := range all {
		names[i] = sp.entry
		pkgs[i] = sp.pkg
	}
	return names, pkgs, total
}

// splitInstallEntry parses lock.Install's required "name:arch=version" form.
// core/lock's own equivalent (parseInstallEntry) is unexported, so this is a
// second, small, install-owned implementation of the same documented format
// (lock/types.go's Install field comment) — the same deliberate choice made
// throughout this package to avoid a compile-time dependency on another package helper functions for a handful of lines of string splitting.
func splitInstallEntry(s string) (name, arch, version string, ok bool) {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 || eq == len(s)-1 {
		return "", "", "", false
	}
	left, version := s[:eq], s[eq+1:]
	colon := strings.IndexByte(left, ':')
	if colon <= 0 || colon == len(left)-1 {
		return "", "", "", false
	}
	return left[:colon], left[colon+1:], version, true
}
