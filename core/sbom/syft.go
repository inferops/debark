package sbom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// This file is the "optional syft exec for richer output" half of that
// split. It
// has NO caller in the shipped product: core/engine/sbom.go writes the
// bundle's sbom.cdx.json with the native FromLock/Build path in sbom.go, and
// nothing else in the tree calls BuildPreferSyft. It is kept because the
// SBOM design sanctions it, but it is reviewed and documented as what it is -- an
// unwired exec path that a future caller would inherit.
//
// # Read this before wiring it into a bundle
//
// BuildPreferSyft's syft branch returns syft's bytes verbatim, and those
// bytes are NOT deterministic: syft stamps a fresh random UUID into
// serialNumber and the wall clock into metadata.timestamp on every run, and
// describes whatever files it finds on disk rather than what the lock
// selected. Writing them into the bundle would break the byte-reproducibility
// the manifest and signature depend on (principle 2) and would decouple
// the SBOM from the lock, which is the document that actually says what
// crossed the gap. Anything that wants richer syft output has to normalise
// serialNumber and timestamp from the caller's Options, and reconcile syft's
// component list against lock.Packages, before the result may go in a bundle.
//
// The exec itself is deliberately narrow: the only caller-derived value that
// reaches the argv is target, and it is always prefixed with "dir:", so it
// can never be read as an option however it starts, and it is passed as one
// argv element with no shell involved. exec.LookPath is what decides whether
// syft runs at all; on Windows it reports ErrDot rather than silently
// resolving a syft.exe sitting in the working directory, so SyftAvailable
// returns false for that case.

// Source reports which writer actually produced a document: syft when the
// operator has it installed and it succeeded, native as the always-available
// fallback.
type Source string

const (
	SourceNative Source = "native"
	SourceSyft   Source = "syft"
)

// maxSyftOutput caps what will be read from syft's stdout. An SBOM for a
// bundle is a few megabytes at the very outside; a runaway or hostile syft on
// PATH must not be able to make debark allocate without bound.
const maxSyftOutput = 64 << 20

// SyftAvailable reports whether a syft binary is on PATH.
func SyftAvailable() bool { return syftAvailable() }

// syftAvailable is the seam BuildPreferSyft actually consults, so a test can
// exercise the syft branch on a machine that has no syft (every CI machine
// this project runs on) and, just as importantly, can prove the branch is NOT
// taken when it should not be. Without it those assertions silently pass by
// never running any syft code at all.
var syftAvailable = func() bool {
	_, err := exec.LookPath("syft")
	return err == nil
}

// runSyft executes syft and returns its stdout. It is a variable so tests can
// substitute a fake without touching the real PATH or spawning a process —
// the sandbox this runs in during CI may well not have syft installed, and
// this project does not add it as a build dependency.
var runSyft = func(ctx context.Context, target string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "syft", "scan", "dir:"+target, "-o", "cyclonedx-json")
	var stdout, stderr bytes.Buffer
	// Both streams are bounded: stderr goes into an error message, and an
	// unbounded error message is its own denial of service.
	cmd.Stdout = &limitedWriter{w: &stdout, n: maxSyftOutput}
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8 << 10}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sbom: syft: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// limitedWriter drops everything past n bytes instead of failing, so a
// too-chatty syft is treated as unusable output (the caller's JSON probe
// rejects a truncated document) rather than as a process error.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	if len(p) > l.n {
		if _, err := l.w.Write(p[:l.n]); err != nil {
			return 0, err
		}
		l.n = 0
		return len(p), nil
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// BuildPreferSyft produces a CycloneDX document for target (a directory —
// typically the bundle's repo/ tree): syft when it is installed and produces
// a valid CycloneDX document at the spec version this package writes, the
// native writer otherwise. It never returns an error just because syft is
// missing or fails — it falls back silently, as the task requires — but it
// does return an error if the native writer itself fails (bad Options/
// Component data), since that is a real bug to surface.
//
// target is only used for the syft path (syft inspects a directory itself);
// the native path uses opts and components exactly like Build.
//
// It has no caller in the shipped product, and its syft branch produces
// non-deterministic bytes: see this file's header before giving it one.
func BuildPreferSyft(ctx context.Context, target string, opts Options, components []Component) ([]byte, Source, error) {
	// An empty target would be sent as the bare "dir:", which syft resolves
	// against its own working directory: a scan of something nobody asked for.
	if target != "" && syftAvailable() {
		if out, err := runSyft(ctx, target); err == nil {
			if syftOutputUsable(out) {
				return out, SourceSyft, nil
			}
			// Not a CycloneDX document at the version this package claims to
			// write: fall through to native rather than shipping a file whose
			// name promises one thing and whose content says another.
		}
	}
	doc, err := Build(opts, components)
	if err != nil {
		return nil, SourceNative, err
	}
	out, err := doc.JSON()
	if err != nil {
		return nil, SourceNative, err
	}
	return out, SourceNative, nil
}

// syftOutputUsable decides whether syft's stdout may be shipped under the
// name sbom.cdx.json. bomFormat alone is not enough: syft happily emits older
// CycloneDX spec versions depending on its own version and flags, and a
// document declaring 1.4 would be validated against 1.6 by any consumer that
// trusts this package's SpecVersion constant.
func syftOutputUsable(out []byte) bool {
	var probe struct {
		BOMFormat   string `json:"bomFormat"`
		SpecVersion string `json:"specVersion"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return false
	}
	return probe.BOMFormat == BOMFormat && probe.SpecVersion == SpecVersion
}
