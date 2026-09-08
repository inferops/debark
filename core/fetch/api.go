// Package fetch downloads the inputs apt cannot: vendor .deb files named by
// URL. Archive packages are always downloaded by apt itself, which verifies
// them against the signed indices; this package never touches them.
package fetch

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/store"
)

// Options configures a fetcher.
type Options struct {
	// Client is the HTTP client. Empty means a client with sane timeouts and
	// no redirect to a non-https scheme.
	Client *http.Client
	// Timeout is the per-request timeout. Default 5 minutes.
	Timeout time.Duration
	// Retries is the number of attempts per URL. Default 3, with backoff.
	Retries int
	// Store receives the downloaded bytes.
	Store store.Store
	// Events receives fetch.file. Optional.
	Events evidence.Sink
	// UserAgent is sent with every request. It carries the tool version and
	// nothing else: no machine identity ever leaves the builder.
	UserAgent string
}

// Fetcher downloads vendor .deb files.
type Fetcher interface {
	// Fetch downloads one URL into the store and returns what it learned.
	// A user-supplied digest that does not match is a verification failure
	// (exit 4), not a retry.
	Fetch(ctx context.Context, in buildjob.URLInput) (*Fetched, error)

	// FetchAll downloads several URLs concurrently, bounded. It returns one
	// entry per input in the same order; a failed entry has Err set and the
	// caller decides whether that is exit 3.
	FetchAll(ctx context.Context, in []buildjob.URLInput) []Result
}

// Fetched is one successfully downloaded file.
type Fetched struct {
	// URL is the URL that was fetched, with any userinfo credential and any
	// query string already redacted (RedactURL) — this is what a caller
	// should write into a persisted artefact such as lock.Origin.URI; the
	// actual fetch used the real, credentialed URL, which is not retained
	// here. For a FromLocalFile / FromLocalFileWithDigest result there was
	// no URL: this instead carries the path the caller gave, since Fetched
	// has no separate field for that (compare lock.Origin, which does
	// distinguish URI from LocalPath) — callers must not url.Parse it in
	// that case.
	URL string
	// Filename is the name to use in the pool: from Content-Disposition when
	// present and safe, otherwise the last path element of the URL.
	Filename string
	Digest   string
	Size     int64
	// StorePath is where the bytes now live.
	StorePath string
	// Verification is user-digest when the operator supplied a matching
	// digest, url-unverified otherwise. HTTPS is not provenance.
	Verification lock.PublisherVerification
}

// Result pairs a fetch outcome with its error.
type Result struct {
	Input   buildjob.URLInput
	Fetched *Fetched
	Err     error
}

// New returns the standard fetcher. A zero Options is valid: it builds a
// default HTTP client, three retries and no evidence sink.
func New(opts Options) Fetcher {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.Retries <= 0 {
		opts.Retries = defaultRetries
	}
	if opts.UserAgent == "" {
		opts.UserAgent = defaultUserAgent
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{CheckRedirect: checkRedirect}
	} else if client.CheckRedirect == nil {
		// The caller supplied a client but not a redirect policy: still
		// enforce the no-downgrade rule rather than silently accepting
		// Go's default (unconditional-follow) behaviour.
		c2 := *client
		c2.CheckRedirect = checkRedirect
		client = &c2
	}
	return &fetcher{opts: opts, client: client}
}

// FromLocalFile ingests an operator-supplied .deb path into the store,
// returning the same shape a download would. It never moves or modifies the
// caller's file (user-supplied files are never pruned, and are never
// even touched beyond being read).
//
// This frozen signature carries no digest parameter, so it can never itself
// report VerifiedUserDigest; see FromLocalFileWithDigest for the case where
// the caller has an expected digest to check. See that function's comment
// for why a bare local file is labelled url-unverified rather than
// user-digest.
func FromLocalFile(ctx context.Context, st store.Store, path string) (*Fetched, error) {
	return fromLocalFile(ctx, st, path, "")
}

// ScanDir returns every .deb file directly inside dir, sorted, non-recursive.
// A local-debs directory is a flat drop box: nothing but its own topmost
// entries participate, matching the prototype's `find -maxdepth 1`.
func ScanDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, dferr.New(dferr.Usage, "fetch: local-dir not found: %s", dir)
		}
		return nil, dferr.Wrap(dferr.Usage, err, "fetch: reading %s", dir)
	}
	// os.ReadDir already returns entries sorted by filename; filtering
	// preserves that order, so no separate sort is needed.
	var out []string
	for _, e := range entries {
		if e.IsDir() || !e.Type().IsRegular() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".deb") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

// ParseListFile expands a packages.txt-style list into request inputs,
// applying the syntax:
//
//	vlc                       apt package (apt: prefix optional)
//	vlc=3.0.21-1build1        pinned version
//	https://host/x.deb        URL (url: prefix for URLs not ending in .deb)
//	./local-debs/zoom.deb     local file (file: prefix optional), relative to
//	                          the list file
//
// Blank lines and lines starting with # are ignored. A trailing
// "sha256=<hex>" on a URL line supplies the expected digest.
func ParseListFile(path string) (buildjob.Inputs, error) {
	return parseListFile(path)
}
