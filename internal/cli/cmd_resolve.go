package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/apt"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/resolve"
	"github.com/inferops/debark/core/snapshot"
	"github.com/inferops/debark/internal/cli/progress"
)

// debark resolve — hidden, and the container backend's re-entry point
// (ADR-013). The full frozen contract — flags, the JSON envelope, the mount
// table the container driver uses to invoke this — lives in
// docs/dev/resolve-contract.md; that file is the single source of truth and
// this comment only summarises it, so read that file before changing
// anything here.
//
// In one sentence: open the snapshot, select a backend, call
// Backend.Resolve, and print one JSON envelope — schema_version, the
// resulting resolve.Plan verbatim, which backend actually ran, and an
// independent scan of --archives so the caller can catch a truncated mount
// without trusting either side alone. Nothing here signs, assembles, prunes
// or touches the store.
//
// Two rules the contract is strict about: stdout carries the envelope and
// *nothing else* — diagnostics, --json-events, and the "wrote plan to"
// notice when --plan-out names a file all go to stderr, so a caller reading
// stdout can never have the envelope corrupted; and the plan is written even
// on exit 3 (some input unresolved), because a container backend still
// needs the partial result.
func newResolveCmd() *cobra.Command {
	var (
		snapshotFlag, archives, work, planOut, backend, externalRepo string
		selfBinary                                                   string
		packages, externalNames, approvedKeys                        []string
		recommends                                                   string
		upgrades                                                     bool
		arch                                                         string
	)

	cmd := &cobra.Command{
		Use:    "resolve",
		Hidden: true,
		Short:  "Resolve one snapshot against a package set and print the plan.",
		Long: "Hidden debugging command and the container backend's re-entry point.\n" +
			"See docs/dev/resolve-contract.md for the full, frozen contract this\n" +
			"command and the container driver both compile against.",
		Args: cobra.NoArgs,
		RunE: wrapRun(func(ctx *Ctx, args []string) (err error) {
			if recommends != "" && !isOneOf(recommends, "true", "false") {
				return dferr.Usagef("resolve: --recommends must be true or false, got %q", recommends)
			}
			if !isOneOf(backend, "auto", "local", "container") {
				return dferr.Usagef("resolve: --backend must be local, auto or container, got %q", backend)
			}

			archive, err := snapshot.Open(ctx.Context, snapshotFlag)
			if err != nil {
				return err
			}
			defer func() { _ = archive.Close() }()

			effRecommends := resolveRecommends(archive, recommends)

			// §"two rules": --json-events goes to stderr for this command
			// specifically, never stdout, so it can never land inside the
			// envelope.
			events, closeEvents, err := resolveEvidenceSink(ctx)
			if err != nil {
				return err
			}
			defer func() {
				// The event stream is often the only durable record of the
				// run (core/evidence: "this is the one sink whose Close error
				// the Sink contract says must not be swallowed"), so a
				// truncated one is a real failure, not a detail — but never a
				// failure that gets to stand in for the one the command was
				// already reporting.
				if cerr := closeEvents(); cerr != nil && err == nil {
					err = dferr.Wrap(dferr.Environment, cerr, "close event stream")
				}
			}()

			be, caps, err := apt.SelectBackend(ctx.Context, apt.Selection{
				Target:    archive.Snapshot.Target,
				Requested: lock.Backend(backend),
				Events:    events,
				SelfPath:  ctx.selfBinary(selfBinary),
			})
			if err != nil {
				return err
			}

			in := apt.ResolveInput{
				Snapshot:         archive.Snapshot,
				SnapshotFilesDir: archive.FilesDir(),
				Packages:         packages,
				ExternalRepoDir:  externalRepo,
				ExternalNames:    externalNames,
				ArchivesDir:      archives,
				WorkDir:          work,
				Options: buildjob.Options{
					Upgrades:     upgrades,
					ArchOverride: arch,
				},
				Recommends:   effRecommends,
				Upgrades:     upgrades,
				PhasedPolicy: archive.Snapshot.PhasedPolicyFor(),
				ApprovedKeys: approvedKeys,
				DownloadOnly: true,
			}

			plan, err := be.Resolve(ctx.Context, in)
			if err != nil {
				return err
			}

			if err := writeResolveEnvelope(ctx, planOut, plan, be.Kind(), caps, archives); err != nil {
				return err
			}
			if n := len(plan.Unresolved); n > 0 {
				return dferr.New(dferr.Incomplete, "resolve: %d input(s) unresolved (plan written)", n)
			}
			return nil
		}),
	}

	flags := cmd.Flags()
	flags.StringVar(&snapshotFlag, "snapshot", "", "snapshot archive or extracted directory (required)")
	flags.StringVar(&archives, "archives", "", "where the backend leaves downloaded .deb files; Dir::Cache::archives (required)")
	flags.StringVar(&work, "work", "", "scratch directory for the private apt root (required)")
	flags.StringVar(&planOut, "plan-out", "", "write the envelope here instead of stdout")
	flags.StringVar(&backend, "backend", "auto", "local, auto or container; inside a container this is always local")
	flags.StringArrayVar(&packages, "package", nil, "an apt package name, optionally pinned (NAME[=VERSION]); repeatable")
	flags.StringVar(&externalRepo, "external-repo", "", "a directory of vendor .deb files already indexed with a Packages file")
	flags.StringArrayVar(&externalNames, "external-name", nil, "a package name --external-repo provides; repeatable")
	flags.StringVar(&selfBinary, "self-binary", "", selfBinaryFlagUsage)
	flags.StringVar(&recommends, "recommends", "", "true or false: override the target's effective Install-Recommends (default: from the snapshot)")
	flags.BoolVar(&upgrades, "upgrades", false, "add the full-upgrade --download-only pass")
	flags.StringVar(&arch, "arch", "", "resolve for a different architecture than the snapshot's")
	flags.StringArrayVar(&approvedKeys, "approved-key", nil, "restrict which archive key fingerprints resolution may trust; repeatable")

	_ = cmd.MarkFlagRequired("snapshot")
	_ = cmd.MarkFlagRequired("archives")
	_ = cmd.MarkFlagRequired("work")
	return cmd
}

// resolveRecommends applies --recommends=true|false, or with neither given
// derives the value from the snapshot's own captured apt.conf via
// snapshot.InstallRecommends — the same default `build` uses.
func resolveRecommends(archive *snapshot.Archive, flag string) bool {
	switch flag {
	case "true":
		return true
	case "false":
		return false
	default:
		fs := loadConfFileSet(archive)
		value, _ := snapshot.InstallRecommends(archive.Snapshot, fs)
		return value
	}
}

// loadConfFileSet reads just the captured apt.conf.d files an opened
// archive lists, which is all snapshot.InstallRecommends looks at; it never
// reads the rest of the (potentially large) files/ tree.
func loadConfFileSet(archive *snapshot.Archive) *snapshot.FileSet {
	fs := &snapshot.FileSet{Bytes: map[string][]byte{}}
	for _, f := range archive.Snapshot.APT.Conf {
		data, err := os.ReadFile(filepath.Join(archive.FilesDir(), filepath.FromSlash(f.ArchivePath)))
		if err == nil {
			fs.Bytes[f.ArchivePath] = data
		}
	}
	return fs
}

// resolveEvidenceSink is Ctx.NewEvidenceSink with one difference required by
// the frozen contract: --json-events "-" (and the interactive progress line)
// go to Stderr, never Stdout, because Stdout is reserved for the envelope.
func resolveEvidenceSink(ctx *Ctx) (evidence.Sink, func() error, error) {
	var sinks evidence.MultiSink
	var fileToClose io.Closer

	if target := ctx.jsonEvents; target != "" {
		if target == "-" {
			sinks = append(sinks, evidence.NewNDJSONSink(ctx.Stderr))
		} else {
			f, err := openEventsFile(target)
			if err != nil {
				return nil, nil, dferr.Wrap(dferr.Environment, err, "resolve: create %s", target)
			}
			fileToClose = f
			sinks = append(sinks, evidence.NewNDJSONSink(f))
		}
	}
	if ctx.Progress {
		sinks = append(sinks, progress.New(ctx.Stderr, true, ctx.Style))
	}
	if len(sinks) == 0 {
		return evidence.Discard{}, func() error { return nil }, nil
	}
	return sinks, func() error {
		err := sinks.Close()
		if fileToClose != nil {
			if cerr := fileToClose.Close(); err == nil {
				err = cerr
			}
		}
		return err
	}, nil
}

// resolveEnvelope is debark.resolveplan/v1, the single JSON object
// `resolve` prints — see docs/dev/resolve-contract.md.
type resolveEnvelope struct {
	SchemaVersion string             `json:"schema_version"`
	Plan          *resolve.Plan      `json:"plan"`
	Backend       resolveBackendInfo `json:"backend"`
	ArchivesDir   string             `json:"archives_dir"`
	Files         []resolveFileInfo  `json:"files"`
}

type resolveBackendInfo struct {
	Kind        string `json:"kind"`
	APTVersion  string `json:"apt_version"`
	DpkgVersion string `json:"dpkg_version"`
	DistroID    string `json:"distro_id"`
	VersionID   string `json:"version_id"`
}

type resolveFileInfo struct {
	Name     string `json:"name"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}

const resolveEnvelopeSchemaVersion = "debark.resolveplan/v1"

func writeResolveEnvelope(ctx *Ctx, planOut string, plan *resolve.Plan, backendKind lock.Backend, caps apt.Capabilities, archivesDir string) error {
	files, err := scanArchivesDir(archivesDir)
	if err != nil {
		return err
	}
	env := resolveEnvelope{
		SchemaVersion: resolveEnvelopeSchemaVersion,
		Plan:          plan,
		Backend: resolveBackendInfo{
			Kind:        string(backendKind),
			APTVersion:  caps.APTVersion,
			DpkgVersion: caps.DpkgVersion,
			DistroID:    caps.DistroID,
			VersionID:   caps.VersionID,
		},
		ArchivesDir: archivesDir,
		Files:       files,
	}
	return writeJSONTo(ctx, planOut, env)
}

// writeJSONTo writes v as indented JSON to path, or to ctx.Stdout when path
// is empty. Only the file-path case writes a notice, and it goes to Stderr:
// stdout must carry the envelope alone.
func writeJSONTo(ctx *Ctx, path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if path == "" || path == "-" {
		_, err := ctx.Stdout.Write(b)
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return dferr.Wrap(dferr.Environment, err, "resolve: create %s", dir)
		}
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return dferr.Wrap(dferr.Environment, err, "resolve: write %s", path)
	}
	return nil
}

// scanArchivesDir lists the .deb files actually present in dir and hashes
// each, independently of what the plan says — the contract's deliberate
// redundancy that catches a truncated mount or a partial download. Debian
// tooling always names a staged .deb <package>_<version>_<arch>.deb (Debian
// version strings never contain '_'), so splitting the basename on '_' is
// exact, not a heuristic.
func scanArchivesDir(dir string) ([]resolveFileInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, dferr.Wrap(dferr.Environment, err, "resolve: read archives dir %s", dir)
	}
	var out []resolveFileInfo
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".deb") {
			continue
		}
		path := filepath.Join(dir, de.Name())
		h, err := digest.AllFile(path)
		if err != nil {
			return nil, dferr.Wrap(dferr.Environment, err, "resolve: hash %s", path)
		}
		info := resolveFileInfo{Filename: de.Name(), SHA256: h.SHA256, Size: h.Size}
		if pkg, version, arch, ok := parseDebFilename(de.Name()); ok {
			info.Name, info.Version, info.Arch = pkg, version, arch
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out, nil
}

func parseDebFilename(name string) (pkg, version, arch string, ok bool) {
	base := strings.TrimSuffix(name, ".deb")
	parts := strings.SplitN(base, "_", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
