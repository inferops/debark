package snapshot

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/inferops/debark/core/dferr"
)

// Bounds and character rules for the snapshot strings a human ever sees.
//
// A snapshot is untrusted input, and several of its fields exist purely to be
// printed: Warnings, Labels, the target's codename and pretty name, the apt
// and dpkg versions. Nothing bounded how many there were, how long each was,
// or which characters they could contain -- so a captured "warning" could
// carry ESC[1A ESC[2K and, when the CLI printed the warning list under the
// digest line, scroll back over the digest and overwrite it with a forged
// one. That is aimed precisely at the human comparison docs/threat-model.md
// §6 asks a reviewer to perform, which makes the reviewer's own terminal
// the attack surface.
//
// The rule here is refusal, not repair: doValidate's contract is "decide
// whether a document can be trusted at all", the same family of failure as a
// digest mismatch, and a document whose display strings are trying to drive
// the terminal has already answered that question. Capture's own output is
// sanitised at the point it is produced (sanitizeCaptured) so an honest
// capture never writes a document this refuses.
//
// This bounds the snapshot side only. The printing side needs its own
// sanitiser -- internal/cli renders these strings, and no validation here can
// protect a caller that got its string from somewhere else.
const (
	// maxWarnings is generous next to what a real capture emits (a complete
	// Debian 12 target emits none; a badly broken one, a handful per missing
	// directory) while still bounding how much a document can push through a
	// terminal in one go.
	maxWarnings = 256
	// maxWarningLen bounds one warning. Real ones are well under 200 bytes;
	// this leaves room for an embedded path plus an error string.
	maxWarningLen = 4096
	// maxLabels, maxLabelKeyLen and maxLabelValueLen bound operator-supplied
	// metadata (hostname, site, ticket): a handful of short pairs.
	maxLabels        = 64
	maxLabelKeyLen   = 128
	maxLabelValueLen = 1024
	// maxDisplayStringLen bounds every other single-valued display string
	// (codename, pretty name, versions, tool identity, machine id).
	maxDisplayStringLen = 256
)

// hasControlChars reports whether s carries any character a terminal treats
// as a command rather than as text: C0 (including NUL, ESC, CR and LF), DEL,
// and the C1 range U+0080-U+009F (whose members include CSI, the 8-bit form
// of the ESC-[ that starts a cursor-movement sequence).
//
// The C0/DEL test is per byte and the C1 test per rune, deliberately: a raw
// 0x9B byte is not valid UTF-8, so a UTF-8 terminal will not act on it, while
// a properly encoded U+009B is two bytes that it will.
func hasControlChars(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7F {
			return true
		}
	}
	for _, r := range s {
		if r >= 0x80 && r <= 0x9F {
			return true
		}
	}
	return false
}

// checkDisplayStrings enforces the bounds and character rules above over
// every snapshot-sourced string that reaches an operator's terminal.
func checkDisplayStrings(s *Snapshot) error {
	single := []struct {
		field string
		value string
	}{
		{"tool.name", s.Tool.Name},
		{"tool.version", s.Tool.Version},
		{"tool.edition", s.Tool.Edition},
		{"target.distro_id", s.Target.DistroID},
		{"target.version_id", s.Target.VersionID},
		{"target.codename", s.Target.Codename},
		{"target.pretty_name", s.Target.PrettyName},
		{"target.apt_version", s.Target.APTVersion},
		{"target.dpkg_version", s.Target.DpkgVersion},
		{"target.machine_id", s.Target.MachineID},
	}
	for _, f := range single {
		if err := checkDisplayString(f.field, f.value, maxDisplayStringLen); err != nil {
			return err
		}
	}
	for _, r := range s.Redactions {
		if err := checkDisplayString("redactions", r, maxDisplayStringLen); err != nil {
			return err
		}
	}

	if len(s.Warnings) > maxWarnings {
		return dferr.New(dferr.Verification,
			"snapshot: %d warnings exceeds the limit of %d", len(s.Warnings), maxWarnings)
	}
	for i, w := range s.Warnings {
		if err := checkDisplayString("warnings["+strconv.Itoa(i)+"]", w, maxWarningLen); err != nil {
			return err
		}
	}

	if len(s.Labels) > maxLabels {
		return dferr.New(dferr.Verification,
			"snapshot: %d labels exceeds the limit of %d", len(s.Labels), maxLabels)
	}
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys) // a map: without this the refusal message is nondeterministic
	for _, k := range keys {
		if err := checkDisplayString("labels key", k, maxLabelKeyLen); err != nil {
			return err
		}
		if err := checkDisplayString("labels["+k+"]", s.Labels[k], maxLabelValueLen); err != nil {
			return err
		}
	}
	return nil
}

// The byte budget is named `limit`, not `max`: `max` is a predeclared builtin
// as of Go 1.21 and shadowing it buys nothing. Same in sanitizeCaptured below.
func checkDisplayString(field, value string, limit int) error {
	if len(value) > limit {
		return dferr.New(dferr.Verification,
			"snapshot: %s is %d bytes, over the %d-byte limit", field, len(value), limit)
	}
	if hasControlChars(value) {
		return dferr.New(dferr.Verification,
			"snapshot: %s contains control characters, which a terminal would act on rather than print", field)
	}
	return nil
}

// sanitizeCaptured makes one string safe to record in a document that
// checkDisplayStrings will later have to accept: control characters become
// spaces (never removed outright, so two words cannot silently merge into
// one) and the result is truncated on a rune boundary.
//
// This runs on the capture side, over values read from the target's own
// files -- an /etc/os-release PRETTY_NAME, an error string naming a file --
// so that a target which is already hostile produces a snapshot the builder
// can still open and read, rather than one that fails validation with the
// operator none the wiser about why. The builder-side refusal stays the real
// defence; this only keeps honest captures out of its way.
func sanitizeCaptured(s string, limit int) string {
	if s == "" {
		return s
	}
	if hasControlChars(s) {
		var b strings.Builder
		b.Grow(len(s))
		for _, r := range s {
			switch {
			case r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F):
				b.WriteByte(' ')
			default:
				b.WriteRune(r)
			}
		}
		s = strings.TrimSpace(b.String())
	}
	for len(s) > limit {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// sanitizeSnapshotStrings applies sanitizeCaptured to every display string a
// capture fills in, in one pass at the end of doCapture rather than at each
// assignment: the set of fields is exactly the set checkDisplayStrings
// checks, and keeping the two lists next to each other is what stops one from
// growing without the other.
func sanitizeSnapshotStrings(s *Snapshot) {
	if s == nil {
		return
	}
	s.Tool.Name = sanitizeCaptured(s.Tool.Name, maxDisplayStringLen)
	s.Tool.Version = sanitizeCaptured(s.Tool.Version, maxDisplayStringLen)
	s.Tool.Edition = sanitizeCaptured(s.Tool.Edition, maxDisplayStringLen)
	s.Target.DistroID = sanitizeCaptured(s.Target.DistroID, maxDisplayStringLen)
	s.Target.VersionID = sanitizeCaptured(s.Target.VersionID, maxDisplayStringLen)
	s.Target.Codename = sanitizeCaptured(s.Target.Codename, maxDisplayStringLen)
	s.Target.PrettyName = sanitizeCaptured(s.Target.PrettyName, maxDisplayStringLen)
	s.Target.APTVersion = sanitizeCaptured(s.Target.APTVersion, maxDisplayStringLen)
	s.Target.DpkgVersion = sanitizeCaptured(s.Target.DpkgVersion, maxDisplayStringLen)
	s.Target.MachineID = sanitizeCaptured(s.Target.MachineID, maxDisplayStringLen)

	if len(s.Warnings) > maxWarnings {
		// Keep the first maxWarnings-1 and say how many were dropped, rather
		// than truncating silently: the count is itself information about how
		// badly the capture went.
		dropped := len(s.Warnings) - (maxWarnings - 1)
		s.Warnings = append(s.Warnings[:maxWarnings-1:maxWarnings-1],
			"snapshot: "+strconv.Itoa(dropped)+" further warnings were dropped to keep the document bounded")
	}
	for i := range s.Warnings {
		s.Warnings[i] = sanitizeCaptured(s.Warnings[i], maxWarningLen)
	}

	if len(s.Labels) > 0 {
		labels := make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			labels[sanitizeCaptured(k, maxLabelKeyLen)] = sanitizeCaptured(v, maxLabelValueLen)
		}
		s.Labels = labels
	}
}

// Bounds on origin.assumed_installed. It is the only unbounded list in the
// format that reaches the target inside a bundle: install reads it back and
// prints a sample of the names it did not find. Everything else in this file
// exists because a captured string is attacker-influenced text that ends up
// in front of a human, and a package name is no different.
const (
	maxAssumedInstalled       = 65536
	maxAssumedInstalledLength = 256
)

// checkAssumedInstalled bounds and sanitises the assumed installed set.
//
// The count is capped generously — a full Ubuntu desktop closure is around
// two thousand entries, so this is a refusal of the absurd rather than a
// budget. The per-entry rules are the same ones every other display string in
// this file is held to, for the same reason: install prints these names, and
// a package name carrying an ESC sequence could scroll back over the digest a
// human was told to compare (threat model §6).
func checkAssumedInstalled(names []string) error {
	if len(names) > maxAssumedInstalled {
		return dferr.New(dferr.Verification,
			"snapshot: origin.assumed_installed has %d entries, more than the %d allowed", len(names), maxAssumedInstalled)
	}
	for _, n := range names {
		if len(n) > maxAssumedInstalledLength {
			return dferr.New(dferr.Verification,
				"snapshot: origin.assumed_installed entry is %d bytes, longer than the %d allowed", len(n), maxAssumedInstalledLength)
		}
		if hasControlChars(n) {
			return dferr.New(dferr.Verification,
				"snapshot: origin.assumed_installed entry %q contains control characters", n)
		}
	}
	return nil
}
