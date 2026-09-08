package doctor

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/inferops/debark/core/distro"
	"github.com/inferops/debark/core/lock"
)

// --- network-postinst -------------------------------------------------

// checkNetworkPostinst scans a package's maintainer scripts, and — when
// unscanned is non-empty — reports that it could not.
//
// That second half exists because silence from this check is not neutral: the
// CLI renders it as NoNetworkActionFound, "no obvious network action found",
// and ar.go already calls a false clean result "the one outcome this package
// must never produce". Until now three separate paths produced exactly that.
// A .deb whose control member uses a compression this build cannot decode
// (control.tar.lzma, or any member name decompressMember does not recognise)
// set debFile.Unscannable, which nothing read; a .deb that failed to parse at
// all, or tripped one of ar.go's bounds, was skipped; and a lock Filename
// that poolPath refused as an escape was skipped too. In all three the
// package appeared in the report as one with no maintainer scripts, which is
// what a hostile bundle wants. Experiment E7 scanned 1,749 real Debian 12 and
// Ubuntu 24.04 packages and found 0 unscannable, so this fires on no real
// archive package.
//
// It is reported under the existing network-postinst check id rather than a
// new one: doctorreport.v1.schema.json's "check" enum is closed and frozen,
// and this is the check whose silence was misleading. Flag is left empty, so
// an unreadable package earns no lock flag and policy's deny_flags (which
// matches lock flags, not check ids) is unaffected.
func checkNetworkPostinst(pkg lock.Package, deb *debFile, unscanned string) []Finding {
	if unscanned != "" {
		return []Finding{{
			Check:    CheckNetworkPostinst,
			Severity: SeverityWarn,
			Package:  pkg.Name,
			Version:  pkg.Version,
			Message:  "maintainer scripts could not be read, so nothing here is evidence that this package is clean",
			Evidence: unscanned,
		}}
	}
	if deb == nil {
		return nil
	}
	var findings []Finding
	// A single line can legitimately carry more than one signal at once
	// (e.g. the word "curl" and a bare URL literal both matching
	// `curl https://example.com/x`) — scanScriptForNetwork reports each
	// signal separately, which is what experiment E7's own signal-count
	// tally wants, but a human-facing Finding per pattern on an already-
	// reported line would just be noise for the same warning. Dedupe to one
	// Finding per (script, line).
	seen := map[string]bool{}
	for _, script := range maintainerScripts {
		content, ok := deb.Scripts[script]
		if !ok {
			continue
		}
		for _, m := range scanScriptForNetwork(script, content) {
			key := fmt.Sprintf("%s:%d", m.Script, m.Line)
			if seen[key] {
				continue
			}
			seen[key] = true
			findings = append(findings, Finding{
				Check:    CheckNetworkPostinst,
				Severity: SeverityWarn,
				Package:  pkg.Name,
				Version:  pkg.Version,
				Message:  "maintainer script looks like it performs network activity; the target has none available at install time",
				Evidence: m.String(),
				Flag:     lock.FlagNetworkPostinst,
			})
		}
	}
	return findings
}

// --- snap-shim ----------------------------------------------------------

// snapShimTinyBytes is a generous upper bound on the size of a real
// transitional snap-shim .deb (Ubuntu's chromium-browser/firefox shims are a
// few KB); it only strengthens the maintainer-script signal - description
// text alone fires regardless of size.
const snapShimTinyBytes = 200 * 1024

var (
	reSnapInstall     = regexp.MustCompile(`\bsnap\s+install\b`)
	reTransitionalPkg = regexp.MustCompile(`(?i)\btransitional\s+package\b`)
)

func checkSnapShim(pkg lock.Package, deb *debFile) *Finding {
	if deb == nil {
		return nil
	}
	var scriptHit string
	for _, script := range maintainerScripts {
		content, ok := deb.Scripts[script]
		if !ok {
			continue
		}
		if loc := reSnapInstall.FindString(content); loc != "" && pkg.Size > 0 && pkg.Size <= snapShimTinyBytes {
			scriptHit = script + ": " + loc
			break
		}
	}
	desc := stanzaGet(deb.Control, "Description")
	transitional := reTransitionalPkg.MatchString(desc) && strings.Contains(strings.ToLower(desc), "snap")

	if scriptHit == "" && !transitional {
		return nil
	}
	evidence := scriptHit
	if transitional {
		if evidence != "" {
			evidence += "; "
		}
		evidence += "description: " + firstLine(desc)
	}
	return &Finding{
		Check:    CheckSnapShim,
		Severity: SeverityWarn,
		Package:  pkg.Name,
		Version:  pkg.Version,
		Message:  "transitional package that installs a snap; the software will not be available on an air-gapped target",
		Evidence: evidence,
		Flag:     lock.FlagSnapShim,
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// --- dkms-headers ---------------------------------------------------------

func isDKMSPackage(pkg lock.Package, deb *debFile) bool {
	if strings.Contains(strings.ToLower(pkg.Name), "dkms") {
		return true
	}
	return deb != nil && deb.HasDKMSConf
}

func checkDKMSHeaders(pkg lock.Package, deb *debFile, installed map[string]bool, kernelSuffixes []string) *Finding {
	if !isDKMSPackage(pkg, deb) {
		return nil
	}
	if len(installed) == 0 {
		// No dpkg_status content was supplied (Input.DpkgStatus empty) - the
		// snapshot is unavailable, so debark cannot confirm headers either
		// way. Say so rather than staying silent, since a DKMS package with
		// no visibility into the target's kernel is exactly the case this
		// check exists to catch.
		return &Finding{
			Check:    CheckDKMSHeaders,
			Severity: SeverityWarn,
			Package:  pkg.Name,
			Version:  pkg.Version,
			Message:  "dkms package, but no snapshot dpkg status was available to confirm matching linux-headers are installed on the target",
			Flag:     lock.FlagDKMS,
		}
	}
	if len(kernelSuffixes) == 0 {
		return &Finding{
			Check:    CheckDKMSHeaders,
			Severity: SeverityWarn,
			Package:  pkg.Name,
			Version:  pkg.Version,
			Message:  "dkms package, but the target's installed kernel could not be determined from the snapshot; verify matching linux-headers are installed",
			Flag:     lock.FlagDKMS,
		}
	}
	var missing []string
	for _, suf := range kernelSuffixes {
		want := "linux-headers-" + suf
		if !installed[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &Finding{
		Check:    CheckDKMSHeaders,
		Severity: SeverityWarn,
		Package:  pkg.Name,
		Version:  pkg.Version,
		Message:  "dkms package with no matching linux-headers installed on the target; the kernel module will not build there",
		Evidence: "missing " + strings.Join(missing, ", "),
		Flag:     lock.FlagDKMS,
	}
}

// --- redistribution ---------------------------------------------------------

func checkRedistribution(pkg lock.Package) *Finding {
	if flag, ok := distro.ComponentFlag(pkg.Origin.Component); ok {
		return &Finding{
			Check:    CheckRedistribution,
			Severity: SeverityWarn,
			Package:  pkg.Name,
			Version:  pkg.Version,
			Message:  fmt.Sprintf("package comes from the %s component; confirm you are authorised to redistribute it outside your organisation", pkg.Origin.Component),
			Evidence: redistributionEvidence(pkg.Origin.Component, pkg.SourcePackage),
			Flag:     flag,
		}
	}
	if pkg.Reason == lock.ReasonExternal {
		origin := firstNonEmpty(pkg.Origin.URI, pkg.Origin.LocalPath, pkg.Filename)
		return &Finding{
			Check:    CheckRedistribution,
			Severity: SeverityWarn,
			Package:  pkg.Name,
			Version:  pkg.Version,
			Message:  "vendor-supplied .deb, not from a distro archive; confirm you are authorised to redistribute it and record its corresponding-source obligations",
			Evidence: redistributionEvidence(origin, pkg.SourcePackage),
			Flag:     lock.FlagUserSupplied,
		}
	}
	return nil
}

func redistributionEvidence(origin, sourcePackage string) string {
	if sourcePackage == "" {
		return origin
	}
	return fmt.Sprintf("%s (source package: %s)", origin, sourcePackage)
}

// --- unverified-url ---------------------------------------------------------

func checkUnverifiedURL(pkg lock.Package) *Finding {
	if pkg.PublisherVerification != lock.VerifiedURLUnverified {
		return nil
	}
	return &Finding{
		Check:    CheckUnverifiedURL,
		Severity: SeverityWarn,
		Package:  pkg.Name,
		Version:  pkg.Version,
		Message:  "fetched over a URL with no publisher signature and no supplied digest; HTTPS is not provenance",
		Evidence: firstNonEmpty(pkg.Origin.URI, pkg.Origin.LocalPath),
	}
}

// --- held-package ---------------------------------------------------------

func checkHeldPackage(pkg lock.Package, held map[string]bool) *Finding {
	if !held[pkg.Name] {
		return nil
	}
	return &Finding{
		Check:    CheckHeldPackage,
		Severity: SeverityWarn,
		Package:  pkg.Name,
		Version:  pkg.Version,
		Message:  "the target's dpkg status shows this package on hold; installing the lock's version will override that hold",
		Evidence: "Status: hold ok installed",
	}
}

// --- dpkg_status parsing ---------------------------------------------------

// parseDpkgStatus turns a captured /var/lib/dpkg/status into an installed-name
// set and a held-name set. A package is installed when its Status field ends
// in "installed"; it is held when Status begins with "hold ".
func parseDpkgStatus(content []byte) (installed, held map[string]bool) {
	installed = map[string]bool{}
	held = map[string]bool{}
	if len(content) == 0 {
		return installed, held
	}
	stanzas, err := parseDeb822(bytes.NewReader(content))
	if err != nil {
		return installed, held
	}
	for _, s := range stanzas {
		name := stanzaGet(s, "Package")
		if name == "" {
			continue
		}
		status := stanzaGet(s, "Status")
		fields := strings.Fields(status)
		if len(fields) == 3 && fields[2] == "installed" {
			installed[name] = true
			if fields[0] == "hold" {
				held[name] = true
			}
		}
	}
	return installed, held
}

// kernelHeaderSuffixes finds the running/installed kernel flavour(s) from the
// installed-package set (there is no explicit kernel field in the frozen
// snapshot schema) by taking every installed linux-image-<suffix> package's
// suffix - real version suffixes (6.1.0-13-amd64) and meta-package flavours
// (generic, virtual) both work, since the matching linux-headers-<suffix>
// package follows the same naming convention either way.
func kernelHeaderSuffixes(installed map[string]bool) []string {
	var suffixes []string
	for name := range installed {
		if suf, ok := strings.CutPrefix(name, "linux-image-"); ok {
			suffixes = append(suffixes, suf)
		}
	}
	sort.Strings(suffixes)
	return suffixes
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
