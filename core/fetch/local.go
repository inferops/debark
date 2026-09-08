package fetch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
	"github.com/inferops/debark/core/lock"
	"github.com/inferops/debark/core/store"
)

// FromLocalFileWithDigest is FromLocalFile plus an expected sha256 the
// operator supplied out of band (a per-line "sha256=" on a list entry, or a
// --digest flag). A mismatch is dferr.Verification (exit 4), immediately —
// the same rule Fetch applies to a downloaded file, applied symmetrically
// here even though the task that produced this package only states it for
// URLs: a digest check that only worked for one of the two input kinds
// would be a surprising, hard-to-notice gap.
//
// FromLocalFile's frozen signature has no parameter for this, which is why
// it exists as a new function rather than a changed one for the reasoning.
func FromLocalFileWithDigest(ctx context.Context, st store.Store, path, expectedSHA256 string) (*Fetched, error) {
	return fromLocalFile(ctx, st, path, expectedSHA256)
}

func fromLocalFile(ctx context.Context, st store.Store, path, expectedSHA256 string) (*Fetched, error) {
	if st == nil {
		return nil, because(buildjob.ReasonLocalStorage, dferr.New(dferr.Usage, "fetch: %s: no Store configured", path))
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, because(buildjob.ReasonUnreadable, dferr.New(dferr.Usage, "fetch: local .deb not found: %s", path))
		}
		return nil, because(buildjob.ReasonUnreadable, dferr.Wrap(dferr.Usage, err, "fetch: %s", path))
	}
	if info.IsDir() {
		return nil, because(buildjob.ReasonUnreadable, dferr.New(dferr.Usage, "fetch: %s: is a directory, not a .deb file", path))
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, because(buildjob.ReasonUnreadable, dferr.Wrap(dferr.Usage, err, "fetch: opening %s", path))
	}
	sniffErr := SniffDeb(f)
	closeErr := f.Close()
	if sniffErr != nil {
		return nil, because(buildjob.ReasonNotADeb, fmt.Errorf("fetch: %s: %w", path, sniffErr))
	}
	if closeErr != nil {
		return nil, because(buildjob.ReasonLocalStorage, dferr.Wrap(dferr.Environment, closeErr, "fetch: %s", path))
	}

	// The pool filename is derived here, before any bytes are ingested, so
	// an unusable name fails as an operator error naming their own file
	// rather than surfacing later as a confusing repository or filesystem
	// error. See poolFilenameForLocal for why a caller-supplied path needs
	// the same sanitising a server-supplied Content-Disposition gets.
	filename, err := poolFilenameForLocal(path)
	if err != nil {
		return nil, because(buildjob.ReasonBadInput, err)
	}

	want := strings.ToLower(strings.TrimSpace(expectedSHA256))
	if want != "" {
		if !digest.Valid(want) {
			return nil, because(buildjob.ReasonBadInput, dferr.New(dferr.Usage, "fetch: %s: malformed sha256 (want 64 lowercase hex characters)", path))
		}
		got, _, err := digest.SHA256File(path)
		if err != nil {
			return nil, because(buildjob.ReasonLocalStorage, dferr.Wrap(dferr.Environment, err, "fetch: %s: hashing", path))
		}
		if !digest.Equal(got, want) {
			return nil, because(buildjob.ReasonDigestMismatch, dferr.New(dferr.Verification, "fetch: %s: sha256 mismatch: expected %s, got %s", path, want, got))
		}
	}

	// moveOK=false: an operator-supplied file is never moved, renamed or
	// otherwise touched — only read. User-supplied files are never
	// pruned once they are in a bundle; the same care applies one step
	// earlier, at ingestion, so the operator's own copy on disk is
	// untouched no matter what debark does with the copy it takes.
	storeDigest, size, err := st.PutFile(ctx, path, false)
	if err != nil {
		return nil, because(buildjob.ReasonLocalStorage, dferr.Wrap(dferr.Environment, err, "fetch: %s: storing", path))
	}

	// Re-check the operator's digest against the digest the store computed
	// over the bytes it actually ingested. The check above hashed the
	// caller's path; PutFile then read that same path a second time, and
	// this file is deliberately never moved or locked (see moveOK=false
	// below), so between the two reads it is still fully writable by
	// whoever owns it — including whatever produced it. Without this
	// comparison a file swapped in that window is recorded as
	// VerifiedUserDigest, which is the strongest provenance claim debark
	// makes, for content no digest ever matched. Confirmed reachable
	// before this check existed.
	if want != "" && !digest.Equal(storeDigest, want) {
		return nil, because(buildjob.ReasonDigestMismatch, dferr.New(dferr.Verification,
			"fetch: %s: sha256 mismatch: expected %s, but the stored bytes hash to %s (the file changed while it was being ingested)",
			path, want, storeDigest))
	}

	// Provenance label, decided between lock.VerifiedUserDigest and
	// lock.VerifiedURLUnverified as the contract brief asked:
	//
	// VerifiedUserDigest's own doc comment is specific: "the operator
	// supplied --digest and it matched" — an assertion of an independently
	// known hash that this run then confirmed. Handing debark a file is
	// not that: no external assertion was checked, so labelling it
	// user-digest would claim a verification step that never happened.
	//
	// VerifiedURLUnverified's doc comment says "downloaded over HTTPS with
	// no publisher signature and no user-supplied digest", which is not
	// literally what happened either — there was no download. But the
	// point is not about HTTPS specifically; it is that possessing bytes,
	// however they arrived, proves nothing about who originally produced
	// them ("HTTPS is not provenance"). A file the operator hands over
	// directly and a file fetched over HTTPS with no digest share the exact
	// epistemic status debark can honestly claim: no cryptographic link to
	// a publisher exists. Reusing url-unverified for that case, rather than
	// the strictly stronger user-digest, is the choice that never overclaims.
	verification := lock.VerifiedURLUnverified
	if want != "" {
		verification = lock.VerifiedUserDigest
	}

	return &Fetched{
		// Fetched.URL has no separate "local path" sibling field in the
		// frozen contract (compare lock.Origin, which does distinguish URI
		// from LocalPath). Reusing it here for the path the caller gave us
		// is the only way this result carries enough information for the
		// engine to fill in lock.Origin.LocalPath later; it is never a URL
		// for a FromLocalFile result, and callers must not url.Parse it.
		URL:          path,
		Filename:     filename,
		Digest:       storeDigest,
		Size:         size,
		StorePath:    st.Path(storeDigest),
		Verification: verification,
	}, nil
}

// poolFilenameForLocal turns an operator-supplied .deb path into the single
// path component that will name it in the staging repository and, later, in
// the bundle's pool.
//
// SECURITY. The name here is as untrusted as a server's
// Content-Disposition, and for a while only the server-supplied one was
// sanitised: this function's predecessor was a bare filepath.Base(path).
// The asymmetry mattered because the value does not stay a display string.
// core/engine/inputs.go joins it onto the staging directory and materialises
// the store object there, then hands the same string to
// repository.PoolFile.Path, where it is written verbatim into the Packages
// stanza's Filename field. A POSIX filename may legally contain a newline,
// and a newline inside a deb822 field ends that field, so a drop-box file
// whose name embeds one followed by "Package: evil" was a stanza-injection
// primitive, and filepath.Base does not remove it. The same base name can also be ".."
// (filepath.Base("/x/..") == ".."), which is a path component the join
// downstream would follow out of the staging directory.
//
// Refusing is the right answer rather than mangling: renaming an operator's
// file silently would break the correspondence between what they asked for
// and what the lock records, and there is no name debark could invent that
// is more honest than saying which file it will not accept.
func poolFilenameForLocal(path string) (string, error) {
	name := sanitizeFilename(filepath.Base(path))
	if name == "" {
		return "", dferr.New(dferr.Usage,
			"fetch: %s: the file's own name cannot be used as a package filename (no separators, control characters, drive or device names, and at most 255 bytes)", path)
	}
	return name, nil
}
