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
