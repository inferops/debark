package catalog

// Target resolution: turning "the operator picked this" into a Target the
// catalogue can actually be built from.
//
// This is the join between the target screen and everything in this package.
// The frozen Target carries Sources, IndexRefs expands them, and the whole
// catalogue is made of the files those refs name — but nothing produces them
// until here. A Target with an empty Sources fails its own Validate, so
// without this file the real catalogue path is unreachable.
//
// # Both halves come out of exported debark APIs
//
// It was thought this needed a change to the core repository, because
// `snapshot list-bases --json` deliberately omits sources (base.ListEntry says
// why: they are long, they are the part an operator overrides, and an archive
// URI per row would bury the fields a person scans for). It does not.
//
//   - A stock base: base.Resolve(id, arch) returns a Definition whose Sources
//     field is the deb822 document itself, already materialised — ${codename},
//     ${version_id} and ${arch} substituted, and base.Validate refusing any
//     survivor.
//   - A snapshot: snapshot.Open(ctx, path) extracts the captured files, and
//     each APT.Sources entry's ArchivePath under FilesDir() is the target's own
//     sources file, byte for byte as it was on the machine.
//
// Both then go through the one parser in sources.go.
//
// # This file decides nothing
//
// It transcribes. Identity fields are copied across, sources are parsed, and
// that is all. There is no choosing a "best" mirror, no preferring one suite
// over another, no version anything. Where a source cannot be used, it is
// reported rather than judged.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/inferops/debark/core/base"
	"github.com/inferops/debark/core/snapshot"
)

// ErrNoUsableSources means the target's sources were read but none of them
// yields an index the catalogue could be built from.
//
// It is separate from a read failure because the operator's next move differs:
// a target whose sources are all deb-src, all flat repositories or all for
// another architecture is a target that simply has nothing to browse, and the
// accompanying []SourceProblem says which of those it was. Compare with
// errors.Is; the returned error wraps it with the summary.
var ErrNoUsableSources = errors.New("catalog: the target names no apt source the catalogue can be built from")

// TargetSelection is what the operator chose on the target screen: exactly one
// of the two kinds, with the fields that kind needs.
//
// It mirrors app.TargetSelection field for field rather than importing it,
// because internal/app is the view layer and this package must not depend on
// it — the dependency runs the other way. The duplication is three strings
// wide and it is what keeps a bridge type from becoming a catalogue type.
type TargetSelection struct {
	// Kind is TargetBase or TargetSnapshot.
	Kind TargetKind `json:"kind"`
	// BaseID is the base definition's id ("ubuntu:26.04/desktop") or a path to
	// an operator's own definition file. Required when Kind is TargetBase.
	// base.Resolve applies the same precedence the CLI does: a builtin id wins
	// outright, so no file lying about can redefine a documented base.
	BaseID string `json:"base_id,omitempty"`
	// Arch is the dpkg architecture to resolve the base for. Empty means the
	// builder's own architecture, which is what `debark --arch` defaults to
	// and what an operator who did not say almost always means. Ignored for a
	// snapshot, which carries the architecture it was measured on.
	Arch string `json:"arch,omitempty"`
	// SnapshotPath is the snapshot archive or extracted directory the operator
	// picked. Required when Kind is TargetSnapshot. It is read locally and
	// nothing about it is uploaded anywhere.
	SnapshotPath string `json:"snapshot_path,omitempty"`
}

// ResolveTarget turns a selection into a Target with real Sources.
//
// The returned []SourceProblem lists every apt source that was read but not
// used, deliberate skips included, so the UI can say "3 sources were ignored"
// rather than presenting a short catalogue as a complete one. It is returned
// alongside a non-nil error too: when resolution fails *because* nothing was
// usable, the problems are the explanation, and dropping them would leave the
// operator with "no usable binary apt sources" and nowhere to look.
//
// The Target is returned partially filled on error for the same reason — the
// identity fields are usually correct even when the sources are not, and a
// screen can still say which target it failed on.
//
// ctx is honoured throughout: opening a snapshot extracts an archive, which is
// the one part of this that takes measurable time.
func ResolveTarget(ctx context.Context, sel TargetSelection) (Target, []SourceProblem, error) {
	switch sel.Kind {
	case TargetBase:
		if strings.TrimSpace(sel.BaseID) == "" {
			return Target{}, nil, errors.New("catalog: no base was named to resolve")
		}
		if err := ctx.Err(); err != nil {
			return Target{}, nil, fmt.Errorf("catalog: resolving base %s: %w", sel.BaseID, err)
		}
		arch := sel.Arch
		if strings.TrimSpace(arch) == "" {
			arch = base.HostArch()
		}
		res, err := base.Resolve(sel.BaseID, arch)
		if err != nil {
			return Target{}, nil, fmt.Errorf("catalog: resolving base %s: %w", sel.BaseID, err)
		}
		return ResolveBase(res.Definition)

	case TargetSnapshot:
		if strings.TrimSpace(sel.SnapshotPath) == "" {
			return Target{}, nil, errors.New("catalog: no snapshot file was named to resolve")
		}
		return ResolveSnapshot(ctx, sel.SnapshotPath)

	default:
		return Target{}, nil, fmt.Errorf("catalog: unknown target kind %q", sel.Kind)
	}
}

// ResolveBase turns an already-resolved base definition into a Target.
//
// It is exported separately from ResolveTarget because a caller that has
// already listed the bases — the target screen, and the test that resolves
// every builtin one — has the Definition in hand and should not pay for a
// second lookup that could, in principle, answer differently.
//
// A surviving ${...} placeholder in the definition's Sources is an error here
// rather than a note. base.Validate refuses a definition that still contains
// one, so a survivor means that guarantee did not hold, and the honest answer
// to a broken guarantee is to stop rather than to fetch from an archive whose
// name contains a literal dollar sign.
func ResolveBase(def base.Definition) (Target, []SourceProblem, error) {
	t := Target{
		Kind:       TargetBase,
		BaseID:     def.ID,
		DistroID:   def.DistroID,
		VersionID:  def.VersionID,
		Codename:   def.Codename,
		Arch:       def.Arch,
		PrettyName: resolveBasePrettyName(def),
	}

	// The same path core/base's Synthesize writes this very document to
	// (sourcesDir + "/" + distro_id + ".sources"), so a problem reported
	// against a base names the file an operator would find if they built the
	// snapshot and looked. It also gives the parser the ".sources" suffix that
	// selects the deb822 grammar, which is what this document always is.
	name := resolveBaseSourcesPath(def.DistroID)
	entries, problems := ParseSourcesDeb822(name, []byte(def.Sources))
	sources, archProblems := SourcesForArch(def.Arch, entries)
	problems = append(problems, archProblems...)
	t.Sources = sources

	problems = append(problems, igSchemeProblems(t)...)

	for _, p := range problems {
		if p.Kind == SourceProblemPlaceholder {
			return t, problems, fmt.Errorf(
				"catalog: base %s still carries an unexpanded template placeholder in its apt sources, which base.Validate should have refused: %s",
				def.ID, p)
		}
	}

	if err := t.Validate(); err != nil {
		return t, problems, resolveNoSourcesError(err, "base "+def.ID, problems)
	}
	return t, problems, nil
}

// ResolveSnapshot turns a snapshot the operator picked from disk into a Target.
//
// path is an archive (snapshot.tar.zst) or an already-extracted snapshot
// directory; snapshot.Open accepts either, validates the document against the
// schema and digest-verifies every captured file before this function sees a
// byte of it. The archive is always closed, on every path out, because Open
// extracts into a temporary directory that would otherwise be left behind.
//
// Identity comes from snapshot.Target — the target's own /etc/os-release as it
// was measured — and the sources from every file in APT.Sources, parsed in the
// order the document lists them and with the grammar each file's own name
// implies. A file that cannot be read is reported, never skipped quietly: a
// snapshot missing one of its sources files would otherwise produce a
// catalogue that is short by a whole archive with nothing on screen to say so.
func ResolveSnapshot(ctx context.Context, path string) (Target, []SourceProblem, error) {
	if err := ctx.Err(); err != nil {
		return Target{}, nil, fmt.Errorf("catalog: opening snapshot %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	ar, err := snapshot.Open(ctx, path)
	if err != nil {
		return Target{}, nil, fmt.Errorf("catalog: opening snapshot %s: %w", path, err)
	}
	defer func() { _ = ar.Close() }()

	st := ar.Snapshot.Target
	t := Target{
		Kind:         TargetSnapshot,
		SnapshotPath: abs,
		DistroID:     st.DistroID,
		VersionID:    st.VersionID,
		Codename:     st.Codename,
		PrettyName:   st.PrettyName,
		Arch:         st.Arch,
	}

	// A synthesized snapshot carries its base id forward, a captured one does
	// not. This is not a stray field on the wrong kind of target: BaseID is
	// documented in iface.go as "the same value as snapshot.Origin.BaseID",
	// and this is the only place that value can enter a Target built from a
	// snapshot.
	//
	// It is load-bearing, so do not tidy it away as "a base-target field". A
	// synthesized snapshot is an *assumption* about a machine nobody measured,
	// exactly like a stock base, and the operator has to be told: if the base
	// assumed packages the real target lacks, the bundle is short, and that is
	// discovered on the offline side where it cannot be fixed. The frozen
	// Target has no origin-kind field, so a non-empty BaseID on a snapshot
	// target is the one signal the view layer has that it is looking at an
	// assumption rather than a measurement — and an empty one is what it reads
	// as "measured". Nothing downstream is disturbed by setting it:
	// Target.Identity excludes BaseID, so the cache key does not move, and
	// Validate asks a snapshot target only for its path.
	if ar.Snapshot.Synthesized() {
		t.BaseID = ar.Snapshot.Origin.BaseID
	}

	var problems []SourceProblem
	filesDir := ar.FilesDir()
	for _, f := range ar.Snapshot.APT.Sources {
		if err := ctx.Err(); err != nil {
			return t, problems, fmt.Errorf("catalog: reading the sources of %s: %w", path, err)
		}
		name := f.Path
		if name == "" {
			name = f.ArchivePath
		}
		data, readErr := resolveReadCaptured(filesDir, f)
		if readErr != nil {
			problems = append(problems, SourceProblem{
				Kind: SourceProblemUnreadable,
				// Sanitised for the reason srcSanitize documents: the name
				// comes from the snapshot's own document and the error text
				// quotes it back, so both can carry bytes a rendered string
				// cannot.
				File:   srcSanitize(name),
				Reason: srcSanitize("this source file could not be read out of the snapshot: " + readErr.Error()),
			})
			continue
		}
		entries, fileProblems := ParseSourcesFile(name, data)
		problems = append(problems, fileProblems...)
		sources, archProblems := SourcesForArch(st.Arch, entries)
		problems = append(problems, archProblems...)
		t.Sources = append(t.Sources, sources...)
	}
	problems = append(problems, igSchemeProblems(t)...)

	if err := t.Validate(); err != nil {
		return t, problems, resolveNoSourcesError(err, "snapshot "+abs, problems)
	}
	return t, problems, nil
}

// resolveReadCaptured reads one captured file out of an opened archive's
// files/ tree.
//
// The archive path is re-checked against escaping that tree even though
// snapshot.Open has already validated it. This function joins a path from a
// document onto a directory and then opens the result; the check costs one
// call and removes the whole question of whether the validation upstream
// covered this particular field.
func resolveReadCaptured(filesDir string, f snapshot.File) ([]byte, error) {
	rel := filepath.FromSlash(f.ArchivePath)
	if rel == "" || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("archive path %q is not a path inside the snapshot's files directory", f.ArchivePath)
	}
	full := filepath.Join(filesDir, rel)
	info, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if info.Size() > SourceMaxDocumentBytes {
		return nil, fmt.Errorf("it is %d bytes, past the %d a sources file may be", info.Size(), SourceMaxDocumentBytes)
	}
	return os.ReadFile(full)
}

// resolveNoSourcesError explains a Target that did not validate.
//
// Target.Validate's own message for the case that matters here — "target has
// no usable binary apt sources" — is correct and useless on its own: it says
// what is missing and nothing about why. Attaching the problem summary turns
// it into something an operator can act on, which is the project's
// definition-of-done item about actionable errors, applied at the one place
// this package can honour it.
func resolveNoSourcesError(err error, what string, problems []SourceProblem) error {
	if summary := SourceProblemsSummary(problems); summary != "" {
		return fmt.Errorf("catalog: %s: %w (%s)", what, err, summary)
	}
	if strings.Contains(err.Error(), "no usable binary apt sources") {
		return fmt.Errorf("catalog: %s: %w", what, errors.Join(err, ErrNoUsableSources))
	}
	return fmt.Errorf("catalog: %s: %w", what, err)
}

// resolveBaseSourcesPath is where core/base writes a definition's Sources
// document inside a synthesized snapshot. Kept identical so that the same
// source reported from a base and from a snapshot built out of that base has
// the same name in both.
func resolveBaseSourcesPath(distroID string) string {
	if distroID == "" {
		distroID = "base"
	}
	return "/etc/apt/sources.list.d/" + distroID + ".sources"
}

// resolveBasePrettyName composes the display string a base has no field for.
//
// A snapshot carries PRETTY_NAME from the target's own /etc/os-release; a base
// definition does not, because it describes a machine nobody has measured. Its
// Description is a full sentence meant for a table row, far too long for a
// title bar. So the label is composed from the parts the definition does
// carry, and it is display only — Target.Identity excludes PrettyName, so
// nothing here can change a cache key.
func resolveBasePrettyName(def base.Definition) string {
	parts := make([]string, 0, 3)
	if d := resolveDistroLabel(def.DistroID); d != "" {
		parts = append(parts, d)
	}
	if def.VersionID != "" {
		parts = append(parts, def.VersionID)
	}
	if def.Variant != "" {
		parts = append(parts, def.Variant)
	}
	return strings.Join(parts, " ")
}

// resolveDistroLabel capitalises a distro id for display. core/base has the
// same two-case switch unexported; this is the display half of it and nothing
// depends on the two agreeing.
func resolveDistroLabel(id string) string {
	switch id {
	case "ubuntu":
		return "Ubuntu"
	case "debian":
		return "Debian"
	case "":
		return ""
	default:
		r := []rune(id)
		r[0] = unicode.ToUpper(r[0])
		return string(r)
	}
}

// igSchemeProblems is the second half of the URI scheme allow-list: IndexRefs
// drops a source the catalogue cannot fetch from, and this is what says so.
//
// It is called once per Resolve, after Target.Sources is complete, so a
// caller of any of the three entry points gets the same answer for the same
// target. Splitting it this way keeps the filtering itself at the point where
// a URL would be built — a Target assembled without going through Resolve is
// still safe — while the sentence an operator reads is produced exactly once
// per resolution rather than once per index file.
//
// The problems carry no File or Line. The rejection is a property of the URI
// rather than of the stanza's syntax, and the parser that knew the line has
// already returned; Text carries the URI itself, which is what an operator
// searches their sources for.
func igSchemeProblems(t Target) []SourceProblem {
	_, problems := t.IndexRefsWithProblems()
	return problems
}
