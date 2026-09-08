package apt

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"pault.ag/go/debian/control"

	"github.com/inferops/debark/core/lock"
)

// This file's parsers were written against real apt output, not guessed:
// captured from apt 2.6.1 in debian:bookworm-slim, resolving against the
// checked-in testdata/real-targets/debian-12-state.tar.gz fixture, on
// 2026-09-03. See parse_test.go for the exact commands and the recorded
// goldens under testdata/golden/.

// splitAptQualifiedName splits an apt package reference that may carry an
// explicit architecture qualifier, e.g. "libc6:i386" -> ("libc6", "i386").
// A Debian package name never itself contains ':' (Policy §5.6.7), so any
// colon present unambiguously introduces the qualifier.
func splitAptQualifiedName(ref string) (name, arch string) {
	if i := strings.IndexByte(ref, ':'); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

func splitLines(b []byte) []string {
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

// --- apt-get update ----------------------------------------------------

// UpdateEntry is one Hit:/Get:/Ign:/Err: line from apt-get update.
type UpdateEntry struct {
	// Status is hit, get, ign or err.
	Status string
	// URI is the first whitespace-separated token of the line's remainder —
	// almost always the source URI. Suite is the second token with any
	// "/component" suffix removed, best-effort (apt's status-line shape
	// differs between an InRelease line and a per-index Packages line; see
	// ParseUpdate's doc).
	URI, Suite string
	// Detail is the full remainder of the line (and, for an Err: entry, its
	// indented reason line, appended), verbatim.
	Detail string
}

// GPGError is one "W: GPG error: ..." block and the "E: ... is not signed"
// line that follows it.
type GPGError struct {
	// Repository is the repository apt named (its own free-text label,
	// typically "<uri> <suite> InRelease").
	Repository string
	// NoPubkeys are the key ids named after "NO_PUBKEY", uppercase hex.
	NoPubkeys []string
	Detail    string
}

// UpdateResult is apt-get update's outcome, parsed for evidence and for a
// clear operator-facing error instead of a bare "exit 100".
type UpdateResult struct {
	Entries   []UpdateEntry
	GPGErrors []GPGError
	// Failed is true when any Err: entry, GPG error or top-level "E:" line
	// appeared, or the process exited non-zero.
	Failed bool
	// Summary joins every top-level "E: ..." line, for display.
	Summary string
}

// SourceOutcomes reduces the per-line Entries to a per-SOURCE verdict:
// fetched is the number of distinct sources apt ended up with an index for,
// failed the number it ended up with nothing for.
//
// The reduction is the point. apt prints one line per acquire ATTEMPT, and
// with Acquire::Retries in play one source produces several: with no network
// at all, Ubuntu 24.04 prints each source three times as "Ign:" and once as
// "Err:", so a bare len(Entries) of 16 describes four unreachable sources.
// That number is what the apt.update event reported, and it is why the event
// looked healthy on a machine with the network unplugged.
//
// A source is keyed by URI and suite, which is what an Entry carries.
// "Hit" and "Get" are terminal successes and win over anything else recorded
// for the same source, because apt only prints them once it has the file.
// "Err" counts as failed only if no success was seen for that source. "Ign"
// on its own is neither: apt uses it for an index that is absent by design
// (a missing optional Translation file, a suite with no Contents), and
// counting those as failures would report a healthy machine as broken.
func (u UpdateResult) SourceOutcomes() (fetched, failed int) {
	// Insertion-ordered keys, so the counts do not depend on map iteration
	// order even though only the totals are returned. Cheap, and it keeps
	// this function usable if a caller ever wants the names.
	type outcome struct{ ok, err bool }
	order := make([]string, 0, len(u.Entries))
	seen := make(map[string]*outcome, len(u.Entries))
	for _, e := range u.Entries {
		key := e.URI + " " + e.Suite
		o, ok := seen[key]
		if !ok {
			o = &outcome{}
			seen[key] = o
			order = append(order, key)
		}
		switch e.Status {
		case "hit", "get":
			o.ok = true
		case "err":
			o.err = true
		}
	}
	for _, key := range order {
		o := seen[key]
		switch {
		case o.ok:
			fetched++
		case o.err:
			failed++
		}
	}
	return fetched, failed
}

// Error renders a one-line, human explanation of why the update failed, or
// "" when it did not.
func (u UpdateResult) Error() string {
	if !u.Failed {
		return ""
	}
	var parts []string
	for _, ge := range u.GPGErrors {
		if len(ge.NoPubkeys) > 0 {
			parts = append(parts, fmt.Sprintf("GPG error on %s: missing key(s) %s", ge.Repository, strings.Join(ge.NoPubkeys, ", ")))
		} else {
			parts = append(parts, fmt.Sprintf("GPG error on %s: %s", ge.Repository, ge.Detail))
		}
	}
	if u.Summary != "" {
		parts = append(parts, u.Summary)
	}
	if len(parts) == 0 {
		return "apt-get update failed"
	}
	return strings.Join(parts, "; ")
}

var (
	updateLineRE = regexp.MustCompile(`^(Hit|Get|Ign|Err):\d+\s+(.*)$`)
	gpgErrorRE   = regexp.MustCompile(`^W:\s*GPG error:\s*(.*?):\s*(.*)$`)
	noPubkeyRE   = regexp.MustCompile(`NO_PUBKEY\s+([0-9A-Fa-f]+)`)
)

// ParseUpdate parses apt-get update output. Run plain "update" (not -qq) so
// these lines are actually printed — confirmed empirically that apt behaves
// this way by default once stdout is not a terminal, which is always true
// under Runner.
//
// Suite recovery from a Hit:/Get: line is best-effort: an InRelease line's
// remainder is "<uri> <suite> InRelease", but a per-index Packages line's is
// "<uri> <suite>/<component> <arch> Packages [<size>]" — both put the suite
// in the second field, which is all ParseUpdate relies on. Precise
// suite/component attribution for a resolved package comes from
// ReadPackagesIndex instead, which reads the fetched index files' own names.
//
// Failed is driven by apt's own exit code (plus a top-level "E:" verdict
// line as a defensive second signal), never by the presence of an
// individual Err: line or GPG-error block on its own: apt retries a failed
// per-item acquire internally (Acquire::Retries) and a retry that
// eventually succeeds is not a failure, even though the line for the failed
// attempt is still printed. Measured directly: a bare Packages file (no
// Release) over file:// intermittently logs "Err: ... Method gave a blank
// filename" for a few attempts before succeeding, with the whole run still
// exiting 0 — treating that Err: line as fatal would misreport a working
// closed-world check as failed. Individual Err:/GPGError entries are still
// recorded, for diagnostics, on every apt-get run regardless of the
// eventual outcome — apt is the oracle for whether the run failed, not this
// parser's own reading of one line in isolation.
func ParseUpdate(out Output) UpdateResult {
	var res UpdateResult
	lines := splitLines(out.Combined())
	var summary []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := updateLineRE.FindStringSubmatch(line); m != nil {
			status := strings.ToLower(m[1])
			rest := strings.TrimSpace(m[2])
			entry := UpdateEntry{Status: status, Detail: rest}
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				entry.URI = fields[0]
			}
			if len(fields) > 1 {
				s := fields[1]
				if idx := strings.IndexByte(s, '/'); idx >= 0 {
					s = s[:idx]
				}
				entry.Suite = s
			}
			if status == "err" && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "  ") {
				entry.Detail += " " + strings.TrimSpace(lines[i+1])
				i++
			}
			res.Entries = append(res.Entries, entry)
			continue
		}
		if m := gpgErrorRE.FindStringSubmatch(line); m != nil {
			ge := GPGError{Repository: strings.TrimSpace(m[1]), Detail: strings.TrimSpace(m[2])}
			for _, pk := range noPubkeyRE.FindAllStringSubmatch(m[2], -1) {
				ge.NoPubkeys = append(ge.NoPubkeys, strings.ToUpper(pk[1]))
			}
			res.GPGErrors = append(res.GPGErrors, ge)
			continue
		}
		if strings.HasPrefix(line, "E:") {
			res.Failed = true
			summary = append(summary, strings.TrimSpace(line))
		}
	}
	res.Summary = strings.Join(summary, "; ")
	if out.ExitCode != 0 {
		res.Failed = true
	}
	return res
}

// --- apt-get -s install / -s full-upgrade -------------------------------

// InstAction is one "Inst" or "Conf" line from a simulated install.
type InstAction struct {
	// Kind is "Inst" or "Conf".
	Kind string
	Name string
	// OldVersion is set when this is an upgrade ("Inst pkg [old] (new ...)").
	OldVersion string
	Version    string
	// Origins are the raw origin labels apt printed (e.g.
	// "Debian:12.15/oldstable"), comma-separated in the source line.
	Origins []string
	Arch    string
}

// SimulateResult is a parsed "apt-get -s install/full-upgrade" run.
type SimulateResult struct {
	Actions    []InstAction
	Unresolved []lock.Unresolved
	Failed     bool
	// Summary is the final "E: ..." line(s) apt printed, when it failed.
	Summary string
	// UnparsedActions is every line that announced itself as an Inst/Conf
	// action ("Inst" or "Conf" followed by whitespace, at the start of the
	// line) but that instConfRE could not read. It is deliberately NOT
	// folded into Failed: apt exited however it exited, and apt is the
	// oracle for that. It exists because the alternative — dropping the
	// line — is silent and unrecoverable: a dropped action never enters
	// the version set, is never fetched, and its absence is later reported
	// to the operator as the reassuring "already satisfied" case, so the
	// build succeeds with a package missing from the closure. Callers must
	// treat a non-empty UnparsedActions as a hard stop (see
	// simulateParseError in local.go), not as something to work around:
	// output this package cannot read is output whose meaning it does not
	// know.
	UnparsedActions []string
}

var (
	// instConfRE reads one simulated action line. The optional trailing
	// bracket group is apt's short-breaks suffix: a simulated transaction
	// that leaves something broken prints the broken set after the closing
	// parenthesis ("Inst a [1] (2 Debian:12/stable [amd64]) [b:amd64 ]").
	// None of the recorded goldens under testdata/golden/ contains one —
	// every captured run resolved cleanly — so this half is defensive
	// rather than measured; it is accepted anyway because the alternative
	// is to silently drop a real action line, which is exactly the failure
	// UnparsedActions exists to make impossible.
	// The trailing bracket groups are a REPEAT, not an option. apt appends one
	// per reason it has for touching the package, and a real `-s full-upgrade`
	// on Ubuntu 24.04 emits two:
	//
	//	Inst libpam-modules-bin [1.5.3-5ubuntu5.6] (1.5.3-5ubuntu5.7 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64]) [libpam-modules:amd64 on libpam-modules-bin:amd64] [libpam-modules:amd64 ]
	//
	// With `?` instead of `*` that line matched instConfPrefixRE and not this,
	// so ParseSimulate recorded it as an unreadable action and the build
	// refused with exit 5 — correctly, since dropping an action line would
	// silently drop a package from the closure, but it made --upgrades
	// unusable on that release. Found by the phased-updates fixture once it
	// was retargeted at the upgrade pass (2026-09-06).
	instConfRE = regexp.MustCompile(`^(Inst|Conf)\s+(\S+)(?:\s+\[([^\]]*)\])?\s+\((.*)\)(?:\s*\[[^\]]*\])*\s*$`)
	// instConfPrefixRE recognises a line that claims to be an action,
	// whether or not instConfRE can actually read it.
	instConfPrefixRE = regexp.MustCompile(`^(?:Inst|Conf)\s`)
	parenContentRE   = regexp.MustCompile(`^(\S+)\s+(.*?)\s*\[([^\]]+)\]\s*$`)
	unmetHeaderRE    = regexp.MustCompile(`^The following packages have unmet dependencies:\s*$`)
	unmetLineRE      = regexp.MustCompile(`^ (\S+)\s*:\s*(.*)$`)
	unableLocateRE   = regexp.MustCompile(`^E:\s*Unable to locate package (\S+)`)
	versionNotFndRE  = regexp.MustCompile(`^E:\s*Version '([^']*)' for '([^']*)' was not found`)
	missingDepRE     = regexp.MustCompile(`(?:Depends|Pre-Depends|Recommends):\s*([A-Za-z0-9][A-Za-z0-9+.:-]*)`)
)

// ParseSimulate parses "apt-get -s install ..." / "-s full-upgrade" output:
// the Inst/Conf lines that are the authoritative new-install-vs-upgrade
// signal, and the "unmet dependencies" block, from which the unsatisfiable
// package names are extracted into Unresolved (destined for lock.Unresolved
// / exit 3 territory).
func ParseSimulate(out Output) SimulateResult {
	var res SimulateResult
	lines := splitLines(out.Combined())
	inUnmet := false
	var curPkg string
	var curDetail []string
	flush := func() {
		if curPkg == "" {
			return
		}
		detail := strings.Join(curDetail, " ")
		var missing []string
		for _, m := range missingDepRE.FindAllStringSubmatch(detail, -1) {
			missing = append(missing, m[1])
		}
		res.Unresolved = append(res.Unresolved, lock.Unresolved{Input: curPkg, Kind: "package", Detail: detail, Missing: missing})
		curPkg, curDetail = "", nil
	}
	for _, line := range lines {
		if unmetHeaderRE.MatchString(strings.TrimSpace(line)) {
			inUnmet = true
			res.Failed = true
			continue
		}
		if inUnmet {
			if m := unmetLineRE.FindStringSubmatch(line); m != nil {
				flush()
				curPkg, curDetail = m[1], []string{m[2]}
				continue
			}
			trimmed := strings.TrimSpace(line)
			switch {
			case trimmed == "" || strings.HasPrefix(line, "E:"):
				flush()
				inUnmet = false
			case strings.HasPrefix(line, "  "):
				curDetail = append(curDetail, trimmed)
				continue
			default:
				flush()
				inUnmet = false
			}
		}
		if instConfPrefixRE.MatchString(line) && !instConfRE.MatchString(line) {
			res.UnparsedActions = append(res.UnparsedActions, strings.TrimRight(line, " \t"))
			continue
		}
		if m := instConfRE.FindStringSubmatch(line); m != nil {
			name, nameArch := splitAptQualifiedName(m[2])
			act := InstAction{Kind: m[1], Name: name, Arch: nameArch, OldVersion: m[3]}
			if pm := parenContentRE.FindStringSubmatch(m[4]); pm != nil {
				act.Version = pm[1]
				if pm[3] != "" {
					act.Arch = pm[3] // the bracketed arch is authoritative when present
				}
				for _, o := range strings.Split(pm[2], ",") {
					if o = strings.TrimSpace(o); o != "" {
						act.Origins = append(act.Origins, o)
					}
				}
			} else {
				act.Version = strings.TrimSpace(m[4])
			}
			res.Actions = append(res.Actions, act)
			continue
		}
		if m := unableLocateRE.FindStringSubmatch(line); m != nil {
			res.Failed = true
			res.Unresolved = append(res.Unresolved, lock.Unresolved{Input: m[1], Kind: "package", Detail: "unable to locate package"})
			continue
		}
		if m := versionNotFndRE.FindStringSubmatch(line); m != nil {
			res.Failed = true
			res.Unresolved = append(res.Unresolved, lock.Unresolved{Input: m[2], Kind: "package", Detail: fmt.Sprintf("version %s not found", m[1])})
			continue
		}
		if strings.HasPrefix(line, "E:") {
			res.Failed = true
			if res.Summary == "" {
				res.Summary = strings.TrimSpace(line)
			} else {
				res.Summary += "; " + strings.TrimSpace(line)
			}
		}
	}
	flush()
	if out.ExitCode != 0 {
		res.Failed = true
	}
	return res
}

// --- apt-get install --print-uris ---------------------------------------

// URIEntry is one line of "apt-get install --print-uris" output: the
// authoritative URI, filename, size and (weak, apt always prints MD5) hash
// for one file apt would fetch.
type URIEntry struct {
	// URI is exactly what apt printed, percent-encoding included — the real
	// URI apt would fetch, not a decoded approximation of it.
	URI      string
	Filename string
	Size     int64
	HashAlgo string
	Hash     string
}

var printURIRE = regexp.MustCompile(`^'([^']*)'\s+(\S+)\s+(\d+)\s+([A-Za-z0-9]+):([0-9a-fA-F]+)\s*$`)

// ParsePrintURIs parses "apt-get install --print-uris" output. It never
// downloads anything and never blocks on network access, so it is safe to
// run before the real --download-only fetch to learn the exact URI and
// filename apt will use for each file, ahead of time.
func ParsePrintURIs(out Output) []URIEntry {
	var res []URIEntry
	for _, line := range splitLines(out.Combined()) {
		m := printURIRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		size, _ := strconv.ParseInt(m[3], 10, 64)
		res = append(res, URIEntry{URI: m[1], Filename: m[2], Size: size, HashAlgo: m[4], Hash: strings.ToLower(m[5])})
	}
	return res
}

// --- apt-get -v / dpkg --version -----------------------------------------

var (
	aptVersionRE  = regexp.MustCompile(`^apt\s+(\S+)`)
	dpkgVersionRE = regexp.MustCompile(`(?i)version\s+(\S+)`)
)

// ParseAptGetVersion extracts the version token from "apt-get -v" (or
// "apt -v") output, e.g. "apt 2.6.1 (amd64)" -> "2.6.1".
func ParseAptGetVersion(out []byte) (string, bool) {
	lines := splitLines(out)
	if len(lines) == 0 {
		return "", false
	}
	m := aptVersionRE.FindStringSubmatch(strings.TrimSpace(lines[0]))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ParseDpkgVersion extracts the version token from "dpkg --version" output,
// e.g. "Debian 'dpkg' package management program version 1.21.23 (amd64)."
// -> "1.21.23".
func ParseDpkgVersion(out []byte) (string, bool) {
	lines := splitLines(out)
	if len(lines) == 0 {
		return "", false
	}
	m := dpkgVersionRE.FindStringSubmatch(lines[0])
	if m == nil {
		return "", false
	}
	return strings.TrimRight(m[1], "."), true
}

// --- var/lib/apt/lists/*_Packages ----------------------------------------

// PackagesIndexEntry is one stanza from a fetched Packages index, tagged with
// the suite and component recovered from the index file's own name.
type PackagesIndexEntry struct {
	control.BinaryIndex
	Suite     string
	Component string
	// IndexFile is the lists/ file's base name, for diagnostics.
	IndexFile string
}

// Essential reports the stanza's Essential field (apt/dpkg only ever writes
// "Essential: yes"; the field is entirely absent otherwise).
func (e PackagesIndexEntry) Essential() bool {
	return strings.EqualFold(strings.TrimSpace(e.Values["Essential"]), "yes")
}

var packagesIndexNameRE = regexp.MustCompile(`_dists_(.+)_binary-[^_]+_Packages$`)

// deriveSuiteComponent recovers (suite, component) from a fetched index
// file's base name. apt names it
// "<mangled-source-URI>_dists_<suite>_<component>_binary-<arch>_Packages"
// (URItoFileName replaces '/' with '_'); everything between the literal
// "_dists_" marker and "_binary-<arch>_Packages" is "<suite>_<component>",
// split at its last underscore since a component name never contains one
// (main, contrib, non-free, non-free-firmware, universe, restricted,
// multiverse) while a suite name uses hyphens, not underscores, in every
// real Debian or Ubuntu archive (bookworm-updates, noble-security, ...).
// Confirmed against real fetched file names in testdata/golden/.
func deriveSuiteComponent(filename string) (suite, component string, ok bool) {
	m := packagesIndexNameRE.FindStringSubmatch(filename)
	if m == nil {
		return "", "", false
	}
	mid := m[1]
	i := strings.LastIndexByte(mid, '_')
	if i < 0 {
		return mid, "", true
	}
	return mid[:i], mid[i+1:], true
}

// ReadPackagesIndex reads every plain-text "*_Packages" file directly under
// listsDir and returns every stanza. It requires the private root to have
// been updated with Acquire::GzipIndexes=false (buildOptions always sets
// this) so the fetched indices are plain deb822 text; a mirror offering a
// compression apt cannot itself decompress is out of scope, same as for the
// prototype.
func ReadPackagesIndex(listsDir string) ([]PackagesIndexEntry, error) {
	matches, err := filepath.Glob(filepath.Join(listsDir, "*_Packages"))
	if err != nil {
		return nil, fmt.Errorf("apt: glob %s: %w", listsDir, err)
	}
	sort.Strings(matches)
	var entries []PackagesIndexEntry
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("apt: read %s: %w", path, err)
		}
		base := filepath.Base(path)
		suite, component, _ := deriveSuiteComponent(base)
		stanzas, err := control.ParseBinaryIndex(bufio.NewReader(bytes.NewReader(data)))
		if err != nil {
			return nil, fmt.Errorf("apt: parse %s: %w", path, err)
		}
		for _, s := range stanzas {
			entries = append(entries, PackagesIndexEntry{BinaryIndex: s, Suite: suite, Component: component, IndexFile: base})
		}
	}
	return entries, nil
}
