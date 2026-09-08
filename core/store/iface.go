// Package store is the content-addressed object store that makes repeat builds
// incremental: nothing already held is fetched again, and bundles are assembled
// from it by hardlink or copy.
//
// Layout: <root>/sha256/<first two hex chars>/<digest>, plus index.json mapping
// name_version_arch to a digest for human queries and prune decisions.
package store

import (
	"context"
	"io"
)

// Store is the object store contract.
type Store interface {
	// Root is the store's directory, for reporting.
	Root() string

	// Has reports whether an object is filed under digest, which must be a
	// lowercase hex SHA-256; anything else is not an address and is reported
	// absent. Presence is not evidence about content - use Open or
	// Materialise, which verify - so Has is only ever a hint about work
	// already done.
	Has(digest string) bool

	// Path returns the absolute path an object would have, or "" when digest
	// is not a store address. It does not imply the object exists.
	Path(digest string) string

	// Put copies everything r yields into the store and returns its digest and
	// size. The write is atomic: a partial object is never observable.
	Put(ctx context.Context, r io.Reader) (digest string, size int64, err error)

	// PutFile ingests a file. Implementations may move or hardlink instead of
	// copying when the source is on the same filesystem and moveOK is true;
	// with moveOK false the source is left untouched.
	PutFile(ctx context.Context, path string, moveOK bool) (digest string, size int64, err error)

	// Open reads an object, hashing it as it is read: a reader taken to EOF
	// fails with a Verification error rather than handing back content that
	// does not match the address it was stored under.
	Open(digest string) (io.ReadCloser, error)

	// Materialise places the object at dest, creating parent directories. It
	// hardlinks when possible and copies otherwise; dest is never a symlink to
	// the store, because a bundle must survive being copied to other media.
	// It verifies what it wrote against digest and fails with a Verification
	// error on any mismatch, leaving nothing at dest.
	Materialise(digest, dest string) error

	// Index returns the metadata index.
	Index() (Index, error)

	// Record notes package identity for an object already in the store, so
	// store ls and prune can work without opening every .deb.
	Record(entry Entry) error

	// GC removes objects that keep returns false for, and reports what it
	// freed. keep is called once per object.
	GC(ctx context.Context, keep func(digest string) bool) (GCStats, error)
}

// Entry is one object's package identity.
type Entry struct {
	Digest  string `json:"digest"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
	Size    int64  `json:"size"`
	// Filename is the .deb base name.
	Filename string `json:"filename"`
	// AddedAt is when the object entered the store.
	AddedAt string `json:"added_at"`
	// UserSupplied marks objects that came from an operator-provided file or
	// URL. They are never pruned from a bundle and never garbage-collected
	// without an explicit request.
	UserSupplied bool `json:"user_supplied,omitempty"`
}

// Index is the store's metadata index.
type Index struct {
	SchemaVersion string  `json:"schema_version"`
	Entries       []Entry `json:"entries"`
}

// IndexSchemaVersion is the store index schema. It is internal to the store —
// it never crosses the gap — but it is versioned all the same.
const IndexSchemaVersion = "debark.storeindex/v1"

// IndexFileName is the index's name inside the store root.
const IndexFileName = "index.json"

// GCStats is what a GC run did.
type GCStats struct {
	Removed        int   `json:"removed"`
	Kept           int   `json:"kept"`
	BytesFreed     int64 `json:"bytes_freed"`
	BytesRemaining int64 `json:"bytes_remaining"`
}
