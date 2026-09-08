package engine

import (
	"strings"
	"testing"

	buildjob "github.com/inferops/debark/api/buildjob/v1"
	"github.com/inferops/debark/core/canonical"
)

func requestDigest(t *testing.T, req buildjob.BuildRequest) string {
	t.Helper()
	d, err := canonical.Digest(newRequestDigestInput(req))
	if err != nil {
		t.Fatalf("canonical.Digest: %v", err)
	}
	return d
}

// laptopRequest is one complete build request as an operator would issue it
// from their own machine: every path-shaped field is set, and every one of
// them is a location under /home/alice.
func laptopRequest() buildjob.BuildRequest {
	return buildjob.BuildRequest{
		SchemaVersion: buildjob.SchemaVersion,
		SnapshotRef:   "/home/alice/snapshots/prod.tar.zst",
		Inputs: buildjob.Inputs{
			Packages:  []string{"vlc", "curl"},
			URLs:      []buildjob.URLInput{{URL: "https://vendor.example.com/acme-agent_2.1.0_amd64.deb", SHA256: "5ca54ae9"}},
			Files:     []string{"/home/alice/debs/acme-agent_2.1.0_amd64.deb"},
			LocalDirs: []string{"/home/alice/vendor-debs"},
			ListFiles: []string{"/home/alice/packages.txt"},
		},
		Options: buildjob.Options{
			UpdateMode:      buildjob.UpdateAdditive,
			StoreDir:        "/home/alice/.cache/debark",
			PolicyRef:       "/home/alice/policy.yaml",
			ApprovedKeysRef: "/home/alice/approved-keys.txt",
			EmbedBinary:     "/home/alice/bin/debark",
		},
		Output: buildjob.Output{
			Path:   "/home/alice/out/bundle",
			Format: buildjob.FormatDir,
			Sign:   buildjob.SignOptions{SignerRef: "/home/alice/keys/op.key", Required: true},
		},
	}
}

// TestRequestDigest_SameRequestFromAnotherMachine is the whole point of the
// field: the identical logical build, issued from a CI runner where every
// single path is different, must produce the identical RequestDigest — and
// therefore the identical lock.json, LockDigest, BundleID and README.txt.
// Before this was narrowed, seven fields (Options.StoreDir, PolicyRef,
// ApprovedKeysRef, EmbedBinary and Inputs.Files, LocalDirs, ListFiles) each
// moved the digest on their own; --local-dir was the most exposed of them,
// since it is the headline vendor-.deb flag.
func TestRequestDigest_SameRequestFromAnotherMachine(t *testing.T) {
	laptop := laptopRequest()

	ci := laptopRequest()
	ci.SnapshotRef = "/builds/acme/artifacts/prod.tar.zst"
	ci.Inputs.Files = []string{"/builds/acme/repo/vendor/acme-agent_2.1.0_amd64.deb"}
	ci.Inputs.LocalDirs = []string{"/builds/acme/repo/vendor-debs"}
	ci.Inputs.ListFiles = []string{"/builds/acme/repo/packages.txt"}
	ci.Options.PolicyRef = "/etc/debark/policy.yaml"
	ci.Options.ApprovedKeysRef = "/etc/debark/approved-keys.txt"
	ci.Options.EmbedBinary = "/usr/lib/debark/bin/debark"
	// StoreDir is dropped from the digest outright rather than reduced to a
	// basename (it names a machine-level cache, not an input), so a
	// completely unrelated one must still not matter.
	ci.Options.StoreDir = "/var/cache/debark-shared"
	ci.Output = buildjob.Output{
		Path:   "/builds/acme/out",
		Format: buildjob.FormatTar,
		Sign:   buildjob.SignOptions{SignerRef: "gpg:DEADBEEF", Required: true},
	}

	if got, want := requestDigest(t, ci), requestDigest(t, laptop); got != want {
		t.Errorf("the same request from two machines produced two RequestDigests:\n  laptop=%s\n  ci    =%s", want, got)
	}
}

// TestRequestDigest_ListOrderDoesNotMatter holds buildjob.Inputs to its own
// documented promise: "Every list is order-preserving; the engine sorts
// before hashing so a reordering does not change the request digest."
func TestRequestDigest_ListOrderDoesNotMatter(t *testing.T) {
	a := laptopRequest()
	a.Inputs.Packages = []string{"curl", "vlc"}
	a.Inputs.URLs = []buildjob.URLInput{
		{URL: "https://vendor.example.com/a.deb"},
		{URL: "https://vendor.example.com/b.deb"},
	}
	a.Inputs.Files = []string{"/x/one.deb", "/x/two.deb"}

	b := laptopRequest()
	b.Inputs.Packages = []string{"vlc", "curl"}
	b.Inputs.URLs = []buildjob.URLInput{
		{URL: "https://vendor.example.com/b.deb"},
		{URL: "https://vendor.example.com/a.deb"},
	}
	b.Inputs.Files = []string{"/x/two.deb", "/x/one.deb"}

	if got, want := requestDigest(t, b), requestDigest(t, a); got != want {
		t.Errorf("reordering the input lists changed RequestDigest:\n  A=%s\n  B=%s", want, got)
	}
}

// TestRequestDigest_ContentStillMovesIt is the other half, and the one that
// makes the test above mean something: narrowing the digest must not have
// flattened it. Every change here is a change to WHAT was asked for, and each
// must still move the digest — including a file input whose basename differs,
// which is the fidelity the basename representation deliberately keeps.
func TestRequestDigest_ContentStillMovesIt(t *testing.T) {
	base := laptopRequest()
	ref := requestDigest(t, base)

	mutations := map[string]func(*buildjob.BuildRequest){
		"a different package":              func(r *buildjob.BuildRequest) { r.Inputs.Packages = []string{"vlc", "wget"} },
		"a pinned package version":         func(r *buildjob.BuildRequest) { r.Inputs.Packages = []string{"vlc=3.0.21-1", "curl"} },
		"a different vendor URL":           func(r *buildjob.BuildRequest) { r.Inputs.URLs[0].URL = "https://vendor.example.com/other.deb" },
		"a different expected URL digest":  func(r *buildjob.BuildRequest) { r.Inputs.URLs[0].SHA256 = "ffffffff" },
		"a different .deb file name":       func(r *buildjob.BuildRequest) { r.Inputs.Files = []string{"/home/alice/debs/other_1.0_amd64.deb"} },
		"a different local dir name":       func(r *buildjob.BuildRequest) { r.Inputs.LocalDirs = []string{"/home/alice/other-debs"} },
		"a different list file name":       func(r *buildjob.BuildRequest) { r.Inputs.ListFiles = []string{"/home/alice/other.txt"} },
		"a different policy file name":     func(r *buildjob.BuildRequest) { r.Options.PolicyRef = "/home/alice/strict-policy.yaml" },
		"a different approved keys name":   func(r *buildjob.BuildRequest) { r.Options.ApprovedKeysRef = "/home/alice/other-keys.txt" },
		"a different embedded binary name": func(r *buildjob.BuildRequest) { r.Options.EmbedBinary = "/home/alice/bin/debark-static" },
		"recommends forced on":             func(r *buildjob.BuildRequest) { on := true; r.Options.Recommends = &on },
		"upgrades requested":               func(r *buildjob.BuildRequest) { r.Options.Upgrades = true },
		"refresh instead of additive":      func(r *buildjob.BuildRequest) { r.Options.UpdateMode = buildjob.UpdateRefresh },
		"an architecture override":         func(r *buildjob.BuildRequest) { r.Options.ArchOverride = "arm64" },
		"the closed-world check disabled":  func(r *buildjob.BuildRequest) { off := false; r.Options.ClosedWorldCheck = &off },
		"an SBOM requested":                func(r *buildjob.BuildRequest) { r.Options.SBOM = true },
		"a mirror override": func(r *buildjob.BuildRequest) {
			r.Options.MirrorOverrides = map[string]string{"http://deb.debian.org": "http://mirror.internal"}
		},
		"a different backend": func(r *buildjob.BuildRequest) { r.Options.Backend = "container" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			req := laptopRequest()
			mutate(&req)
			if got := requestDigest(t, req); got == ref {
				t.Errorf("%s did not change RequestDigest (%s); the narrowing went too far", name, got)
			}
		})
	}
}

// TestRequestDigest_NoHostPathInTheDigestedBytes checks the other half of
// contract brief rule 2 — a host path must never reach an artefact — at the
// level of the bytes that are actually hashed, not just their digest. A
// digest that merely happens to be stable would still be a defect if the
// directories were in the pre-image.
func TestRequestDigest_NoHostPathInTheDigestedBytes(t *testing.T) {
	raw, err := canonical.Marshal(newRequestDigestInput(laptopRequest()))
	if err != nil {
		t.Fatalf("canonical.Marshal: %v", err)
	}
	for _, forbidden := range []string{
		"/home/alice", "debs/", ".cache/debark", "/out/bundle", "keys/op.key",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("RequestDigest is computed over a host path: %q appears in %s", forbidden, raw)
		}
	}
}
