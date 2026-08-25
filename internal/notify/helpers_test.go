package notify

import (
	"net"
	"net/http"
	"time"
)

// testSender builds a Sender with a PLAIN http client and SMTP dialer — the
// production New() is SSRF-guarded (safedial) and would refuse the loopback
// httptest and fake-SMTP servers these tests point at. The guard itself is
// covered by internal/safedial.
func testSender() *Sender {
	return &Sender{
		HTTP:     &http.Client{Timeout: 10 * time.Second},
		SMTPDial: &net.Dialer{Timeout: 10 * time.Second},
	}
}
