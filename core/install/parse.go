package install

import (
	"bufio"
	"bytes"
	"sort"
	"strings"
)

// parseAptSim parses the LC_ALL=C output of `apt-get -s install ...` (or
// `-s full-upgrade`) into the three buckets Report carries, each rendered as
// "name=version". apt's simulation output uses three action verbs:
//
//	Inst name (version ...)              a fresh install
//	Inst name [oldversion] (version ...) an upgrade (or downgrade) of an
//	                                      already-installed package
//	Conf name (version ...)              apt will configure it — this always
//	                                      shadows a preceding Inst line for
//	                                      the same package, so it carries no
//	                                      information Inst didn't already
//	                                      give and is recognised (so it is
//	                                      never mistaken for a problem line)
//	                                      but not double-counted
//	Remv name [version]                  a removal, e.g. forced by a
//	                                      Conflicts/Breaks relationship
//
// Recorded, real apt output for every one of these shapes lives under
// testdata/ (golden files, table-driven tests).
func parseAptSim(output []byte) (toInstall, toUpgrade, toRemove, problems []string) {
	seenInstall := map[string]bool{}
	seenUpgrade := map[string]bool{}
	seenRemove := map[string]bool{}

	// inUnmet/unmetKept track apt's "The following packages have unmet
	// dependencies:" block, whose indented body lines are the only place apt
	// says WHICH package and WHICH dependency is unsatisfiable. See
	// isUnmetHeaderLine/isUnmetDetailLine below for the measurement that added
	// this.
	inUnmet := false
	unmetKept := 0

	sc := bufio.NewScanner(bytes.NewReader(output))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		raw := strings.TrimRight(sc.Text(), "\r")
		line := strings.TrimSpace(raw)

		// The unmet block is handled before the switch because it is the one
		// part of apt's output whose meaning depends on the LINE'S
		// INDENTATION, not on its own prefix: " zlib1g:i386 : Depends: ..."
		// looks like unremarkable free text on its own and is recognisable
		// only as the body of the header line above it.
		if inUnmet {
			switch {
			case line == "", strings.HasPrefix(line, "E:"):
				inUnmet = false // block over; fall through to the switch below
			case isUnmetDetailLine(raw):
				if unmetKept < maxUnmetDetailLines {
					problems = append(problems, line)
					unmetKept++
				} else if unmetKept == maxUnmetDetailLines {
					problems = append(problems, "... (further unmet dependency lines omitted; see the captured apt output)")
					unmetKept++
				}
				continue
			default:
				inUnmet = false
			}
		}

		switch {
		case strings.HasPrefix(line, "Inst "):
			name, version, upgrade, ok := parseInstLine(line)
			if !ok {
				continue
			}
			nv := nameVersion(name, version)
			if upgrade {
				if !seenUpgrade[nv] {
					seenUpgrade[nv] = true
					toUpgrade = append(toUpgrade, nv)
				}
			} else {
				if !seenInstall[nv] {
					seenInstall[nv] = true
					toInstall = append(toInstall, nv)
				}
			}

		case strings.HasPrefix(line, "Remv "):
			name, version, ok := parseRemvLine(line)
			if !ok {
				continue
			}
			nv := nameVersion(name, version)
			if !seenRemove[nv] {
				seenRemove[nv] = true
				toRemove = append(toRemove, nv)
			}

		case strings.HasPrefix(line, "Conf "):
			// Informational only: every Conf mirrors a preceding Inst for the
			// same package. Recognised explicitly so it is never picked up by
			// the problem-line detection below.

		case isUnmetHeaderLine(line):
			// The header on its own says only "something is unsatisfiable".
			// It is kept because it introduces the lines that follow, and the
			// flag is what makes those lines readable at all.
			problems = append(problems, line)
			inUnmet = true
			unmetKept = 0

		case strings.HasPrefix(line, "E: "),
			strings.Contains(line, "Unable to correct problems"),
			strings.Contains(line, "unmet dependencies"):
			problems = append(problems, line)
		}
	}

	sort.Strings(toInstall)
	sort.Strings(toUpgrade)
	sort.Strings(toRemove)
	return toInstall, toUpgrade, toRemove, problems
}

func nameVersion(name, version string) string {
	if version == "" {
		return name
	}
	return name + "=" + version
}

// parseInstLine parses one "Inst ..." line. version is always the *new*
// version (the one in the required trailing parenthesis); upgrade is true
// exactly when a bracketed old version preceded it.
func parseInstLine(line string) (name, version string, upgrade, ok bool) {
	rest := strings.TrimPrefix(line, "Inst ")
	if rest == line {
		return "", "", false, false
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return "", "", false, false
	}
	name = fields[0]
	i := 1
	if strings.HasPrefix(fields[i], "[") {
		upgrade = true
		// dpkg version strings never contain whitespace, so this normally
		// terminates on the same field; the loop is defensive only.
		for i < len(fields) && !strings.HasSuffix(fields[i], "]") {
			i++
		}
		i++
	}
	if i >= len(fields) {
		return "", "", false, false
	}
	version = strings.TrimPrefix(fields[i], "(")
	if version == "" {
		return "", "", false, false
	}
	return name, version, upgrade, true
}

// parseRemvLine parses one "Remv name [version]" line. The version is
// optional in principle (apt always prints it in practice, since a
// removal implies a previously-installed, versioned package).
func parseRemvLine(line string) (name, version string, ok bool) {
	rest := strings.TrimPrefix(line, "Remv ")
	if rest == line {
		return "", "", false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", "", false
	}
	name = fields[0]
	if len(fields) >= 2 {
		v := strings.TrimPrefix(fields[1], "[")
		v = strings.TrimSuffix(v, "]")
		version = v
	}
	return name, version, true
}

// --- apt's "unmet dependencies" block --------------------------------------
//
// WHY these two helpers exist. Measured verbatim from a real container run,
// 2026-09-06, `apt-get -s install` against a bundle whose i386 closure was
// incomplete:
//
//	The following packages have unmet dependencies:
//	 zlib1g:i386 : Depends: libc6:i386 (>= 2.4) but it is not installable
//	E: Unable to correct problems, you have held broken packages.
//
// The problem filter used to keep only lines starting "E: " or containing
// "unmet dependencies"/"Unable to correct problems", which kept the first and
// third lines and dropped the second - the ONE line that names which package
// and which dependency. What reached the operator was "the following packages
// have unmet dependencies; E: Unable to correct problems, you have held
// broken packages", a report of a blocker with no subject, and the failure
// mode that follows is an operator who cannot tell an incomplete bundle from
// a mis-targeted one without re-running the whole install by hand.
//
// The block-boundary rule (a blank line or an "E:" line ends it; indented
// lines are its body) is deliberately the same rule core/apt's ParseSimulate
// applies to the same block, so the two parsers cannot disagree about where
// apt's answer stops - core/apt turns the body into structured
// lock.Unresolved entries for the build side, this keeps it as the text an
// operator reads on the target. core/apt is not edited here; only its
// behaviour is matched.

// maxUnmetDetailLines bounds how many body lines are kept. Report.Problems is
// joined into an error message and written to the durable evidence log, so a
// pathological block (one line per package in a large closure) must not turn
// into an unbounded transcript. Twenty is far more than any measured failure
// has produced and still fits in a message a human reads.
const maxUnmetDetailLines = 20

// isUnmetHeaderLine recognises the block header. Prefix rather than exact
// match: apt has printed this line with and without trailing whitespace, and
// the caller has already trimmed.
func isUnmetHeaderLine(line string) bool {
	return strings.HasPrefix(line, "The following packages have unmet dependencies")
}

// isUnmetDetailLine reports whether raw (UNtrimmed - the indentation is the
// signal) is a body line of the unmet block: apt indents every one of them,
// by one space for a package line and more for its continuations.
func isUnmetDetailLine(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	return strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")
}
