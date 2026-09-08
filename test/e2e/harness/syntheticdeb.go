package harness

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file builds small, synthetic .deb packages and flat repositories
// entirely in Go, on the host — never with dpkg-deb inside a container. That
// is a deliberate correction made during development, not the original
// design: an earlier version of this harness shelled out to dpkg-deb inside
// whichever container needed a package, and used plain file:// sources for
// synthetic repositories. That breaks the moment two different containers
// need to see the same repository — the "state" container that builds a
// fixture's installed set and the separately-started "builder" container
// that resolves the snapshot's recorded sources do not share a filesystem,
// so a file:// URI written on one is simply absent on the other. Building
// once on the host and serving it over HTTP (httprepo.go) to every container
// in the run fixes that, and happens to match the exact technique
// core/repository/testdeb_test.go already uses for its own dpkg-deb-less,
// Windows-runnable .deb fixtures — the same reasoning applies here.
//
// The Packages/Release files are hand-written rather than produced by
// dpkg-scanpackages/apt-ftparchive because neither is guaranteed present
// (docs/dev/prototype-baseline.md found apt-ftparchive missing from
// debian:bookworm-slim) and because hand-writing is the only way to inject a
// field no real tool would give on demand, such as Phased-Update-Percentage
// — and, now, the only way to build anything at all without
// a container in the loop.

// SyntheticDebSpec describes one small .deb to build. It doubles as a
// fixture JSON field type (test/e2e/fixture.go embeds it via
// harness.Scenario), so it carries json tags even though most harness types
// otherwise wouldn't need to.
type SyntheticDebSpec struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Arch is required when building directly; left empty in a fixture file
	// it defaults to the fixture's target architecture (see ResolveArch).
	Arch string `json:"arch,omitempty"`
	// Depends and Provides are raw control field values, e.g.
	// "jq, tree" or "nonexistent-package-xyz (>= 999.0)".
	Depends    string `json:"depends,omitempty"`
	Provides   string `json:"provides,omitempty"`
	Recommends string `json:"recommends,omitempty"`
	// Bin installs /usr/bin/<name>, a tiny script that prints "<name>
	// <version>" and exits 0 — something real for the "binaries actually
	// execute" assertion to run.
	Bin bool `json:"bin,omitempty"`
	// Postinst, when non-empty, becomes the package's postinst script body
	// (after the #!/bin/sh line) — used to give doctor's network-postinst,
	// snap-shim and DKMS heuristics something concrete to find.
	Postinst string `json:"postinst,omitempty"`
	// PadBytes adds a file of that many incompressible bytes under
	// /usr/share/<name>/, so the package has a realistic transfer size.
	//
	// It exists for one fixture (vendor-url-fetch) and for one reason: a
	// download has to take long enough to span several of core/fetch's
	// 250ms progress ticks, or the paced and unpaced fetches of it emit the
	// same number of progress events and the determinism row can no longer
	// see the defect it is there for. A few kilobytes would arrive in one
	// tick however slowly it were served.
	//
	// The bytes are incompressible (the payload is gzipped into data.tar.gz
	// and zeroes would vanish) and DETERMINISTIC — a fixed-seed generator,
	// never crypto/rand — because a synthetic package whose digest changed
	// per run would be a reproducibility break planted by the harness
	// itself, in the one fixture whose subject is reproducibility.
	PadBytes int `json:"pad_bytes,omitempty"`
}

// padding returns n incompressible but perfectly reproducible bytes from a
// fixed-seed xorshift generator. Same n, same bytes, every process, forever.
func padding(n int) []byte {
	out := make([]byte, n)
	x := uint64(0x9E3779B97F4A7C15) // any fixed non-zero seed
	for i := range out {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		// Masked rather than a bare byte(x): taking the low 8 bits is the
		// intent, and saying so keeps gosec's integer-overflow check quiet
		// without a nolint. The lint baseline for this repository is three
		// revive findings in core/apt/select.go and nothing else, which is
		// only useful as a tripwire if it stays exact.
		out[i] = byte(x & 0xFF)
	}
	return out
}

// ResolveArch returns s with Arch defaulted to fallback when unset, without
// mutating s.
func (s SyntheticDebSpec) ResolveArch(fallback string) SyntheticDebSpec {
	if s.Arch == "" {
		s.Arch = fallback
	}
	return s
}

func (s SyntheticDebSpec) debFilename() string {
	return fmt.Sprintf("%s_%s_%s.deb", s.Name, sanitize(s.Version), s.Arch)
}

func (s SyntheticDebSpec) controlText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Package: %s\n", s.Name)
	fmt.Fprintf(&b, "Version: %s\n", s.Version)
	fmt.Fprintf(&b, "Architecture: %s\n", s.Arch)
	fmt.Fprintf(&b, "Maintainer: debark E2E harness <e2e@debark.invalid>\n")
	fmt.Fprintf(&b, "Installed-Size: 1\n")
	if s.Depends != "" {
		fmt.Fprintf(&b, "Depends: %s\n", s.Depends)
	}
	if s.Recommends != "" {
		fmt.Fprintf(&b, "Recommends: %s\n", s.Recommends)
	}
	if s.Provides != "" {
		fmt.Fprintf(&b, "Provides: %s\n", s.Provides)
	}
	fmt.Fprintf(&b, "Section: misc\n")
	fmt.Fprintf(&b, "Priority: optional\n")
	fmt.Fprintf(&b, "Description: synthetic fixture package (test/e2e/harness)\n")
	fmt.Fprintf(&b, " Built by debark's integration harness; not a real package.\n")
	return b.String()
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, s)
}

// tarEntry is one member of a control.tar.gz or data.tar.gz. A directory
// entry (Name ending in "/") carries no data.
type tarEntry struct {
	Name string
	Mode int64
	Data []byte
}

func gzipTarOf(entries []tarEntry) ([]byte, error) {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, e := range entries {
		typeflag := byte(tar.TypeReg)
		if strings.HasSuffix(e.Name, "/") {
			typeflag = tar.TypeDir
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.Name,
			Typeflag: typeflag,
			Mode:     e.Mode,
			Size:     int64(len(e.Data)),
			ModTime:  time.Unix(0, 0), // no wall-clock timestamps in a test artefact either
		}); err != nil {
			return nil, fmt.Errorf("tar header %s: %w", e.Name, err)
		}
		if typeflag == tar.TypeDir {
			continue
		}
		if _, err := tw.Write(e.Data); err != nil {
			return nil, fmt.Errorf("tar write %s: %w", e.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("tar close: %w", err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(tarBuf.Bytes()); err != nil {
		return nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return gzBuf.Bytes(), nil
}

// arMember writes one ar(1) member: a 60-byte header (name, mtime, uid, gid,
// mode, size, then the 0x60 0x0A magic) followed by the data and, if size is
// odd, a single pad byte — the same layout
// core/repository/testdeb_test.go's arMember produces, which pault.ag/go/
// debian's deb.ArEntry parses.
func arMember(buf *bytes.Buffer, name string, data []byte) {
	h := make([]byte, 60)
	for i := range h {
		h[i] = ' '
	}
	copy(h[0:16], name)
	copy(h[16:28], "0")
	copy(h[28:34], "0")
	copy(h[34:40], "0")
	copy(h[40:48], "100644")
	copy(h[48:58], strconv.Itoa(len(data)))
	h[58] = 0x60
	h[59] = 0x0A
	buf.Write(h)
	buf.Write(data)
	if len(data)%2 == 1 {
		buf.WriteByte('\n')
	}
}

// BuildSyntheticDebBytes assembles a minimal, valid Debian binary package
// (format 2.0: debian-binary, control.tar.gz, data.tar.gz) for spec, with no
// external tool involved — dpkg-deb, ar and a running container are all
// unnecessary for this.
func BuildSyntheticDebBytes(spec SyntheticDebSpec) ([]byte, error) {
	if spec.Arch == "" {
		return nil, fmt.Errorf("synthetic deb %s: Arch is required", spec.Name)
	}
	controlEntries := []tarEntry{{Name: "./control", Mode: 0o644, Data: []byte(spec.controlText())}}
	if spec.Postinst != "" {
		controlEntries = append(controlEntries, tarEntry{
			Name: "./postinst", Mode: 0o755, Data: []byte("#!/bin/sh\nset -e\n" + spec.Postinst + "\n"),
		})
	}
	controlTGZ, err := gzipTarOf(controlEntries)
	if err != nil {
		return nil, fmt.Errorf("synthetic deb %s: control.tar.gz: %w", spec.Name, err)
	}

	var dataEntries []tarEntry
	if spec.Bin {
		script := fmt.Sprintf("#!/bin/sh\necho \"%s %s\"\nexit 0\n", spec.Name, spec.Version)
		dataEntries = append(dataEntries,
			tarEntry{Name: "./usr/", Mode: 0o755, Data: nil},
			tarEntry{Name: "./usr/bin/", Mode: 0o755, Data: nil},
			tarEntry{Name: "./usr/bin/" + spec.Name, Mode: 0o755, Data: []byte(script)},
		)
	}
	if spec.PadBytes > 0 {
		dataEntries = append(dataEntries,
			tarEntry{Name: "./usr/", Mode: 0o755, Data: nil},
			tarEntry{Name: "./usr/share/", Mode: 0o755, Data: nil},
			tarEntry{Name: "./usr/share/" + spec.Name + "/", Mode: 0o755, Data: nil},
			tarEntry{Name: "./usr/share/" + spec.Name + "/payload.bin", Mode: 0o644, Data: padding(spec.PadBytes)},
		)
	}
	dataTGZ, err := gzipTarOf(dataEntries)
	if err != nil {
		return nil, fmt.Errorf("synthetic deb %s: data.tar.gz: %w", spec.Name, err)
	}

	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	arMember(&out, "debian-binary", []byte("2.0\n"))
	arMember(&out, "control.tar.gz", controlTGZ)
	arMember(&out, "data.tar.gz", dataTGZ)
	return out.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Synthetic repositories
// ---------------------------------------------------------------------------

// SyntheticRepoPackage is one stanza in a hand-written flat Packages file.
type SyntheticRepoPackage struct {
	Spec SyntheticDebSpec `json:"spec"`
	// ExtraFields are appended verbatim after the standard fields, e.g.
	// {"Phased-Update-Percentage": "0"} for the phased-updates fixture.
	ExtraFields map[string]string `json:"extra_fields,omitempty"`
}

// SyntheticRelease is the optional Release-file description for a
// SyntheticRepo. When set, WriteSyntheticRepo emits a Release file alongside
// Packages — still in the same flat layout
// (/ the "Suites: ./" form; no dists/ hierarchy needed: apt's
// flat-repository form serves Release and Packages directly from the given
// URI root, exactly as core/repository/e2e_realapt_test.go proves against a
// real apt) — so a fixture can pin by origin or release,
// which requires those fields to come from somewhere.
type SyntheticRelease struct {
	Origin   string `json:"origin"`
	Label    string `json:"label"`
	Suite    string `json:"suite"`
	Codename string `json:"codename"`
}

// SyntheticRepo is one flat repository, written once to a host directory and
// served over HTTP (see RepoServer) so every container in a fixture run sees
// identical bytes.
type SyntheticRepo struct {
	Name     string                 `json:"name"`
	Packages []SyntheticRepoPackage `json:"packages"`
	Release  *SyntheticRelease      `json:"release,omitempty"`
}

// WriteSyntheticRepo builds every package in repo, writes repo's Packages
// (and, when repo.Release is set, Release) file, and returns the deb822
// source stanza a container should write under
// /etc/apt/sources.list.d/ to use it — pointing at baseURL (this run's
// RepoServer), never at a local path, precisely because a local path is
// only ever visible to one container.
func WriteSyntheticRepo(hostDir, baseURL string, repo SyntheticRepo, arch string) (sourcesStanza string, err error) {
	repoDir := filepath.Join(hostDir, sanitize(repo.Name))
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return "", err
	}

	pkgs := append([]SyntheticRepoPackage(nil), repo.Packages...)
	sort.Slice(pkgs, func(i, j int) bool {
		if pkgs[i].Spec.Name != pkgs[j].Spec.Name {
			return pkgs[i].Spec.Name < pkgs[j].Spec.Name
		}
		return pkgs[i].Spec.Version < pkgs[j].Spec.Version
	})

	var packagesBuf strings.Builder
	for _, p := range pkgs {
		spec := p.Spec.ResolveArch(arch)
		debBytes, err := BuildSyntheticDebBytes(spec)
		if err != nil {
			return "", fmt.Errorf("synthetic repo %s: %w", repo.Name, err)
		}
		debName := spec.debFilename()
		if err := os.WriteFile(filepath.Join(repoDir, debName), debBytes, 0o644); err != nil {
			return "", fmt.Errorf("synthetic repo %s: write %s: %w", repo.Name, debName, err)
		}
		md5sum := md5.Sum(debBytes)
		sha256sum := sha256.Sum256(debBytes)

		fmt.Fprintf(&packagesBuf, "Package: %s\n", spec.Name)
		fmt.Fprintf(&packagesBuf, "Version: %s\n", spec.Version)
		fmt.Fprintf(&packagesBuf, "Architecture: %s\n", spec.Arch)
		fmt.Fprintf(&packagesBuf, "Maintainer: debark E2E harness <e2e@debark.invalid>\n")
		if spec.Depends != "" {
			fmt.Fprintf(&packagesBuf, "Depends: %s\n", spec.Depends)
		}
		if spec.Recommends != "" {
			fmt.Fprintf(&packagesBuf, "Recommends: %s\n", spec.Recommends)
		}
		if spec.Provides != "" {
			fmt.Fprintf(&packagesBuf, "Provides: %s\n", spec.Provides)
		}
		fmt.Fprintf(&packagesBuf, "Filename: %s\n", debName)
		fmt.Fprintf(&packagesBuf, "Size: %d\n", len(debBytes))
		fmt.Fprintf(&packagesBuf, "MD5sum: %s\n", hex.EncodeToString(md5sum[:]))
		fmt.Fprintf(&packagesBuf, "SHA256: %s\n", hex.EncodeToString(sha256sum[:]))
		fmt.Fprintf(&packagesBuf, "Description: synthetic fixture package (test/e2e/harness)\n")
		for _, k := range sortedKeys(p.ExtraFields) {
			fmt.Fprintf(&packagesBuf, "%s: %s\n", k, p.ExtraFields[k])
		}
		packagesBuf.WriteString("\n")
	}
	packagesBytes := []byte(packagesBuf.String())
	if err := os.WriteFile(filepath.Join(repoDir, "Packages"), packagesBytes, 0o644); err != nil {
		return "", fmt.Errorf("synthetic repo %s: write Packages: %w", repo.Name, err)
	}

	repoURL := strings.TrimRight(baseURL, "/") + "/" + sanitize(repo.Name)
	if repo.Release != nil {
		rel := *repo.Release
		if rel.Suite == "" {
			rel.Suite = "bundle"
		}
		md5sum := md5.Sum(packagesBytes)
		sha256sum := sha256.Sum256(packagesBytes)
		var relBuf strings.Builder
		fmt.Fprintf(&relBuf, "Origin: %s\n", rel.Origin)
		fmt.Fprintf(&relBuf, "Label: %s\n", rel.Label)
		fmt.Fprintf(&relBuf, "Suite: %s\n", rel.Suite)
		if rel.Codename != "" {
			fmt.Fprintf(&relBuf, "Codename: %s\n", rel.Codename)
		}
		fmt.Fprintf(&relBuf, "Architectures: %s\n", arch)
		fmt.Fprintf(&relBuf, "Components: main\n")
		fmt.Fprintf(&relBuf, "Description: synthetic fixture repository (test/e2e/harness)\n")
		fmt.Fprintf(&relBuf, "Date: %s\n", time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 UTC"))
		fmt.Fprintf(&relBuf, "MD5Sum:\n %s %16d Packages\n", hex.EncodeToString(md5sum[:]), len(packagesBytes))
		fmt.Fprintf(&relBuf, "SHA256:\n %s %16d Packages\n", hex.EncodeToString(sha256sum[:]), len(packagesBytes))
		if err := os.WriteFile(filepath.Join(repoDir, "Release"), []byte(relBuf.String()), 0o644); err != nil {
			return "", fmt.Errorf("synthetic repo %s: write Release: %w", repo.Name, err)
		}
	}

	stanza := fmt.Sprintf("Types: deb\nURIs: %s\nSuites: ./\nTrusted: yes\n", repoURL)
	return stanza, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
