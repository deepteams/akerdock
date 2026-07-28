// Client ID Metadata Documents (ADR-044): an MCP client identified by an
// https `client_id` that IS the URL of its metadata document. The instance
// fetches that document to learn the client's name and redirect uris, so the
// identity shown at consent is a domain the client demonstrably controls —
// not a self-declared string.
//
// Fetching a caller-supplied URL is the textbook SSRF vector: the fetch goes
// through the instance's hardened outbound client (PRD §23.3), refuses
// redirects, and is bounded in time and size.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/deepteams/akerdock/internal/safedial"
)

const (
	// cimdTimeout bounds one document fetch: a consent redirect is waiting on
	// it, so a slow host must fail fast rather than hang the browser.
	cimdTimeout = 5 * time.Second
	// cimdMaxBody caps the document — metadata, not a payload.
	cimdMaxBody = 64 << 10
	// cimdCacheTTL keeps an authorization round-trip from re-fetching; short
	// enough that a client rotating its redirect uris is picked up quickly.
	cimdCacheTTL = 5 * time.Minute
)

// mcpClient is what the authorization flow needs about a client, whatever
// identified it: a stored registration or a metadata document.
type mcpClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	// Verified is true for a CIMD client: the identity is the document's
	// origin, which nobody else can serve. A DCR client's name is
	// self-declared, and the consent screen says so.
	Verified bool
	// Origin is the document's scheme://host, shown as the identity.
	Origin string
}

// cimdCache is a tiny TTL cache shared by the authorization endpoints.
type cimdCache struct {
	mu      sync.Mutex
	entries map[string]cimdEntry
}

type cimdEntry struct {
	client  mcpClient
	expires time.Time
}

var cimdDocuments = &cimdCache{entries: map[string]cimdEntry{}}

func (c *cimdCache) get(id string) (mcpClient, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	if !ok || time.Now().After(entry.expires) {
		return mcpClient{}, false
	}
	return entry.client, true
}

func (c *cimdCache) put(id string, client mcpClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Bound the cache: a hostile caller must not be able to grow it forever.
	if len(c.entries) > 256 {
		c.entries = map[string]cimdEntry{}
	}
	c.entries[id] = cimdEntry{client: client, expires: time.Now().Add(cimdCacheTTL)}
}

// isCIMD reports whether a client_id is a metadata document URL.
func isCIMD(clientID string) bool {
	return strings.HasPrefix(clientID, "https://")
}

// resolveMcpClient identifies the client of an authorization request: a
// metadata document when the client_id is an https URL (ADR-044), a stored
// registration otherwise.
func (a *API) resolveMcpClient(ctx context.Context, clientID string) (mcpClient, error) {
	if isCIMD(clientID) {
		return a.resolveCIMD(ctx, clientID)
	}
	stored, err := a.Store.GetMcpOauthClient(ctx, clientID)
	if err != nil {
		return mcpClient{}, fmt.Errorf("unknown client_id")
	}
	return mcpClient{
		ID: stored.ClientID, Name: stored.ClientName,
		RedirectURIs: stored.RedirectUris, Verified: false,
	}, nil
}

// cimdDocument is the subset of a client metadata document we read.
type cimdDocument struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name"`
	ClientURI    string   `json:"client_uri"`
	RedirectURIs []string `json:"redirect_uris"`
}

// resolveCIMD fetches and validates a client metadata document. The two
// checks that make the identity meaningful: the document must claim the very
// URL it was served from, and its redirect uris must live on that origin —
// otherwise anyone could host a document impersonating another client.
func (a *API) resolveCIMD(ctx context.Context, clientID string) (mcpClient, error) {
	if client, ok := cimdDocuments.get(clientID); ok {
		return client, nil
	}
	target, err := url.Parse(clientID)
	if err != nil || target.Scheme != "https" || target.Host == "" {
		return mcpClient{}, fmt.Errorf("client_id must be an https URL")
	}
	if target.Fragment != "" || target.User != nil {
		return mcpClient{}, fmt.Errorf("client_id must not carry a fragment or credentials")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return mcpClient{}, fmt.Errorf("cannot fetch the client metadata document")
	}
	req.Header.Set("Accept", "application/json")
	httpClient := safedial.HTTPClient(cimdTimeout)
	// A redirect could walk out of the SSRF guard's evaluation and land the
	// document on a different origin than the client_id: refuse them outright.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return mcpClient{}, fmt.Errorf("cannot reach the client metadata document: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return mcpClient{}, fmt.Errorf("the client metadata document answered %s", resp.Status)
	}
	var doc cimdDocument
	if err := json.NewDecoder(io.LimitReader(resp.Body, cimdMaxBody)).Decode(&doc); err != nil {
		return mcpClient{}, fmt.Errorf("the client metadata document is not valid JSON")
	}
	if doc.ClientID != clientID {
		return mcpClient{}, fmt.Errorf("the document declares a different client_id than the URL it was served from")
	}
	if len(doc.RedirectURIs) == 0 {
		return mcpClient{}, fmt.Errorf("the client metadata document declares no redirect_uris")
	}
	origin := target.Scheme + "://" + target.Host
	for _, uri := range doc.RedirectURIs {
		if !sameOrigin(uri, origin) {
			return mcpClient{}, fmt.Errorf("redirect_uri %s is not on the document's origin %s", uri, origin)
		}
	}
	name := doc.ClientName
	if name == "" {
		name = target.Host
	}
	client := mcpClient{
		ID: clientID, Name: name, RedirectURIs: doc.RedirectURIs,
		Verified: true, Origin: origin,
	}
	cimdDocuments.put(clientID, client)
	return client, nil
}

// sameOrigin reports whether uri is served by origin (scheme://host).
func sameOrigin(uri, origin string) bool {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme+"://"+u.Host == origin
}

// mcpConsentPage renders the authorization screen. Self-contained HTML: it is
// served on the panel origin but must not depend on the SPA being loaded, and
// what it says is the one thing the user has to be able to trust — hence the
// verified origin front and centre, and an explicit warning when the identity
// is only self-declared (a DCR client, ADR-044).
func mcpConsentPage(client mcpClient, req mcpAuthorizeParams, teamName, csrfToken string) string {
	identity := htmlEscape(client.Name)
	var badge string
	if client.Verified {
		badge = `<p class="verified">Verified: this client is served by <strong>` +
			htmlEscape(client.Origin) + `</strong></p>`
	} else {
		badge = `<p class="unverified">This client registered dynamically — its name is
			self-declared and nobody verified it. Approve only if you recognise it.</p>`
	}
	team := htmlEscape(teamName)
	if team == "" {
		team = "your current team"
	}
	return `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize ` + identity + ` — AkerDock</title>
<style>
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
background:#101014;color:#e6e6ea;font:15px/1.55 system-ui,sans-serif}
main{max-width:30rem;padding:2rem}
h1{font-size:1.2rem;margin:0 0 .75rem}
p{margin:.4rem 0;color:#9a9aa5}
.verified{color:#4cb782}
.unverified{color:#d9a441}
ul{margin:1rem 0;padding-left:1.1rem;color:#c9c9d2}
li{margin:.2rem 0}
.actions{display:flex;gap:.6rem;margin-top:1.5rem}
button{font:inherit;padding:.55rem 1.1rem;border-radius:.5rem;border:1px solid #2a2a33;cursor:pointer}
.approve{background:#4cb782;border-color:#4cb782;color:#08130d;font-weight:600}
.deny{background:transparent;color:#e6e6ea}
code{font-family:ui-monospace,monospace;font-size:.85rem;color:#9a9aa5;word-break:break-all}
</style></head><body><main>
<h1>` + identity + ` wants to read your AkerDock inventory</h1>
` + badge + `
<ul>
<li>Scope: <strong>` + team + `</strong> — that team only</li>
<li>Read-only: servers, projects, applications, databases and stacks</li>
<li>It cannot deploy, restart, or read a secret or an environment variable</li>
<li>Access expires after 12 hours</li>
</ul>
<p><code>` + htmlEscape(req.RedirectURI) + `</code></p>
<form method="post" action="/oauth/mcp/approve">
<input type="hidden" name="client_id" value="` + htmlEscape(req.ClientID) + `">
<input type="hidden" name="redirect_uri" value="` + htmlEscape(req.RedirectURI) + `">
<input type="hidden" name="state" value="` + htmlEscape(req.State) + `">
<input type="hidden" name="code_challenge" value="` + htmlEscape(req.Challenge) + `">
<input type="hidden" name="code_challenge_method" value="S256">
<input type="hidden" name="response_type" value="code">
<input type="hidden" name="csrf_token" value="` + htmlEscape(csrfToken) + `">
<div class="actions">
<button class="approve" type="submit" name="approve" value="yes">Approve</button>
<button class="deny" type="submit" name="approve" value="no">Cancel</button>
</div>
</form>
</main></body></html>`
}

// htmlEscape neutralises the client-controlled strings the consent page
// shows: a client name comes from a document we fetched, so it is untrusted
// text on a page whose whole job is to be trustworthy.
func htmlEscape(s string) string {
	return html.EscapeString(s)
}
