package harness

import (
	"os"
	"path/filepath"
)

// DefaultWorkRoot is where RunOpts.WorkDir points by default: the
// DEBARK_E2E_WORKDIR environment variable when set (so a session can point
// it at a scratch area of its choosing), otherwise a "debark-e2e"
// directory under the OS temp dir. Never inside the debark repository
// itself — test/e2e and hack/matrix are this package's owned *source*
// directories, not a place for run artefacts (snapshots, bundles, built
// binaries) to accumulate.
func DefaultWorkRoot() string {
	if v := os.Getenv("DEBARK_E2E_WORKDIR"); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "debark-e2e")
}
