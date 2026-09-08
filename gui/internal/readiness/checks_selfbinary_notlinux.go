//go:build !linux

package readiness

import "context"

// selfBinaryCheck lives here, behind !linux, rather than beside the rest of
// the self-binary probe in checks_selfbinary.go.
//
// The check asks whether a Linux debark exists to mount into a container.
// Only the two non-Linux registries need it — checks_windows.go and
// checks_other.go — because on Linux the host's own debark is already a
// Linux ELF and there is nothing to go looking for. So on Linux this wrapper
// had no callers, and golangci-lint's `unused` said so: the package was clean
// on Windows and red on Linux, which is the shipping platform.
//
// Everything it calls stays in checks_selfbinary.go with no build tag,
// deliberately. probeSelfBinary, decideSelfBinary and their table tests are
// pure functions over an observation and are worth running on every platform,
// including the one that never registers the row — the bug they exist to
// prevent was a Windows-only path nobody could test.
func selfBinaryCheck(ctx context.Context, o Options) Result {
	return decideSelfBinary(probeSelfBinary(ctx, o))
}
