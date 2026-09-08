// Wails' asset server links against the platform webview: on Linux that is
// webkit2gtk through cgo, and the ABI is chosen by a build tag. The Makefile
// and .golangci.yml both pass webkit2_41 on every Go command for exactly this
// reason. The tag here keeps a bare `go test ./...` on a Linux box with no
// webkit headers compiling, rather than turning this file into everyone
// else's problem.
//go:build !linux || webkit2_41

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	wailsassetserver "github.com/wailsapp/wails/v2/pkg/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

// stubRuntime is the RuntimeAssets seam Wails' asset server needs in order to
// answer /wails/runtime.js and /wails/ipc.js. Nothing in this test loads them;
// they only have to exist.
type stubRuntime struct{}

func (stubRuntime) DesktopIPC() []byte       { return []byte("// ipc") }
func (stubRuntime) WebsocketIPC() []byte     { return []byte("// ws ipc") }
func (stubRuntime) RuntimeDesktopJS() []byte { return []byte("// runtime") }

// TestServedIndexHTMLMatchesItsOwnHash is the test that makes the hash
// trustworthy rather than plausible.
//
// The hash is computed from the embedded index.html, but that is not the
// document the webview parses: Wails re-parses index.html with
// golang.org/x/net/html to inject its two runtime <script src> tags and
// re-serialises the result. If that round trip changed one byte of the inline
// theme bootstrap, the shipped hash would not match what the engine computes,
// the script would be blocked, and the only symptom would be the app quietly
// opening in the wrong theme.
//
// So this drives the real asset server — the same constructor Wails calls,
// with the same middleware the app installs — and checks the hash in the
// response header against the script in the response body.
func TestServedIndexHTMLMatchesItsOwnHash(t *testing.T) {
	policy := assetContentSecurityPolicy(assets)

	srv, err := wailsassetserver.NewAssetServer("", assetserver.Options{
		Assets:     assets,
		Middleware: cspMiddleware(policy),
	}, false, nil, stubRuntime{})
	if err != nil {
		t.Fatalf("NewAssetServer: %v (run `make frontend`?): %v", err, err)
	}

	for _, path := range []string{"/", "/index.html"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))

			if rr.Code != http.StatusOK {
				t.Fatalf("GET %s: status %d", path, rr.Code)
			}
			header := rr.Header().Get(cspHeader)
			if header == "" {
				t.Fatalf("GET %s: no %s header on the response the webview receives", path, cspHeader)
			}
			if header != policy {
				t.Fatalf("GET %s: header %q does not match the policy the app built %q", path, header, policy)
			}

			body := rr.Body.Bytes()

			// The document really did go through Wails' rewriter, or this
			// test is proving nothing.
			if !strings.Contains(string(body), "/wails/runtime.js") {
				t.Fatalf("GET %s: the served document carries no injected runtime script; "+
					"this test is not exercising the rewrite it exists to check", path)
			}

			served := inlineScripts(body)
			if len(served) != 1 {
				t.Fatalf("GET %s: %d inline scripts in the served document, want 1", path, len(served))
			}
			sum := sha256.Sum256(served[0])
			want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !strings.Contains(header, want) {
				t.Errorf("GET %s: the served inline script hashes to %s, which the policy does not allow:\n%s\n"+
					"script as served:\n%s", path, want, header, served[0])
			}

			// And the byte-for-byte claim, stated directly: what Wails serves
			// is what was embedded.
			embedded, err := fs.ReadFile(assets, cspIndexPath)
			if err != nil {
				t.Fatalf("read %s: %v", cspIndexPath, err)
			}
			source := inlineScripts(embedded)
			if len(source) != 1 || string(source[0]) != string(served[0]) {
				t.Errorf("GET %s: the inline script changed in the rewrite.\nembedded:\n%q\nserved:\n%q",
					path, source, served[0])
			}
		})
	}
}

// TestServedSubresourcesCarryThePolicy checks the other half of the seam: the
// modules and stylesheets the document pulls in are served through the same
// middleware, so a stale cached response cannot arrive without a policy.
func TestServedSubresourcesCarryThePolicy(t *testing.T) {
	policy := assetContentSecurityPolicy(assets)

	srv, err := wailsassetserver.NewAssetServer("", assetserver.Options{
		Assets:     assets,
		Middleware: cspMiddleware(policy),
	}, false, nil, stubRuntime{})
	if err != nil {
		t.Fatalf("NewAssetServer: %v", err)
	}

	for _, path := range []string{"/src/main.js", "/src/design/tokens.css", "/nothing-here.js"} {
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rr.Header().Get(cspHeader); got != policy {
			t.Errorf("GET %s (status %d): %s = %q", path, rr.Code, cspHeader, got)
		}
	}
}
