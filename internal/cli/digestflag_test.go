package cli

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/inferops/debark/core/dferr"
)

// sha is a real-shaped sha256: 64 lowercase hex characters, and — the fact
// the whole splitting rule rests on — no "=" anywhere in it.
const sha = "2e6e2f1a3b4c5d6e7f8091a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6"

// TestDigestFlag_ReproducesTheStringToStringDefect is the reproduction, kept
// as a test rather than described in prose: it drives pflag's StringToString
// exactly as the flag used to be registered and records what it does. Every
// want below was observed, not predicted.
//
// The security consequence is the same in all four rows and is why the
// finding exists: the digest never binds to the URL the operator typed, so
// the download proceeds as url-unverified when they had asked for
// user-digest — the strongest provenance claim debark makes about a file.
// Three of the four are silent.
func TestDigestFlag_ReproducesTheStringToStringDefect(t *testing.T) {
	cases := []struct {
		name string
		arg  string
		// wantKey/wantVal describe the WRONG binding StringToString makes.
		wantKey string
		wantVal string
		// wantErrFragment, when set, means StringToString refused outright.
		wantErrFragment string
	}{
		{
			name:    "a query string with one parameter",
			arg:     "https://vendor.example/a.deb?ver=1=" + sha,
			wantKey: "https://vendor.example/a.deb?ver",
			wantVal: "1=" + sha,
		},
		{
			name:    "a query string with two parameters",
			arg:     "https://vendor.example/a.deb?a=1&b=2=" + sha,
			wantKey: "https://vendor.example/a.deb?a",
			wantVal: "1&b=2=" + sha,
		},
		{
			// Worse than a misbinding: with two or more "=" pflag runs the
			// value through a CSV reader, so a comma in the query string
			// splits it into two separate pairs and the digest binds to the
			// key "2".
			name:    "a comma in the query string",
			arg:     "https://vendor.example/a.deb?ids=1,2=" + sha,
			wantKey: "2",
			wantVal: sha,
		},
		{
			name:            "a quote in the query string",
			arg:             `https://vendor.example/a.deb?q="x"=` + sha,
			wantErrFragment: "bare \" in non-quoted-field",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]string
			fs := pflag.NewFlagSet("repro", pflag.ContinueOnError)
			fs.SetOutput(nopWriter{})
			fs.StringToStringVar(&got, "digest", nil, "")
			err := fs.Parse([]string{"--digest", tc.arg})

			if tc.wantErrFragment != "" {
				if err == nil {
					t.Fatalf("StringToString accepted %q; this row no longer describes the defect", tc.arg)
				}
				if !strings.Contains(err.Error(), tc.wantErrFragment) {
					t.Errorf("error = %v, want it to contain %q", err, tc.wantErrFragment)
				}
				return
			}
			if err != nil {
				t.Fatalf("StringToString refused %q: %v; this row no longer describes the defect", tc.arg, err)
			}
			if v := got[tc.wantKey]; v != tc.wantVal {
				t.Errorf("StringToString bound %#v; this row no longer describes the defect (expected the wrong key %q -> %q)", got, tc.wantKey, tc.wantVal)
			}
			// The point of the whole thing: the URL the operator actually
			// typed has no digest.
			url := strings.TrimSuffix(tc.arg, "="+sha)
			if _, present := got[url]; present {
				t.Errorf("StringToString bound the real URL after all; the defect is gone and this test should be too")
			}
		})
	}
}

// TestDigestFlag is the fix. Same arguments, through the flag the commands
// now register.
func TestDigestFlag(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    map[string]string
		wantErr string
	}{
		{
			name: "a plain URL",
			args: []string{"https://vendor.example/a.deb=" + sha},
			want: map[string]string{"https://vendor.example/a.deb": sha},
		},
		{
			name: "a query string with one parameter",
			args: []string{"https://vendor.example/a.deb?ver=1=" + sha},
			want: map[string]string{"https://vendor.example/a.deb?ver=1": sha},
		},
		{
			name: "a query string with two parameters",
			args: []string{"https://vendor.example/a.deb?a=1&b=2=" + sha},
			want: map[string]string{"https://vendor.example/a.deb?a=1&b=2": sha},
		},
		{
			name: "a comma in the query string",
			args: []string{"https://vendor.example/a.deb?ids=1,2=" + sha},
			want: map[string]string{"https://vendor.example/a.deb?ids=1,2": sha},
		},
		{
			name: "a quote in the query string",
			args: []string{`https://vendor.example/a.deb?q="x"=` + sha},
			want: map[string]string{`https://vendor.example/a.deb?q="x"`: sha},
		},
		{
			name: "a presigned URL whose signature ends in base64 padding",
			args: []string{"https://vendor.example/a.deb?X-Amz-Signature=abcd==" + sha},
			want: map[string]string{"https://vendor.example/a.deb?X-Amz-Signature=abcd=": sha},
		},
		{
			name: "repeatable",
			args: []string{"https://a.example/x.deb?v=1=" + sha, "https://b.example/y.deb=" + sha},
			want: map[string]string{
				"https://a.example/x.deb?v=1": sha,
				"https://b.example/y.deb":     sha,
			},
		},
		{
			name: "an uppercase digest is accepted and normalised",
			args: []string{"https://vendor.example/a.deb=" + strings.ToUpper(sha)},
			want: map[string]string{"https://vendor.example/a.deb": sha},
		},
		{
			// The validation is what makes splitting at the last "=" safe
			// to rely on, so a genuinely malformed argument has to be loud.
			name:    "no separator at all",
			args:    []string{"https://vendor.example/a.deb"},
			wantErr: "expected URL=SHA256",
		},
		{
			name:    "a digest that is not a digest",
			args:    []string{"https://vendor.example/a.deb=deadbeef"},
			wantErr: "is not a sha256",
		},
		{
			// The message has to show where the split landed, because not
			// being able to see that is the entire failure mode this
			// replaces.
			name:    "a query string and no digest",
			args:    []string{"https://vendor.example/a.deb?ver=1"},
			wantErr: `the URL read as "https://vendor.example/a.deb?ver"`,
		},
		{
			name:    "nothing before the separator",
			args:    []string{"=" + sha},
			wantErr: "no URL before the last",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]string
			fs := pflag.NewFlagSet("digest", pflag.ContinueOnError)
			fs.SetOutput(nopWriter{})
			registerDigestFlag(fs, &got)

			var argv []string
			for _, a := range tc.args {
				argv = append(argv, "--digest", a)
			}
			err := fs.Parse(argv)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse accepted %v and produced %#v, want an error containing %q", tc.args, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got[%q] = %q, want %q (whole map: %#v)", k, got[k], v, got)
				}
			}
		})
	}
}

// TestSplitDigestPairClass keeps the refusal on the class the CLI's own
// exit-code table gives a bad flag.
func TestSplitDigestPairClass(t *testing.T) {
	_, _, err := splitDigestPair("https://vendor.example/a.deb=nope")
	if err == nil {
		t.Fatal("splitDigestPair accepted a non-digest")
	}
	if got := dferr.ClassOf(err); got != dferr.Usage {
		t.Errorf("dferr.ClassOf = %v, want %v: a malformed flag value is a usage error", got, dferr.Usage)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
