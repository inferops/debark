package export

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MarkerName is the file an export writes into the destination directory
// before it copies its first byte and removes only after verification has
// passed. Its presence means: whatever is in this folder is NOT a complete,
// checked bundle. It is the single most important thing this package does — an
// interrupted copy that looks finished is the failure mode that hurts an
// air-gapped operator, because they discover it on the side where they cannot
// fix it.
const MarkerName = ".debark-export-incomplete"

const (
	// defaultBufferSize is the size of the copy buffer. It is large enough to
	// amortise the per-operation cost of a USB mass-storage device (where each
	// syscall crosses a slow bus) and small enough that the cancellation check
	// at the top of every iteration runs roughly 30 times a second on a
	// 30 MB/s stick.
	defaultBufferSize = 1 << 20

	// defaultProgressInterval is the minimum wall-clock gap between two
	// progress callbacks. See reporter for the reasoning.
	defaultProgressInterval = 100 * time.Millisecond

	// allocationUnit is the block size the free-space estimate assumes the
	// destination filesystem rounds every file up to. 32 KiB is the FAT32
	// cluster size for the 16-32 GB volumes this app is most often pointed at,
	// and is a conservative over-estimate for ext4 (4 KiB) and exFAT.
	allocationUnit = 32 << 10

	// minMargin and maxMargin clamp the free-space safety margin.
	minMargin = 64 << 20
	maxMargin = 512 << 20

	// fat32MaxFileSize is the largest file FAT32 can hold. Files above it get a
	// plan warning, because the operator's stick is very often FAT32 and the
	// failure would otherwise arrive hours into a copy.
	fat32MaxFileSize = int64(4)<<30 - 1
)

// ErrorKind classifies an export failure so the UI can pick an icon, a colour
// and a recovery action without parsing prose.
type ErrorKind string

// The kinds an export can fail with. Every one of them has a sentence in
// describe.
const (
	ErrKindSourceMissing ErrorKind = "source_missing"
	ErrKindEmptySource   ErrorKind = "empty_source"
	ErrKindNotDirectory  ErrorKind = "not_directory"
	ErrKindOverlap       ErrorKind = "overlap"
	ErrKindPermission    ErrorKind = "permission"
	ErrKindReadOnly      ErrorKind = "read_only"
	ErrKindDiskFull      ErrorKind = "disk_full"
	ErrKindNotEnoughRoom ErrorKind = "not_enough_room"
	ErrKindDriveRemoved  ErrorKind = "drive_removed"
	ErrKindNameRejected  ErrorKind = "name_rejected"
	ErrKindFileTooLarge  ErrorKind = "file_too_large"
	ErrKindUnsupported   ErrorKind = "unsupported_entry"
	ErrKindSourceChanged ErrorKind = "source_changed"
	ErrKindCancelled     ErrorKind = "cancelled"
	ErrKindVerifyFailed  ErrorKind = "verification_failed"
	ErrKindIO            ErrorKind = "io"

	// errKindNotExist never escapes this package; classify turns it into a
	// side-appropriate public kind.
	errKindNotExist ErrorKind = "not_exist"
)

// ExportError is the only error type this package returns. Error renders an
// actionable sentence — never a bare errno, never a bare exit code — while Err
// keeps the operating system's own error for a details drawer.
type ExportError struct {
	// Kind lets the UI branch without reading the text.
	Kind ErrorKind
	// Op is what was being attempted, in plain words ("write", "read back").
	Op string
	// Path is the file or folder involved, if any.
	Path string
	// Summary states what went wrong, as one complete sentence.
	Summary string
	// Hint states what the operator should do about it, as one complete
	// sentence. It is empty only when there is genuinely nothing to advise.
	Hint string
	// Err is the underlying error. Never shown as the primary message.
	Err error
}

func (e *ExportError) Error() string {
	if e.Hint == "" {
		return e.Summary
	}
	return e.Summary + " " + e.Hint
}

func (e *ExportError) Unwrap() error { return e.Err }

// Detail is the technical line for a "show details" drawer: the operation, the
// path and the operating system's own words. Never put this in front of an
// operator as the headline.
func (e *ExportError) Detail() string {
	var parts []string
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	if e.Path != "" {
		parts = append(parts, e.Path)
	}
	line := strings.Join(parts, " ")
	if e.Err != nil {
		if line != "" {
			line += ": "
		}
		line += e.Err.Error()
	}
	return line
}

// KindOf reports the ErrorKind carried by err, or an empty ErrorKind if err did
// not come from this package.
func KindOf(err error) ErrorKind {
	var e *ExportError
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

// IsCancelled reports whether err is the result of cancelling the context.
func IsCancelled(err error) bool { return KindOf(err) == ErrKindCancelled }

// IsDriveRemoved reports whether err means the destination stopped responding —
// almost always because the operator pulled the drive.
func IsDriveRemoved(err error) bool { return KindOf(err) == ErrKindDriveRemoved }

// IsOutOfSpace reports whether err means the destination could not hold the
// bundle, either predicted before the copy or hit during it.
func IsOutOfSpace(err error) bool {
	k := KindOf(err)
	return k == ErrKindDiskFull || k == ErrKindNotEnoughRoom
}

// side says which filesystem an operating system error came from. The same
// errno means different things on each: ENOENT on the source is a missing
// bundle, ENOENT on the destination mid-copy is an unmounted drive.
type side int

const (
	sideSource side = iota
	sideDest
)

// classify turns an operating system error into an ExportError whose Summary an
// operator can act on. Errors that are already ExportErrors pass through
// unchanged, so a message is never wrapped twice.
func classify(s side, op, path string, err error) error {
	if err == nil {
		return nil
	}
	var already *ExportError
	if errors.As(err, &already) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return &ExportError{
			Kind: ErrKindCancelled, Op: op, Path: path, Err: err,
			Summary: "The export was cancelled before it finished.",
			Hint:    cancelHint,
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &ExportError{
			Kind: ErrKindCancelled, Op: op, Path: path, Err: err,
			Summary: "The export ran out of time and was stopped before it finished.",
			Hint:    cancelHint,
		}
	}

	kind := kindFromOS(s, err)
	summary, hint := describe(kind, s, op, path)
	return &ExportError{Kind: kind, Op: op, Path: path, Summary: summary, Hint: hint, Err: err}
}

// cancelHint states the operator's obligation after an interrupted export. It
// is the documented post-cancel contract in one sentence.
const cancelHint = "The drive now holds a partly copied bundle; the file " + MarkerName +
	" in the destination folder marks it as unusable. Export again to replace it, or delete the folder, before taking the drive to the offline machine."

// kindFromOS maps an errno onto a kind. The POSIX names are what Linux — the
// shipping target — reports; the Windows table below covers developer machines,
// where Go's syscall package gives these POSIX constants invented values that
// real Windows errors never match.
func kindFromOS(s side, err error) ErrorKind {
	switch {
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return ErrKindDiskFull
	case errors.Is(err, syscall.EIO), errors.Is(err, syscall.ENODEV),
		errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.ESTALE):
		return ErrKindDriveRemoved
	case errors.Is(err, syscall.EROFS):
		return ErrKindReadOnly
	case errors.Is(err, syscall.EFBIG):
		return ErrKindFileTooLarge
	case errors.Is(err, syscall.ENAMETOOLONG):
		return ErrKindNameRejected
	case s == sideDest && errors.Is(err, syscall.EINVAL):
		// vfat rejects a name it cannot represent with EINVAL. On the source
		// side EINVAL means something else entirely, so only the destination
		// gets this reading.
		return ErrKindNameRejected
	case errors.Is(err, fs.ErrPermission):
		return ErrKindPermission
	case errors.Is(err, fs.ErrNotExist):
		return errKindNotExist
	}
	if runtime.GOOS == "windows" {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			if k, ok := windowsErrorKind(uintptr(errno)); ok {
				return k
			}
		}
	}
	return ErrKindIO
}

// windowsErrorKind maps the Windows system error codes an export can
// realistically hit. It is a pure function so it can be tested on any host.
func windowsErrorKind(code uintptr) (ErrorKind, bool) {
	switch code {
	case 5: // ERROR_ACCESS_DENIED
		return ErrKindPermission, true
	case 19: // ERROR_WRITE_PROTECT
		return ErrKindReadOnly, true
	case 21: // ERROR_NOT_READY
		return ErrKindDriveRemoved, true
	case 112: // ERROR_DISK_FULL
		return ErrKindDiskFull, true
	case 123: // ERROR_INVALID_NAME
		return ErrKindNameRejected, true
	case 433: // ERROR_NO_SUCH_DEVICE
		return ErrKindDriveRemoved, true
	case 1006: // ERROR_FILE_INVALID - the volume was changed externally
		return ErrKindDriveRemoved, true
	case 1167: // ERROR_DEVICE_NOT_CONNECTED
		return ErrKindDriveRemoved, true
	}
	return "", false
}

// describe writes the operator-facing sentences for a kind. Every branch here
// is a sentence a non-technical person can act on; none of them contain an
// errno, a numeric code or a Go type name.
func describe(kind ErrorKind, s side, op, path string) (summary, hint string) {
	where := path
	if where == "" {
		where = "the destination"
	}
	switch kind {
	case ErrKindDiskFull:
		return fmt.Sprintf("The drive ran out of space while writing %s.", where),
			"Free up space on the drive, or use a larger one, and export again. What is on the drive now is an incomplete bundle and must not be used."
	case ErrKindDriveRemoved:
		return fmt.Sprintf("The drive stopped responding while writing %s — it looks like it was unplugged.", where),
			"Plug the drive back in, wait for the system to mount it, and export again from the start. Do not use what is on the drive now."
	case ErrKindReadOnly:
		return fmt.Sprintf("The drive is read-only, so %s cannot be written.", where),
			"Turn off the drive's write-protect switch, or remount it with write access, and export again."
	case ErrKindPermission:
		if s == sideSource {
			return fmt.Sprintf("The bundle file %s cannot be read: permission denied.", where),
				"Check that your user account can read the whole bundle folder, then export again."
		}
		return fmt.Sprintf("The destination %s cannot be written: permission denied.", where),
			"Pick a folder your user account owns, or fix that folder's permissions. This app never asks for administrator rights and never touches the drive as a raw device."
	case ErrKindFileTooLarge:
		return fmt.Sprintf("%s is too large for this drive's filesystem — FAT32 cannot store a single file bigger than 4 GiB.", where),
			"Reformat the drive as exFAT or ext4, or use a drive that already is, and export again."
	case ErrKindNameRejected:
		return fmt.Sprintf("The drive's filesystem would not accept the file name %s.", where),
			`FAT and exFAT drives cannot store names containing \ / : * ? " < > | or names ending in a dot or a space. Use a drive formatted ext4 or NTFS, or rebuild the bundle without that file.`
	case errKindNotExist:
		if s == sideSource {
			return fmt.Sprintf("The bundle file %s is no longer there.", where),
				"Something changed the bundle folder while the export was running. Rebuild or re-select the bundle and export again."
		}
		return fmt.Sprintf("The destination folder for %s is no longer there — the drive was probably removed.", where),
			"Plug the drive back in, wait for the system to mount it, and export again from the start."
	case ErrKindSourceChanged:
		return fmt.Sprintf("The bundle file %s changed while it was being copied.", where),
			"Nothing may write to the bundle folder during an export. Make sure the build has finished, then export again."
	default:
		verb := op
		if verb == "" {
			verb = "use"
		}
		return fmt.Sprintf("Could not %s %s.", verb, where),
			"See the details for exactly what the operating system reported."
	}
}

// destFile is the seam every byte written to the destination passes through.
// *os.File satisfies it; tests substitute a writer that fails on demand, which
// is how the drive-removed and disk-full paths are exercised without a drive.
type destFile interface {
	io.Writer
	Sync() error
	Close() error
}

func openDestFile(path string) (destFile, error) {
	// Replace the entry rather than truncating its inode: a previous export
	// may share a hardlink with another bundle or an unrelated file.
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, unsafeDestination(path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	// 0o666 before umask: the bundle is data, not an installed tree, and the
	// FAT/exFAT sticks this app targets have no Unix permission bits at all.
	// O_EXCL refuses a link or file created since the check above.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// reporter throttles progress callbacks.
//
// Cadence: at most one callback every ProgressInterval (100 ms by default),
// plus one forced callback when a phase begins and one when it ends. File
// boundaries deliberately do NOT force a callback.
//
// Why: a per-write callback would fire ~700 times for one 700 MB .deb, and
// thousands of times a second from a fast source, flooding the Wails bridge for
// no visible benefit. A per-file callback is the opposite problem — a 700 MB
// package would freeze the bar for half a minute, which is exactly when an
// operator starts wondering whether it is safe to pull the drive. A fixed 10 Hz
// tick is above the rate at which a progress bar stops looking smooth, is well
// inside one frame's budget, and — unlike either alternative — does not change
// with file size, file count or drive speed. The forced tick at the end
// guarantees the bar lands on 100% with the final counts rather than on
// whatever the last throttled sample happened to be.
type reporter struct {
	fn       func(Progress)
	interval time.Duration
	now      func() time.Time
	started  time.Time
	last     time.Time
	p        Progress
}

func newReporter(o Options, phase Phase, filesTotal int, bytesTotal int64) *reporter {
	r := &reporter{fn: o.Progress, interval: o.ProgressInterval, now: o.now}
	r.started = r.now()
	r.p = Progress{Phase: phase, FilesTotal: filesTotal, BytesTotal: bytesTotal}
	r.emit(true)
	return r
}

func (r *reporter) beginFile(rel string) {
	r.p.File = rel
	r.emit(false)
}

func (r *reporter) advance(n int64) {
	r.p.BytesDone += n
	r.emit(false)
}

func (r *reporter) endFile() {
	r.p.FilesDone++
	r.emit(false)
}

// setPhase moves to a new phase and forces a callback, so the UI can change its
// label the moment the work changes rather than up to a tick later.
func (r *reporter) setPhase(ph Phase) {
	r.p.Phase = ph
	r.p.File = ""
	r.emit(true)
}

func (r *reporter) finish() { r.emit(true) }

func (r *reporter) emit(force bool) {
	if r.fn == nil {
		return
	}
	now := r.now()
	if !force && now.Sub(r.last) < r.interval {
		return
	}
	r.last = now
	p := r.p
	p.Elapsed = now.Sub(r.started)
	r.fn(p)
}

// copier holds the buffer and the reporter for one phase of one export. Nothing
// here starts a goroutine: an export runs entirely on its caller's goroutine, so
// cancelling the context cannot leave anything spinning.
type copier struct {
	opts Options
	rep  *reporter
	buf  []byte
}

func newCopier(o Options, rep *reporter) *copier {
	return &copier{opts: o, rep: rep, buf: make([]byte, o.BufferSize)}
}

// copyFile copies one regular file and returns the SHA-256 of everything it read
// from the source, which is also everything it handed to the destination.
//
// On any failure — including cancellation — the partly written destination file
// is removed before returning, so the destination never holds a truncated file
// under a real name. Removal is best effort: if the drive is gone it cannot
// work, which is why the marker file, not this cleanup, is what makes an
// interrupted destination honest.
func (c *copier) copyFile(ctx context.Context, srcPath, dstPath string, want int64) (sum string, written int64, err error) {
	src, oerr := os.Open(srcPath)
	if oerr != nil {
		return "", 0, classify(sideSource, "read", srcPath, oerr)
	}
	defer src.Close()

	dst, derr := c.opts.openDest(dstPath)
	if derr != nil {
		return "", 0, classify(sideDest, "create", dstPath, derr)
	}

	h := sha256.New()
	var failure error

	for {
		if cerr := ctx.Err(); cerr != nil {
			failure = classify(sideDest, "copy", dstPath, cerr)
			break
		}
		n, rerr := src.Read(c.buf)
		if n > 0 {
			h.Write(c.buf[:n])
			w, werr := dst.Write(c.buf[:n])
			if w > 0 {
				written += int64(w)
				c.rep.advance(int64(w))
			}
			if werr != nil {
				failure = classify(sideDest, "write", dstPath, werr)
				break
			}
			if w != n {
				failure = classify(sideDest, "write", dstPath, io.ErrShortWrite)
				break
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			failure = classify(sideSource, "read", srcPath, rerr)
			break
		}
	}

	if failure == nil && written != want {
		failure = &ExportError{
			Kind: ErrKindSourceChanged, Op: "copy", Path: srcPath,
			Summary: fmt.Sprintf("The bundle file %s changed size while it was being copied: %s was expected but %s arrived.",
				srcPath, humanBytes(want), humanBytes(written)),
			Hint: "Nothing may write to the bundle folder during an export. Make sure the build has finished, then export again.",
		}
	}
	// fsync before the file is declared done. A copy still sitting in the page
	// cache when the operator pulls the drive is not a copy.
	if failure == nil {
		if serr := dst.Sync(); serr != nil {
			failure = classify(sideDest, "flush", dstPath, serr)
		}
	}
	if cerr := dst.Close(); cerr != nil && failure == nil {
		failure = classify(sideDest, "finish writing", dstPath, cerr)
	}

	if failure != nil {
		_ = os.Remove(dstPath)
		return "", written, failure
	}
	return hex.EncodeToString(h.Sum(nil)), written, nil
}

// hashFile re-reads a file from the destination and returns its SHA-256.
func (c *copier) hashFile(ctx context.Context, path string) (sum string, read int64, err error) {
	f, oerr := os.Open(path)
	if oerr != nil {
		return "", 0, classify(sideDest, "read back", path, oerr)
	}
	defer f.Close()

	h := sha256.New()
	for {
		if cerr := ctx.Err(); cerr != nil {
			return "", read, classify(sideDest, "read back", path, cerr)
		}
		n, rerr := f.Read(c.buf)
		if n > 0 {
			h.Write(c.buf[:n])
			read += int64(n)
			c.rep.advance(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", read, classify(sideDest, "read back", path, rerr)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), read, nil
}

// syncDir flushes a directory's own entries. Creating a file and fsyncing it is
// not enough: the name can still be lost if the directory entry never reached
// the device.
//
// Not every platform or filesystem supports fsync on a directory handle
// (Windows refuses outright), so callers treat a failure as a note rather than
// a failure — unless it classifies as a removed drive or a full disk, which
// mean something real.
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// roundUp rounds n up to a whole number of units. Used to charge each file the
// space a block-allocating filesystem will actually spend on it.
func roundUp(n, unit int64) int64 {
	if unit <= 0 || n <= 0 {
		return n
	}
	if r := n % unit; r != 0 {
		return n + (unit - r)
	}
	return n
}

// marginFor is the free space an export refuses to consume: 2% of the payload,
// clamped to between 64 MB and 512 MB. Filesystems need slack for their own
// metadata, and the free-space number was measured before the copy started —
// another process may be writing to the same drive while this one runs.
func marginFor(payload int64) int64 {
	m := payload / 50
	if m < minMargin {
		m = minMargin
	}
	if m > maxMargin {
		m = maxMargin
	}
	return m
}

// humanByteUnits and humanByteBase are the one byte convention this
// application uses. See humanBytes.
var humanByteUnits = [...]string{"KiB", "MiB", "GiB", "TiB", "PiB"}

const humanByteBase = 1024

// humanBytes renders a byte count in binary units: 1.4 MiB, never 1.5 MB.
//
// This package used to be the one place that rendered decimal units, on the
// reasoning that a stick sold as 64 GB should not read as 59.6 GiB and prompt
// a support question about the missing capacity. That is a fair point about
// the string "GB", which genuinely means two different numbers depending on
// who wrote it, and it does not survive the suffix being spelled "GiB", which
// is exact and cannot be misread.
//
// What did not survive was the result. Everything else in this application
// counts in binary — apt and dpkg report Installed-Size in KiB, `df -h` and
// `du -h` are what an operator checks the same drive with, internal/readiness
// reports free space this way, and the frontend's shared formatter in
// frontend/src/shell/states.js does too — so the readiness screen and the
// export screen were rendering the very same quantity, the free space on the
// chosen destination, in two different units. One convention beats the better
// argument for either of them.
//
// The output matches states.js's formatBytes exactly, units and rounding
// alike, so a sentence produced here and a number the UI renders beside it
// agree to the last digit. The single deliberate difference is that a negative
// count reads "an unknown amount" rather than empty, because these go into
// sentences.
func humanBytes(n int64) string {
	if n < 0 {
		return "an unknown amount"
	}
	if n < humanByteBase {
		return strconv.FormatInt(n, 10) + " B"
	}
	v, exp := float64(n), 0
	for v >= humanByteBase && exp < len(humanByteUnits) {
		v /= humanByteBase
		exp++
	}
	if v >= 100 {
		return strconv.FormatFloat(v, 'f', 0, 64) + " " + humanByteUnits[exp-1]
	}
	return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + " " + humanByteUnits[exp-1]
}

// reservedNameChars are rejected by FAT, exFAT and NTFS alike.
const reservedNameChars = `\/:*?"<>|`

// nameProblem reports why a path element cannot be stored on a FAT, exFAT or
// NTFS volume, or "" if it can. It is advisory: the destination filesystem is
// not knowable from a mount path alone, so this produces a plan warning rather
// than a refusal, and the real write error is classified separately.
func nameProblem(elem string) string {
	for _, r := range elem {
		if r < 0x20 {
			return "it contains a control character"
		}
		if strings.ContainsRune(reservedNameChars, r) {
			return fmt.Sprintf("it contains %q", r)
		}
	}
	if strings.HasSuffix(elem, ".") {
		return "it ends with a dot"
	}
	if strings.HasSuffix(elem, " ") {
		return "it ends with a space"
	}
	return ""
}
