package install

import (
	"fmt"
	"os"
	"path/filepath"
)

// sourcesFileName is the name of the deb822 source file install writes, both
// in the temporary private root and (with --keep-source) permanently under
// /etc/apt/sources.list.d/.
const sourcesFileName = "debark-bundle.sources"

// privateRoot is the target-side equivalent of the private apt root: a
// scratch directory that makes the bundle's repo/ the *only* thing apt-get
// can see, without touching a single file under the system's real
// /etc/apt or /var/lib/apt. Every -o option below, and the deb822 source
// stanza, is ported option-for-option from install-offline.sh (lines
// ~139-159), which is the proven, measured design (step 3).
type privateRoot struct {
	dir         string
	sourcesFile string
	repoDir     string
	baseOpts    []string
}

// newPrivateRoot builds the temporary apt view over repoDir (a bundle's
// repo/ directory, already verified). fast mirrors install-offline.sh's
// --fast branch: it adds the dpkg speed options to every apt-get invocation
// made through this root (harmless for `update`/`-s`, which never touch
// dpkg).
func newPrivateRoot(repoDir string, fast bool) (pr *privateRoot, err error) {
	absRepo, err := filepath.Abs(repoDir)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path: %w", err)
	}
	if fi, statErr := os.Stat(absRepo); statErr != nil || !fi.IsDir() {
		if statErr == nil {
			statErr = fmt.Errorf("%s is not a directory", absRepo)
		}
		return nil, fmt.Errorf("bundle repository: %w", statErr)
	}

	dir, err := os.MkdirTemp("", "debark-install-*")
	if err != nil {
		return nil, fmt.Errorf("create private apt root: %w", err)
	}
	defer func() {
		if err != nil {
			// Deliberately ignored, not forgotten: this is the unwind path
			// for a constructor that is already returning an error, and
			// there is nothing useful to do with a second one.
			_ = os.RemoveAll(dir)
		}
	}()

	for _, sub := range []string{
		filepath.Join("sources.list.d"),
		filepath.Join("preferences.d"),
		filepath.Join("lists", "partial"),
	} {
		if mkErr := os.MkdirAll(filepath.Join(dir, sub), 0o755); mkErr != nil {
			return nil, fmt.Errorf("create %s: %w", sub, mkErr)
		}
	}

	sourcesFile := filepath.Join(dir, "sources.list.d", sourcesFileName)
	stanza := sourceStanza(absRepo)
	if writeErr := os.WriteFile(sourcesFile, []byte(stanza), 0o644); writeErr != nil {
		return nil, fmt.Errorf("write %s: %w", sourcesFileName, writeErr)
	}

	opts := []string{
		"-o", "Dir::Etc::sourcelist=/dev/null",
		"-o", "Dir::Etc::sourceparts=" + toSlash(filepath.Join(dir, "sources.list.d")),
		"-o", "Dir::Etc::preferences=/dev/null",
		"-o", "Dir::Etc::preferencesparts=" + toSlash(filepath.Join(dir, "preferences.d")),
		"-o", "Dir::State::lists=" + toSlash(filepath.Join(dir, "lists")),
		"-o", "Dir::Cache::pkgcache=",
		"-o", "Dir::Cache::srcpkgcache=",
		"-o", "Acquire::Languages=none",
		"-o", "APT::Sandbox::User=root",
	}
	if fast {
		// install-offline.sh --fast: no per-file fsync, and a plain
		// (non-progress-bar) dpkg status stream, for a batch install that can
		// simply be redone if interrupted.
		opts = append(opts,
			"-o", "Dpkg::Options::=--force-unsafe-io",
			"-o", "Dpkg::Use-Pty=false",
		)
	}

	return &privateRoot{dir: dir, sourcesFile: sourcesFile, repoDir: absRepo, baseOpts: opts}, nil
}

// sourceStanza is the deb822 .sources content: a flat-format repository
// ("Suites: ./" — no dists/ hierarchy), trusted because the bundle's
// manifest signature has already been verified ("install uses
// Trusted: yes on the private source after manifest verification has
// already succeeded").
func sourceStanza(absRepoDir string) string {
	uri := "file://" + toSlash(absRepoDir)
	return "Types: deb\n" +
		"URIs: " + uri + "\n" +
		"Suites: ./\n" +
		"Trusted: yes\n"
}

// toSlash normalises a path to forward slashes for use inside an apt option
// value or a file: URI. This only ever runs for real on Linux, where
// filepath.Separator is already '/'; the conversion is cheap insurance for
// the Windows unit-test run, where argv is captured and compared as text but
// never actually opened by a real apt-get.
func toSlash(p string) string {
	out := make([]byte, len(p))
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' {
			out[i] = '/'
		} else {
			out[i] = p[i]
		}
	}
	return string(out)
}

// AptArgs returns the -o option set for one apt-get invocation, with -y
// appended when yes is true. -y is deliberately never implied any other way
// (see requireConfirmation): a caller must ask for it explicitly.
func (p *privateRoot) AptArgs(yes bool) []string {
	args := make([]string, len(p.baseOpts), len(p.baseOpts)+1)
	copy(args, p.baseOpts)
	if yes {
		args = append(args, "-y")
	}
	return args
}

// Close removes the temporary private root. It never touches anything
// outside the directory newPrivateRoot created.
func (p *privateRoot) Close() error {
	if p == nil || p.dir == "" {
		return nil
	}
	return os.RemoveAll(p.dir)
}

// PersistSource copies the .sources file to the real
// /etc/apt/sources.list.d/ under root (--keep-source). This is
// the one deliberate, explicit, opt-in exception to "never touch the
// system's apt configuration" — install-offline.sh does the same thing under
// the same flag. It returns the path written.
func (p *privateRoot) PersistSource(root string) (string, error) {
	data, err := os.ReadFile(p.sourcesFile)
	if err != nil {
		return "", fmt.Errorf("read private .sources: %w", err)
	}
	destDir := filepath.Join(root, "etc", "apt", "sources.list.d")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", destDir, err)
	}
	dest := filepath.Join(destDir, sourcesFileName)
	// Every component of dest is fixed except `root`, which is the target
	// filesystem root the operator passed (normally "/"): "etc", "apt",
	// "sources.list.d" are literals and sourcesFileName is a package constant.
	// There is no path component here that any untrusted input can influence,
	// so G703 has nothing to traverse. The taint it followed is `data`, the
	// file CONTENT, which is written into a fixed location by design -- that
	// is what --keep-source means, and the doc comment above says so.
	// #nosec G703 -- destination is entirely constant below the caller's root
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", dest, err)
	}
	return dest, nil
}
