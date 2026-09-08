//go:build ignore

// fetch-indexes.go stages a real apt index corpus outside the repository, so
// the catalogue packages can measure the performance budgets in
// docs/dev/contract-brief.md against real data instead of fixtures.
//
// Nothing here is part of the shipped application. It is a build-time helper,
// stdlib only, no module dependencies (hence the `ignore` build tag: it is run
// with `go run`, never compiled into the binary).
//
// Usage:
//
//	go run ./hack/fetch-indexes.go -out <dir>
//	go run ./hack/fetch-indexes.go -out <dir> -only ubuntu   # skip Debian
//	go run ./hack/fetch-indexes.go -out <dir> -icons         # also DEP-11 icon tarballs
//	go run ./hack/fetch-indexes.go -out <dir> -force         # re-download everything
//
// It downloads to <dir>, decompresses every .gz alongside its archive, and
// writes <dir>/MANIFEST.txt with the URL, byte sizes (compressed and
// uncompressed) and SHA-256 of every artefact. Re-running is cheap: a file
// whose size matches the server's Content-Length is left alone unless -force.
//
// Never point -out at the working tree. These files total roughly 90 MB
// compressed and 700 MB uncompressed; committing any of them is a mistake.
//
// A note on compression, because it constrains the catalogue implementation:
// Go's standard library decompresses gzip and not xz. This script therefore
// prefers the .gz variant of every index. Ubuntu publishes both. Debian
// publishes Translation-en only as .xz, so that one file is fetched
// compressed and left for the caller to expand (`unxz`). If the catalogue ever
// needs to read Debian translations in-process it needs an xz reader;
// github.com/xi2/xz is already in the module graph transitively, via
// pault.ag/go/debian, so promoting it is not a new dependency — but it is a
// decision, and it belongs in docs/dependency-review.md.
package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Mirrors. archive.ubuntu.com and deb.debian.org are the canonical entry
// points; both are CDN-fronted round robins. A geo mirror
// (e.g. http://gb.archive.ubuntu.com/ubuntu/) is a drop-in substitute.
const (
	ubuntuMirror = "http://archive.ubuntu.com/ubuntu/"
	debianMirror = "http://deb.debian.org/debian/"
)

type artefact struct {
	// dist is the group this artefact belongs to: "ubuntu" or "debian".
	dist string
	// url is the absolute source URL.
	url string
	// name is the local filename under the output directory.
	name string
	// icons marks artefacts fetched only with -icons.
	icons bool
	// why is a one-line note recorded in the manifest.
	why string
}

func artefacts() []artefact {
	u := func(p string) string { return ubuntuMirror + p }
	d := func(p string) string { return debianMirror + p }

	return []artefact{
		// ---- Ubuntu 24.04 LTS (noble), amd64 ----
		{"ubuntu", u("dists/noble/Release"), "ubuntu-noble-Release", false,
			"unsigned index-of-indexes: component list, per-file SHA-256, Date/Valid-Until"},
		{"ubuntu", u("dists/noble/InRelease"), "ubuntu-noble-InRelease", false,
			"same content, inline PGP-signed; this is what apt actually fetches"},

		{"ubuntu", u("dists/noble/main/binary-amd64/Packages.gz"), "ubuntu-noble-main-Packages.gz", false,
			"binary package index, main component"},
		{"ubuntu", u("dists/noble/universe/binary-amd64/Packages.gz"), "ubuntu-noble-universe-Packages.gz", false,
			"binary package index, universe component -- the search budget's real target"},

		// The -updates pocket is a separate index. A real target's catalogue is
		// the union of release + -updates + -security, so the corpus carries it.
		{"ubuntu", u("dists/noble-updates/main/binary-amd64/Packages.gz"), "ubuntu-noble-updates-main-Packages.gz", false,
			"updates pocket, main: overlays the release index"},
		{"ubuntu", u("dists/noble-updates/universe/binary-amd64/Packages.gz"), "ubuntu-noble-updates-universe-Packages.gz", false,
			"updates pocket, universe"},

		// Translation-en carries the long descriptions that Packages omits.
		// See docs/dev/index-formats.md -- this is load-bearing, not optional.
		{"ubuntu", u("dists/noble/main/i18n/Translation-en.gz"), "ubuntu-noble-main-Translation-en.gz", false,
			"English long descriptions for main; Packages carries only Description-md5"},
		{"ubuntu", u("dists/noble/universe/i18n/Translation-en.gz"), "ubuntu-noble-universe-Translation-en.gz", false,
			"English long descriptions for universe"},

		{"ubuntu", u("dists/noble/main/dep11/Components-amd64.yml.gz"), "ubuntu-noble-main-Components-amd64.yml.gz", false,
			"DEP-11 AppStream metadata, main"},
		{"ubuntu", u("dists/noble/universe/dep11/Components-amd64.yml.gz"), "ubuntu-noble-universe-Components-amd64.yml.gz", false,
			"DEP-11 AppStream metadata, universe"},

		// The -updates pocket publishes DEP-11 too, and Target.IndexRefs asks
		// for it. Staging only the release pocket's made an offline
		// reproduction of a build quietly short of application names — the
		// entries were all there, the human names for the updated ones were
		// not — so the corpus carries all four files the loaders request.
		{"ubuntu", u("dists/noble-updates/main/dep11/Components-amd64.yml.gz"), "ubuntu-noble-updates-main-Components-amd64.yml.gz", false,
			"DEP-11 AppStream metadata, updates pocket, main"},
		{"ubuntu", u("dists/noble-updates/universe/dep11/Components-amd64.yml.gz"), "ubuntu-noble-updates-universe-Components-amd64.yml.gz", false,
			"DEP-11 AppStream metadata, updates pocket, universe"},

		{"ubuntu", u("dists/noble/main/dep11/icons-64x64.tar.gz"), "ubuntu-noble-main-icons-64x64.tar.gz", true,
			"DEP-11 icon cache, 64px, main"},
		{"ubuntu", u("dists/noble/universe/dep11/icons-64x64.tar.gz"), "ubuntu-noble-universe-icons-64x64.tar.gz", true,
			"DEP-11 icon cache, 64px, universe"},

		// ---- Debian 12 (bookworm), amd64 ----
		// Present so the parsers are not accidentally Ubuntu-shaped.
		{"debian", d("dists/bookworm/Release"), "debian-bookworm-Release", false,
			"Debian Release, for comparison with Ubuntu's"},
		{"debian", d("dists/bookworm/InRelease"), "debian-bookworm-InRelease", false,
			"Debian InRelease"},
		{"debian", d("dists/bookworm/main/binary-amd64/Packages.gz"), "debian-bookworm-main-Packages.gz", false,
			"Debian main binary index -- carries full Description inline, unlike Ubuntu"},
		{"debian", d("dists/bookworm/main/dep11/Components-amd64.yml.gz"), "debian-bookworm-main-Components-amd64.yml.gz", false,
			"Debian DEP-11 metadata"},
		// Debian publishes Translation-en as .xz only; no .gz variant exists.
		{"debian", d("dists/bookworm/main/i18n/Translation-en.xz"), "debian-bookworm-main-Translation-en.xz", false,
			"Debian long descriptions; .xz only, no .gz published -- stdlib cannot expand this"},
		{"debian", d("dists/bookworm/main/dep11/icons-64x64.tar.gz"), "debian-bookworm-main-icons-64x64.tar.gz", true,
			"DEP-11 icon cache, 64px"},
	}
}

func main() {
	out := flag.String("out", "", "output directory (required; must be outside the repository)")
	only := flag.String("only", "", `restrict to one distribution: "ubuntu" or "debian"`)
	icons := flag.Bool("icons", false, "also fetch the DEP-11 icon tarballs")
	force := flag.Bool("force", false, "re-download even when a local file already matches the server's size")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-request timeout")
	flag.Parse()

	if *out == "" {
		fmt.Fprintln(os.Stderr, "fetch-indexes: -out is required")
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*out, *only, *icons, *force, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "fetch-indexes: %v\n", err)
		os.Exit(1)
	}
}

func run(out, only string, icons, force bool, timeout time.Duration) error {
	if err := refuseRepoDir(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	client := &http.Client{Timeout: timeout}

	type record struct {
		name       string
		url        string
		why        string
		compressed int64
		plain      int64 // 0 when the artefact is not decompressed
		sumComp    string
		sumPlain   string
	}
	var records []record

	for _, a := range artefacts() {
		if only != "" && a.dist != only {
			continue
		}
		if a.icons && !icons {
			continue
		}

		dst := filepath.Join(out, a.name)
		size, err := fetch(client, a.url, dst, force)
		if err != nil {
			return fmt.Errorf("%s: %w", a.url, err)
		}
		sum, err := sha256File(dst)
		if err != nil {
			return err
		}
		rec := record{name: a.name, url: a.url, why: a.why, compressed: size, sumComp: sum}

		// Expand .gz alongside the archive. .xz is left as-is: no stdlib decoder.
		if strings.HasSuffix(a.name, ".gz") && !strings.HasSuffix(a.name, ".tar.gz") {
			plain := strings.TrimSuffix(dst, ".gz")
			n, err := gunzip(dst, plain, force)
			if err != nil {
				return fmt.Errorf("gunzip %s: %w", a.name, err)
			}
			ps, err := sha256File(plain)
			if err != nil {
				return err
			}
			rec.plain, rec.sumPlain = n, ps
		}
		records = append(records, rec)
		fmt.Printf("ok  %-52s %10d bytes\n", a.name, size)
	}

	sort.Slice(records, func(i, j int) bool { return records[i].name < records[j].name })

	var b strings.Builder
	fmt.Fprintf(&b, "# apt index corpus fetched by hack/fetch-indexes.go\n")
	fmt.Fprintf(&b, "# fetched-at: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "# ubuntu-mirror: %s\n", ubuntuMirror)
	fmt.Fprintf(&b, "# debian-mirror: %s\n#\n", debianMirror)
	fmt.Fprintf(&b, "# Sizes are bytes. sha256-plain is the decompressed file where one exists.\n\n")
	for _, r := range records {
		fmt.Fprintf(&b, "file:           %s\n", r.name)
		fmt.Fprintf(&b, "url:            %s\n", r.url)
		fmt.Fprintf(&b, "note:           %s\n", r.why)
		fmt.Fprintf(&b, "bytes:          %d\n", r.compressed)
		fmt.Fprintf(&b, "sha256:         %s\n", r.sumComp)
		if r.plain > 0 {
			fmt.Fprintf(&b, "bytes-plain:    %d\n", r.plain)
			fmt.Fprintf(&b, "sha256-plain:   %s\n", r.sumPlain)
		}
		fmt.Fprintln(&b)
	}
	manifest := filepath.Join(out, "MANIFEST.txt")
	if err := os.WriteFile(manifest, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("\nwrote %s (%d artefacts)\n", manifest, len(records))
	return nil
}

// refuseRepoDir is a guard, not a courtesy. A 90 MB corpus inside the working
// tree is a commit waiting to happen.
func refuseRepoDir(out string) error {
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	for dir := abs; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return fmt.Errorf("refusing to write inside the module rooted at %s: pick a directory outside the repository", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// fetch downloads url to dst, skipping the transfer when dst already exists at
// the length the server reports. Returns the local file's size.
func fetch(c *http.Client, url, dst string, force bool) (int64, error) {
	if !force {
		if st, err := os.Stat(dst); err == nil {
			if n, err := contentLength(c, url); err == nil && n == st.Size() {
				fmt.Printf("--  %-52s (cached)\n", filepath.Base(dst))
				return st.Size(), nil
			}
		}
	}

	resp, err := c.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %s", resp.Status)
	}

	// Write to a temporary and rename, so an interrupted run never leaves a
	// truncated index that the size check would then accept as complete.
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, resp.Body)
	cerr := f.Close()
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if cerr != nil {
		os.Remove(tmp)
		return 0, cerr
	}
	if err := os.Rename(tmp, dst); err != nil {
		return 0, err
	}
	return n, nil
}

func contentLength(c *http.Client, url string) (int64, error) {
	resp, err := c.Head(url)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %s", resp.Status)
	}
	if resp.ContentLength < 0 {
		return 0, errors.New("no Content-Length")
	}
	return resp.ContentLength, nil
}

func gunzip(src, dst string, force bool) (int64, error) {
	if !force {
		if st, err := os.Stat(dst); err == nil {
			si, err2 := os.Stat(src)
			if err2 == nil && st.ModTime().After(si.ModTime()) {
				return st.Size(), nil
			}
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return 0, err
	}
	defer zr.Close()

	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, zr)
	cerr := out.Close()
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if cerr != nil {
		os.Remove(tmp)
		return 0, cerr
	}
	if err := os.Rename(tmp, dst); err != nil {
		return 0, err
	}
	return n, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
