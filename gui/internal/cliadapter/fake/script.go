package fake

import (
	"fmt"
	"strings"
	"time"

	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/snapshot"

	"github.com/inferops/debark/gui/internal/cliadapter"
)

// This file is the scripted build. It exists so that the build screen's
// progress UI can be written, demoed and screenshotted without a container
// runtime, an apt mirror or a network — and so that the shapes the UI has to
// survive (total_bytes = -1, a retry, several files in flight, warnings
// arriving mid-stream) are all reachable on purpose rather than by luck.
//
// Every event here is the shape core/ really emits. The attrs were read out
// of the emitting call sites, not invented: see docs/dev/cli-surface.md for
// the table, and core/fetch/fetcher.go plus core/engine for the originals.

// Step is one scripted moment: an event, and the nominal wall-clock gap
// before it. Adapter.Speed scales the gap; 0 replays the whole script
// instantly, which is what tests want.
type Step struct {
	Event cliadapter.Event
	// Delay is how long a real build would have taken to get here from the
	// previous step.
	Delay time.Duration
}

// ScriptStart is the timestamp the first scripted event carries. Fixed, so a
// replay is byte-for-byte reproducible and a golden test can pin it.
var ScriptStart = time.Date(2026, 9, 6, 10, 15, 0, 0, time.UTC)

// scriptFile is one .deb the scripted build fetches.
type scriptFile struct {
	url string
	// filename is the pool file name, which is what fetch.file reports.
	filename string
	// size is the file's real size in bytes.
	size int64
	// unknownTotal makes the server send no Content-Length, so every progress
	// event for this file carries total_bytes = -1. Exactly one file in the
	// default script does this, because the UI path it exercises (an
	// indeterminate bar for one row while its neighbours have real bars) is
	// otherwise never reached in a demo.
	unknownTotal bool
	// retryAfter, when non-zero, makes the file fail once and emit a retry
	// notice after that many progress events.
	retryAfter int
	// verification is the provenance claim: core/lock's PublisherVerification.
	verification lock.PublisherVerification
	// digest is the fetched file's SHA-256, lowercase hex.
	digest string
	// fromStore, when true, makes the file a store hit rather than a
	// download: no progress and no fetch.file, one store.hit instead.
	fromStore bool
	// name, version and arch are only used for a store.hit.
	name, version, arch string
}

// archiveFiles is a plausible nginx closure off the Ubuntu archive. The sizes
// are the right order of magnitude for these packages, which matters: a demo
// whose bar fills in three equal jumps does not look like a build.
var archiveFiles = []scriptFile{
	{
		url:          "http://archive.ubuntu.com/ubuntu/pool/main/o/openssl/libssl3t64_3.0.13-0ubuntu3.4_amd64.deb",
		filename:     "libssl3t64_3.0.13-0ubuntu3.4_amd64.deb",
		size:         1_923_664,
		verification: lock.VerifiedAPTSigned,
		digest:       "7f8b0c2d4e6a91b3c5d7e9f1a3b5c7d9e1f3a5b7c9d1e3f5a7b9c1d3e5f7a9b1",
	},
	{
		url:          "http://archive.ubuntu.com/ubuntu/pool/main/p/pcre2/libpcre2-8-0_10.42-4ubuntu2.1_amd64.deb",
		filename:     "libpcre2-8-0_10.42-4ubuntu2.1_amd64.deb",
		size:         253_012,
		verification: lock.VerifiedAPTSigned,
		digest:       "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809",
	},
	{
		url:          "http://archive.ubuntu.com/ubuntu/pool/main/n/nginx/nginx-common_1.24.0-2ubuntu7.3_all.deb",
		filename:     "nginx-common_1.24.0-2ubuntu7.3_all.deb",
		size:         31_388,
		verification: lock.VerifiedAPTSigned,
		digest:       "9c8b7a6d5e4f3021c9b8a7d6e5f40312c9b8a7d6e5f40312c9b8a7d6e5f40312",
	},
	{
		// The retry. A flaky mirror is the single most common thing an
		// operator sees, and the UI must not treat it as a failure.
		url:          "http://archive.ubuntu.com/ubuntu/pool/main/n/nginx/nginx-core_1.24.0-2ubuntu7.3_amd64.deb",
		filename:     "nginx-core_1.24.0-2ubuntu7.3_amd64.deb",
		size:         1_484_212,
		retryAfter:   2,
		verification: lock.VerifiedAPTSigned,
		digest:       "44556677889900aabbccddeeff00112233445566778899aabbccddeeff001122",
	},
	{
		// Already in the content-addressed store: no bytes move.
		url:          "http://archive.ubuntu.com/ubuntu/pool/main/z/zlib/zlib1g_1.3.dfsg-3.1ubuntu2.1_amd64.deb",
		filename:     "zlib1g_1.3.dfsg-3.1ubuntu2.1_amd64.deb",
		size:         64_996,
		fromStore:    true,
		name:         "zlib1g",
		version:      "1:1.3.dfsg-3.1ubuntu2.1",
		arch:         "amd64",
		verification: lock.VerifiedAPTSigned,
		digest:       "aabbccddeeff00112233445566778899aabbccddeeff001122334455667788990",
	},
	{
		url:          "http://archive.ubuntu.com/ubuntu/pool/main/libx/libxcrypt/libcrypt1_4.4.36-4build1_amd64.deb",
		filename:     "libcrypt1_4.4.36-4build1_amd64.deb",
		size:         92_268,
		fromStore:    true,
		name:         "libcrypt1",
		version:      "1:4.4.36-4build1",
		arch:         "amd64",
		verification: lock.VerifiedAPTSigned,
		digest:       "0011223344556677889900aabbccddeeff0011223344556677889900aabbccdd",
	},
	{
		// The unknown-total case: a mirror behind a CDN that answers with
		// chunked transfer encoding and no Content-Length.
		url:          "http://archive.ubuntu.com/ubuntu/pool/main/n/nginx/nginx_1.24.0-2ubuntu7.3_all.deb",
		filename:     "nginx_1.24.0-2ubuntu7.3_all.deb",
		size:         3_772,
		unknownTotal: true,
		verification: lock.VerifiedAPTSigned,
		digest:       "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100",
	},
}

// scriptBuilder stamps deterministic timestamps as it appends.
type scriptBuilder struct {
	at    time.Time
	steps []Step
}

func (b *scriptBuilder) add(delay time.Duration, level, typ, msg string, attrs map[string]any) {
	b.at = b.at.Add(delay)
	b.steps = append(b.steps, Step{
		Delay: delay,
		Event: cliadapter.Event{
			Schema: evidence.SchemaVersion,
			TS:     b.at.Format("2006-01-02T15:04:05Z"),
			Type:   typ,
			Level:  level,
			Msg:    msg,
			Attrs:  attrs,
		},
	})
}

func (b *scriptBuilder) info(delay time.Duration, typ, msg string, attrs map[string]any) {
	b.add(delay, "", typ, msg, attrs)
}

func (b *scriptBuilder) warn(delay time.Duration, typ, msg string, attrs map[string]any) {
	b.add(delay, evidence.LevelWarn, typ, msg, attrs)
}

// fetch appends one file's worth of events: throttled progress, an optional
// retry notice, and the closing fetch.file.
//
// The 250 ms gap is not decoration — it is core/fetch's real throttle
// (progressWriter emits at most once per 250 ms per file, plus a final emit),
// and a progress UI that assumes a faster tick will look stalled against a
// real build.
func (b *scriptBuilder) fetch(f scriptFile) {
	total := f.size
	if f.unknownTotal {
		total = -1
	}
	// Four ticks plus a final one at full size: enough for a bar to visibly
	// move without making the script tedious.
	const ticks = 4
	for i := 1; i <= ticks; i++ {
		if f.retryAfter > 0 && i == f.retryAfter+1 {
			b.info(400*time.Millisecond, evidence.TypeProgress,
				fmt.Sprintf("retrying %s (attempt 2/3)", f.url),
				map[string]any{"url": f.url, "attempt": 2, "attempts": 3})
			// A retry restarts the file, so the byte count restarts too.
			// A UI that only ever moves a bar forward will glitch here, and
			// that is exactly why the script contains it.
			b.info(250*time.Millisecond, evidence.TypeProgress, "",
				map[string]any{"url": f.url, "bytes": int64(0), "total_bytes": total})
		}
		sent := f.size * int64(i) / int64(ticks+1)
		b.info(250*time.Millisecond, evidence.TypeProgress, "",
			map[string]any{"url": f.url, "bytes": sent, "total_bytes": total})
	}
	// The final emit: progressWriter.final() always fires, so bytes reaches
	// the real size even when total_bytes was -1 all along.
	b.info(180*time.Millisecond, evidence.TypeProgress, "",
		map[string]any{"url": f.url, "bytes": f.size, "total_bytes": total})

	b.info(20*time.Millisecond, evidence.TypeFetchFile, "fetched "+f.filename, map[string]any{
		"url":          f.url,
		"filename":     f.filename,
		"digest":       f.digest,
		"size":         f.size,
		"verification": string(f.verification),
	})
}

// DefaultScript is the scripted build the fake replays when Adapter.Script is
// nil: snapshot loaded, backend selected, apt update, apt resolve, a stream of
// downloads with throttled progress (including one file with no
// Content-Length and one retry), store hits, a couple of warnings, a doctor
// finding, the closed-world check, the repository index, the signature and
// the assembled bundle.
//
// It is derived from spec so a demo shows the operator's own vendor URLs and
// local .debs, not somebody else's. The resolved closure it downloads is
// archiveFiles, the same plausible nginx closure whatever was selected:
// choosing files per package would mean inventing a version for each one, and
// rule 1 puts every version decision in debark and nowhere else.
//
// spec.Packages reaches the counts, though, and not the files — see
// fxScriptClosure. A demo of forty selected packages that reported nine
// resolved selections was a shape debark cannot emit, and the build screen
// renders those numbers.
func DefaultScript(spec cliadapter.BuildSpec) []Step {
	b := &scriptBuilder{at: ScriptStart}
	target := targetFor(spec)

	b.info(0, evidence.TypeSnapshotLoaded, "snapshot loaded", map[string]any{
		"snapshot_digest": "3d0c2f6bb1e2a4c8f9d7b5a3c1e0f2d4b6a8c0e2f4d6b8a0c2e4f6d8b0a2c4e6",
		"distro_id":       target.DistroID,
		"version_id":      target.VersionID,
		"codename":        target.Codename,
		"arch":            target.Arch,
	})

	backend := spec.Backend
	if backend == "" || backend == "auto" {
		backend = "container"
	}
	image := spec.Image
	if image == "" {
		image = fmt.Sprintf("docker.io/library/%s:%s", target.DistroID, target.VersionID)
	}
	b.info(120*time.Millisecond, evidence.TypeBackendSelected, "backend selected", map[string]any{
		"backend":      backend,
		"runtime":      "docker",
		"image":        image,
		"image_digest": "sha256:6b0f1a9c2d3e4f50617283949a5b6c7d8e9f0a1b2c3d4e5f60718293a4b5c6d7",
		"platform":     "linux/" + goArchFor(target.Arch),
	})
	if spec.Image == "" {
		// The real container backend warns when it had to pick an unpinned
		// image, and the build screen must have somewhere to put a warning
		// that arrives before any progress does.
		b.warn(60*time.Millisecond, evidence.TypeWarning,
			"container image "+image+" is not pinned to a digest; the same build tomorrow may resolve against different packages",
			map[string]any{"code": "unpinned-image"})
	}

	b.info(1400*time.Millisecond, evidence.TypeAPTUpdate, "apt-get update", map[string]any{
		"failed":  false,
		"entries": 12,
	})

	// No packages *list* here on purpose: apt.resolve reports the resolved
	// closure, not the seeds the operator typed, and archiveFiles below IS
	// that closure. The count is a different matter — a closure is never
	// smaller than the seed set that produced it — so it comes from
	// fxScriptClosure.
	b.info(2600*time.Millisecond, evidence.TypeAPTResolve, "apt resolve complete", map[string]any{
		"selections": fxScriptClosure(spec),
		"unresolved": 0,
	})

	if len(spec.LocalDebs) > 0 {
		// The real shape: one event naming every external input, not one per
		// file. See docs/dev/cli-surface.md §5.1.
		names := make([]string, 0, len(spec.LocalDebs))
		for _, f := range spec.LocalDebs {
			names = append(names, filenameOf(f))
		}
		b.info(40*time.Millisecond, evidence.TypeInputExternal, "external packages",
			map[string]any{"names": names})
	}

	for _, f := range archiveFiles {
		if f.fromStore {
			b.info(30*time.Millisecond, evidence.TypeStoreHit, "ingested "+f.filename, map[string]any{
				"name": f.name, "version": f.version, "arch": f.arch,
				"sha256": f.digest, "size": f.size,
			})
			continue
		}
		b.fetch(f)
	}

	// The operator's own vendor downloads, last, because that is when the
	// engine fetches them — and they are the ones whose provenance is
	// weakest, so the UI has to be able to say so.
	for i, u := range spec.URLs {
		ver := lock.VerifiedURLUnverified
		if u.SHA256 != "" {
			ver = lock.VerifiedUserDigest
		}
		b.fetch(scriptFile{
			url:          u.URL,
			filename:     filenameOf(u.URL),
			size:         int64(2_400_000 + i*811_003),
			verification: ver,
			digest:       nonEmpty(u.SHA256, "cafebabe00112233445566778899aabbccddeeff00112233445566778899aabb"),
			// A vendor host behind a CDN is the realistic place for a
			// missing Content-Length, so the first one gets it too.
			unknownTotal: i == 0 && u.SHA256 == "",
		})
		if ver == lock.VerifiedURLUnverified {
			b.warn(20*time.Millisecond, evidence.TypeWarning,
				u.URL+" was downloaded over HTTPS with no publisher signature and no expected digest",
				map[string]any{"code": "url-unverified", "url": u.URL})
		}
	}

	b.info(180*time.Millisecond, evidence.TypeDoctorFinding,
		"nginx-common ships a maintainer script that starts a service on install",
		map[string]any{
			"check": "maintainer-script", "severity": "info",
			"package": "nginx-common", "flag": "starts-service",
		})

	if spec.PolicyPath != "" {
		b.info(60*time.Millisecond, evidence.TypePolicyFinding,
			"3 packages carry a non-DFSG-free licence",
			map[string]any{
				"rule": "require-free-licence", "severity": "warn",
				"packages": []string{"libssl3t64", "nginx-core", "nginx-common"},
			})
	}

	b.info(900*time.Millisecond, evidence.TypeClosedWorld, "closed-world check passed", map[string]any{
		"backend":      backend,
		"image":        image,
		"image_digest": "sha256:6b0f1a9c2d3e4f50617283949a5b6c7d8e9f0a1b2c3d4e5f60718293a4b5c6d7",
		"network":      "none",
	})

	b.info(600*time.Millisecond, evidence.TypeRepoIndexed, "repository indexed", map[string]any{
		"package_count": fxScriptClosure(spec),
		"pool_bytes":    totalScriptBytes(spec),
	})

	if !spec.NoSign {
		kind, keyID := "ed25519", "a1b2c3d4e5f60718"
		if strings.HasPrefix(spec.SignerRef, "gpg:") {
			kind, keyID = "gpg", strings.TrimPrefix(spec.SignerRef, "gpg:")
		}
		b.info(140*time.Millisecond, evidence.TypeManifestSigned, "manifest signed", map[string]any{
			"signer_kind": kind, "key_id": keyID,
		})
	}

	b.info(320*time.Millisecond, evidence.TypeBundleAssembled, "bundle assembled", map[string]any{
		"package_count": fxScriptClosure(spec),
		"added":         fxScriptClosure(spec),
		"removed":       0,
		"unreferenced":  0,
	})
	return b.steps
}

// fxScriptClosure is how many packages the scripted build reports having
// resolved, indexed and assembled.
//
// Which files it downloads is fixed: archiveFiles, whatever was selected, for
// the reason DefaultScript gives. The counts are not the same question, and
// leaving them fixed too meant a demo said the same thing whether the operator
// had picked one package or forty — and, worse, could report fewer selections
// than seeds, which no real resolve can do. A closure is never smaller than the
// seed set that produced it, so the demo takes the larger of its own fixed
// closure and what the operator actually asked for. External inputs are exact
// and are added on top: each vendor URL and each local .deb is one package,
// and neither is resolved against anything.
//
// This is arithmetic on a demo's labels, not resolution. Nothing here decides
// what gets installed, and no version is invented — which is the line rule 1
// actually draws.
//
// Prefixed fx because internal/cliadapter/fake is shared and this symbol was
// added while another package was writing in it.
func fxScriptClosure(spec cliadapter.BuildSpec) int {
	n := len(archiveFiles)
	if len(spec.Packages) > n {
		n = len(spec.Packages)
	}
	return n + len(spec.URLs) + len(spec.LocalDebs)
}

// targetFor invents a plausible target for the spec: the base id's own
// release when there is one, and Ubuntu 24.04 otherwise. It parses the id by
// splitting on ":" and "/", which is the id's documented shape — it is not
// deciding anything, only labelling a demo.
func targetFor(spec cliadapter.BuildSpec) snapshot.Target {
	t := snapshot.Target{
		DistroID: "ubuntu", VersionID: "24.04", Codename: "noble",
		PrettyName: "Ubuntu 24.04.1 LTS", Arch: "amd64",
		APTVersion: "2.7.14build2", DpkgVersion: "1.22.6ubuntu6.1",
	}
	if spec.Arch != "" {
		t.Arch = spec.Arch
	}
	if spec.BaseID == "" {
		return t
	}
	id := spec.BaseID
	if i := strings.IndexByte(id, '/'); i >= 0 {
		id = id[:i]
	}
	distro, version, ok := strings.Cut(id, ":")
	if !ok {
		return t
	}
	t.DistroID, t.VersionID = distro, version
	t.Codename = codenames[distro+":"+version]
	t.PrettyName = strings.ToUpper(distro[:1]) + distro[1:] + " " + version
	return t
}

// codenames covers the releases base.Builtin ships. A miss leaves the
// codename empty, which is honest: this is demo labelling, not a lookup table
// anything decides on.
var codenames = map[string]string{
	"debian:12": "bookworm", "debian:13": "trixie",
	"ubuntu:22.04": "jammy", "ubuntu:24.04": "noble", "ubuntu:26.04": "resolute",
}

func goArchFor(dpkgArch string) string {
	switch dpkgArch {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "armhf":
		return "arm"
	case "i386":
		return "386"
	default:
		return dpkgArch
	}
}

func filenameOf(url string) string {
	if i := strings.LastIndexByte(url, '/'); i >= 0 && i+1 < len(url) {
		return url[i+1:]
	}
	return "vendor.deb"
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func totalScriptBytes(spec cliadapter.BuildSpec) int64 {
	var n int64
	for _, f := range archiveFiles {
		n += f.size
	}
	for i := range spec.URLs {
		n += int64(2_400_000 + i*811_003)
	}
	return n
}
