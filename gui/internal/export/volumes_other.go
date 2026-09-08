//go:build !linux && !windows

package export

// Fallback for every other GOOS.
//
// The product targets Linux; Windows is built but unsupported. Rather than
// leave the package unbuildable on macOS or the BSDs — which would break `go
// build ./...` for anyone who happens to run it there, and would break a
// cross-compile check in CI — enumeration degrades to an explicit,
// recognisable error.
//
// It returns ErrUnsupportedPlatform rather than an empty list on purpose. An
// empty list is indistinguishable from "no drives are plugged in", which
// would show the operator an empty chooser and no explanation. A named
// sentinel lets the caller fall back to a plain directory picker and say why.
//
// A getmntinfo(2)-based implementation would not be hard to add here, but
// unsupported code that compiles is a maintenance liability without a
// machine running its tests, so it is deliberately absent.

func listVolumes() ([]Volume, error) {
	return nil, ErrUnsupportedPlatform
}

// freeSpaceOne has no implementation here, for the same reason listVolumes
// does not: unsupported code that compiles is a maintenance liability without
// a machine running its tests. A statfs(2) call through golang.org/x/sys/unix
// would work on macOS and the BSDs and is deliberately absent.
//
// Answering "not measured" rather than zero is the whole point of the second
// return value: FreeSpaceAt turns this into FreeSpaceUnknown, so Plan warns
// that it cannot promise the bundle will fit instead of refusing the export
// as though the disk were full.
func freeSpaceOne(string) (uint64, bool) {
	return 0, false
}
