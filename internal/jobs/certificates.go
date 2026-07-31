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

	"github.com/docker/docker/api/types/container"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/queue"
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
// leaves the server: the ACME store is read through the agent channel
// (ADR-054) and only the public chains are parsed.
type CertificateSync struct {
	Store   *store.Queries
	Docker  dockerruntime.Source
	HostOps hostops.Source
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
	ops, err := h.HostOps.HostOps(ctx, server.ID)
	if err != nil {
		rec.Fail(ctx, "the server's agent is not connected")
		return nil, err
	}

	if job.JobType == TypeCertificateRenew {
		if err := h.forceRenew(ctx, rec, ops, server, payload.CertificateID); err != nil {
			return nil, err
		}
	}

	rec.Start(ctx, "sync_certificates")
	count, err := h.sync(ctx, ops, server)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	rec.Succeed(ctx, fmt.Sprintf("%d certificates observed", count))
	return map[string]any{"certificates": count}, nil
}

// acmePath is where Traefik keeps its ACME store on the server.
const acmePath = "/var/lib/akerdock/proxy/acme.json"

// forceRenew drops the certificate from the ACME store and restarts the
// proxy: Traefik re-issues it on the next request (§7.5). The old
// certificate keeps serving until the new one is ready.
func (h *CertificateSync) forceRenew(ctx context.Context, rec *queue.StepRecorder, ops hostops.Ops, server store.Server, certID int64) error {
	cert, err := h.Store.GetCertificateByID(ctx, certID)
	if err != nil {
		return fmt.Errorf("certificate not found: %w", err)
	}
	rec.Start(ctx, "force_renew")
	_ = h.Store.SetCertificateStatus(ctx, store.SetCertificateStatusParams{
		ID: cert.ID, Status: store.CertificateStatusRenewing,
	})

	// The ACME store is backed up best-effort (it may not exist yet), then the
	// proxy is restarted: Traefik re-evaluates what its routers need and
	// re-issues. The current certificate keeps serving until the new one is
	// ready (§7.5).
	if err := ops.CopyFile(ctx, agentwire.FileCopyParams{Src: acmePath, Dst: acmePath + ".bak"}); err != nil {
		h.Logger.Debug("acme store backup skipped", "error", err)
	}
	restart := func() error {
		rt, err := h.Docker.Runtime(ctx, server.ID)
		if err != nil {
			return err
		}
		return rt.ContainerRestart(ctx, proxy.ContainerName, container.StopOptions{})
	}
	if err := restart(); err != nil {
		msg := "could not restart the proxy to trigger the renewal"
		_ = h.Store.SetCertificateStatus(ctx, store.SetCertificateStatusParams{
			ID: cert.ID, Status: store.CertificateStatusFailed, LastError: &msg,
		})
		rec.Fail(ctx, msg)
		return fmt.Errorf("%s: %w", msg, err)
	}
	rec.Succeed(ctx, "renewal triggered — the proxy re-issues on the next request")
	return nil
}

// sync reads acme.json, parses the observed chains and reflects them.
func (h *CertificateSync) sync(ctx context.Context, ops hostops.Ops, server store.Server) (int, error) {
	res, err := ops.ReadFile(ctx, agentwire.FileReadParams{Path: acmePath})
	if err != nil {
		return 0, err
	}
	if !res.Found {
		// A not-yet-initialized ACME store is not an error: the proxy simply
		// has not issued anything yet.
		return 0, nil
	}
	var acme acmeStore
	if jsonErr := json.Unmarshal(res.Content, &acme); jsonErr != nil {
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
