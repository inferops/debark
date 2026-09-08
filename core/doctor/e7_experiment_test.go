package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestE7NetworkPostinstSample is experiment E7: measure
// network-postinst's real-world false-positive rate. It scans
// every .deb in DEBARK_E7_SAMPLE_DIR (recursively one level, so a directory
// of per-distro subdirectories works too) with exactly the production
// scanning path (openDebFile + scanScriptForNetwork) and reports counts.
//
// It is skipped by default — this is a measurement tool, not a correctness
// test (TestScanScriptForNetwork and TestParseDebReader_RealXZFixture cover
// correctness) — and needs a real package sample to be useful. See
// docs/experiments/E7-network-postinst.md for exactly how the sample used in
// that writeup was produced (apt-get download against live Debian 12 and
// Ubuntu 24.04 archives in Docker containers) and for the actual numbers.
//
// To reproduce:
//
//	DEBARK_E7_SAMPLE_DIR=/path/to/debs go test ./core/doctor/... -run TestE7NetworkPostinstSample -v
func TestE7NetworkPostinstSample(t *testing.T) {
	root := os.Getenv("DEBARK_E7_SAMPLE_DIR")
	if root == "" {
		t.Skip("set DEBARK_E7_SAMPLE_DIR to a directory of .deb files (optionally containing subdirectories) to run experiment E7")
	}

	var debPaths []string
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	collect := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".deb") {
				debPaths = append(debPaths, filepath.Join(dir, e.Name()))
			}
		}
	}
	collect(root)
	for _, e := range rootEntries {
		if e.IsDir() {
			collect(filepath.Join(root, e.Name()))
		}
	}
	sort.Strings(debPaths)
	if len(debPaths) == 0 {
		t.Fatalf("no .deb files found under %s", root)
	}

	type matchRecord struct {
		File    string
		Matches []networkMatch
	}
	var (
		scanned       int
		unscannable   int
		unscannableBy = map[string]int{}
		matches       []matchRecord
		signalCounts  = map[string]int{}
		compression   = map[string]int{}
	)

	for _, p := range debPaths {
		df, err := openDebFile(p)
		if err != nil {
			t.Logf("skip %s: %v", filepath.Base(p), err)
			continue
		}
		compression[df.ControlCompression]++
		if df.Unscannable != "" {
			unscannable++
			unscannableBy[df.Unscannable]++
			continue
		}
		scanned++
		var all []networkMatch
		for _, script := range maintainerScripts {
			content, ok := df.Scripts[script]
			if !ok {
				continue
			}
			all = append(all, scanScriptForNetwork(script, content)...)
		}
		if len(all) > 0 {
			for _, m := range all {
				signalCounts[m.Signal]++
			}
			matches = append(matches, matchRecord{File: filepath.Base(p), Matches: all})
		}
	}

	t.Logf("E7: %d .deb files found, %d scanned, %d unscannable %v, %d matched (%.2f%% of scanned)",
		len(debPaths), scanned, unscannable, unscannableBy, len(matches), pct(len(matches), scanned))
	t.Logf("E7: control.tar compression: %v", compression)
	t.Logf("E7: signal counts: %v", signalCounts)
	for _, m := range matches {
		for _, sig := range m.Matches {
			t.Logf("E7 MATCH %s | %s", m.File, sig.String())
		}
	}

	if out := os.Getenv("DEBARK_E7_REPORT_JSON"); out != "" {
		report := struct {
			TotalDebs     int            `json:"total_debs"`
			Scanned       int            `json:"scanned"`
			Unscannable   int            `json:"unscannable"`
			UnscannableBy map[string]int `json:"unscannable_by_kind"`
			Compression   map[string]int `json:"control_compression"`
			Matched       int            `json:"matched"`
			SignalCounts  map[string]int `json:"signal_counts"`
			Matches       []matchRecord  `json:"matches"`
		}{len(debPaths), scanned, unscannable, unscannableBy, compression, len(matches), signalCounts, matches}
		b, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Logf("write report json: %v", err)
		}
	}
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}
