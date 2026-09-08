package snapshot

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// buildFuzzTarBytes is a tiny local helper (not the test-only
// mustWriteTarEntryRaw, which needs a *testing.T and so cannot run at
// FuzzXxx(f *testing.F) seed-registration time) for building a
// structurally simple, well-formed tar as fuzzer seed material.
func buildFuzzTarBytes(entries map[string][]byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range entries {
		hdr := &tar.Header{Name: name, Size: int64(len(data)), Mode: 0o644, Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			panic(err)
		}
		if _, err := tw.Write(data); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// FuzzExtractTar fuzzes the snapshot archive reader's tar-extraction step
// with arbitrary (already "decompressed") tar bytes — this is untrusted
// input by design: a snapshot can be tampered with before it reaches the
// builder (docs/threat-model.md §3.2), and extractTar's whole job is to be
// the boundary that stops a crafted archive entry from writing outside its
// own extraction directory ("zip slip", per its own doc comment).
//
// The invariant is exactly that: a rejected (or accepted) archive must
// never cause a byte to land anywhere outside destDir. This is checked by
// diffing destDir's parent directory's entries before and after the call,
// rather than trusting extractTar's return value — a real path-traversal
// bug would show up as a new, unexpected sibling of destDir even if
// extractTar otherwise returned an error.
func FuzzExtractTar(f *testing.F) {
	f.Add(buildFuzzTarBytes(map[string][]byte{DocumentName: []byte(`{"schema_version":"debark.snapshot/v1"}`)}))
	f.Add(buildFuzzTarBytes(map[string][]byte{
		DocumentName:            []byte(`{"schema_version":"debark.snapshot/v1"}`),
		FilesDir + "/etc/hosts": []byte("127.0.0.1 localhost\n"),
	}))
	f.Add(buildFuzzTarBytes(map[string][]byte{FilesDir + "/../../../etc/passwd": []byte("evil")}))
	f.Add(buildFuzzTarBytes(map[string][]byte{FilesDir + "/a/../../evil": []byte("evil")}))
	f.Add(buildFuzzTarBytes(map[string][]byte{}))
	f.Add([]byte{})
	f.Add([]byte("not a tar file at all"))
	f.Add([]byte{0x1f, 0x8b, 0x08, 0x00}) // gzip magic, not tar — must not be mistaken for one

	// A tar entry whose Typeflag is a symlink/hardlink pointing outside the
	// tree: extractTar's own switch only special-cases tar.TypeReg
	// (everything else, including link types, is silently skipped — see
	// its "default: ignore" case), so this seed exercises that path too.
	{
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{
			Name: FilesDir + "/evil-link", Typeflag: tar.TypeSymlink, Linkname: "../../../etc/passwd",
		})
		_ = tw.Close()
		f.Add(buf.Bytes())
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		destDir := t.TempDir()
		parent := filepath.Dir(destDir)

		before, err := os.ReadDir(parent)
		if err != nil {
			t.Skip("cannot list parent of a fresh t.TempDir(); not this function's concern")
		}
		beforeNames := make(map[string]bool, len(before))
		for _, e := range before {
			beforeNames[e.Name()] = true
		}

		_, _ = extractTar(data, destDir) // an error is fine; a panic or an escape is not

		after, err := os.ReadDir(parent)
		if err != nil {
			t.Fatalf("listing parent of destDir after extractTar: %v", err)
		}
		for _, e := range after {
			if !beforeNames[e.Name()] {
				t.Fatalf("extractTar caused a new entry %q to appear outside destDir (in its parent) for input %q", e.Name(), data)
			}
		}
	})
}
