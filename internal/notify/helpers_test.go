package notify

import (
	"net/http"
	"time"
)

// testSender builds a Sender with a PLAIN http client — the production New() is
// SSRF-guarded (safedial) and would refuse the loopback httptest servers these
// tests point at. The guard itself is covered by internal/safedial.
func testSender() *Sender {
	return &Sender{HTTP: &http.Client{Timeout: 10 * time.Second}}
}
