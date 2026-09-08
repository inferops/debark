package resolve

import "sort"

// AttributeReasons fills in Reason for every selection that does not already
// have one, by walking a dependency adjacency map outward from roots.
// debark never invents an edge the resolved packages' own control data did
// not declare (principle 1): deps must be built from their actual
// Depends/Pre-Depends/Recommends fields, restricted to relation
// alternatives that are themselves resolved selections.
//
// roots is every selection name whose Reason is fixed by the caller to
// something other than a dependency edge: the packages actually requested,
// the packages full-upgrade pulled in (reason upgrade), and external package
// names (reason external) — a dependency introduced by any of those is
// honestly "because of" that root, not only because of the packages typed on
// the command line.
//
// A root is never given a Reason here, even when the caller has not filled
// one in yet. A root is by definition not a dependency of anything, least of
// all of itself, and the caller is the only party that knows which kind of
// root it is (requested, upgrade, external). A root reaching this function
// with a blank Reason therefore leaves with a blank Reason, for the caller to
// name; core/apt's local backend relies on exactly that to stamp
// lock.ReasonUpgrade on the roots full-upgrade contributed.
//
// deps maps a resolved package name to the resolved package names its
// control data relates to. AttributeReasons runs a multi-source breadth-
// first search from roots over deps and assigns each remaining selection
// dependency-of:<root> for the root that reaches it in the fewest hops;
// ties (equal hop count from more than one root) are broken by choosing the
// lexicographically smallest root, so two runs over the same input produce
// the same attribution.
//
// A selection deps cannot explain at all (apt pulled it in for a reason not
// visible in the relation fields available here — a Conflicts-driven
// replacement, for instance) is attributed to a representative root rather
// than left blank: that is a deliberate, documented approximation, not a
// discovered edge. The representative is the lexicographically first root
// that is itself among the selections, so the recorded parent always names a
// package the bundle actually contains.
func AttributeReasons(roots []string, selections []Selection, deps map[string][]string) {
	known := make(map[string]bool, len(selections))
	for _, s := range selections {
		known[s.Name] = true
	}

	sortedRoots := append([]string(nil), roots...)
	sort.Strings(sortedRoots)

	dist := make(map[string]int, len(known))
	via := make(map[string]string, len(known))
	var queue []string
	for _, r := range sortedRoots {
		if !known[r] {
			continue
		}
		if _, seen := dist[r]; seen {
			continue
		}
		dist[r] = 0
		via[r] = r
		queue = append(queue, r)
	}
	for i := 0; i < len(queue); i++ {
		cur := queue[i]
		next := append([]string(nil), deps[cur]...)
		sort.Strings(next)
		for _, n := range next {
			if !known[n] {
				continue
			}
			if _, seen := dist[n]; seen {
				continue
			}
			dist[n] = dist[cur] + 1
			via[n] = via[cur]
			queue = append(queue, n)
		}
	}

	// The representative used for selections deps cannot explain. Preferring
	// a root that is itself a selection keeps the recorded parent inside the
	// bundle: a root can legitimately be absent from selections (a requested
	// package held on the target, say), and naming it as a parent would put
	// a package the bundle does not contain into the lock.
	fallback := ""
	for _, r := range sortedRoots {
		if known[r] {
			fallback = r
			break
		}
	}
	if fallback == "" && len(sortedRoots) > 0 {
		fallback = sortedRoots[0]
	}

	for i := range selections {
		s := &selections[i]
		if s.Reason != "" {
			continue
		}
		if origin, ok := via[s.Name]; ok {
			if origin != s.Name {
				s.Reason = DependencyOf(origin)
			}
			// origin == s.Name means this selection is a seeded root: leave
			// it blank rather than fabricating dependency-of:<itself>.
			continue
		}
		if fallback != "" && fallback != s.Name {
			// Fallback representative: deps did not explain this selection.
			// See the doc comment above — this is a deliberate
			// approximation, recorded as a plain dependency-of edge so
			// downstream code never has to special-case an "unknown" reason.
			s.Reason = DependencyOf(fallback)
		}
	}
}
