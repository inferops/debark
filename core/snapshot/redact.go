package snapshot

import (
	"regexp"
	"sort"
	"strings"

	"github.com/inferops/debark/core/digest"
)

// Redact removes the fields named by kinds from a loaded snapshot, in place,
// and records them in Redactions.
//
// Each kind is applied unconditionally, whether or not it finds anything to
// remove: asking for RedactMachineID is asking for the *policy* "treat this
// as a machine-id-less snapshot" (which flips PhasedPolicyFor's answer, see
// ADR-006) even on the rare snapshot that had no machine id to begin with, so
// the kind is always recorded. Per-file File.Redacted is different: it is
// only set true on a file whose bytes actually changed.
func doRedact(s *Snapshot, fs *FileSet, kinds ...string) {
	if s == nil || len(kinds) == 0 {
		return
	}
	applied := map[string]bool{}
	for _, k := range s.Redactions {
		applied[k] = true
	}
	for _, kind := range kinds {
		switch kind {
		case RedactMachineID:
			s.Target.MachineID = ""
		case RedactLabels:
			s.Labels = nil
		case RedactProxies:
			if fs != nil {
				redactProxies(s, fs)
			}
		default:
			continue // an unrecognised kind is a caller bug; don't claim it was applied
		}
		applied[kind] = true
	}
	out := make([]string, 0, len(applied))
	for k := range applied {
		out = append(out, k)
	}
	sort.Strings(out)
	s.Redactions = out
}

// redactProxies scans every captured apt.conf file for proxy settings and
// rewrites the ones that carry something worth removing.
func redactProxies(s *Snapshot, fs *FileSet) {
	for i := range s.APT.Conf {
		f := &s.APT.Conf[i]
		data, ok := fs.Bytes[f.ArchivePath]
		if !ok {
			continue
		}
		out, changed := redactProxyBytes(data)
		if !changed {
			continue
		}
		fs.Bytes[f.ArchivePath] = out
		f.Size = int64(len(out))
		f.SHA256 = digest.Bytes(out)
		f.Redacted = true
	}
}

// proxyDirectiveRE matches one apt.conf(5) "Acquire::<scheme>::Proxy[::host]
// value;" statement. A proxy directive is always a single-line scalar
// assignment, never a block, so a per-line match is exact, not a heuristic.
//
// Groups: 1 leading whitespace + "Acquire::scheme::Proxy", 2 scheme word
// (nested inside 1), 3 optional "::host" suffix, 4 whitespace before the
// value, 5 opening quote (or empty), 6 the value, 7 closing quote (or
// empty), 8 the trailing ";" and anything after it on the line.
var proxyDirectiveRE = regexp.MustCompile(
	`(?im)^([ \t]*Acquire::([A-Za-z0-9_]+)::Proxy)(::[^\s"{};]+)?([ \t]+)("?)([^;"\r\n]*?)("?)([ \t]*;.*)$`,
)

// credentialValueRE finds any quoted "scheme://user:pass@host..." value
// anywhere in the file, independent of which key it is assigned to. It is
// the "any http_proxy-style value containing credentials" half of the
// contract, covering a per-host proxy override or any other creatively
// named key that ends up carrying a credentialed URL; proxyDirectiveRE above
// already handles the two directives the contract names explicitly.
var credentialValueRE = regexp.MustCompile(`"([a-zA-Z][a-zA-Z0-9+.-]*://[^"@]*@[^"]*)"`)

// redactProxyBytes strips proxy credentials, and the two named blanket
// directives, from one apt.conf-syntax file's raw bytes -- preserving
// everything else (comments, formatting, unrelated keys) exactly, since this
// is meant to remove secrets, not reformat the file.
func redactProxyBytes(data []byte) ([]byte, bool) {
	changed := false

	out := proxyDirectiveRE.ReplaceAllFunc(data, func(m []byte) []byte {
		sub := proxyDirectiveRE.FindSubmatch(m)
		prefix, scheme, hostSuffix := sub[1], sub[2], sub[3]
		ws, openq, value, closeq, tail := sub[4], sub[5], sub[6], sub[7], sub[8]

		blanket := len(hostSuffix) == 0 &&
			(strings.EqualFold(string(scheme), "http") || strings.EqualFold(string(scheme), "https"))

		v := string(value)
		if blanket {
			t := strings.TrimSpace(v)
			if strings.EqualFold(t, "DIRECT") || t == "" {
				return m // an explicit no-proxy override carries no secret; leave it alone
			}
			changed = true
			return joinProxyLine(prefix, hostSuffix, ws, openq, []byte("REDACTED"), closeq, tail)
		}
		if nv, ok := redactCredentials(v); ok {
			changed = true
			return joinProxyLine(prefix, hostSuffix, ws, openq, []byte(nv), closeq, tail)
		}
		return m
	})

	out = credentialValueRE.ReplaceAllFunc(out, func(m []byte) []byte {
		sub := credentialValueRE.FindSubmatch(m)
		if nv, ok := redactCredentials(string(sub[1])); ok {
			changed = true
			return []byte(`"` + nv + `"`)
		}
		return m
	})

	return out, changed
}

func joinProxyLine(prefix, hostSuffix, ws, openq, value, closeq, tail []byte) []byte {
	out := make([]byte, 0, len(prefix)+len(hostSuffix)+len(ws)+len(openq)+len(value)+len(closeq)+len(tail))
	out = append(out, prefix...)
	out = append(out, hostSuffix...)
	out = append(out, ws...)
	out = append(out, openq...)
	out = append(out, value...)
	out = append(out, closeq...)
	out = append(out, tail...)
	return out
}

// redactCredentials replaces the userinfo of a "scheme://user:pass@host..."
// value with a fixed marker, leaving the host, port and path -- not secret
// -- intact. ok is false when v carries no such pattern (including an empty
// userinfo, "scheme://@host", which has nothing to redact).
func redactCredentials(v string) (string, bool) {
	i := strings.Index(v, "://")
	if i < 0 {
		return v, false
	}
	rest := v[i+3:]
	at := strings.IndexByte(rest, '@')
	if at <= 0 {
		return v, false
	}
	return v[:i+3] + "REDACTED@" + rest[at+1:], true
}
