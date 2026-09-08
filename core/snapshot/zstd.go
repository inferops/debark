package snapshot

import (
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// zstdCompress returns data compressed as a single zstd frame.
//
// Concurrency is pinned to 1: klauspost/compress's encoder otherwise splits
// large input across GOMAXPROCS goroutines, and while the decompressed
// result is always correct, the exact block boundaries it chooses can then
// depend on the host's core count and scheduler -- fine for a general-purpose
// compressor, not for a byte-identical-archive determinism guarantee ("two
// builds of the same request must produce byte-identical manifests"). A
// snapshot is at most a few tens of megabytes; single-threaded encoding is
// not a performance concern at that size.
func zstdCompress(data []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("snapshot: zstd encoder: %w", err)
	}
	// EncodeAll returns a complete, self-contained frame; Close here only
	// releases the encoder's internal buffers and goroutines, it does not
	// flush anything into the result. There is no output to truncate and
	// nothing to report, so the error is discarded explicitly. (Contrast
	// core/bundle/tar.go, where the same type's Close DOES flush a stream and
	// the error is checked.)
	defer func() { _ = enc.Close() }()
	return enc.EncodeAll(data, make([]byte, 0, len(data)/2+64)), nil
}

// maxSnapshotDecompressedSize bounds how much memory a single call to
// zstdDecompress may allocate. A real snapshot archive is at most a few
// tens of megabytes (zstdCompress's own doc above); this is set an order of
// magnitude above that, comfortably covering even an unusually large
// dpkg_status or keyring set, while still refusing a crafted, highly
// compressed snapshot.tar.zst long before it could exhaust the builder's
// memory (F6, docs/security/review-findings.md — the same "impose a sane
// limit on decompression of untrusted input" reasoning that finding asks to
// be applied everywhere debark decompresses data it has not yet
// verified, not only at the one call site it names). A snapshot is
// untrusted at this point in Open(): doValidate and the digest check that
// follow are what decide whether it can be trusted, and both need this
// decompression to have happened first.
const maxSnapshotDecompressedSize = 512 << 20 // 512 MiB

// zstdDecompress returns the decompressed content of a single zstd frame.
// zstd.WithDecoderMaxMemory makes the decoder itself refuse to produce more
// than maxSnapshotDecompressedSize bytes (returning an error) rather than
// silently truncating a stream that decodes to more than that -- exactly
// the "refuse, don't silently truncate" behaviour a decompression-bomb
// defence needs, and the klauspost/compress option built for it ("This can
// be used to control memory usage of potentially hostile content.").
func zstdDecompress(data []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxSnapshotDecompressedSize))
	if err != nil {
		return nil, fmt.Errorf("snapshot: zstd decoder: %w", err)
	}
	defer dec.Close()
	out, err := dec.DecodeAll(data, nil)
	if err != nil {
		return nil, fmt.Errorf("snapshot: zstd decode: %w", err)
	}
	return out, nil
}

// zstdMagic is the four-byte magic number that begins every zstd frame
// (RFC 8878 §3.1.1: 0xFD2FB528, little-endian on the wire).
var zstdMagic = [4]byte{0x28, 0xB5, 0x2F, 0xFD}

// looksLikeZstd reports whether data begins with a zstd frame header.
//
// This is a "is this the kind of file I was asked for at all" check, not an
// integrity check: it is what lets Open tell "you pointed me at a .deb, a
// PNG or a text file" (an operator mistake — dferr.Usage) apart from "this
// IS a snapshot archive and something about it is wrong" (dferr.Verification,
// the class whose whole meaning is a signature, digest or metadata
// mismatch). Reporting the first as the second is not merely imprecise: it
// tells an operator who mis-clicked in a file picker that their media may
// have been tampered with.
func looksLikeZstd(data []byte) bool {
	return len(data) >= len(zstdMagic) && [4]byte(data[:4]) == zstdMagic
}
