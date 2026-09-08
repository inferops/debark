package engine

import (
	"context"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/fetch"
)

// dpkgDebPackageName reads the Package: control field out of a staged .deb
// file, so the engine can hand apt the package name it needs to resolve an
// external's own dependencies against the private staging repo.
//
// # History
//
// This used to shell out to `dpkg-deb -f FILE Package`, exactly the way the
// validated Bash prototype does it (download-packages.sh's deb_field). The
// change that introduced it flagged the real consequence at the time: external
// .deb staging happens on the host, before backend dispatch, so a Windows or
// macOS builder using URL or local-file inputs needed dpkg-deb installed on
// the host even when it used the container backend for resolution — which
// defeats the promise that `build` works on macOS and Windows with only a
// container runtime. pault.ag/go/debian was the documented pure-Go answer,
// but it was not a go.mod requirement when this file was first written
// (present in the local module cache, absent from go.mod/go.sum), and the
// brief is explicit that a genuine new-dependency need gets reported, not
// `go get`-ed around — hence the shell-out, as a correct-but-host-only
// stopgap.
//
// # Pure Go, without a second .deb parser
//
// pault.ag/go/debian is a real go.mod dependency now, so the shell-out (and
// the "dpkg-deb not found on PATH" environment error it could return) is
// gone. This does not, however, hand-roll a second ar/control.tar reader in
// this package: core/fetch already grew ReadControlInfo (core/fetch/control.go)
// for exactly this job — that work was asked to provide a shared .deb
// control-stanza reader — and it is the right one to reuse rather than
// duplicate:
//
//   - It is layered on pault.ag/go/debian/deb for the part worth delegating
//     (finding the right control.* ar member and decompressing it, whichever
//     of gzip/xz/bzip2/lzma/zstd dpkg-deb used), while parsing the resulting
//     deb822 paragraph itself, so relationship fields (Depends and the rest)
//     come back exactly as written in the file — no parse-then-restringify
//     round trip through a type this package does not control.
//   - It is cross-validated in core/fetch's own tests against pault.ag's
//     typed decode of the same bytes (crossvalidate_test.go) and against a
//     real dpkg-deb-produced xz-compressed control.tar (control_test.go), so
//     it is not just self-consistent with its own reader.
//
// Two .deb parsers that could quietly disagree about what a control stanza
// says would be a defect, not a convenience — this file has none.
//
// ReadControlInfo parses the whole control stanza in a single pass — package
// name, version, architecture and every relationship field, not just
// Package: — because that is what one read of the ar/tar structure already
// yields. Today's only caller (stageExternals, in inputs.go) needs just the
// package name to hand apt, so that is all this function surfaces; a future
// caller that wants the rest does not need a second reader or a second pass
// over the file, since core/fetch.ReadControlInfo(path) is already the
// one-read, everything-included call, and this package already imports
// core/fetch for its other input-handling needs.
//
// # Why this keeps its old name
//
// vars.go's injectable debPackageName var — the seam this package's tests
// substitute a fake into — is declared as `debPackageName = dpkgDebPackageName`.
// This function keeps that exact name and its original
// func(context.Context, string) (string, error) signature so that
// assignment, and every test that reassigns debPackageName instead of this
// function, keeps compiling and working unchanged.
func dpkgDebPackageName(ctx context.Context, path string) (string, error) {
	info, err := fetch.ReadControlInfo(path)
	if err != nil {
		return "", classify(err, dferr.Incomplete, "engine: %s: reading .deb control data", path)
	}
	return info.Package, nil
}
