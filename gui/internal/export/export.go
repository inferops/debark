package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FreeSpaceUnknown is the value to pass as Request.DestFreeBytes when the
// destination's free space could not be measured. Planning then skips the
// free-space refusal and records a warning instead, because a copy that might
// fail is still better than refusing to try. Note that zero is NOT this value:
// zero means a full drive, and is refused.
const FreeSpaceUnknown = int64(-1)

// Phase says which part of an export a Progress belongs to.
type Phase string

const (
	// PhaseCopy is reading the bundle and writing the drive.
	PhaseCopy Phase = "copy"
	// PhaseFlush is fsyncing the directories at the end of the copy. It is
	// usually brief, because each file was already fsynced as it was finished.
	PhaseFlush Phase = "flush"
	// PhaseVerify is re-reading the drive and comparing SHA-256 digests.
	PhaseVerify Phase = "verify"
)

// Progress is one sample of an export in flight. It carries everything a
// progress bar and an ETA need; the UI computes the rate from BytesDone and
// Elapsed rather than being told a smoothed number it cannot reason about.
type Progress struct {
	Phase      Phase         `json:"phase"`
	File       string        `json:"file"` // slash-separated, relative to the bundle root
	FilesDone  int           `json:"filesDone"`
	FilesTotal int           `json:"filesTotal"`
	BytesDone  int64         `json:"bytesDone"`
	BytesTotal int64         `json:"bytesTotal"`
	Elapsed    time.Duration `json:"elapsed"`
}

// SymlinkPolicy selects what an export does with a symbolic link in the bundle.
type SymlinkPolicy int

const (
	// SymlinkDereference — the default — copies the contents of the file a
	// link points at, storing it under the link's own name.
	//
	// A bundle is data, not an installed tree: what the offline machine needs
	// is the bytes of every .deb, the lock plan and the signed manifest, and
	// nothing downstream reads a link as a link. Recreating links is also
	// simply impossible on the drives this app most often targets — FAT and
	// exFAT have no symbolic links at all — so a copy that recreated them
	// would fail on exactly the common case. Skipping them instead would
	// silently drop data, which is the one thing this package must never do.
	//
	// Only links resolving to a regular file are dereferenced. A link to a
	// directory, to a device, or to nothing at all is refused at plan time with
	// an actionable message, so no loop can be entered and nothing is dropped
	// in silence.
	SymlinkDereference SymlinkPolicy = iota

	// SymlinkFail refuses to export a bundle containing any symbolic link.
	SymlinkFail
)

// Options configure an Exporter. The zero Options is usable and safe: it
// verifies, uses a 1 MiB buffer, ticks progress at 10 Hz and dereferences
// symbolic links.
type Options struct {
	// Progress, if set, receives one sample per tick. It is called
	// synchronously from the goroutine doing the copying and must not block:
	// the copy is stopped for its duration. The Wails binding layer should
	// translate it into an event and return immediately.
	Progress func(Progress)

	// SkipVerify turns off the verification pass.
	//
	// Leave it false. Verification is the step that makes this feature worth
	// having, and this field exists only so a caller who genuinely does not
	// want it has to write the words down. With it set, Run removes the
	// incomplete marker itself and the Result says verification was skipped.
	SkipVerify bool

	// MarginBytes overrides the free space an export refuses to consume.
	// Zero means the default: 2% of the payload, clamped to 64 MB..512 MB.
	MarginBytes int64

	// Symlinks selects the symbolic-link policy. Zero is SymlinkDereference.
	Symlinks SymlinkPolicy

	// BufferSize is the copy buffer in bytes. Zero means 1 MiB.
	BufferSize int

	// ProgressInterval is the minimum gap between progress callbacks. Zero
	// means 100 ms.
	ProgressInterval time.Duration

	// openDest and now are test seams. They are unexported on purpose: the
	// production surface has no way to redirect writes away from the real
	// filesystem, and package tests inject a failing writer and a fake clock
	// through them.
	openDest func(path string) (destFile, error)
	now      func() time.Time
}

func (o Options) normalized() Options {
	if o.BufferSize <= 0 {
		o.BufferSize = defaultBufferSize
	}
	if o.ProgressInterval <= 0 {
		o.ProgressInterval = defaultProgressInterval
	}
	if o.openDest == nil {
		o.openDest = openDestFile
	}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

// Request is what the operator asked for.
type Request struct {
	// SourceDir is the bundle folder to copy. Its contents, not the folder
	// itself, land in DestDir.
	SourceDir string

	// DestDir is the folder on the mounted drive to copy into. It is created
	// if its parent exists.
	DestDir string

	// DestFreeBytes is the free space on the filesystem holding DestDir, as
	// measured by the caller. Pass FreeSpaceUnknown if it could not be
	// measured. Zero is not "unknown" — zero means a full drive and is
	// refused, which is the safe reading if a caller forgets to set it.
	//
	// Volume enumeration reports capacity as an unsigned count that is zero
	// when it could not be determined, so a caller holding a Volume writes:
	//
	//	free := export.FreeSpaceUnknown
	//	if v.TotalBytes > 0 {
	//		free = int64(v.FreeBytes)
	//	}
	//
	// That is the only coupling between the two halves of this package, and it
	// is deliberately a number rather than a type.
	DestFreeBytes int64
}

// PlanFile is one regular file an export will copy.
type PlanFile struct {
	// Rel is the path relative to the bundle root, always slash-separated.
	Rel string `json:"rel"`
	// Size is the number of bytes to copy.
	Size int64 `json:"size"`
	// ModTime is the source modification time, reapplied to the copy on a
	// best-effort basis.
	ModTime time.Time `json:"modTime"`
	// Symlink records that the source entry was a symbolic link that was
	// dereferenced to a regular file.
	Symlink bool `json:"symlink"`
	// SourcePath is the absolute path actually opened for reading.
	SourcePath string `json:"sourcePath"`
}

// Plan is everything an export is about to do, computed without writing
// anything. It is safe to show the operator and ask them to confirm.
type Plan struct {
	SourceDir string `json:"sourceDir"`
	DestDir   string `json:"destDir"`

	// Dirs are the directories to create, relative to DestDir and
	// slash-separated, already in an order where each parent precedes its
	// children.
	Dirs []string `json:"dirs"`
	// Files are the files to copy, in the order they will be copied.
	Files []PlanFile `json:"files"`

	TotalFiles int   `json:"totalFiles"`
	TotalBytes int64 `json:"totalBytes"`

	// RequiredBytes is TotalBytes with every file rounded up to a filesystem
	// allocation unit, plus MarginBytes. This, not TotalBytes, is what was
	// compared against FreeBytes.
	RequiredBytes int64 `json:"requiredBytes"`
	// FreeBytes is the free space the caller reported, or FreeSpaceUnknown.
	FreeBytes int64 `json:"freeBytes"`
	// MarginBytes is the slack the export refuses to consume.
	MarginBytes int64 `json:"marginBytes"`

	// Warnings are things the operator should know that are not fatal: a name
	// FAT cannot store, a file too big for FAT32, unknown free space. Show
	// them; do not swallow them.
	Warnings []string `json:"warnings"`

	// MarkerPath is where the incomplete marker will be written.
	MarkerPath string `json:"markerPath"`
	// VerifyPlanned is false only when Options.SkipVerify was set.
	VerifyPlanned bool `json:"verifyPlanned"`
}

// EquivalentCommand is the nearest shell equivalent of the copy stage, for a UI
// that shows the operator what it is about to do. It is genuinely only the copy
// stage: it does not fsync, it does not verify and it does not write the
// incomplete marker.
func (p *Plan) EquivalentCommand() []string {
	return []string{"cp", "-r", "-L", "--", filepath.Join(p.SourceDir, "."), p.DestDir}
}

// CopiedFile records one file as it was written, including the digest computed
// from the bytes handed to the destination. Verify compares against this.
type CopiedFile struct {
	Rel    string `json:"rel"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// RunReport is the outcome of the copy stage. Run returns a partially filled
// report alongside an error when it fails part-way, so the UI can say how far
// it got and on which file.
type RunReport struct {
	SourceDir string `json:"sourceDir"`
	DestDir   string `json:"destDir"`

	Files       []CopiedFile `json:"files"`
	FilesCopied int          `json:"filesCopied"`
	BytesCopied int64        `json:"bytesCopied"`

	Started  time.Time     `json:"started"`
	Finished time.Time     `json:"finished"`
	Duration time.Duration `json:"duration"`

	// MarkerPath is the incomplete marker. MarkerPresent says whether it is
	// still there — after a successful Run it always is, unless verification
	// was skipped.
	MarkerPath    string `json:"markerPath"`
	MarkerPresent bool   `json:"markerPresent"`

	// VerifyRequired is true when the caller still owes a Verify call. Run
	// leaves it true unless Options.SkipVerify was set.
	VerifyRequired bool `json:"verifyRequired"`

	// FailedFile is the file the copy stopped on, if it stopped.
	FailedFile string `json:"failedFile"`

	// Notes are non-fatal remarks, such as a directory the platform would not
	// let us fsync.
	Notes []string `json:"notes"`
}

// MismatchReason says how a file on the drive failed verification.
type MismatchReason string

const (
	MismatchMissing    MismatchReason = "missing"
	MismatchSize       MismatchReason = "size"
	MismatchContent    MismatchReason = "content"
	MismatchNotCopied  MismatchReason = "not_copied"
	MismatchUnreadable MismatchReason = "unreadable"
)

// Mismatch is one file that did not survive the trip.
type Mismatch struct {
	Rel        string         `json:"rel"`
	Reason     MismatchReason `json:"reason"`
	Detail     string         `json:"detail"`
	WantSize   int64          `json:"wantSize"`
	GotSize    int64          `json:"gotSize"`
	WantSHA256 string         `json:"wantSha256"`
	GotSHA256  string         `json:"gotSha256"`
}

// VerifyReport says exactly what was checked and what came back.
type VerifyReport struct {
	// Method describes, in one sentence an operator can read, what was checked.
	Method string `json:"method"`
	// Caveat states the limit of that check, honestly.
	Caveat string `json:"caveat"`

	FilesChecked int   `json:"filesChecked"`
	BytesChecked int64 `json:"bytesChecked"`

	Started  time.Time     `json:"started"`
	Finished time.Time     `json:"finished"`
	Duration time.Duration `json:"duration"`

	// Mismatches is empty when OK is true.
	Mismatches []Mismatch `json:"mismatches"`
	OK         bool       `json:"ok"`

	// MarkerRemoved says whether the incomplete marker was cleared, which
	// happens only when OK is true.
	MarkerRemoved bool `json:"markerRemoved"`
}

const (
	verifyMethod = "Every file was hashed with SHA-256 as it was written, flushed to the drive with fsync, then re-read from the drive and hashed again; both the size on the drive and the two digests had to match, for every file."
	verifyCaveat = "This proves the drive accepted and returned exactly the bytes that were read from the bundle. It cannot promise those flash cells will still read back correctly months from now — check the bundle with debark verify on the offline machine before you rely on it."
	skipMethod   = "Verification was skipped because Options.SkipVerify was set. Nothing on the drive has been read back or checked."
)

// Result is the whole export, start to finish, in the shape a summary screen
// needs.
type Result struct {
	Plan         *Plan         `json:"plan"`
	Copy         *RunReport    `json:"copy"`
	Verification *VerifyReport `json:"verification"`

	Started  time.Time     `json:"started"`
	Finished time.Time     `json:"finished"`
	Duration time.Duration `json:"duration"`

	// Complete is true only when every file was copied, flushed, re-read and
	// matched, and the incomplete marker was removed — that is, only when the
	// bundle on the drive is trustworthy. It is deliberately false for an
	// export that was told to skip verification, because such a drive has not
	// been checked and nothing here may pretend otherwise.
	//
	// This, not the absence of an error, is the field to branch on before
	// telling an operator the drive is ready to carry across the air gap.
	Complete bool `json:"complete"`

	// Summary is a one-sentence human account of the outcome.
	Summary string `json:"summary"`
}

// Exporter copies a finished bundle folder onto an already-mounted drive and
// proves that the copy arrived intact.
//
// # Safety boundary
//
// An export does what a file manager does and nothing more: it reads files and
// writes files through the ordinary filesystem API. It never opens a block
// device, never partitions, never formats, never makes a drive bootable and
// never needs root. The destination is a path the volume enumeration in this
// package already found mounted; the copier is handed a path and a free-byte
// count and deliberately does not take a Volume, so the two halves stay
// independent.
//
// # Why it is this careful
//
// The audience moves software across an air gap. A silently truncated copy is
// discovered on the offline side, where it cannot be fixed. So an export
//
//   - refuses up front if the bundle demonstrably will not fit,
//   - reports progress often enough for a real bar and an ETA,
//   - fsyncs every file and the directories that hold them,
//   - re-reads and SHA-256 verifies every byte it wrote, by default, and
//   - marks the destination unusable for the whole time it is in flight,
//     clearing the mark only after verification passes.
//
// # Three stages
//
// The app layer drives Plan, then Run, then Verify. Plan is cheap and writes
// nothing, so it is safe to show the operator for confirmation; Run does the
// writing; Verify is what makes the feature worth having. Export runs all
// three.
//
// # The incomplete marker — the caller's obligation
//
// Run writes MarkerName into the destination directory before its first byte
// and does not remove it. Only a fully successful Verify removes it (or Run
// itself, if Options.SkipVerify was deliberately set). Every other outcome —
// cancellation, a full drive, a yanked drive, a crash, a caller who forgets to
// call Verify — leaves the marker in place. A caller must therefore treat a
// destination containing MarkerName as unusable; IsIncomplete answers that
// question. This is deliberately the safe default: forgetting a step leaves the
// drive marked bad, never marked good.
//
// # Concurrency
//
// An export runs entirely on its caller's goroutine and starts none of its own,
// so cancelling the context cannot leave anything spinning. Options.Progress is
// called synchronously from that goroutine and must not block.
type Exporter struct {
	opts Options
}

// NewExporter returns an Exporter. The zero Options is valid.
func NewExporter(opts Options) *Exporter {
	return &Exporter{opts: opts.normalized()}
}

// Plan walks the bundle, counts it, checks it fits and returns what is about to
// happen. It writes nothing. An error from Plan means the export must not start
// — including the free-space refusal, which exists so a copy that can be
// predicted to fail is never begun.
func (e *Exporter) Plan(ctx context.Context, req Request) (*Plan, error) {
	src, err := filepath.Abs(filepath.Clean(req.SourceDir))
	if err != nil {
		return nil, classify(sideSource, "read", req.SourceDir, err)
	}
	dst, err := filepath.Abs(filepath.Clean(req.DestDir))
	if err != nil {
		return nil, classify(sideDest, "use", req.DestDir, err)
	}

	si, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &ExportError{
				Kind: ErrKindSourceMissing, Op: "read", Path: src, Err: err,
				Summary: fmt.Sprintf("There is no bundle folder at %s.", src),
				Hint:    "Build a bundle first, or pick the folder that contains its manifest.",
			}
		}
		return nil, classify(sideSource, "read", src, err)
	}
	if !si.IsDir() {
		return nil, &ExportError{
			Kind: ErrKindNotDirectory, Op: "read", Path: src,
			Summary: fmt.Sprintf("%s is a file, not a bundle folder.", src),
			Hint:    "Pick the folder the build produced, not a file inside it.",
		}
	}

	if err := checkDestination(dst); err != nil {
		return nil, err
	}
	if err := checkOverlap(src, dst); err != nil {
		return nil, err
	}

	p := &Plan{
		SourceDir:     src,
		DestDir:       dst,
		FreeBytes:     req.DestFreeBytes,
		MarkerPath:    markerPath(dst),
		VerifyPlanned: !e.opts.SkipVerify,
	}
	if err := e.walk(ctx, p); err != nil {
		return nil, err
	}

	if p.TotalFiles == 0 {
		return nil, &ExportError{
			Kind: ErrKindEmptySource, Op: "read", Path: src,
			Summary: fmt.Sprintf("The bundle folder %s contains no files.", src),
			Hint:    "Point the export at the folder the build wrote, and check the build actually finished.",
		}
	}

	p.MarginBytes = e.opts.MarginBytes
	if p.MarginBytes <= 0 {
		p.MarginBytes = marginFor(p.TotalBytes)
	}
	var onDisk int64
	for _, f := range p.Files {
		onDisk += roundUp(f.Size, allocationUnit)
	}
	p.RequiredBytes = onDisk + p.MarginBytes

	switch {
	case req.DestFreeBytes < 0:
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"The free space on this drive could not be measured, so the export cannot promise the bundle (%s) will fit.",
			humanBytes(p.TotalBytes)))
	case req.DestFreeBytes < p.RequiredBytes:
		short := p.RequiredBytes - req.DestFreeBytes
		return nil, &ExportError{
			Kind: ErrKindNotEnoughRoom, Op: "check", Path: dst,
			Summary: fmt.Sprintf("The drive does not have room for this bundle: it needs %s (%s of files plus a %s safety margin) but only %s is free.",
				humanBytes(p.RequiredBytes), humanBytes(p.TotalBytes), humanBytes(p.MarginBytes), humanBytes(req.DestFreeBytes)),
			Hint: fmt.Sprintf("Free up %s on the drive, or choose a larger one, then export again.", humanBytes(short)),
		}
	}

	return p, nil
}

// checkDestination requires either an existing destination directory, or a
// parent that exists so the directory can be created. Getting this wrong at
// plan time matters: after it passes, a missing destination during the copy can
// honestly be reported as a removed drive.
func checkDestination(dst string) error {
	di, err := os.Stat(dst)
	switch {
	case err == nil && di.IsDir():
		return nil
	case err == nil:
		return &ExportError{
			Kind: ErrKindNotDirectory, Op: "use", Path: dst,
			Summary: fmt.Sprintf("%s is a file, so the bundle cannot be copied into it.", dst),
			Hint:    "Choose a folder on the drive instead.",
		}
	case !errors.Is(err, fs.ErrNotExist):
		return classify(sideDest, "use", dst, err)
	}

	parent := filepath.Dir(dst)
	pi, perr := os.Stat(parent)
	if perr != nil || !pi.IsDir() {
		return &ExportError{
			Kind: ErrKindSourceMissing, Op: "use", Path: dst, Err: perr,
			Summary: fmt.Sprintf("The folder %s does not exist and neither does the folder that would contain it.", dst),
			Hint:    "Check the drive is still plugged in and mounted, then pick a folder on it.",
		}
	}
	return nil
}

// checkOverlap refuses a copy where source and destination are the same tree or
// one contains the other. Copying a folder into itself destroys the thing being
// copied, and no error further down would explain what happened.
func checkOverlap(src, dst string) error {
	switch {
	case src == dst:
		return &ExportError{
			Kind: ErrKindOverlap, Op: "use", Path: dst,
			Summary: "The bundle folder and the destination folder are the same folder.",
			Hint:    "Choose a folder on the drive as the destination.",
		}
	case within(src, dst):
		return &ExportError{
			Kind: ErrKindOverlap, Op: "use", Path: dst,
			Summary: fmt.Sprintf("The destination %s is inside the bundle folder, so the export would copy the bundle into itself.", dst),
			Hint:    "Choose a folder on the drive, outside the bundle.",
		}
	case within(dst, src):
		return &ExportError{
			Kind: ErrKindOverlap, Op: "use", Path: dst,
			Summary: fmt.Sprintf("The bundle folder is inside the destination %s, so the export would overwrite the bundle while reading it.", dst),
			Hint:    "Choose a different folder on the drive.",
		}
	}
	return nil
}

// within reports whether child is parent or lies beneath it.
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// walk fills in Dirs, Files, the totals and the advisory warnings.
func (e *Exporter) walk(ctx context.Context, p *Plan) error {
	var dirs []string
	var files []PlanFile
	var warnings []string

	err := filepath.WalkDir(p.SourceDir, func(path string, d fs.DirEntry, werr error) error {
		if cerr := ctx.Err(); cerr != nil {
			return classify(sideSource, "read", path, cerr)
		}
		if werr != nil {
			return classify(sideSource, "read", path, werr)
		}
		rel, rerr := filepath.Rel(p.SourceDir, path)
		if rerr != nil {
			return classify(sideSource, "read", path, rerr)
		}
		slashRel := filepath.ToSlash(rel)

		if problem := nameProblem(d.Name()); problem != "" && slashRel != "." {
			warnings = append(warnings, fmt.Sprintf(
				"%s may not be storable on a FAT or exFAT drive because %s.", slashRel, problem))
		}

		if d.IsDir() {
			if slashRel != "." {
				dirs = append(dirs, slashRel)
			}
			return nil
		}

		// A marker left at the bundle root by an earlier interrupted export is
		// not part of the bundle, and copying it would collide with the marker
		// this export writes.
		if slashRel == MarkerName {
			warnings = append(warnings, fmt.Sprintf(
				"The bundle folder still contains %s from an interrupted export, so it may itself be incomplete. It will not be copied.", MarkerName))
			return nil
		}

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			pf, perr := e.planSymlink(path, slashRel)
			if perr != nil {
				return perr
			}
			files = append(files, pf)
		case d.Type().IsRegular():
			info, ierr := d.Info()
			if ierr != nil {
				return classify(sideSource, "read", path, ierr)
			}
			files = append(files, PlanFile{
				Rel: slashRel, Size: info.Size(), ModTime: info.ModTime(), SourcePath: path,
			})
		default:
			return &ExportError{
				Kind: ErrKindUnsupported, Op: "read", Path: path,
				Summary: fmt.Sprintf("%s is not an ordinary file, so it cannot be copied to a drive.", slashRel),
				Hint:    "A bundle should contain only ordinary files and folders. Rebuild the bundle, or remove that entry, and export again.",
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	sort.Strings(dirs)
	for _, f := range files {
		p.TotalBytes += f.Size
		if f.Size > fat32MaxFileSize {
			warnings = append(warnings, fmt.Sprintf(
				"%s is %s, which a FAT32 drive cannot store — it needs exFAT or ext4.", f.Rel, humanBytes(f.Size)))
		}
	}
	p.Dirs = dirs
	p.Files = files
	p.TotalFiles = len(files)
	p.Warnings = append(p.Warnings, warnings...)
	return nil
}

// planSymlink applies the symbolic-link policy. See SymlinkDereference for why
// dereferencing is the default and why anything other than a regular file is
// refused rather than skipped.
func (e *Exporter) planSymlink(path, slashRel string) (PlanFile, error) {
	if e.opts.Symlinks == SymlinkFail {
		return PlanFile{}, &ExportError{
			Kind: ErrKindUnsupported, Op: "read", Path: path,
			Summary: fmt.Sprintf("%s is a shortcut (a symbolic link), and this export is set to refuse them.", slashRel),
			Hint:    "Rebuild the bundle with real files instead of links, or allow links to be followed.",
		}
	}
	info, err := os.Stat(path) // follows the link
	if err != nil {
		return PlanFile{}, &ExportError{
			Kind: ErrKindUnsupported, Op: "read", Path: path, Err: err,
			Summary: fmt.Sprintf("%s is a shortcut (a symbolic link) that points at something that is not there.", slashRel),
			Hint:    "Rebuild the bundle: a broken link means a file the offline machine will need is missing.",
		}
	}
	if !info.Mode().IsRegular() {
		return PlanFile{}, &ExportError{
			Kind: ErrKindUnsupported, Op: "read", Path: path,
			Summary: fmt.Sprintf("%s is a shortcut (a symbolic link) to a folder or a device, which cannot be copied to a drive.", slashRel),
			Hint:    "A bundle should contain only ordinary files and folders. Rebuild the bundle without that link.",
		}
	}
	return PlanFile{
		Rel: slashRel, Size: info.Size(), ModTime: info.ModTime(),
		Symlink: true, SourcePath: path,
	}, nil
}

// Run performs the copy described by p.
//
// It writes the incomplete marker first, creates the directories, then copies
// each file, fsyncing it before moving on. It does not remove the marker: the
// caller must call Verify, which removes it only if everything checks out. The
// single exception is Options.SkipVerify, where Run removes the marker itself.
//
// On failure Run returns a partially filled report together with the error, so
// the UI can say how far it got. Whatever it managed to write stays on the
// drive, marked incomplete.
//
// Run never starts a goroutine. Cancelling ctx stops it within one buffer's
// worth of copying, plus however long the in-flight write or fsync takes to
// return — a removed drive can make that a few seconds.
func (e *Exporter) Run(ctx context.Context, p *Plan) (*RunReport, error) {
	if p == nil {
		return nil, &ExportError{
			Kind: ErrKindIO, Op: "copy",
			Summary: "The export was asked to run without a plan.",
			Hint:    "Call Plan first and show the operator what it found.",
		}
	}

	started := e.opts.now()
	rep := &RunReport{
		SourceDir:      p.SourceDir,
		DestDir:        p.DestDir,
		Started:        started,
		MarkerPath:     p.MarkerPath,
		VerifyRequired: !e.opts.SkipVerify,
	}
	finish := func(err error) (*RunReport, error) {
		rep.Finished = e.opts.now()
		rep.Duration = rep.Finished.Sub(rep.Started)
		rep.MarkerPresent = markerExists(p.MarkerPath)
		return rep, err
	}

	if err := os.MkdirAll(p.DestDir, 0o777); err != nil {
		return finish(classify(sideDest, "create", p.DestDir, err))
	}
	// Writing the marker is also the writability probe: if the drive is
	// read-only or the folder is not ours, we find out before copying a byte.
	if err := writeMarker(p); err != nil {
		return finish(err)
	}
	rep.MarkerPresent = true

	progress := newReporter(e.opts, PhaseCopy, p.TotalFiles, p.TotalBytes)
	cp := newCopier(e.opts, progress)

	created := make([]string, 0, len(p.Dirs)+1)
	created = append(created, p.DestDir)
	for _, d := range p.Dirs {
		if err := ctx.Err(); err != nil {
			return finish(classify(sideDest, "copy", p.DestDir, err))
		}
		full := filepath.Join(p.DestDir, filepath.FromSlash(d))
		if err := os.MkdirAll(full, 0o777); err != nil {
			return finish(classify(sideDest, "create", full, err))
		}
		created = append(created, full)
	}

	for _, f := range p.Files {
		if err := ctx.Err(); err != nil {
			rep.FailedFile = f.Rel
			return finish(classify(sideDest, "copy", p.DestDir, err))
		}
		dstPath := filepath.Join(p.DestDir, filepath.FromSlash(f.Rel))
		progress.beginFile(f.Rel)

		sum, n, err := cp.copyFile(ctx, f.SourcePath, dstPath, f.Size)
		if err != nil {
			rep.FailedFile = f.Rel
			rep.BytesCopied += n
			return finish(err)
		}
		// Modification times are cosmetic — the bundle's integrity lives in its
		// manifest, not its timestamps — and FAT stores them at two-second
		// granularity, so a failure here is not worth failing an export over.
		_ = os.Chtimes(dstPath, f.ModTime, f.ModTime)

		rep.Files = append(rep.Files, CopiedFile{Rel: f.Rel, Bytes: n, SHA256: sum})
		rep.FilesCopied++
		rep.BytesCopied += n
		progress.endFile()
	}
	// Force a sample at 100% of the copy before the phase changes, so the bar
	// visibly completes rather than jumping to "flushing" from 97%.
	progress.finish()

	progress.setPhase(PhaseFlush)
	for _, d := range created {
		if err := syncDir(d); err != nil {
			cerr := classify(sideDest, "flush the folder", d, err)
			if IsDriveRemoved(cerr) || IsOutOfSpace(cerr) {
				return finish(cerr)
			}
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"The folder %s could not be flushed to the drive on this platform; the files inside it were flushed individually.", d))
		}
	}
	progress.finish()

	if e.opts.SkipVerify {
		if err := os.Remove(p.MarkerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return finish(classify(sideDest, "remove", p.MarkerPath, err))
		}
		rep.Notes = append(rep.Notes, "Verification was skipped, so nothing on the drive has been read back and checked.")
	}
	return finish(nil)
}

// Verify re-reads every file from the drive and compares it against what Run
// says it wrote: first the size, then a SHA-256 digest of the whole file.
//
// A file-size comparison alone is not verification — a drive that silently
// dropped a write, or a cable that flipped a bit, produces a file of exactly
// the right length. Both digests are computed on this machine, from the bytes
// on the drive after fsync, which is what makes a truncated or corrupted copy
// detectable here rather than on the offline side.
//
// Only a fully successful Verify removes the incomplete marker. When anything
// mismatches, Verify returns the report alongside an ExportError of kind
// ErrKindVerifyFailed and the marker stays, so the destination remains marked
// unusable.
func (e *Exporter) Verify(ctx context.Context, p *Plan, run *RunReport) (*VerifyReport, error) {
	if p == nil || run == nil {
		return nil, &ExportError{
			Kind: ErrKindIO, Op: "verify",
			Summary: "The export was asked to verify a copy that never ran.",
			Hint:    "Call Plan and Run first.",
		}
	}

	started := e.opts.now()
	vr := &VerifyReport{Method: verifyMethod, Caveat: verifyCaveat, Started: started}

	written := make(map[string]CopiedFile, len(run.Files))
	for _, f := range run.Files {
		written[f.Rel] = f
	}

	progress := newReporter(e.opts, PhaseVerify, p.TotalFiles, p.TotalBytes)
	cp := newCopier(e.opts, progress)

	for _, f := range p.Files {
		if err := ctx.Err(); err != nil {
			return vr, classify(sideDest, "verify", p.DestDir, err)
		}
		progress.beginFile(f.Rel)
		// A returned error aborts the whole verification; a recorded mismatch
		// does not, so the operator sees every bad file at once rather than one
		// per attempt.
		fatal := e.verifyOne(ctx, cp, p, f, written, vr)
		progress.endFile()
		if fatal != nil {
			return vr, fatal
		}
	}
	progress.finish()

	vr.Finished = e.opts.now()
	vr.Duration = vr.Finished.Sub(vr.Started)
	vr.OK = len(vr.Mismatches) == 0

	if !vr.OK {
		return vr, verifyError(p, vr)
	}

	if err := os.Remove(p.MarkerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		vr.OK = false
		return vr, classify(sideDest, "remove", p.MarkerPath, err)
	}
	if err := syncDir(p.DestDir); err != nil {
		cerr := classify(sideDest, "flush the folder", p.DestDir, err)
		if IsDriveRemoved(cerr) {
			vr.OK = false
			return vr, cerr
		}
	}
	vr.MarkerRemoved = true
	return vr, nil
}

// verifyOne checks one file on the drive. It appends to vr.Mismatches for a
// problem with the file, and returns a non-nil error only for a problem with
// the drive or the operator's context — the cases where continuing is pointless.
func (e *Exporter) verifyOne(ctx context.Context, cp *copier, p *Plan, f PlanFile, written map[string]CopiedFile, vr *VerifyReport) error {
	dstPath := filepath.Join(p.DestDir, filepath.FromSlash(f.Rel))

	want, ok := written[f.Rel]
	if !ok {
		vr.Mismatches = append(vr.Mismatches, Mismatch{
			Rel: f.Rel, Reason: MismatchNotCopied, WantSize: f.Size,
			Detail: "the copy stopped before this file was written",
		})
		return nil
	}

	info, err := os.Stat(dstPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			vr.Mismatches = append(vr.Mismatches, Mismatch{
				Rel: f.Rel, Reason: MismatchMissing, WantSize: want.Bytes, WantSHA256: want.SHA256,
				Detail: "the file is not on the drive",
			})
			return nil
		}
		// An unreadable destination is a drive problem, not a bad file: report
		// it as the drive problem it is.
		return classify(sideDest, "read back", dstPath, err)
	}

	vr.FilesChecked++
	if info.Size() != want.Bytes {
		vr.Mismatches = append(vr.Mismatches, Mismatch{
			Rel: f.Rel, Reason: MismatchSize, WantSize: want.Bytes, GotSize: info.Size(), WantSHA256: want.SHA256,
			Detail: fmt.Sprintf("%s was written but %s is on the drive", humanBytes(want.Bytes), humanBytes(info.Size())),
		})
		return nil
	}

	sum, n, err := cp.hashFile(ctx, dstPath)
	if err != nil {
		if IsCancelled(err) || IsDriveRemoved(err) {
			return err
		}
		vr.Mismatches = append(vr.Mismatches, Mismatch{
			Rel: f.Rel, Reason: MismatchUnreadable, WantSize: want.Bytes, WantSHA256: want.SHA256,
			Detail: "the file on the drive could not be read back",
		})
		return nil
	}
	vr.BytesChecked += n
	if sum != want.SHA256 {
		vr.Mismatches = append(vr.Mismatches, Mismatch{
			Rel: f.Rel, Reason: MismatchContent, WantSize: want.Bytes, GotSize: n,
			WantSHA256: want.SHA256, GotSHA256: sum,
			Detail: "the file on the drive is the right length but its contents differ",
		})
	}
	return nil
}

func verifyError(p *Plan, vr *VerifyReport) error {
	first := vr.Mismatches[0]
	var summary string
	if len(vr.Mismatches) == 1 {
		summary = fmt.Sprintf("The copy on the drive does not match the bundle: %s did not survive the copy (%s).", first.Rel, first.Detail)
	} else {
		summary = fmt.Sprintf("The copy on the drive does not match the bundle: %d files did not survive the copy, starting with %s (%s).",
			len(vr.Mismatches), first.Rel, first.Detail)
	}
	return &ExportError{
		Kind: ErrKindVerifyFailed, Op: "verify", Path: p.DestDir,
		Summary: summary,
		Hint:    "Do not take this drive to the offline machine. Export again, ideally to a different drive — a drive that corrupts a copy usually does it twice. The folder is still marked incomplete by " + MarkerName + ".",
	}
}

// Export runs all three stages and returns one Result. It is the call the app
// layer makes for the ordinary case.
//
// The Result is returned even when the error is non-nil, so a summary screen
// can show how far the export got and exactly which file stopped it. Check
// Result.Complete, not the absence of an error, before telling an operator the
// drive is ready to carry across the air gap.
func (e *Exporter) Export(ctx context.Context, req Request) (*Result, error) {
	res := &Result{Started: e.opts.now()}
	finish := func(err error) (*Result, error) {
		res.Finished = e.opts.now()
		res.Duration = res.Finished.Sub(res.Started)
		res.Summary = summarize(res, err)
		return res, err
	}

	plan, err := e.Plan(ctx, req)
	res.Plan = plan
	if err != nil {
		return finish(err)
	}

	run, err := e.Run(ctx, plan)
	res.Copy = run
	if err != nil {
		return finish(err)
	}

	if e.opts.SkipVerify {
		// The copy finished, but Complete stays false: an unchecked drive is
		// not a trustworthy drive, and the caller asked for that trade.
		res.Verification = &VerifyReport{Method: skipMethod, OK: false}
		return finish(nil)
	}

	vr, err := e.Verify(ctx, plan, run)
	res.Verification = vr
	if err != nil {
		return finish(err)
	}
	res.Complete = vr.OK && vr.MarkerRemoved
	return finish(nil)
}

// summarize writes the one-sentence account a summary screen leads with.
func summarize(res *Result, err error) string {
	switch {
	case err != nil && res.Copy != nil:
		return fmt.Sprintf("The export stopped after %d of %d files (%s). %s",
			res.Copy.FilesCopied, totalFiles(res), humanBytes(res.Copy.BytesCopied), err.Error())
	case err != nil:
		return err.Error()
	case res.Verification != nil && res.Verification.OK:
		return fmt.Sprintf("Copied %d files (%s) to %s in %s and verified every byte with SHA-256.",
			res.Copy.FilesCopied, humanBytes(res.Copy.BytesCopied), res.Copy.DestDir, res.Duration.Round(time.Second))
	default:
		return fmt.Sprintf("Copied %d files (%s) to %s in %s. Verification was skipped, so the copy has not been checked.",
			res.Copy.FilesCopied, humanBytes(res.Copy.BytesCopied), res.Copy.DestDir, res.Duration.Round(time.Second))
	}
}

func totalFiles(res *Result) int {
	if res.Plan == nil {
		return 0
	}
	return res.Plan.TotalFiles
}

// markerPath is where an export's incomplete marker lives.
func markerPath(destDir string) string { return filepath.Join(destDir, MarkerName) }

// IsIncomplete reports whether destDir holds the marker an export leaves behind
// when it did not finish and verify. A caller must treat a true here as "the
// bundle on this drive is not usable", whatever else the folder looks like.
func IsIncomplete(destDir string) (bool, error) {
	_, err := os.Stat(markerPath(destDir))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, classify(sideDest, "read", markerPath(destDir), err)
	}
}

func markerExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// markerDoc is the JSON an export leaves in the destination while it works. It
// leads with a sentence, because the person most likely to read it is an
// operator staring at a drive wondering whether it is safe to use.
type markerDoc struct {
	Warning    string    `json:"warning"`
	Tool       string    `json:"tool"`
	SourceDir  string    `json:"sourceDir"`
	Started    time.Time `json:"started"`
	TotalFiles int       `json:"totalFiles"`
	TotalBytes int64     `json:"totalBytes"`
}

func writeMarker(p *Plan) error {
	doc := markerDoc{
		Warning:    "This folder is an INCOMPLETE bundle export. Do not install from it. This file is removed only after every file has been copied, flushed to the drive and verified with SHA-256.",
		Tool:       "debark-gui",
		SourceDir:  p.SourceDir,
		Started:    time.Now().UTC(),
		TotalFiles: p.TotalFiles,
		TotalBytes: p.TotalBytes,
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return classify(sideDest, "create", p.MarkerPath, err)
	}
	body = append(body, '\n')

	f, err := os.OpenFile(p.MarkerPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return classify(sideDest, "create", p.MarkerPath, err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return classify(sideDest, "write", p.MarkerPath, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return classify(sideDest, "flush", p.MarkerPath, err)
	}
	if err := f.Close(); err != nil {
		return classify(sideDest, "finish writing", p.MarkerPath, err)
	}
	// Best effort: the marker's own directory entry should reach the device
	// before the copy starts, but not every platform allows this.
	_ = syncDir(p.DestDir)
	return nil
}
