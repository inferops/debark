package fetch

import "testing"

// FuzzParseControlStanza fuzzes the deb822 control-stanza reader with
// arbitrary text — a vendor .deb's control file is exactly the untrusted,
// attacker-shaped input F1 (docs/security/review-findings.md) was found
// through. The invariant is parseControlStanza's own explicit, documented
// contract: it refuses to return success unless Package, Version and
// Architecture are all present. A parser that ever returned an empty
// Package on "success" is exactly the class of bug that let an
// unsanitised, attacker-chosen value flow downstream into a pool path.
func FuzzParseControlStanza(f *testing.F) {
	f.Add("Package: vlc\nVersion: 3.0.21-1build1\nArchitecture: amd64\n")
	f.Add("Package: acme-agent\nVersion: 1.0\nArchitecture: amd64\nDepends: libc6 (>= 2.31)\n")
	f.Add("")
	f.Add("Package: only-package\n")
	f.Add("not a control file at all, just text\nwith\nno colons on some lines")
	f.Add("Package:\nVersion:\nArchitecture:\n")
	f.Add("Package: p\n Continued\nVersion: 1\nArchitecture: amd64\n")
	f.Add("Description: a multi-line description\n one\n .\n two\nPackage: p\nVersion: 1\nArchitecture: amd64\n")
	f.Add(": leading colon, no key\nPackage: p\nVersion: 1\nArchitecture: amd64\n")

	f.Fuzz(func(t *testing.T, text string) {
		info, err := parseControlStanza(text)
		if err != nil {
			return
		}
		if info.Package == "" || info.Version == "" || info.Architecture == "" {
			t.Fatalf("parseControlStanza returned success with a required field empty: %+v (input %q)", info, text)
		}
	})
}
