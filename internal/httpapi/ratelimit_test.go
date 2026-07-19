package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLimiterBudget(t *testing.T) {
	l := &Limiter{rate: RateLimitPerMinute, buckets: map[string]*bucket{}}

	// The reference budget of §10.3 is spendable in a burst.
	for i := range RateLimitPerMinute {
		if allowed, _ := l.Allow("token-a"); !allowed {
			t.Fatalf("request %d must be allowed within the budget", i+1)
		}
	}
	allowed, wait := l.Allow("token-a")
	if allowed {
		t.Fatal("the request over budget must be rejected")
	}
	if wait <= 0 {
		t.Fatal("a rejected request must carry a positive Retry-After")
	}
}

func TestLimiterIsolatesTokens(t *testing.T) {
	l := &Limiter{rate: RateLimitPerMinute, buckets: map[string]*bucket{}}
	for range RateLimitPerMinute {
		l.Allow("token-a")
	}
	if allowed, _ := l.Allow("token-b"); !allowed {
		t.Fatal("another token must keep its own budget")
	}
}

// The /auth endpoints answer password and passkey guesses: their budget is a
// separate, much smaller number, and it must actually be the one enforced.
func TestLimiterHonorsACustomRate(t *testing.T) {
	l := &Limiter{rate: AuthRatePerMinute, buckets: map[string]*bucket{}}
	for i := range AuthRatePerMinute {
		if allowed, _ := l.Allow("1.2.3.4"); !allowed {
			t.Fatalf("request %d must be allowed within the auth budget", i+1)
		}
	}
	if allowed, _ := l.Allow("1.2.3.4"); allowed {
		t.Fatalf("request %d must be rejected: the auth budget is %d, not the API's %d",
			AuthRatePerMinute+1, AuthRatePerMinute, RateLimitPerMinute)
	}
}

func TestClientIPKeyNeverExempts(t *testing.T) {
	r := httptest.NewRequest("POST", "/auth/login", nil)
	r.RemoteAddr = "203.0.113.9:4242"
	if got := ClientIPKey(r); got != "203.0.113.9" {
		t.Errorf("ClientIPKey = %q, want the bare host", got)
	}

	// An unparseable RemoteAddr must map to a SHARED key, never to the empty
	// string: an empty key means "no limit" to Limiter.Handler, and a login
	// endpoint exempted from its own brute-force limit is the worst failure
	// mode this function has.
	r.RemoteAddr = "garbage"
	if got := ClientIPKey(r); got == "" {
		t.Fatal("ClientIPKey returned the empty (exempt) key for an unparseable address")
	}
}

// The client must not be able to choose its own bucket: X-Forwarded-For is
// client-controlled, and honouring it would turn "rotate a header" into
// "reset the limiter".
func TestClientIPKeyIgnoresForwardedHeaders(t *testing.T) {
	r := httptest.NewRequest("POST", "/auth/login", nil)
	r.RemoteAddr = "203.0.113.9:4242"
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	if got := ClientIPKey(r); got != "203.0.113.9" {
		t.Errorf("ClientIPKey = %q: the key must come from the connection, not from a header the attacker writes", got)
	}
}

func TestLimiterConstructors(t *testing.T) {
	custom := NewLimiterRate(7)
	if custom.rate != 7 || custom.buckets == nil {
		t.Fatalf("custom limiter = %+v", custom)
	}
	standard := NewLimiter()
	if standard.rate != RateLimitPerMinute || standard.buckets == nil {
		t.Fatalf("standard limiter = %+v", standard)
	}
}

func TestLimiterEvictsOnlyIdleBuckets(t *testing.T) {
	now := time.Now()
	limiter := &Limiter{
		rate: 1,
		buckets: map[string]*bucket{
			"stale": {lastSeen: now.Add(-11 * time.Minute)},
			"live":  {lastSeen: now.Add(-time.Minute)},
		},
	}
	limiter.evict(now.Add(-10 * time.Minute))
	if _, ok := limiter.buckets["stale"]; ok {
		t.Fatal("stale bucket was not evicted")
	}
	if _, ok := limiter.buckets["live"]; !ok {
		t.Fatal("live bucket was evicted")
	}
}

func TestLimiterHandler(t *testing.T) {
	t.Run("empty key bypasses limiter", func(t *testing.T) {
		limiter := &Limiter{rate: 1, buckets: map[string]*bucket{}}
		rec := httptest.NewRecorder()
		limiter.Handler(func(*http.Request) string { return "" })(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Code != http.StatusNoContent || len(limiter.buckets) != 0 {
			t.Fatalf("response = %d, buckets = %v", rec.Code, limiter.buckets)
		}
	})

	t.Run("over budget", func(t *testing.T) {
		limiter := &Limiter{rate: 1, buckets: map[string]*bucket{}}
		handler := limiter.Handler(func(*http.Request) string { return "token" })(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/apps", nil))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/apps", nil))
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" ||
			!strings.Contains(rec.Body.String(), "rate_limited") {
			t.Fatalf("response = %d %v %q", rec.Code, rec.Header(), rec.Body.String())
		}
	})
}
