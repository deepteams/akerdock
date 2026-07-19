// Package pki mints the per-server certificate authority that signs the TLS
// certificates of the managed databases (§6.3).
//
// One CA per server, not one per instance: a server is the blast radius. An
// instance-wide CA that leaked would let anyone impersonate every database on
// every server the instance manages.
//
// The CA's private key never leaves the control plane — it is envelope
// encrypted at rest and used only here, to sign. What goes to the server is a
// leaf certificate and its key; what goes to the client is the CA certificate,
// which is public by nature: it is what lets the client VERIFY. A TLS the client
// does not verify protects against nothing.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"
)

var randomReader io.Reader = rand.Reader
var newSerialNumber = serialNumber
var createCertificate = x509.CreateCertificate

// caLifetime outlives any leaf it signs, by a wide margin: a CA that expires
// under its own certificates would take every database down at once, and the
// only symptom would be clients failing to verify.
const caLifetime = 10 * 365 * 24 * time.Hour

// leafLifetime is deliberately long. A database certificate is renewed by
// recreating the database, which is a restart — so a short lifetime would trade
// a real outage against a theoretical exposure.
const leafLifetime = 5 * 365 * 24 * time.Hour

// CA is a certificate authority: the PEM of its certificate, and the PEM of its
// private key. The key is what the caller encrypts before storing.
type CA struct {
	CertPEM []byte
	KeyPEM  []byte
}

// NewCA mints a certificate authority for one server.
func NewCA(serverName string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), randomReader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerialNumber()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "AkerDock CA — " + serverName},
		NotBefore:             time.Now().Add(-time.Hour), // clock skew between the instance and the server
		NotAfter:              time.Now().Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0, // it signs leaves, never another CA
		MaxPathLenZero:        true,
	}
	der, err := createCertificate(randomReader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &CA{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// Leaf is a signed server certificate and its key, both PEM.
type Leaf struct {
	CertPEM []byte
	KeyPEM  []byte
}

// SignLeaf issues a server certificate for a database.
//
// `hosts` are every name and address a client may legitimately use to reach it:
// the container name on the Docker network, and the server's public host when
// the database is exposed. A certificate that omits the name the client dialled
// fails verification — and the operator, told only "certificate verify failed",
// has no way to guess which name was missing.
func SignLeaf(ca *CA, commonName string, hosts []string) (*Leaf, error) {
	caCert, caKey, err := parseCA(ca)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), randomReader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerialNumber()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, h)
	}
	der, err := createCertificate(randomReader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &Leaf{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func parseCA(ca *CA) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if ca == nil {
		return nil, nil, fmt.Errorf("pki: the CA material is missing")
	}
	certBlock, _ := pem.Decode(ca.CertPEM)
	keyBlock, _ := pem.Decode(ca.KeyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, fmt.Errorf("pki: the CA material is not valid PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func serialNumber() (*big.Int, error) {
	// 128 bits of randomness: serials must be unpredictable, not sequential —
	// a guessable serial is one of the ingredients of a certificate forgery.
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(randomReader, limit)
}
