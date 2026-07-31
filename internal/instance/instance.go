// Package instance caches the instance_settings singleton. Settings are
// hot-editable through the API/UI (instance-config §1.1); a short TTL keeps
// multi-instance deployments convergent without a per-request query.
package instance

import (
	"context"
	"sync"
	"time"

	"github.com/deepteams/akerdock/internal/store"
)

const cacheTTL = 3 * time.Second

// Cache serves instance settings with a small TTL. Get sits on the path of
// every authenticated request (auth middleware), so the fresh case takes a
// read lock only, one refill runs at a time, and once a value has been
// served a failing refresh returns the stale value rather than an error — a
// Postgres hiccup must not take the whole API down with it.
type Cache struct {
	q settingsStore

	// refresh single-flights the refill: expirees queue here and all but the
	// first find a fresh value on re-check instead of re-querying.
	refresh sync.Mutex

	mu      sync.RWMutex
	value   store.InstanceSetting
	fetched time.Time
	// primed is true once a value has been fetched successfully: only then may
	// a failed refresh fall back to the stale value.
	primed bool
}

type settingsStore interface {
	GetInstanceSettings(context.Context) (store.InstanceSetting, error)
}

// NewCache builds a settings cache over the store.
func NewCache(q settingsStore) *Cache {
	return &Cache{q: q}
}

// Get returns the current settings, refreshing them when stale.
func (c *Cache) Get(ctx context.Context) (store.InstanceSetting, error) {
	if value, ok := c.fresh(); ok {
		return value, nil
	}
	c.refresh.Lock()
	defer c.refresh.Unlock()
	if value, ok := c.fresh(); ok {
		return value, nil
	}
	value, err := c.q.GetInstanceSettings(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if c.primed {
			return c.value, nil
		}
		return store.InstanceSetting{}, err
	}
	c.value, c.fetched, c.primed = value, time.Now(), true
	return value, nil
}

func (c *Cache) fresh() (store.InstanceSetting, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.primed && time.Since(c.fetched) < cacheTTL {
		return c.value, true
	}
	return store.InstanceSetting{}, false
}

// Invalidate drops the cached value (called after a settings mutation on
// this instance; other instances converge within the TTL). The value stays
// primed: it remains the stale fallback should the next refresh fail.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetched = time.Time{}
}
