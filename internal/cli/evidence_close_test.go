package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

// failingEventsFile is a --json-events target that accepts every write and
// then fails to close. That is how the failure this test exists for actually
// presents itself: a full disk or removable media pulled out mid-run does not
// refuse the open, it refuses the final flush, and Close is the only call
// that ever reports it. There is no portable way to make a real
// (*os.File).Close fail on both Linux and Windows, which is why
// openEventsFile is a substitutable seam.
type failingEventsFile struct{ *os.File }

var errEventsFileFull = errors.New("no space left on device")

// Close still releases the real handle before reporting the failure, exactly
// as (*os.File).Close does when the underlying flush fails — and, on Windows,
// so t.TempDir can still delete the file afterwards.
func (f failingEventsFile) Close() error {
	_ = f.File.Close()
	return errEventsFileFull
}

// useFailingEventsFile points --json-events at a stream whose Close fails,
// for the duration of one test.
func useFailingEventsFile(t *testing.T) {
	t.Helper()
	saved := openEventsFile
	openEventsFile = func(path string) (io.WriteCloser, error) {
		f, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		return failingEventsFile{f}, nil
	}
	t.Cleanup(func() { openEventsFile = saved })
}

// minimalDeb is the smallest byte sequence core/fetch's SniffDeb accepts: an
// ar archive whose first member is debian-binary containing "2.0\n". The
// fetch path refuses anything else, and the success half of this test needs a
// command that genuinely reaches exit 0 before the close error is considered.
func minimalDeb() []byte {
	const content = "2.0\n"
	header := fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10d`\n", "debian-binary", "0", "0", "0", "100644", len(content))
	return []byte("!<arch>\n" + header + content)
}

// TestEventStreamCloseError_IsReported is the regression test for every
// `defer closeEvents()` in this package throwing the close error away. The
// event stream is frequently the only durable record of a run (core/evidence:
// "this is the one sink whose Close error the Sink contract says must not be
// swallowed"), so `debark ... --json-events run.ndjson` reporting success
// over a truncated or never-written log is exactly the failure an air-gapped
// operator has no second chance to notice.
func TestEventStreamCloseError_IsReported(t *testing.T) {
	deb := minimalDeb()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pkg.deb" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.debian.binary-package")
		_, _ = w.Write(deb)
	}))
	t.Cleanup(srv.Close)

	// Both directions of the `&& err == nil` guard: a close failure must be
	// reported when the command itself succeeded, and must never displace the
	// command's own error when it did not.
	cases := []struct {
		name string
		url  string
		want dferr.Class
	}{
		{"command succeeds: the close failure is the result", srv.URL + "/pkg.deb", dferr.Environment},
		{"command already failed: its own class survives", srv.URL + "/missing.deb", dferr.Incomplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The baseline first: without a failing close this exact
			// invocation must produce the class the command itself decided,
			// or the case below would prove nothing about Close at all.
			wantBaseline := dferr.Success
			if tc.want == dferr.Incomplete {
				wantBaseline = dferr.Incomplete
			}
			_, _, code := runReal(t, "fetch", "--config", tempConfig(t),
				"--store", t.TempDir(),
				"--json-events", filepath.Join(t.TempDir(), "events.ndjson"), tc.url)
			if code != int(wantBaseline) {
				t.Fatalf("baseline exit = %d, want %d (%s)", code, int(wantBaseline), wantBaseline)
			}

			useFailingEventsFile(t)
			_, _, code = runReal(t, "fetch", "--config", tempConfig(t),
				"--store", t.TempDir(),
				"--json-events", filepath.Join(t.TempDir(), "events.ndjson"), tc.url)
			if code != int(tc.want) {
				t.Errorf("exit = %d, want %d (%s)", code, int(tc.want), tc.want)
			}
		})
	}
}

// TestEveryEvidenceSinkCloseIsChecked covers the four call sites the runtime
// test above cannot reach without a snapshot, a bundle or an apt backend.
// `defer closeEvents()` is the exact shape of the defect — it compiles, it
// runs, and it silently discards the merged error Ctx.NewEvidenceSink goes out
// of its way to build — so the source itself is what gets asserted here.
func TestEveryEvidenceSinkCloseIsChecked(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var sites int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "defer closeEvents()" {
				t.Errorf("%s:%d discards the event stream's Close error; "+
					"use the deferred `if cerr := closeEvents(); cerr != nil && err == nil` form", f, i+1)
			}
			if strings.Contains(trimmed, "closeEvents(); cerr != nil && err == nil") {
				sites++
			}
		}
	}
	// build, install, apt-root inspect, fetch, resolve, snapshot from-base.
	//
	// `build` has exactly one site even though it now does two pieces of work
	// that emit events -- synthesizing a snapshot from --base, then the build
	// itself. That is deliberate: one sink covers the whole command, so the
	// two halves land in one stream and one progress line. A second
	// NewEvidenceSink inside build would show up here as a seventh site, and
	// should be read as a bug rather than as a number to bump.
	if sites != 6 {
		t.Errorf("found %d checked closeEvents sites, want 6 (build, install, apt-root inspect, fetch, resolve, snapshot from-base)", sites)
	}
}
