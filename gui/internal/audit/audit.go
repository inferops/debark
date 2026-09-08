// Package audit holds the mechanical checks that keep the contract brief's
// non-negotiable rules true after the people who wrote them have moved on.
//
// The rules this package enforces are stated as prose in
// `docs/dev/contract-brief.md`, and prose decays. Rule 3 in particular — "no
// network call to any debark-operated endpoint, ever" — is a definition-of-
// done item that says *proven by a test, not asserted*, because the failure it
// guards against is not a bug someone writes on purpose. It is a plausible
// future commit: an update check, a crash report, a hosted catalogue "just for
// the first run". Every one of those looks reasonable in isolation, and none
// of them is caught by any other test in this repository.
//
// So the tests here read the tree itself. They are deliberately not unit tests
// of anything: there is almost no logic to unit-test, and what little there is
// exists to make the scan honest rather than to compute a result. What matters
// is what they refuse to allow:
//
//   - rule3_test.go   — no compiled-in endpoint, and a fixed inventory of the
//     files that may open a socket at all.
//   - rule4_test.go   — no telemetry, analytics or crash-reporting library in
//     the linked dependency closure.
//   - rule5_test.go   — zero npm dependencies, as built.
//   - domsinks_test.go — the inventory of HTML-injection sinks in the
//     frontend, which is what makes "every screen renders untrusted text
//     through textContent" checkable rather than claimed.
//
// `xssharness/` is the other half of that last one and is not a Go test: it
// boots the real shell in a headless browser against a bridge whose every
// string carries an injection payload. The two are complementary and neither
// is sufficient — the scan cannot see run-time behaviour, and the harness
// cannot reach a code path that needs an interaction sequence to invent. Its
// README says which 21 of the 49 bound methods it does not reach.
//
// # How these tests fail
//
// Every one of them fails **closed**: an unrecognised host, an unexpected file
// opening a socket, a new `innerHTML`, a `package.json` carrying dependencies.
// None of them has a wildcard exemption and none of them can be satisfied by
// adding an entry to a list without also writing down why. That is the whole
// design: a check whose exemption list can grow silently is a check that will
// be silently defeated.
//
// # What they cannot prove
//
// Stated plainly, because a reviewer reading a green test run deserves to know
// its edges:
//
//   - A source scan cannot see runtime behaviour. If an operator-supplied
//     vendor URL happened to name a debark-operated host, this package would
//     not notice, and should not: rule 3 forbids *this application* choosing
//     such a host, not the operator typing one.
//   - It cannot see into a dependency. `wails` links an HTTP server for its
//     own dev mode; rule4_test.go checks the module names in the link closure,
//     not what each module does. The dependency review
//     (`docs/dependency-review.md`) is where that judgement lives.
//   - It reads the source tree, not the shipped binary. A build that injected a
//     host through `-ldflags -X` would pass. That is recorded as a residual in
//     `docs/security-review.md` rather than papered over here.
package audit

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// repoRoot walks up from the test's working directory to the directory holding
// go.mod, mirroring hack/copyfrontend's projectRoot. Tests run with the working
// directory set to their own package, so every path below is anchored here and
// none of them is relative to wherever `go test` was invoked from.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod in " + start + " or any parent directory")
		}
		dir = parent
	}
}

// skipDir names directories no scan descends into.
//
// frontend/dist is the one that matters and the one a naive scan gets wrong:
// it is a byte-for-byte copy of frontend/index.html, frontend/src and
// frontend/wailsjs produced by hack/copyfrontend, so scanning it double-counts
// every finding and, worse, makes a stale copy look like a second defect. The
// authored files are the source of truth; the copy is output.
//
// `build/` is deliberately NOT here, and the distinction cost a real finding:
// only `build/bin/` is output. The rest of `build/` is committed packaging —
// the Wails NSIS installer scripts, the darwin plists, the Windows manifest —
// which is exactly the kind of file that acquires a download URL or an
// elevation request without anyone reading it. skipRel handles the output
// directory by path so the packaging stays in scope.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist":
		return true
	default:
		return false
	}
}

// skipRel names directories excluded by their path rather than their name,
// because "build" as a name is too broad to skip.
func skipRel(rel string) bool {
	switch rel {
	case "build/bin":
		return true
	default:
		return false
	}
}

// sourceFiles returns every file under root whose extension is in exts,
// skipping generated and vendored trees. Paths are returned slash-separated
// and relative to root, so a failure message reads the same on every platform.
func sourceFiles(root string, exts ...string) ([]string, error) {
	want := make(map[string]bool, len(exts))
	for _, e := range exts {
		want[e] = true
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			if skipDir(d.Name()) || skipRel(filepath.ToSlash(rel)) {
				return fs.SkipDir
			}
			return nil
		}
		if !want[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}

// readSource reads one repo-relative path.
func readSource(root, rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// lineOf reports the 1-based line number of byte offset off in src, so a
// finding can be cited as file:line rather than as a byte offset nobody can
// navigate to.
func lineOf(src string, off int) int {
	if off > len(src) {
		off = len(src)
	}
	return 1 + strings.Count(src[:off], "\n")
}

// isDevOnlyFrontend reports whether a frontend path is a development harness
// that hack/copyfrontend deliberately excludes from the bundle
// (isDevArtifact there). These files are still scanned for a compiled-in
// endpoint — a beacon in a demo page is still a beacon in the repository — but
// they are not held to the shipped-asset rules, because they are not shipped.
func isDevOnlyFrontend(rel string) bool {
	base := filepath.Base(rel)
	switch {
	case strings.HasSuffix(base, ".demo.html"):
		return true
	case base == "gallery.html":
		return true
	case strings.HasSuffix(base, ".md"):
		return true
	default:
		return false
	}
}
