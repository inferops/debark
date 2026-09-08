package doctor

import (
	"bytes"
	"testing"
)

func TestParseArRoundTrip(t *testing.T) {
	want := []arMember{
		{Name: "debian-binary", Data: []byte("2.0\n")},
		{Name: "control.tar.gz", Data: []byte("not really gzip but ar doesn't care")},
		{Name: "data.tar.gz", Data: []byte("x")}, // odd length, exercises padding
	}
	raw := buildAr(want)

	got, err := parseAr(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parseAr: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d members, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name {
			t.Errorf("member %d name = %q, want %q", i, got[i].Name, want[i].Name)
		}
		if !bytes.Equal(got[i].Data, want[i].Data) {
			t.Errorf("member %d data = %q, want %q", i, got[i].Data, want[i].Data)
		}
	}
}

func TestParseArBadMagic(t *testing.T) {
	_, err := parseAr(bytes.NewReader([]byte("not an ar archive at all")))
	if err == nil {
		t.Fatal("expected an error for bad magic")
	}
}

func TestParseArTruncated(t *testing.T) {
	raw := buildAr([]arMember{{Name: "debian-binary", Data: []byte("2.0\n")}})
	_, err := parseAr(bytes.NewReader(raw[:len(raw)-2])) // cut off the last byte of data
	if err == nil {
		t.Fatal("expected an error for a truncated archive")
	}
}
