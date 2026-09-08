package fetch

// ParseListFile's syntax matches the proven Bash prototype's
// download-packages.sh / packages.txt exactly for every line it already
// handles. It adds exactly one thing the prototype does not
// have: a trailing "sha256=<hex>" token on a URL line. Anything else beyond
// the first field, or a recognised prefix with nothing after it, or a URL
// that does not parse as http(s), is rejected as ambiguous rather than
// silently ignored the way the prototype's `awk '{print $1}'` would.

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
)

type lineKind int

const (
	kindApt lineKind = iota
	kindURL
	kindFile
	// kindReject is an entry that is syntactically a bare word but must not
	// become any kind of input. See classifyEntry.
	kindReject
)

// classifyEntry applies the prototype's case statement, in the same order:
// http(s):// or url: is a URL, file: or a bare path ending in .deb is a
// local file, apt: or anything else is an apt package name. Order matters:
// apt: is checked before the .deb suffix so "apt:something.deb" (unlikely,
// but unambiguous once you see the prefix) is a package, not a file.
//
// The one departure from the prototype is kindReject: a string the case
// statement would have fallen through to "package name" but that must not
// become one. See classifyPackage.
func classifyEntry(entry string) (kind lineKind, payload string) {
	switch {
	case strings.HasPrefix(entry, "https://"), strings.HasPrefix(entry, "http://"):
		return kindURL, entry
	case strings.HasPrefix(entry, "url:"):
		return kindURL, strings.TrimPrefix(entry, "url:")
	case strings.HasPrefix(entry, "file:"):
		return kindFile, strings.TrimPrefix(entry, "file:")
	case strings.HasPrefix(entry, "apt:"):
		return classifyPackage(strings.TrimPrefix(entry, "apt:"))
	case strings.HasSuffix(entry, ".deb"):
		return kindFile, entry
	default:
		return classifyPackage(entry)
	}
}

// classifyPackage is the "or anything else is an apt package name" arm of
// classifyEntry, with the one thing a package name must not be filtered out.
//
// SECURITY. A package name from this file becomes an operand in the argv of
// the apt invocation that resolves the closure. argv has no separate channel
// for "this is data": a leading "-" makes the very same string an option, so
// a list line reading "--allow-unauthenticated", or "apt:-o" followed by
// nothing at all, turned an untrusted list file into a way to change how apt
// itself behaves on the builder — including switching off the signature
// checking that makes apt the trustworthy oracle in the first place. The
// resolver's own argv construction is being hardened separately; refusing to
// call an option-shaped string a package name is this parser's half, and the
// two are independent (either alone closes the hole, and neither is a reason
// to skip the other).
//
// Control characters are refused on the same argv reasoning plus a second
// one: a package name also reaches deb822 records and evidence strings,
// where a control byte is at best unreadable and at worst a field
// terminator. strings.Fields has already removed whitespace, but not ESC,
// NUL or DEL.
//
// Nothing else is filtered. A legitimate apt operand carries "=" version
// pins, ":" architecture qualifiers and "/" release suffixes, all of which
// are documented and none of which are option-shaped.
func classifyPackage(name string) (lineKind, string) {
	if strings.HasPrefix(name, "-") {
		return kindReject, name
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return kindReject, name
		}
	}
	return kindApt, name
}

func parseListFile(path string) (buildjob.Inputs, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return buildjob.Inputs{}, dferr.New(dferr.Usage, "fetch: list file not found: %s", path)
		}
		return buildjob.Inputs{}, dferr.Wrap(dferr.Usage, err, "fetch: reading %s", path)
	}

	// Match the prototype's `tr -d '\r'`: every carriage return is deleted
	// outright (not just ones immediately before a newline), so a
	// Windows-edited list behaves exactly as it does in the shell version.
	content := strings.ReplaceAll(string(raw), "\r", "")
	listDir := filepath.Dir(path)

	var inputs buildjob.Inputs
	for i, line := range strings.Split(content, "\n") {
		lineNo := i + 1

		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		entry := fields[0]
		extra := fields[1:]

		kind, payload := classifyEntry(entry)
		if kind == kindReject {
			return buildjob.Inputs{}, dferr.New(dferr.Usage,
				"fetch: %s:%d: %q is not usable as an apt package name (a name may not start with \"-\", which apt would read as an option, or contain a control character)",
				path, lineNo, payload)
		}
		if payload == "" {
			return buildjob.Inputs{}, dferr.New(dferr.Usage,
				"fetch: %s:%d: %q has no value after its prefix", path, lineNo, entry)
		}

		var sha256 string
		if len(extra) > 0 {
			if kind != kindURL || len(extra) > 1 {
				return buildjob.Inputs{}, dferr.New(dferr.Usage,
					"fetch: %s:%d: unexpected extra text after %q (only a trailing sha256=<hex> is allowed, and only on a URL line)",
					path, lineNo, entry)
			}
			hexPart, ok := strings.CutPrefix(extra[0], "sha256=")
			hexPart = strings.ToLower(hexPart)
			if !ok || !digest.Valid(hexPart) {
				return buildjob.Inputs{}, dferr.New(dferr.Usage,
					"fetch: %s:%d: malformed digest %q (want sha256=<64 lowercase hex characters>)",
					path, lineNo, extra[0])
			}
			sha256 = hexPart
		}

		switch kind {
		case kindApt:
			inputs.Packages = append(inputs.Packages, payload)

		case kindURL:
			u, err := url.Parse(payload)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return buildjob.Inputs{}, dferr.New(dferr.Usage,
					"fetch: %s:%d: not a valid http(s) URL: %q", path, lineNo, payload)
			}
			inputs.URLs = append(inputs.URLs, buildjob.URLInput{URL: payload, SHA256: sha256})

		case kindFile:
			resolved := payload
			if !looksAbsolute(payload) {
				resolved = filepath.Join(listDir, payload)
			}
			inputs.Files = append(inputs.Files, resolved)
		}
	}

	inputs.ListFiles = []string{path}
	return inputs, nil
}

// looksAbsolute reports whether p should be left untouched rather than
// joined against the list file's directory. It checks the running OS's own
// notion of absolute (filepath.IsAbs) plus the two conventions a
// packages.txt might contain regardless of which OS wrote it versus which
// OS is running debark: a Unix-style leading "/", and a Windows drive or
// UNC prefix. Without this, a list authored on Linux and parsed on Windows
// (or the reverse) would mishandle an author's absolute path by joining it
// onto the list directory instead of using it as-is.
func looksAbsolute(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	if strings.HasPrefix(p, "/") {
		return true
	}
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	if len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0]) {
		return true
	}
	return false
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}
