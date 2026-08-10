package agent

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// PendingRoute claims one public host for a preview that has no container yet
// (ADR-073 §1): the control plane writes the preview's Traefik file at
// reservation, pointing at the agent, and deposits this entry alongside it so
// the URL answers with the preview's state instead of a 404 during the queue,
// the clone and the build.
//
// State is what the control plane last wrote — `queued`, `deploying` or
// `failed`. The agent renders it and never asks for a fresher one: it holds no
// API token and no view of the control plane (INV-007, ADR-073 §2). A state
// that is momentarily stale costs one refresh cycle.
//
// The entry is removed when the deployment points the route at the container,
// so a host is never both pending and routed (ADR-073 §3).
type PendingRoute struct {
	Host         string `json:"host"`
	ResourceUUID string `json:"resource_uuid"`
	State        string `json:"state"`
	PRNumber     int    `json:"pr_number,omitempty"`
}

// Preview states a pending entry can carry — the states of §21 the dashboard
// shows for a preview that is not serving yet.
const (
	PreviewStateQueued    = "queued"
	PreviewStateDeploying = "deploying"
	PreviewStateFailed    = "failed"
)

// PreviewStateHeader marks a response as the pending-preview holding page and
// names the state it rendered — the diagnostic sibling of X-AkerDock-Scale,
// which does the same for the scale-to-zero pages. Its value is always one of
// the constants above (an unrecognised deposited state normalises to
// "queued"), so a probe can switch on it without sanitising.
const PreviewStateHeader = "X-AkerDock-Preview"

// pendingRefreshSeconds is the auto-refresh cadence of the holding page, in
// step with the waking page's: the moment the deployment switches the route,
// the next refresh reaches the container instead of this page.
const pendingRefreshSeconds = 2

// normalizePreviewState maps a deposited state onto the three the page knows.
// Anything else — an older control plane, a state this agent predates — is
// treated as queued: the preview is not serving, which is the only claim the
// page makes, and refreshing is the recoverable behaviour. Only an explicit
// `failed` stops the refresh.
func normalizePreviewState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case PreviewStateDeploying:
		return PreviewStateDeploying
	case PreviewStateFailed:
		return PreviewStateFailed
	default:
		return PreviewStateQueued
	}
}

// previewStateWords is what the page says for one state, in the words the
// dashboard uses for the same badge.
type previewStateWords struct {
	title   string
	detail  string
	refresh bool
}

func previewWords(state string) previewStateWords {
	switch state {
	case PreviewStateDeploying:
		return previewStateWords{
			title:   "This preview is deploying",
			detail:  "It is being built and started. The page refreshes by itself and shows the preview as soon as it answers.",
			refresh: true,
		}
	case PreviewStateFailed:
		return previewStateWords{
			title:  "This preview failed",
			detail: "Its last deployment did not complete, so there is nothing to serve here yet. The page no longer refreshes.",
		}
	default:
		return previewStateWords{
			title:   "This preview is queued",
			detail:  "It is waiting for a deployment slot. The page refreshes by itself and shows the preview as soon as it answers.",
			refresh: true,
		}
	}
}

// pendingRoute returns the pending preview claiming this host, if any.
func (w *Waker) pendingRoute(host string) (PendingRoute, bool) {
	p, ok := w.pending[host]
	return p, ok
}

// servePendingPage answers a visitor on a host whose preview has no container
// yet. Nothing here is fetched and nothing beyond the state and the PR number
// is shown: the access wall in front may be `none` (ADR-073 §4), so the page
// must be safe to a stranger — no repository, no branch, no error, no log.
func (w *Waker) servePendingPage(rw http.ResponseWriter, req *http.Request, pending PendingRoute) {
	state := normalizePreviewState(pending.State)
	words := previewWords(state)

	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	// no-store, as the waking page's: the holding page must not survive in a
	// cache one second past the route switch (ADR-073 §3).
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set(PreviewStateHeader, state)
	if words.refresh {
		rw.Header().Set("Retry-After", strconv.Itoa(pendingRefreshSeconds))
	}
	rw.WriteHeader(http.StatusServiceUnavailable)
	if req.Method == http.MethodHead {
		return
	}
	_, _ = rw.Write([]byte(renderPendingPage(hostname(req.Host), pending, state)))
}

// renderPendingPage builds the self-contained HTML, in the family of the
// waking page and the ingress offline page: no external asset (there is no
// application behind this host to serve one), auto-refresh while the preview
// can still become ready, and English like every UI string (§25.2).
func renderPendingPage(host string, pending PendingRoute, state string) string {
	words := previewWords(state)

	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	if words.refresh {
		fmt.Fprintf(&b, "<meta http-equiv=\"refresh\" content=\"%d\">", pendingRefreshSeconds)
	}
	fmt.Fprintf(&b, "<title>Preview %s — %s</title>", state, htmlEscape(host))
	b.WriteString("<style>body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;" +
		"background:#101014;color:#e6e6ea;font:15px/1.5 system-ui,sans-serif}" +
		"main{max-width:26rem;padding:2rem}" +
		"h1{font-size:1.15rem;font-weight:600;margin:0 0 .35rem}" +
		"p{margin:.25rem 0;color:#9a9aa5}" +
		"ul{list-style:none;margin:1.25rem 0 0;padding:0}" +
		"li{display:flex;align-items:center;gap:.6rem;padding:.3rem 0;font-family:ui-monospace,monospace;font-size:.85rem}" +
		".dot{width:.55rem;height:.55rem;border-radius:50%;flex:none}" +
		".queued .dot{background:#d9a441;animation:p 1.2s ease-in-out infinite}" +
		".deploying .dot{background:#d9a441;animation:p 1.2s ease-in-out infinite}" +
		".failed .dot{background:#d9534f}" +
		".state{margin-left:auto;color:#9a9aa5}" +
		"@keyframes p{50%{opacity:.35}}")
	b.WriteString("</style></head><body><main>")
	fmt.Fprintf(&b, "<h1>%s</h1>", words.title)
	fmt.Fprintf(&b, "<p>%s</p>", words.detail)
	b.WriteString("<ul>")
	label := "preview"
	if pending.PRNumber > 0 {
		label = fmt.Sprintf("pull request #%d", pending.PRNumber)
	}
	fmt.Fprintf(&b, "<li class=\"%s\"><span class=\"dot\"></span>%s<span class=\"state\">%s</span></li>",
		state, htmlEscape(label), state)
	b.WriteString("</ul>")
	b.WriteString("</main></body></html>")
	return b.String()
}
