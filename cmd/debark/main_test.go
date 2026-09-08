package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Isolate from the real user's config the same way internal/cli's own
	// TestMain does — this binary's tests call run() directly too.
	dir, err := os.MkdirTemp("", "debark-cmd-tests-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	os.Setenv("DEBARK_CONFIG", filepath.Join(dir, "unused-default-config.json"))
	os.Exit(m.Run())
}

func TestRunHelpPrintsFullTree(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"--help"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb.String())
	}
	for _, want := range []string{
		"snapshot", "build", "verify", "install", "inspect", "doctor",
		"store", "config", "version", "completion", "keygen",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--help output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunVersionJSON(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"version", "--json"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb.String())
	}
	if !strings.Contains(out.String(), `"name": "debark"`) {
		t.Errorf("version --json = %s", out.String())
	}
}

func TestRunUnknownCommandExitsOne(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"not-a-real-command"}, strings.NewReader(""), &out, &errb)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %q", code, errb.String())
	}
}
