package snapshot

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/inferops/debark/core/canonical"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/version"
)

// Capture's failure contract: never require root, and never fail because a
// *configuration* file is missing or unreadable -- that becomes a Warning,
// per the package doc and CaptureOptions. It does fail (dferr.Environment)
// when the root does not look like a Debian/Ubuntu system at all: without an
// architecture or an installed-package list there is nothing to snapshot,
// the same hard stop the bash prototype makes with its own leading
// `command -v dpkg || die`. dferr.Usage is reserved for Root itself being
// unusable (not a directory) -- the caller's mistake, not the target's.
func doCapture(ctx context.Context, opts CaptureOptions) (*Snapshot, *FileSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, dferr.Wrap(dferr.Usage, err, "snapshot: capture cancelled")
	}

	root := opts.Root
	realSystem := root == "" || root == "/"
	if root == "" {
		root = "/"
	}
	if realSystem && runtime.GOOS != "linux" {
		return nil, nil, dferr.New(dferr.Environment,
			"snapshot: capturing the real system requires Linux (dpkg/apt); this host is %s", runtime.GOOS).
			WithHint("point CaptureOptions.Root at a fixture tree, or run debark on the target itself")
	}
	if !realSystem {
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			return nil, nil, dferr.New(dferr.Usage, "snapshot: root %q is not a directory", root).
				WithHint("Root must be a directory laid out like a filesystem root (etc/, var/lib/dpkg/, ...)")
		}
	}

	fs := &FileSet{Bytes: map[string][]byte{}}
	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	warnStr := func(w string) {
		if w != "" {
			warnings = append(warnings, w)
		}
	}

	s := &Snapshot{
		SchemaVersion: SchemaVersion,
		CreatedAt:     canonical.Time(time.Now()),
		Tool:          Tool{Name: version.Name, Version: version.Version, Edition: version.Edition},
		// This is the one producer that measures a real machine (ADR-014).
		Origin: Origin{Kind: OriginCaptured},
	}

	// Identity.
	if f, w := captureOptionalFile(root, "/etc/os-release", fs); f != nil {
		s.Target.OSRelease = f
		vals := parseOSRelease(fs.Bytes[f.ArchivePath])
		s.Target.DistroID = vals["ID"]
		s.Target.VersionID = vals["VERSION_ID"]
		s.Target.Codename = vals["VERSION_CODENAME"]
		s.Target.PrettyName = vals["PRETTY_NAME"]
	} else {
		warnStr(w)
	}

	arch, foreign, err := captureArchitectures(ctx, root, realSystem)
	if err != nil {
		return nil, nil, err
	}
	s.Target.Arch = arch
	s.Target.ForeignArchs = foreign

	if v, verr := captureAPTVersion(ctx, root, realSystem); verr == nil {
		s.Target.APTVersion = v
	} else {
		warn("snapshot: apt-get -v: %v", verr)
	}
	if v, verr := captureDpkgVersion(ctx, root, realSystem); verr == nil {
		s.Target.DpkgVersion = v
	} else {
		warn("snapshot: dpkg --version: %v", verr)
	}

	if data, merr := os.ReadFile(hostPath(root, "/etc/machine-id")); merr == nil {
		s.Target.MachineID = strings.TrimSpace(string(data))
	} else if !os.IsNotExist(merr) {
		warn("snapshot: /etc/machine-id: %v", merr)
	}

	// Installed set: fatal if absent, there is nothing to snapshot without it.
	statusRec, serr := captureFile(root, "/var/lib/dpkg/status", fs)
	if serr != nil {
		return nil, nil, dferr.New(dferr.Environment, "snapshot: /var/lib/dpkg/status: %v", serr).
			WithHint("this does not look like a Debian/Ubuntu system (or fixture root)")
	}
	s.DpkgStatus = statusRec
	s.InstalledCount = countInstalled(fs.Bytes[statusRec.ArchivePath])

	// apt sources.
	var sourceFiles []File
	if f, w := captureOptionalFile(root, "/etc/apt/sources.list", fs); f != nil {
		sourceFiles = append(sourceFiles, *f)
	} else {
		warnStr(w)
	}
	dirFiles, dirWarns := captureDir(root, "/etc/apt/sources.list.d", isSourcesListName, fs)
	sourceFiles = append(sourceFiles, dirFiles...)
	for _, w := range dirWarns {
		warnStr(w)
	}
	sortFilesByPath(sourceFiles)
	s.APT.Sources = sourceFiles

	// apt preferences.
	var prefFiles []File
	if f, w := captureOptionalFile(root, "/etc/apt/preferences", fs); f != nil {
		prefFiles = append(prefFiles, *f)
	} else {
		warnStr(w)
	}
	dirFiles, dirWarns = captureDir(root, "/etc/apt/preferences.d", nil, fs)
	prefFiles = append(prefFiles, dirFiles...)
	for _, w := range dirWarns {
		warnStr(w)
	}
	sortFilesByPath(prefFiles)
	s.APT.Preferences = prefFiles

	// apt.conf: the field the prototype was missing (E4).
	var confFiles []File
	if f, w := captureOptionalFile(root, "/etc/apt/apt.conf", fs); f != nil {
		confFiles = append(confFiles, *f)
	} else {
		warnStr(w)
	}
	dirFiles, dirWarns = captureDir(root, "/etc/apt/apt.conf.d", nil, fs)
	confFiles = append(confFiles, dirFiles...)
	for _, w := range dirWarns {
		warnStr(w)
	}
	sortFilesByPath(confFiles)
	s.APT.Conf = confFiles

	// Keyrings: trusted.gpg.d wholesale, plus whatever Signed-By/signed-by=
	// references from the sources just captured, plus every fingerprint.
	if opts.IncludeKeyrings {
		trustedFiles, trustedWarns := captureDir(root, "/etc/apt/trusted.gpg.d", nil, fs)
		sortFilesByPath(trustedFiles)
		s.APT.Trusted = trustedFiles
		for _, w := range trustedWarns {
			warnStr(w)
		}

		keyringFiles, fps := captureKeyrings(root, sourceFiles, trustedFiles, fs, warn)
		s.APT.Keyrings = keyringFiles
		s.KeyringFingerprints = fps
	}

	if len(opts.Labels) > 0 {
		if len(opts.Labels) > maxLabels {
			// The operator's own mistake, so say it now rather than writing a
			// document the builder will refuse: labels are metadata (hostname,
			// site, ticket), and a document is bounded in how many display
			// strings it may carry (see displaystrings.go).
			return nil, nil, dferr.New(dferr.Usage,
				"snapshot: %d labels exceeds the limit of %d", len(opts.Labels), maxLabels)
		}
		labels := make(map[string]string, len(opts.Labels))
		for k, v := range opts.Labels {
			labels[k] = v
		}
		s.Labels = labels
	}

	s.Warnings = warnings

	if opts.Redact {
		doRedact(s, fs, RedactMachineID, RedactProxies, RedactLabels)
	}

	// Last, so it covers everything above: the display strings a capture
	// fills in come from the target's own files (an /etc/os-release
	// PRETTY_NAME, an error string naming a file) and from the operator's
	// flags, and a document carrying terminal control sequences in them is
	// one Open refuses. Sanitising here keeps an honest capture -- even of an
	// already-hostile target -- readable, without weakening that refusal.
	sanitizeSnapshotStrings(s)

	return s, fs, nil
}

// countInstalled counts dpkg status stanzas whose Status field contains
// "install ok installed" (as opposed to e.g. "deinstall ok config-files"):
// packages actually present, not merely known to dpkg.
func countInstalled(data []byte) int {
	n := 0
	for _, stz := range parseDeb822(data) {
		if v, ok := stz.Get("Status"); ok && strings.Contains(v, "install ok installed") {
			n++
		}
	}
	return n
}
