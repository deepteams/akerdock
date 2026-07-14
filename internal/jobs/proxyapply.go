package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

// verifyTimeout bounds the wait for the proxy to load a new configuration
// (proxy-contract §6.3: poll every second, at most 30 s).
const verifyTimeout = 30 * time.Second

// ProxyApplier implements the atomic apply + verify + rollback contract of
// proxy-contract §6.2–§6.4, shared by the deployment engine and the
// configuration-change job. Every apply is a checksummed revision.
type ProxyApplier struct {
	Store  *store.Queries
	Client *sshexec.Client
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

// upload writes (or removes) the dynamic file atomically: tmp + mv on the
// same filesystem, so the proxy never sees a partial file (§6.2).
func (p *ProxyApplier) upload(ctx context.Context, appUUID, content string) error {
	path := "/data/akerdock/proxy/dynamic/" + appUUID + ".yaml"
	var res *sshexec.Result
	var err error
	if content == "" {
		res, err = p.Client.Run(ctx, "rm -f "+path)
	} else {
		res, err = p.Client.RunInput(ctx, fmt.Sprintf(
			"docker network connect %s %s >/dev/null 2>&1 || true; umask 077 && cat > %s.tmp && mv -f %s.tmp %s",
			p.Network, proxy.ContainerName, path, path, path), content)
	}
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("writing the routing file exited with code %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// verify polls the proxy's local API until the expected routers/services
// appear — or disappear, when the routing was removed (§6.3, §6.5).
func (p *ProxyApplier) verify(ctx context.Context, appUUID, content, expectEndpoint string) error {
	removal := content == ""
	deadline := time.Now().Add(verifyTimeout)
	var last string
	for time.Now().Before(deadline) {
		res, err := p.Client.Run(ctx, fmt.Sprintf(
			"docker exec %s wget -qO- http://127.0.0.1:8080/api/http/services", proxy.ContainerName))
		if err != nil {
			return err
		}
		if res.ExitCode == 0 {
			last = res.Stdout
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
