package sbom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/inferops/debark/core/lock"
)

// The tests here are about the one promise this document makes: it names
// exactly what the bundle contains, and it names it correctly. A component
// that is silently renamed, silently unhashed, or silently duplicated is a
// supply-chain claim that is wrong, and it ships inside the signed bundle.

// TestBuildRejectsInvalidUTF8Identity is the regression test for the writer
// silently changing a package's identity. encoding/json rewrites every
// invalid UTF-8 byte as U+FFFD, so two distinct packages used to come out of
// Build carrying the same "name" while their purls stayed different: an SBOM
// naming a package that is not the one in the pool.
func TestBuildRejectsInvalidUTF8Identity(t *testing.T) {
	base := Component{Name: "vlc", Version: "3.0.21", Arch: "amd64", Distro: "debian", SHA256: strings.Repeat("a", 64)}
	cases := []struct {
		field string
		mut   func(*Component)
	}{
		{"name", func(c *Component) { c.Name = "vlc\xff" }},
		{"version", func(c *Component) { c.Version = "3.0\xfe" }},
		{"arch", func(c *Component) { c.Arch = "amd\xff64" }},
		{"distro", func(c *Component) { c.Distro = "deb\xffian" }},
		{"source package", func(c *Component) { c.SourcePackage = "vlc\xff" }},
	}
	for _, tc := range cases {
		c := base
		tc.mut(&c)
		if _, err := Build(fixtureOptions(), []Component{c}); err == nil {
			t.Errorf("Build accepted invalid UTF-8 in %s", tc.field)
		}
	}

	// The concrete failure the check exists to stop: two different packages
	// rendering as one name.
	bad := []Component{
		{Name: "pkg\xff", Version: "1", Arch: "amd64", Distro: "debian", SHA256: strings.Repeat("a", 64)},
		{Name: "pkg\xfe", Version: "1", Arch: "amd64", Distro: "debian", SHA256: strings.Repeat("b", 64)},
	}
	doc, err := Build(fixtureOptions(), bad)
	if err == nil {
		out, _ := doc.JSON()
		var back Document
		if uerr := json.Unmarshal(out, &back); uerr == nil &&
			len(back.Components) == 2 && back.Components[0].Name == back.Components[1].Name {
			t.Fatalf("two distinct packages were written under one name %q", back.Components[0].Name)
		}
		t.Fatal("Build accepted package names that are not valid UTF-8")
	}
}

// TestBuildRejectsControlCharacters keeps a rendered SBOM line from carrying
// a newline or a terminal escape and reading as something it is not. No
// Debian name, version, architecture or source name may contain one.
func TestBuildRejectsControlCharacters(t *testing.T) {
	// \u0085 is NEL, a C1 control: legal UTF-8, invisible in a terminal, and
	// still something a renderer may act on.
	for _, name := range []string{"vlc\n", "vlc\r", "vlc\x00", "vlc\x1b[31m", "vlc\x7f", "vlc\u0085"} {
		c := Component{Name: name, Version: "1", Arch: "amd64", Distro: "debian", SHA256: strings.Repeat("a", 64)}
		if _, err := Build(fixtureOptions(), []Component{c}); err == nil {
			t.Errorf("Build accepted the control character in name %q", name)
		}
	}
	if _, err := Build(Options{BundleID: "b\n1", Timestamp: "2026-09-03T12:00:00Z"}, nil); err == nil {
		t.Error("Build accepted a control character in Options.BundleID")
	}
	// The check must not reject the ordinary case it sits in front of.
	clean := Component{Name: "vlc", Version: "3.0.21-1build1", Arch: "amd64", Distro: "debian", SHA256: strings.Repeat("a", 64)}
	if _, err := Build(fixtureOptions(), []Component{clean}); err != nil {
		t.Errorf("Build rejected an ordinary component: %v", err)
	}
}

// TestBuildRejectsMalformedDigest: a hashes[] entry is a claim about bytes in
// the pool. A malformed one is both a false claim and a document that fails
// CycloneDX's own hash-content pattern, which a consumer only discovers on
// the far side of the gap.
func TestBuildRejectsMalformedDigest(t *testing.T) {
	for _, d := range []string{
		"not-a-digest",
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.ToUpper(strings.Repeat("a", 64)),
		"sha256:" + strings.Repeat("a", 64),
	} {
		c := Component{Name: "vlc", Version: "1", Arch: "amd64", Distro: "debian", SHA256: d}
		if _, err := Build(fixtureOptions(), []Component{c}); err == nil {
			t.Errorf("Build accepted the malformed sha256 %q", d)
		}
	}
	// An absent digest is a different thing from a wrong one: it simply omits
	// the hashes block, which stays legal.
	c := Component{Name: "vlc", Version: "1", Arch: "amd64", Distro: "debian"}
	doc, err := Build(fixtureOptions(), []Component{c})
	if err != nil {
		t.Fatalf("Build rejected a component with no digest at all: %v", err)
	}
	if len(doc.Components[0].Hashes) != 0 {
		t.Error("a component with no digest must not carry a hashes block")
	}
}

// TestBuildRejectsDuplicateBOMRef: bom-ref must identify exactly one
// component (CycloneDX definitions/refType), and two entries claiming one
// identity is the ambiguity an SBOM exists to remove.
func TestBuildRejectsDuplicateBOMRef(t *testing.T) {
	dup := []Component{
		{Name: "vlc", Version: "1", Arch: "amd64", Distro: "debian", SHA256: strings.Repeat("a", 64)},
		{Name: "vlc", Version: "1", Arch: "amd64", Distro: "debian", SHA256: strings.Repeat("b", 64)},
	}
	if _, err := Build(fixtureOptions(), dup); err == nil {
		t.Fatal("Build accepted two components with the same purl, so the document has a duplicate bom-ref")
	}
	// Same name and version at two architectures is legitimate (Multi-Arch:
	// same) and must still be accepted.
	ok := []Component{
		{Name: "libc6", Version: "1", Arch: "amd64", Distro: "debian", SHA256: strings.Repeat("a", 64)},
		{Name: "libc6", Version: "1", Arch: "i386", Distro: "debian", SHA256: strings.Repeat("b", 64)},
	}
	if _, err := Build(fixtureOptions(), ok); err != nil {
		t.Fatalf("Build rejected the same package at two architectures: %v", err)
	}
}

// TestFromLockDescribesEveryLockPackageExactly is the completeness check: the
// SBOM is the auditor's answer to "what is in this bundle", and it is built
// from lock.Packages, so every row must appear exactly once with its own
// identity attached to it -- not merely the right number of rows.
func TestFromLockDescribesEveryLockPackageExactly(t *testing.T) {
	l := &lock.Lock{
		Target: lock.Target{DistroID: "ubuntu", Codename: "noble", Arch: "amd64"},
		Packages: []lock.Package{
			{Name: "zeta", Arch: "amd64", Version: "1:2.0~rc1", SHA256: strings.Repeat("1", 64), SourcePackage: "zeta-src", Filename: "pool/z/zeta.deb"},
			{Name: "alpha", Arch: "arm64", Version: "0.1+deb12u1", SHA256: strings.Repeat("2", 64), Filename: "pool/a/alpha.deb"},
			{Name: "alpha", Arch: "amd64", Version: "0.1+deb12u1", SHA256: strings.Repeat("3", 64), SourcePackage: "alpha", Filename: "pool/a/alpha2.deb"},
		},
	}
	doc, err := FromLock(l, "bundle-x", "2026-09-03T12:00:00Z")
	if err != nil {
		t.Fatalf("FromLock: %v", err)
	}
	if len(doc.Components) != len(l.Packages) {
		t.Fatalf("SBOM has %d components for %d lock packages", len(doc.Components), len(l.Packages))
	}
	byRef := map[string]BOMComponent{}
	for _, c := range doc.Components {
		byRef[c.BOMRef] = c
	}
	for _, p := range l.Packages {
		purl, err := PURL(l.Target.DistroID, p.Name, p.Version, p.Arch)
		if err != nil {
			t.Fatal(err)
		}
		c, ok := byRef[purl]
		if !ok {
			t.Errorf("lock package %s %s/%s is missing from the SBOM", p.Name, p.Arch, p.Version)
			continue
		}
		if c.Name != p.Name || c.Version != p.Version {
			t.Errorf("%s: SBOM says %s@%s, lock says %s@%s", purl, c.Name, c.Version, p.Name, p.Version)
		}
		if len(c.Hashes) != 1 || c.Hashes[0].Alg != HashAlgSHA256 || c.Hashes[0].Content != p.SHA256 {
			t.Errorf("%s: SBOM hash %+v does not match the lock's %s", purl, c.Hashes, p.SHA256)
		}
		var src string
		for _, prop := range c.Properties {
			if prop.Name == SourcePackageProperty {
				src = prop.Value
			}
		}
		if src != p.SourcePackage {
			t.Errorf("%s: SBOM source package %q, lock says %q", purl, src, p.SourcePackage)
		}
	}
}

// TestFromLockIsIndependentOfInputOrder is the determinism test that can
// actually express the variation it is checking for: the same packages are
// handed to FromLock in a different order every iteration, and the rendered
// bytes must not move. A test that fed one fixed order would pass even if
// FromLock did no sorting at all, so the shuffle is asserted to be real
// before the comparison is trusted.
func TestFromLockIsIndependentOfInputOrder(t *testing.T) {
	build := func(seed int64) (out string, order string) {
		pkgs := make([]lock.Package, 0, 64)
		for i := 0; i < 64; i++ {
			pkgs = append(pkgs, lock.Package{
				Name:          fmt.Sprintf("pkg%02d", i),
				Arch:          []string{"amd64", "arm64", "all"}[i%3],
				Version:       fmt.Sprintf("%d:%d.0~rc1+deb12u%d", i%2, i, i%7),
				SHA256:        strings.Repeat(fmt.Sprintf("%02x", i), 32),
				SourcePackage: fmt.Sprintf("src%02d", i%9),
				Filename:      fmt.Sprintf("pool/p/pkg%02d.deb", i),
			})
		}
		r := rand.New(rand.NewSource(seed))
		r.Shuffle(len(pkgs), func(i, j int) { pkgs[i], pkgs[j] = pkgs[j], pkgs[i] })
		names := make([]string, len(pkgs))
		for i, p := range pkgs {
			names[i] = p.Name + "/" + p.Arch
		}
		doc, err := FromLock(&lock.Lock{Target: lock.Target{DistroID: "debian"}, Packages: pkgs}, "bundle-det", "2026-09-03T12:00:00Z")
		if err != nil {
			t.Fatalf("FromLock: %v", err)
		}
		raw, err := doc.JSON()
		if err != nil {
			t.Fatalf("JSON: %v", err)
		}
		return string(raw), strings.Join(names, ",")
	}

	firstOut, firstOrder := build(1)
	sawADifferentOrder := false
	for seed := int64(2); seed <= 12; seed++ {
		out, order := build(seed)
		if order != firstOrder {
			sawADifferentOrder = true
		}
		if out != firstOut {
			t.Fatalf("seed %d produced different bytes from seed 1; the document depends on input order", seed)
		}
	}
	if !sawADifferentOrder {
		t.Fatal("every seed produced the same input order, so this test proved nothing")
	}
}

// TestDocumentHasNoWallClockOrHostState guards the other half of
// determinism: nothing in the rendered document may come from a clock, a
// random source or this machine. Everything variable is a Build input.
func TestDocumentHasNoWallClockOrHostState(t *testing.T) {
	doc, err := Build(fixtureOptions(), fixtureComponents())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	raw, err := doc.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if doc.Metadata.Timestamp != fixtureOptions().Timestamp {
		t.Errorf("metadata.timestamp = %q, want the caller's %q", doc.Metadata.Timestamp, fixtureOptions().Timestamp)
	}
	if want := deterministicSerialNumber(fixtureOptions().BundleID); doc.SerialNumber != want {
		t.Errorf("serialNumber = %q, want the digest-derived %q", doc.SerialNumber, want)
	}
	// A second Build in the same process must be byte-identical too; this is
	// the cheap guard against a future map or clock creeping in.
	again, err := Build(fixtureOptions(), fixtureComponents())
	if err != nil {
		t.Fatal(err)
	}
	rawAgain, err := again.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(rawAgain) {
		t.Fatal("two Builds from the same inputs produced different bytes")
	}
}

// TestSyftOutputMustMatchTheSpecVersionWeClaim: the file is written as
// sbom.cdx.json and this package's SpecVersion tells consumers which schema
// to validate it against, so accepting syft output that declares a different
// CycloneDX version would ship a document that fails validation elsewhere.
func TestSyftOutputMustMatchTheSpecVersionWeClaim(t *testing.T) {
	defer fakeSyft(t, true)()

	for _, body := range []string{
		`{"bomFormat":"CycloneDX","specVersion":"1.4","components":[]}`,
		`{"bomFormat":"CycloneDX","components":[]}`,
		`{"bomFormat":"SPDX","specVersion":"1.6"}`,
		`not json at all`,
		``,
	} {
		runSyft = func(ctx context.Context, target string) ([]byte, error) { return []byte(body), nil }
		out, src, err := BuildPreferSyft(context.Background(), t.TempDir(), fixtureOptions(), fixtureComponents())
		if err != nil {
			t.Fatalf("BuildPreferSyft(%q): %v", body, err)
		}
		if src != SourceNative {
			t.Errorf("syft output %q was accepted; want a fallback to native", body)
		}
		if !strings.Contains(string(out), `"specVersion": "`+SpecVersion+`"`) {
			t.Errorf("fallback output does not declare specVersion %s", SpecVersion)
		}
	}
}

// TestBuildPreferSyftNeverScansAnUnnamedDirectory: an empty target would
// reach syft as the bare "dir:", which it resolves against its own working
// directory -- a scan of something the caller never asked for.
func TestBuildPreferSyftNeverScansAnUnnamedDirectory(t *testing.T) {
	defer fakeSyft(t, true)()
	called := false
	runSyft = func(ctx context.Context, target string) ([]byte, error) {
		called = true
		return []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[]}`), nil
	}
	if _, src, err := BuildPreferSyft(context.Background(), "", fixtureOptions(), fixtureComponents()); err != nil {
		t.Fatalf("BuildPreferSyft: %v", err)
	} else if src != SourceNative {
		t.Errorf("Source = %s, want native", src)
	}
	if called {
		t.Error("syft was executed for an empty target")
	}
}

// fakeSyft makes both syft seams substitutable for the duration of a test and
// returns the restore func. available is forced rather than read off the real
// PATH: a test that only asserts "we fell back to native" is worthless on a
// machine with no syft, because the branch under test never runs at all.
func fakeSyft(t *testing.T, available bool) func() {
	t.Helper()
	origAvail, origRun := syftAvailable, runSyft
	syftAvailable = func() bool { return available }
	return func() { syftAvailable, runSyft = origAvail, origRun }
}

// TestBuildPreferSyftUsesSyftWhenItIsUsable is the other half of the syft
// gate: with the branch forced on and syft returning a document at the spec
// version this package claims, its bytes are what comes back. Without this the
// "falls back to native" tests could all pass because the syft branch is dead.
func TestBuildPreferSyftUsesSyftWhenItIsUsable(t *testing.T) {
	defer fakeSyft(t, true)()
	const body = `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[]}`
	var sawTarget string
	runSyft = func(ctx context.Context, target string) ([]byte, error) {
		sawTarget = target
		return []byte(body), nil
	}
	dir := t.TempDir()
	out, src, err := BuildPreferSyft(context.Background(), dir, fixtureOptions(), fixtureComponents())
	if err != nil {
		t.Fatalf("BuildPreferSyft: %v", err)
	}
	if src != SourceSyft {
		t.Fatalf("Source = %s, want syft", src)
	}
	if string(out) != body {
		t.Errorf("output = %q, want syft's own bytes", out)
	}
	if sawTarget != dir {
		t.Errorf("syft was given target %q, want %q", sawTarget, dir)
	}
}

// TestBuildPreferSyftFallsBackWhenSyftFails: a missing or failing syft is a
// fallback, never a build failure.
func TestBuildPreferSyftFallsBackWhenSyftFails(t *testing.T) {
	defer fakeSyft(t, true)()
	runSyft = func(ctx context.Context, target string) ([]byte, error) {
		return nil, errFakeSyftMissing
	}
	out, src, err := BuildPreferSyft(context.Background(), t.TempDir(), fixtureOptions(), fixtureComponents())
	if err != nil {
		t.Fatalf("BuildPreferSyft should fall back, not error: %v", err)
	}
	if src != SourceNative {
		t.Errorf("Source = %s, want native", src)
	}
	var probe Document
	if err := json.Unmarshal(out, &probe); err != nil || probe.BOMFormat != BOMFormat {
		t.Errorf("fallback output is not a CycloneDX document: %v", err)
	}
}

// TestFromLockRecordsWhichDebarkProducedIt is the provenance check on
// metadata.tools. An SBOM ships inside a signed bundle next to a manifest
// whose tool block names debark AND its version; before WithToolVersion
// existed, FromLock hardcoded an empty ToolVersion, so the two documents
// disagreed and the SBOM could not say which debark wrote it.
//
// Both halves are asserted, because only asserting the option's effect would
// pass on an implementation that ignored the caller entirely and always wrote
// some version of its own:
//   - with the option, metadata.tools carries exactly that version;
//   - without it, the tools entry still names debark (so the document is
//     not silently anonymous) but carries no invented version.
func TestFromLockRecordsWhichDebarkProducedIt(t *testing.T) {
	l := &lock.Lock{
		Target:   lock.Target{DistroID: "debian", Codename: "bookworm", Arch: "amd64"},
		Packages: []lock.Package{{Name: "libc6", Version: "2.36-9", Arch: "amd64", SHA256: strings.Repeat("a", 64)}},
	}

	doc, err := FromLock(l, "bundle-x", "2026-09-03T12:00:00Z", WithToolVersion("1.4.2"))
	if err != nil {
		t.Fatalf("FromLock: %v", err)
	}
	if doc.Metadata == nil || doc.Metadata.Tools == nil || len(doc.Metadata.Tools.Components) != 1 {
		t.Fatalf("metadata.tools should hold exactly one component, got %+v", doc.Metadata)
	}
	tool := doc.Metadata.Tools.Components[0]
	if tool.Name != "debark" {
		t.Errorf("metadata.tools names %q, want debark", tool.Name)
	}
	if tool.Version != "1.4.2" {
		t.Errorf("metadata.tools version = %q, want 1.4.2 — the SBOM does not say which debark produced it", tool.Version)
	}
	// The version must survive rendering, not just live on the struct: the
	// bytes are what goes in the bundle.
	raw, err := doc.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if !strings.Contains(string(raw), `"version": "1.4.2"`) {
		t.Errorf("rendered SBOM does not carry the tool version:\n%s", raw)
	}

	bare, err := FromLock(l, "bundle-x", "2026-09-03T12:00:00Z")
	if err != nil {
		t.Fatalf("FromLock (no option): %v", err)
	}
	bareTool := bare.Metadata.Tools.Components[0]
	if bareTool.Name != "debark" {
		t.Errorf("without the option metadata.tools names %q, want debark", bareTool.Name)
	}
	if bareTool.Version != "" {
		t.Errorf("without the option metadata.tools invented version %q", bareTool.Version)
	}
}

// TestFromLockWithToolVersionStaysDeterministic keeps the option inside the
// guarantee the rest of this package is held to: core/sbom ships inside the
// signed bundle, so the same inputs must render byte-identical output. The
// option adds a field to metadata; this proves it added no non-determinism
// with it.
func TestFromLockWithToolVersionStaysDeterministic(t *testing.T) {
	l := &lock.Lock{
		Target: lock.Target{DistroID: "ubuntu", Codename: "noble", Arch: "amd64"},
		Packages: []lock.Package{
			{Name: "zeta", Arch: "amd64", Version: "1:2.0~rc1", SHA256: strings.Repeat("1", 64), SourcePackage: "zeta-src"},
			{Name: "alpha", Arch: "arm64", Version: "0.1+deb12u1", SHA256: strings.Repeat("2", 64)},
		},
	}
	var prev []byte
	for i := 0; i < 5; i++ {
		doc, err := FromLock(l, "bundle-det", "2026-09-03T12:00:00Z", WithToolVersion("1.4.2"))
		if err != nil {
			t.Fatalf("FromLock: %v", err)
		}
		raw, err := doc.JSON()
		if err != nil {
			t.Fatalf("JSON: %v", err)
		}
		if i > 0 && !bytes.Equal(raw, prev) {
			t.Fatalf("run %d differs from run %d; FromLock is not byte-reproducible", i, i-1)
		}
		prev = raw
	}
}
