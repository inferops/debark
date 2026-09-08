package export

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// test doubles
// ---------------------------------------------------------------------------

// fakeClock drives the progress reporter deterministically, so the cadence can
// be asserted without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

// faultyFile is a destFile that writes to a real file until a byte budget runs
// out and then fails with a chosen operating system error. It is how the
// drive-removed and drive-full paths get tested without a drive.
type faultyFile struct {
	inner   destFile
	seen    *int64
	failAt  int64
	failErr error
	syncErr error
}

func (f *faultyFile) Write(p []byte) (int, error) {
	if *f.seen >= f.failAt {
		return 0, f.failErr
	}
	n, err := f.inner.Write(p)
	*f.seen += int64(n)
	if err != nil {
		return n, err
	}
	if *f.seen >= f.failAt {
		return n, f.failErr
	}
	return n, nil
}

func (f *faultyFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.inner.Sync()
}

func (f *faultyFile) Close() error { return f.inner.Close() }

// failingDestAfter returns an openDest seam that fails, across the whole run,
// once failAt bytes have been written in total.
func failingDestAfter(failAt int64, failErr error) func(string) (destFile, error) {
	var seen int64
	return func(path string) (destFile, error) {
		inner, err := openDestFile(path)
		if err != nil {
			return nil, err
		}
		return &faultyFile{inner: inner, seen: &seen, failAt: failAt, failErr: failErr}, nil
	}
}

// ---------------------------------------------------------------------------
// assertions
// ---------------------------------------------------------------------------

// forbiddenInMessages are the shapes of a raw operating system error. None of
// them may appear in a message shown to an operator: "every error path renders
// an actionable message" is a definition-of-done item for this project.
var forbiddenInMessages = []string{
	"errno", "syscall", "EIO", "ENOSPC", "EROFS", "EACCES", "EFBIG",
	"input/output error", "no space left on device", "0x",
}

func assertSentence(t *testing.T, what, msg string) {
	t.Helper()
	if strings.TrimSpace(msg) == "" {
		t.Fatalf("%s: message is empty", what)
	}
	if msg != strings.TrimSpace(msg) {
		t.Errorf("%s: message has leading or trailing space: %q", what, msg)
	}
	if !strings.HasSuffix(msg, ".") {
		t.Errorf("%s: message is not a sentence (no full stop): %q", what, msg)
	}
	if len(strings.Fields(msg)) < 4 {
		t.Errorf("%s: message is too terse to be a sentence: %q", what, msg)
	}
	for _, bad := range forbiddenInMessages {
		if strings.Contains(msg, bad) {
			t.Errorf("%s: message leaks the raw OS error (%q): %q", what, bad, msg)
		}
	}
}

func exportErr(t *testing.T, err error) *ExportError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var e *ExportError
	if !errors.As(err, &e) {
		t.Fatalf("expected an *ExportError, got %T: %v", err, err)
	}
	return e
}

func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// error classification
// ---------------------------------------------------------------------------

func TestClassifyRendersActionableSentences(t *testing.T) {
	cases := []struct {
		name     string
		side     side
		op       string
		err      error
		wantKind ErrorKind
		contains string
	}{
		{"disk full", sideDest, "write", syscall.ENOSPC, ErrKindDiskFull, "ran out of space"},
		{"quota", sideDest, "write", syscall.EDQUOT, ErrKindDiskFull, "ran out of space"},
		{"drive yanked, EIO", sideDest, "write", syscall.EIO, ErrKindDriveRemoved, "unplugged"},
		{"drive yanked, ENODEV", sideDest, "write", syscall.ENODEV, ErrKindDriveRemoved, "unplugged"},
		{"drive yanked, ENXIO", sideDest, "write", syscall.ENXIO, ErrKindDriveRemoved, "unplugged"},
		{"stale handle", sideDest, "write", syscall.ESTALE, ErrKindDriveRemoved, "unplugged"},
		{"read-only mount", sideDest, "write", syscall.EROFS, ErrKindReadOnly, "read-only"},
		{"no permission on dest", sideDest, "create", syscall.EACCES, ErrKindPermission, "permission denied"},
		{"no permission on source", sideSource, "read", syscall.EACCES, ErrKindPermission, "permission denied"},
		{"file too big for FAT32", sideDest, "write", syscall.EFBIG, ErrKindFileTooLarge, "4 GiB"},
		{"name too long", sideDest, "create", syscall.ENAMETOOLONG, ErrKindNameRejected, "would not accept"},
		{"name rejected by vfat", sideDest, "create", syscall.EINVAL, ErrKindNameRejected, "would not accept"},
		{"dest vanished", sideDest, "write", syscall.ENOENT, errKindNotExist, "no longer there"},
		{"source vanished", sideSource, "read", syscall.ENOENT, errKindNotExist, "no longer there"},
		{"cancelled", sideDest, "copy", context.Canceled, ErrKindCancelled, "cancelled"},
		{"timed out", sideDest, "copy", context.DeadlineExceeded, ErrKindCancelled, "ran out of time"},
		{"anything else", sideDest, "write", errors.New("something odd"), ErrKindIO, "Could not write"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := &fs.PathError{Op: "write", Path: "/media/usb/pool/x.deb", Err: tc.err}
			got := classify(tc.side, tc.op, "/media/usb/pool/x.deb", wrapped)
			e := exportErr(t, got)

			if e.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", e.Kind, tc.wantKind)
			}
			if !strings.Contains(e.Summary, tc.contains) {
				t.Errorf("summary %q does not mention %q", e.Summary, tc.contains)
			}
			assertSentence(t, "summary", e.Summary)
			assertSentence(t, "hint", e.Hint)
			assertSentence(t, "Error()", e.Error())

			// The underlying error survives for the details drawer.
			if !errors.Is(got, tc.err) {
				t.Errorf("underlying error was lost: %v", got)
			}
			if !strings.Contains(e.Detail(), wrapped.Error()) {
				t.Errorf("Detail() %q does not carry the OS error %q", e.Detail(), wrapped.Error())
			}
		})
	}
}

func TestClassifyDoesNotWrapTwice(t *testing.T) {
	first := classify(sideDest, "write", "/media/usb/a.deb", syscall.ENOSPC)
	second := classify(sideDest, "flush", "/media/usb", first)
	if first != second {
		t.Fatalf("an already-classified error was wrapped again:\n first:  %v\n second: %v", first, second)
	}
}

func TestErrorPredicates(t *testing.T) {
	removed := classify(sideDest, "write", "/media/usb/a.deb", syscall.EIO)
	full := classify(sideDest, "write", "/media/usb/a.deb", syscall.ENOSPC)
	cancelled := classify(sideDest, "copy", "/media/usb", context.Canceled)

	if !IsDriveRemoved(removed) || IsDriveRemoved(full) {
		t.Error("IsDriveRemoved misclassified")
	}
	if !IsOutOfSpace(full) || IsOutOfSpace(removed) {
		t.Error("IsOutOfSpace misclassified")
	}
	if !IsCancelled(cancelled) || IsCancelled(full) {
		t.Error("IsCancelled misclassified")
	}
	if KindOf(errors.New("plain")) != "" {
		t.Error("KindOf claimed a foreign error")
	}
	if KindOf(removed) != ErrKindDriveRemoved {
		t.Errorf("KindOf = %q", KindOf(removed))
	}
}

func TestEveryKindHasSentences(t *testing.T) {
	kinds := []ErrorKind{
		ErrKindSourceMissing, ErrKindEmptySource, ErrKindNotDirectory, ErrKindOverlap,
		ErrKindPermission, ErrKindReadOnly, ErrKindDiskFull, ErrKindNotEnoughRoom,
		ErrKindDriveRemoved, ErrKindNameRejected, ErrKindFileTooLarge, ErrKindUnsupported,
		ErrKindSourceChanged, ErrKindCancelled, ErrKindVerifyFailed, ErrKindIO,
		errKindNotExist,
	}
	for _, k := range kinds {
		for _, s := range []side{sideSource, sideDest} {
			summary, hint := describe(k, s, "write", "/media/usb/pool/main/x.deb")
			assertSentence(t, fmt.Sprintf("summary for %s", k), summary)
			assertSentence(t, fmt.Sprintf("hint for %s", k), hint)
		}
	}
}

func TestWindowsErrorKind(t *testing.T) {
	cases := map[uintptr]ErrorKind{
		5:    ErrKindPermission,
		19:   ErrKindReadOnly,
		21:   ErrKindDriveRemoved,
		112:  ErrKindDiskFull,
		123:  ErrKindNameRejected,
		433:  ErrKindDriveRemoved,
		1006: ErrKindDriveRemoved,
		1167: ErrKindDriveRemoved,
	}
	for code, want := range cases {
		got, ok := windowsErrorKind(code)
		if !ok || got != want {
			t.Errorf("windowsErrorKind(%d) = %q, %v; want %q, true", code, got, ok, want)
		}
	}
	if _, ok := windowsErrorKind(999999); ok {
		t.Error("windowsErrorKind claimed an unknown code")
	}
}

// ---------------------------------------------------------------------------
// progress cadence
// ---------------------------------------------------------------------------

func TestProgressCadenceIsThrottledNotPerWrite(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	var samples []Progress
	opts := Options{
		Progress:         func(p Progress) { samples = append(samples, p) },
		ProgressInterval: 100 * time.Millisecond,
		now:              clk.now,
	}.normalized()

	r := newReporter(opts, PhaseCopy, 3, 3000)
	if len(samples) != 1 {
		t.Fatalf("expected one forced sample when the phase starts, got %d", len(samples))
	}

	// A thousand writes inside one interval must not produce a thousand
	// callbacks: that is the whole point of the cadence.
	for i := 0; i < 1000; i++ {
		r.advance(1)
	}
	if len(samples) != 1 {
		t.Fatalf("1000 writes inside one tick produced %d callbacks, want 1", len(samples))
	}

	clk.add(150 * time.Millisecond)
	r.advance(1)
	if len(samples) != 2 {
		t.Fatalf("a write after the interval produced %d callbacks, want 2", len(samples))
	}

	// File boundaries do not force a callback either.
	r.beginFile("pool/main/a.deb")
	r.endFile()
	if len(samples) != 2 {
		t.Fatalf("file boundaries forced a callback: %d samples", len(samples))
	}

	// ...but the end of the phase always does, so the bar lands on the real
	// totals rather than the last throttled sample.
	r.finish()
	if len(samples) != 3 {
		t.Fatalf("the end of a phase did not force a callback: %d samples", len(samples))
	}
	last := samples[len(samples)-1]
	if last.BytesDone != 1001 || last.FilesDone != 1 || last.FilesTotal != 3 || last.BytesTotal != 3000 {
		t.Errorf("final sample is wrong: %+v", last)
	}
	if last.Elapsed != 150*time.Millisecond {
		t.Errorf("Elapsed = %v, want 150ms", last.Elapsed)
	}
}

func TestSetPhaseForcesACallback(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	var samples []Progress
	opts := Options{
		Progress:         func(p Progress) { samples = append(samples, p) },
		ProgressInterval: time.Hour,
		now:              clk.now,
	}.normalized()

	r := newReporter(opts, PhaseCopy, 1, 10)
	r.setPhase(PhaseVerify)
	if len(samples) != 2 {
		t.Fatalf("setPhase did not force a callback: %d samples", len(samples))
	}
	if samples[1].Phase != PhaseVerify {
		t.Errorf("phase = %q, want %q", samples[1].Phase, PhaseVerify)
	}
}

func TestReporterToleratesNoCallback(t *testing.T) {
	r := newReporter(Options{}.normalized(), PhaseCopy, 1, 1)
	r.beginFile("x")
	r.advance(1)
	r.endFile()
	r.finish() // must not panic
}

// ---------------------------------------------------------------------------
// the copy engine
// ---------------------------------------------------------------------------

func TestCopyFileHashesWhatItWrote(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	body := make([]byte, 300*1024)
	for i := range body {
		body[i] = byte(i * 7)
	}
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}

	opts := Options{BufferSize: 4096}.normalized()
	cp := newCopier(opts, newReporter(opts, PhaseCopy, 1, int64(len(body))))
	sum, n, err := cp.copyFile(context.Background(), src, dst, int64(len(body)))
	if err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("copied %d bytes, want %d", n, len(body))
	}
	if want := sha256OfFile(t, src); sum != want {
		t.Errorf("digest = %s, want %s", sum, want)
	}
	if got := sha256OfFile(t, dst); got != sum {
		t.Errorf("destination digest = %s, want %s", got, sum)
	}
}

func TestCopyFileCopiesAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty")
	dst := filepath.Join(dir, "empty.copy")
	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	opts := Options{}.normalized()
	cp := newCopier(opts, newReporter(opts, PhaseCopy, 1, 0))
	sum, n, err := cp.copyFile(context.Background(), src, dst, 0)
	if err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if n != 0 {
		t.Errorf("copied %d bytes from an empty file", n)
	}
	// SHA-256 of the empty string; a copier that skipped empty files would not
	// produce this.
	const emptySum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if sum != emptySum {
		t.Errorf("digest = %s, want the empty-string digest", sum)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("the empty file was not created: %v", err)
	}
}

func TestCopyFileRemovesThePartialFileWhenTheDriveFails(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "big.deb")
	dst := filepath.Join(dir, "big.deb.copy")
	body := make([]byte, 64*1024)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}

	opts := Options{
		BufferSize: 4096,
		openDest:   failingDestAfter(8*1024, syscall.EIO),
	}.normalized()
	cp := newCopier(opts, newReporter(opts, PhaseCopy, 1, int64(len(body))))

	_, _, err := cp.copyFile(context.Background(), src, dst, int64(len(body)))
	if !IsDriveRemoved(err) {
		t.Fatalf("error is not a drive removal: %v", err)
	}
	assertSentence(t, "Error()", err.Error())
	if _, statErr := os.Stat(dst); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the truncated destination file was left behind: %v", statErr)
	}
}

func TestCopyFileMapsAFailedFsync(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.deb")
	dst := filepath.Join(dir, "a.deb.copy")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := Options{
		openDest: func(path string) (destFile, error) {
			inner, err := openDestFile(path)
			if err != nil {
				return nil, err
			}
			var seen int64
			return &faultyFile{inner: inner, seen: &seen, failAt: 1 << 40, syncErr: syscall.ENOSPC}, nil
		},
	}.normalized()
	cp := newCopier(opts, newReporter(opts, PhaseCopy, 1, 5))

	_, _, err := cp.copyFile(context.Background(), src, dst, 5)
	if !IsOutOfSpace(err) {
		t.Fatalf("a failed fsync was not reported as a full drive: %v", err)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, fs.ErrNotExist) {
		t.Error("an unflushed file was left behind")
	}
}

func TestCopyFileNoticesTheSourceChangingSize(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.deb")
	dst := filepath.Join(dir, "a.deb.copy")
	if err := os.WriteFile(src, []byte("only five"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := Options{}.normalized()
	cp := newCopier(opts, newReporter(opts, PhaseCopy, 1, 100))
	// The plan said 100 bytes; the file on disk is 9. That is what a build
	// still writing into the bundle looks like.
	_, _, err := cp.copyFile(context.Background(), src, dst, 100)
	e := exportErr(t, err)
	if e.Kind != ErrKindSourceChanged {
		t.Fatalf("kind = %q, want %q", e.Kind, ErrKindSourceChanged)
	}
	assertSentence(t, "summary", e.Summary)
	if _, statErr := os.Stat(dst); !errors.Is(statErr, fs.ErrNotExist) {
		t.Error("the short copy was left behind")
	}
}

func TestCopyFileStopsPromptlyOnCancel(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.deb")
	dst := filepath.Join(dir, "a.deb.copy")
	if err := os.WriteFile(src, make([]byte, 256*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	// A clock that advances one tick per reading makes every write produce a
	// progress callback, so the cancel lands at a known byte offset.
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	const bufSize = 4096
	var cancelledAt int64
	opts := Options{
		BufferSize:       bufSize,
		ProgressInterval: time.Millisecond,
		now:              func() time.Time { clk.add(time.Millisecond); return clk.now() },
		Progress: func(p Progress) {
			if cancelledAt == 0 && p.BytesDone >= 16*1024 {
				cancelledAt = p.BytesDone
				cancel()
			}
		},
	}.normalized()
	cp := newCopier(opts, newReporter(opts, PhaseCopy, 1, 256*1024))

	_, n, err := cp.copyFile(ctx, src, dst, 256*1024)
	if !IsCancelled(err) {
		t.Fatalf("error is not a cancellation: %v", err)
	}
	if cancelledAt == 0 {
		t.Fatal("the test never cancelled")
	}
	if n >= 256*1024 {
		t.Errorf("the copy ran to completion after cancel (%d bytes)", n)
	}
	// Promptness: the copy must stop within a buffer or two of the cancel, not
	// keep going to the end of a 700 MB package.
	if n-cancelledAt > 2*bufSize {
		t.Errorf("cancel took %d further bytes to take effect, more than two %d-byte buffers", n-cancelledAt, bufSize)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, fs.ErrNotExist) {
		t.Error("the partly copied file was left behind after cancel")
	}
}

func TestHashFileMatchesTheEngineDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.deb")
	body := []byte("some bundle bytes")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{BufferSize: 4}.normalized()
	cp := newCopier(opts, newReporter(opts, PhaseVerify, 1, int64(len(body))))

	sum, n, err := cp.hashFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(body)) {
		t.Errorf("read %d bytes, want %d", n, len(body))
	}
	if want := sha256OfFile(t, path); sum != want {
		t.Errorf("digest = %s, want %s", sum, want)
	}
}

// ---------------------------------------------------------------------------
// small pure helpers
// ---------------------------------------------------------------------------

// TestHumanBytes pins the one byte convention this application uses. The
// expectations below are also what frontend/src/shell/states.js's formatBytes
// produces for the same inputs, which is the point: an engine sentence and the
// number the UI renders beside it must agree to the last digit.
func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1023, "1023 B"},
		{1024, "1 KiB"},
		{1500, "1.5 KiB"},
		{15000, "14.6 KiB"},
		{150000, "146 KiB"},
		{4_200_000_000, "3.9 GiB"},
		{-1, "an unknown amount"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRoundUp(t *testing.T) {
	cases := []struct{ n, unit, want int64 }{
		{0, 32768, 0},
		{1, 32768, 32768},
		{32768, 32768, 32768},
		{32769, 32768, 65536},
		{100, 0, 100},
	}
	for _, tc := range cases {
		if got := roundUp(tc.n, tc.unit); got != tc.want {
			t.Errorf("roundUp(%d, %d) = %d, want %d", tc.n, tc.unit, got, tc.want)
		}
	}
}

func TestMarginIsClamped(t *testing.T) {
	if got := marginFor(1000); got != minMargin {
		t.Errorf("a tiny bundle got a %d byte margin, want the %d floor", got, minMargin)
	}
	if got := marginFor(100 << 30); got != maxMargin {
		t.Errorf("a huge bundle got a %d byte margin, want the %d ceiling", got, maxMargin)
	}
	if got := marginFor(10 << 30); got != (10<<30)/50 {
		t.Errorf("a mid-sized bundle got %d, want 2%% of the payload", got)
	}
}

func TestNameProblemFindsNamesFATCannotStore(t *testing.T) {
	bad := map[string]string{
		"libfoo_1:2.3_amd64.deb": "colon",
		"what?.deb":              "question mark",
		"a*b.deb":                "star",
		"trailing.":              "trailing dot",
		"trailing ":              "trailing space",
		"a\tb.deb":               "control character",
	}
	for name, why := range bad {
		if nameProblem(name) == "" {
			t.Errorf("nameProblem(%q) found nothing, but the name has a %s", name, why)
		}
	}
	for _, name := range []string{"libfoo_1.2.3-1_amd64.deb", "Packages.gz", "manifest.json", "pügins_ähh_中文.deb"} {
		if p := nameProblem(name); p != "" {
			t.Errorf("nameProblem(%q) = %q, want no problem", name, p)
		}
	}
}
