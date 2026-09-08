package install

import (
	"context"
	"strings"
	"testing"
)

func TestFakeExecSmoke(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("FAKE_ARCH", "riscv64")
	log := newFakeLogFile(t)

	out, err := runProcess(context.Background(), "dpkg", []string{"--print-architecture"}, buildEnv(false))
	if err != nil {
		t.Fatalf("runProcess: %v (output: %s)", err, out.Combined)
	}
	got := strings.TrimSpace(string(out.Combined))
	if got != "riscv64" {
		t.Fatalf("fake dpkg --print-architecture = %q, want riscv64", got)
	}

	entries := fakeArgvLog(t, log)
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1: %v", len(entries), entries)
	}
	if len(entries[0]) < 2 || entries[0][1] != "--print-architecture" {
		t.Fatalf("logged argv = %v", entries[0])
	}
}
