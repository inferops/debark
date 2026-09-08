package doctor

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/xi2/xz"
)

// maintainerScripts are the four maintainer script names doctor scans, in the
// stable order findings are reported.
var maintainerScripts = []string{"preinst", "postinst", "prerm", "postrm"}

// scanByteLimit caps how much of any single member debark decompresses.
// Maintainer scripts and control files are always small (KBs); this is a
// safety bound against a pathological or hostile archive, not a real-world
// limit.
const scanByteLimit = 8 * 1024 * 1024

// maxZstdWindow bounds the window a zstd decoder here may allocate, via
// WithDecoderMaxMemory ("This can be used to control memory usage of
// potentially hostile content"), which sets both the in-memory decode
// ceiling and, for streaming, the maximum window size. Note it bounds the
// decoder's own buffers, not the length of the stream: core/bundle passes
// 1<<30 to the same option on a bundle tar that legitimately decompresses to
// many GiB.
//
// 64 MiB is chosen from what a real .deb declares, with room on both sides:
// dpkg-deb's default `zstd -19` produces windowLog 23 (8 MiB), so this is 8x
// the largest window any dpkg-built package asks for and 8x what doctor can
// read out of one (scanByteLimit) — while being 8x below klauspost's own
// 512 MiB default, which a 10-byte frame was measured turning into a
// 537,921,440-byte allocation.
//
// Concurrency is pinned to 1 alongside it: doctor reads at most
// scanByteLimit bytes from a member and then closes the decoder, so the
// default (up to 4) async decode-ahead goroutines and their per-goroutine
// buffers are pure cost, once per member per package in the lock.
const maxZstdWindow = 64 << 20 // 64 MiB

// debFile is what doctor needs from one .deb, extracted from its control
// member (and, best-effort, a listing of its data member for the DKMS
// secondary signal).
type debFile struct {
	// Control is the parsed control stanza: Package, Version, Source,
	// Description, ...
	Control map[string]string
	// Scripts maps maintainer script name (preinst, postinst, prerm, postrm)
	// to its content, for the scripts present in the package.
	Scripts map[string]string
	// HasDKMSConf is true when the data member could be listed and contained
	// a usr/src/*/dkms.conf path.
	HasDKMSConf bool
	// Unscannable records why script/description content could not be read
	// (e.g. a control member compression this build cannot decode at all),
	// so a clean result is never confused with "we didn't actually look".
	Unscannable string
	// ControlCompression is the control.tar member's compression, always
	// set: "gzip", "bzip2", "xz", "zstd", "none", or "unknown:<ar member
	// name>". Not used by any check — recorded for experiment E7's
	// real-world compression survey (docs/experiments/E7-network-postinst.md).
	ControlCompression string
}

// poolPath resolves one lock Package.Filename against a bundle root and
// guarantees the result stays inside <bundleDir>/repo.
//
// Filename is attacker-controlled: `debark doctor BUNDLE` reads it out of
// the bundle's own lock.json, and bundle.Open never verifies (documented, by
// design — verify.Verify is what does). A bare filepath.Join was measured
// escaping: Filename "../../outside/secret.deb" resolved outside the bundle
// entirely and doctor read that file, parsed it as a .deb, and reported its
// maintainer-script text back in Finding.Evidence — an arbitrary-file read
// whose contents land in the report.
//
// The rules are core/bundle's safeRelPath, applied for the same reason and
// with the same refusals: backslashes and a Windows drive-letter prefix are
// rejected whatever platform this runs on, because a hostile bundle is not
// obliged to know what will read it. The containment check is then repeated
// on the joined result with filepath.Rel, so a rule missed above still
// cannot produce a path outside the pool.
func poolPath(bundleDir, filename string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("doctor: empty pool filename")
	}
	if strings.ContainsRune(filename, 0) {
		return "", fmt.Errorf("doctor: pool filename contains a NUL byte")
	}
	if strings.ContainsRune(filename, '\\') {
		return "", fmt.Errorf("doctor: pool filename contains a backslash")
	}
	if strings.HasPrefix(filename, "/") {
		return "", fmt.Errorf("doctor: pool filename is an absolute path")
	}
	if len(filename) >= 2 && filename[1] == ':' {
		return "", fmt.Errorf("doctor: pool filename looks like a Windows drive-letter path")
	}
	cleaned := path.Clean(filename)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("doctor: pool filename %q escapes the bundle", filename)
	}

	root := filepath.Join(bundleDir, "repo")
	full := filepath.Join(root, filepath.FromSlash(cleaned))
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", fmt.Errorf("doctor: pool filename %q: %w", filename, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("doctor: pool filename %q escapes the bundle", filename)
	}
	return full, nil
}

// openDebFile reads path as a .deb and extracts what doctor's checks need.
//
// The Lstat guard is not a formality. A bundle can be a plain directory the
// operator points doctor at (bundle.Open accepts one), so nothing has
// filtered what lives under repo/ the way import does for a tar. Two things
// follow: a symlink there would otherwise redirect this read anywhere the
// process can reach, and on Unix a FIFO would make os.Open block until a
// writer appeared — a hang, not an error. Requiring a regular file settles
// both before anything is opened.
func openDebFile(path string) (*debFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("doctor: %s is not a regular file (mode %v)", path, info.Mode())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseDebReader(f)
}

// parseDebReader parses .deb bytes from r. Exported at package scope (not
// capital-P Public) so tests can build a .deb in memory without touching disk.
func parseDebReader(r io.Reader) (*debFile, error) {
	members, err := parseAr(r)
	if err != nil {
		return nil, fmt.Errorf("doctor: parse .deb: %w", err)
	}

	out := &debFile{Scripts: map[string]string{}}

	var controlMember, dataMember *arMember
	for i := range members {
		switch {
		case strings.HasPrefix(members[i].Name, "control.tar"):
			controlMember = &members[i]
		case strings.HasPrefix(members[i].Name, "data.tar"):
			dataMember = &members[i]
		}
	}
	if controlMember == nil {
		return nil, fmt.Errorf("doctor: .deb has no control.tar member")
	}
	out.ControlCompression = compressionBucket(controlMember.Name)

	cr, unsupported, err := decompressMember(*controlMember)
	if err != nil {
		return nil, fmt.Errorf("doctor: open control member %q: %w", controlMember.Name, err)
	}
	if unsupported != "" {
		out.Unscannable = unsupported
		return out, nil
	}
	func() {
		defer cr.Close()
		tr := tar.NewReader(io.LimitReader(cr, scanByteLimit))
		for {
			hdr, terr := tr.Next()
			if terr == io.EOF {
				return
			}
			if terr != nil {
				err = fmt.Errorf("doctor: read control.tar: %w", terr)
				return
			}
			if hdr.Typeflag != tar.TypeReg {
				continue
			}
			name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
			name = strings.TrimPrefix(name, "./")
			base := path.Base(name)
			switch {
			case base == "control" && !strings.Contains(name, "/"):
				body, rerr := io.ReadAll(io.LimitReader(tr, scanByteLimit))
				if rerr != nil {
					err = fmt.Errorf("doctor: read control file: %w", rerr)
					return
				}
				stanzas, perr := parseDeb822(bytes.NewReader(body))
				if perr == nil && len(stanzas) > 0 {
					out.Control = stanzas[0]
				}
			case isMaintainerScript(base) && !strings.Contains(name, "/"):
				body, rerr := io.ReadAll(io.LimitReader(tr, scanByteLimit))
				if rerr != nil {
					err = fmt.Errorf("doctor: read %s: %w", base, rerr)
					return
				}
				out.Scripts[base] = string(body)
			}
		}
	}()
	if err != nil {
		return nil, err
	}

	// Best-effort secondary DKMS signal: list (never fully read) the data
	// member looking for usr/src/*/dkms.conf. Skipped silently when the data
	// member uses a compression this build cannot decode at all — the
	// name-based signal in checkDKMSHeaders does not depend on this.
	if dataMember != nil {
		if dr, unsupported, derr := decompressMember(*dataMember); derr == nil && unsupported == "" {
			func() {
				defer dr.Close()
				dtr := tar.NewReader(io.LimitReader(dr, scanByteLimit))
				for {
					hdr, terr := dtr.Next()
					if terr == io.EOF || terr != nil {
						return
					}
					name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
					if strings.HasPrefix(name, "usr/src/") && strings.HasSuffix(name, "/dkms.conf") {
						out.HasDKMSConf = true
						return
					}
				}
			}()
		}
	}

	return out, nil
}

func isMaintainerScript(name string) bool {
	for _, s := range maintainerScripts {
		if name == s {
			return true
		}
	}
	return false
}

// zstdReadCloser adapts *zstd.Decoder (whose Close takes and returns nothing)
// to io.ReadCloser.
type zstdReadCloser struct{ *zstd.Decoder }

func (z zstdReadCloser) Close() error { z.Decoder.Close(); return nil }

// decompressMember returns a reader over member's decompressed content based
// on its ar member name suffix, and closes cleanly in every case (the caller
// always defers Close(), including for the stdlib formats that don't
// otherwise need it, so a leaked zstd decoder goroutine can never happen from
// forgetting which format needs cleanup).
//
// gzip and bzip2 are read with the standard library. xz and zstd are real,
// full decoders (github.com/xi2/xz, github.com/klauspost/compress/zstd) —
// not a new dependency debark added for this: both arrived transitively
// through pault.ag/go/debian, which the apt adapter added to
// go.mod for .deb/deb822 parsing, and which itself depends on both to read
// modern .deb files. Experiment E7 (docs/experiments/E7-network-postinst.md)
// measured control.tar compression across 1,749 real Debian 12 and Ubuntu
// 24.04 archive packages: 917 xz, 812 zstd, 20 gzip/uncompressed — gzip alone
// (or gzip+bzip2) would have left this check unable to scan the control
// member of virtually every one of them.
//
// The two decoders that allocate from a number the archive itself declares
// are bounded here rather than left at their library defaults, because this
// runs on a bundle nothing has verified yet:
//
//   - zstd: a frame header carries a window size, and klauspost/compress
//     allocates the window before any content arrives. At its default
//     512 MiB ceiling (MaxWindowSize, 1<<29) a 10-byte frame declaring
//     windowLog 29 was measured costing 537,921,440 bytes of heap in 178ms —
//     for a member doctor then reads at most scanByteLimit bytes of.
//     maxZstdWindow below is what that is bounded to now.
//   - xz: xi2/xz allocates its LZMA2 dictionary the same way, but its
//     dictMax argument of 0 already means DefaultDictMax (1<<26, 64 MiB),
//     not "unlimited" — checked in the dependency, and left as it is.
//
// gzip and bzip2 allocate fixed-size state (32 KiB and 900 KiB windows), so
// neither has a knob to set.
//
// unsupported is non-empty (and reader nil) only for a compression this
// build genuinely cannot decode (lzma, or an unrecognised member name).
func decompressMember(m arMember) (io.ReadCloser, string, error) {
	switch {
	case m.Name == "control.tar" || m.Name == "data.tar":
		return io.NopCloser(bytes.NewReader(m.Data)), "", nil
	case strings.HasSuffix(m.Name, ".tar.gz"):
		gr, err := gzip.NewReader(bytes.NewReader(m.Data))
		if err != nil {
			return nil, "", err
		}
		return gr, "", nil
	case strings.HasSuffix(m.Name, ".tar.bz2"):
		return io.NopCloser(bzip2.NewReader(bytes.NewReader(m.Data))), "", nil
	case strings.HasSuffix(m.Name, ".tar.xz"):
		xr, err := xz.NewReader(bytes.NewReader(m.Data), 0)
		if err != nil {
			return nil, "", fmt.Errorf("xz: %w", err)
		}
		return io.NopCloser(xr), "", nil
	case strings.HasSuffix(m.Name, ".tar.zst"):
		zr, err := zstd.NewReader(bytes.NewReader(m.Data),
			zstd.WithDecoderMaxMemory(maxZstdWindow),
			zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, "", fmt.Errorf("zstd: %w", err)
		}
		return zstdReadCloser{zr}, "", nil
	case strings.HasSuffix(m.Name, ".tar.lzma"):
		return nil, "lzma", nil
	default:
		return nil, "unknown:" + m.Name, nil
	}
}

// compressionBucket classifies an ar member name by its compression suffix,
// for reporting only (debFile.ControlCompression) — decompressMember does the
// same classification for real, but this stays a plain string label rather
// than reusing its (reader, unsupported, error) shape.
func compressionBucket(name string) string {
	switch {
	case name == "control.tar" || name == "data.tar":
		return "none"
	case strings.HasSuffix(name, ".tar.gz"):
		return "gzip"
	case strings.HasSuffix(name, ".tar.bz2"):
		return "bzip2"
	case strings.HasSuffix(name, ".tar.xz"):
		return "xz"
	case strings.HasSuffix(name, ".tar.zst"):
		return "zstd"
	case strings.HasSuffix(name, ".tar.lzma"):
		return "lzma"
	default:
		return "unknown:" + name
	}
}
