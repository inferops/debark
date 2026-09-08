package cli

import (
	"fmt"
	"strings"

	"github.com/inferops/debark/core/dferr"
)

// exitClassFromString maps a buildjob.ExitClass (or any of dferr's stable
// class strings) back to a dferr.Class, for results that travel as a string
// because they may cross a process or JSON boundary (BuildResult.ExitClass).
// Unknown values map to Usage, matching dferr.ClassOf's own fallback.
func exitClassFromString(s string) dferr.Class {
	for _, c := range dferr.Classes() {
		if c.String() == s {
			return c
		}
	}
	return dferr.Usage
}

// maxNamedItems is how many names a one-line diagnosis carries before it
// starts counting instead. Three is enough to recognise the failure without
// wrapping a terminal line, and the full list is always on stdout — in the
// human report and in the --json document.
const maxNamedItems = 3

// nameSome renders a list of names for a one-line diagnosis on stderr: the
// first few verbatim, then how many were left out.
//
// The point is that the line NAMES something. A command that fails must say
// which package apt could not satisfy, which URL did not download, which
// file's digest did not match — "3 inputs unresolved" is a count, and a
// count is not a diagnosis. Callers that have the list pass it here rather
// than formatting their own, so every command elides the same way.
func nameSome(items []string) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) <= maxNamedItems {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:maxNamedItems], ", "), len(items)-maxNamedItems)
}
