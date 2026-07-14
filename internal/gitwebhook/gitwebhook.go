// Package gitwebhook receives and authenticates the webhooks Git providers
// send (spec git-webhook-protocols).
//
// These endpoints are not behind the Bearer token: the signature IS the
// authentication, and the endpoint UUID names the target without revealing it.
// That inverts the usual trust model, so the rules here are strict:
//
//   - the signature is verified against the RAW body, before any parsing —
//     unmarshalling first would mean acting on unauthenticated bytes;
//   - every comparison is constant-time (hmac.Equal), never == on strings;
//   - an invalid signature is persisted for the audit trail and then answered
//     401 — it triggers nothing (INV-009);
//   - no error body distinguishes "wrong secret" from "no secret", nor
//     "unknown endpoint" from "another team's endpoint" (INV-002).
package gitwebhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// MaxBodyBytes bounds a delivery (§1.2). Real push payloads are far below it;
// anything larger is refused with 413 rather than read into memory.
const MaxBodyBytes = 2 << 20 // 2 MiB

// Provider is a Git forge.
type Provider string

// The providers whose signature scheme is implemented.
const (
	GitHub Provider = "github"
	GitLab Provider = "gitlab"
	Gitea  Provider = "gitea"
)

// Supported reports whether a provider can be configured today.
func Supported(p string) bool {
	switch Provider(p) {
	case GitHub, GitLab, Gitea:
		return true
	default:
		return false
	}
}

// DeliveryID reads the provider's delivery identifier — the key that makes a
// replay a duplicate rather than a second deployment (INV-009). Providers do
// not sign a timestamp, so this is the whole anti-replay story.
func DeliveryID(p Provider, h http.Header) string {
	switch p {
	case GitHub:
		return h.Get("X-GitHub-Delivery")
	case GitLab:
		return h.Get("X-Gitlab-Event-UUID")
	case Gitea:
		return h.Get("X-Gitea-Delivery")
	default:
		return ""
	}
}

// EventType reads the event name (push, pull_request…).
func EventType(p Provider, h http.Header) string {
	switch p {
	case GitHub:
		return h.Get("X-GitHub-Event")
	case GitLab:
		return h.Get("X-Gitlab-Event")
	case Gitea:
		// Gitea also emits GitHub-compatible headers; the native one wins.
		if e := h.Get("X-Gitea-Event"); e != "" {
			return e
		}
		return h.Get("X-GitHub-Event")
	default:
		return ""
	}
}

// VerifySignature authenticates a raw body against the endpoint secret.
//
// GitHub and Gitea sign with HMAC-SHA256. GitLab does NOT sign at all: it
// sends the secret in clear in X-Gitlab-Token — that is GitLab's model, not a
// choice made here — so the secret is compared in constant time, exactly like
// a signature would be.
func VerifySignature(p Provider, h http.Header, body, secret []byte) error {
	switch p {
	case GitHub:
		// The legacy SHA-1 header is deliberately ignored.
		return verifyHMAC(h.Get("X-Hub-Signature-256"), "sha256=", body, secret)
	case Gitea:
		sig := h.Get("X-Gitea-Signature")
		if sig == "" {
			sig = h.Get("X-Forgejo-Signature")
		}
		if sig == "" {
			// Some Gitea versions only send the GitHub-compatible header.
			return verifyHMAC(h.Get("X-Hub-Signature-256"), "sha256=", body, secret)
		}
		return verifyHMAC(sig, "", body, secret)
	case GitLab:
		token := h.Get("X-Gitlab-Token")
		if token == "" || !hmac.Equal([]byte(token), secret) {
			return fmt.Errorf("invalid signature")
		}
		return nil
	default:
		return fmt.Errorf("invalid signature")
	}
}

// verifyHMAC checks an hex HMAC-SHA256 of the raw body, with an optional
// provider prefix.
func verifyHMAC(header, prefix string, body, secret []byte) error {
	if header == "" {
		return fmt.Errorf("invalid signature")
	}
	got, ok := strings.CutPrefix(header, prefix)
	if !ok && prefix != "" {
		return fmt.Errorf("invalid signature")
	}
	raw, err := hex.DecodeString(strings.TrimSpace(got))
	if err != nil {
		return fmt.Errorf("invalid signature")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(raw, mac.Sum(nil)) {
		return fmt.Errorf("invalid signature")
	}
	return nil
}

// Sign produces the signature a provider would send. Used by the tests and by
// the E2E harness — never in the request path.
func Sign(body, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Push is what the engine needs out of a push payload, whichever forge sent it.
type Push struct {
	Ref     string
	Commit  string
	Files   []string
	Message string
}

// Branch extracts the branch name from a ref (refs/heads/main → main).
func (p Push) Branch() string {
	return strings.TrimPrefix(p.Ref, "refs/heads/")
}

// Deleted reports a branch deletion push, which must never deploy: the commit
// is all zeroes and there is nothing to build.
func (p Push) Deleted() bool {
	return p.Commit == "" || strings.Trim(p.Commit, "0") == ""
}

// SkipRequested honours the [skip ci] / [skip cd] convention in the head commit
// message — the author explicitly asked for no deployment.
func (p Push) SkipRequested() bool {
	msg := strings.ToLower(p.Message)
	for _, marker := range []string{"[skip ci]", "[ci skip]", "[skip cd]", "[cd skip]"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
