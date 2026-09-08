package policy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferops/debark/core/resolve"
)

func TestLoad_EmptyPathReturnsEmpty(t *testing.T) {
	ev, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	findings, err := ev.Evaluate(context.Background(), Input{Plan: &resolve.Plan{Selections: []resolve.Selection{sel("x", withComponent("multiverse"))}}})
	if err != nil || findings != nil {
		t.Fatalf("Load(\"\") should behave like Empty(), got (%+v, %v)", findings, err)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestLoad_YAMLByExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	content := "schema_version: " + SchemaVersion + "\n" +
		"deny_components:\n" +
		"  - multiverse\n" +
		"  - non-free\n" +
		"require_signed_publisher: true\n" +
		"default_severity: deny\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	findings, err := ev.Evaluate(context.Background(), Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
		sel("vlc", withComponent("multiverse")),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Rule != "component-deny" || findings[0].Severity != SeverityDeny {
		t.Fatalf("unexpected findings from YAML-loaded policy: %+v", findings)
	}
}

func TestLoad_JSONByExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	content := `{
		"schema_version": "` + SchemaVersion + `",
		"deny_packages": ["bad*"],
		"default_severity": "warn"
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	findings, err := ev.Evaluate(context.Background(), Input{Plan: &resolve.Plan{Selections: []resolve.Selection{sel("badtool")}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Rule != "package-deny" {
		t.Fatalf("unexpected findings from JSON-loaded policy: %+v", findings)
	}
}

func TestLoad_JSONDetectedByContentWithoutExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.conf") // no .json/.yaml extension
	body := `{"schema_version": "` + SchemaVersion + `", "require_signed_publisher": true}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	findings, err := ev.Evaluate(context.Background(), Input{Plan: &resolve.Plan{Selections: []resolve.Selection{
		sel("vendor-tool", withVerification(vendorUnverified)),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Rule != "require-signed-publisher" {
		t.Fatalf("content-sniffed JSON policy did not evaluate as expected: %+v", findings)
	}
}

func TestLoad_YAMLDetectedByContentWithoutExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.conf")
	body := "schema_version: " + SchemaVersion + "\ndefault_severity: deny\ndeny_packages:\n  - bad*\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	findings, err := ev.Evaluate(context.Background(), Input{Plan: &resolve.Plan{Selections: []resolve.Selection{sel("badtool")}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Severity != SeverityDeny {
		t.Fatalf("content-sniffed YAML policy did not evaluate as expected: %+v", findings)
	}
}

func TestLoadApprovedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.txt")
	content := "" +
		"# approved archive keys\n" +
		"\n" +
		"aaaa1111bbbb2222cccc3333dddd4444eeee5555  # debian archive\n" +
		"BBBB2222CCCC3333DDDD4444EEEE5555FFFF6666\n" +
		"   \n" +
		"# comment-only line\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadApprovedKeys(path)
	if err != nil {
		t.Fatalf("LoadApprovedKeys: %v", err)
	}
	want := []string{
		"AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555",
		"BBBB2222CCCC3333DDDD4444EEEE5555FFFF6666",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadApprovedKeys_Missing(t *testing.T) {
	if _, err := LoadApprovedKeys(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// vendorUnverified exists only so this file does not need to import
// core/lock just for one constant already exercised via withVerification's
// signature in evaluate_test.go.
const vendorUnverified = "url-unverified"
