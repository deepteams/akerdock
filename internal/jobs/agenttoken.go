// Agent enrollment (ADR-040 phase 1): the server helper authenticates its
// outbound observations with a per-server token, injected as environment when
// the control plane (re)creates the helper container over SSH. The plaintext
// survives under envelope encryption so the idempotent ensure command
// re-injects the SAME token on every pass — no rotation churn, no pairing
// flow, no shared secret across servers.
package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// agentTokenScheme prefixes agent tokens ("akda_"), next to the API ("akd_")
// and port-forward ("akdp_") schemes — greppable, never ambiguous in logs.
const agentTokenScheme = "akda_"

// AgentEnrollmentStore is the slice of the store the enrollment needs — an
// interface so the scheduler (whose Store is already an interface) and the
// deploy jobs (concrete *store.Queries) both qualify.
type AgentEnrollmentStore interface {
	GetInstanceSettings(context.Context) (store.InstanceSetting, error)
	GetAgentTokenByServerID(context.Context, int64) (store.AgentToken, error)
	CreateAgentToken(context.Context, store.CreateAgentTokenParams) (store.AgentToken, error)
}

// EnsureAgentToken returns the server's agent token plaintext, creating it on
// first use. Only the SHA-256 hash authenticates ingestion; the ciphertext
// exists solely so provisioning can re-inject the same value idempotently.
func EnsureAgentToken(ctx context.Context, q AgentEnrollmentStore, keyring *envelope.Keyring, serverID int64) (string, error) {
	if row, err := q.GetAgentTokenByServerID(ctx, serverID); err == nil {
		plain, err := keyring.Decrypt("agent_tokens", "token_enc", pguuid.String(row.Uuid), row.TokenEnc)
		if err != nil {
			return "", fmt.Errorf("agent token decrypt: %w", err)
		}
		return string(plain), nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	plain := agentTokenScheme + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plain))
	u, err := pguuid.New()
	if err != nil {
		return "", err
	}
	enc, err := keyring.Encrypt("agent_tokens", "token_enc", pguuid.String(u), []byte(plain))
	if err != nil {
		return "", err
	}
	if _, err := q.CreateAgentToken(ctx, store.CreateAgentTokenParams{
		Uuid: u, ServerID: serverID, TokenHash: hex.EncodeToString(sum[:]), TokenEnc: enc,
	}); err != nil {
		return "", err
	}
	return plain, nil
}

// AgentInstanceURL is the control-plane base URL the server helper pushes its
// observations to (ADR-040): the Docker host gateway for the localhost server
// (no DNS, no public hairpin), the instance FQDN elsewhere. Empty when no
// FQDN is configured and the server is remote — the agent then stays silent
// and everything degrades to the SSH scans.
func AgentInstanceURL(ctx context.Context, q AgentEnrollmentStore, server store.Server, controlPlanePort int) string {
	if server.IsLocalhost && controlPlanePort > 0 {
		return fmt.Sprintf("http://host.docker.internal:%d", controlPlanePort)
	}
	if st, err := q.GetInstanceSettings(ctx); err == nil && st.Fqdn != nil && *st.Fqdn != "" {
		return "https://" + *st.Fqdn
	}
	return ""
}
