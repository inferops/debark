package cli

import (
	"slices"
	"strings"

	"github.com/spf13/pflag"

	"github.com/inferops/debark/core/dferr"
	"github.com/inferops/debark/core/digest"
)

// digestFlagUsage is the one wording of --digest's help text, so the two
// commands that carry the flag cannot describe it differently.
const digestFlagUsage = "expected sha256 for a URL input, upgrading its provenance to user-digest (URL=SHA256, split at the LAST '='); repeatable"

// digestFlag is --digest's value type.
//
// It replaces pflag's StringToString, which cannot express this flag. Two
// things go wrong there, and both are silent:
//
//   - StringToString splits each pair at the FIRST "=". A vendor URL with a
//     query string has its own "=" long before the separator, so
//     `--digest 'https://vendor.example/a.deb?ver=1=<sha>'` bound the digest
//     "1=<sha>" to the key "https://vendor.example/a.deb?ver". No error. The
//     real URL then has no digest, so its provenance silently stays
//     url-unverified instead of user-digest -- the operator asked for the
//     strongest claim debark makes about a file and quietly did not get
//     it.
//   - With two or more "=" in the value, pflag runs it through a CSV reader
//     first. So `?ids=1,2=<sha>` became TWO entries, {"...?ids": "1", "2":
//     "<sha>"}, and `?q="x"=<sha>` failed outright with `parse error on line
//     1, column 35: bare " in non-quoted-field`, a message that says nothing
//     about digests or URLs.
//
// All four shapes are in TestDigestFlag.
//
// Splitting at the LAST "=" is exact rather than a better guess: a SHA-256
// is 64 hexadecimal characters and can never contain "=", so the final "="
// in a well-formed pair is always the separator. The digest is validated
// here, which is what makes that reasoning safe to rely on -- a value that
// is not a sha256 is refused with both halves shown, so a genuinely
// malformed argument becomes a loud error instead of a quiet misbinding.
type digestFlag struct {
	dest *map[string]string
}

// newDigestFlag returns the pflag.Value to register --digest with, writing
// into dest. Callers keep their existing map[string]string, so adopting it is
// a one-line change at the registration site and nothing downstream moves.
func newDigestFlag(dest *map[string]string) *digestFlag {
	return &digestFlag{dest: dest}
}

func (f *digestFlag) String() string {
	if f.dest == nil || len(*f.dest) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(*f.dest))
	for k, v := range *f.dest {
		pairs = append(pairs, k+"="+v)
	}
	// Sorted, because a flag's String() feeds help output and error
	// messages, and map order is not an ordering.
	slices.Sort(pairs)
	return strings.Join(pairs, ",")
}

// Type is what pflag prints after the flag name in help output.
func (f *digestFlag) Type() string { return "URL=SHA256" }

func (f *digestFlag) Set(v string) error {
	url, sum, err := splitDigestPair(v)
	if err != nil {
		return err
	}
	if *f.dest == nil {
		*f.dest = map[string]string{}
	}
	(*f.dest)[url] = sum
	return nil
}

// splitDigestPair splits one --digest argument into its URL and its sha256.
func splitDigestPair(v string) (url, sum string, err error) {
	i := strings.LastIndexByte(v, '=')
	if i < 0 {
		return "", "", dferr.New(dferr.Usage, "--digest %q: expected URL=SHA256", v)
	}
	url, sum = v[:i], strings.ToLower(strings.TrimSpace(v[i+1:]))
	if url == "" {
		return "", "", dferr.New(dferr.Usage, "--digest %q: no URL before the last '='", v)
	}
	if !digest.Valid(sum) {
		// Both halves, because the whole failure mode this replaces is not
		// being able to see where the split landed.
		return "", "", dferr.New(dferr.Usage,
			"--digest %q: %q is not a sha256 (want 64 lowercase hex characters); the pair is split at the LAST '=', so the URL read as %q",
			v, sum, url)
	}
	return url, sum, nil
}

// registerDigestFlag adds --digest to flags, writing into dest.
//
// Both commands that take --digest call this, so the flag cannot end up with
// two splitting rules or two help strings. `build`'s own registration lives
// in cmd_build.go.
func registerDigestFlag(flags *pflag.FlagSet, dest *map[string]string) {
	flags.Var(newDigestFlag(dest), "digest", digestFlagUsage)
}
