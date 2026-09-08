package fetch

import (
	"net/url"
	"strings"
)

// RedactURL returns raw with any userinfo (embedded credentials, the
// "https://user:secret@host/..." form RFC 3986 §3.2.1 reserves for exactly
// this) and any query string replaced by a visible marker, so the result is
// safe to write into a persisted artefact (evidence.json, lock.json) or an
// evidence event. It is the one place debark decides how a
// possibly-credentialed URL is allowed to reach a record that ships inside
// the bundle and crosses the air gap: core/evidence's own contract requires
// every value there to "never contain a secret" (core/evidence/types.go),
// and lock.Origin.URI's own doc comment promises "the archive URI or the
// vendor URL" to whoever reads lock.json years later.
//
// Scheme, host and path survive unredacted, because they are what still
// makes the record useful to that reader ("which host, which file") once
// the credential is gone. The marker ("REDACTED" in place of the userinfo,
// and again in place of a whole query string) makes the redaction visible,
// on purpose: a URL that silently lost its userinfo would read as though it
// never had one, which is a different, weaker claim than "a credential was
// here and has been removed."
//
// A query string is dropped in its entirety, not scrubbed parameter by
// parameter, and this is a deliberate, documented trade-off, not an
// oversight: a presigned URL's credential (an AWS "X-Amz-Signature", an
// Azure SAS "sig", a bare vendor "token=") lives in a query parameter whose
// *name* is not standardised across vendors, so it cannot be reliably told
// apart from a harmless one (a version pin, a mirror id) by pattern matching
// alone. Guessing which parameter is the secret risks leaving a real
// credential in a shipped artefact under the false belief that it was
// redacted; dropping the whole query string cannot make that mistake. The
// cost is that any non-secret information riding along in the query string
// is lost too — accepted, because "redacted more than strictly necessary"
// is a safe failure mode here and "redacted less than necessary" is not.
func RedactURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// url.Parse could not take this apart, so the structured redaction
		// below cannot run. Returning raw here — which is what this
		// function used to do — is a credential leak, not a safe default:
		// url.Parse rejects a URL whose *path* holds a control character,
		// a space or a bad percent-escape, and reports Host == "" for
		// "https://user:pw@" and "http://user:pw@/path", yet every one of
		// those forms can still carry a real userinfo credential, and
		// every one of them reaches this function through Fetch's own
		// "invalid URL %q" message. A string that cannot be parsed is
		// exactly the string an attacker (or a typo) supplies to route a
		// password around the redactor.
		//
		// So fall back to a textual scrub of the one region RFC 3986
		// allows userinfo to occupy — the authority, between "://" and the
		// first "/", "?" or "#" — plus the query string. It is coarser
		// than the parsed path and deliberately so: it can only ever
		// remove more than necessary.
		return redactUnparsed(raw)
	}

	changed := false
	if u.User != nil {
		u.User = url.User("REDACTED")
		changed = true
	}
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
		changed = true
	}
	if u.Fragment != "" {
		u.Fragment = ""
		u.RawFragment = ""
		changed = true
	}
	if !changed {
		return raw
	}
	return u.String()
}

// redactUnparsed is RedactURL's fallback for a string url.Parse refused, or
// parsed into something with no host. It works on bytes rather than on a
// parsed URL, so it makes no assumption the parser already rejected.
//
// A string with no "://" has no authority for this function to point at, and
// splits in two. With no scheme either it is the documented local-path case
// (Fetched.URL carries a path, not a URL, for a FromLocalFile result) and is
// returned untouched. With a scheme but no "//" — "https:/user:pw@host/a.deb",
// a single-slash typo and equally the shape chosen by someone who noticed
// that a redactor keying on "://" does not look at it — the string is not a
// URL anything downstream can use, so nothing needs its host or path
// preserved; when it also holds an "@" the whole remainder goes.
func redactUnparsed(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		if n := schemePrefixLen(raw); n > 0 && strings.ContainsRune(raw[n:], '@') {
			return raw[:n] + "REDACTED"
		}
		return raw
	}
	authStart := i + len("://")

	// The authority ends at the first delimiter that can follow it. Anything
	// past that point is path/query/fragment, where a "@" is an ordinary
	// character and not a credential.
	authEnd := len(raw)
	if j := strings.IndexAny(raw[authStart:], "/?#"); j >= 0 {
		authEnd = authStart + j
	}

	out := raw[:authStart]
	auth := raw[authStart:authEnd]
	// RFC 3986 §3.2: userinfo runs to the LAST "@" in the authority, so a
	// password containing "@" cannot shorten the region that gets removed.
	if at := strings.LastIndexByte(auth, '@'); at >= 0 {
		auth = "REDACTED" + auth[at:]
	}
	out += auth

	rest := raw[authEnd:]
	// Drop the fragment first so a "?" hiding inside it is not mistaken for
	// the start of a query string.
	if h := strings.IndexByte(rest, '#'); h >= 0 {
		rest = rest[:h]
	}
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		// Whole-query removal, for the reason RedactURL's doc gives: which
		// parameter holds a presigned credential cannot be told from its
		// name.
		rest = rest[:q] + "?REDACTED"
	}
	return out + rest
}

// schemePrefixLen returns the length of raw's leading "scheme:", colon
// included, or 0 if raw does not begin with an RFC 3986 scheme.
//
// A one-character scheme is deliberately not one. A Windows drive reference
// ("C:" followed by a backslash and a path) must come back unchanged, which
// is a documented promise of RedactURL, and no scheme this project fetches is
// a single letter.
func schemePrefixLen(raw string) int {
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == ':' {
			if i < 2 {
				return 0
			}
			return i + 1
		}
		if i == 0 && !isASCIILetter(c) {
			return 0
		}
		// RFC 3986: scheme = ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ).
		// Written as "none of the permitted classes" rather than the
		// De Morgan'd inequality staticcheck's QF1001 would produce, so the
		// line reads as the grammar it implements.
		if !isASCIILetter(c) && !isASCIIDigit(c) && c != '+' && c != '-' && c != '.' {
			return 0
		}
	}
	return 0
}

// RedactMessage returns msg with every URL-shaped run of characters inside
// it passed through RedactURL. It exists because RedactURL is only ever
// applied to a URL this package formats deliberately, and that is not the
// only way a URL reaches a message: net/http wraps every transport failure
// in a *url.Error, whose Error() prints the request URL verbatim,
// credentials and query string included. So
//
//	dferr.Wrap(dferr.Incomplete, err, "fetch: %s: request failed", safeURL)
//
// produces a message whose first half is redacted and whose second half —
// the wrapped cause — is not:
//
//	fetch: https://REDACTED@vendor.example/a.deb?REDACTED: request failed:
//	Get "https://user:s3cr3t@vendor.example/a.deb?token=ABC": dial tcp: ...
//
// That string reached the "error" attribute of a warn-level input.external
// event, which is written into evidence.json, which is covered by the
// manifest and ships inside the bundle across the air gap — exactly what
// core/evidence's contract ("a value there must never contain a secret")
// and Fetched.URL's own doc comment ("it must already be safe by the time
// it leaves this package") forbid.
//
// Scanning prose for URLs cannot be exact, so the bias is the same one
// RedactURL documents: err on the side of removing more. A token runs from
// its scheme up to the first character that cannot appear in a URL at all —
// whitespace, a quote, an angle bracket, a backslash, or one of RFC 3986's
// excluded characters. Deliberately NOT boundaries: ",", ";", ":", "." and
// ")". Each of them can legitimately appear in a query string, and ending a
// token early is the one mistake that leaks: RedactURL would scrub the part
// it was given and this function would then copy the credentialed remainder
// through untouched. Over-long tokens only ever cause harmless extra
// redaction, because RedactURL drops a query string whole in any case.
func RedactMessage(msg string) string {
	if !strings.Contains(msg, "://") {
		return msg
	}
	var b strings.Builder
	b.Grow(len(msg))
	for i := 0; i < len(msg); {
		j := strings.Index(msg[i:], "://")
		if j < 0 {
			b.WriteString(msg[i:])
			break
		}
		sep := i + j

		// Walk back over the scheme. RFC 3986: scheme = ALPHA *( ALPHA /
		// DIGIT / "+" / "-" / "." ), so the first character must be a
		// letter; skip forward over any leading non-letter the walk-back
		// picked up rather than handing RedactURL something it will refuse.
		start := sep
		for start > i && isSchemeByte(msg[start-1]) {
			start--
		}
		for start < sep && !isASCIILetter(msg[start]) {
			start++
		}
		if start == sep {
			// "://" with no scheme in front of it: not a URL, and nothing
			// to redact. Copy it and carry on past it.
			b.WriteString(msg[i : sep+len("://")])
			i = sep + len("://")
			continue
		}

		end := sep + len("://")
		for end < len(msg) && !isURLBoundary(msg[end]) {
			end++
		}
		b.WriteString(msg[i:start])
		b.WriteString(RedactURL(msg[start:end]))
		i = end
	}
	return b.String()
}

// isSchemeByte reports whether b may appear in an RFC 3986 scheme.
func isSchemeByte(b byte) bool {
	return isASCIILetter(b) || isASCIIDigit(b) || b == '+' || b == '-' || b == '.'
}

// isURLBoundary reports whether b definitely ends a URL that was embedded in
// a larger message. See RedactMessage for why this set is deliberately
// small.
func isURLBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	case '"', '\'', '`':
		return true
	case '<', '>', '\\', '^', '|', '{', '}':
		return true
	}
	return false
}

// redactedError presents a cause's message with every URL inside it
// redacted, while leaving errors.Is and errors.As — and therefore
// dferr.ClassOf, which looks for the OUTERMOST *dferr.Error in the chain —
// able to walk straight through to the original error. Wrapping is what
// makes that possible: rebuilding the *dferr.Error around a scrubbed cause
// would fix only the depth it was applied at and would silently miss a
// credential wrapped any deeper, whereas the message is the one thing every
// consumer actually reads.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }

func (e *redactedError) Unwrap() error { return e.err }

// redactErrorMessage returns err presenting a message with every URL in it
// passed through RedactURL, or err itself when there was nothing to redact.
// It is applied once, at this package's public boundary, rather than at the
// dozen places an error is constructed: a transport failure's credential
// arrives inside a cause this package did not format and cannot anticipate,
// so the only reliable place to catch it is on the way out.
func redactErrorMessage(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	safe := RedactMessage(msg)
	if safe == msg {
		return err
	}
	return &redactedError{msg: safe, err: err}
}
