package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/inferops/debark/core/dferr"
)

// Architecture and version detection.
//
// On a real target (Root == "" or "/") these come from running dpkg and
// apt-get, exactly like the prototype. Tests -- including on Windows, where
// neither binary exists -- point Root at a fixture tree instead, and nothing
// here may shell out in that mode: every value is read from a small recorded
// file under the fixture root, using the *same* parser the real command's
// output goes through. That keeps exactly one parsing path per field
// regardless of where the bytes came from.
//
// Architecture fixture files (checked in this order):
//
//  1. <root>/var/lib/dpkg/arch -- dpkg's own on-disk record (present on any
//     system that has ever run `dpkg --add-architecture`, or one hand-built
//     to look like it): first line is the native architecture, any further
//     lines are foreign architectures. This is the real format dpkg itself
//     reads and writes, so a fixture using it is exercising the same layout
//     `dpkg --print-architecture`/`--print-foreign-architectures` derive from.
//  2. <root>/etc/dpkg/arch -- a debark-only fallback for fixtures that
//     don't want to model dpkg's internal state file: same one-native-then-
//     foreign-per-line format, at a path no real dpkg installation uses, so
//     there is no ambiguity about which convention produced it.
//
// Version fixture files, same idea, holding exactly the first line the real
// command would print:
//
//   - <root>/etc/apt/apt-version -- what `apt-get -v` prints, e.g. "apt 2.8.3 (amd64)".
//   - <root>/etc/dpkg/version    -- what `dpkg --version` prints, e.g.
//     "Debian 'dpkg' package management program version 1.22.6 (amd64)."
const (
	dpkgArchFile       = "/var/lib/dpkg/arch"
	fixtureArchFile    = "/etc/dpkg/arch"
	fixtureAPTVersion  = "/etc/apt/apt-version"
	fixtureDpkgVersion = "/etc/dpkg/version"
)

// captureArchitectures returns the native and foreign dpkg architectures.
//
// The "am I looking at the live system or at a fixture root?" flag is spelled
// realSystem, not real: `real` is a predeclared builtin (the complex-number
// accessor), and shadowing a builtin in three signatures to save four
// characters is not a trade worth making.
func captureArchitectures(ctx context.Context, root string, realSystem bool) (arch string, foreign []string, err error) {
	if realSystem {
		return archFromDpkgCommand(ctx)
	}
	if data, rerr := os.ReadFile(hostPath(root, dpkgArchFile)); rerr == nil {
		a, f := parseArchLines(data)
		return a, f, nil
	}
	if data, rerr := os.ReadFile(hostPath(root, fixtureArchFile)); rerr == nil {
		a, f := parseArchLines(data)
		return a, f, nil
	}
	return "", nil, dferr.New(dferr.Environment,
		"snapshot: no architecture recorded (looked for %s and %s under root)",
		dpkgArchFile, fixtureArchFile)
}

// parseArchLines implements the shared format described above: first
// non-blank line is native, remaining non-blank lines are foreign, sorted for
// a stable Target.ForeignArchs.
func parseArchLines(data []byte) (native string, foreign []string) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if native == "" {
			native = line
			continue
		}
		foreign = append(foreign, line)
	}
	sort.Strings(foreign)
	return native, foreign
}

func archFromDpkgCommand(ctx context.Context) (string, []string, error) {
	native, err := runTrim(ctx, "dpkg", "--print-architecture")
	if err != nil {
		return "", nil, dferr.New(dferr.Environment, "snapshot: dpkg --print-architecture: %v", err).
			WithHint("this must run on a Debian/Ubuntu target, or pass a fixture Root")
	}
	var foreign []string
	if out, err := runTrim(ctx, "dpkg", "--print-foreign-architectures"); err == nil && out != "" {
		foreign = strings.Fields(out)
		sort.Strings(foreign)
	}
	return native, foreign, nil
}

// aptVersionLineRE matches apt-get -v's first line, e.g. "apt 2.8.3 (amd64)"
// or "apt 1.8.2.3".
var aptVersionLineRE = regexp.MustCompile(`^apt\s+(\S+)`)

// dpkgVersionLineRE matches dpkg --version's first line, e.g.
// `Debian 'dpkg' package management program version 1.22.6 (amd64).`
// The captured group must start with a digit: "version" is an ordinary
// English word, and an unanchored match on it alone can land on unrelated
// text (a warning message, say) that merely happens to contain it.
var dpkgVersionLineRE = regexp.MustCompile(`version\s+(\d\S*?)\.?\s*(?:\(|$)`)

func captureAPTVersion(ctx context.Context, root string, realSystem bool) (string, error) {
	var firstLine string
	var err error
	if realSystem {
		firstLine, err = runFirstLine(ctx, "apt-get", "-v")
		if err != nil {
			return "", err
		}
	} else {
		data, rerr := os.ReadFile(hostPath(root, fixtureAPTVersion))
		if rerr != nil {
			if os.IsNotExist(rerr) {
				return "", nil // not every fixture cares about exercising this field
			}
			return "", rerr
		}
		firstLine = firstNonBlankLine(data)
	}
	return parseAPTVersion(firstLine), nil
}

func parseAPTVersion(line string) string {
	if m := aptVersionLineRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
		return m[1]
	}
	return ""
}

func captureDpkgVersion(ctx context.Context, root string, realSystem bool) (string, error) {
	var firstLine string
	var err error
	if realSystem {
		firstLine, err = runFirstLine(ctx, "dpkg", "--version")
		if err != nil {
			return "", err
		}
	} else {
		data, rerr := os.ReadFile(hostPath(root, fixtureDpkgVersion))
		if rerr != nil {
			if os.IsNotExist(rerr) {
				return "", nil // not every fixture cares about exercising this field
			}
			return "", rerr
		}
		firstLine = firstNonBlankLine(data)
	}
	return parseDpkgVersion(firstLine), nil
}

func parseDpkgVersion(line string) string {
	if m := dpkgVersionLineRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
		return strings.TrimSuffix(m[1], ".")
	}
	return ""
}

func firstNonBlankLine(data []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			return l
		}
	}
	return ""
}

func runTrim(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func runFirstLine(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", dferr.Wrap(dferr.Environment, err, "snapshot: %s %s", name, strings.Join(args, " "))
	}
	return firstNonBlankLine(out), nil
}
