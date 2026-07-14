// Package totp implements RFC 6238 time-based one-time passwords over the
// RFC 4226 HOTP construction, for the dashboard's 2FA (PRD §10.2, §23.3).
//
// It is written in-house rather than imported: the whole algorithm is one
// HMAC, a truncation and a modulo — pinned to the RFC test vectors below —
// and a dependency would be more code to audit than this file.
//
// The parameters are the de-facto interoperable set (SHA-1, 6 digits, 30 s
// steps): every authenticator app supports them, and several support nothing
// else. SHA-1's collision weakness is irrelevant here — HMAC-SHA1 is not
// collision-bound, and the secret is 160 bits of entropy.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// Digits in a code. Six is what authenticator apps display by default.
	Digits = 6

	// Period is the length of one time step.
	Period = 30 * time.Second

	// SecretSize is the secret length in bytes: 160 bits, the RFC 4226
	// recommended minimum, and exactly one SHA-1 block of entropy.
	SecretSize = 20

	// Skew is how many steps on EACH side of "now" a code is accepted for.
	// One step absorbs clock drift and the human delay between reading a code
	// and submitting it; more would multiply an attacker's guessing budget.
	Skew = 1
)

// Base32 codec for secrets: unpadded, upper-case — the alphabet every
// authenticator app expects for manual entry.
var codec = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a fresh random secret in its base32 form.
func GenerateSecret() (string, error) {
	raw := make([]byte, SecretSize)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("totp: secret generation: %w", err)
	}
	return codec.EncodeToString(raw), nil
}

// URI renders the otpauth:// provisioning URI for a secret, as authenticator
// apps consume it (via QR code or tap). The label is "issuer:account" and the
// issuer is repeated as a parameter — both are required for apps to file the
// entry under the right name.
func URI(issuer, account, secret string) string {
	return fmt.Sprintf("otpauth://totp/%s:%s?algorithm=SHA1&digits=%d&issuer=%s&period=%d&secret=%s",
		url.PathEscape(issuer), url.PathEscape(account),
		Digits, url.QueryEscape(issuer), int(Period.Seconds()), secret)
}

// Step is the time-step counter for a given instant (RFC 6238 §4.2).
func Step(at time.Time) int64 {
	return at.Unix() / int64(Period.Seconds())
}

// Validate checks a code against the secret at the given instant, accepting
// ±Skew steps of drift. It returns the step the code matched, so the caller
// can persist it and refuse the SAME step next time — a TOTP is one-time only
// if somebody remembers it was used (data-dictionary §4.3 last_used_at).
//
// The comparison is constant-time per candidate step. The scan deliberately
// tries every candidate even after a match: whether a code matched the first
// or the last step must not be measurable.
func Validate(secret, code string, at time.Time) (matched int64, ok bool) {
	key, err := codec.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return 0, false
	}
	code = strings.TrimSpace(code)
	if len(code) != Digits {
		return 0, false
	}
	for offset := int64(-Skew); offset <= Skew; offset++ {
		step := Step(at) + offset
		if step < 0 {
			continue
		}
		expected := hotp(key, uint64(step))
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 && !ok {
			matched, ok = step, true
		}
	}
	return matched, ok
}

// hotp computes the RFC 4226 code for one counter value: HMAC-SHA1, dynamic
// truncation (§5.3), then the low decimal digits.
func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	truncated := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	return fmt.Sprintf("%0*d", Digits, truncated%1_000_000)
}
