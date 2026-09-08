package engine

import (
	"sort"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
)

// dedupeStrings drops repeats, keeping the first occurrence's position.
// Determinism here means "no map-iteration order leaks into the output", not
// "the output is sorted" — the caller's own ordering (list-file order,
// command-line order) is preserved, which is what makes a reordered-but-
// otherwise-identical request still resolve to the same input set.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// dedupeURLs drops repeated URLs, first occurrence wins (including whichever
// digest it carried).
func dedupeURLs(in []buildjob.URLInput) []buildjob.URLInput {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]buildjob.URLInput, 0, len(in))
	for _, u := range in {
		if u.URL == "" || seen[u.URL] {
			continue
		}
		seen[u.URL] = true
		out = append(out, u)
	}
	return out
}

// containsString is a small linear lookup; the flag slices it is used on are
// always short (a handful of redistribution/doctor flags per package).
func containsString(in []string, s string) bool {
	for _, v := range in {
		if v == s {
			return true
		}
	}
	return false
}

// sortedStrings sorts in place and returns it, for call sites that want to
// both mutate and chain.
func sortedStrings(in []string) []string {
	sort.Strings(in)
	return in
}

// dedupeFetchFailures is dedupeStrings for fetch failures, keyed on Input:
// the first entry for an input wins, and an entry with no input at all is
// dropped. It returns nil rather than an empty slice, so both fields it
// feeds stay absent from the JSON under omitempty.
func dedupeFetchFailures(in []buildjob.FetchFailure) []buildjob.FetchFailure {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]buildjob.FetchFailure, 0, len(in))
	for _, f := range in {
		if f.Input == "" || seen[f.Input] {
			continue
		}
		seen[f.Input] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
