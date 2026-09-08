package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
)

// This file answers "why did this download fail?" with a value instead of a
// sentence.
//
// The reason was always knowable and never expressible: BuildResult carried
// fetch_failed[] as a list of bare URLs, so an unreachable host, an
// untrusted certificate and a digest mismatch reached a caller as one
// indistinguishable line. The reason existed only in the text of a
// warn-level input.external event, which meant a consumer that read the
// result document had to say "a download failed" and stop, and a consumer
// that wanted more had to correlate two streams by URL and then parse
// English.
//
// The classification is made where the failure happens, not by matching on
// the message afterwards. Message matching is how a reworded error silently
// reclassifies itself; a tag applied at the return statement cannot drift
// from the branch that produced it. reasonError carries the tag and nothing
// else -- the message, the class and the cause are all untouched, and Unwrap
// keeps errors.Is, errors.As and dferr.ClassOf (which looks for the
// outermost *dferr.Error in the chain) working straight through it.

// reasonError tags an error with the reason it happened.
type reasonError struct {
	reason buildjob.FetchFailureReason
	err    error
}

func (e *reasonError) Error() string { return e.err.Error() }

func (e *reasonError) Unwrap() error { return e.err }

// because tags err with reason. It returns nil for a nil error so it can sit
// directly in a return statement, and it never re-tags: the innermost
// classification is the one closest to what actually went wrong, and a
// caller wrapping an already-tagged error is not better informed than the
// site that produced it.
func because(reason buildjob.FetchFailureReason, err error) error {
	if err == nil {
		return nil
	}
	var already *reasonError
	if errors.As(err, &already) {
		return err
	}
	return &reasonError{reason: reason, err: err}
}

// ReasonOf classifies a fetch failure. It returns "" for a nil error and
// buildjob.ReasonOther for an error this package cannot account for.
//
// Errors this package produced carry their reason as a tag. Errors that
// arrive from outside it -- a caller-supplied http.Client, a store
// implementation, a Fetcher a test substituted -- carry nothing, so they are
// classified structurally from the standard library types that do describe
// themselves. Nothing here parses a message.
func ReasonOf(err error) buildjob.FetchFailureReason {
	if err == nil {
		return ""
	}
	var tagged *reasonError
	if errors.As(err, &tagged) {
		return tagged.reason
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return buildjob.ReasonCancelled
	case IsTLSVerificationError(err):
		return buildjob.ReasonTLSUntrusted
	case isTransportError(err):
		return buildjob.ReasonUnreachable
	}
	return buildjob.ReasonOther
}

// IsTLSVerificationError reports whether err is, at any depth, a failure to
// verify the far end's certificate.
//
// It is exported because it is the one classification a caller outside this
// package genuinely needs to make for itself: the same failure is
// environment-classified (and printed) on the snapshot from-base path, and
// docs/dev/error-catalogue.md's environment/tls-untrusted row exists for
// exactly that shape.
//
// Every listed type is a real thing crypto/tls and crypto/x509 return.
// tls.CertificateVerificationError is the wrapper Go has used since 1.20 and
// unwraps to one of the x509 errors; the x509 types are matched directly as
// well, because a caller-supplied VerifyPeerCertificate or a custom dialer
// can return one without the tls wrapper around it.
func IsTLSVerificationError(err error) bool {
	if err == nil {
		return false
	}
	var (
		verify   *tls.CertificateVerificationError
		unknown  x509.UnknownAuthorityError
		invalid  x509.CertificateInvalidError
		hostname x509.HostnameError
		roots    x509.SystemRootsError
	)
	return errors.As(err, &verify) ||
		errors.As(err, &unknown) ||
		errors.As(err, &invalid) ||
		errors.As(err, &hostname) ||
		errors.As(err, &roots)
}

// isTransportError reports whether err is a network-level failure: the far
// end was never reached, or the connection died before the body did.
//
// net.Error covers a dial refusal, a reset and a timeout; *net.DNSError and
// *net.AddrError cover a name or address that never resolved. io.EOF and
// io.ErrUnexpectedEOF are here because a truncated response body is a
// transport failure and reads as one to an operator, even though neither
// type is in package net.
func isTransportError(err error) bool {
	var (
		netErr  net.Error
		dnsErr  *net.DNSError
		addrErr *net.AddrError
		opErr   *net.OpError
	)
	return errors.As(err, &netErr) ||
		errors.As(err, &dnsErr) ||
		errors.As(err, &addrErr) ||
		errors.As(err, &opErr) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}
