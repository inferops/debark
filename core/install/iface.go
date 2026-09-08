// Package install is the target side: the only part of debark that runs on
// the air-gapped machine. It must stay small enough to audit and to package in
// a distribution, and it must never touch the system's apt configuration.
package install

import (
	"context"

	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/verify"
)

// SchemaVersion is the install report schema, emitted by install --json.
const SchemaVersion = "debark.installreport/v1"

// Runner installs a verified bundle.
type Runner interface {
	// Plan verifies the bundle, checks preconditions and computes what would
	// change, without changing anything. It is what --status and --dry-run
	// call.
	Plan(ctx context.Context, bundlePath string, opts Options) (*Report, error)

	// Apply runs Plan and then performs the installation. It refuses to touch
	// the system if verification fails (exit 4) or the target does not match
	// (exit 7).
	Apply(ctx context.Context, bundlePath string, opts Options) (*Report, error)
}

// Options controls one install.
type Options struct {
	// Verify carries the key sources; verification always runs first
	// (ADR-008). AllowUnsigned there is the operator's explicit choice.
	Verify verify.Options

	// Upgrade runs full-upgrade before installing the requested set.
	Upgrade bool
	// All installs every package in the bundle rather than the lock's install
	// set.
	All bool
	// Only restricts the install to these package names (with their locked
	// versions). Empty means the lock's set.
	Only []string

	// KeepSource writes the bundle's .sources file into the system
	// permanently instead of using a temporary one.
	KeepSource bool
	// Fast adds dpkg --force-unsafe-io and noninteractive debconf.
	Fast bool
	// Dpkg bypasses apt: dpkg --unpack everything, then dpkg --configure -a.
	// It skips apt's ordering and conflict checks and says so.
	Dpkg bool

	// Yes assumes yes to apt's prompts. Without it, a non-TTY install refuses
	// rather than hanging.
	Yes bool

	// EvidencePath is where the install record is appended. Empty writes to
	// the default local log; nothing ever leaves the machine.
	EvidencePath string
}

// Report is the machine-readable install result.
type Report struct {
	SchemaVersion string `json:"schema_version"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at,omitempty"`
	BundlePath    string `json:"bundle_path"`
	BundleID      string `json:"bundle_id,omitempty"`

	// Verify is the verification report; install never proceeds without it.
	Verify *verify.Report `json:"verify,omitempty"`

	// Applied is false for Plan, --status and --dry-run.
	Applied bool `json:"applied"`
	// OK is true when the requested state was reached.
	OK bool `json:"ok"`

	// ToInstall, ToUpgrade and ToRemove are parsed from apt's simulation, in
	// name=version form.
	ToInstall []string `json:"to_install,omitempty"`
	ToUpgrade []string `json:"to_upgrade,omitempty"`
	ToRemove  []string `json:"to_remove,omitempty"`
	// AlreadyCurrent counts packages in the bundle the target already has at
	// the locked version.
	AlreadyCurrent int `json:"already_current"`

	// Target restates what was checked, and what was found.
	TargetExpected lock.Target `json:"target_expected"`
	TargetActual   lock.Target `json:"target_actual"`

	// BaseDivergence is present only for a bundle built from a *synthesized*
	// snapshot, and says how far this machine differs from the base that
	// snapshot assumed. Absent for the ordinary captured case. See
	// basedivergence.go.
	BaseDivergence *BaseDivergence `json:"base_divergence,omitempty"`

	// Warnings are non-fatal: codename mismatch, dpkg fallback used, holds
	// bypassed.
	Warnings []string `json:"warnings,omitempty"`
	// Problems are the reasons OK is false.
	Problems []string `json:"problems,omitempty"`
	// AptOutputDigest is the SHA-256 of the captured apt output, which is kept
	// in the local evidence log.
	AptOutputDigest string `json:"apt_output_digest,omitempty"`
}

// BaseDivergence is how far the machine this install is running on differs
// from the base a *synthesized* bundle was built against (ADR-014).
//
// It is present only when the bundle's snapshot.json says
// origin.kind = "synthesized". A captured snapshot is a measurement of this
// very machine, so there is no assumption to diverge from and this field is
// absent, along with the query that would have produced it. The reasoning,
// and why this is always a warning and never a refusal, is in
// basedivergence.go.
type BaseDivergence struct {
	// BaseID is the base definition the bundle was built against, e.g.
	// "ubuntu:26.04/desktop", restated from the snapshot's origin.base_id.
	BaseID string `json:"base_id,omitempty"`
	// Assumed is how many distinct packages that base claimed a stock
	// install already has — the size of the assumption, and the denominator
	// the operator needs to read Missing.
	Assumed int `json:"assumed"`
	// Missing is every one of those, name:arch, that dpkg does not report as
	// installed here, sorted. This is the COMPLETE list; the human warning
	// names only a bounded sample of it, which is the reason a machine
	// consumer needs the field at all.
	//
	// The opposite direction — packages this machine has that the base did
	// not assume — is deliberately not recorded: it means the bundle is
	// larger than it needed to be, which is harmless, and on a real machine
	// it is much the longer list of the two.
	Missing []string `json:"missing,omitempty"`
}
