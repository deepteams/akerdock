package httpserver

import (
	"net/http"
	"strings"
)

// robotsBody allows the crawl on purpose (ADR-074). Disallowing it would
// forbid the fetch that reads `X-Robots-Tag: noindex, nofollow` — the header
// this control plane answers everything with, and the only thing that keeps a
// URL out of an index. The two do not add up: the ban silences the header and
// strands any already-indexed URL there for good. The comment ships in the
// file because a bare `Allow: /` on a product that promises "never indexable"
// reads as a bug, and the next reader would put the ban back.
const robotsBody = "# This control plane must not appear in search results, and says so with an\n" +
	"# `X-Robots-Tag: noindex, nofollow` header on every single response. Reading\n" +
	"# that header requires fetching the page, so crawling is allowed on purpose.\n" +
	"# A `Disallow: /` here would not add protection: it would silence the header\n" +
	"# and leave any already-indexed URL in the index permanently.\n" +
	"User-agent: *\nAllow: /\n"

// Robots answers /robots.txt for the whole control-plane port, before the
// request can reach the SPA — whose catch-all serves index.html for unknown
// paths, so a missing robots.txt would come back as HTML with a 200, which a
// crawler reads as "no rules". Permissive is not the same as absent: this one
// says so in plain text, to a crawler and to whoever curls it.
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
