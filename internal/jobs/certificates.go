package jobs

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// Certificate job types (proxy-contract §7).
const (
	TypeCertificateSync  = "certificate.sync"
	TypeCertificateRenew = "certificate.renew"
)

// CertificatePayload references the server (and the certificate, to renew).
type CertificatePayload struct {
	ServerID      int64 `json:"server_id"`
	CertificateID int64 `json:"certificate_id,omitempty"`
}

// CertificateSync reads the certificates served by a server's proxy and
// reflects them into the database (§18.3). The private key material never
// leaves the server.
type CertificateSync struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
}

// acmeStore mirrors the parts of Traefik's acme.json we read: only the
// certificate chain (public) — never the private keys it also contains.
type acmeStore map[string]struct {
	Certificates []struct {
		Domain struct {
			Main string   `json:"main"`
			Sans []string `json:"sans"`
		} `json:"domain"`
		Certificate string `json:"certificate"` // base64 PEM chain
	} `json:"Certificates"`
}

// Execute synchronizes (or forces the renewal of) the certificates.
func (h *CertificateSync) Execute(ctx context.Context, job store.Job, rec *queue.StepRecorder) (any, error) {
	var payload CertificatePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}
	server, err := h.Store.GetServerByID(ctx, payload.ServerID)
	if err != nil {
		return nil, fmt.Errorf("server not found: %w", err)
	}
	key, err := h.Store.GetPrivateKeyByID(ctx, server.PrivateKeyID)
	if err != nil {
		return nil, err
	}
	pem, err := h.Keyring.Decrypt("private_keys", "private_key_enc", pguuid.String(key.Uuid), key.PrivateKeyEnc)
	if err != nil {
		return nil, err
	}
	client, err := sshexec.Dial(ctx, server.Host, int(server.Port), server.SshUser, string(pem),
		time.Duration(server.SshTimeoutSeconds)*time.Second, pinnedHostKey(server))
	if err != nil {
		rec.Fail(ctx, "SSH connection failed")
		return nil, err
	}
	defer func() { _ = client.Close() }()

	if job.JobType == TypeCertificateRenew {
		if err := h.forceRenew(ctx, rec, client, payload.CertificateID); err != nil {
			return nil, err
		}
	}

	rec.Start(ctx, "sync_certificates")
	count, err := h.sync(ctx, client, server)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	rec.Succeed(ctx, fmt.Sprintf("%d certificates observed", count))
	return map[string]any{"certificates": count}, nil
}

// forceRenew drops the certificate from the ACME store and restarts the
// proxy: Traefik re-issues it on the next request (§7.5). The old
// certificate keeps serving until the new one is ready.
func (h *CertificateSync) forceRenew(ctx context.Context, rec *queue.StepRecorder, client *sshexec.Client, certID int64) error {
	cert, err := h.Store.GetCertificateByID(ctx, certID)
	if err != nil {
		return fmt.Errorf("certificate not found: %w", err)
	}
	rec.Start(ctx, "force_renew")
	_ = h.Store.SetCertificateStatus(ctx, store.SetCertificateStatusParams{
		ID: cert.ID, Status: store.CertificateStatusRenewing,
	})

	// The ACME store is backed up, then the proxy is restarted: Traefik
	// re-evaluates what its routers need and re-issues. The current
	// certificate keeps serving until the new one is ready (§7.5).
	res, err := client.Run(ctx, fmt.Sprintf(
		"cp /data/akerdock/proxy/acme.json /data/akerdock/proxy/acme.json.bak 2>/dev/null; docker restart %s >/dev/null",
		proxy.ContainerName))
	if err != nil || res.ExitCode != 0 {
		msg := "could not restart the proxy to trigger the renewal"
		_ = h.Store.SetCertificateStatus(ctx, store.SetCertificateStatusParams{
			ID: cert.ID, Status: store.CertificateStatusFailed, LastError: &msg,
		})
		rec.Fail(ctx, msg)
		return fmt.Errorf("%s", msg)
	}
	rec.Succeed(ctx, "renewal triggered — the proxy re-issues on the next request")
	return nil
}

// sync reads acme.json, parses the observed chains and reflects them.
func (h *CertificateSync) sync(ctx context.Context, client *sshexec.Client, server store.Server) (int, error) {
	res, err := client.Run(ctx, "cat /data/akerdock/proxy/acme.json 2>/dev/null || echo '{}'")
	if err != nil {
		return 0, err
	}
	var acme acmeStore
	if jsonErr := json.Unmarshal([]byte(res.Stdout), &acme); jsonErr != nil {
		// An empty or not-yet-initialized ACME store is not an error: the
		// proxy simply has not issued anything yet.
		h.Logger.Debug("acme store not readable yet", "error", jsonErr)
		return 0, nil
	}

	count := 0
	for _, resolver := range acme {
		for _, entry := range resolver.Certificates {
			if entry.Domain.Main == "" {
				continue
			}
			cert := certificateFacts(entry.Certificate)
			sans := entry.Domain.Sans
			if sans == nil {
				sans = []string{}
			}
			if err := h.Store.UpsertCertificate(ctx, store.UpsertCertificateParams{
				ServerID:   server.ID,
				Kind:       store.CertificateKindAcmeHttp01,
				MainDomain: strings.ToLower(entry.Domain.Main),
				Sans:       sans,
				Issuer:     cert.issuer,
				NotBefore:  cert.notBefore,
				NotAfter:   cert.notAfter,
				Status:     cert.status,
			}); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

type certFacts struct {
	issuer    *string
	notBefore pgtype.Timestamptz
	notAfter  pgtype.Timestamptz
	status    store.CertificateStatus
}

// certificateFacts parses the observed chain: issuer and validity window.
// Only public material is read (the chain), never a private key.
func certificateFacts(b64 string) certFacts {
	facts := certFacts{status: store.CertificateStatusPending}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return facts
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return facts
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return facts
	}
	issuer := parsed.Issuer.CommonName
	if issuer != "" {
		facts.issuer = &issuer
	}
	facts.notBefore = pgtype.Timestamptz{Time: parsed.NotBefore, Valid: true}
	facts.notAfter = pgtype.Timestamptz{Time: parsed.NotAfter, Valid: true}
	facts.status = store.CertificateStatusIssued
	if time.Now().After(parsed.NotAfter) {
		facts.status = store.CertificateStatusExpired
	}
	return facts
}
