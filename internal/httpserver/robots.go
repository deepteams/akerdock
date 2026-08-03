package httpserver

import (
	"net/http"
	"strings"
)

// robotsBody disallows the whole control plane. Crawl-delay and per-agent
// stanzas would be noise: there is nothing here any crawler should fetch.
const robotsBody = "User-agent: *\nDisallow: /\n"

// Robots answers /robots.txt for the whole control-plane port, before the
// request can reach the SPA — whose catch-all serves index.html for unknown
// paths, so a missing robots.txt would come back as HTML with a 200, which a
// crawler reads as "no rules" and proceeds to index the dashboard.
//
// It is a middleware rather than a route so the answer does not depend on a
// dashboard build being embedded: an api-mode instance behind the same FQDN
// must give the same reply.
func Robots(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSuffix(r.URL.Path, "/") != "/robots.txt" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(robotsBody))
	})
}
