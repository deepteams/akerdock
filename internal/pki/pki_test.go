package pki

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func parseCertPEM(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("certificate PEM does not decode")
	}
	if block.Type != "CERTIFICATE" {
		t.Fatalf("PEM block type = %q, want %q", block.Type, "CERTIFICATE")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}

func TestNewCA(t *testing.T) {
	before := time.Now()
	ca, err := NewCA("server-1")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	cert := parseCertPEM(t, ca.CertPEM)

	if !cert.IsCA {
		t.Error("IsCA = false, want true")
	}
	if !cert.BasicConstraintsValid {
		t.Error("BasicConstraintsValid = false, want true")
	}
	if !cert.MaxPathLenZero {
		t.Error("MaxPathLenZero = false, want true: the CA must never sign an intermediate")
	}
	if cert.MaxPathLen != 0 {
		t.Errorf("MaxPathLen = %d, want 0", cert.MaxPathLen)
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("KeyUsage lacks KeyUsageCertSign")
	}

	// The template backdates NotBefore by an hour to absorb clock skew.
	if !cert.NotBefore.Before(before) {
		t.Errorf("NotBefore = %v, want before %v (clock-skew tolerance)", cert.NotBefore, before)
	}

	lifetime := cert.NotAfter.Sub(cert.NotBefore)
	tenYears := 10 * 365 * 24 * time.Hour
	// The exact span is 10 years plus the one-hour backdate; allow a small
	// margin for the two time.Now() calls in the template.
	if lifetime < tenYears || lifetime > tenYears+2*time.Hour {
		t.Errorf("validity = %v, want ~%v", lifetime, tenYears)
	}

	keyBlock, _ := pem.Decode(ca.KeyPEM)
	if keyBlock == nil {
		t.Fatal("key PEM does not decode")
	}
	if keyBlock.Type != "EC PRIVATE KEY" {
		t.Errorf("key PEM block type = %q, want %q", keyBlock.Type, "EC PRIVATE KEY")
	}
	if _, err := x509.ParseECPrivateKey(keyBlock.Bytes); err != nil {
		t.Errorf("ParseECPrivateKey: %v", err)
	}
}

func TestSignLeafHosts(t *testing.T) {
	ca, err := NewCA("server-1")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	tests := []struct {
		name    string
		hosts   []string
		wantDNS []string
		wantIPs []string
	}{
		{
			name:    "mixed IPv4, IPv6 and DNS names",
			hosts:   []string{"db.internal", "10.0.0.5", "::1", "postgres", "2001:db8::1"},
			wantDNS: []string{"db.internal", "postgres"},
			wantIPs: []string{"10.0.0.5", "::1", "2001:db8::1"},
		},
		{
			name:    "empty strings are skipped",
			hosts:   []string{"", "db.internal", ""},
			wantDNS: []string{"db.internal"},
			wantIPs: nil,
		},
		{
			name:    "no hosts",
			hosts:   nil,
			wantDNS: nil,
			wantIPs: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leaf, err := SignLeaf(ca, "db-1", tt.hosts)
			if err != nil {
				t.Fatalf("SignLeaf: %v", err)
			}
			cert := parseCertPEM(t, leaf.CertPEM)

			if len(cert.DNSNames) != len(tt.wantDNS) {
				t.Fatalf("DNSNames = %v, want %v", cert.DNSNames, tt.wantDNS)
			}
			for i, want := range tt.wantDNS {
				if cert.DNSNames[i] != want {
					t.Errorf("DNSNames[%d] = %q, want %q", i, cert.DNSNames[i], want)
				}
			}

			if len(cert.IPAddresses) != len(tt.wantIPs) {
				t.Fatalf("IPAddresses = %v, want %v", cert.IPAddresses, tt.wantIPs)
			}
			for i, want := range tt.wantIPs {
				if !cert.IPAddresses[i].Equal(net.ParseIP(want)) {
					t.Errorf("IPAddresses[%d] = %v, want %v", i, cert.IPAddresses[i], want)
				}
			}
		})
	}
}

// TestVerifyChain exercises what a real client does: trust the CA, dial a
// name, and verify the leaf — the failure mode the package exists to prevent.
func TestVerifyChain(t *testing.T) {
	ca, err := NewCA("server-1")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := SignLeaf(ca, "db-1", []string{"db.internal", "10.0.0.5"})
	if err != nil {
		t.Fatalf("SignLeaf: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM) {
		t.Fatal("AppendCertsFromPEM rejected the CA PEM")
	}
	leafCert := parseCertPEM(t, leaf.CertPEM)

	if _, err := leafCert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "db.internal"}); err != nil {
		t.Errorf("Verify with a SAN name: %v, want success", err)
	}
	if _, err := leafCert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "not-in-san.example"}); err == nil {
		t.Error("Verify with a name absent from the SANs succeeded, want failure")
	}
}

func TestParseCAInvalidPEM(t *testing.T) {
	valid, err := NewCA("server-1")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	tests := []struct {
		name string
		ca   *CA
	}{
		{"missing CA", nil},
		{"garbage cert PEM", &CA{CertPEM: []byte("not pem at all"), KeyPEM: valid.KeyPEM}},
		{"garbage key PEM", &CA{CertPEM: valid.CertPEM, KeyPEM: []byte("not pem at all")}},
		{"invalid cert DER", &CA{CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("bad")}), KeyPEM: valid.KeyPEM}},
		{"invalid EC key DER", &CA{CertPEM: valid.CertPEM, KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("bad")})}},
		{"empty material", &CA{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parseCA(tt.ca); err == nil {
				t.Error("parseCA succeeded, want error")
			}
			// SignLeaf must surface the same failure to its caller.
			if _, err := SignLeaf(tt.ca, "db-1", nil); err == nil {
				t.Error("SignLeaf succeeded with invalid CA material, want error")
			}
		})
	}
}

func TestEntropyFailuresAreReported(t *testing.T) {
	old := randomReader
	randomReader = errorReader{}
	t.Cleanup(func() { randomReader = old })

	if _, err := NewCA("server"); err == nil || !strings.Contains(err.Error(), "entropy unavailable") {
		t.Fatalf("NewCA should surface entropy failure, got %v", err)
	}

	// SignLeaf parses the CA before generating its own key, so use material
	// minted before replacing the entropy source.
	randomReader = old
	ca, err := NewCA("server")
	if err != nil {
		t.Fatal(err)
	}
	randomReader = errorReader{}
	if _, err := SignLeaf(ca, "leaf", nil); err == nil || !strings.Contains(err.Error(), "entropy unavailable") {
		t.Fatalf("SignLeaf should surface entropy failure, got %v", err)
	}
}

func TestSerialFailuresAreReported(t *testing.T) {
	old := newSerialNumber
	newSerialNumber = func() (*big.Int, error) {
		return nil, errors.New("serial unavailable")
	}
	t.Cleanup(func() { newSerialNumber = old })

	if _, err := NewCA("server"); err == nil || !strings.Contains(err.Error(), "serial unavailable") {
		t.Fatalf("NewCA should surface serial generation failure, got %v", err)
	}

	newSerialNumber = old
	ca, err := NewCA("server")
	if err != nil {
		t.Fatal(err)
	}
	newSerialNumber = func() (*big.Int, error) {
		return nil, errors.New("serial unavailable")
	}
	if _, err := SignLeaf(ca, "leaf", nil); err == nil || !strings.Contains(err.Error(), "serial unavailable") {
		t.Fatalf("SignLeaf should surface serial generation failure, got %v", err)
	}
}

func TestCertificateSigningFailuresAreReported(t *testing.T) {
	old := createCertificate
	createCertificate = func(io.Reader, *x509.Certificate, *x509.Certificate, any, any) ([]byte, error) {
		return nil, errors.New("signing unavailable")
	}
	t.Cleanup(func() { createCertificate = old })

	if _, err := NewCA("server"); err == nil || !strings.Contains(err.Error(), "signing unavailable") {
		t.Fatalf("NewCA should surface signing failure, got %v", err)
	}

	createCertificate = old
	ca, err := NewCA("server")
	if err != nil {
		t.Fatal(err)
	}
	createCertificate = func(io.Reader, *x509.Certificate, *x509.Certificate, any, any) ([]byte, error) {
		return nil, errors.New("signing unavailable")
	}
	if _, err := SignLeaf(ca, "leaf", nil); err == nil || !strings.Contains(err.Error(), "signing unavailable") {
		t.Fatalf("SignLeaf should surface signing failure, got %v", err)
	}
}
