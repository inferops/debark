package cliadapter

import (
	"strings"

	"github.com/inferops/debark/core/fetch"
)

// RedactArgv returns a copy of argv with vendor-URL credentials removed, for
// showing a person and for putting on a clipboard.
//
// # Why this is not Command
//
// Command returns the exact argv a build runs, and it has to: the real adapter
// execs Command(spec)[1:], and two tests (TestInvokeCommandIsBuildArgv, and the
// drift check in events_test.go) exist to pin that the previewed command IS the
// executed one. Redacting inside Command would break builds outright; redacting
// inside BuildArgv would break the same property more quietly, by making the
// preview a description of a command nobody runs. So the exact argv stays
// exact, and this is a separate, explicit transform applied at the point where
// the argv stops being an instruction to a process and becomes text for a
// person.
//
// # What an operator is supposed to do with the command they are shown
//
// This is the question that decides the whole finding (docs/security-review.md
// §6.1), so it is answered here rather than assumed. The preview has two
// audiences and they want different strings:
//
//   - Read it. "What is this application about to run on my machine?" That is
//     rule 8's real purpose — the CLI is the complete interface and the GUI
//     must not be a black box — and a redacted command answers it completely.
//     Every flag, every input, the exact shape.
//   - Run it. Paste it into a shell, a script, a ticket, a chat message. Only
//     the first of those four is a safe home for a secret, and the application
//     cannot tell which one the clipboard is going to. The review found the
//     button is used in practice for the other three.
//
// A command carrying a password serves the second audience and endangers it.
// A redacted command serves the first completely and the second partially: an
// operator pasting into a terminal has to re-supply the credential, which is a
// thing they can do — they typed it into this application, and a credential
// they cannot reproduce is not one they should be building with. That asymmetry
// is why the redacted form is the one to show AND the one to copy. The
// alternative, a "reveal" control, is not a compromise: a secret you can reveal
// is a secret on the screen, and the operator already has the URL in the
// selection tray where they entered it.
//
// # The redaction itself is debark's, not a second one
//
// fetch.RedactURL is the engine's single decision about how a
// possibly-credentialed URL is allowed to reach a record a person will read.
// It is imported rather than reimplemented — core/fetch is already in this
// binary's link closure, so this costs nothing — because the review's own
// argument for fixing §6.1 was consistency between the two halves of one
// product, and two redactors that can disagree is two answers to one question.
//
// It removes more than userinfo, and that is deliberate on core's part: the
// whole query string goes too, because a presigned URL's credential lives in a
// query parameter (an AWS X-Amz-Signature, an Azure SAS sig, a bare vendor
// token=) whose NAME is not standardised, so it cannot be told apart from a
// harmless one by pattern. Guessing risks shipping a real credential under the
// belief it was redacted.
//
// The cost lands here and is worth stating plainly: a vendor URL with an
// ordinary, secret-free query string — a version pin, a mirror id — is
// redacted too, so for that input the previewed command is a faithful
// description of the build and not a paste-and-run script. That is the right
// trade for a string built to be copied, and it is the same trade core made
// for lock.json and evidence.json. The redaction is visible ("REDACTED"), never
// silent, so nobody reads the result as a URL that never had a credential.
//
// # Where this must be called
//
// Everywhere the argv becomes text: internal/app's PreviewCommand
// (CommandPreview.Argv and .Display) and the Command field of BuildStarted and
// BuildStatus, which the build screen renders and its details drawer copies.
// Those are another package files; see §6.1a for the exact call sites.
//
// argv is not modified. A nil argv returns nil.
func RedactArgv(argv []string) []string {
	if argv == nil {
		return nil
	}
	out := make([]string, len(argv))
	copy(out, argv)
	for i, a := range out {
		switch {
		case strings.HasPrefix(a, urlInputPrefix):
			out[i] = urlInputPrefix + fetch.RedactURL(strings.TrimPrefix(a, urlInputPrefix))
		case i > 0 && out[i-1] == digestFlag:
			out[i] = redactDigestPair(a)
		}
	}
	return out
}

const (
	// urlInputPrefix is the explicit classifier BuildArgv puts on a positional
	// URL input, so the CLI cannot reclassify a value by its shape.
	urlInputPrefix = "url:"
	digestFlag     = "--digest"
)

// redactDigestPair redacts the URL half of a "URL=SHA256" --digest value.
//
// The split is at the LAST "=", which is where the digest half begins: a
// SHA-256 is 64 hexadecimal characters and can never contain one. That is also
// how debark's fixed flag splits it (core registerDigestFlag), and for a spec
// that passed Validate the URL half holds no "=" at all, so the two rules agree
// on every value this application emits.
//
// A value with no "=" is not a pair. It is redacted whole rather than passed
// through, because the one thing that must not happen here is a credential
// surviving because the string was an unexpected shape.
func redactDigestPair(v string) string {
	i := strings.LastIndex(v, "=")
	if i < 0 {
		return fetch.RedactURL(v)
	}
	return fetch.RedactURL(v[:i]) + v[i:]
}
