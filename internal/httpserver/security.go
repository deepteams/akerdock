package httpserver

import "net/http"

// SecurityHeaders hardens every response of the single control-plane port:
// the dashboard, /auth, the API and /metrics all pass through here.
//
// The policy is written for the dashboard (the only HTML this port serves) and
// is harmless on JSON: a Content-Security-Policy on an API response constrains
// nothing — unless a bug ever makes the API reflect HTML, at which point it is
// exactly the net we want under it.
//
// hsts is derived from the instance FQDN being set, like the Secure cookie
// flag: promising browsers "always HTTPS" on a plain-HTTP instance would lock
// the operator out of their own dashboard for the header's max-age.
func SecurityHeaders(hsts bool) func(http.Handler) http.Handler {
	// Built once: the policy is static, and string concatenation per request
	// would be pure waste.
	csp := "default-src 'self'; " +
		// Angular injects component styles as inline <style> elements: without
		// 'unsafe-inline' the dashboard renders unstyled. Inline SCRIPTS stay
		// forbidden — style injection is a nuisance, script injection is game
		// over, and the value of this header is concentrated there.
		"style-src 'self' 'unsafe-inline'; " +
		"script-src 'self'; " +
		"img-src 'self' data:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"object-src 'none'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'self'; " +
		"form-action 'self'"

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", csp)
			// A control plane has no business being framed, embedded
			// cross-origin, or content-sniffed.
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY") // legacy twin of frame-ancestors
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			// URLs here carry uuids and, on some routes, tokens in query
			// strings (SSE). None of that belongs in another site's logs.
			h.Set("Referrer-Policy", "no-referrer")
			// The dashboard uses none of these; saying so turns a class of
			// XSS escalations into no-ops. WebAuthn needs no entry: publickey
			// credentials default to self, which is exactly right.
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
			if hsts {
				// Two years, no includeSubDomains: the instance FQDN may share
				// a domain with application FQDNs this control plane does not
				// speak for.
				h.Set("Strict-Transport-Security", "max-age=63072000")
			}
			next.ServeHTTP(w, r)
		})
	}
}
