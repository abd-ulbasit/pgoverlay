package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// The web UI is a single embedded HTML page (vanilla JS/CSS, zero external
// assets) so branchd stays one self-contained binary and works air-gapped.
//
//go:embed ui
var uiFiles embed.FS

// uiHandler serves the embedded UI under /ui/. The assets are static and
// secret-free, so they bypass auth; the page itself asks for the API token
// and sends it as a bearer header on every /v1 call.
func uiHandler() http.Handler {
	sub, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic(err) // impossible: "ui" is embedded above
	}
	return http.StripPrefix("/ui/", http.FileServerFS(sub))
}
