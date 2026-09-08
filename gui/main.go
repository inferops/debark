// Command debark-gui is the desktop front end for debark.
//
// It is a Wails v2 application: a Go process that owns all the work, and a
// system webview that renders the interface. This file is deliberately thin.
// It does three things and nothing else — embed the frontend bundle,
// describe the window, and hand Wails the objects the frontend is allowed to
// call. Everything with behaviour lives in internal/.
//
// Linux is the shipping target. Windows builds and runs, but is not a
// supported platform. macOS is out of scope.
package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/inferops/debark/gui/internal/app"
	"github.com/inferops/debark/gui/internal/catalog"
	"github.com/inferops/debark/gui/internal/cliadapter"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

// assets is the frontend bundle, produced by `make frontend`
// (hack/copyfrontend), which is a plain recursive copy of frontend/index.html,
// frontend/src and frontend/wailsjs into frontend/dist. There is no npm and no
// JavaScript build step in this project; see hack/copyfrontend/main.go.
//
// The `all:` prefix matters: without it go:embed skips names beginning with a
// dot or an underscore, which would drop the committed frontend/dist/.gitkeep
// and, more importantly, would silently skip any dotfile the frontend needs.
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	application := newApplication()

	opts := &options.App{
		// The product is Debark. The binary it drives is debark; that name
		// belongs on a command line, not in the window title.
		Title: "Debark",

		// 1280x820 is sized for the widest thing the app has to show without
		// horizontal scrolling — a package table with version, architecture
		// and origin columns beside a details pane. The 960x640 minimum is
		// the point below which that layout has to collapse rather than
		// merely tighten; it is a floor, not a target.
		Width:     1040,
		Height:    720,
		MinWidth:  960,
		MinHeight: 640,

		AssetServer: &assetserver.Options{
			Assets: assets,

			// Every response the webview loads carries a
			// Content-Security-Policy. See contentSecurityPolicy below for
			// what it is and what it costs; docs/security-review.md §6.7 is
			// the finding it closes.
			Middleware: cspMiddleware(assetContentSecurityPolicy(assets)),
		},

		// BackgroundColour is only the fill behind the webview: what the user
		// sees for the few frames between the window appearing and the page
		// painting, and behind any area the page does not cover. It is a
		// single static colour — Wails has no "follow the OS theme" option
		// for it — so it is set to the dark surface the frontend paints, and
		// the frontend's own prefers-color-scheme CSS owns the real page
		// background from first paint onwards. If the frontend ever lands
		// light-first, this one value changes with it.
		BackgroundColour: &options.RGBA{R: 0x18, G: 0x18, B: 0x1B, A: 1},

		Bind: application.Bind(),

		Linux: &linux.Options{
			// Sets the WM program name (g_set_prgname).
			//
			// It is NOT what a Linux desktop matches .desktop files by, which
			// is what this comment used to claim. The packaging work
			// measured it on the running app: `xprop` reports
			// WM_CLASS(STRING) = "debark-gui", "Debark-gui" — GTK took
			// both halves from the EXECUTABLE name, not from this value, which
			// is "debark". So the desktop entry's StartupWMClass has to be
			// debark-gui; a .desktop written from the old reading of this
			// comment would have lost the icon in the dock, and nothing in
			// §5's twenty-row checklist looks at window grouping.
			//
			// What it does still do is set the name GTK reports to the session
			// (g_get_prgname), which shows up in some window-manager and
			// notification contexts. That is worth having and costs nothing;
			// it is simply not load-bearing for the icon.
			ProgramName: "debark",

			// Wails uses WebviewGpuPolicyNever when options.Linux is nil,
			// deliberately, because hardware acceleration crashes the
			// webview on a range of Linux GPU/driver combinations
			// (wailsapp/wails#2977). Supplying a non-nil Linux options
			// struct silently re-enables it, because Always is the zero
			// value — so the safe default has to be restated here. This is
			// the shipping platform; a blank window on someone's laptop is
			// not worth the frames.
			WebviewGpuPolicy: linux.WebviewGpuPolicyNever,
		},

		Windows: &windows.Options{
			// The dark-mode-aware part that Wails genuinely provides: the
			// title bar, its text and the window border follow the system
			// theme and keep following it when the user switches, rather
			// than being frozen at whatever the theme was at launch.
			Theme: windows.SystemDefault,
			CustomTheme: &windows.ThemeSettings{
				DarkModeTitleBar:   windows.RGB(0x18, 0x18, 0x1B),
				DarkModeTitleText:  windows.RGB(0xE4, 0xE4, 0xE7),
				DarkModeBorder:     windows.RGB(0x27, 0x27, 0x2A),
				LightModeTitleBar:  windows.RGB(0xFA, 0xFA, 0xFA),
				LightModeTitleText: windows.RGB(0x18, 0x18, 0x1B),
				LightModeBorder:    windows.RGB(0xE4, 0xE4, 0xE7),
			},
		},
	}

	opts.OnStartup = application.Startup
	opts.OnBeforeClose = application.BeforeClose
	opts.OnShutdown = application.Shutdown

	if err := wails.Run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "debark-gui: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Content-Security-Policy
// ---------------------------------------------------------------------------

// cspIndexPath is the embedded document the webview loads, and the only place
// an inline script may live. hack/copyfrontend copies frontend/index.html here
// verbatim.
const cspIndexPath = "frontend/dist/index.html"

// cspHeader is the response header name. Spelled out rather than taken from a
// constant elsewhere so a grep for the header finds this file.
const cspHeader = "Content-Security-Policy"

// cspMiddleware sets the policy on every response the asset server produces.
//
// This is the seam Wails gives us: assetserver.Options.Middleware wraps the
// handler that serves the embedded files, and — because the recorder Wails
// uses to re-render index.html embeds the real ResponseWriter — a header set
// here survives the runtime-script injection and reaches the webview. On
// Linux/WebKit2GTK it arrives as a real HTTP response header, forwarded into
// the WebKitURISchemeResponse's soup headers by
// pkg/assetserver/webview/webkit2_36+.go, and WebKit enforces it exactly as it
// would over https.
//
// Two Wails-internal paths, /wails/runtime.js and /wails/ipc.js, are answered
// before the middleware runs and therefore carry no header of their own. That
// is harmless: a policy governs the document that loads a script, not the
// script's own response, and both are same-origin `src` loads that script-src
// 'self' already covers.
func cspMiddleware(policy string) assetserver.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(cspHeader, policy)
			next.ServeHTTP(w, r)
		})
	}
}

// assetContentSecurityPolicy builds the policy for a given asset bundle,
// hashing whatever inline scripts the embedded index.html actually contains.
//
// Deriving the hash at startup rather than pinning it as a constant is a
// deliberate departure from the recommendation in docs/security-review.md
// §6.7, and the reason is the failure mode. A pinned hash that falls out of
// step with the script does not fail loudly: it blocks the theme bootstrap,
// and the app opens in the wrong theme with nothing on screen to say why. A
// derived hash cannot fall out of step. The review's real requirement — that
// nobody adds an inline script without a person looking at it — is met by
// TestIndexHTMLHasExactlyOneInlineScript instead, which fails the build when a
// second one appears.
//
// A bundle with no readable index.html still yields a usable policy: Wails
// itself will fail the launch over the missing file, and a half-built policy
// here would only confuse that error.
func assetContentSecurityPolicy(fsys fs.FS) string {
	doc, err := fs.ReadFile(fsys, cspIndexPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debark-gui: cannot read %s for the content security policy: %v\n", cspIndexPath, err)
		return contentSecurityPolicy(nil)
	}
	return contentSecurityPolicy(inlineScriptHashes(doc))
}

// contentSecurityPolicy is the policy itself. scriptHashes are CSP source
// expressions ("'sha256-…'") for the inline scripts index.html carries.
//
// What each clause is doing, because a policy nobody can read is a policy
// nobody will keep true:
//
//   - default-src 'none' — nothing loads unless a clause below says so. This
//     is what makes the rest of the list exhaustive: media, workers, manifests
//     and frames have no clause because the app has none of them, and the
//     default denies them.
//   - script-src 'self' plus one hash per inline script. 'self' is the app's
//     own origin (wails://wails on Linux, http://wails.localhost on Windows),
//     which covers src/main.js, every module it imports, and the two scripts
//     Wails injects. The hash covers the theme bootstrap in <head>, which must
//     stay inline: it applies the stored theme before first paint and a
//     deferred module cannot. 'unsafe-inline' would have bought the same thing
//     and given back most of the header's value.
//   - style-src 'self' 'unsafe-inline' — the one clause that is relaxed, and
//     the reason is real rather than precautionary. Three screens
//     (picker-list.js, picker-search.js, picker-tray.js) build their layout CSS
//     with document.createElement('style'), and a <style> element's text is
//     checked against style-src whether it was parsed from markup or created
//     by script. 'self' alone therefore unstyles the whole picker. The cost is
//     bounded by the rest of the policy: CSS can only exfiltrate by fetching
//     something, and img-src, font-src and connect-src leave it nowhere to
//     fetch from. See docs/security-review.md §6.7 for the change that would
//     let this drop back to 'self'.
//   - img-src 'self' data: — no <img> element exists today; data: is kept
//     because an inline icon is the one thing that would plausibly want it.
//   - connect-src 'none' — the clause worth having. Rule 3 (no network call to
//     any debark-operated endpoint) is proven for the frontend by
//     internal/audit reading the source; this makes it a property of the
//     runtime as well, so a fetch(), XHR, WebSocket, EventSource or
//     sendBeacon added by a future commit fails in the webview too. Note that
//     it does not touch the Wails bridge: desktop IPC is
//     webkit.messageHandlers.external.postMessage, not a network fetch.
//   - form-action 'none', base-uri 'none', object-src 'none',
//     frame-ancestors 'none' — the standard four. There is no form, no <base>,
//     no plugin content and nothing may frame this document.
//
// There is deliberately no report-uri or report-to. A violation report is a
// network call, and rule 4 forbids telemetry of every kind.
func contentSecurityPolicy(scriptHashes []string) string {
	script := make([]string, 0, len(scriptHashes)+1)
	script = append(script, "'self'")
	script = append(script, scriptHashes...)

	return strings.Join([]string{
		"default-src 'none'",
		"script-src " + strings.Join(script, " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"object-src 'none'",
	}, "; ")
}

// inlineScriptHashes returns one CSP source expression per inline <script> in
// doc — every <script> element with no src attribute — in document order.
//
// Hand-written rather than done with an HTML parser on purpose. What CSP
// hashes is the element's child text exactly as it appears between the tags,
// and the shortest way to get exactly those bytes is to apply HTML's own
// raw-text rule: everything after the tag's '>' up to the next "</script".
// Round-tripping through a parser and a serialiser would be a second thing to
// trust; TestServedIndexHTMLMatchesItsOwnHash checks the answer against the
// bytes Wails really serves instead.
func inlineScriptHashes(doc []byte) []string {
	scripts := inlineScripts(doc)
	if len(scripts) == 0 {
		return nil
	}
	out := make([]string, 0, len(scripts))
	for _, s := range scripts {
		sum := sha256.Sum256(s)
		out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return out
}

// inlineScripts returns the child text of every inline <script> in doc, in
// document order, as the exact bytes CSP hashes.
func inlineScripts(doc []byte) [][]byte {
	const open = "<script"

	lower := bytes.ToLower(doc)
	var out [][]byte

	for i := 0; i < len(doc); {
		rel := bytes.Index(lower[i:], []byte(open))
		if rel < 0 {
			break
		}
		nameEnd := i + rel + len(open)
		if nameEnd < len(doc) && !isTagNameEnd(doc[nameEnd]) {
			// <scripts>, <scriptfoo>: not a script element.
			i = nameEnd
			continue
		}

		gt := bytes.IndexByte(doc[nameEnd:], '>')
		if gt < 0 {
			break
		}
		tagEnd := nameEnd + gt
		bodyStart := tagEnd + 1

		closeRel := bytes.Index(lower[bodyStart:], []byte("</script"))
		if closeRel < 0 {
			break
		}
		bodyEnd := bodyStart + closeRel

		if !tagHasSrc(doc[nameEnd:tagEnd]) {
			out = append(out, doc[bodyStart:bodyEnd])
		}
		i = bodyEnd
	}
	return out
}

// isTagNameEnd reports whether c can follow a tag name.
func isTagNameEnd(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '/', '>':
		return true
	}
	return false
}

// tagHasSrc reports whether the attribute text of a start tag — the bytes
// between the tag name and its '>' — declares a src attribute.
//
// It walks attributes rather than searching for "src", so neither data-src nor
// a value that happens to contain the word can be mistaken for one.
func tagHasSrc(attrs []byte) bool {
	i := 0
	for i < len(attrs) {
		for i < len(attrs) && isASCIISpace(attrs[i]) {
			i++
		}
		if i >= len(attrs) || attrs[i] == '/' {
			break
		}

		start := i
		for i < len(attrs) && !isASCIISpace(attrs[i]) && attrs[i] != '=' && attrs[i] != '/' {
			i++
		}
		name := strings.ToLower(string(attrs[start:i]))

		for i < len(attrs) && isASCIISpace(attrs[i]) {
			i++
		}
		if i < len(attrs) && attrs[i] == '=' {
			i++
			for i < len(attrs) && isASCIISpace(attrs[i]) {
				i++
			}
			if i < len(attrs) && (attrs[i] == '"' || attrs[i] == '\'') {
				quote := attrs[i]
				i++
				for i < len(attrs) && attrs[i] != quote {
					i++
				}
				if i < len(attrs) {
					i++
				}
			} else {
				for i < len(attrs) && !isASCIISpace(attrs[i]) {
					i++
				}
			}
		}

		if name == "src" {
			return true
		}
	}
	return false
}

func isASCIISpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f':
		return true
	}
	return false
}

// newApplication constructs the object graph the frontend can call into.
//
// The three seams the app is built around are wired here and nowhere else:
// the CLI adapter (every debark invocation), the catalogue (the browsing
// index), and the readiness checker. Each is an interface, so the whole
// application can be driven against fakes — which is what internal/app's
// smoke test does.
//
// Nothing here fails the launch. A missing debark binary, an unreadable
// cache directory or a machine with no apt are all conditions the readiness
// screen exists to explain, and an app that refuses to start cannot explain
// anything. Errors are carried into the UI instead.
func newApplication() *app.App {
	// Lazy: New touches no filesystem and starts no process, so a machine
	// without debark still gets a window and a readiness screen telling it
	// what to install.
	cli, err := cliadapter.New(cliadapter.Options{})
	if err != nil {
		cli = nil
	}

	return app.New(app.Deps{
		CLI: cli,
		Catalog: catalog.New(catalog.Options{
			ToolVersion: "debark-gui " + version,
		}),
		Version: version,
	})
}

// version is the build identity, overridden at link time:
//
//	go build -ldflags "-X main.version=0.1.0"
var version = "dev"
