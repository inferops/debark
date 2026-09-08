package install

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/evidence"
)

// DefaultEvidencePath is where install.result records are appended when
// Options.EvidencePath is empty. It is a per-user XDG state path
// ($XDG_STATE_HOME, falling back to ~/.local/state, matching the config
// path conventions the CLI uses elsewhere), never a system-wide path install
// would need extra privilege to create beyond what running apt already
// requires. Nothing written here ever leaves the machine (step 6).
func DefaultEvidencePath() string {
	if v := os.Getenv("DEBARK_STATE_DIR"); v != "" {
		return filepath.Join(v, "install-log.ndjson")
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = os.TempDir()
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "debark", "install-log.ndjson")
}

// appendEvidenceLog appends one line to the local install evidence log: a
// debark.events/v1 Event, NDJSON, one record per install.result. This is
// deliberately not evidence.NewFileSink: that sink collects in memory and
// overwrites the whole file on Close, which is right for a single bundle's
// evidence.json but wrong for install's own log, which is a durable trail
// that must accumulate across every install this machine ever runs. A
// single os.O_APPEND write of one JSON line is atomic on every platform this
// matters on (the target is always Linux), so no locking is needed for
// the single-installer-at-a-time case this tool is built for.
func appendEvidenceLog(path string, ev evidence.Event) error {
	if ev.Schema == "" {
		ev.Schema = evidence.SchemaVersion
	}
	if ev.TS == "" {
		ev.TS = canonical.Time(time.Now())
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode evidence record: %w", err)
	}
	b = append(b, '\n')

	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
