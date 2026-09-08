package version

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/inferops/debark/core/manifest"
)

// TestApplyVCSStampFillsDateWhenCommitIsSet is the regression test for the
// bug where Date was only ever taken from the vcs stamp inside an "if Commit
// is empty" branch: a build whose -ldflags set Commit but not Date reported
// no build date at all, even though the go command had stamped one in.
func TestApplyVCSStampFillsDateWhenCommitIsSet(t *testing.T) {
	settings := []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "ffffffffffffffffffffffffffffffffffffffff"},
		{Key: "vcs.time", Value: "2026-09-03T00:00:00Z"},
		{Key: "vcs.modified", Value: "false"},
	}

	got := applyVCSStamp(Info{Commit: "deadbeef"}, settings)
	if got.Commit != "deadbeef" {
		t.Errorf("an explicit -X Commit must win over the vcs stamp: got %q", got.Commit)
	}
	if got.Date != "2026-09-03T00:00:00Z" {
		t.Errorf("Date should have been filled from vcs.time, got %q", got.Date)
	}
}

func TestApplyVCSStampNeverOverwritesLinkTimeValues(t *testing.T) {
	settings := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "stamped"},
		{Key: "vcs.time", Value: "1999-01-01T00:00:00Z"},
	}
	got := applyVCSStamp(Info{Commit: "ldflags", Date: "2026-01-01T00:00:00Z"}, settings)
	if got.Commit != "ldflags" || got.Date != "2026-01-01T00:00:00Z" {
		t.Errorf("link-time values were overwritten: %+v", got)
	}
}

func TestApplyVCSStampFillsBothWhenNeitherIsSet(t *testing.T) {
	settings := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abc123"},
		{Key: "vcs.time", Value: "2026-09-03T00:00:00Z"},
	}
	got := applyVCSStamp(Info{}, settings)
	if got.Commit != "abc123" || got.Date != "2026-09-03T00:00:00Z" {
		t.Errorf("stamp not applied to an empty Info: %+v", got)
	}
}

// TestToolIsStableWithinAProcess is the determinism guard that matters for
// bundles: Tool() lands in the signed manifest, so two calls in one build
// must not differ.
func TestToolIsStableWithinAProcess(t *testing.T) {
	a, b := Tool(), Tool()
	if a != b {
		t.Fatalf("Tool() is not stable within a process: %+v vs %+v", a, b)
	}
	if a.Name != Name {
		t.Errorf("Tool().Name = %q, want %q", a.Name, Name)
	}
	if a.Version != Version {
		t.Errorf("Tool().Version = %q, want %q", a.Version, Version)
	}
	if a.Edition != Edition {
		t.Errorf("Tool().Edition = %q, want %q", a.Edition, Edition)
	}
}

// TestDefaultsWithoutLdflags pins what a build made with no -ldflags at all
// reports, since that is what `go install` and every contributor's local
// `go build` produce.
func TestDefaultsWithoutLdflags(t *testing.T) {
	if Version == "" {
		t.Error("Version must never be empty; the no-ldflags default is \"dev\"")
	}
	if Edition != manifest.EditionCommunity && Edition != manifest.EditionOfficial && Edition != manifest.EditionEnterprise {
		t.Errorf("Edition %q is not one of the three defined editions", Edition)
	}
	if s := String(); !strings.HasPrefix(s, Name+" ") || !strings.HasSuffix(s, "("+Edition+")") {
		t.Errorf("String() = %q, want %q ... (%s)", s, Name, Edition)
	}
}

// TestBuildDigestMatchesTheRunningBinary proves build_digest is what it
// claims to be -- the SHA-256 of the executable that produced the manifest --
// rather than a value that merely looks like a digest.
func TestBuildDigestMatchesTheRunningBinary(t *testing.T) {
	got := BuildDigest()
	if got == "" {
		t.Skip("the running executable could not be read on this platform")
	}
	if got != BuildDigest() {
		t.Fatal("BuildDigest() is not stable across calls")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Skipf("read %s: %v", exe, err)
	}
	sum := sha256.Sum256(raw)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("BuildDigest() = %s, but the running executable hashes to %s", got, want)
	}
}
