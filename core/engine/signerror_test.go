package engine

import (
	"context"
	"os"
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/dferr"
)

// TestBuild_SignerErrorKeepsItsClassAndHint pins the one place in
// finalizeBundle that calls a collaborator which classifies its own failures.
//
// core/sign returns Usage for the two things an operator can actually fix
// themselves — cancelling at the gpg pinentry prompt (core/sign/gpg.go) and a
// misbehaving plugin's protocol errors (core/sign/plugin.go) — and attaches a
// remedy hint to several of them. dferr.ClassOf and dferr.HintOf both resolve
// the OUTERMOST *dferr.Error, so wrapping that error unconditionally with
// dferr.Wrap(dferr.Environment, ...) replaced both: the operator was told the
// machine could not do the job, with no hint, when in fact they had pressed
// Esc. The fix is the package's own classify helper, which returns an
// already-classified cause untouched.
//
// The default is asserted too, in the same test: a signer that returns a bare
// error still comes back as Environment, so this is a narrowing of the wrap,
// not a removal of it.
func TestBuild_SignerErrorKeepsItsClassAndHint(t *testing.T) {
	const hint = "unlock the key, or pass --sign with a different key reference"

	cases := []struct {
		name      string
		signErr   error
		wantClass dferr.Class
		wantHint  string
	}{
		{
			name: "classified usage error survives with its hint",
			signErr: dferr.New(dferr.Usage, "sign: gpg: operation cancelled at the pinentry prompt").
				WithHint("%s", hint),
			wantClass: dferr.Usage,
			wantHint:  hint,
		},
		{
			name:      "unclassified error still defaults to environment",
			signErr:   errString("the smartcard went away"),
			wantClass: dferr.Environment,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.signer.signErr = tc.signErr
			h.req.Output.Sign = buildjob.SignOptions{Required: true}

			eng, err := New(h.deps)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			res, err := eng.Build(context.Background(), h.req)
			if err == nil {
				t.Fatal("expected the build to fail when the signer fails")
			}
			if res != nil {
				t.Fatalf("expected a nil result, got %+v", res)
			}
			if got := dferr.ClassOf(err); got != tc.wantClass {
				t.Errorf("dferr.ClassOf(err) = %v, want %v (err: %v)", got, tc.wantClass, err)
			}
			if got := dferr.HintOf(err); got != tc.wantHint {
				t.Errorf("dferr.HintOf(err) = %q, want %q", got, tc.wantHint)
			}
			if !strings.Contains(err.Error(), "the pinentry prompt") && !strings.Contains(err.Error(), "smartcard") {
				t.Errorf("the signer's own message was lost: %v", err)
			}
			// finalizeBundle removes the half-built bundle when signing
			// fails; classifying rather than wrapping must not change that.
			if _, statErr := os.Stat(h.outDir); !os.IsNotExist(statErr) {
				t.Errorf("expected the bundle directory to be removed, stat returned err=%v", statErr)
			}
		})
	}
}

// errString is a plain error with no dferr class, standing in for a signer
// implementation (or a future one) that does not classify its failures.
type errString string

func (e errString) Error() string { return string(e) }
