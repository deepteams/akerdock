package store_test

// The edge relay file is rebuilt whole from ListServerRelayFQDNs (ADR-077):
// a model domain (00105) missing from that UNION would answer on its origin
// but never traverse the relay — the LAN-only GPU server case it exists for.
// Like the list filters, the guarantee IS the SQL, so this runs the
// statements against real rows.

import (
	"context"
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

func TestServerFQDNQueriesSeeModelDomains(t *testing.T) {
	pool := testDB(t)
	q := store.New(pool)
	teamID := testTeam(t, pool)
	ctx := context.Background()
	name := "mdl-" + randomHex(4)

	t.Cleanup(func() {
		for _, stmt := range []string{
			"DELETE FROM resources WHERE team_id = $1",
			"DELETE FROM destinations WHERE server_id IN (SELECT id FROM servers WHERE team_id = $1)",
			"DELETE FROM servers WHERE team_id = $1",
			"DELETE FROM private_keys WHERE team_id = $1",
			"DELETE FROM environments WHERE project_id IN (SELECT id FROM projects WHERE team_id = $1)",
			"DELETE FROM projects WHERE team_id = $1",
		} {
			if _, err := pool.Exec(context.Background(), stmt, teamID); err != nil {
				t.Errorf("cleaning the model fixture (%s): %v", stmt, err)
			}
		}
	})

	project, err := q.CreateProject(ctx, store.CreateProjectParams{TeamID: teamID, Name: name, Slug: name})
	if err != nil {
		t.Fatalf("creating project: %v", err)
	}
	env, err := q.CreateEnvironment(ctx, store.CreateEnvironmentParams{ProjectID: project.ID, Name: "production", Slug: "production"})
	if err != nil {
		t.Fatalf("creating environment: %v", err)
	}
	key, err := q.CreatePrivateKey(ctx, store.CreatePrivateKeyParams{
		Uuid: mustUUID(t), TeamID: &teamID, Name: name + "-key",
		FingerprintSha256: "SHA256:" + randomHex(16), PublicKey: "ssh-ed25519 AAAA", PrivateKeyEnc: fakeEnvelope(),
	})
	if err != nil {
		t.Fatalf("creating private key: %v", err)
	}
	server, err := q.CreateServer(ctx, store.CreateServerParams{
		TeamID: teamID, Name: name + "-srv", Host: name + ".test", Port: 22,
		SshUser: "root", SshTimeoutSeconds: 10, PrivateKeyID: key.ID,
		ProxyType: store.ProxyTypeTraefik, ProxyHttpPort: 80, ProxyHttpsPort: 443,
	})
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	dest, err := q.CreateDestination(ctx, store.CreateDestinationParams{
		Uuid: mustUUID(t), ServerID: server.ID, Name: "default", Network: "akerdock", IsDefault: true,
	})
	if err != nil {
		t.Fatalf("creating destination: %v", err)
	}
	res, err := q.CreateResource(ctx, store.CreateResourceParams{
		Uuid: mustUUID(t), TeamID: teamID, EnvironmentID: env.ID, DestinationID: dest.ID,
		ResourceType: store.ResourceTypeModel, Name: name + "-model",
	})
	if err != nil {
		t.Fatalf("creating model resource: %v", err)
	}
	if err := q.CreateModelRow(ctx, store.CreateModelRowParams{
		ID: res.ID, Engine: store.InferenceEngineVllm, ModelID: "org/m",
		TensorParallelSize: 1, EngineFlags: []byte("[]"), ApiKeyEnc: fakeEnvelope(),
		PublishedPort: 18001, ServerID: server.ID,
	}); err != nil {
		t.Fatalf("creating model row: %v", err)
	}
	fqdn := name + ".example.com"
	if _, err := q.CreateModelDomain(ctx, store.CreateModelDomainParams{
		Uuid: mustUUID(t), ModelID: &res.ID, Fqdn: fqdn, Path: "/",
	}); err != nil {
		t.Fatalf("creating model domain: %v", err)
	}

	relay, err := q.ListServerRelayFQDNs(ctx, server.ID)
	if err != nil {
		t.Fatalf("relay fqdns: %v", err)
	}
	if len(relay) != 1 || relay[0] != fqdn {
		t.Fatalf("the relay must carry the model domain, got %v", relay)
	}
	rows, err := q.ListServerDomains(ctx, server.ID)
	if err != nil {
		t.Fatalf("server domains: %v", err)
	}
	if len(rows) != 1 || rows[0].Fqdn != fqdn || rows[0].ResourceType != store.ResourceTypeModel {
		t.Fatalf("the server's domain list must name the model, got %+v", rows)
	}

	// A deleted model routes nothing: the relay derives from live placements.
	if _, err := q.SoftDeleteResource(ctx, res.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	relay, err = q.ListServerRelayFQDNs(ctx, server.ID)
	if err != nil {
		t.Fatalf("relay fqdns after delete: %v", err)
	}
	if len(relay) != 0 {
		t.Fatalf("a soft-deleted model must leave the relay, got %v", relay)
	}
}
