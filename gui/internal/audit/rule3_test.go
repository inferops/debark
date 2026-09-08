package audit

// Rule 3, mechanically.
//
//	"No network call to any debark-operated endpoint. Ever. Not behind a
//	 flag, not for updates, not for a catalogue. The only hosts this app
//	 contacts are the distro archives named in the target's own sources, and
//	 vendor .deb URLs the operator typed."
//	                              — docs/dev/contract-brief.md, rule 3
//
// and the definition-of-done item it carries:
//
//	"No network call to any debark-operated endpoint exists in the tree —
//	 proven by a test, not asserted."
//
// Five tests, because no single one of them is sufficient and each fails for a
// different reason:
//
//  1. TestNoDebarkOperatedHostAppearsInAnySource — nothing in the tree names
//     a host under a domain this project would operate. The direct reading.
//  2. TestOnlyInventoriedFilesTouchTheNetwork — the set of files that can reach
//     the network at all is fixed, small, and each entry says why. This is the
//     one that catches a future "check for updates on start-up": it does not
//     matter which host such a commit names, because it has to import a
//     network package from a file that is not on the list.
//  3. TestNoRequestURLIsCompiledIn — at every request-construction site inside
//     those files, the destination is a runtime value, never a literal and
//     never a package-level constant. This is what turns "the only hosts are
//     the ones the target's sources name" into a structural property.
//  4. TestArchiveHostHasNoCompiledInDefault — the one place that dials a bare
//     host never picks that host itself.
//  5. TestFrontendHasNoNetworkPrimitive — the webview half. Go's link closure
//     says nothing about a `fetch()` in a screen, a remote `<script src>` or a
//     web font, each of which is a network call from the operator's machine.
//
// And one test about the tests: TestRule3ScannerDetectsAPlantedEndpoint runs
// the same matchers over source that *does* violate the rule, so a green run
// above cannot be a green run of matchers that match nothing.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// secPlant builds a string that violates rule 3 without putting the violation
// in this file as a literal — "%P" becomes the project name at run time.
//
// The alternative was to exclude this file from the scan, and that is the worse
// trade: an exclusion is permanent and covers everything the file will ever
// contain, whereas this keeps internal/audit inside the same scan as every
// other package. A test file that is exempt from the rule it enforces is the
// obvious place to hide a violation.
func secPlant(s string) string { return strings.ReplaceAll(s, "%P", "deb"+"ark") }

// ---------------------------------------------------------------------------
// 1. No debark-operated host, anywhere in the tree
// ---------------------------------------------------------------------------

// secURLHost matches the host of any absolute URL, in any file type. Kept
// deliberately loose on the scheme so that a `wss://`, an `ftp://` or a
// hand-rolled `debark+https://` is caught by the same rule as an https URL.
var secURLHost = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]{1,15})://([^\s"'` + "`" + `<>()\[\]{},;\\]+)`)

// secBareDomain is the backstop for a host that never appears in a URL — one
// assembled at run time, or handed to a dialler as `host:port`.
//
// It is a heuristic and it is scoped to say so: only a label sequence ending in
// `debark.<tld>` for a TLD a project would plausibly register. The list is
// short on purpose. `.events`, `.build` and `.tools` are real gTLDs, and this
// tree already contains `debark.events/v1`, `debark.buildjob/v1` and
// `debark.theme` as schema ids and storage keys. A check that fires on
// correct code is a check that gets deleted, so those suffixes are left out and
// the URL matcher above is what covers them.
var secBareDomain = regexp.MustCompile(`(?i)\b(?:[a-z0-9-]+\.)*debark\.(com|net|org|io|dev|app|sh|ai|co|cloud|xyz|me|tech|link|host|systems|software)\b`)

// secDebarkOperated reports whether host is one this project would run.
//
// The test is on the host, never on the path: `github.com/inferops/debark/gui`
// is a repository under GitHub's control, reachable by anyone, and naming it is
// not a phone-home. `catalog.%P.io` is the thing rule 3 forbids, and the
// difference between the two is which side of the `://…/` the word sits on.
func secDebarkOperated(host string) bool {
	h := strings.ToLower(host)
	// userinfo first: `user:pw@telemetry.example/x` hides the host behind a
	// colon, so trimming at the first ":" would read the username as the host.
	if i := strings.LastIndex(h, "@"); i >= 0 {
		h = h[i+1:]
	}
	if i := strings.IndexAny(h, "/:?#"); i >= 0 {
		h = h[:i]
	}
	for _, label := range strings.Split(h, ".") {
		if label == "debark" {
			return true
		}
	}
	return strings.HasPrefix(h, "debark-") || strings.HasSuffix(h, "-debark")
}

// secExemptDebarkToken lists every occurrence the two matchers above flag
// that is not an endpoint, with the reason it is not.
//
// The key is `<repo-relative path>|<token>`, and the file half is the point:
// an exemption is scoped to the one file that carries it, so a hostname
// allowed as an example in a document is still a failure the moment it appears
// in Go source or in a screen. A token-only exemption would be a hole shaped
// exactly like the thing being guarded against — write the endpoint into a
// document once, and it is permitted everywhere.
//
// An exemption has to be written down, here, with a reason a reviewer can
// disagree with. A matcher quietly widened until it stops matching is
// indistinguishable from one that was never right.
var secExemptDebarkToken = map[string]string{
	secPlant("docs/security-review.md|catalog.%P.io"): "prose. The security review's own " +
		"§4.6 explains what this check forbids, and it cannot do that without naming an " +
		"example of a forbidden host. Scoped to that file: the same string in any .go or " +
		".js file is still a failure.",
}

func TestNoDebarkOperatedHostAppearsInAnySource(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	// Packaging scripts are in the list on purpose. An installer is the one
	// artefact that legitimately downloads things, so it is the one place a
	// URL would not look out of place — see docs/security-review.md §6.6.
	files, err := sourceFiles(root,
		".go", ".js", ".mjs", ".ts", ".html", ".css", ".json", ".yml", ".yaml", ".md", ".mod",
		".sh", ".nsi", ".nsh", ".plist", ".ps1", ".bat", ".desktop", ".service", ".policy", ".rules")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 50 {
		t.Fatalf("scanned only %d files; the walk is not reaching the tree", len(files))
	}

	var findings []string
	used := map[string]bool{}
	for _, rel := range files {
		src, err := readSource(root, rel)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range secURLHost.FindAllStringSubmatchIndex(src, -1) {
			host := src[m[4]:m[5]]
			if !secDebarkOperated(host) {
				continue
			}
			tok := src[m[2]:m[3]] + "://" + host
			if _, ok := secExemptDebarkToken[rel+"|"+tok]; ok {
				used[rel+"|"+tok] = true
				continue
			}
			findings = append(findings,
				rel+":"+strconv.Itoa(lineOf(src, m[0]))+": URL names a debark-operated host: "+tok)
		}
		for _, m := range secBareDomain.FindAllStringIndex(src, -1) {
			tok := src[m[0]:m[1]]
			if _, ok := secExemptDebarkToken[rel+"|"+tok]; ok {
				used[rel+"|"+tok] = true
				continue
			}
			findings = append(findings,
				rel+":"+strconv.Itoa(lineOf(src, m[0]))+": names a debark-operated domain: "+tok)
		}
	}

	sort.Strings(findings)
	for _, f := range findings {
		t.Error(f)
	}
	if len(findings) > 0 {
		t.Log("Rule 3 is absolute: there is no hosted API, no catalogue service and no phone-home. " +
			"If one of the above is genuinely not an endpoint, add `<path>|<token>` to " +
			"secExemptDebarkToken with the reason.")
	}

	// An exemption for something that is no longer in the file it names is an
	// exemption nobody will re-examine, and it silently widens the allowance
	// for the next edit to that file.
	for key := range secExemptDebarkToken {
		if !used[key] {
			t.Errorf("secExemptDebarkToken still allows %q, but no such token is in that file "+
				"any more; remove the entry", key)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. The inventory of files that may touch the network
// ---------------------------------------------------------------------------

// secNetworkImports is every import that can produce a socket. `net/url` is
// deliberately absent: it parses, it does not connect, and internal/cliadapter
// uses it to redact credentials out of an error message.
var secNetworkImports = map[string]bool{
	"net":                          true,
	"net/http":                     true,
	"net/http/httptest":            true,
	"net/http/httputil":            true,
	"net/rpc":                      true,
	"net/smtp":                     true,
	"crypto/tls":                   true,
	"golang.org/x/net/proxy":       true,
	"github.com/gorilla/websocket": true,

	// The engine's HTTP fetcher. Not a standard-library network
	// package, and listed for the same reason gorilla/websocket is: it is a
	// package whose whole purpose is to open a socket, and until this audit
	// existed nothing stopped a file here importing it. It IS already in the binary's
	// link closure — core/verify reaches it — so this is not about the
	// closure. It is about which files in THIS repository can call it.
	"github.com/inferops/debark/core/fetch": true,
}

// secNetworkFiles is every file permitted to import one of those, and why.
//
// The reason is not decoration. Rule 3's protection is not that these files are
// careful; it is that there are few of them and each contacts a host it was
// handed. A new entry is a design change, and having to edit this table is what
// forces that change to be argued for rather than merged.
var secNetworkFiles = map[string]string{
	"internal/catalog/packages.go": "fetches the target's own Packages index. The destination is " +
		"IndexRef.URL(), built from Source.URIs, which come from the target's own sources — never " +
		"from this repository.",
	"internal/catalog/dep11.go": "fetches the target's own DEP-11 Components index, from the same IndexRef.",
	"internal/readiness/checks.go": "the opt-in archive-reachability check. It dials Options.ArchiveHost, " +
		"which this repository never gives a default: the check is not registered when it is empty, so a " +
		"session that has not chosen a target never opens a socket. TestArchiveHostHasNoCompiledInDefault " +
		"pins that.",
	"internal/catalog/packages_test.go": "drives the loader against httptest servers on the loopback " +
		"interface. A test that reached a real archive would be flaky and a rule-3 hole at once.",
	"internal/catalog/dep11_test.go":    "same, for the DEP-11 loader.",
	"internal/readiness/checks_test.go": "substitutes archiveDialer; the net import is for net.Conn and net.Error.",
	"internal/catalog/realcorpus_bench_test.go": "serves a staged, on-disk archive corpus through an " +
		"http.RoundTripper. It constructs http.Response values and never dials: a benchmark that " +
		"downloaded 26 MB per iteration would be measuring the mirror.",
	"hack/fetch-indexes.go": "a maintainer script that re-fetches the benchmark corpus from an archive the " +
		"operator names on the command line. Behind //go:build ignore, so it is in no build of any package " +
		"and not in the binary's link closure — TestNetworkScriptsAreNotLinked pins that.",

	// Added with the Content-Security-Policy
	// (docs/security-review.md §6.7a). These three are a DIFFERENT KIND of
	// entry from the seven above and the distinction is the whole reason they
	// are safe: the seven dial. These do not, and cannot — they name net/http
	// only for the http.Handler, http.ResponseWriter and http.Request types of
	// an in-process asset server that answers the webview out of an embed.FS.
	// There is no client, no transport, no dialler and no URL in any of them,
	// which is why TestNoRequestURLIsCompiledIn finds no request-construction
	// site to check. If one of these files ever gains an http.Client, this
	// reason is wrong and the entry has to be argued again.
	"main.go": "installs the CSP middleware on Wails' assetserver.Options. net/http is imported for " +
		"http.Handler/http.ResponseWriter/http.Request — the middleware's own signature. Nothing here " +
		"constructs a client, a transport or a URL, and on Linux the asset server is a WebKit custom-URI-" +
		"scheme handler with no socket behind it at all.",
	"csp_test.go": "drives the middleware with httptest.NewRecorder and httptest.NewRequest, in memory. " +
		"httptest.NewRequest builds a *http.Request without a listener; no server is started and nothing " +
		"is dialled.",
	"csp_assetserver_test.go": "same, one layer out: it drives Wails' real assetserver.NewAssetServer " +
		"through httptest so the policy can be checked against the document the webview actually receives. " +
		"In memory, no listener.",

	// Added with the command-preview redaction
	// (docs/security-review.md §6.1a). Same shape as the three above: named
	// for a pure function, not for a client.
	"internal/cliadapter/redact.go": "calls core/fetch's RedactURL, the engine's single " +
		"decision about how a possibly-credentialed URL may reach something a person reads. RedactURL " +
		"takes a string and returns a string; nothing in this file constructs a Client, a Transport or a " +
		"Request. The import is listed because core/fetch is the package that CAN dial, and the point of " +
		"this inventory is that the set of files able to reach it is small and argued for.",
}

// secGoFileImports returns the import paths of one Go file.
func secGoFileImports(t *testing.T, root, rel string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(rel)), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	out := make([]string, 0, len(f.Imports))
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

func TestOnlyInventoriedFilesTouchTheNetwork(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := sourceFiles(root, ".go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 40 {
		t.Fatalf("scanned only %d Go files; the walk is not reaching the tree", len(files))
	}

	networked := map[string][]string{}
	for _, rel := range files {
		for _, p := range secGoFileImports(t, root, rel) {
			if secNetworkImports[p] {
				networked[rel] = append(networked[rel], p)
			}
		}
	}

	var unexpected []string
	for rel, imports := range networked {
		if _, ok := secNetworkFiles[rel]; !ok {
			unexpected = append(unexpected, rel+" imports "+strings.Join(imports, ", "))
		}
	}
	sort.Strings(unexpected)
	for _, u := range unexpected {
		t.Error("a file outside the network inventory can open a socket: " + u)
	}
	if len(unexpected) > 0 {
		t.Log("Adding an entry to secNetworkFiles is a design decision, not a test fix. Rule 3 holds " +
			"because there are a handful of such files and every one of them contacts a host it was handed.")
	}

	// The inventory must not rot the other way either: an entry for a file
	// that no longer touches the network is an entry nobody will question.
	//
	// A missing file is logged rather than failed, deliberately. A stale entry
	// is dead weight, not a hole — the property being protected is about files
	// that DO reach the network — and failing on it would mean that deleting a
	// benchmark somewhere else in the tree breaks this package's build.
	present := make(map[string]bool, len(files))
	for _, rel := range files {
		present[rel] = true
	}
	for rel := range secNetworkFiles {
		switch {
		case !present[rel]:
			t.Logf("secNetworkFiles lists %s, which no longer exists; the entry can be removed", rel)
		case present[rel] && len(networked[rel]) == 0:
			t.Errorf("secNetworkFiles lists %s, but it imports no network package any more; remove the entry", rel)
		}
	}
}

// TestNetworkScriptsAreNotLinked pins the one claim in secNetworkFiles that no
// import scan can see: hack/fetch-indexes.go is a maintainer script, so its
// network access is not the application's.
func TestNetworkScriptsAreNotLinked(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	src, err := readSource(root, "hack/fetch-indexes.go")
	if err != nil {
		t.Fatal(err)
	}
	// `//go:build ignore` is what keeps it out of every build of every package.
	if !strings.Contains(src, "//go:build ignore") {
		t.Error("hack/fetch-indexes.go reaches the network and is no longer behind //go:build ignore, " +
			"so it may now be part of a build. Re-check secNetworkFiles' claim about it.")
	}
}

// ---------------------------------------------------------------------------
// 3. No request URL is compiled in
// ---------------------------------------------------------------------------

// secURLArg gives, for a package-level function call, the index of the
// argument carrying the destination.
var secURLArg = map[string]int{
	"http.Get": 0, "http.Head": 0, "http.Post": 0, "http.PostForm": 0,
	"http.NewRequest": 1, "http.NewRequestWithContext": 2,
	"net.Dial": 1, "net.DialTimeout": 1, "tls.Dial": 1,
}

// secURLArgMethod gives the same for a method called on a value, which is how
// `client.Get(url)` and `(&net.Dialer{}).DialContext(ctx, "tcp", addr)` reach
// the network. Only applied inside the inventoried files, where a `Get` is
// overwhelmingly likely to be an HTTP one.
var secURLArgMethod = map[string]int{
	"Get": 0, "Head": 0, "Post": 0, "PostForm": 0,
	"Dial": 1, "DialContext": 2,
}

// secLooksLikeDestination keeps `resp.Header.Get("Content-Length")` and
// `d.DialContext(ctx, "tcp", addr)` out of the results: a literal is only a
// destination if it carries a scheme or reads as a host.
var secLooksLikeDestination = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://|^[a-z0-9-]+(\.[a-z0-9-]+)+(:\d+)?$`)

func TestNoRequestURLIsCompiledIn(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()

	checked := 0
	for rel := range secNetworkFiles {
		f, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(rel)), nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}

		// Package-level string constants and vars that read as a location. An
		// identifier argument naming one of these is a compiled-in endpoint
		// wearing a name, which is the shape this check exists to refuse.
		located := map[string]string{}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					if v, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(v, "://") {
						located[name.Name] = v
					}
				}
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			idx, found := -1, false
			if id, ok := sel.X.(*ast.Ident); ok {
				if i, ok := secURLArg[id.Name+"."+sel.Sel.Name]; ok {
					idx, found = i, true
				}
			}
			if !found {
				if i, ok := secURLArgMethod[sel.Sel.Name]; ok {
					idx, found = i, true
				}
			}
			if !found || idx >= len(call.Args) {
				return true
			}

			pos := rel + ":" + strconv.Itoa(fset.Position(call.Args[idx].Pos()).Line)
			switch arg := call.Args[idx].(type) {
			case *ast.BasicLit:
				v, err := strconv.Unquote(arg.Value)
				if err != nil || !secLooksLikeDestination.MatchString(strings.ToLower(v)) {
					return true // a header name, a network kind: not a destination
				}
				checked++
				t.Errorf("%s: the destination is the string literal %q — rule 3 forbids a compiled-in endpoint", pos, v)
			case *ast.Ident:
				checked++
				if v, bad := located[arg.Name]; bad {
					t.Errorf("%s: the destination is the package-level constant %s = %q — "+
						"a compiled-in endpoint with a name is still a compiled-in endpoint", pos, arg.Name, v)
				}
			default:
				checked++
			}
			return true
		})
	}

	// A run that checked nothing proves nothing. If the request builders are
	// renamed or wrapped, this fails rather than passing silently.
	if checked < 4 {
		t.Fatalf("only %d request-construction sites were found across %d inventoried files; "+
			"secURLArg/secURLArgMethod are out of date and this test is no longer checking anything",
			checked, len(secNetworkFiles))
	}
}

// TestArchiveHostHasNoCompiledInDefault pins the readiness check's own argument
// for why it is allowed to exist: the host is never this repository's to pick.
func TestArchiveHostHasNoCompiledInDefault(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := sourceFiles(root, ".go")
	if err != nil {
		t.Fatal(err)
	}
	assign := regexp.MustCompile(`ArchiveHost\s*[:=]\s*"([^"]+)"`)
	for _, rel := range files {
		if strings.HasSuffix(rel, "_test.go") {
			continue // a test naming a host it never dials is what the seam is for
		}
		src, err := readSource(root, rel)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range assign.FindAllStringSubmatchIndex(src, -1) {
			t.Errorf("%s:%d: ArchiveHost is given the compiled-in value %q. A hostname compiled into "+
				"this repository and dialled on launch is a beacon regardless of who owns the other end.",
				rel, lineOf(src, m[0]), src[m[2]:m[3]])
		}
	}
}

// ---------------------------------------------------------------------------
// 4. The webview half
// ---------------------------------------------------------------------------

// secFrontendEgress is every way a page in this application could reach the
// network, and the reason each is refused.
//
// The list is mostly the ones a grep for `fetch(` misses: a stylesheet
// `@import`, a web font, a remote `<script src>` and a service worker are all
// network calls, and none of them is JavaScript.
var secFrontendEgress = []struct {
	pattern *regexp.Regexp
	what    string
}{
	{regexp.MustCompile(`\bfetch\s*\(`), "fetch()"},
	{regexp.MustCompile(`\bXMLHttpRequest\b`), "XMLHttpRequest"},
	{regexp.MustCompile(`\bnew\s+WebSocket\b`), "WebSocket"},
	{regexp.MustCompile(`\bnew\s+EventSource\b`), "EventSource"},
	{regexp.MustCompile(`\bsendBeacon\s*\(`), "navigator.sendBeacon"},
	{regexp.MustCompile(`\bnavigator\.serviceWorker\b`), "a service worker"},
	{regexp.MustCompile(`\bimportScripts\s*\(`), "importScripts"},
	{regexp.MustCompile(`\bnew\s+(Shared)?Worker\s*\(`), "a worker"},
	{regexp.MustCompile(`(?i)<script[^>]+src\s*=\s*["']?[a-z]+:`), "a remote <script src>"},
	{regexp.MustCompile(`(?i)<link[^>]+href\s*=\s*["']?[a-z]+://`), "a remote <link href>"},
	{regexp.MustCompile(`(?i)<(img|iframe|video|audio|source|embed|object)[^>]+(src|data)\s*=\s*["']?[a-z]+://`), "a remote subresource"},
	{regexp.MustCompile(`@import\b`), "a stylesheet @import"},
	{regexp.MustCompile(`@font-face\b`), "a web font"},
	{regexp.MustCompile(`(?i)url\(\s*["']?[a-z]+:`), "a CSS url() with a scheme"},
}

// secBlankComments replaces the bytes of /* … */ and <!-- … --> comments with
// spaces, preserving every offset so a line number stays right.
//
// Comments are blanked for this scan and only this scan. `tokens.css` opens
// with "No build step, no npm, no @import, no webfont, no network reference",
// which is a promise about the file, not a violation of it — and a check that
// fires on a comment saying the right thing teaches people to stop reading it.
// The rule-3 host scan above deliberately does NOT blank comments: a URL in a
// comment is still an endpoint someone wrote down.
func secBlankComments(src string) string {
	b := []byte(src)
	blank := func(i, j int) {
		for k := i; k < j && k < len(b); k++ {
			if b[k] != '\n' {
				b[k] = ' '
			}
		}
	}
	for _, pair := range [][2]string{{"/*", "*/"}, {"<!--", "-->"}} {
		for i := 0; ; {
			s := strings.Index(string(b[i:]), pair[0])
			if s < 0 {
				break
			}
			s += i
			e := strings.Index(string(b[s:]), pair[1])
			if e < 0 {
				blank(s, len(b))
				break
			}
			e += s + len(pair[1])
			blank(s, e)
			i = e
		}
	}
	// Line comments, but never the "//" inside a scheme.
	out := make([]byte, 0, len(b))
	lines := strings.Split(string(b), "\n")
	for i, ln := range lines {
		if j := strings.Index(ln, "//"); j >= 0 && (j == 0 || ln[j-1] != ':') {
			ln = ln[:j] + strings.Repeat(" ", len(ln)-j)
		}
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, ln...)
	}
	return string(out)
}

func TestFrontendHasNoNetworkPrimitive(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	frontend := filepath.Join(root, "frontend")
	files, err := sourceFiles(frontend, ".js", ".mjs", ".html", ".css")
	if err != nil {
		t.Fatal(err)
	}

	shipped := 0
	for _, rel := range files {
		if isDevOnlyFrontend(rel) {
			continue
		}
		shipped++
		src, err := readSource(frontend, rel)
		if err != nil {
			t.Fatal(err)
		}
		src = secBlankComments(src)
		for _, e := range secFrontendEgress {
			for _, m := range e.pattern.FindAllStringIndex(src, -1) {
				t.Errorf("frontend/%s:%d: %s. The frontend talks to the backend over the Wails bridge "+
					"and to nothing else; a page that can reach the network can phone home.",
					rel, lineOf(src, m[0]), e.what)
			}
		}
	}
	if shipped < 15 {
		t.Fatalf("only %d shipped frontend files were scanned; the walk or isDevOnlyFrontend is excluding too much", shipped)
	}
}

// ---------------------------------------------------------------------------
// The test about the tests
// ---------------------------------------------------------------------------

// TestRule3ScannerDetectsAPlantedEndpoint runs every matcher above over source
// that violates rule 3 in each of the ways it can be violated.
//
// Without this, a matcher that silently stopped matching — a refactor, a
// regexp escape that changed meaning — would turn five passing tests into five
// tests that read every file and find nothing. The engine's own
// review names this failure mode: the question to ask of any test guarding a
// security property is "can this fixture produce the failure at all?"
func TestRule3ScannerDetectsAPlantedEndpoint(t *testing.T) {
	for _, u := range []string{
		secPlant("https://api.%P.io/v1/catalog"),
		secPlant("http://updates.%P.dev/latest.json"),
		secPlant("wss://events.%P.app/stream"),
		secPlant("https://user:pw@telemetry.%P.co/ingest"),
	} {
		m := secURLHost.FindStringSubmatch(u)
		if m == nil {
			t.Errorf("secURLHost did not match %q at all", u)
			continue
		}
		if !secDebarkOperated(m[2]) {
			t.Errorf("secDebarkOperated(%q) = false, so %q would pass the scan", m[2], u)
		}
	}

	for _, bare := range []string{
		secPlant("api.%P.io"), secPlant("%P.cloud"), secPlant("cdn.%P.software"),
	} {
		if !secBareDomain.MatchString(bare) {
			t.Errorf("secBareDomain did not match the bare host %q", bare)
		}
	}

	// And the other direction: the tokens this tree legitimately contains must
	// not match, or the check fires on correct code and gets deleted.
	for _, ok := range []string{
		secPlant("%P.events/v1"), secPlant("%P.theme"), secPlant("%P.buildjob/v1"),
		secPlant("https://github.com/%P/%P-gui"), secPlant("%P.tar"),
	} {
		if secBareDomain.MatchString(ok) {
			t.Errorf("secBareDomain matched %q, which is a schema id or a repository path, not a host", ok)
		}
		if m := secURLHost.FindStringSubmatch(ok); m != nil && secDebarkOperated(m[2]) {
			t.Errorf("secDebarkOperated flagged %q, whose host is %q", ok, m[2])
		}
	}

	// The frontend matchers, against a page that phones home six ways.
	planted := `<link rel="stylesheet" href="https://cdn.example/x.css">
	<script src="https://cdn.example/a.js"></script>
	<style>@import url("https://fonts.example/f.css"); @font-face { src: url(https://f.example/a.woff2); }</style>
	<script>fetch('https://telemetry.example/e'); new WebSocket('wss://x/y'); navigator.sendBeacon('/b');</script>`
	hit := map[string]bool{}
	for _, e := range secFrontendEgress {
		if e.pattern.MatchString(planted) {
			hit[e.what] = true
		}
	}
	for _, want := range []string{
		"fetch()", "WebSocket", "navigator.sendBeacon", "a remote <script src>",
		"a remote <link href>", "a stylesheet @import", "a web font",
	} {
		if !hit[want] {
			t.Errorf("secFrontendEgress failed to detect %s in the planted page", want)
		}
	}

	// An exemption must be scoped to its file. A token-only exemption would be
	// a hole shaped exactly like the thing being guarded against.
	for key := range secExemptDebarkToken {
		if !strings.Contains(key, "|") {
			t.Errorf("secExemptDebarkToken key %q is not `<path>|<token>`; a token-only "+
				"exemption permits that host everywhere in the tree", key)
		}
	}

	// Comment blanking must hide a comment and nothing else.
	if got := secBlankComments("/* no @import here */\na { color: red }"); strings.Contains(got, "@import") {
		t.Error("secBlankComments left an @import inside a block comment visible")
	}
	if got := secBlankComments("@import url(x);\n"); !strings.Contains(got, "@import") {
		t.Error("secBlankComments blanked a real @import that was not in a comment")
	}
	if got := secBlankComments("const u = 'https://archive.example/a';"); !strings.Contains(got, "https://archive.example") {
		t.Error("secBlankComments treated the // in a URL scheme as a line comment")
	}
}
