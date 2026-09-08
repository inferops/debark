package snapshot

import (
	"bytes"
	"testing"
)

func TestZstdRoundTrip(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte(""),
		[]byte("hello, snapshot"),
		bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog "), 10000), // exercise multi-block input
	}
	for i, in := range cases {
		compressed, err := zstdCompress(in)
		if err != nil {
			t.Fatalf("case %d: compress: %v", i, err)
		}
		out, err := zstdDecompress(compressed)
		if err != nil {
			t.Fatalf("case %d: decompress: %v", i, err)
		}
		if !bytes.Equal(out, in) {
			t.Errorf("case %d: round trip mismatch (%d in, %d out)", i, len(in), len(out))
		}
	}
}

func TestZstdCompressDeterministic(t *testing.T) {
	in := bytes.Repeat([]byte("deterministic bytes please "), 5000)
	a, err := zstdCompress(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := zstdCompress(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("compressing the same input twice produced different output")
	}
}

func TestZstdMagicNumber(t *testing.T) {
	compressed, err := zstdCompress([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x28, 0xB5, 0x2F, 0xFD}
	if len(compressed) < 4 || !bytes.Equal(compressed[:4], want) {
		t.Errorf("output does not start with the zstd magic number: %x", compressed[:min(4, len(compressed))])
	}
}
