package snapshot

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzRedactProxyBytes fuzzes the apt.conf proxy scrubber. --redact is a
// promise about what does *not* cross the air gap: the operator is told that
// a snapshot carrying it has had its proxy credentials removed, and once the
// archive is on removable media there is no taking that back. The scrubber
// is two regular expressions over an attacker-influenced file, which is
// exactly the shape where an unanticipated input silently leaves the secret
// in place.
//
// Three invariants:
//
//  1. NOTHING THE SCRUBBER RECOGNISES SURVIVES IT. Re-running the two
//     detectors over the *output* must find no credential left to remove:
//     no userinfo other than the fixed "REDACTED" marker, and no blanket
//     http/https proxy still naming a proxy. This is the promise itself,
//     checked at the exit boundary rather than by re-deriving what the
//     answer should have been -- so a value the first pass mangled into a
//     still-credentialed shape is a failure, not a coincidence.
//
//  2. IT IS A FIXED POINT ON BYTES. redactProxyBytes(redactProxyBytes(x))
//     must equal redactProxyBytes(x). reapplyRedactions runs the scrubber
//     again over an already-scrubbed snapshot on every Open and decides,
//     purely by whether the bytes moved, whether to warn the operator that
//     "this document claimed redactions it had not applied; treat it as
//     hand-edited or tampered with". A scrubber that is not a fixed point
//     makes Open shout that at every honest redacted snapshot, which trains
//     the operator to ignore the one signal that matters.
//
//     Note the check is on BYTES, not on the reported changed flag: a value
//     that already reads "scheme://REDACTED@host" is re-matched and re-written
//     to itself, so a second pass can legitimately report changed while
//     producing identical output. reapplyRedactions compares bytes too
//     (bytes.Equal against what is on disk), so bytes are the contract.
//
//  3. NO CHANGE MEANS NO REWRITE. When changed is false the output must be
//     byte-identical to the input -- the scrubber's own doc promises it
//     "preserves everything else exactly (comments, formatting, unrelated
//     keys)", and a file it had no reason to touch is the clearest case of
//     that. It also matters concretely: redactProxies only recomputes a
//     File's size, digest and Redacted flag when changed is true.
func FuzzRedactProxyBytes(f *testing.F) {
	// Real captured apt.conf.d fixtures: the scrubber has to leave these
	// completely alone, and "leaves an honest file alone" is half the
	// contract.
	for _, p := range []string{
		filepath.Join("testdata", "recommends-false", "etc", "apt", "apt.conf.d", "99recommends"),
		filepath.Join("testdata", "ubuntu2404-deb822", "etc", "apt", "apt.conf.d", "70debconf"),
	} {
		if data, err := os.ReadFile(p); err == nil {
			f.Add(data)
		}
	}

	// The two directives the contract names explicitly, quoted and unquoted.
	f.Add([]byte(`Acquire::http::Proxy "http://user:secret@proxy.example.com:3128/";` + "\n"))
	f.Add([]byte(`Acquire::https::Proxy "https://proxy.example.com:3128/";` + "\n"))
	f.Add([]byte(`Acquire::http::Proxy http://user:secret@proxy:3128/;` + "\n"))
	// The two documented no-op cases: an explicit no-proxy override, and an
	// empty value. Both must come through untouched -- neither carries a
	// secret, and rewriting them would change apt's behaviour.
	f.Add([]byte(`Acquire::http::Proxy "DIRECT";` + "\n"))
	f.Add([]byte(`Acquire::http::Proxy "";` + "\n"))
	// A per-host override, which keeps its host but loses its credentials,
	// and a non-http scheme, which is not "blanket" and so takes the same
	// credentials-only path.
	f.Add([]byte(`Acquire::http::Proxy::internal.example.com "http://user:secret@internal-proxy:3128/";` + "\n"))
	f.Add([]byte(`Acquire::ftp::Proxy "ftp://bob:hunter2@ftp-proxy.example.com/";` + "\n"))
	// Already-scrubbed inputs: invariant 2's fixed point, seeded directly so
	// the seed corpus covers it even without fuzzing.
	f.Add([]byte(`Acquire::http::Proxy "REDACTED";` + "\n"))
	f.Add([]byte(`Acquire::http::Proxy::h "http://REDACTED@h:3128/";` + "\n"))
	// Empty userinfo: redactCredentials explicitly reports "nothing to
	// redact" here, so this value must survive verbatim.
	f.Add([]byte(`Acquire::http::Proxy::h "http://@host/";` + "\n"))
	// Formatting the two regexes have to navigate: comments after the
	// statement, block syntax around it, case variation, an unterminated
	// quote, and a value that reaches the end of the file with no ";" at all.
	f.Add([]byte("// leading comment\nAcquire::http::Proxy \"http://u:p@h/\"; // trailing\n/* block */\n"))
	f.Add([]byte("Acquire { http { Proxy \"http://u:p@h/\"; }; };\n"))
	f.Add([]byte(`acquire::HTTP::proxy   "http://u:p@h/"   ;  ` + "\n"))
	f.Add([]byte(`Acquire::http::Proxy "http://u:p@h/ ;` + "\n"))
	f.Add([]byte(`Acquire::http::Proxy "http://u:p@h/"`))
	// A credentialed URL under a key the contract does not name: the
	// "any creatively named key" half of credentialValueRE's job.
	f.Add([]byte(`Some::Other::Key "https://alice:pw@example.com/path?q=1";` + "\n"))
	// Hostile text shapes prior reviews found interesting in this package:
	// NUL, invalid UTF-8, a CR-only line ending, and a quoted value that
	// spans lines (legal for the regex, since its trailing class admits a
	// newline).
	f.Add([]byte("Acquire::http::Proxy \"http://u:p@h/\";\x00trailing\n"))
	f.Add([]byte("Acquire::http::Proxy \"http://\xff\xfe:p@h/\";\n"))
	f.Add([]byte("Acquire::http::Proxy \"http://u:p@h/\";\r"))
	f.Add([]byte("Key \"http://user\nspanning:p@host/\";\n"))
	f.Add([]byte(""))
	f.Add([]byte(`"://@"`))

	f.Fuzz(func(t *testing.T, data []byte) {
		out, changed := redactProxyBytes(data)

		if !changed && !bytes.Equal(out, data) {
			t.Fatalf("redactProxyBytes reported no change but rewrote the file:\nin:  %q\nout: %q", data, out)
		}

		if left := credentialsLeftIn(out); len(left) > 0 {
			t.Fatalf("redactProxyBytes left credential material its own detectors still find: %q\nin:  %q\nout: %q",
				left, data, out)
		}

		twice, _ := redactProxyBytes(out)
		if !bytes.Equal(twice, out) {
			t.Fatalf("redactProxyBytes is not a fixed point, so every honest re-Open would be warned "+
				"as tampered with:\nin:    %q\nonce:  %q\ntwice: %q", data, out, twice)
		}
	})
}

// credentialsLeftIn re-runs redactProxyBytes' own two detectors over an
// already-scrubbed file and reports whatever they still consider a secret:
// any userinfo that is not the fixed "REDACTED" marker (or empty, the
// documented nothing-to-redact case), and any blanket http/https proxy
// directive still naming a proxy rather than REDACTED, DIRECT or nothing.
//
// Using the scrubber's own regexes as the oracle is deliberate. An
// independent re-implementation of "what counts as a credential" would only
// test that two hand-written parsers agree; running the real detectors over
// the real output tests the thing the operator was actually promised -- that
// after --redact there is nothing left here that this code knows how to
// recognise as a secret.
func credentialsLeftIn(out []byte) []string {
	var left []string

	for _, m := range proxyDirectiveRE.FindAllSubmatch(out, -1) {
		scheme, hostSuffix, value := string(m[2]), m[3], strings.TrimSpace(string(m[6]))
		blanket := len(hostSuffix) == 0 && (strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https"))
		if blanket && value != "REDACTED" && !strings.EqualFold(value, "DIRECT") && value != "" {
			left = append(left, "blanket "+scheme+" proxy still set to "+value)
		}
		if _, ok := redactCredentials(string(m[6])); ok && userinfoOf(string(m[6])) != "REDACTED" {
			left = append(left, "proxy directive userinfo "+userinfoOf(string(m[6])))
		}
	}

	for _, m := range credentialValueRE.FindAllSubmatch(out, -1) {
		v := string(m[1])
		if _, ok := redactCredentials(v); ok && userinfoOf(v) != "REDACTED" {
			left = append(left, "quoted value userinfo "+userinfoOf(v))
		}
	}
	return left
}

// userinfoOf returns the "user:pass" part of a "scheme://user:pass@host"
// value, or "" when there is none. It mirrors redactCredentials' own
// scheme-then-first-@ split exactly, so the two can never disagree about
// where the secret was.
func userinfoOf(v string) string {
	i := strings.Index(v, "://")
	if i < 0 {
		return ""
	}
	rest := v[i+3:]
	at := strings.IndexByte(rest, '@')
	if at < 0 {
		return ""
	}
	return rest[:at]
}
