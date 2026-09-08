package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/evidence"
	"github.com/inferops/debark/core/lock"
)

// Defaults used when Options leaves a field at its zero value.
const (
	defaultTimeout     = 5 * time.Minute
	defaultRetries     = 3
	defaultUserAgent   = "debark"
	defaultConcurrency = 4 // bounded: polite to vendor servers, not a knob on Options (frozen)

	backoffBase = 200 * time.Millisecond
	backoffCap  = 5 * time.Second
)

// maxDownloadSize bounds how many response bytes one Fetch will write to
// disk. Without it io.Copy runs to EOF, and a hostile or compromised vendor
// server picks that EOF: an HTTP/1.1 chunked response has no declared
// length, so a server can answer a .deb request with an endless stream and
// fill the builder's disk. That builder is the one host holding the signing
// key, so "the machine that signs runs out of disk" is a denial of service
// against the whole air-gap pipeline, not a local nuisance. Confirmed before
// this limit existed: a 600-byte .deb prefix followed by an unbounded
// chunked body was accepted in full.
//
// Content-Length is checked against the same limit first, purely to refuse
// early — it is a claim by the same server the limit exists to distrust, so
// it can lie in either direction and the streaming check below is what
// actually enforces the bound.
//
// 8 GiB matches core/bundle's maxImportEntrySize, chosen there for the same
// reason: comfortably past any real .deb (the largest in Debian are a few
// hundred MiB; vendor CUDA/toolchain packages reach a few GiB) while still
// a bound. A var, not a const, for the reason bundle gives too — the only
// way to exercise the limit without a test that genuinely writes gigabytes
// is to lower it.
var maxDownloadSize int64 = 8 << 30 // 8 GiB

// fetcher is the standard Fetcher.
type fetcher struct {
	opts   Options
	client *http.Client
}

// checkRedirect enforces the one rule that must never be silent: a redirect
// chain that started at https must never continue at anything else. It is
// installed on every client New builds, including a caller-supplied one that
// did not set its own policy.
//
// A redirect that only changes host is allowed (vendor downloads routinely
// bounce through a CDN); Fetch records that separately as a warning once the
// exchange completes, so it is seen, not followed "without saying so".
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	orig := via[0].URL
	if orig.Scheme == "https" && req.URL.Scheme != "https" {
		// from/to feed straight into an error message that can reach
		// evidence (via attempt's use of schemeDowngradeError below, then
		// the engine's "error" attribute) — redact here, once, at
		// construction, rather than at every place the error is later
		// formatted.
		return &schemeDowngradeError{
			from: RedactURL(orig.String()),
			to:   RedactURL(req.URL.String()),
			why:  "would downgrade from https to plaintext",
		}
	}
	if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
		// Fetch validates the scheme of the URL the operator typed, but a
		// redirect target is chosen by the server, and the downgrade rule
		// above only fires for a chain that STARTED at https. So an
		// operator who opted into a plaintext http:// download could be
		// redirected to any scheme at all. In practice Go's default
		// transport refuses an unknown scheme itself ("unsupported
		// protocol scheme"), which is why this was never exploitable — but
		// that is the transport's decision, not this package's, and
		// Options.Client lets a caller supply a transport with more
		// protocols registered (http.Transport.RegisterProtocol, "file"
		// being the obvious one). The policy belongs here, where it holds
		// whatever transport is underneath.
		return &schemeDowngradeError{
			from: RedactURL(orig.String()),
			to:   RedactURL(req.URL.String()),
			why:  fmt.Sprintf("only http and https are fetched, not %q", req.URL.Scheme),
		}
	}
	return nil
}

// schemeDowngradeError marks a refused redirect so Fetch can classify it as
// non-retryable instead of a generic transport failure. from and to are
// already redacted by the time they get here (checkRedirect).
type schemeDowngradeError struct{ from, to, why string }

func (e *schemeDowngradeError) Error() string {
	return fmt.Sprintf("refusing redirect from %s to %s: %s", e.from, e.to, e.why)
}

// Fetch downloads one URL into the store. See the Fetcher interface doc.
//
// Every error leaves through redactErrorMessage. Each message this package
// formats itself already names safeURL rather than in.URL, but a transport
// failure arrives wrapped in a *url.Error that net/http built, and that one
// prints the request URL verbatim — userinfo, query string and all. The
// result used to be a half-redacted sentence that the engine then wrote
// into the "error" attribute of a warn-level input.external event, hence
// into evidence.json, hence into a signed artefact that crosses the air
// gap. See RedactMessage.
func (f *fetcher) Fetch(ctx context.Context, in buildjob.URLInput) (*Fetched, error) {
	fetched, err := f.fetch(ctx, in)
	if err != nil {
		return nil, redactErrorMessage(err)
	}
	return fetched, nil
}

func (f *fetcher) fetch(ctx context.Context, in buildjob.URLInput) (*Fetched, error) {
	u, err := url.Parse(in.URL)
	if err != nil || u.Host == "" {
		return nil, because(buildjob.ReasonBadInput, dferr.New(dferr.Usage, "fetch: invalid URL %q", RedactURL(in.URL)))
	}
	// safeURL is in.URL with any userinfo/query credential stripped
	// (RedactURL). Every message or evidence attribute built in this
	// function and the ones it calls uses safeURL, never in.URL directly —
	// the actual HTTP request still goes out over u/in.URL further down,
	// credentials included; only what could end up in evidence.json or
	// lock.json is redacted (core/evidence's contract: a value there "must
	// never contain a secret").
	safeURL := RedactURL(in.URL)
	switch u.Scheme {
	case "https":
		// The default and the only scheme that proceeds silently.
	case "http":
		// Explicitly requested by the operator: honoured, but the
		// operator is told plainly that this download has no transport
		// confidentiality or integrity.
		evidence.Warn(f.opts.Events, evidence.TypeWarning,
			fmt.Sprintf("fetching %s over plaintext HTTP: no transport confidentiality or integrity", safeURL),
			map[string]any{"url": safeURL})
	default:
		return nil, because(buildjob.ReasonBadInput, dferr.New(dferr.Usage, "fetch: unsupported URL scheme %q in %q (only https:// is fetched by default; http:// is honoured, with a warning)", u.Scheme, safeURL))
	}

	wantDigest := strings.ToLower(strings.TrimSpace(in.SHA256))
	if wantDigest != "" && !digest.Valid(wantDigest) {
		return nil, because(buildjob.ReasonBadInput, dferr.New(dferr.Usage, "fetch: %s: malformed sha256 (want 64 lowercase hex characters)", safeURL))
	}

	attempts := f.opts.Retries
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		fetched, retryable, err := f.attempt(ctx, in, u, wantDigest)
		if err == nil {
			return fetched, nil
		}
		lastErr = err
		if !retryable || attempt == attempts {
			break
		}
		evidence.Emit(f.opts.Events, evidence.TypeProgress, fmt.Sprintf("retrying %s (attempt %d/%d)", safeURL, attempt+1, attempts),
			map[string]any{"url": safeURL, "attempt": attempt + 1, "attempts": attempts})
		if !sleepBackoff(ctx, attempt) {
			return nil, because(buildjob.ReasonCancelled, dferr.Wrap(dferr.Incomplete, ctx.Err(), "fetch: %s: cancelled", safeURL))
		}
	}
	return nil, lastErr
}

// attempt performs one HTTP round trip and, on a structurally sound and
// (when requested) digest-matching response, ingests the result into the
// store. retryable tells Fetch whether trying again could plausibly help.
func (f *fetcher) attempt(ctx context.Context, in buildjob.URLInput, u *url.URL, wantDigest string) (*Fetched, bool, error) {
	safeURL := RedactURL(in.URL)

	reqCtx, cancel := context.WithTimeout(ctx, f.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, because(buildjob.ReasonBadInput, dferr.Wrap(dferr.Usage, err, "fetch: %s: building request", safeURL))
	}
	req.Header.Set("User-Agent", f.opts.UserAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		var sd *schemeDowngradeError
		if errors.As(err, &sd) {
			// sd's own from/to fields were already redacted where the error
			// was constructed (checkRedirect), so %v here is already safe.
			return nil, false, because(buildjob.ReasonRefusedRedirect, dferr.New(dferr.Incomplete, "fetch: %s: %v", safeURL, sd))
		}
		if ctx.Err() != nil {
			return nil, false, because(buildjob.ReasonCancelled, dferr.Wrap(dferr.Incomplete, ctx.Err(), "fetch: %s: cancelled", safeURL))
		}
		// The one branch where the reason is not decided by which line was
		// reached: "the client could not complete the exchange" covers both
		// "nothing answered" and "something answered and its certificate
		// could not be verified", and those are different problems on
		// different machines. IsTLSVerificationError asks the error itself
		// rather than reading its text.
		if IsTLSVerificationError(err) {
			// Not retryable: an untrusted certificate is a fact about this
			// machine's trust store, and asking again produces it again.
			return nil, false, because(buildjob.ReasonTLSUntrusted, dferr.Wrap(dferr.Incomplete, err, "fetch: %s: request failed", safeURL))
		}
		return nil, true, because(buildjob.ReasonUnreachable, dferr.Wrap(dferr.Incomplete, err, "fetch: %s: request failed", safeURL))
	}
	defer resp.Body.Close()

	if resp.Request != nil && resp.Request.URL != nil && !strings.EqualFold(resp.Request.URL.Host, u.Host) {
		// resp.Request.URL.Host never carries userinfo (url.URL keeps User
		// separate from Host), so it needs no redaction of its own.
		evidence.Warn(f.opts.Events, evidence.TypeWarning,
			fmt.Sprintf("%s redirected to a different host: %s", safeURL, resp.Request.URL.Host),
			map[string]any{"url": safeURL, "redirected_host": resp.Request.URL.Host})
	}

	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode >= 500
		return nil, retryable, because(buildjob.ReasonHTTPStatus, dferr.New(dferr.Incomplete, "fetch: %s: server returned %s", safeURL, resp.Status))
	}

	if resp.ContentLength > maxDownloadSize {
		// Not retryable: a server that says it is about to send more than
		// the limit will say so again.
		return nil, false, because(buildjob.ReasonTooLarge, dferr.New(dferr.Incomplete,
			"fetch: %s: server declared %d bytes, over the %d-byte download limit", safeURL, resp.ContentLength, maxDownloadSize))
	}

	filename := filenameFor(u, resp.Header.Get("Content-Disposition"))

	fetched, retryable, err := f.download(ctx, in, resp, filename, wantDigest)
	if err != nil {
		return nil, retryable, err
	}

	evidence.Emit(f.opts.Events, evidence.TypeFetchFile, fmt.Sprintf("fetched %s", filename), map[string]any{
		"url":          safeURL,
		"filename":     filename,
		"digest":       fetched.Digest,
		"size":         fetched.Size,
		"verification": string(fetched.Verification),
	})
	return fetched, false, nil
}

// download streams resp.Body to a scratch file, verifies it is really a
// .deb and (when requested) that it matches wantDigest, then hands it to the
// store. The scratch file lives outside the store's own tree (this
// deliberately does not reach into store.Store.Root(), since GC/index
// behaviour there belongs to core/store) so a failed or
// cancelled download never becomes a partial store object.
func (f *fetcher) download(ctx context.Context, in buildjob.URLInput, resp *http.Response, filename, wantDigest string) (*Fetched, bool, error) {
	safeURL := RedactURL(in.URL)

	tmp, err := os.CreateTemp("", "debark-fetch-*.tmp")
	if err != nil {
		return nil, false, because(buildjob.ReasonLocalStorage, dferr.Wrap(dferr.Environment, err, "fetch: %s: creating scratch file", safeURL))
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			// Best-effort cleanup of a scratch file this function is
			// abandoning: the download already failed (or was refused), an
			// error is already on its way back to the caller, and a leftover
			// file in the OS temp directory is not worth displacing it. The
			// `_ =` is deliberate, not forgotten.
			_ = os.Remove(tmpPath)
		}
	}()

	pw := &progressWriter{sink: f.opts.Events, url: safeURL, total: resp.ContentLength}
	// LimitReader at maxDownloadSize+1, not maxDownloadSize: reading one
	// byte past the limit is what distinguishes "a file of exactly the
	// maximum size" from "a stream that had not finished", so the check
	// below can refuse the second without rejecting the first.
	written, copyErr := io.Copy(io.MultiWriter(tmp, pw), io.LimitReader(resp.Body, maxDownloadSize+1))
	closeErr := tmp.Close()
	pw.final()
	if copyErr != nil {
		if ctx.Err() != nil {
			return nil, false, because(buildjob.ReasonCancelled, dferr.Wrap(dferr.Incomplete, ctx.Err(), "fetch: %s: cancelled after %d bytes", safeURL, written))
		}
		return nil, true, because(buildjob.ReasonUnreachable, dferr.Wrap(dferr.Incomplete, copyErr, "fetch: %s: download failed after %d bytes", safeURL, written))
	}
	if closeErr != nil {
		return nil, true, because(buildjob.ReasonLocalStorage, dferr.Wrap(dferr.Environment, closeErr, "fetch: %s: writing scratch file", safeURL))
	}
	if written > maxDownloadSize {
		// See maxDownloadSize. Not retryable: the response was not a
		// transport hiccup, it was more data than this package will ever
		// accept, and asking again invites the same flood.
		return nil, false, because(buildjob.ReasonTooLarge, dferr.New(dferr.Incomplete,
			"fetch: %s: response exceeds the %d-byte download limit", safeURL, maxDownloadSize))
	}

	f2, err := os.Open(tmpPath)
	if err != nil {
		return nil, false, because(buildjob.ReasonLocalStorage, dferr.Wrap(dferr.Environment, err, "fetch: %s: reopening scratch file", safeURL))
	}
	sniffErr := SniffDeb(f2)
	// Read-only handle, closed the moment the sniff is done: a failing Close
	// on a file that was only ever read has nothing to report and nothing to
	// flush, and tmpPath is re-read below regardless. Deliberately ignored.
	_ = f2.Close()
	if sniffErr != nil {
		return nil, false, because(buildjob.ReasonNotADeb, fmt.Errorf("fetch: %s: %w", safeURL, sniffErr))
	}

	gotDigest, _, err := digest.SHA256File(tmpPath)
	if err != nil {
		return nil, false, because(buildjob.ReasonLocalStorage, dferr.Wrap(dferr.Environment, err, "fetch: %s: hashing download", safeURL))
	}
	if wantDigest != "" && !digest.Equal(gotDigest, wantDigest) {
		return nil, false, because(buildjob.ReasonDigestMismatch, dferr.New(dferr.Verification, "fetch: %s: sha256 mismatch: expected %s, got %s", safeURL, wantDigest, gotDigest))
	}

	if f.opts.Store == nil {
		return nil, false, because(buildjob.ReasonLocalStorage, dferr.New(dferr.Usage, "fetch: %s: no Store configured", safeURL))
	}
	storeDigest, size, err := f.opts.Store.PutFile(ctx, tmpPath, true)
	if err != nil {
		return nil, true, because(buildjob.ReasonLocalStorage, dferr.Wrap(dferr.Environment, err, "fetch: %s: storing download", safeURL))
	}
	committed = true

	// The digest was checked above against the scratch file; what the lock
	// and the bundle will carry is the digest the STORE computed over the
	// bytes it actually ingested. Those are two separate reads of a path,
	// and only this comparison makes the operator's --digest a statement
	// about the bytes that were kept rather than about bytes that were
	// merely seen. Nothing routine makes them differ, which is the point:
	// if they ever do, the file changed underneath the check or the store
	// filed something else, and neither may be recorded as verified.
	if !digest.Equal(storeDigest, gotDigest) {
		return nil, false, because(buildjob.ReasonDigestMismatch, dferr.New(dferr.Verification,
			"fetch: %s: stored bytes hash to %s but the download hashed to %s", safeURL, storeDigest, gotDigest))
	}

	verification := lock.VerifiedURLUnverified
	if wantDigest != "" {
		verification = lock.VerifiedUserDigest
	}
	return &Fetched{
		// URL is redacted, not the raw in.URL: see RedactURL's doc and
		// F3 in docs/security/review-findings.md — this is the value a
		// caller may end up writing into a persisted artefact (e.g.
		// lock.Origin.URI), so it must already be safe by the time it
		// leaves this package, not merely at whichever downstream call
		// site happens to remember to redact it.
		URL:          safeURL,
		Filename:     filename,
		Digest:       storeDigest,
		Size:         size,
		StorePath:    f.opts.Store.Path(storeDigest),
		Verification: verification,
	}, false, nil
}

// FetchAll downloads several URLs concurrently, bounded, preserving input
// order in the result and never aborting the batch over one failure.
func (f *fetcher) FetchAll(ctx context.Context, in []buildjob.URLInput) []Result {
	results := make([]Result, len(in))
	if len(in) == 0 {
		return results
	}
	sem := make(chan struct{}, defaultConcurrency)
	var wg sync.WaitGroup
	for i, one := range in {
		wg.Add(1)
		go func(i int, one buildjob.URLInput) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = Result{Input: one, Err: because(buildjob.ReasonCancelled, dferr.Wrap(dferr.Incomplete, ctx.Err(), "fetch: %s: cancelled", RedactURL(one.URL)))}
				return
			}
			defer func() { <-sem }()
			fetched, err := f.Fetch(ctx, one)
			results[i] = Result{Input: one, Fetched: fetched, Err: err}
		}(i, one)
	}
	wg.Wait()
	return results
}

// progressWriter emits throttled progress events while a download streams
// to disk, so the CLI can render a bar or a line without core/ ever touching
// a terminal itself.
type progressWriter struct {
	sink     evidence.Sink
	url      string
	total    int64
	written  int64
	lastEmit time.Time
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.written += int64(len(p))
	now := time.Now()
	if w.lastEmit.IsZero() || now.Sub(w.lastEmit) >= 250*time.Millisecond {
		w.emit()
		w.lastEmit = now
	}
	return len(p), nil
}

func (w *progressWriter) final() { w.emit() }

func (w *progressWriter) emit() {
	evidence.Emit(w.sink, evidence.TypeProgress, "", map[string]any{
		"url":         w.url,
		"bytes":       w.written,
		"total_bytes": w.total, // -1 when the server did not send Content-Length
	})
}

// sleepBackoff waits an exponentially increasing delay before the next
// retry, honouring ctx. It returns false when ctx was cancelled first.
func sleepBackoff(ctx context.Context, attempt int) bool {
	d := backoffBase << uint(attempt-1)
	if d > backoffCap || d <= 0 {
		d = backoffCap
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// filenameFor derives the pool filename for a downloaded .deb: the
// Content-Disposition filename when present and safe, otherwise the last
// path element of the URL. contentDisposition is untrusted input from a
// remote server that becomes a filename inside the bundle, so it is
// never trusted blindly.
func filenameFor(u *url.URL, contentDisposition string) string {
	if name := safeFilenameFromContentDisposition(contentDisposition); name != "" {
		return name
	}
	return safeFilenameFromURL(u)
}

func safeFilenameFromContentDisposition(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	if ext, ok := params["filename*"]; ok && ext != "" {
		if decoded, ok := decodeExtValue(ext); ok {
			if safe := sanitizeFilename(decoded); safe != "" {
				return safe
			}
		}
		// A present-but-unusable filename* means the server tried to be
		// specific and failed; do not silently fall back to a plain
		// "filename" that might be stale or, worse, attacker-controlled in
		// a different way. Fall through to the URL instead.
		return ""
	}
	return sanitizeFilename(params["filename"])
}

// decodeExtValue decodes an RFC 5987/6266 ext-value: charset'lang'value,
// where value is percent-encoded. Go's mime.ParseMediaType may or may not
// have already decoded this depending on version, so both a still-encoded
// and an already-decoded input are handled: if the charset'lang' structure
// is still visible, decode it; otherwise use the value as given.
func decodeExtValue(v string) (string, bool) {
	if parts := strings.SplitN(v, "'", 3); len(parts) == 3 {
		decoded, err := url.PathUnescape(parts[2])
		if err != nil {
			return "", false
		}
		return decoded, true
	}
	return v, true
}

func safeFilenameFromURL(u *url.URL) string {
	base := path.Base(u.EscapedPath())
	if unescaped, err := url.PathUnescape(base); err == nil {
		base = unescaped
	}
	if safe := sanitizeFilename(base); safe != "" {
		return safe
	}
	return "download.deb"
}

// sanitizeFilename returns name unchanged if it is safe to use as a single
// path component joined under the bundle's pool directory, or "" if not.
// "Safe" means: no path separator of either flavour (a hostile header must
// not be able to smuggle a Windows-style separator into a build running on
// Linux, or vice versa), no ".", no "..", no control character (including
// one that arrived via percent- or RFC-2231-decoding rather than literally
// in the header bytes), and not something the local OS would treat as an
// absolute path or a drive reference.
//
// Every rule is applied on every platform, never under a GOOS check. Two
// reasons. Builds are deterministic, so the same header must be
// accepted or refused identically whichever OS the builder runs; and a name
// that is harmless where it is created is not necessarily harmless where the
// bundle is opened, since the pool entry travels to a target whose OS this
// package cannot see.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	if strings.ContainsAny(name, "/\\") {
		return ""
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	if filepath.IsAbs(name) {
		return ""
	}
	if strings.ContainsRune(name, ':') {
		// Wider than the old "second byte is a colon" test, which caught
		// "C:evil.deb" and missed "x.deb:evil". On NTFS everything after a
		// colon names an alternate data stream, so "x.deb:evil" opens a
		// hidden stream of x.deb rather than a file called "x.deb:evil" —
		// bytes written where nothing later looks for them. No legitimate
		// .deb filename contains a colon.
		return ""
	}
	if strings.HasSuffix(name, ".") {
		// Win32 silently strips trailing dots when resolving a path, so
		// "vlc.deb." and "vlc.deb" are the same file there and different
		// strings everywhere else. A server that can name two downloads
		// that way makes the second quietly overwrite the first on a
		// Windows builder while the records still show two distinct files.
		// (A trailing space is the same trick; TrimSpace above removes it.)
		return ""
	}
	if len(name) > maxFilenameLen {
		// 255 bytes is the per-component maximum on ext4, NTFS, APFS and
		// XFS alike, and matches core/bundle's maxImportNameComponentLen.
		// Past it the name is not merely long: it cannot be written at all,
		// so accepting it only defers the failure to a later stage that
		// cannot say which server sent it.
		return ""
	}
	if isReservedDeviceName(name) {
		return ""
	}
	return name
}

// maxFilenameLen bounds one pool path component. See sanitizeFilename.
const maxFilenameLen = 255

// reservedDeviceNames are the Win32 device names, lower-cased. COM0 and
// LPT0 are in the set although Win32's own matcher takes only the digits
// 1-9: the set is deliberately a superset, because a name wrongly refused
// costs a fallback to the URL-derived filename and a name wrongly accepted
// costs a download that is silently thrown away. See isReservedDeviceName.
var reservedDeviceNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com0": true, "com1": true, "com2": true, "com3": true, "com4": true,
	"com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt0": true, "lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
	"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// isReservedDeviceName reports whether name would address a Win32 character
// device instead of naming a file. Windows resolves these in any directory,
// and the resolution is invisible from the caller's side: opening "NUL.deb"
// for writing succeeds, every Write succeeds, Close succeeds, and the bytes
// are gone. The builder would then record, hash and sign a .deb it never
// actually kept.
//
// What is matched is the stem before the FIRST dot, case-insensitively and
// ignoring trailing spaces, so "nul.deb", "nul.deb.deb" and "nul .deb" are
// all refused. That is the widest rule any Windows applies rather than the
// narrowest: the legacy Win32 path parser looks only at the name portion up
// to the first ".", ignores whatever extension follows, and ignores trailing
// spaces while doing it, which is why Microsoft's own naming guidance warns
// against "NUL.txt" and not merely against "NUL".
//
// It is deliberately stricter than what the machine this was written on
// does. Measured on Windows 11 10.0.26200, through cmd.exe redirection as
// well as through os.Create so the result is the OS's and not the Go
// runtime's: a bare "NUL" goes to the device (create, write and close all
// return nil, no file appears, a read back yields nothing), while
// "nul.deb", "nul.deb.deb", "con.log" and "lpt1.deb" are all created as
// ordinary files — which is precisely what Microsoft's own naming guidance
// says must not be relied on, and what every Windows that follows it does
// send to the device. The disagreement is the finding: which names resolve
// is a property of the Windows BUILD. It therefore cannot be probed and
// acted on either, because builds are deterministic and the same
// header has to be accepted or refused identically on every builder — and
// the pool entry is opened again on a target host whose Windows this
// package never sees at all.
//
// Refusing costs almost nothing on the other side of the trade: a refused
// Content-Disposition falls back to the URL's own last path element, a
// refused local file is reported to the operator as a name they can change,
// and no real Debian package filename has a stem of nul, con, aux, prn,
// com1-9 or lpt1-9.
func isReservedDeviceName(name string) bool {
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	// The whole-name TrimSpace in sanitizeFilename cannot see this one: in
	// "nul .deb" the space is interior, and Win32 discards it before it
	// compares the stem.
	stem = strings.TrimRight(stem, " ")
	return reservedDeviceNames[strings.ToLower(stem)]
}
