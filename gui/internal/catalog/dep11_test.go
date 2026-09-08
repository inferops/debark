package catalog

// Tests for the DEP-11 / AppStream parser.
//
// Everything here runs against byte-exact slices of the real Ubuntu noble and
// Debian bookworm archives in ../../testdata, served over httptest. No test
// touches the network.
//
// The load-bearing test is TestDep11TypeErrorIsNotFatal. Everything else
// checks a field; that one checks that the applications tier does not silently
// lose 83% of itself (docs/dev/index-formats.md §7.3).

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	dep11FixtureUbuntuMain     = "../../testdata/ubuntu-noble-main-Components-amd64.yml.excerpt"
	dep11FixtureUbuntuUniverse = "../../testdata/ubuntu-noble-universe-Components-amd64.yml.excerpt"
	dep11FixtureDebianMain     = "../../testdata/debian-bookworm-main-Components-amd64.yml.excerpt"
)

func dep11ReadFixture(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// dep11TestRef is a ref whose suite/component match the Ubuntu main fixture,
// so icon references in assertions read like the real thing.
func dep11TestRef(suite, component string) IndexRef {
	return IndexRef{
		URI:       "https://archive.ubuntu.com/ubuntu",
		Suite:     suite,
		Component: component,
		Arch:      "amd64",
		Kind:      IndexDEP11,
	}
}

// dep11DecodeString decodes a stream and fails the test on a hard error.
func dep11DecodeString(t testing.TB, s string, ref IndexRef) *dep11Index {
	t.Helper()
	idx := dep11NewIndex()
	if err := dep11Decode(context.Background(), strings.NewReader(s), ref, idx); err != nil {
		t.Fatalf("dep11Decode: unexpected error: %v", err)
	}
	return idx
}

func dep11MustLookup(t testing.TB, idx *dep11Index, pkg string) dep11App {
	t.Helper()
	a, ok := idx.Lookup(pkg)
	if !ok {
		t.Fatalf("package %q not in the DEP-11 index (index has %d packages)", pkg, idx.Len())
	}
	return a
}

func dep11EqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The stream shape: header documents and per-fixture totals
// ---------------------------------------------------------------------------

// TestDep11Header covers §7.1: the first document is a header, identified by
// its File: key, it never becomes an application, and Version is a string
// ('0.14' / '0.16') and not a number.
func TestDep11Header(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    dep11Header
	}{
		{
			name:    "ubuntu noble main",
			fixture: dep11FixtureUbuntuMain,
			want: dep11Header{
				File:         "DEP-11",
				Version:      "0.14",
				Origin:       "ubuntu-noble-main",
				MediaBaseURL: "https://appstream.ubuntu.com/media/noble",
				Time:         "20231112T224135",
			},
		},
		{
			name:    "ubuntu noble universe",
			fixture: dep11FixtureUbuntuUniverse,
			want: dep11Header{
				File:         "DEP-11",
				Version:      "0.14",
				Origin:       "ubuntu-noble-universe",
				MediaBaseURL: "https://appstream.ubuntu.com/media/noble",
				Time:         "20231112T224353",
			},
		},
		{
			// Debian ships DEP-11 0.16 where Ubuntu noble ships 0.14. Neither
			// value may be hard-coded and neither is a float.
			name:    "debian bookworm main",
			fixture: dep11FixtureDebianMain,
			want: dep11Header{
				File:         "DEP-11",
				Version:      "0.16",
				Origin:       "debian-bookworm-main",
				MediaBaseURL: "https://appstream.debian.org/media/bookworm",
				Time:         "20230609T082314",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx := dep11DecodeString(t, dep11ReadFixture(t, tc.fixture), dep11TestRef("noble", "main"))

			headers := idx.Headers()
			if len(headers) != 1 {
				t.Fatalf("headers = %d, want 1", len(headers))
			}
			if headers[0] != tc.want {
				t.Errorf("header =\n  %+v\nwant\n  %+v", headers[0], tc.want)
			}
			if got := idx.Stats().Headers; got != 1 {
				t.Errorf("stats.Headers = %d, want 1", got)
			}
			// A header carries no Package:, so it must not have produced a row.
			if _, ok := idx.Lookup(""); ok {
				t.Error("the header document became an index entry")
			}
			for _, a := range idx.Apps() {
				if a.Package == "" || a.Type == "" {
					t.Errorf("index holds a non-component entry: %+v", a)
				}
			}
		})
	}
}

// TestDep11FixtureTotals decodes each real fixture whole and pins the counts.
// A change in any of these numbers means the parser started or stopped seeing
// something, which is exactly the class of regression §7.3 warns about.
func TestDep11FixtureTotals(t *testing.T) {
	tests := []struct {
		name           string
		fixture        string
		wantDocuments  int
		wantHeaders    int
		wantComponents int
		wantAddons     int
		wantMerged     int
		wantApps       int
		wantByType     map[string]int
	}{
		{
			name:           "ubuntu noble main",
			fixture:        dep11FixtureUbuntuMain,
			wantDocuments:  33,
			wantHeaders:    1,
			wantComponents: 32,
			wantAddons:     2,
			wantMerged:     30,
			wantApps:       9,
			wantByType: map[string]int{
				"desktop-application": 9,
				"font":                11,
				"inputmethod":         4,
				"generic":             3,
				"addon":               2,
				"codec":               2,
				"console-application": 1,
			},
		},
		{
			name:           "ubuntu noble universe",
			fixture:        dep11FixtureUbuntuUniverse,
			wantDocuments:  2,
			wantHeaders:    1,
			wantComponents: 1,
			wantMerged:     1,
			wantApps:       1,
			wantByType:     map[string]int{"desktop-application": 1},
		},
		{
			name:           "debian bookworm main",
			fixture:        dep11FixtureDebianMain,
			wantDocuments:  13,
			wantHeaders:    1,
			wantComponents: 12,
			wantAddons:     5,
			wantMerged:     7,
			wantApps:       5,
			wantByType: map[string]int{
				"desktop-application": 5,
				"addon":               5,
				"codec":               1,
				"generic":             1,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx := dep11DecodeString(t, dep11ReadFixture(t, tc.fixture), dep11TestRef("noble", "main"))
			s := idx.Stats()

			for _, got := range []struct {
				what string
				have int
				want int
			}{
				{"Documents", s.Documents, tc.wantDocuments},
				{"Headers", s.Headers, tc.wantHeaders},
				{"Components", s.Components, tc.wantComponents},
				{"Addons", s.Addons, tc.wantAddons},
				{"Merged", s.Merged, tc.wantMerged},
				{"Apps", s.Apps, tc.wantApps},
				{"Len()", idx.Len(), tc.wantMerged},
			} {
				if got.have != got.want {
					t.Errorf("%s = %d, want %d", got.what, got.have, got.want)
				}
			}
			for typ, want := range tc.wantByType {
				if got := s.ByType[typ]; got != want {
					t.Errorf("ByType[%q] = %d, want %d", typ, got, want)
				}
			}
			if len(s.ByType) != len(tc.wantByType) {
				t.Errorf("ByType = %v, want exactly %v", s.ByType, tc.wantByType)
			}
			// Real fixtures must not need the soft-error path.
			if s.TypeErrors != 0 {
				t.Errorf("TypeErrors = %d on clean real data: %v", s.TypeErrors, s.TypeErrorDetail)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Component shapes
// ---------------------------------------------------------------------------

// TestDep11Components is the field-level table: one case per shape
// index-formats.md says occurs, all of them real components.
func TestDep11Components(t *testing.T) {
	ubuntu := dep11DecodeString(t, dep11ReadFixture(t, dep11FixtureUbuntuMain), dep11TestRef("noble", "main"))
	universe := dep11DecodeString(t, dep11ReadFixture(t, dep11FixtureUbuntuUniverse), dep11TestRef("noble", "universe"))
	debian := dep11DecodeString(t, dep11ReadFixture(t, dep11FixtureDebianMain), dep11TestRef("bookworm", "main"))

	tests := []struct {
		name string
		idx  *dep11Index
		pkg  string
		want dep11App
	}{
		{
			// §7.2 verbatim. Note ID != Package, and Summary's C key is 19th
			// in the file: taking the first entry would yield Dutch.
			name: "desktop-application, C is not the first locale",
			idx:  ubuntu,
			pkg:  "htop",
			want: dep11App{
				Package:    "htop",
				ID:         "htop.desktop",
				Type:       "desktop-application",
				Name:       "Htop",
				Summary:    "Show System Processes",
				Categories: []string{"System", "Monitor", "ConsoleOnly"},
				IconRef:    "dep11-cached:noble/main/64x64/htop_htop.png",
				IsApp:      true,
			},
		},
		{
			// The name a human recognises, which is the entire reason this
			// file exists: "libreoffice-writer" -> "LibreOffice Writer".
			name: "the human application name",
			idx:  ubuntu,
			pkg:  "libreoffice-writer",
			want: dep11App{
				Package:    "libreoffice-writer",
				ID:         "libreoffice-writer.desktop",
				Type:       "desktop-application",
				Name:       "LibreOffice Writer",
				Summary:    "Word processor part of the LibreOffice productivity suite",
				Categories: []string{"Office", "WordProcessor"},
				IconRef:    "dep11-cached:noble/main/64x64/libreoffice-writer_libreoffice-writer.png",
				IsApp:      true,
			},
		},
		{
			// Five categories, in file order, plus all three Icon kinds
			// present at once (cached, remote, stock) — cached wins and remote
			// is never recorded, because its URL is relative to MediaBaseUrl
			// and rule 3 forbids fetching that host.
			name: "several categories, all three icon kinds",
			idx:  ubuntu,
			pkg:  "libreoffice-draw",
			want: dep11App{
				Package:    "libreoffice-draw",
				ID:         "libreoffice-draw.desktop",
				Type:       "desktop-application",
				Name:       "LibreOffice Draw",
				Summary:    "Graphics editor part of the LibreOffice productivity suite",
				Categories: []string{"Office", "FlowChart", "Graphics", "2DGraphics", "VectorGraphics"},
				IconRef:    "dep11-cached:noble/main/64x64/libreoffice-draw_libreoffice-draw.png",
				IsApp:      true,
			},
		},
		{
			// Categories missing (14.2% of components, §6.3) and no Icon
			// (8.9%). Both optional; neither may look like breakage.
			name: "no Categories, no Icon, non-application type",
			idx:  ubuntu,
			pkg:  "fwupd",
			want: dep11App{
				Package: "fwupd",
				ID:      "org.freedesktop.fwupd",
				Type:    "console-application",
				Name:    "fwupd",
				Summary: "Update device firmware on Linux",
				IsApp:   false,
			},
		},
		{
			// §7.2's second verbatim component: no Icon, two categories, and
			// a ContentRating that decodes to an empty nested map.
			name: "generic type with categories but no icon",
			idx:  ubuntu,
			pkg:  "gamemode",
			want: dep11App{
				Package:    "gamemode",
				ID:         "io.github.feralinteractive.gamemode",
				Type:       "generic",
				Name:       "gamemode",
				Summary:    "daemon that allows games to request a set of optimizations be temporarily applied",
				Categories: []string{"Utility", "Game"},
				IsApp:      false,
			},
		},
		{
			// A font: a real human name worth showing, but not an application.
			name: "font",
			idx:  ubuntu,
			pkg:  "fonts-smc-uroob",
			want: dep11App{
				Package: "fonts-smc-uroob",
				ID:      "in.org.smc.uroob",
				Type:    "font",
				Name:    "Uroob font",
				Summary: "Uroob Malayalam Unicode font",
				IconRef: "dep11-cached:noble/main/64x64/fonts-smc-uroob_uroob-regular.png",
				IsApp:   false,
			},
		},
		{
			name: "codec",
			idx:  ubuntu,
			pkg:  "gstreamer1.0-gtk3",
			want: dep11App{
				Package: "gstreamer1.0-gtk3",
				ID:      "gstreamer1.0-gtk3",
				Type:    "codec",
				Name:    "GStreamer Multimedia Codecs",
				Summary: "GStreamer plugin for GTK+3",
				IsApp:   false,
			},
		},
		{
			// The component behind the yaml.TypeError in the real archive:
			// 33 localised names, of which exactly one is kept.
			name: "heavily localised names keep only C",
			idx:  universe,
			pkg:  "gnome-klotski",
			want: dep11App{
				Package:    "gnome-klotski",
				ID:         "org.gnome.Klotski",
				Type:       "desktop-application",
				Name:       "GNOME Klotski",
				Summary:    "Slide blocks to solve the puzzle",
				Categories: []string{"Game", "LogicGame"},
				IconRef:    "dep11-cached:noble/universe/64x64/gnome-klotski_org.gnome.Klotski.png",
				IsApp:      true,
			},
		},
		{
			name: "debian desktop-application",
			idx:  debian,
			pkg:  "usbview",
			want: dep11App{
				Package:    "usbview",
				ID:         "com.kroah.usbview",
				Type:       "desktop-application",
				Name:       "USBView",
				Summary:    "A USB device tree viewer",
				Categories: []string{"System", "HardwareSettings", "Monitor"},
				IconRef:    "dep11-cached:bookworm/main/64x64/usbview_usbview.png",
				IsApp:      true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dep11MustLookup(t, tc.idx, tc.pkg)
			if got.Package != tc.want.Package || got.ID != tc.want.ID || got.Type != tc.want.Type {
				t.Errorf("identity = {%q %q %q}, want {%q %q %q}",
					got.Package, got.ID, got.Type, tc.want.Package, tc.want.ID, tc.want.Type)
			}
			if got.Name != tc.want.Name {
				t.Errorf("Name = %q, want %q", got.Name, tc.want.Name)
			}
			if got.Summary != tc.want.Summary {
				t.Errorf("Summary = %q, want %q", got.Summary, tc.want.Summary)
			}
			if !dep11EqualStrings(got.Categories, tc.want.Categories) {
				t.Errorf("Categories = %v, want %v", got.Categories, tc.want.Categories)
			}
			if got.IconRef != tc.want.IconRef {
				t.Errorf("IconRef = %q, want %q", got.IconRef, tc.want.IconRef)
			}
			if got.IsApp != tc.want.IsApp {
				t.Errorf("IsApp = %v, want %v", got.IsApp, tc.want.IsApp)
			}
		})
	}
}

// TestDep11AddonsAreSkipped covers §6.2: an addon describes a plugin, carries
// Extends: pointing at another component, and must not name its package.
//
// The real main fixture has two addon components whose Package: is "evince"
// and whose C names are "TIFF Documents" and "XPS Documents". Letting either
// win would put "TIFF Documents" in the picker where "Evince" belongs.
func TestDep11AddonsAreSkipped(t *testing.T) {
	idx := dep11DecodeString(t, dep11ReadFixture(t, dep11FixtureUbuntuMain), dep11TestRef("noble", "main"))

	if a, ok := idx.Lookup("evince"); ok {
		t.Errorf("addon component became the entry for evince: %+v", a)
	}
	if got, want := idx.Stats().Addons, 2; got != want {
		t.Errorf("stats.Addons = %d, want %d", got, want)
	}
	// Counted as components, so the coverage number stays honest.
	if got := idx.Stats().ByType["addon"]; got != 2 {
		t.Errorf("ByType[addon] = %d, want 2", got)
	}
}

// TestDep11LocaleSelection covers §7.5/§7.2 directly: only the English value
// is retained, it is found by key and never by position, and a map with no
// English key yields nothing rather than a random language.
func TestDep11LocaleSelection(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "C wins over everything",
			yaml: "de: Systemprozesse anzeigen\nfr: Affiche les processus\nC: Show System Processes\nru: Просмотр\n",
			want: "Show System Processes",
		},
		{
			name: "C found by key, not by position",
			yaml: "nl: Systeemprocessen\nes: Mostrar procesos\nsv: Visa\nC: Show System Processes\n",
			want: "Show System Processes",
		},
		{
			name: "en when there is no C",
			yaml: "de: Deutsch\nen: English\nfr: Francais\n",
			want: "English",
		},
		{
			name: "en beats en_US",
			yaml: "en_US: American\nen: English\n",
			want: "English",
		},
		{
			name: "en_US when there is no C or en",
			yaml: "de: Deutsch\nen_US: American\nen_GB: British\n",
			want: "American",
		},
		{
			name: "en_GB before an unlisted en variant",
			yaml: "en_AU: Australian\nen_GB: British\n",
			want: "British",
		},
		{
			name: "regional and modifier locales are discarded",
			yaml: "sr@ijekavianlatin: Prikaz\npt_BR: Mostra\nzh_TW: 顯示\nC: Show\n",
			want: "Show",
		},
		{
			name: "no English key retains nothing",
			yaml: "de: Deutsch\nfr: Francais\nru: Русский\n",
			want: "",
		},
		{
			// The archive never does this, but a duplicate key must be
			// deterministic rather than order-dependent.
			name: "a duplicated C keeps the first",
			yaml: "C: first\nde: Deutsch\nC: second\n",
			want: "first",
		},
		{
			name: "a bare scalar is accepted",
			yaml: "just a string\n",
			want: "just a string",
		},
		{
			name: "a sequence yields nothing rather than an error",
			yaml: "- one\n- two\n",
			want: "",
		},
		{
			name: "a nested map value is ignored, not crashed on",
			yaml: "C:\n  nested: value\nen: English\n",
			want: "English",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Wrap in a real component so the value goes through the same
			// path production code uses.
			doc := "---\nType: generic\nID: test\nPackage: test-pkg\nName:\n" +
				dep11Indent(tc.yaml, "  ") + "\n"
			idx := dep11DecodeString(t, doc, dep11TestRef("noble", "main"))
			got := dep11MustLookup(t, idx, "test-pkg")
			if got.Name != tc.want {
				t.Errorf("Name = %q, want %q", got.Name, tc.want)
			}
		})
	}
}

func dep11Indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// TestDep11LocalisationIsNotRetained asserts the memory property of §7.5
// rather than a value: the Klotski component carries 33 localised names and
// 30-odd localised summaries, and exactly one string of each survives the
// decode. dep11App has no map for the rest to hide in, and this test fails if
// one is ever added.
func TestDep11LocalisationIsNotRetained(t *testing.T) {
	src := dep11ReadFixture(t, dep11FixtureUbuntuUniverse)
	idx := dep11DecodeString(t, src, dep11TestRef("noble", "universe"))
	app := dep11MustLookup(t, idx, "gnome-klotski")

	// Strings that are in the fixture and must not survive into the retained
	// value: three localised names, and the MediaBaseUrl this application is
	// forbidden to fetch (contract-brief rule 3).
	foreign := []string{
		"Кльоцки GNOME",
		"Klotski de GNOME",
		"GNOME 華容道",
		"appstream.ubuntu.com",
	}
	blob := fmt.Sprintf("%+v", app)
	for _, s := range foreign {
		if !strings.Contains(src, s) {
			t.Fatalf("fixture no longer contains %q — the test needs updating", s)
		}
		if strings.Contains(blob, s) {
			t.Errorf("decoded component retained %q:\n%s", s, blob)
		}
	}
	if app.Name != "GNOME Klotski" {
		t.Errorf("Name = %q, want the C value %q", app.Name, "GNOME Klotski")
	}
}

// ---------------------------------------------------------------------------
// §7.3 — the one that would silently ruin this package
// ---------------------------------------------------------------------------

// TestDep11TypeErrorIsNotFatal is the highest-value test here.
//
// gopkg.in/yaml.v3 returns *yaml.TypeError on real DEP-11 data. It is an
// accumulating error, not a stream error: the decoder populated what it could
// and the stream is still positioned correctly. Treating it as fatal stops the
// real Ubuntu universe parse at component 449 of 2,587 and throws away 83% of
// the applications, silently — the catalogue simply comes up with almost no
// apps and nothing anywhere says why.
//
// Each subtest asserts the same two things: the error did not end the stream,
// and every component after the offending one is still present.
func TestDep11TypeErrorIsNotFatal(t *testing.T) {
	main := dep11ReadFixture(t, dep11FixtureUbuntuMain)
	universe := dep11ReadFixture(t, dep11FixtureUbuntuUniverse)

	// Components that appear after the trouble in every stream below, and so
	// are the ones a fatal-error regression would lose.
	laterPackages := []string{
		"info", "libgphoto2-6", "nvidia-settings", "libreoffice-draw",
		"brltty", "gstreamer1.0-plugins-good", "fonts-lohit-beng-bengali",
	}

	t.Run("real universe stream concatenated with real main", func(t *testing.T) {
		// §7.1: concatenating components from several files puts several
		// headers in one logical stream. This is also the exact fixture pair
		// §7.3 was measured on: gnome-klotski (whose Keywords map declares
		// ca_ES twice) followed by 32 more real components.
		idx := dep11DecodeString(t, universe+main, dep11TestRef("noble", "universe"))

		if got, want := idx.Stats().Headers, 2; got != want {
			t.Errorf("Headers = %d, want %d (a header must be recognised wherever it appears)", got, want)
		}
		if got, want := idx.Stats().Components, 33; got != want {
			t.Fatalf("Components = %d, want %d — the stream stopped early", got, want)
		}
		if _, ok := idx.Lookup("gnome-klotski"); !ok {
			t.Error("the component that trips the real-world TypeError was dropped")
		}
		for _, pkg := range laterPackages {
			if _, ok := idx.Lookup(pkg); !ok {
				t.Errorf("component after the trouble was lost: %q", pkg)
			}
		}
	})

	t.Run("duplicate key inside a decoded field keeps the component", func(t *testing.T) {
		// The exact shape of the real defect — a duplicated mapping key
		// inside one component — placed in a field this parser actually
		// descends into. yaml.v3 abandons that one mapping and populates
		// everything else, which is precisely what "soft" means.
		broken := strings.Replace(main,
			"Icon:\n  cached:\n  - name: htop_htop.png\n    width: 48\n    height: 48",
			"Icon:\n  cached:\n  - name: htop_htop.png\n    width: 48\n    height: 48\n"+
				"  cached:\n  - name: htop_htop.png\n    width: 64\n    height: 64", 1)
		if broken == main {
			t.Fatal("fixture changed: could not inject a duplicate Icon key")
		}

		idx := dep11DecodeString(t, broken, dep11TestRef("noble", "main"))
		s := idx.Stats()

		if s.TypeErrors != 1 {
			t.Fatalf("TypeErrors = %d, want 1 — the soft-error path was not exercised", s.TypeErrors)
		}
		if len(s.TypeErrorDetail) == 0 || !strings.Contains(s.TypeErrorDetail[0], "already defined") {
			t.Errorf("TypeErrorDetail = %v, want the yaml duplicate-key message", s.TypeErrorDetail)
		}
		if got, want := s.Components, 32; got != want {
			t.Fatalf("Components = %d, want %d — the stream stopped at the TypeError", got, want)
		}

		// The component survives; only the field that hit the duplicate is
		// abandoned.
		htop := dep11MustLookup(t, idx, "htop")
		if htop.Name != "Htop" || htop.Summary != "Show System Processes" {
			t.Errorf("component was not kept across the TypeError: %+v", htop)
		}
		if !dep11EqualStrings(htop.Categories, []string{"System", "Monitor", "ConsoleOnly"}) {
			t.Errorf("Categories = %v, want the real three", htop.Categories)
		}
		if htop.IconRef != "" {
			t.Errorf("IconRef = %q, want empty: the Icon mapping is the field yaml abandoned", htop.IconRef)
		}
		for _, pkg := range laterPackages {
			if _, ok := idx.Lookup(pkg); !ok {
				t.Errorf("component after the TypeError was lost: %q", pkg)
			}
		}
	})

	t.Run("duplicate key at the top level drops only that component", func(t *testing.T) {
		// When the duplicate is in the component's own top-level mapping,
		// yaml.v3 abandons the whole document. That must cost one component,
		// not the rest of the file.
		broken := strings.Replace(main, "Package: htop\n", "Package: htop\nPackage: htop\n", 1)
		if broken == main {
			t.Fatal("fixture changed: could not inject a duplicate top-level key")
		}

		idx := dep11DecodeString(t, broken, dep11TestRef("noble", "main"))
		s := idx.Stats()

		if s.TypeErrors != 1 {
			t.Fatalf("TypeErrors = %d, want 1", s.TypeErrors)
		}
		if s.Skipped != 1 {
			t.Errorf("Skipped = %d, want 1 (the abandoned component)", s.Skipped)
		}
		if _, ok := idx.Lookup("htop"); ok {
			t.Error("a component abandoned wholesale still produced an entry")
		}
		if got, want := s.Components, 31; got != want {
			t.Fatalf("Components = %d, want %d", got, want)
		}
		for _, pkg := range laterPackages {
			if _, ok := idx.Lookup(pkg); !ok {
				t.Errorf("component after the TypeError was lost: %q", pkg)
			}
		}
	})

	t.Run("wrong type for a field is soft too", func(t *testing.T) {
		// Categories as a scalar instead of a sequence: one field lost, the
		// component and the stream both survive.
		broken := strings.Replace(main,
			"Categories:\n- System\n- Monitor\n- ConsoleOnly",
			"Categories: System", 1)
		if broken == main {
			t.Fatal("fixture changed: could not rewrite htop's Categories")
		}

		idx := dep11DecodeString(t, broken, dep11TestRef("noble", "main"))
		if got := idx.Stats().TypeErrors; got != 1 {
			t.Fatalf("TypeErrors = %d, want 1", got)
		}
		htop := dep11MustLookup(t, idx, "htop")
		if htop.Name != "Htop" {
			t.Errorf("Name = %q, want Htop", htop.Name)
		}
		if len(htop.Categories) != 0 {
			t.Errorf("Categories = %v, want none", htop.Categories)
		}
		if got, want := idx.Stats().Components, 32; got != want {
			t.Errorf("Components = %d, want %d", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// Truncation and cancellation
// ---------------------------------------------------------------------------

// TestDep11TruncatedStream covers a download cut short. Two distinct shapes,
// because they behave differently and both are real.
func TestDep11TruncatedStream(t *testing.T) {
	main := dep11ReadFixture(t, dep11FixtureUbuntuMain)
	ref := dep11TestRef("noble", "main")

	t.Run("truncated yaml keeps what was already parsed", func(t *testing.T) {
		// Cutting a YAML stream at an arbitrary byte usually leaves a
		// syntactically valid prefix: the documents before the cut are simply
		// all there is. Nothing errors, and the catalogue is short a few
		// application names.
		idx := dep11NewIndex()
		if err := dep11Decode(context.Background(), strings.NewReader(main[:20000]), ref, idx); err != nil {
			t.Fatalf("unexpected error on a truncated stream: %v", err)
		}
		if idx.Len() == 0 {
			t.Fatal("no components survived the truncation")
		}
		if idx.Len() >= 30 {
			t.Fatalf("Len() = %d, expected fewer than the full 30 — the fixture may have changed", idx.Len())
		}
		if _, ok := idx.Lookup("libreoffice-writer"); !ok {
			t.Error("the first component was lost")
		}
	})

	t.Run("a malformed later document errors but keeps earlier components", func(t *testing.T) {
		idx := dep11NewIndex()
		stream := main + "---\nType: generic\nID: broken\n\tPackage: nope\n"
		err := dep11Decode(context.Background(), strings.NewReader(stream), ref, idx)
		if err == nil {
			t.Fatal("want an error for a syntactically broken document")
		}
		if !strings.Contains(err.Error(), ref.Path()) {
			t.Errorf("error does not name the index file it came from: %v", err)
		}
		// Everything decoded before the break is still in the index: a hard
		// error truncates, it does not discard.
		if got, want := idx.Len(), 30; got != want {
			t.Errorf("Len() = %d, want %d components kept from before the break", got, want)
		}
	})

	t.Run("truncated gzip surfaces as an error and is recorded, not fatal", func(t *testing.T) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := io.WriteString(zw, main); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		half := buf.Bytes()[:buf.Len()/2]

		srv := dep11TestServer(t, map[string][]byte{
			"/dists/noble/main/dep11/Components-amd64.yml.gz": half,
		})
		refs := []IndexRef{dep11ServerRef(srv, "noble", "main")}

		idx, err := dep11Load(context.Background(), refs, dep11Options{Client: srv.Client()})
		if err != nil {
			t.Fatalf("dep11Load returned a fatal error for one bad file: %v", err)
		}
		if got := len(idx.Stats().Failures); got != 1 {
			t.Fatalf("Failures = %d, want 1", got)
		}
		if !strings.Contains(idx.Stats().Failures[0].Err.Error(), "EOF") {
			t.Errorf("failure = %v, want an unexpected-EOF", idx.Stats().Failures[0])
		}
		if idx.Len() == 0 {
			t.Error("components decoded before the stream was cut were discarded")
		}
	})
}

// dep11SlowReader hands out data in small chunks and cancels ctx once it has
// delivered `after` bytes, so cancellation lands in the middle of a decode.
type dep11SlowReader struct {
	src    *strings.Reader
	after  int
	n      int
	cancel context.CancelFunc
}

func (r *dep11SlowReader) Read(p []byte) (int, error) {
	if len(p) > 4096 {
		p = p[:4096]
	}
	n, err := r.src.Read(p)
	r.n += n
	if r.n >= r.after && r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	return n, err
}

// TestDep11CancelMidDecode covers Build's cancellability: an operator who
// pressed Cancel gets a prompt stop, not a completed parse.
func TestDep11CancelMidDecode(t *testing.T) {
	main := dep11ReadFixture(t, dep11FixtureUbuntuMain)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := &dep11SlowReader{src: strings.NewReader(main), after: 8 << 10, cancel: cancel}
	idx := dep11NewIndex()
	err := dep11Decode(ctx, r, dep11TestRef("noble", "main"), idx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if idx.Stats().Documents == 0 {
		t.Error("cancellation happened before any document was read; the test is not exercising a mid-decode cancel")
	}
	if got, want := idx.Len(), 30; got >= want {
		t.Errorf("Len() = %d — the whole stream was parsed despite cancellation", got)
	}
}

// TestDep11CancelMidLoad covers cancellation across the fetch loop.
func TestDep11CancelMidLoad(t *testing.T) {
	main := dep11ReadFixture(t, dep11FixtureUbuntuMain)
	srv := dep11TestServer(t, map[string][]byte{
		"/dists/noble/main/dep11/Components-amd64.yml.gz":     dep11Gzip(t, main),
		"/dists/noble/universe/dep11/Components-amd64.yml.gz": dep11Gzip(t, main),
	})

	ctx, cancel := context.WithCancel(context.Background())
	refs := []IndexRef{
		dep11ServerRef(srv, "noble", "main"),
		dep11ServerRef(srv, "noble", "universe"),
	}

	var seen int
	_, err := dep11Load(ctx, refs, dep11Options{
		Client: srv.Client(),
		Progress: func(p Progress) {
			if p.Phase == PhaseParse {
				seen++
				cancel() // cancel after the first file is done
			}
		},
	})
	defer cancel()

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if seen == 0 {
		t.Error("no file was processed before cancellation")
	}
}

// ---------------------------------------------------------------------------
// Fetching: httptest only, never the network
// ---------------------------------------------------------------------------

func dep11Gzip(t testing.TB, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(zw, s); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// dep11TestServer serves the given path -> body map. A path mapped to nil is
// served as 404; a path mapped to a body starting with "STATUS:" is served
// with that status.
func dep11TestServer(t testing.TB, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok || body == nil {
			http.NotFound(w, r)
			return
		}
		if code, rest, found := strings.Cut(string(body), ":"); found && code == "STATUS" {
			status := 500
			_, _ = fmt.Sscanf(rest, "%d", &status)
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dep11ServerRef(srv *httptest.Server, suite, component string) IndexRef {
	return IndexRef{URI: srv.URL, Suite: suite, Component: component, Arch: "amd64", Kind: IndexDEP11}
}

// TestDep11Load covers the fetch path end to end against fixtures.
func TestDep11Load(t *testing.T) {
	main := dep11ReadFixture(t, dep11FixtureUbuntuMain)
	universe := dep11ReadFixture(t, dep11FixtureUbuntuUniverse)

	t.Run("two components, both gzipped", func(t *testing.T) {
		srv := dep11TestServer(t, map[string][]byte{
			"/dists/noble/main/dep11/Components-amd64.yml.gz":     dep11Gzip(t, main),
			"/dists/noble/universe/dep11/Components-amd64.yml.gz": dep11Gzip(t, universe),
		})
		refs := []IndexRef{
			dep11ServerRef(srv, "noble", "main"),
			dep11ServerRef(srv, "noble", "universe"),
		}

		idx, err := dep11Load(context.Background(), refs, dep11Options{Client: srv.Client()})
		if err != nil {
			t.Fatalf("dep11Load: %v", err)
		}
		s := idx.Stats()
		if s.Files != 2 {
			t.Errorf("Files = %d, want 2", s.Files)
		}
		if got, want := idx.Len(), 31; got != want {
			t.Errorf("Len() = %d, want %d", got, want)
		}
		if s.CompressedBytes <= 0 || s.DecodedBytes <= s.CompressedBytes {
			t.Errorf("byte counters look wrong: compressed=%d decoded=%d", s.CompressedBytes, s.DecodedBytes)
		}
		// The icon reference must carry the component the ref actually came
		// from, not the one that happened to be fetched first.
		k := dep11MustLookup(t, idx, "gnome-klotski")
		if !strings.Contains(k.IconRef, "noble/universe/") {
			t.Errorf("IconRef = %q, want it to name noble/universe", k.IconRef)
		}
	})

	t.Run("an uncompressed body is accepted", func(t *testing.T) {
		// Some mirrors label a .gz file Content-Encoding: gzip, and Go's
		// transport then hands us plain YAML. Sniffing the magic bytes rather
		// than assuming keeps that from being a hard failure.
		srv := dep11TestServer(t, map[string][]byte{
			"/dists/noble/main/dep11/Components-amd64.yml.gz": []byte(main),
		})
		idx, err := dep11Load(context.Background(),
			[]IndexRef{dep11ServerRef(srv, "noble", "main")},
			dep11Options{Client: srv.Client()})
		if err != nil {
			t.Fatalf("dep11Load: %v", err)
		}
		if got, want := idx.Len(), 30; got != want {
			t.Errorf("Len() = %d, want %d", got, want)
		}
	})

	t.Run("404 is not an error", func(t *testing.T) {
		// A component may simply not publish DEP-11. That must cost nicer
		// labels on some rows and nothing else.
		srv := dep11TestServer(t, map[string][]byte{
			"/dists/noble/main/dep11/Components-amd64.yml.gz": dep11Gzip(t, main),
		})
		refs := []IndexRef{
			dep11ServerRef(srv, "noble", "main"),
			dep11ServerRef(srv, "noble", "restricted"), // not served
		}

		idx, err := dep11Load(context.Background(), refs, dep11Options{Client: srv.Client()})
		if err != nil {
			t.Fatalf("a missing DEP-11 file must not fail the load: %v", err)
		}
		s := idx.Stats()
		if s.NotPublished != 1 {
			t.Errorf("NotPublished = %d, want 1", s.NotPublished)
		}
		if len(s.Failures) != 0 {
			t.Errorf("Failures = %v, want none: a 404 is expected, not a failure", s.Failures)
		}
		if s.Files != 1 {
			t.Errorf("Files = %d, want 1", s.Files)
		}
		if idx.Len() == 0 {
			t.Error("the component that did publish DEP-11 was lost")
		}
	})

	t.Run("a server error is recorded and the rest still loads", func(t *testing.T) {
		srv := dep11TestServer(t, map[string][]byte{
			"/dists/noble/main/dep11/Components-amd64.yml.gz":     []byte("STATUS:503"),
			"/dists/noble/universe/dep11/Components-amd64.yml.gz": dep11Gzip(t, universe),
		})
		refs := []IndexRef{
			dep11ServerRef(srv, "noble", "main"),
			dep11ServerRef(srv, "noble", "universe"),
		}

		idx, err := dep11Load(context.Background(), refs, dep11Options{Client: srv.Client()})
		if err != nil {
			t.Fatalf("dep11Load: %v", err)
		}
		s := idx.Stats()
		if len(s.Failures) != 1 {
			t.Fatalf("Failures = %v, want exactly one", s.Failures)
		}
		if !strings.Contains(s.Failures[0].String(), "503") {
			t.Errorf("failure = %s, want it to name the status", s.Failures[0])
		}
		if _, ok := idx.Lookup("gnome-klotski"); !ok {
			t.Error("the healthy component was not loaded")
		}
	})

	t.Run("non-DEP-11 refs are ignored", func(t *testing.T) {
		var hits int
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			http.NotFound(w, r)
		}))
		defer srv.Close()

		refs := []IndexRef{
			{URI: srv.URL, Suite: "noble", Component: "main", Arch: "amd64", Kind: IndexPackages},
			{URI: srv.URL, Suite: "noble", Component: "universe", Arch: "amd64", Kind: IndexPackages},
		}
		idx, err := dep11Load(context.Background(), refs, dep11Options{Client: srv.Client()})
		if err != nil {
			t.Fatalf("dep11Load: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if hits != 0 {
			t.Errorf("fetched %d Packages refs; dep11Load must only touch IndexDEP11 refs", hits)
		}
		if idx.Len() != 0 {
			t.Errorf("Len() = %d, want 0", idx.Len())
		}
	})

	t.Run("target IndexRefs feed straight in", func(t *testing.T) {
		// The whole point of the frozen IndexRef type: paths are derived once,
		// in iface.go, and both index packages read the same tuple.
		srv := dep11TestServer(t, map[string][]byte{
			"/dists/noble/main/dep11/Components-amd64.yml.gz": dep11Gzip(t, main),
		})
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		tgt := Target{
			Arch: "amd64",
			Sources: []Source{{
				Types:      []string{"deb"},
				URIs:       []string{u.String()},
				Suites:     []string{"noble"},
				Components: []string{"main"},
			}},
		}
		idx, err := dep11Load(context.Background(), tgt.IndexRefs(), dep11Options{Client: srv.Client()})
		if err != nil {
			t.Fatalf("dep11Load: %v", err)
		}
		if got, want := idx.Len(), 30; got != want {
			t.Errorf("Len() = %d, want %d", got, want)
		}
	})

	t.Run("decompression cap is enforced", func(t *testing.T) {
		srv := dep11TestServer(t, map[string][]byte{
			"/dists/noble/main/dep11/Components-amd64.yml.gz": dep11Gzip(t, main),
		})
		idx, err := dep11Load(context.Background(),
			[]IndexRef{dep11ServerRef(srv, "noble", "main")},
			dep11Options{Client: srv.Client(), MaxDecompressedBytes: 4096})
		if err != nil {
			t.Fatalf("dep11Load: %v", err)
		}
		if len(idx.Stats().Failures) != 1 {
			t.Fatalf("Failures = %v, want one oversize failure", idx.Stats().Failures)
		}
		if !strings.Contains(idx.Stats().Failures[0].Err.Error(), "exceeds") {
			t.Errorf("failure = %v, want the size-cap message", idx.Stats().Failures[0])
		}
	})
}

// TestDep11Progress checks the reports a progress view depends on: never an
// empty label, the phase placed in the whole job, and both the download and
// parse phases represented.
func TestDep11Progress(t *testing.T) {
	main := dep11ReadFixture(t, dep11FixtureUbuntuMain)
	srv := dep11TestServer(t, map[string][]byte{
		"/dists/noble/main/dep11/Components-amd64.yml.gz": dep11Gzip(t, main),
	})

	var reports []Progress
	_, err := dep11Load(context.Background(),
		[]IndexRef{dep11ServerRef(srv, "noble", "main")},
		dep11Options{
			Client:   srv.Client(),
			Progress: func(p Progress) { reports = append(reports, p) },
		})
	if err != nil {
		t.Fatalf("dep11Load: %v", err)
	}
	if len(reports) < 2 {
		t.Fatalf("got %d progress reports, want at least a download and a parse", len(reports))
	}

	seen := map[Phase]bool{}
	for _, p := range reports {
		seen[p.Phase] = true
		if p.Label == "" {
			t.Errorf("progress report with an empty Label: %+v", p)
		}
		if p.PhaseCount != len(Phases) {
			t.Errorf("PhaseCount = %d, want %d", p.PhaseCount, len(Phases))
		}
		if p.PhaseIndex != p.Phase.Index() {
			t.Errorf("PhaseIndex = %d, want %d for phase %q", p.PhaseIndex, p.Phase.Index(), p.Phase)
		}
		if f := p.OverallFraction(); f < 0 || f > 1 {
			t.Errorf("OverallFraction() = %v, out of range", f)
		}
		if p.Item != "dists/noble/main/dep11/Components-amd64.yml.gz" {
			t.Errorf("Item = %q, want the index path", p.Item)
		}
	}
	for _, want := range []Phase{PhaseDownload, PhaseParse} {
		if !seen[want] {
			t.Errorf("no progress report for phase %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Joining to the Packages side
// ---------------------------------------------------------------------------

// TestDep11Apply covers the merge catalog.go performs. Applying is driven
// from the entry side, so a component naming a package that does not exist
// cannot invent a row.
func TestDep11Apply(t *testing.T) {
	idx := dep11DecodeString(t, dep11ReadFixture(t, dep11FixtureUbuntuMain), dep11TestRef("noble", "main"))

	t.Run("a known application is enriched", func(t *testing.T) {
		e := Entry{
			Name:      "libreoffice-writer",
			Version:   "4:24.2.7-0ubuntu0.24.04.4",
			Section:   "editors",
			Summary:   "office productivity suite -- word processor",
			Arch:      "amd64",
			Component: "main",
		}
		if !idx.Apply(&e) {
			t.Fatal("Apply reported no DEP-11 data for libreoffice-writer")
		}
		if e.AppName != "LibreOffice Writer" {
			t.Errorf("AppName = %q, want %q", e.AppName, "LibreOffice Writer")
		}
		if e.Summary != "Word processor part of the LibreOffice productivity suite" {
			t.Errorf("Summary = %q, want DEP-11's summary to win", e.Summary)
		}
		if !dep11EqualStrings(e.Categories, []string{"Office", "WordProcessor"}) {
			t.Errorf("Categories = %v", e.Categories)
		}
		if !e.IsApp {
			t.Error("IsApp = false, want true for a desktop-application")
		}
		if e.IconRef == "" {
			t.Error("IconRef was not recorded")
		}
		// Nothing from the Packages side may be disturbed.
		if e.Version != "4:24.2.7-0ubuntu0.24.04.4" || e.Section != "editors" || e.Component != "main" {
			t.Errorf("Apply modified Packages-derived fields: %+v", e)
		}
	})

	t.Run("a package DEP-11 does not know is untouched", func(t *testing.T) {
		e := Entry{Name: "libc6", Summary: "GNU C Library"}
		if idx.Apply(&e) {
			t.Fatal("Apply claimed DEP-11 data for libc6")
		}
		if e.AppName != "" || e.IsApp || len(e.Categories) != 0 || e.Summary != "GNU C Library" {
			t.Errorf("entry was modified: %+v", e)
		}
	})

	t.Run("a non-application gets a name but not IsApp", func(t *testing.T) {
		e := Entry{Name: "fonts-smc-uroob", Summary: "Uroob font for Malayalam"}
		if !idx.Apply(&e) {
			t.Fatal("Apply reported no data for a font component")
		}
		if e.AppName != "Uroob font" {
			t.Errorf("AppName = %q", e.AppName)
		}
		if e.IsApp {
			t.Error("IsApp = true for a font; AppsOnly would fill the picker with fonts")
		}
		if len(e.Categories) != 0 {
			t.Errorf("Categories = %v, want none", e.Categories)
		}
	})

	t.Run("a component with no summary leaves the Packages one alone", func(t *testing.T) {
		var e Entry
		e.Name = "x"
		a := dep11App{Package: "x", Name: "X"}
		e.Summary = "from the Packages index"
		a.Apply(&e)
		if e.Summary != "from the Packages index" {
			t.Errorf("Summary = %q, want the Packages value kept", e.Summary)
		}
	})

	t.Run("categories are copied, not aliased", func(t *testing.T) {
		e := Entry{Name: "htop"}
		if !idx.Apply(&e) {
			t.Fatal("no data for htop")
		}
		e.Categories[0] = "MUTATED"
		other := Entry{Name: "htop"}
		if !idx.Apply(&other) {
			t.Fatal("no data for htop")
		}
		if other.Categories[0] != "System" {
			t.Errorf("mutating one entry's Categories changed the index: %v", other.Categories)
		}
	})
}

// TestDep11Reconcile covers §6.1's 57 dangling references: DEP-11 components
// naming packages that do not exist in the Packages index, because noble's
// DEP-11 was generated five months before noble released. Real staleness, not
// a parser bug, and it must cost a count and not any good data.
func TestDep11Reconcile(t *testing.T) {
	idx := dep11DecodeString(t, dep11ReadFixture(t, dep11FixtureUbuntuMain), dep11TestRef("noble", "main"))
	before := idx.Len()

	// Stand in for packages.go's package set: everything except two packages,
	// as if they had been removed from the archive since the metadata was
	// built.
	removed := map[string]bool{"nvidia-settings": true, "brltty": true}
	dropped := idx.Reconcile(func(pkg string) bool { return !removed[pkg] })

	if dropped != 2 {
		t.Fatalf("Reconcile dropped %d, want 2", dropped)
	}
	if got, want := idx.Len(), before-2; got != want {
		t.Errorf("Len() = %d, want %d", got, want)
	}
	s := idx.Stats()
	if s.Dangling != 2 {
		t.Errorf("Dangling = %d, want 2", s.Dangling)
	}
	if !dep11EqualStrings(s.DanglingPackages, []string{"brltty", "nvidia-settings"}) {
		t.Errorf("DanglingPackages = %v", s.DanglingPackages)
	}
	// Apps is corrected too: nvidia-settings was a desktop-application.
	if s.Apps != 8 {
		t.Errorf("Apps = %d, want 8 after dropping one application", s.Apps)
	}
	if _, ok := idx.Lookup("nvidia-settings"); ok {
		t.Error("a dangling component survived Reconcile")
	}
	if _, ok := idx.Lookup("htop"); !ok {
		t.Error("Reconcile dropped a component whose package exists")
	}

	t.Run("a nil predicate is a no-op", func(t *testing.T) {
		n := idx.Len()
		if got := idx.Reconcile(nil); got != 0 {
			t.Errorf("Reconcile(nil) = %d, want 0", got)
		}
		if idx.Len() != n {
			t.Error("Reconcile(nil) changed the index")
		}
	})

	t.Run("Apply alone never invents a row", func(t *testing.T) {
		// Even without Reconcile, a dangling component cannot produce an
		// entry, because the merge iterates entries and not components.
		fresh := dep11DecodeString(t, dep11ReadFixture(t, dep11FixtureUbuntuMain), dep11TestRef("noble", "main"))
		entries := []Entry{{Name: "htop"}} // the only package that "exists"
		var enriched int
		for i := range entries {
			if fresh.Apply(&entries[i]) {
				enriched++
			}
		}
		if enriched != 1 || len(entries) != 1 {
			t.Errorf("enriched=%d entries=%d, want 1 and 1", enriched, len(entries))
		}
	})
}

// TestDep11Precedence covers several components naming one package. The rule
// is by component type and then file order — never by anything version-like,
// which would be engine work.
func TestDep11Precedence(t *testing.T) {
	tests := []struct {
		name     string
		stream   string
		wantID   string
		wantApp  bool
		wantDups int
	}{
		{
			name: "desktop-application beats a font seen first",
			stream: dep11Component("font", "a.font", "shared", "A Font") +
				dep11Component("desktop-application", "b.desktop", "shared", "B App"),
			wantID: "b.desktop", wantApp: true, wantDups: 1,
		},
		{
			name: "a font seen second does not displace an application",
			stream: dep11Component("desktop-application", "b.desktop", "shared", "B App") +
				dep11Component("font", "a.font", "shared", "A Font"),
			wantID: "b.desktop", wantApp: true, wantDups: 1,
		},
		{
			name: "equal rank keeps the first, in file order",
			stream: dep11Component("desktop-application", "first.desktop", "shared", "First") +
				dep11Component("desktop-application", "second.desktop", "shared", "Second"),
			wantID: "first.desktop", wantApp: true, wantDups: 1,
		},
		{
			name: "console-application beats generic",
			stream: dep11Component("generic", "g", "shared", "Generic") +
				dep11Component("console-application", "c", "shared", "Console"),
			wantID: "c", wantApp: false, wantDups: 1,
		},
		{
			name: "an unknown future type is kept at the base rank",
			stream: dep11Component("operating-system", "os", "shared", "Some OS") +
				dep11Component("desktop-application", "app.desktop", "shared", "App"),
			wantID: "app.desktop", wantApp: true, wantDups: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx := dep11DecodeString(t, tc.stream, dep11TestRef("noble", "main"))
			got := dep11MustLookup(t, idx, "shared")
			if got.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tc.wantID)
			}
			if got.IsApp != tc.wantApp {
				t.Errorf("IsApp = %v, want %v", got.IsApp, tc.wantApp)
			}
			if idx.Len() != 1 {
				t.Errorf("Len() = %d, want 1: two components for one package is one row", idx.Len())
			}
			if got := idx.Stats().Superseded; got != tc.wantDups {
				t.Errorf("Superseded = %d, want %d", got, tc.wantDups)
			}
			if got, want := idx.Stats().Merged, 1; got != want {
				t.Errorf("Merged = %d, want %d", got, want)
			}
		})
	}
}

func dep11Component(typ, id, pkg, name string) string {
	return fmt.Sprintf("---\nType: %s\nID: %s\nPackage: %s\nName:\n  C: %s\nSummary:\n  C: summary of %s\n",
		typ, id, pkg, name, name)
}

// TestDep11IconRef covers the reference this package records and does not fetch.
func TestDep11IconRef(t *testing.T) {
	ref := dep11TestRef("noble", "universe")

	tests := []struct {
		name string
		icon dep11Icon
		want string
	}{
		{name: "nothing at all", icon: dep11Icon{}, want: ""},
		{
			name: "a single cached icon",
			icon: dep11Icon{Cached: []dep11IconFile{{Name: "htop_htop.png", Width: 48, Height: 48}}},
			want: "dep11-cached:noble/universe/48x48/htop_htop.png",
		},
		{
			name: "the 64px variant is preferred",
			icon: dep11Icon{Cached: []dep11IconFile{
				{Name: "a.png", Width: 48, Height: 48},
				{Name: "a.png", Width: 64, Height: 64},
				{Name: "a.png", Width: 128, Height: 128},
			}},
			want: "dep11-cached:noble/universe/64x64/a.png",
		},
		{
			name: "larger wins over smaller at equal distance",
			icon: dep11Icon{Cached: []dep11IconFile{
				{Name: "a.png", Width: 32, Height: 32},
				{Name: "a.png", Width: 96, Height: 96},
			}},
			want: "dep11-cached:noble/universe/96x96/a.png",
		},
		{
			name: "cached beats stock and remote",
			icon: dep11Icon{
				Cached: []dep11IconFile{{Name: "a.png", Width: 64, Height: 64}},
				Remote: []dep11IconFile{{URL: "l/li/x/icons/128x128/a.png", Width: 128, Height: 128}},
				Stock:  "some-theme-icon",
			},
			want: "dep11-cached:noble/universe/64x64/a.png",
		},
		{
			name: "stock is the fallback",
			icon: dep11Icon{Stock: "libreoffice-draw"},
			want: "dep11-stock:libreoffice-draw",
		},
		{
			// remote: is relative to MediaBaseUrl, which rule 3 forbids
			// fetching. It must never become a reference.
			name: "remote alone yields nothing",
			icon: dep11Icon{Remote: []dep11IconFile{{URL: "l/li/x/icons/128x128/a.png", Width: 128}}},
			want: "",
		},
		{
			name: "a nameless cached entry is ignored",
			icon: dep11Icon{Cached: []dep11IconFile{{Width: 64, Height: 64}}, Stock: "fallback"},
			want: "dep11-stock:fallback",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dep11IconRef(ref, tc.icon); got != tc.want {
				t.Errorf("dep11IconRef = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("no icon reference is ever a fetchable URL", func(t *testing.T) {
		idx := dep11DecodeString(t, dep11ReadFixture(t, dep11FixtureUbuntuMain), dep11TestRef("noble", "main"))
		for _, a := range idx.Apps() {
			if strings.Contains(a.IconRef, "://") || strings.Contains(a.IconRef, "appstream.") {
				t.Errorf("%s: IconRef = %q looks like a URL", a.Package, a.IconRef)
			}
		}
	})
}

// TestDep11CleanCategories covers the tidy-up applied to every category list.
func TestDep11CleanCategories(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil stays nil", in: nil, want: nil},
		{name: "order is preserved", in: []string{"Office", "Graphics"}, want: []string{"Office", "Graphics"}},
		{name: "duplicates collapse", in: []string{"Game", "Game", "LogicGame"}, want: []string{"Game", "LogicGame"}},
		{name: "blanks are dropped", in: []string{"", "  ", "Utility"}, want: []string{"Utility"}},
		{name: "whitespace is trimmed", in: []string{" Utility "}, want: []string{"Utility"}},
		{name: "all blank yields nil", in: []string{"", " "}, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dep11CleanCategories(tc.in); !dep11EqualStrings(got, tc.want) {
				t.Errorf("dep11CleanCategories(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestDep11NilSafety: the index is handed between packages, so the zero and nil
// cases must not panic.
func TestDep11NilSafety(t *testing.T) {
	var idx *dep11Index
	if idx.Len() != 0 || idx.Packages() != nil || idx.Apps() != nil || idx.Headers() != nil {
		t.Error("nil index accessors misbehaved")
	}
	if _, ok := idx.Lookup("x"); ok {
		t.Error("nil index returned a lookup hit")
	}
	if idx.Apply(&Entry{Name: "x"}) {
		t.Error("nil index applied something")
	}
	if idx.Add(dep11App{Package: "x"}) {
		t.Error("nil index accepted an Add")
	}
	if idx.Reconcile(func(string) bool { return false }) != 0 {
		t.Error("nil index reconciled something")
	}
	_ = idx.Stats()

	real := dep11NewIndex()
	if real.Apply(nil) {
		t.Error("Apply(nil) reported success")
	}
	if real.Add(dep11App{}) {
		t.Error("Add accepted a component with no Package")
	}
	if err := dep11Decode(context.Background(), strings.NewReader("---\n"), dep11TestRef("noble", "main"), nil); err == nil {
		t.Error("dep11Decode(nil index) did not error")
	}
	dep11App{}.Apply(nil)
}

// TestDep11EmptyAndOddDocuments: streams that are not what the archive ships.
func TestDep11EmptyAndOddDocuments(t *testing.T) {
	ref := dep11TestRef("noble", "main")
	tests := []struct {
		name       string
		stream     string
		wantLen    int
		wantSkip   int
		wantHeader int
	}{
		{name: "empty input", stream: "", wantLen: 0},
		{name: "just a document marker", stream: "---\n", wantLen: 0, wantSkip: 1},
		{name: "header only", stream: "---\nFile: DEP-11\nVersion: '0.14'\n", wantLen: 0, wantHeader: 1},
		{
			name:     "component with no Package",
			stream:   "---\nType: desktop-application\nID: orphan.desktop\nName:\n  C: Orphan\n",
			wantLen:  0,
			wantSkip: 1,
		},
		{
			name:    "component with only the guaranteed keys",
			stream:  "---\nType: desktop-application\nID: bare.desktop\nPackage: bare\nName:\n  C: Bare\nSummary:\n  C: bare summary\n",
			wantLen: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx := dep11DecodeString(t, tc.stream, ref)
			if idx.Len() != tc.wantLen {
				t.Errorf("Len() = %d, want %d", idx.Len(), tc.wantLen)
			}
			if idx.Stats().Skipped != tc.wantSkip {
				t.Errorf("Skipped = %d, want %d", idx.Stats().Skipped, tc.wantSkip)
			}
			if idx.Stats().Headers != tc.wantHeader {
				t.Errorf("Headers = %d, want %d", idx.Stats().Headers, tc.wantHeader)
			}
		})
	}
}

// TestDep11StatsString keeps the log line honest, since it is what a
// maintainer reads when the applications tier looks wrong.
func TestDep11StatsString(t *testing.T) {
	idx := dep11DecodeString(t, dep11ReadFixture(t, dep11FixtureUbuntuMain), dep11TestRef("noble", "main"))
	got := idx.Stats().String()
	for _, want := range []string{"32 components", "30 merged", "9 apps", "2 addons skipped"} {
		if !strings.Contains(got, want) {
			t.Errorf("stats string %q missing %q", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Benchmark
// ---------------------------------------------------------------------------

// dep11BenchCorpus builds a stream of at least the given size out of the real
// fixtures, so the benchmark measures real component shapes — the big locale
// maps, the folded HTML descriptions, the nested Icon maps — rather than a
// synthetic best case. The three fixtures are ~160 KB together; the real
// Ubuntu main+universe DEP-11 is 19.5 MB uncompressed (index-formats.md §7.4).
//
// Repeating them is honest for a throughput measurement and slightly
// pessimistic for a per-document one: the fixtures were cut to be shape-dense,
// so this corpus averages ~3.2 KB per document against the real archive's
// ~7.5 KB, which means proportionally more per-document decoder overhead.
func dep11BenchCorpus(tb testing.TB, target int) string {
	tb.Helper()
	unit := dep11ReadFixture(tb, dep11FixtureUbuntuUniverse) +
		dep11ReadFixture(tb, dep11FixtureUbuntuMain) +
		dep11ReadFixture(tb, dep11FixtureDebianMain)
	if target <= len(unit) {
		return unit
	}
	var sb strings.Builder
	sb.Grow(target + len(unit))
	for sb.Len() < target {
		sb.WriteString(unit)
	}
	return sb.String()
}

// BenchmarkDep11Decode measures the parse phase §7.4 budgets at 10 s for the
// whole catalogue. Run with -benchtime to taste; the number that matters is
// MB/s, since the real corpus is 19.5 MB.
func BenchmarkDep11Decode(b *testing.B) {
	for _, size := range []struct {
		name  string
		bytes int
	}{
		{"fixtures", 0},
		{"1MB", 1 << 20},
		{"20MB_real_corpus_size", 20 << 20},
	} {
		b.Run(size.name, func(b *testing.B) {
			corpus := dep11BenchCorpus(b, size.bytes)
			ref := dep11TestRef("noble", "universe")
			b.SetBytes(int64(len(corpus)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				idx := dep11NewIndex()
				if err := dep11Decode(context.Background(), strings.NewReader(corpus), ref, idx); err != nil {
					b.Fatal(err)
				}
				if idx.Len() == 0 {
					b.Fatal("decoded nothing")
				}
			}
		})
	}
}

// BenchmarkDep11Load measures fetch plus decode over httptest, which is what
// the catalogue build actually does minus the wire.
func BenchmarkDep11Load(b *testing.B) {
	corpus := dep11BenchCorpus(b, 4<<20)
	srv := dep11TestServer(b, map[string][]byte{
		"/dists/noble/main/dep11/Components-amd64.yml.gz": dep11Gzip(b, corpus),
	})
	refs := []IndexRef{dep11ServerRef(srv, "noble", "main")}
	opts := dep11Options{Client: srv.Client()}

	b.SetBytes(int64(len(corpus)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := dep11Load(context.Background(), refs, opts); err != nil {
			b.Fatal(err)
		}
	}
}

// TestDep11DecodeWorkIsBounded replaces TestDep11ParseBudget, which asserted
// that a 20 MB corpus decoded in under 4 seconds.
//
// # Why the wall-clock assertion had to go
//
// It failed twice, on two different runs, at 9.4 s and 12.0 s, on a machine
// that was building a frontend and running a container at the same time. Both
// failures were real measurements and neither was a regression: the decode
// itself is 0.41 s. A budget asserted in a unit test measures the runner, not
// the code, and a test that goes red because someone else's build was running
// teaches the next person to re-run it rather than to read it — which costs
// far more than the regression it was watching for.
//
// contract-brief.md already says this outright: "Budgets are measured and
// recorded, never asserted." The 10 s parse budget lives in that document, the
// number that proves it lives in BenchmarkDep11Decode and docs/performance.md,
// and this test's job is to catch the kind of regression a benchmark nobody
// ran would miss.
//
// # What it asserts instead
//
// Work per unit of input, which is a property of the code rather than of the
// machine. Measured on this tree, three consecutive runs:
//
//	ordinary build: 16.57x corpus bytes allocated, 1014.53 allocations/document
//	-race build:    17.11x corpus bytes allocated, 1014.60 allocations/document
//
// The byte ratio varied by 0.005% between runs of the same build and by 3%
// between the two builds; the allocation count varied by 0.01% across all six.
// Wall time over the same six runs varied by 14x. That is the whole argument:
// one of these numbers is about the decoder and the other is about what else
// the computer was doing.
//
// It also means the -race carve-out is gone. catalogRaceEnabled existed only
// because the detector's ~14x slowdown broke a timing assertion; it does not
// perturb an allocation count, so the check is now equally sharp in both
// builds instead of being skipped in one of them.
//
// The ceilings are roughly 3x the measured values: loose enough that an
// honest refactor does not trip them, tight enough that the regressions worth
// catching here — a decoder that retains every document, an accidental
// quadratic, a per-field allocation where there was a shared buffer — move the
// number by an order of magnitude, not by a third.
func TestDep11DecodeWorkIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("20 MB corpus; -short")
	}
	const (
		maxAllocBytesPerCorpusByte = 48   // measured 16.6 (17.1 under -race)
		maxAllocsPerDocument       = 3000 // measured 1014.5, both builds
	)

	corpus := dep11BenchCorpus(t, 20<<20)
	idx := dep11NewIndex()

	// GC first so the delta is this decode's work and not the corpus builder's
	// garbage. TotalAlloc and Mallocs are monotonic counters, so the
	// difference is exact regardless of when a collection happens after.
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	if err := dep11Decode(context.Background(), strings.NewReader(corpus), dep11TestRef("noble", "universe"), idx); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	docs := idx.Stats().Documents
	if docs == 0 {
		t.Fatal("decoded no documents, so every ratio below would be measuring nothing")
	}
	allocBytes := after.TotalAlloc - before.TotalAlloc
	allocs := after.Mallocs - before.Mallocs
	perByte := float64(allocBytes) / float64(len(corpus))
	perDoc := float64(allocs) / float64(docs)

	// Recorded, not asserted. The throughput number is the useful one for
	// docs/performance.md and for anyone reading a CI log, and printing it
	// costs nothing; only believing it on a loaded machine does.
	t.Logf("decoded %.1f MB / %d documents in %s (%.0f MB/s); allocated %d bytes (%.2fx corpus) in %d allocations (%.1f/document)",
		float64(len(corpus))/(1<<20), docs, elapsed.Round(time.Millisecond),
		float64(len(corpus))/(1<<20)/elapsed.Seconds(),
		allocBytes, perByte, allocs, perDoc)

	if perByte > maxAllocBytesPerCorpusByte {
		t.Errorf("decode allocated %.2f bytes per corpus byte, over the %d ceiling; something now retains or copies per-field where it did not",
			perByte, maxAllocBytesPerCorpusByte)
	}
	if perDoc > maxAllocsPerDocument {
		t.Errorf("decode made %.1f allocations per document, over the %d ceiling",
			perDoc, maxAllocsPerDocument)
	}
}
