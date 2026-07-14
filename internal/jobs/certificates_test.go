package jobs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/store"
)

// selfSignedCertB64 mints a throwaway self-signed certificate (test-only key
// material, generated on the fly — nothing real) and returns it the way the
// proxy reports it: PEM, then base64.
func selfSignedCertB64(t *testing.T, issuerCN string, notBefore, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: issuerCN},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return base64.StdEncoding.EncodeToString(pemBytes)
}

func TestCertificateFacts(t *testing.T) {
	// ASN.1 stores validity with one-second precision, in UTC: use times the
	// round trip preserves exactly.
	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-48 * time.Hour)
	future := now.Add(48 * time.Hour)

	garbageDER := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(
		&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")}))

	tests := []struct {
		name       string
		b64        string
		status     store.CertificateStatus
		issuer     *string
		notBefore  time.Time
		notAfter   time.Time
		timesValid bool
	}{
		{
			name:   "invalid base64",
			b64:    "%%% not base64 %%%",
			status: store.CertificateStatusPending,
		},
		{
			name:   "valid base64 but no PEM block",
			b64:    base64.StdEncoding.EncodeToString([]byte("plain text, no PEM")),
			status: store.CertificateStatusPending,
		},
		{
			name:   "PEM block with unparsable certificate",
			b64:    garbageDER,
			status: store.CertificateStatusPending,
		},
		{
			name:       "valid certificate",
			b64:        selfSignedCertB64(t, "Test CA", past, future),
			status:     store.CertificateStatusIssued,
			issuer:     new("Test CA"),
			notBefore:  past,
			notAfter:   future,
			timesValid: true,
		},
		{
			name:       "expired certificate",
			b64:        selfSignedCertB64(t, "Test CA", past, now.Add(-time.Hour)),
			status:     store.CertificateStatusExpired,
			issuer:     new("Test CA"),
			notBefore:  past,
			notAfter:   now.Add(-time.Hour),
			timesValid: true,
		},
		{
			name:       "issued certificate without an issuer common name",
			b64:        selfSignedCertB64(t, "", past, future),
			status:     store.CertificateStatusIssued,
			issuer:     nil,
			notBefore:  past,
			notAfter:   future,
			timesValid: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := certificateFacts(tt.b64)
			if facts.status != tt.status {
				t.Errorf("status = %q, want %q", facts.status, tt.status)
			}
			switch {
			case tt.issuer == nil && facts.issuer != nil:
				t.Errorf("issuer = %q, want nil", *facts.issuer)
			case tt.issuer != nil && facts.issuer == nil:
				t.Errorf("issuer = nil, want %q", *tt.issuer)
			case tt.issuer != nil && *facts.issuer != *tt.issuer:
				t.Errorf("issuer = %q, want %q", *facts.issuer, *tt.issuer)
			}
			if facts.notBefore.Valid != tt.timesValid || facts.notAfter.Valid != tt.timesValid {
				t.Fatalf("timestamps valid = (%v, %v), want %v",
					facts.notBefore.Valid, facts.notAfter.Valid, tt.timesValid)
			}
			if tt.timesValid {
				if !facts.notBefore.Time.Equal(tt.notBefore) {
					t.Errorf("notBefore = %v, want %v", facts.notBefore.Time, tt.notBefore)
				}
				if !facts.notAfter.Time.Equal(tt.notAfter) {
					t.Errorf("notAfter = %v, want %v", facts.notAfter.Time, tt.notAfter)
				}
			}
		})
	}
}
