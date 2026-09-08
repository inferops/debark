package sbom

import (
	"fmt"
	"strings"
)

// PURL builds a package-url (https://github.com/package-url/purl-spec) for a
// Debian-family package:
//
//	pkg:deb/<distro>/<name>@<version>?arch=<arch>
//
// distro is the lowercase distro id (debian, ubuntu); name, version and arch
// are percent-encoded per the purl spec, since Debian versions routinely
// contain characters ('~', '+', ':' for an epoch) that are not valid in a URL
// path segment or query value.
func PURL(distro, name, version, arch string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("sbom: purl: empty package name")
	}
	if distro == "" {
		distro = "debian" // the purl "deb" type defaults its namespace to Debian per the purl-spec registry
	}
	var b strings.Builder
	b.WriteString("pkg:deb/")
	b.WriteString(purlEscape(strings.ToLower(distro)))
	b.WriteByte('/')
	b.WriteString(purlEscape(name))
	if version != "" {
		b.WriteByte('@')
		b.WriteString(purlEscape(version))
	}
	if arch != "" {
		b.WriteString("?arch=")
		b.WriteString(purlEscape(arch))
	}
	return b.String(), nil
}

// purlUnreserved is the set of bytes the purl spec leaves unescaped in a
// component: unreserved URI characters (RFC 3986) plus the small set purl
// itself carves out.
func purlUnreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '-', '_', '.', '~':
		return true
	}
	return false
}

// purlEscape percent-encodes every byte outside the unreserved set, uppercase
// hex, matching RFC 3986's pct-encoding (which is what the purl spec asks
// implementations to apply to each component).
func purlEscape(s string) string {
	needsEscape := false
	for i := 0; i < len(s); i++ {
		if !purlUnreserved(s[i]) {
			needsEscape = true
			break
		}
	}
	if !needsEscape {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		if purlUnreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}
