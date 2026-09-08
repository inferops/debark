package fetch

import (
	"bytes"
	"testing"
)

// FuzzSniffDeb fuzzes the .deb sniffer against arbitrary bytes — exactly
// the untrusted input it exists to gate (of docs/threat-model.md: "a
// .deb-shaped file with attacker-chosen contents"). The invariant is tied
// to SniffDeb's own documented contract ("an ar archive whose first member
// is named debian-binary"): whenever it reports success, the input must
// actually have begun with the ar magic, because that is the one thing a
// caller downstream (fetcher.download) relies on this function to have
// confirmed before trusting the bytes as a .deb at all.
func FuzzSniffDeb(f *testing.F) {
	deb, err := BuildFixtureDeb(FixtureDeb{Package: "vlc", Version: "3.0.21-1build1"})
	if err == nil {
		f.Add(deb)
	}
	f.Add([]byte(arMagic))
	f.Add([]byte(arMagic + "debian-binary"))
	f.Add([]byte{})
	f.Add([]byte("not a deb at all"))
	f.Add([]byte("<html><body>404 not found</body></html>")) // the documented common case
	f.Add(bytes.Repeat([]byte{0}, 200))

	f.Fuzz(func(t *testing.T, data []byte) {
		err := SniffDeb(bytes.NewReader(data))
		if err == nil && !bytes.HasPrefix(data, []byte(arMagic)) {
			t.Fatalf("SniffDeb accepted input that does not even start with the ar magic %q: %q", arMagic, data)
		}
	})
}
