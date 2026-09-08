package audit

// Rule 5, mechanically.
//
//	"Zero runtime npm dependencies, and — as built — zero npm dependencies at
//	 all. There is no bundler and no JavaScript build step. Adding an npm
//	 package is a decision that needs a written justification in
//	 docs/dependency-review.md, not a `npm install`."
//	                              — docs/dev/contract-brief.md, rule 5
//
// This is a supply-chain rule before it is a build rule. The frontend runs
// inside a webview on the one host in the pipeline that holds a signing key,
// and an npm dependency tree is the shape of supply-chain compromise this
// project can least afford to review. `frontend/package.json` was deleted
// early on and the "build" is `hack/copyfrontend`, a recursive file copy.
//
// The check has to look for three different things, because "zero npm
// dependencies" fails in three different ways: a manifest that declares one, a
// lockfile that pins one, and an installed tree that carries one whether or not
// anything declares it.

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secLockfile is a file whose presence means a package manager resolved a
// dependency graph here.
var secLockfile = map[string]bool{
	"package-lock.json":   true,
	"yarn.lock":           true,
	"pnpm-lock.yaml":      true,
	"npm-shrinkwrap.json": true,
	"bun.lockb":           true,
	"deno.lock":           true,
}

func TestZeroNPMDependencies(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}

	var manifests []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git":
				return fs.SkipDir
			case "node_modules":
				// Not skipped — reported. .gitignore says so in as many words:
				// "node_modules is NOT ignored, also on purpose... if a
				// node_modules/ ever appears in `git status`, that is a defect
				// to investigate, not noise to hide."
				t.Errorf("%s exists: something ran a package manager in this tree", rel)
				return fs.SkipDir
			}
			return nil
		}
		if secLockfile[d.Name()] {
			t.Errorf("%s exists: a package manager resolved a dependency graph here", rel)
		}
		if d.Name() == "package.json" {
			manifests = append(manifests, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, rel := range manifests {
		// frontend/dist is a copy of frontend/ made by hack/copyfrontend, so a
		// manifest there is the same file counted twice.
		if strings.HasPrefix(rel, "frontend/dist/") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		var pkg struct {
			Name         string            `json:"name"`
			Dependencies map[string]string `json:"dependencies"`
			Dev          map[string]string `json:"devDependencies"`
			Peer         map[string]string `json:"peerDependencies"`
			Optional     map[string]string `json:"optionalDependencies"`
		}
		if err := json.Unmarshal(b, &pkg); err != nil {
			t.Errorf("%s: not valid JSON: %v", rel, err)
			continue
		}
		for label, m := range map[string]map[string]string{
			"dependencies": pkg.Dependencies, "devDependencies": pkg.Dev,
			"peerDependencies": pkg.Peer, "optionalDependencies": pkg.Optional,
		} {
			for name := range m {
				t.Errorf("%s declares the %s entry %q. Rule 5 wants a written justification in "+
					"docs/dependency-review.md before an npm package enters this tree, not an install.",
					rel, label, name)
			}
		}
		t.Logf("%s (%s) declares no dependencies of any kind", rel, pkg.Name)
	}

	// The one manifest that legitimately exists is Wails' own generated runtime
	// package metadata, which is committed so a fresh clone builds without the
	// wails CLI. If it ever disappears, or a second one appears, this test's
	// reader should be told rather than left to infer it from a silent pass.
	if len(manifests) == 0 {
		t.Log("no package.json anywhere in the tree")
	}
}

// TestNoJavaScriptBuildStep guards the other half of rule 5: the reason there
// are no npm dependencies is that there is nothing to run them. A bundler
// config appearing in the tree is the change that makes an npm dependency
// possible, and it is easier to spot here than after it has one.
func TestNoJavaScriptBuildStep(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	bundlerConfig := []string{
		"vite.config.js", "vite.config.ts", "webpack.config.js", "rollup.config.js",
		"rollup.config.mjs", "esbuild.config.js", "parcel.config.json", "snowpack.config.js",
		"tsconfig.json", "babel.config.js", ".babelrc", "svelte.config.js", "next.config.js",
	}
	want := make(map[string]bool, len(bundlerConfig))
	for _, n := range bundlerConfig {
		want[n] = true
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !want[d.Name()] {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		t.Errorf("%s exists: the frontend is vanilla ES modules copied by hack/copyfrontend, "+
			"and a build step is the change that makes an npm dependency possible",
			filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
