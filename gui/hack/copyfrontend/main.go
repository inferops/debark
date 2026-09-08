// Command copyfrontend is debark-gui's entire frontend build step.
//
// This project has zero npm dependencies and no JavaScript build step. That
// is a product requirement, not a preference: the frontend is authored as
// plain HTML/CSS/JS under frontend/, loaded by the browser as native ES
// modules, and there is nothing to transpile, bundle or minify. The "build"
// is therefore a recursive copy of the authored files into frontend/dist,
// which main.go embeds with `//go:embed all:frontend/dist`.
//
// It is written in Go rather than as a shell script for two reasons:
//
//   - it must run identically on Linux and on Windows, where `cp -r` is not
//     a given (Git Bash is not guaranteed, PowerShell and cmd differ), and
//   - the only tool it is allowed to need is the Go toolchain, which the
//     project already requires. Adding a copy dependency in order to avoid
//     npm dependencies would defeat the point.
//
// The layout under dist/ mirrors the layout under frontend/ exactly, so a
// relative import that resolves while editing (say, ../wailsjs/runtime/
// runtime.js from src/main.js) resolves identically in the shipped bundle.
// Nothing is rewritten, renamed or fingerprinted.
//
// It locates the project root by walking up from the working directory
// looking for wails.json, because it is invoked from two different places:
// `make frontend` runs it from the repository root, while Wails runs
// wails.json's "frontend:build" with the working directory set to frontend/.
//
// Usage:
//
//	go run ./hack/copyfrontend          # populate frontend/dist
//	go run ./hack/copyfrontend -clean   # empty frontend/dist
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// keepName is committed to frontend/dist so that main.go's
// `//go:embed all:frontend/dist` resolves on a fresh clone, before this
// command has ever run. go:embed fails the build on a missing directory,
// and frontend/dist is generated and gitignored, so the directory has to be
// held open by something. This command never deletes it.
const keepName = ".gitkeep"

// dirPerm and filePerm are the modes used for everything written into
// frontend/dist. The source modes are deliberately not preserved: these are
// static web assets read back by the app's own asset server, the tree is
// disposable and regenerated on every build, and copying an odd mode across
// from a working copy (or from a Windows checkout, where it is meaningless)
// would only make the output vary by machine.
const (
	dirPerm  fs.FileMode = 0o755
	filePerm fs.FileMode = 0o644
)

// sources are the paths under frontend/ that make up the shipped bundle,
// relative to frontend/ and copied to the same relative path under
// frontend/dist. Each may be a file or a directory.
//
//   - index.html and src/ are the hand-authored frontend.
//   - wailsjs/ is Wails' generated bindings (`wails generate module`). It is
//     committed rather than generated on demand, and it is copied here
//     rather than imported from outside dist/ because the embedded asset FS
//     is rooted at dist/: anything the page loads at runtime has to be
//     inside it.
//
// A missing entry is a warning, not an error: index.html and src/ are owned
// by other packages, and this command has to keep working while they land.
var sources = []string{"index.html", "src", "wailsjs"}

func main() {
	clean := flag.Bool("clean", false, "empty frontend/dist instead of populating it")
	flag.Parse()

	if err := run(*clean); err != nil {
		fmt.Fprintf(os.Stderr, "copyfrontend: %v\n", err)
		os.Exit(1)
	}
}

func run(clean bool) error {
	root, err := projectRoot()
	if err != nil {
		return err
	}

	frontend := filepath.Join(root, "frontend")
	dist := filepath.Join(frontend, "dist")

	if err := resetDist(dist); err != nil {
		return err
	}
	if clean {
		fmt.Printf("copyfrontend: emptied %s\n", rel(root, dist))
		return nil
	}

	total := 0
	for _, name := range sources {
		src := filepath.Join(frontend, name)

		info, err := os.Lstat(src)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			fmt.Fprintf(os.Stderr, "copyfrontend: warning: %s does not exist, skipping\n", rel(root, src))
			continue
		case err != nil:
			return err
		}

		n, err := copyPath(src, filepath.Join(dist, name), info)
		if err != nil {
			return err
		}
		total += n
	}

	if total == 0 {
		return fmt.Errorf("copied nothing into %s: none of %v exist under %s",
			rel(root, dist), sources, rel(root, frontend))
	}

	fmt.Printf("copyfrontend: copied %d file(s) into %s\n", total, rel(root, dist))
	return nil
}

// projectRoot walks up from the working directory until it finds the
// directory holding wails.json.
func projectRoot() (string, error) {
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
		_, err := os.Stat(filepath.Join(dir, "wails.json"))
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no wails.json in %s or any parent directory; "+
				"run this from inside the debark-gui checkout", start)
		}
		dir = parent
	}
}

// resetDist makes dist exist and empties it of everything but keepName, so
// that a file deleted from frontend/ also disappears from the bundle. An
// incremental copy would leave the deleted file behind and ship it.
func resetDist(dist string) error {
	if err := os.MkdirAll(dist, dirPerm); err != nil {
		return err
	}

	entries, err := os.ReadDir(dist)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == keepName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dist, entry.Name())); err != nil {
			return err
		}
	}

	// Recreate the sentinel if a previous `rm -rf frontend/dist` took it.
	keep := filepath.Join(dist, keepName)
	_, err = os.Stat(keep)
	if errors.Is(err, fs.ErrNotExist) {
		return os.WriteFile(keep, nil, filePerm)
	}
	return err
}

// copyPath copies src (a regular file or a directory tree) to dst and
// returns the number of regular files written.
func copyPath(src, dst string, info fs.FileInfo) (int, error) {
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("%s is not a regular file or directory (mode %s)", src, info.Mode())
		}
		return 1, copyFile(src, dst)
	}

	count := 0
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		target, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target = filepath.Join(dst, target)

		switch {
		case entry.IsDir():
			return os.MkdirAll(target, dirPerm)
		case isDevArtifact(entry.Name()):
			// Skipped, not refused: these are development artefacts that live
			// beside the source on purpose (a component gallery, a screen's
			// standalone harness, the design system's own README) and are
			// reviewed in a browser from the source tree. Shipping them
			// embeds a couple of hundred kilobytes of pages no operator can
			// reach into every binary.
			return nil
		case entry.Type().IsRegular():
			count++
			return copyFile(path, target)
		default:
			// Symlinks and device nodes are refused rather than skipped:
			// silently dropping a file from the shipped bundle produces a
			// blank window at runtime and no clue as to why.
			return fmt.Errorf("%s is not a regular file or directory (mode %s)", path, entry.Type())
		}
	})

	return count, err
}

// isDevArtifact reports whether a frontend file exists for developers rather
// than for the shipped app.
//
// Matched by naming convention rather than by a list, so a new screen's
// harness is excluded the day it is written instead of the day someone
// notices it in the bundle. The conventions are fixed in
// docs/dev/screen-contract.md: a harness is "<name>.demo.html", the design
// system's browsable catalogue is "gallery.html", and documentation is
// Markdown.
func isDevArtifact(name string) bool {
	switch {
	case strings.HasSuffix(name, ".demo.html"):
		return true
	case name == "gallery.html":
		return true
	case strings.HasSuffix(name, ".md"):
		return true
	default:
		return false
	}
}

func copyFile(src, dst string) (err error) {
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return err
	}
	// The write is only complete once Close has succeeded: a buffered write
	// can fail on flush, and a truncated asset in the bundle is a runtime
	// failure with no error message attached to it.
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	_, err = io.Copy(out, in)
	return err
}

// rel renders path relative to root for readable log output, falling back to
// the absolute path if it is somehow outside the tree.
func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}
