package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimit is the reference budget of §10.3: 200 requests per minute and
// per token, enforced as a token bucket refilled continuously.
const (
	RateLimitPerMinute = 200
	rateWindow         = time.Minute

	// AuthRatePerMinute is the per-IP budget of the /auth endpoints. Far lower
	// than the API budget: nobody types their password thirty times a minute,
	// and login is the one endpoint that turns a guess into an answer. It only
	// BOUNDS an online attack — the account lockout is the real stop — and it
	// is per IP, so a NAT'd office shares it: tight enough to blunt a scanner,
	// loose enough that a team behind one address still logs in.
	AuthRatePerMinute = 30
)

// Limiter enforces the per-token rate limit. It is in-process: with several
// API instances the effective budget is per instance, which is the accepted
// trade-off until a shared counter is needed (no Redis — ADR-002).
type Limiter struct {
	rate    float64 // budget per rateWindow
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// NewLimiter builds a limiter with the reference API budget and starts its
// eviction loop.
func NewLimiter() *Limiter {
	return NewLimiterRate(RateLimitPerMinute)
}

// NewLimiterRate builds a limiter with an explicit per-minute budget.
func NewLimiterRate(perMinute int) *Limiter {
	l := &Limiter{rate: float64(perMinute), buckets: map[string]*bucket{}}
	go l.evictLoop()
	return l
}

// Allow consumes one unit of the key's budget. It returns false and the
// delay to wait when the budget is exhausted.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	now := time.Now()
	refillPerSecond := l.rate / rateWindow.Seconds()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &bucket{tokens: l.rate - 1, lastSeen: now}
		return true, 0
	}
	b.tokens = min(l.rate, b.tokens+now.Sub(b.lastSeen).Seconds()*refillPerSecond)
	b.lastSeen = now
	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / refillPerSecond * float64(time.Second))
		return false, max(wait, time.Second)
	}
	b.tokens--
	return true, 0
}

// ClientIPKey keys a limiter by caller address. Deliberately RemoteAddr, not
// X-Forwarded-For: a header the CLIENT controls would let an attacker rotate
// through unlimited buckets by rotating a string. Behind a reverse proxy the
// whole proxy shares one bucket — a blunt but unforgeable budget.
func ClientIPKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		// Never exempt: an unparseable address shares one bucket rather than
		// bypassing the limit.
		return "unknown"
	}
	return host
}

// evictLoop drops idle buckets so the map cannot grow unbounded.
func (l *Limiter) evictLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-10 * time.Minute)
		l.evict(cutoff)
	}
}

func (l *Limiter) evict(cutoff time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, key)
		}
	}
}

// Handler rejects requests over budget with 429 + Retry-After (§24.1).
// keyFor identifies the caller; an empty key skips the limit (health).
func (l *Limiter) Handler(keyFor func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyFor(r)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			allowed, wait := l.Allow(key)
			if !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
				WriteError(w, r, http.StatusTooManyRequests, "rate_limited",
					fmt.Sprintf("rate limit exceeded (%d requests per minute) — respect Retry-After", int(l.rate)))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
