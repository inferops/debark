package sign

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateKeyPublicFileFailureLeavesNoPrivateKey(t *testing.T) {
	priv := filepath.Join(t.TempDir(), "operator.key")
	pub := publicKeyPathFor(priv)
	want := []byte("existing public key")
	if err := os.WriteFile(pub, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateKey(priv, ""); err == nil {
		t.Fatal("accepted an existing public key")
	}
	if _, err := os.Lstat(priv); !os.IsNotExist(err) {
		t.Fatalf("failed key generation left a private key behind: %v", err)
	}
	got, err := os.ReadFile(pub)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("existing public key was modified: %q, %v", got, err)
	}
	if err := os.Remove(pub); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateKey(priv, "retry"); err != nil {
		t.Fatalf("could not retry after fixing the public path: %v", err)
	}
}

func TestGenerateKeyRejectsMultilineComment(t *testing.T) {
	for _, comment := range []string{"first\nsecond", "first\rsecond", "first\r\nsecond"} {
		t.Run(comment, func(t *testing.T) {
			priv := filepath.Join(t.TempDir(), "operator.key")
			if _, err := GenerateKey(priv, comment); err == nil {
				t.Fatal("generated an unreadable key file with a multiline comment")
			}
			if _, err := os.Lstat(priv); !os.IsNotExist(err) {
				t.Fatalf("invalid comment left a private key behind: %v", err)
			}
		})
	}
}
