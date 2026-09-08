package audit

// Rule 4, mechanically.
//
//	"No telemetry, no analytics, no crash reporting. Architecturally absent,
//	 not merely disabled. No such library may appear in the tree."
//	                              — docs/dev/contract-brief.md, rule 4
//
// The honest set is what the binary actually links, not what go.mod lists. The
// module graph is much larger than the link closure because the Wails CLI pulls
// in tooling — a template engine, a file-watcher, an ANSI parser for its own
// terminal output — that never reaches this application. Auditing go.mod would
// therefore report packages nobody ships, and, worse, would give the reader a
// number they cannot reconcile with anything.
//
// So this asks the toolchain: `go list -deps .` is the transitive set of
// packages the main package needs, and nothing else is in the binary.

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// secTelemetryModule is the substring of an import path that identifies a
// telemetry, analytics, crash-reporting or session-replay SDK.
//
// Written as substrings rather than exact module paths on purpose: a vendor
// renames its Go module far more often than it renames itself, and an exact
// list would go quietly out of date while continuing to pass.
var secTelemetryModule = []string{
	"getsentry", "sentry-go", "bugsnag", "rollbar", "raygun", "honeybadger",
	"airbrake", "datadog", "dd-trace", "newrelic", "elastic/apm", "honeycomb",
	"opentelemetry", "go.opentelemetry", "opencensus", "prometheus/client_golang",
	"statsd", "segmentio/analytics", "amplitude", "mixpanel", "posthog",
	"heap-analytics", "google-analytics", "googleanalytics", "firebase",
	"appcenter", "applicationinsights", "appinsights", "instana", "dynatrace",
	"logrocket", "fullstory", "smartlook", "countly", "matomo", "plausible",
	"crashlytics", "breakpad", "crashpad", "sentry.io",
}

// secLinkedPackages asks the toolchain for the transitive package set of the
// main package.
func secLinkedPackages(t *testing.T) []string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		// Not skipped. A run that cannot ask the question must say so loudly:
		// rule 4 is a definition-of-done item and a silently skipped check is
		// how one stops being enforced.
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list -deps . failed: %v\n%s", err, stderr)
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs
}

func TestNoTelemetryLibraryIsLinked(t *testing.T) {
	pkgs := secLinkedPackages(t)
	if len(pkgs) < 100 {
		t.Fatalf("go list -deps . returned only %d packages; that is not a whole link closure", len(pkgs))
	}

	var bad []string
	for _, p := range pkgs {
		lower := strings.ToLower(p)
		for _, needle := range secTelemetryModule {
			if strings.Contains(lower, needle) {
				bad = append(bad, p+" (matches "+needle+")")
			}
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error("a telemetry or crash-reporting package is in the binary's link closure: " + b)
	}
	if len(bad) > 0 {
		t.Log("Rule 4 asks for architectural absence, not a disabled flag. " +
			"There is no configuration that makes a linked SDK acceptable.")
	}
	t.Logf("link closure: %d packages, none matching any of %d telemetry markers",
		len(pkgs), len(secTelemetryModule))
}

// TestNoTelemetryLibraryIsImportedAnywhere covers what the link closure cannot:
// a package imported only by a test, a build-tagged file, or a maintainer
// script under hack/ is not in `go list -deps .` and would still be a library
// in the tree, which is what rule 4 forbids.
func TestNoTelemetryLibraryIsImportedAnywhere(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := sourceFiles(root, ".go")
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range files {
		for _, imp := range secGoFileImports(t, root, rel) {
			lower := strings.ToLower(imp)
			for _, needle := range secTelemetryModule {
				if strings.Contains(lower, needle) {
					t.Errorf("%s imports %s, which is a telemetry or crash-reporting library", rel, imp)
				}
			}
		}
	}
}

// TestTelemetryScannerDetectsAPlantedSDK is the "can this fixture produce the
// failure at all?" check for the two tests above.
func TestTelemetryScannerDetectsAPlantedSDK(t *testing.T) {
	planted := []string{
		"github.com/getsentry/sentry-go",
		"go.opentelemetry.io/otel/trace",
		"github.com/DataDog/dd-trace-go/v2/ddtrace",
		"github.com/segmentio/analytics-go/v3",
		"github.com/bugsnag/bugsnag-go/v2",
		"github.com/posthog/posthog-go",
	}
	for _, p := range planted {
		hit := false
		for _, needle := range secTelemetryModule {
			if strings.Contains(strings.ToLower(p), needle) {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("secTelemetryModule does not recognise %q; the scan would pass with it linked", p)
		}
	}
	// And the other direction: this tree's real dependencies must not match,
	// or the check fires on correct code and gets deleted.
	for _, p := range []string{
		"github.com/wailsapp/wails/v2", "gopkg.in/yaml.v3", "golang.org/x/sys/windows",
		"github.com/klauspost/compress/zstd", "pault.ag/go/debian/control",
		"github.com/inferops/debark/core/store",
	} {
		for _, needle := range secTelemetryModule {
			if strings.Contains(strings.ToLower(p), needle) {
				t.Errorf("secTelemetryModule marker %q matches the real dependency %q", needle, p)
			}
		}
	}
}
