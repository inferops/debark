package doctor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/inferops/debark/core/lock"
)

// These tests all attack the same surface from different directions: the
// parse-and-decompress path `debark doctor BUNDLE` reaches on an extracted
// bundle that verify.Verify has not looked at. doctor is the command an
// operator runs FIRST, on media that just arrived, to decide whether to trust
// it, so every bound below has to hold against a file whose every byte the
// attacker chose. They mirror core/bundle/import_bomb_test.go and
// core/fetch/control_bomb_test.go, which cover the same class on the paths
// those packages own.

// arHeaderWithSize builds one ar member header whose declared size need not
// match the bytes that follow it. buildAr (fixture_test.go) always tells the
// truth about a member's length, which is exactly what these tests must not
// do.
func arHeaderWithSize(name, sizeStr string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%-16s", name)
	fmt.Fprintf(&b, "%-12d", 0)       // mtime
	fmt.Fprintf(&b, "%-6d", 0)        // uid
	fmt.Fprintf(&b, "%-6d", 0)        // gid
	fmt.Fprintf(&b, "%-8s", "100644") // mode
	fmt.Fprintf(&b, "%-10s", sizeStr) // size — the field under test
	b.WriteString("`\n")
	return b.Bytes()
}

// totalAlloc is the process's cumulative allocation counter. Cumulative, not
// live heap: it cannot be hidden by a GC running between the two reads, which
// is what makes it a usable assertion about "did this allocate a gigabyte".
func totalAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

// TestParseArIgnoresDeclaredSizeWhenAllocating is the regression test for the
// worst thing found in this package: parseAr used to size its buffer from the
// ar member header's declared length before reading a single byte of the
// member, so `make([]byte, size)` allocated whatever a 10-character decimal
// field said.
//
// Measured on the unfixed code: this exact 69-byte payload produced
// 10,000,759,040 bytes of heap and took 10,022,283,512 bytes from the OS in
// 17ms. On a target with less committable memory than that — an air-gapped
// machine is not usually a build server — the Go runtime raises a fatal,
// unrecoverable out-of-memory instead, which the CLI cannot turn into an exit
// class because the process is already gone.
//
// The fix is not a policy choice about sizes: parseAr reads through io.CopyN
// into a growing buffer, so the cost follows the bytes that actually exist.
// The assertion is therefore about allocation, not about the error — parseAr
// returned an error before the fix too, just after allocating 9.3 GiB to do
// it. This is also not a purely hostile-input path: E7 hit a genuinely
// truncated 30 MB package whose declared member size overran its bytes.
func TestParseArIgnoresDeclaredSizeWhenAllocating(t *testing.T) {
	var payload bytes.Buffer
	payload.WriteString(arMagic)
	payload.Write(arHeaderWithSize("data.tar.gz", "9999999999")) // ~9.31 GiB claimed
	payload.WriteString("x")                                     // one byte actually present

	if payload.Len() > 128 {
		t.Fatalf("test setup bug: payload is %d bytes, it is meant to be tiny", payload.Len())
	}

	runtime.GC()
	before := totalAlloc()
	_, err := parseAr(bytes.NewReader(payload.Bytes()))
	allocated := totalAlloc() - before

	if err == nil {
		t.Fatal("parseAr accepted a member whose declared size overruns the file")
	}
	// Three orders of magnitude below the 9.3 GiB the declared size asks for,
	// and far above anything this 69-byte input can legitimately need.
	const budget = 8 << 20
	if allocated > budget {
		t.Fatalf("parseAr allocated %d bytes for a %d-byte input declaring %s bytes; budget is %d",
			allocated, payload.Len(), "9999999999", budget)
	}
	t.Logf("%d-byte input declaring 9999999999 bytes cost %d bytes of allocation", payload.Len(), allocated)
}

// TestParseArRefusesNegativeMemberSize covers the sibling of the case above:
// a negative size parses fine as an int64, and the old code reported it
// through a branch that wrapped a nil error with %w, rendering the message as
// "%!w(<nil>)".
func TestParseArRefusesNegativeMemberSize(t *testing.T) {
	var payload bytes.Buffer
	payload.WriteString(arMagic)
	payload.Write(arHeaderWithSize("data.tar.gz", "-1"))

	_, err := parseAr(bytes.NewReader(payload.Bytes()))
	if err == nil {
		t.Fatal("parseAr accepted a negative member size")
	}
	if strings.Contains(err.Error(), "%!w") {
		t.Errorf("error message is malformed: %v", err)
	}
}

// TestParseArRefusesTooManyMembers protects the dimension a per-member size
// bound leaves open: an archive that is nothing but member headers. Debian
// Policy §5.6.1 fixes a real .deb at three members, so maxArMembers at 64 is
// an order of magnitude above anything legitimate.
func TestParseArRefusesTooManyMembers(t *testing.T) {
	var payload bytes.Buffer
	payload.WriteString(arMagic)
	for i := 0; i <= maxArMembers; i++ {
		payload.Write(arHeaderWithSize(fmt.Sprintf("m%d", i), "0"))
	}
	if _, err := parseAr(bytes.NewReader(payload.Bytes())); err == nil {
		t.Fatalf("parseAr accepted an archive with more than %d members", maxArMembers)
	}

	// The boundary itself must still be accepted, or the bound is off by one
	// in the direction that rejects real input.
	var ok bytes.Buffer
	ok.WriteString(arMagic)
	for i := 0; i < maxArMembers; i++ {
		ok.Write(arHeaderWithSize(fmt.Sprintf("m%d", i), "0"))
	}
	members, err := parseAr(bytes.NewReader(ok.Bytes()))
	if err != nil {
		t.Fatalf("parseAr refused exactly %d members: %v", maxArMembers, err)
	}
	if len(members) != maxArMembers {
		t.Fatalf("got %d members, want %d", len(members), maxArMembers)
	}
}

// TestParseArRefusesOversizedControlMember checks the half of the byte budget
// that must REFUSE rather than truncate. Silently keeping a prefix of the
// control member could drop the very maintainer script whose absence would
// then read as "no obvious network action found" — a false clean result, the
// one outcome this package must never produce. core/fetch's maxControlSize
// makes the same call for the same reason on the decompressed side.
func TestParseArRefusesOversizedControlMember(t *testing.T) {
	var payload bytes.Buffer
	payload.WriteString(arMagic)
	payload.Write(arHeaderWithSize("control.tar.gz", fmt.Sprint(maxControlMemberSize+1)))
	// No need to back it with real bytes: the refusal must happen on the
	// declared size, before anything is read.
	_, err := parseAr(bytes.NewReader(payload.Bytes()))
	if err == nil {
		t.Fatalf("parseAr accepted a control member declaring %d bytes", maxControlMemberSize+1)
	}
	if !strings.Contains(err.Error(), "refusing rather than silently truncating") {
		t.Errorf("error should say it refused rather than truncated, got: %v", err)
	}
}

// TestParseArTruncatesOversizedDataMember checks the other half. The data
// member is only ever listed, best-effort, looking for usr/src/*/dkms.conf,
// and parseDebReader stops after scanByteLimit DECOMPRESSED bytes of it — so
// buffering scanByteLimit COMPRESSED bytes can never change a result (no
// compressor expands its input) while capping what a multi-gigabyte data
// member costs in memory.
func TestParseArTruncatesOversizedDataMember(t *testing.T) {
	oversize := maxDataMemberPrefix + 4096
	body := make([]byte, oversize)
	raw := append([]byte(arMagic), arHeaderWithSize("data.tar", fmt.Sprint(oversize))...)
	raw = append(raw, body...)

	members, err := parseAr(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parseAr refused an oversized data member instead of truncating it: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("got %d members, want 1", len(members))
	}
	if !members[0].Truncated {
		t.Error("member should be marked Truncated")
	}
	if int64(len(members[0].Data)) != maxDataMemberPrefix {
		t.Errorf("buffered %d bytes, want the %d-byte prefix", len(members[0].Data), maxDataMemberPrefix)
	}
}

// zstdFrameDeclaringWindow hand-builds a zstd frame header that declares a
// window of 1<<(10+exponent) bytes with essentially no content behind it. The
// encoder cannot produce this: it only ever declares a window it really used,
// and the point of the test is a frame that lies.
func zstdFrameDeclaringWindow(exponent byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(0xFD2FB528))
	b.WriteByte(0x00)          // no content size, not single-segment, no dictionary id
	b.WriteByte(exponent << 3) // window descriptor: windowLog = 10 + exponent
	blockHeader := uint32(1)<<3 | 1
	b.WriteByte(byte(blockHeader))
	b.WriteByte(byte(blockHeader >> 8))
	b.WriteByte(byte(blockHeader >> 16))
	b.WriteByte('x')
	return b.Bytes()
}

// TestZstdWindowIsBounded protects maxZstdWindow. klauspost/compress
// allocates a frame's declared window before any content arrives, and its
// default ceiling is 512 MiB — measured here as a 10-byte frame turning into
// a 537,921,440-byte allocation in 178ms, for a member doctor then reads at
// most scanByteLimit bytes of. E7 found 812 of 1,749 real packages use zstd
// for their control member, so this decoder runs on almost every .deb doctor
// ever opens.
func TestZstdWindowIsBounded(t *testing.T) {
	// windowLog 29 (1<<29 = 512 MiB), the largest klauspost's default allows.
	frame := zstdFrameDeclaringWindow(19)

	runtime.GC()
	before := totalAlloc()
	rc, unsupported, err := decompressMember(arMember{Name: "control.tar.zst", Data: frame})
	var readErr error
	if err == nil && unsupported == "" {
		_, readErr = io.Copy(io.Discard, io.LimitReader(rc, scanByteLimit))
		rc.Close()
	}
	allocated := totalAlloc() - before

	if err == nil && readErr == nil {
		t.Errorf("a %d-byte frame declaring a 512 MiB window should be refused, not decoded", len(frame))
	}
	// Comfortably above the 64 MiB window maxZstdWindow permits plus decoder
	// scratch, and an order of magnitude below the 512 MiB the frame asks for.
	const budget = 192 << 20
	if allocated > budget {
		t.Fatalf("decoding a %d-byte frame declaring a 512 MiB window allocated %d bytes; budget is %d",
			len(frame), allocated, budget)
	}
	t.Logf("%d-byte frame declaring a 512 MiB window cost %d bytes of allocation (err=%v readErr=%v)",
		len(frame), allocated, err, readErr)
}

// TestZstdStillReadsRealPackages is the other side of maxZstdWindow: a bound
// that rejected real .deb files would be worse than the bomb. dpkg-deb's
// default is `zstd -19`, whose windowLog is 23 (8 MiB) — well inside the
// 64 MiB cap — and this checks a genuine round trip rather than only that the
// hostile case fails, so the bound cannot be "tightened" into uselessness
// without a test noticing.
func TestZstdStillReadsRealPackages(t *testing.T) {
	deb := buildFixtureDebZstd(t, fixtureDeb{
		Scripts: map[string]string{"postinst": "#!/bin/sh\ncurl http://example.invalid/x\n"},
	})
	d, err := parseDebReader(bytes.NewReader(deb))
	if err != nil {
		t.Fatalf("parseDebReader on a zstd .deb: %v", err)
	}
	if d.ControlCompression != "zstd" {
		t.Fatalf("ControlCompression = %q, want zstd", d.ControlCompression)
	}
	if !strings.Contains(d.Scripts["postinst"], "curl") {
		t.Fatalf("postinst not recovered from a zstd control member: %q", d.Scripts["postinst"])
	}
}

// scriptBombDeb builds a .deb whose postinst decompresses to nearly
// scanByteLimit bytes of lines that all match the network heuristic. It is
// the shape that turned doctor into a memory bomb: highly compressible, so
// the file on media stays tiny, and every line produces a finding.
func scriptBombDeb(t *testing.T) []byte {
	t.Helper()
	const line = "curl http://a.example/x\n"
	script := strings.Repeat(line, (scanByteLimit-4096)/len(line))

	var gz bytes.Buffer
	gw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	tw := tar.NewWriter(gw)
	for _, f := range []struct {
		name, body string
	}{{"./control", defaultControl}, {"./postinst", script}} {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header %s: %v", f.name, err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatalf("tar write %s: %v", f.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buildAr([]arMember{
		{Name: "debian-binary", Data: []byte("2.0\n")},
		{Name: "control.tar.gz", Data: gz.Bytes()},
	})
}

// TestScanScriptForNetworkCapsMatches protects maxScriptMatches directly.
// Without it an 8 MiB maintainer script produced 699,048 matches in 714ms;
// E7's 1,749 real packages produced 6 in total, the busiest single package
// producing 2.
func TestScanScriptForNetworkCapsMatches(t *testing.T) {
	content := strings.Repeat("curl http://a.example/x\n", 100_000)
	start := time.Now()
	got := scanScriptForNetwork("postinst", content)
	elapsed := time.Since(start)

	if len(got) > maxScriptMatches {
		t.Fatalf("scanScriptForNetwork returned %d matches, cap is %d", len(got), maxScriptMatches)
	}
	if len(got) != maxScriptMatches {
		t.Fatalf("got %d matches, want exactly the cap (%d) for input that matches on every line",
			len(got), maxScriptMatches)
	}
	// The cap must stop the SCAN, not merely stop appending: the walk over
	// 8 MiB of lines is the other half of the cost. Reaching the cap after
	// 256 lines should be effectively instant.
	if elapsed > 2*time.Second {
		t.Errorf("scan took %v after hitting the cap; it should stop scanning, not just stop appending", elapsed)
	}
	t.Logf("%d-byte script capped at %d matches in %v", len(content), len(got), elapsed)
}

// TestScanScriptForNetworkKeepsRealMatches is maxScriptMatches's other side:
// the cap must be invisible to every real package. Both genuine positives E7
// found in 1,749 real packages are reproduced here in the shape they actually
// appear (astrometry-data-2mass-07's BASE_URL + curl, and
// cpl-plugin-vimos-calib's bare wget), and both must still be reported in
// full.
func TestScanScriptForNetworkKeepsRealMatches(t *testing.T) {
	const script = `#!/bin/sh
set -e
SERIES=2mass
BASE_URL=http://data.astrometry.net/${SERIES}00
curl --create-dirs -o "${TARGETDIR}/index.fits" "${BASE_URL}/index.fits"
wget -O- ${URL} | tar xzC ${TARGETDIR}
`
	got := scanScriptForNetwork("postinst", script)
	if len(got) == 0 {
		t.Fatal("no matches on a script with two genuine network fetches")
	}
	if len(got) >= maxScriptMatches {
		t.Fatalf("a five-line real-world script produced %d matches; the cap must be nowhere near real input", len(got))
	}
	lines := map[int]bool{}
	for _, m := range got {
		lines[m.Line] = true
	}
	for _, want := range []int{5, 6} { // the curl line and the wget line
		if !lines[want] {
			t.Errorf("line %d not reported; got matches on lines %v", want, lines)
		}
	}
}

// TestRunSurvivesScriptBombAcrossPackages is the end-to-end regression for
// the whole chain, and the one that measured a process death. Run accumulates
// findings across every package in the lock, and both the .deb and the lock
// come from unverified media, so the two multiply.
//
// Unfixed, with this same 20 KB .deb: 20 packages produced 6,987,080 findings
// and 1,742 MiB of live heap in 39s, and 200 packages died on `fatal error:
// runtime: cannot allocate memory` — the process gone, exhausting the host's
// pagefile on the way. 20 packages is used here because it was measured, is
// bounded work now, and would still have been a 1.7 GiB test before the fix.
func TestRunSurvivesScriptBombAcrossPackages(t *testing.T) {
	const packages = 20

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "repo", "pool"), 0o755); err != nil {
		t.Fatal(err)
	}
	deb := scriptBombDeb(t)
	if err := os.WriteFile(filepath.Join(dir, "repo", "pool", "bomb.deb"), deb, 0o644); err != nil {
		t.Fatal(err)
	}

	l := &lock.Lock{}
	for i := 0; i < packages; i++ {
		l.Packages = append(l.Packages, lock.Package{
			Name: fmt.Sprintf("p%03d", i), Version: "1.0", Filename: "pool/bomb.deb",
		})
	}

	runtime.GC()
	before := totalAlloc()
	start := time.Now()
	report, err := Run(context.Background(), Input{BundleDir: dir, Lock: l, ScanScripts: true})
	elapsed := time.Since(start)
	allocated := totalAlloc() - before

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Four maintainer scripts per package, each capped at maxScriptMatches,
	// is the arithmetic ceiling; this .deb only has a postinst.
	if max := packages * len(maintainerScripts) * maxScriptMatches; len(report.Findings) > max {
		t.Fatalf("Run produced %d findings from %d packages; ceiling is %d", len(report.Findings), packages, max)
	}
	// 1 GiB is far above what the bounded path needs and far below the
	// 1,742 MiB of LIVE heap (16 GiB cumulative) the unfixed path reached.
	const budget = 1 << 30
	if allocated > budget {
		t.Fatalf("Run allocated %d bytes for %d packages naming a %d-byte .deb; budget is %d",
			allocated, packages, len(deb), budget)
	}
	t.Logf("%d packages naming one %d-byte .deb: %d findings, %d bytes allocated, %v",
		packages, len(deb), len(report.Findings), allocated, elapsed)
}

// TestParseDeb822FoldedFieldIsLinear is the regression test for the slowest
// thing found in this package: parseDeb822 accumulated a folded (continuation-
// line) field with `cur[lastKey] += "\n" + cont`, rebuilding the whole value
// on every line, so a field folded over n lines copied O(n^2) bytes.
//
// Measured on the `+=` version, one field and nothing but continuation lines:
// 65,538 bytes cost 0.08s and 517,533,024 bytes of allocation; each doubling
// of the input quadrupled both, reaching 31.21s and 491,480,352,480 bytes at
// 2,097,153 bytes of input. scanByteLimit lets a control file reach 8 MiB,
// four more doublings — about eight minutes and 7.5 TiB of allocation churn
// for ONE package, out of a .deb of 8,420 bytes, because the payload is
// nothing but repeated bytes and gzips to nearly nothing. On the fixed parser
// that same 8,420-byte .deb parses in 100ms.
//
// The assertion is on allocation rather than on wall time: allocation is what
// the shape of the bug actually determines, and it does not depend on how
// busy the machine running the test is. 2 MiB is used rather than the full
// 8 MiB so that a regression fails in ~31s instead of ~8 minutes.
func TestParseDeb822FoldedFieldIsLinear(t *testing.T) {
	var doc bytes.Buffer
	doc.WriteString("Package: x\nDescription: short\n")
	for doc.Len() < 2<<20 {
		doc.WriteString(" a\n")
	}

	runtime.GC()
	before := totalAlloc()
	start := time.Now()
	stanzas, err := parseDeb822(bytes.NewReader(doc.Bytes()))
	elapsed := time.Since(start)
	allocated := totalAlloc() - before

	if err != nil {
		t.Fatalf("parseDeb822: %v", err)
	}
	if len(stanzas) != 1 {
		t.Fatalf("got %d stanzas, want 1", len(stanzas))
	}
	// The folded value must still be complete — a bound that silently dropped
	// the tail of a Description would break checkSnapShim's transitional-
	// package signal.
	if got, want := len(stanzas[0]["Description"]), 1_398_087; got != want {
		t.Errorf("Description is %d bytes, want %d (the whole folded field)", got, want)
	}
	// 64 MiB is 8x what the linear parser needs for this input and ~7,600x
	// below the 491 GiB the quadratic one spent on exactly these bytes.
	const budget = 64 << 20
	if allocated > budget {
		t.Fatalf("parseDeb822 allocated %d bytes for a %d-byte folded field; budget is %d",
			allocated, doc.Len(), budget)
	}
	t.Logf("%d-byte folded field parsed in %v for %d bytes of allocation", doc.Len(), elapsed, allocated)
}

// TestParseDeb822RefusesStanzaFlood protects maxDeb822Stanzas, the dimension
// the linear fix above leaves open: not one enormous field but an enormous
// number of tiny stanzas. Measured before the cap, 8,384,515 bytes of
// "P:1\n\n" — the most scanByteLimit allows, about 30 KB once gzipped inside
// a .deb — became 1,676,903 stanza maps costing 639,428,320 bytes, a 610 MiB
// heap spike out of ~30 KB of media, for a control file doctor reads exactly
// one stanza of.
func TestParseDeb822RefusesStanzaFlood(t *testing.T) {
	var doc bytes.Buffer
	for doc.Len() < scanByteLimit-4096 {
		doc.WriteString("P:1\n\n")
	}

	runtime.GC()
	before := totalAlloc()
	_, err := parseDeb822(bytes.NewReader(doc.Bytes()))
	allocated := totalAlloc() - before

	if err == nil {
		t.Fatalf("parseDeb822 accepted a document with far more than %d stanzas", maxDeb822Stanzas)
	}
	// Comfortably above what reading maxDeb822Stanzas stanzas costs and far
	// below the 610 MiB the uncapped parser spent.
	const budget = 128 << 20
	if allocated > budget {
		t.Fatalf("parseDeb822 allocated %d bytes before refusing; budget is %d", allocated, budget)
	}

	// The cap must be nowhere near a real /var/lib/dpkg/status, which has one
	// stanza per installed package — a few thousand on a full desktop.
	var real bytes.Buffer
	for i := 0; i < 20_000; i++ {
		fmt.Fprintf(&real, "Package: p%d\nStatus: install ok installed\n\n", i)
	}
	stanzas, err := parseDeb822(bytes.NewReader(real.Bytes()))
	if err != nil {
		t.Fatalf("parseDeb822 refused a 20,000-package dpkg status: %v", err)
	}
	if len(stanzas) != 20_000 {
		t.Fatalf("got %d stanzas, want 20000", len(stanzas))
	}
}

// TestParseArWholeArchiveBudget protects maxArTotalBuffered, the dimension
// the per-member budgets leave open between them: maxArMembers permits 64
// members and each may be worth 8 MiB, so the two multiply. Measured before
// the whole-archive budget, a 536,874,760-byte ar of 64 members each at the
// control budget returned 536,870,912 bytes of member data and 1,025 MiB of
// live heap in 529ms.
func TestParseArWholeArchiveBudget(t *testing.T) {
	var payload bytes.Buffer
	payload.WriteString(arMagic)
	body := make([]byte, maxControlMemberSize)
	for i := 0; i < maxArMembers; i++ {
		payload.Write(arHeaderWithSize(fmt.Sprintf("m%d.tar.gz", i), fmt.Sprint(maxControlMemberSize)))
		payload.Write(body)
	}

	runtime.GC()
	before := totalAlloc()
	members, err := parseAr(bytes.NewReader(payload.Bytes()))
	allocated := totalAlloc() - before

	var held int64
	for _, m := range members {
		held += int64(len(m.Data))
	}
	if err == nil {
		t.Errorf("parseAr accepted %d members totalling %d bytes", len(members), held)
	}
	if held > maxArTotalBuffered {
		t.Errorf("parseAr buffered %d bytes across members; the whole-archive budget is %d",
			held, maxArTotalBuffered)
	}
	// Allocation, not just the returned members, is what this bounds: the old
	// code spent 2,048 MiB getting to its 512 MiB of member data, and the
	// bounded one spends 100,670,776. The gap between the 24 MiB budget and
	// that 96 MiB is bytes.Buffer's doubling, which is deliberate — parseAr
	// must never pre-size a buffer from a length the archive declared, so it
	// pays growth instead. 192 MiB leaves room for that and is still ~10x
	// below the unbounded figure.
	const budget = 192 << 20
	if allocated > budget {
		t.Fatalf("parseAr allocated %d bytes for a %d-byte ar of oversized members; budget is %d",
			allocated, payload.Len(), budget)
	}
	t.Logf("a %d-byte ar of %d oversized members buffered %d bytes for %d bytes of allocation (err=%v)",
		payload.Len(), maxArMembers, held, allocated, err)
}

// TestParseArStillReadsARealDeb is the other side of maxArTotalBuffered: a
// real .deb's three members must be entirely unaffected by it.
func TestParseArStillReadsARealDeb(t *testing.T) {
	deb := buildFixtureDeb(t, fixtureDeb{
		Scripts: map[string]string{"postinst": "#!/bin/sh\ncurl http://example.invalid/x\n"},
	})
	d, err := parseDebReader(bytes.NewReader(deb))
	if err != nil {
		t.Fatalf("parseDebReader on an ordinary .deb: %v", err)
	}
	if !strings.Contains(d.Scripts["postinst"], "curl") {
		t.Fatalf("postinst not recovered: %q", d.Scripts["postinst"])
	}
}
