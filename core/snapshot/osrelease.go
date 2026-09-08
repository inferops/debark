package snapshot

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// parseOSRelease parses the shell-assignment KEY=VALUE format of
// /etc/os-release (os-release(5)): unquoted, single- or double-quoted values,
// backslash escapes inside double quotes, "#" comment lines, blank lines
// ignored. Malformed lines are skipped rather than failing the parse -- this
// file is captured verbatim regardless, this is only for the handful of
// fields the snapshot schema pulls out of it.
func parseOSRelease(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || !isShellIdent(key) {
			continue
		}
		out[key] = unquoteOSReleaseValue(strings.TrimSpace(val))
	}
	return out
}

func isShellIdent(s string) bool {
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return s != ""
}

// unquoteOSReleaseValue strips the quoting os-release(5) allows. It tries
// Go's own shell-ish unquoting rules first (strconv.Unquote handles the
// double-quoted, backslash-escaped case exactly), and falls back to plain
// single-quote and bare-word handling.
func unquoteOSReleaseValue(v string) string {
	if v == "" {
		return v
	}
	if v[0] == '"' {
		if u, err := strconv.Unquote(v); err == nil {
			return u
		}
		// Malformed escape: strip the surrounding quotes only.
		return strings.Trim(v, `"`)
	}
	if v[0] == '\'' && len(v) >= 2 && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	return v
}
