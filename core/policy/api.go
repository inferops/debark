// Frozen public API of the policy package.

package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/inferops/debark/core/dferr"
)

// Load reads a YAML or JSON policy file and returns an Evaluator. An empty
// path returns Empty().
//
// Format is detected by extension first (.json is JSON; .yaml/.yml is YAML),
// and by content otherwise (the file's first non-whitespace byte is '{' or
// '[' for JSON, anything else is parsed as YAML). YAML support is a small,
// deliberately narrow subset — see miniyaml.go's doc comment — sufficient for
// every field policy.Policy declares, not a general YAML parser: this
// project's go.mod carries no YAML dependency (the "no new dependencies"
// rule), and JSON's encoding/json already handles the JSON half natively.
func Load(path string) (Evaluator, error) {
	if path == "" {
		return Empty(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "policy: read %s", path)
	}
	p, err := parsePolicyFile(path, data)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "policy: parse %s", path)
	}
	return evaluator{p: p}, nil
}

// parsePolicyFile decodes data as JSON or YAML into a Policy, per Load's
// format-detection rule, and refuses anything debark.policy/v1 does not
// describe. Both branches check the file's keys BEFORE decoding and the
// decoded document afterwards — see validate.go for why a policy file that
// cannot be applied exactly as written must be refused rather than partly
// applied.
func parsePolicyFile(path string, data []byte) (*Policy, error) {
	var p Policy
	if looksLikeJSON(path, data) {
		keys, err := jsonObjectKeys(data)
		if err != nil {
			return nil, err
		}
		if err := checkPolicyKeys(keys); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		if err := validatePolicy(&p); err != nil {
			return nil, err
		}
		return &p, nil
	}
	m, err := parseMiniYAML(data)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// parseMiniYAML rejects a repeated key itself (a map cannot carry one),
	// so only the unknown-key half is left for checkPolicyKeys here. Sorting
	// keeps the message an operator sees stable across runs rather than
	// following Go's randomised map order.
	sort.Strings(keys)
	if err := checkPolicyKeys(keys); err != nil {
		return nil, err
	}
	// Round-trip through JSON: policy.Policy's `json` and `yaml` struct tags
	// are identical field-for-field (see iface.go), so encoding/json's own
	// tag-driven decoding does all the real field mapping; parseMiniYAML only
	// has to produce the right generic shape.
	jb, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(jb, &p); err != nil {
		return nil, err
	}
	if err := validatePolicy(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// looksLikeJSON applies Load's format-detection rule.
func looksLikeJSON(path string, data []byte) bool {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".json"):
		return true
	case strings.HasSuffix(lower, ".yaml"), strings.HasSuffix(lower, ".yml"):
		return false
	}
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

// Empty returns an Evaluator that finds nothing, so no caller needs a nil
// check.
func Empty() Evaluator { return empty{} }

type empty struct{}

func (empty) Evaluate(ctx context.Context, in Input) ([]Finding, error) { return nil, nil }

// LoadApprovedKeys reads an approved archive key fingerprint list: one
// fingerprint per line, # comments and blank lines ignored. Fingerprints are
// normalised on read (whitespace and an optional 0x prefix removed,
// upper-cased), so an operator may paste them in whatever form gpg printed —
// including `gpg --fingerprint`'s space-separated groups.
//
// Every line that survives must be a full v4 or v5 fingerprint, and the file
// must name at least one. Both refusals exist because this list is an
// allow-list whose FAILURE MODE IS SILENT in the direction that matters:
//
//   - A file that yields no fingerprints at all — empty, truncated, or every
//     line commented out during a debugging session and never restored —
//     used to load as an empty list, and an empty list means "no constraint"
//     (Input.ApprovedKeys' own doc comment). An operator who passed
//     --approved-keys got precisely no key checking and a build that
//     succeeded. "I listed no approved keys" cannot be allowed to mean
//     "every key is approved".
//   - A line that is not a full fingerprint could never match anything real,
//     so it silently contributed nothing to the list it appears to extend;
//     and a short key ID, which is the thing operators most often paste, is
//     forgeable (see normalizeFingerprint).
func LoadApprovedKeys(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, dferr.Wrap(dferr.Usage, err, "policy: read approved keys %s", path)
	}
	var out []string
	for n, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fp, ok := normalizeFingerprint(line)
		if !ok {
			return nil, dferr.New(dferr.Usage,
				"policy: approved keys %s: line %d: %q is not a full OpenPGP key fingerprint", path, n+1, line).
				WithHint("%s", fingerprintRule)
		}
		out = append(out, fp)
	}
	if len(out) == 0 {
		return nil, dferr.New(dferr.Usage,
			"policy: approved keys %s names no fingerprints; refusing rather than treating it as \"every key is approved\"", path).
			WithHint("list at least one approved fingerprint, or drop --approved-keys to not constrain archive keys")
	}
	return out, nil
}
