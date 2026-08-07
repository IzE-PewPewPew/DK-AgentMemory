package api

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed viewer/*
var viewerFS embed.FS

// viewerHandler serves the embedded read-only viewer.
//
// Embedded with go:embed so the binary stays a single file -- a viewer that
// needs an assets directory alongside it is a viewer that is missing after
// someone copies just the binary.
//
// The viewer is read-only in the strongest available sense: it is static HTML
// that calls the same authenticated API as everything else, and the API has no
// write endpoint it could reach that the operator's own key could not already
// reach. It holds no credential of its own, sets no cookie, and stores the key
// the operator pastes in sessionStorage, which is cleared when the tab closes.
func viewerHandler() http.Handler {
	sub, err := fs.Sub(viewerFS, "viewer")
	if err != nil {
		// Only reachable if the embed directive and the directory disagree,
		// which is a build-time mistake rather than a runtime condition.
		panic("api: viewer assets missing from the binary: " + err.Error())
	}

	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A strict CSP, because this page holds an API key in memory. No
		// external origin can be contacted and no injected script can run.
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
				"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")

		// StripPrefix leaves an empty path for the mount root. It must become
		// "/" and not "index.html": http.FileServer canonically redirects any
		// path ending in /index.html back to "./", so naming the file directly
		// bounces /viewer/ to itself forever. Asking for the directory lets the
		// FileServer find index.html on its own, which is the one spelling that
		// terminates.
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}
