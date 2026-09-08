// Package policy evaluates a local, operator-owned policy file against a plan
// and turns it into findings. The community edition evaluates policy locally
// and nothing else; a commercial edition would administer policy centrally and
// consume exactly these findings.
package policy

import (
	"context"

	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
)

// SchemaVersion is the policy file schema.
const SchemaVersion = "debark.policy/v1"

// Evaluator turns a plan into findings.
type Evaluator interface {
	// Evaluate is a pure function: same input, same findings, no network, no
	// clock. A finding at SeverityDeny fails the build with exit 6.
	Evaluate(ctx context.Context, in Input) ([]Finding, error)
}

// Input is what a policy sees.
type Input struct {
	Snapshot *snapshot.Snapshot
	Plan     *resolve.Plan
	// ApprovedKeys is the operator's approved archive key fingerprint list,
	// uppercase hex. Empty means no constraint.
	ApprovedKeys []string
}

// Severity of a finding.
type Severity string

const (
	// SeverityInfo is recorded and printed, and changes nothing.
	SeverityInfo Severity = "info"
	// SeverityWarn is recorded, printed and carried into the lock and README.
	SeverityWarn Severity = "warn"
	// SeverityDeny fails the build with exit 6.
	SeverityDeny Severity = "deny"
)

// Finding is one policy result.
type Finding struct {
	// Rule is the rule's identifier from the policy file, or a built-in name
	// such as approved-keys.
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	// Packages names what triggered it.
	Packages []string `json:"packages,omitempty"`
	// Detail carries rule-specific context (the offending fingerprint, the
	// component, the URL).
	Detail map[string]string `json:"detail,omitempty"`
}

// Policy is the parsed policy file. Every field is optional: an empty policy
// is valid and finds nothing.
type Policy struct {
	SchemaVersion string `json:"schema_version" yaml:"schema_version"`

	// AllowComponents, when non-empty, is the exhaustive list of archive
	// components packages may come from (main, universe, ...). Anything else
	// produces a finding at DefaultSeverity.
	AllowComponents []string `json:"allow_components,omitempty" yaml:"allow_components,omitempty"`
	// DenyComponents lists components that must not appear.
	DenyComponents []string `json:"deny_components,omitempty" yaml:"deny_components,omitempty"`

	// AllowPackages and DenyPackages are name globs.
	AllowPackages []string `json:"allow_packages,omitempty" yaml:"allow_packages,omitempty"`
	DenyPackages  []string `json:"deny_packages,omitempty" yaml:"deny_packages,omitempty"`

	// RequireSignedPublisher denies any file whose publisher_verification is
	// url-unverified.
	RequireSignedPublisher bool `json:"require_signed_publisher,omitempty" yaml:"require_signed_publisher,omitempty"`

	// AllowURLInputs permits vendor URL inputs at all. Default true.
	AllowURLInputs *bool `json:"allow_url_inputs,omitempty" yaml:"allow_url_inputs,omitempty"`

	// ApprovedKeys is an inline approved archive key fingerprint list,
	// equivalent to --approved-keys.
	ApprovedKeys []string `json:"approved_keys,omitempty" yaml:"approved_keys,omitempty"`

	// DenyFlags denies any package carrying one of these lock flags, e.g.
	// network-postinst or dkms.
	DenyFlags []string `json:"deny_flags,omitempty" yaml:"deny_flags,omitempty"`

	// DefaultSeverity is applied to rules that do not state one. Default warn.
	DefaultSeverity Severity `json:"default_severity,omitempty" yaml:"default_severity,omitempty"`
}

// Deniable reports whether any finding is a denial.
func Deniable(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityDeny {
			return true
		}
	}
	return false
}
