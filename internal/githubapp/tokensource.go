package githubapp

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// TokenSource caches installation access tokens per (installation, scope)
// (§2.2.3): renewed when less than five minutes remain, invalidated on a 401
// by the caller. Tokens are never persisted anywhere else (INV-003).
type TokenSource struct {
	Client        *Client
	AppID         int64
	PrivateKeyPEM []byte

	// now is the clock, injectable for tests.
	now func() time.Time

	mu    sync.Mutex
	cache map[string]InstallationToken
}

// NewTokenSource builds a source for one App.
func NewTokenSource(client *Client, appID int64, privateKeyPEM []byte) *TokenSource {
	return &TokenSource{Client: client, AppID: appID, PrivateKeyPEM: privateKeyPEM, now: time.Now, cache: map[string]InstallationToken{}}
}

func scopeKey(installationID int64, repositories []string) string {
	repos := append([]string(nil), repositories...)
	sort.Strings(repos)
	return strings.Join(append([]string{string(rune(installationID))}, repos...), "\x00")
}

// Token returns a valid installation token for the scope, minting one when
// the cache is empty or about to expire.
func (ts *TokenSource) Token(ctx context.Context, installationID int64, repositories []string) (string, error) {
	key := scopeKey(installationID, repositories)
	ts.mu.Lock()
	cached, ok := ts.cache[key]
	ts.mu.Unlock()
	if ok && ts.now().Add(5*time.Minute).Before(cached.ExpiresAt) {
		return cached.Token, nil
	}

	jwt, err := AppJWT(ts.AppID, ts.PrivateKeyPEM, ts.now())
	if err != nil {
		return "", err
	}
	token, err := ts.Client.InstallationToken(ctx, jwt, installationID, repositories)
	if err != nil {
		return "", err
	}
	ts.mu.Lock()
	ts.cache[key] = token
	ts.mu.Unlock()
	return token.Token, nil
}

// Invalidate drops a cached token after a 401 — the next call re-mints.
func (ts *TokenSource) Invalidate(installationID int64, repositories []string) {
	ts.mu.Lock()
	delete(ts.cache, scopeKey(installationID, repositories))
	ts.mu.Unlock()
}
