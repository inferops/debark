package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

// RepoRoot locates the debark module root from this file's own location,
// so the harness works regardless of the caller's working directory (Go test
// binaries run with cwd set to the package directory; hack/matrix runs from
// wherever the operator invoked `go run` from).
func RepoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("harness: could not determine source location")
	}
	// this file is test/e2e/harness/binary.go
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", fmt.Errorf("harness: computed repo root %s has no go.mod: %w", root, err)
	}
	return root, nil
}

// BuildResult is the outcome of building the debark CLI for one target
// platform.
type BuildResult struct {
	// Path is the built binary on the host.
	Path string
	// GOARCH is the architecture it was built for (amd64, arm64, ...).
	GOARCH string
	// Output is the combined build output, kept even on success so a
	// warning is never silently dropped.
	Output string
}

// buildCache memoises one build per GOARCH per process, since every fixture
// on the same architecture needs the identical binary and rebuilding it once
// per row would dominate wall-clock time for no reason.
var (
	buildCacheMu sync.Mutex
	buildCache   = map[string]*buildCacheEntry{}
)

type buildCacheEntry struct {
	once   sync.Once
	result BuildResult
	err    error
}

// BuildLinuxBinary cross-compiles cmd/debark for linux/GOARCH with
// CGO_ENABLED=0 (a static binary, as resolve-contract.md's container-driver
// rules require for anything mounted into a container) and caches the result
// in workDir for the lifetime of the process. It is the harness's first
// pipeline stage, and today it is also where every fixture currently stops,
// on a compiler error in the tree being cross-compiled.
func BuildLinuxBinary(ctx context.Context, workDir, goarch string) (BuildResult, error) {
	buildCacheMu.Lock()
	entry, ok := buildCache[goarch]
	if !ok {
		entry = &buildCacheEntry{}
		buildCache[goarch] = entry
	}
	buildCacheMu.Unlock()

	entry.once.Do(func() {
		entry.result, entry.err = buildLinuxBinaryOnce(ctx, workDir, goarch)
	})
	return entry.result, entry.err
}

func buildLinuxBinaryOnce(ctx context.Context, workDir, goarch string) (BuildResult, error) {
	root, err := RepoRoot()
	if err != nil {
		return BuildResult{}, err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return BuildResult{}, err
	}
	out := filepath.Join(workDir, "debark-linux-"+goarch)

	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/debark")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH="+goarch,
		"CGO_ENABLED=0",
	)
	output, runErr := cmd.CombinedOutput()
	res := BuildResult{Path: out, GOARCH: goarch, Output: string(output)}
	if runErr != nil {
		return res, fmt.Errorf("go build ./cmd/debark (GOOS=linux GOARCH=%s): %w\n%s", goarch, runErr, output)
	}
	if _, statErr := os.Stat(out); statErr != nil {
		return res, fmt.Errorf("go build ./cmd/debark reported success but %s is missing: %w", out, statErr)
	}
	return res, nil
}

// ResetBuildCacheForTest clears the memoised binary builds. Test-only: it
// lets fixture_test.go and similar unit tests exercise BuildLinuxBinary's
// error path without a stale success from an earlier test poisoning it.
func ResetBuildCacheForTest() {
	buildCacheMu.Lock()
	defer buildCacheMu.Unlock()
	buildCache = map[string]*buildCacheEntry{}
}
