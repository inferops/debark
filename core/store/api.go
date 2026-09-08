// Frozen public API of the store package.

package store

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/inferops/debark/core/dferr"
)

// Open opens or creates the store rooted at dir. An empty dir uses
// DefaultRoot.
func Open(dir string) (Store, error) {
	if dir == "" {
		dir = DefaultRoot()
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, dferr.Wrap(dferr.Environment, err, "store: resolve root %s", dir)
	}
	for _, sub := range []string{filepath.Join("sha256", "tmp")} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "store: create %s", sub)
		}
	}
	return &fsStore{root: root}, nil
}

// DefaultRoot is the XDG-correct store location:
// $XDG_DATA_HOME/debark/store, falling back to ~/.local/share on Unix and
// %LOCALAPPDATA% on Windows.
func DefaultRoot() string {
	if v := os.Getenv("DEBARK_STORE"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "debark", "store")
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "debark", "store")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "debark-store")
	}
	return filepath.Join(home, ".local", "share", "debark", "store")
}
