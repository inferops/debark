package harness

import (
	"os"
	"testing"
)

// RequireE2E skips t unless DEBARK_E2E=1 is set — the same gate
// docs/dev/contract-brief.md's requireLinuxAPT uses, applied here so a plain
// `go test ./...` never starts a container from this package or from
// test/e2e either (task constraint: "Guard the whole suite behind
// DEBARK_E2E=1").
func RequireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("DEBARK_E2E") == "" {
		t.Skip("needs Docker; set DEBARK_E2E=1 (see test/e2e/README.md)")
	}
}
