package audit

// The HTML-injection sink inventory.
//
// Package descriptions, file paths, volume labels, `debark` stderr and
// vendor URLs all reach the DOM, and every screen's header comment claims it
// never assigns `innerHTML`. This test is what turns that claim into a
// property: it enumerates every sink in `frontend/` that can turn a string into
// markup, and fails on any that is not in a reviewed table with a reason.
//
// # Why a source scan and not only a browser run
//
// Both were done, and neither is sufficient alone. The hostile-fixture run
// (docs/security-review.md §2) drives the real screens with real payloads and
// proves the escaping holds on the paths it reaches; it reached 28 of the 49
// bound methods, because the rest need an interaction sequence a harness cannot
// invent. A source scan reaches the other 21 and every path a future commit
// adds, and it is the half that cannot rot: a new `innerHTML` fails this test
// on the commit that introduces it, months before anyone thinks to re-run a
// browser harness.
//
// # Why the list is longer than `innerHTML`
//
// Because a grep for `innerHTML` misses most of the ways a string becomes
// markup: `insertAdjacentHTML`, `outerHTML`, `document.write`,
// `DOMParser.parseFromString`, `Range.createContextualFragment`, an `href` or
// `src` built from an operator-supplied string, and a `style` attribute that
// can carry `url(…)`. Each of those is a separate entry below.

import (
	"path/filepath"
	"regexp"
	"testing"
)

// secDOMSink is one way a string can become markup, script or a network
// reference in the webview.
var secDOMSinks = []struct {
	pattern *regexp.Regexp
	what    string
}{
	{regexp.MustCompile(`\.innerHTML\s*=`), "assignment to innerHTML"},
	{regexp.MustCompile(`\.outerHTML\s*=`), "assignment to outerHTML"},
	{regexp.MustCompile(`\.insertAdjacentHTML\s*\(`), "insertAdjacentHTML"},
	{regexp.MustCompile(`document\s*\.\s*write(ln)?\s*\(`), "document.write"},
	{regexp.MustCompile(`\bnew\s+DOMParser\b|\.parseFromString\s*\(`), "DOMParser.parseFromString"},
	{regexp.MustCompile(`\.createContextualFragment\s*\(`), "Range.createContextualFragment"},
	{regexp.MustCompile(`\.srcdoc\s*=`), "assignment to iframe.srcdoc"},
	{regexp.MustCompile(`\beval\s*\(`), "eval"},
	{regexp.MustCompile(`\bnew\s+Function\s*\(`), "new Function"},
	{regexp.MustCompile(`setTimeout\s*\(\s*['"` + "`" + `]`), "setTimeout with a string body"},
	{regexp.MustCompile(`setInterval\s*\(\s*['"` + "`" + `]`), "setInterval with a string body"},
	{regexp.MustCompile(`setAttribute\s*\(\s*['"](href|src|xlink:href|srcdoc|style|formaction|action|data)['"]`),
		"setAttribute of a URL- or style-bearing attribute"},
	{regexp.MustCompile(`\.(href|src|action|formAction)\s*=[^=]`), "assignment to a URL-bearing property"},
	{regexp.MustCompile(`\.style\.cssText\s*=`), "assignment to style.cssText"},
}

// secSinkException is one reviewed sink: the exact file, the line's shape, and
// why it is safe. The value is the reason, and a reason is required — an entry
// whose justification nobody can state is a finding, not an exception.
//
// The key is `<path>|<what>`; every occurrence in that file of that sink kind
// is covered by the one entry, because the reason below is a statement about
// the file's construction rather than about a line number that will move.
var secSinkException = map[string]string{
	"src/shell/shell.js|assignment to innerHTML": "svg() builds an <svg> element with " +
		"document.createElementNS and assigns ICON[kind] to its innerHTML. ICON is a frozen " +
		"six-entry object literal of path geometry; every call site passes either a literal " +
		"member (ICON.close, ICON.back, ICON.danger) or ICON[normaliseKind(k)], and " +
		"normaliseKind maps anything outside the four-member TOAST_KINDS set to 'info'. " +
		"No value derived from the backend can reach it. Verified by reading all seven call " +
		"sites and by the hostile-fixture run, which drives every banner and toast kind.",

	"src/shell/shell.js|assignment to a URL-bearing property": "skip.href = '#shell-content', " +
		"a same-document fragment written as a literal.",

	"src/main.js|assignment to a URL-bearing property": "link.href is the resolved URL of one of " +
		"two literal, module-relative stylesheet paths (new URL('./design/tokens.css', " +
		"import.meta.url)). addStylesheet has no other caller and no other argument.",

	"src/screens/picker-tray.js|assignment to a URL-bearing property": "a.href = safeHref(doc_url), " +
		"and safeHref parses with new URL() and returns null for anything whose protocol is not " +
		"http: or https:. The javascript: fixture produced no anchor; the https: control " +
		"produced one, so the path is live and the refusal is real rather than the code " +
		"never running.",
}

// secDevOnlySinkNote records that dev harnesses are scanned too. They are not
// shipped — hack/copyfrontend's isDevArtifact excludes *.demo.html and
// gallery.html from the embedded bundle — but a sink in one is still a sink a
// reviewer should see, and a demo page is exactly where an unescaped fixture
// would be written without thinking.
const secDevOnlySinkNote = "dev harness; excluded from the bundle by hack/copyfrontend"

func TestFrontendHTMLSinksAreInventoried(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	frontend := filepath.Join(root, "frontend")
	files, err := sourceFiles(frontend, ".js", ".mjs", ".html")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 15 {
		t.Fatalf("scanned only %d frontend files; the walk is not reaching frontend/", len(files))
	}

	used := map[string]bool{}
	for _, rel := range files {
		src, err := readSource(frontend, rel)
		if err != nil {
			t.Fatal(err)
		}
		// Comments are blanked here: every screen's header comment says in
		// prose that it never assigns innerHTML, and a check that fires on the
		// sentence promising the property is a check nobody keeps.
		scan := secBlankComments(src)
		for _, s := range secDOMSinks {
			hits := s.pattern.FindAllStringIndex(scan, -1)
			if len(hits) == 0 {
				continue
			}
			key := rel + "|" + s.what
			if _, ok := secSinkException[key]; ok {
				used[key] = true
				continue
			}
			if isDevOnlyFrontend(rel) {
				t.Logf("frontend/%s: %d × %s (%s)", rel, len(hits), s.what, secDevOnlySinkNote)
				continue
			}
			for _, m := range hits {
				t.Errorf("frontend/%s:%d: %s reaches the DOM here and is not in secSinkException. "+
					"Untrusted text — package descriptions, file paths, volume labels, debark's "+
					"stderr, vendor URLs — reaches every screen; put it in through textContent, or "+
					"add an entry here saying why this one cannot carry it.",
					rel, lineOf(scan, m[0]), s.what)
			}
		}
	}

	// An exception for a sink that no longer exists is an exception nobody
	// will re-examine, and it quietly widens the allowance for the next edit.
	for key := range secSinkException {
		if !used[key] {
			t.Errorf("secSinkException still allows %q, but no such sink is in the tree any more; "+
				"remove the entry", key)
		}
	}
}

// TestSinkScannerDetectsAPlantedSink is the "can this fixture produce the
// failure at all?" check: fourteen patterns that match nothing would look
// exactly like a clean tree.
func TestSinkScannerDetectsAPlantedSink(t *testing.T) {
	planted := map[string]string{
		"assignment to innerHTML":                           `el.innerHTML = row.description;`,
		"assignment to outerHTML":                           `el.outerHTML = payload;`,
		"insertAdjacentHTML":                                `el.insertAdjacentHTML('beforeend', stderr);`,
		"document.write":                                    `document.write(label);`,
		"DOMParser.parseFromString":                         "const d = new DOMParser().parseFromString(`<p>${s}</p>`, 'text/html');",
		"Range.createContextualFragment":                    `r.createContextualFragment(markup);`,
		"assignment to iframe.srcdoc":                       `f.srcdoc = summary;`,
		"eval":                                              `eval(expr);`,
		"new Function":                                      `const f = new Function('return ' + expr);`,
		"setTimeout with a string body":                     `setTimeout("go()", 10);`,
		"setInterval with a string body":                    `setInterval('tick()', 10);`,
		"setAttribute of a URL- or style-bearing attribute": `a.setAttribute('href', vendorURL);`,
		"assignment to a URL-bearing property":              `img.src = iconRef;`,
		"assignment to style.cssText":                       `n.style.cssText = 'width:' + w;`,
	}
	byWhat := map[string]*regexp.Regexp{}
	for _, s := range secDOMSinks {
		byWhat[s.what] = s.pattern
	}
	for what, code := range planted {
		re, ok := byWhat[what]
		if !ok {
			t.Errorf("no sink pattern named %q; the planted-code map and secDOMSinks disagree", what)
			continue
		}
		if !re.MatchString(code) {
			t.Errorf("the %s pattern did not match %q", what, code)
		}
	}
	if len(byWhat) != len(planted) {
		t.Errorf("secDOMSinks has %d patterns but only %d are exercised; every pattern needs a "+
			"planted example or it could be matching nothing", len(byWhat), len(planted))
	}

	// The safe forms must not match, or the scan fires on correct code.
	for _, safe := range []string{
		`node.textContent = row.description;`,
		`node.appendChild(document.createTextNode(stderr));`,
		`el.setAttribute('aria-label', label);`,
		`el.setAttribute('class', 'df-chip');`,
		`if (a.href === href) return a;`,
		`node.style.setProperty(k, o.style[k]);`,
	} {
		for _, s := range secDOMSinks {
			if s.pattern.MatchString(safe) {
				t.Errorf("the %s pattern matched the safe form %q", s.what, safe)
			}
		}
	}
}
