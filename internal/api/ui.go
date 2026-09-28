package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// The web UI is a single embedded page (vanilla HTML/JS/CSS, zero external
// assets) so branchd stays one self-contained binary and works air-gapped.
//
//go:embed ui
var uiFiles embed.FS

// uiCSP locks the UI to its own same-origin files: no inline or third-party
// script/style, API calls only to this origin, and no framing (the page acts
// on a stored bearer token, so a framing page must not be able to clickjack
// its destroy/reset buttons).
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// uiHandler serves the embedded UI under /ui/. The assets are static and
// secret-free, so they bypass auth; the page itself asks for the API token
// and sends it as a bearer header on every /v1 call. Every response carries
// the CSP above plus the legacy anti-framing and anti-sniffing headers.
func uiHandler() http.Handler {
	sub, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic(err) // impossible: "ui" is embedded above
	}
	files := http.StripPrefix("/ui/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", uiCSP)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		files.ServeHTTP(w, r)
	})
}
