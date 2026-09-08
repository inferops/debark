package sign

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferops/debark/core/dferr"
)

// TestMissingPrivateKeyHintNamesARealCommand pins the hint an operator sees
// when --sign names a key file that is not there.
//
// It used to read: generate one with "debark key generate". There is no
// "key" command. Typing the hint printed
//
//	debark: unknown command "key" for "debark"
//
// which is a particularly unhelpful thing to be told by the sentence that
// exists to tell you what to do next. The command is keygen, and it needs
// --out, so the hint now names the path the operator already gave -- it can
// be pasted rather than adapted.
//
// internal/cli's TestHintedCommandsExist catches the whole class across the
// repository; this pins the one site and the argument.
func TestMissingPrivateKeyHintNamesARealCommand(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "operator.key")

	_, err := newEd25519FileSigner(missing)
	if err == nil {
		t.Fatal("newEd25519FileSigner succeeded on a key file that does not exist")
	}
	if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("dferr.ClassOf = %v, want %v", got, dferr.Usage)
	}

	hint := dferr.HintOf(err)
	if hint == "" {
		t.Fatal("no hint at all on the one error whose whole job is to say what to do next")
	}
	if strings.Contains(hint, "key generate") {
		t.Errorf("the hint still names a command that does not exist:\n%s", hint)
	}
	if !strings.Contains(hint, "debark keygen") {
		t.Errorf("the hint does not name `debark keygen`:\n%s", hint)
	}
	if !strings.Contains(hint, missing) {
		t.Errorf("the hint does not name the path the operator gave, so it cannot be pasted:\n%s", hint)
	}
}
