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

// Cache serves instance settings with a small TTL.
type Cache struct {
	q *store.Queries

	mu      sync.Mutex
	value   store.InstanceSetting
	fetched time.Time
}

// NewCache builds a settings cache over the store.
func NewCache(q *store.Queries) *Cache {
	return &Cache{q: q}
}

// Get returns the current settings, refreshing them when stale.
func (c *Cache) Get(ctx context.Context) (store.InstanceSetting, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.fetched) < cacheTTL {
		return c.value, nil
	}
	value, err := c.q.GetInstanceSettings(ctx)
	if err != nil {
		return store.InstanceSetting{}, err
	}
	c.value, c.fetched = value, time.Now()
	return value, nil
}

// Invalidate drops the cached value (called after a settings mutation on
// this instance; other instances converge within the TTL).
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetched = time.Time{}
}
