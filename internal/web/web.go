// Package web serves the dashboard from the single port of the control plane
// (§27.1, ADR-021): UI, API, SSE and terminal all live behind one port, so an
// operator opens one URL and a reverse proxy has one thing to forward.
//
// The build output is embedded in the binary — there is no second artefact to
// deploy, and no way for the UI and the API to drift out of sync at runtime.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist holds the built Angular app. The directory is committed empty except for
// a placeholder so a Go-only checkout still builds; `make web` fills it.
//
//go:embed all:dist
var dist embed.FS

// Handler serves the SPA. Returns nil when no build is embedded, so the server
// can run API-only (a worker, or a dev loop against `ng serve`).
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil // no build embedded
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")

		// Deep links (/applications/…) are client routes, not files: they must
		// serve index.html, or a page reload would 404 — the same trap the
		// static build pack's nginx config avoids.
		if path != "" {
			if _, err := fs.Stat(sub, path); err == nil {
				// Hashed assets are immutable: their name changes when they do.
				if strings.HasPrefix(path, "chunk-") || strings.Contains(path, ".js") || strings.Contains(path, ".css") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		// index.html must never be cached: it names the current asset hashes, and
		// a stale copy would load assets that no longer exist after an upgrade.
		w.Header().Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	})
}
