package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/store"
)

// verifyTimeout bounds the wait for the proxy to load a new configuration
// (proxy-contract §6.3: poll every second, at most 30 s).
var verifyTimeout = 30 * time.Second

// ProxyApplier implements the atomic apply + verify + rollback contract of
// proxy-contract §6.2–§6.4, shared by the deployment engine and the
// configuration-change job. Every apply is a checksummed revision. It runs
// entirely on the agent channel (ADR-052/054): the routing file rides the
// file primitives, the verification polls the proxy's API with a typed exec.
type ProxyApplier struct {
	Store  *store.Queries
	Docker dockerruntime.Runtime
	Host   hostops.Ops
	Server store.Server
	// Network is the destination network the proxy must be attached to.
	Network string
}

// Apply writes the routing file for one application, verifies that the
// proxy really loaded it, and rolls back to the last applied revision when
// verification fails. An empty content removes the routing (§6.5).
// expectEndpoint, when set, is the service URL the proxy must expose
// before the apply is considered successful.
func (p *ProxyApplier) Apply(ctx context.Context, appUUID, content, expectEndpoint string) error {
	scope := appUUID
	sum := sha256.Sum256([]byte(content))

	rev, err := p.Store.CreateProxyRevision(ctx, store.CreateProxyRevisionParams{
		ServerID: p.Server.ID, Scope: scope, ProxyType: p.Server.ProxyType,
		ChecksumSha256: hex.EncodeToString(sum[:]), Content: content,
	})
	if err != nil {
		return fmt.Errorf("proxy revision: %w", err)
	}

	applyErr := p.upload(ctx, appUUID, content)
	if applyErr == nil {
		applyErr = p.verify(ctx, appUUID, content, expectEndpoint)
	}
	if applyErr == nil {
		return p.Store.MarkProxyRevisionApplied(ctx, rev.ID)
	}

	// Verification failed: re-apply the last applied revision of the same
	// scope; the old container never stopped serving (INV-005, C2 §6.4.3).
	msg := applyErr.Error()
	_ = p.Store.MarkProxyRevisionFailed(ctx, store.MarkProxyRevisionFailedParams{ID: rev.ID, Error: &msg})

	previous, err := p.Store.GetLastAppliedProxyRevision(ctx, store.GetLastAppliedProxyRevisionParams{
		ServerID: p.Server.ID, Scope: scope,
	})
	if err != nil {
		// Nothing applied before: remove the faulty file rather than leave
		// a broken routing in place.
		_ = p.upload(ctx, appUUID, "")
		return fmt.Errorf("proxy apply failed and no previous revision exists: %w", applyErr)
	}
	if rollbackErr := p.upload(ctx, appUUID, previous.Content); rollbackErr != nil {
		// §6.4.4: never delete the last known-good file; surface the anomaly.
		return fmt.Errorf("proxy apply failed (%v) AND rollback failed (%v) — the server has a routing anomaly", applyErr, rollbackErr)
	}
	_ = p.Store.MarkProxyRevisionRolledBack(ctx, rev.ID)
	return fmt.Errorf("proxy apply failed, routing rolled back to revision %d: %w", previous.Revision, applyErr)
}

// upload writes (or removes) the dynamic file atomically, so the proxy never
// sees a partial file (§6.2). The proxy is attached to the destination
// network best-effort first — already-connected is the steady state.
func (p *ProxyApplier) upload(ctx context.Context, appUUID, content string) error {
	path := "/var/lib/akerdock/proxy/dynamic/" + appUUID + ".yaml"
	if content == "" {
		return p.Host.Remove(ctx, agentwire.FileRemoveParams{Path: path})
	}
	// Best-effort, as the shell's `|| true` was: "already connected" answers
	// vary by engine version, and the verification below is the real gate — a
	// proxy off the network never exposes the endpoint.
	_ = p.Docker.NetworkConnect(ctx, p.Network, proxy.ContainerName, nil)
	return p.Host.WriteFile(ctx, agentwire.FileWriteParams{
		Path: path, Content: []byte(content), Mode: 0o600, Atomic: true,
	})
}

// verify polls the proxy's local API until the expected routers/services
// appear — or disappear, when the routing was removed (§6.3, §6.5). The poll
// is a typed exec in the proxy container; a non-zero exit (the proxy still
// starting) keeps polling, exactly as the shell loop did.
func (p *ProxyApplier) verify(ctx context.Context, appUUID, content, expectEndpoint string) error {
	removal := content == ""
	deadline := time.Now().Add(verifyTimeout)
	var last string
	for time.Now().Before(deadline) {
		out, exit, err := execCapture(ctx, p.Docker, proxy.ContainerName,
			[]string{"wget", "-qO-", "http://127.0.0.1:8080/api/http/services"})
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && exit == 0 {
			last = out
			present := strings.Contains(last, appUUID)
			switch {
			case removal && !present:
				return nil
			case !removal && present && (expectEndpoint == "" || strings.Contains(last, expectEndpoint)):
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if removal {
		return fmt.Errorf("the proxy still exposes the routing of %s after %s", appUUID, verifyTimeout)
	}
	if expectEndpoint != "" {
		return fmt.Errorf("the proxy did not expose the endpoint %s within %s", expectEndpoint, verifyTimeout)
	}
	return fmt.Errorf("the proxy did not load the routing of %s within %s", appUUID, verifyTimeout)
}
