package harness

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RepoServer serves a host directory of synthetic repositories over HTTP so
// every container in a fixture run — the "state" container and the
// separately-started "builder" container do not share a filesystem — sees
// identical bytes at identical URLs. See syntheticdeb.go's package doc for
// why file:// URIs cannot do this job.
type RepoServer struct {
	ln  net.Listener
	srv *http.Server

	// serveMu guards serveErr, which records a Serve failure other than the
	// expected http.ErrServerClosed. Without it, a server that died under
	// the fixture's feet is invisible here and surfaces only as an
	// unexplained `apt-get update` failure inside a container — the right
	// row status (target-setup blocked) with the wrong explanation.
	serveMu  sync.Mutex
	serveErr error

	// vendorHits counts responses served under vendorPrefix, and is what
	// pacedFileHandler consults to decide whether to pace this one.
	vendorHits atomic.Int64
}

const (
	// vendorPrefix is the URL prefix RequestSpec.VendorURLs are served
	// under, and vendorDirName is the directory beneath the server root
	// they are written into.
	vendorPrefix  = "/vendor/"
	vendorDirName = "vendor"

	// vendorPaceChunk and vendorPaceDelay pace every vendor response after
	// the first. 64 KiB every 40ms is ~1.6 MB/s: slow enough that a
	// multi-megabyte .deb spans a dozen of core/fetch's 250ms progress
	// ticks, fast enough that the extra cost to a row is a couple of
	// seconds.
	vendorPaceChunk = 64 * 1024
	vendorPaceDelay = 40 * time.Millisecond
)

// pacedFileHandler serves dir, sending the first response at full speed and
// pacing every one after it.
//
// This looks like a flaky server and is the opposite of one: it is
// deterministic in the only variable that matters, the request ordinal, and
// it exists because a determinism row that cannot vary the network cannot
// see the defect it is there to catch.
//
// core/fetch's progressWriter emits a progress event every 250ms of WALL
// CLOCK, so the number of them a download produces is a fact about the
// network and nothing else. When those events reached the collector that
// becomes evidence.json — which the manifest hashes and the signature covers
// — two builds of one request produced two different bundle ids purely
// because the second download was slower. core/engine's liveOnlyTypes now
// drops the type on the way to the bundle's copy while leaving the
// operator's live stream exact.
//
// checkDeterminism runs the same request twice against this server. Served
// at the same speed both times, both downloads would emit the same two
// events (progressWriter emits once on the first write and once at the end),
// and the row would pass whether or not the drop existed — a green row
// proving nothing, which is the failure mode this suite has hit before. With
// the second response paced, build one emits ~2 progress events and build
// two emits ~10; without the drop the two evidence.json files differ by
// eight events and the row is red. Verified both ways before this landed:
// red against a tree with liveOnlyTypes removed, green with it.
//
// Pacing the SECOND rather than the first is deliberate: a paced first
// response would slow every row that only downloads once, for nothing.
func (s *RepoServer) pacedFileHandler(dir http.Dir) http.Handler {
	plain := http.FileServer(dir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.vendorHits.Add(1) == 1 {
			plain.ServeHTTP(w, r)
			return
		}
		f, err := dir.Open(path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/")))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		// Content-Length is set explicitly so the client sees an ordinary
		// sized response rather than a chunked one: core/fetch reads
		// resp.ContentLength into the progress event's total_bytes, and a
		// -1 there would make this fixture exercise a different path from
		// the one a real vendor archive produces.
		w.Header().Set("Content-Type", "application/vnd.debian.binary-package")
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, vendorPaceChunk)
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
				time.Sleep(vendorPaceDelay)
			}
			if rerr != nil {
				return
			}
		}
	})
}

// VendorDir is the directory beneath the server root that VendorURLs are
// written into, and VendorURL is the URL one of them is reachable at.
func (s *RepoServer) VendorDir(rootDir string) string { return filepath.Join(rootDir, vendorDirName) }

// VendorURL returns the URL for one vendor file, with query appended when
// non-empty. query carries no leading "?".
func (s *RepoServer) VendorURL(filename, query string) string {
	u := s.BaseURL() + vendorPrefix + filename
	if query != "" {
		u += "?" + query
	}
	return u
}

// StartRepoServer serves rootDir (created if it does not exist) on an
// OS-assigned free port on the host loopback interface.
func StartRepoServer(rootDir string) (*RepoServer, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start synthetic repo server: %w", err)
	}
	mux := http.NewServeMux()
	s := &RepoServer{ln: ln}
	// Everything except vendorPrefix is served plainly. Under vendorPrefix
	// the FIRST response goes out at full speed and every later one is
	// paced — see pacedFileHandler for why that asymmetry is the only way
	// to make a determinism row able to see the defect it exists for.
	mux.Handle(vendorPrefix, http.StripPrefix(vendorPrefix, s.pacedFileHandler(http.Dir(filepath.Join(rootDir, vendorDirName)))))
	mux.Handle("/", http.FileServer(http.Dir(rootDir)))
	// ReadHeaderTimeout bounds how long a client may take to send its
	// request headers. This server binds loopback only and lives for one
	// fixture run, so the Slowloris exposure is negligible — but a bound is
	// a one-line honest fix, which beats carrying a suppressed lint finding
	// in a package whose whole job is to be trustworthy about failures.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	s.srv = srv
	go func() {
		// http.ErrServerClosed is the expected result of a clean Close;
		// anything else means this run's synthetic repositories stopped
		// being served, which Close() then reports so the row's transcript
		// says so instead of leaving a bare apt failure to explain itself.
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveMu.Lock()
			s.serveErr = err
			s.serveMu.Unlock()
		}
	}()
	return s, nil
}

// Port is the host TCP port the server listens on.
func (s *RepoServer) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// BaseURL is the URL a container on this Docker host reaches this server at.
// host.docker.internal is Docker Desktop's built-in DNS name for the host on
// Windows and macOS; on plain Linux Docker it needs
// --add-host=host.docker.internal:host-gateway, which StartContainer always
// passes (docker.go) so a Linux CI runner behaves the same way.
func (s *RepoServer) BaseURL() string {
	return fmt.Sprintf("http://host.docker.internal:%d", s.Port())
}

// Close shuts the server down, reporting either a shutdown failure or a
// Serve failure that happened earlier in the run (whichever came first is
// what a reader needs to know). Safe to call on a nil *RepoServer.
func (s *RepoServer) Close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := s.srv.Shutdown(ctx)
	s.serveMu.Lock()
	serveErr := s.serveErr
	s.serveMu.Unlock()
	if serveErr != nil {
		return fmt.Errorf("synthetic repo server stopped serving mid-run: %w", serveErr)
	}
	return shutdownErr
}
