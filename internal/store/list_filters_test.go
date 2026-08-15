package store_test

// GET /applications and GET /databases advertise project/environment/server
// filters, and the dashboard's environment page relies on them for the data
// segmentation between projects: a filter the statement ignores returns the
// team-wide list, and every environment shows every other project's resources.
// Like the attach claims (attach_claim_test.go), the guarantee IS the WHERE
// clause, so these tests run the statements against real rows and assert on
// what comes back.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

func mustUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	u, err := pguuid.New()
	if err != nil {
		t.Fatalf("generating a uuid: %v", err)
	}
	return u
}

// fakeEnvelope is a placeholder ciphertext whose first 4 bytes carry the
// big-endian key version the encryption inventory reads (migration 00093);
// anything shorter makes its get_byte calls error on every scan of the column.
func fakeEnvelope() []byte { return []byte{0, 0, 0, 1, 'e', 'n', 'c'} }

// listFixture is one full project → environment → server chain holding one
// application and one database, so each filter has a distinct value to hit.
type listFixture struct {
	projectUUID     pgtype.UUID
	environmentUUID pgtype.UUID
	serverUUID      pgtype.UUID
	appUUID         pgtype.UUID
	dbUUID          pgtype.UUID
	appID           int64
	serverID        int64
}

func seedListFixture(t *testing.T, pool *pgxpool.Pool, q *store.Queries, teamID int64, name string) listFixture {
	t.Helper()
	ctx := context.Background()

	// testTeam's deletion cannot cascade through projects and private_keys (ON
	// DELETE RESTRICT), so the whole chain is dropped first. Registered before
	// the inserts and keyed on team_id: t.Cleanup is LIFO, so this runs before
	// the team's own deletion, and once per fixture is harmlessly idempotent.
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
				t.Errorf("cleaning the list fixture (%s): %v", stmt, err)
			}
		}
	})

	project, err := q.CreateProject(ctx, store.CreateProjectParams{TeamID: teamID, Name: name, Slug: name})
	if err != nil {
		t.Fatalf("creating project %s: %v", name, err)
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
		ProxyType: store.ProxyTypeNone, ProxyHttpPort: 80, ProxyHttpsPort: 443,
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

	appRes, err := q.CreateResource(ctx, store.CreateResourceParams{
		Uuid: mustUUID(t), TeamID: teamID, EnvironmentID: env.ID, DestinationID: dest.ID,
		ResourceType: store.ResourceTypeApplication, Name: name + "-app",
	})
	if err != nil {
		t.Fatalf("creating application resource: %v", err)
	}
	if err := q.CreateApplicationRow(ctx, store.CreateApplicationRowParams{ID: appRes.ID, BaseDirectory: "/"}); err != nil {
		t.Fatalf("creating application row: %v", err)
	}
	image := "nginx"
	if err := q.CreateBuildConfig(ctx, store.CreateBuildConfigParams{
		ApplicationID: appRes.ID, BuildPack: store.BuildPackImage, ImageName: &image,
	}); err != nil {
		t.Fatalf("creating build config: %v", err)
	}
	if err := q.CreateRuntimeConfig(ctx, store.CreateRuntimeConfigParams{ApplicationID: appRes.ID}); err != nil {
		t.Fatalf("creating runtime config: %v", err)
	}

	dbRes, err := q.CreateResource(ctx, store.CreateResourceParams{
		Uuid: mustUUID(t), TeamID: teamID, EnvironmentID: env.ID, DestinationID: dest.ID,
		ResourceType: store.ResourceTypeDatabase, Name: name + "-db",
	})
	if err != nil {
		t.Fatalf("creating database resource: %v", err)
	}
	if err := q.CreateDatabaseRow(ctx, store.CreateDatabaseRowParams{
		ID: dbRes.ID, Engine: store.DbEnginePostgresql, ServerID: server.ID,
	}); err != nil {
		t.Fatalf("creating database row: %v", err)
	}
	if _, err := q.CreateDatabaseCredential(ctx, store.CreateDatabaseCredentialParams{
		Uuid: mustUUID(t), DatabaseID: dbRes.ID, Username: "akerdock", PasswordEnc: fakeEnvelope(),
	}); err != nil {
		t.Fatalf("creating database credential: %v", err)
	}

	return listFixture{
		projectUUID: project.Uuid, environmentUUID: env.Uuid, serverUUID: server.Uuid,
		appUUID: appRes.Uuid, dbUUID: dbRes.Uuid,
		appID: appRes.ID, serverID: server.ID,
	}
}

// assertUUIDs fails unless got is exactly want, order-insensitively.
func assertUUIDs(t *testing.T, label string, got, want []pgtype.UUID) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d rows, want %d", label, len(got), len(want))
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g.Bytes == w.Bytes {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s: expected uuid %x missing from result", label, w.Bytes)
		}
	}
}

func TestListApplicationsPageHonorsTheContractFilters(t *testing.T) {
	pool := testDB(t)
	q := store.New(pool)
	teamID := testTeam(t, pool)
	a := seedListFixture(t, pool, q, teamID, "seg-a-"+randomHex(4))
	b := seedListFixture(t, pool, q, teamID, "seg-b-"+randomHex(4))

	list := func(t *testing.T, params store.ListApplicationsPageParams) []pgtype.UUID {
		t.Helper()
		params.TeamID = teamID
		params.PageLimit = 10
		rows, err := q.ListApplicationsPage(context.Background(), params)
		if err != nil {
			t.Fatalf("ListApplicationsPage: %v", err)
		}
		uuids := make([]pgtype.UUID, 0, len(rows))
		for _, row := range rows {
			uuids = append(uuids, row.Resource.Uuid)
		}
		return uuids
	}

	t.Run("no filter returns the whole team", func(t *testing.T) {
		assertUUIDs(t, "unfiltered", list(t, store.ListApplicationsPageParams{}),
			[]pgtype.UUID{a.appUUID, b.appUUID})
	})
	t.Run("environment filter", func(t *testing.T) {
		assertUUIDs(t, "environment a",
			list(t, store.ListApplicationsPageParams{EnvironmentUuid: a.environmentUUID}),
			[]pgtype.UUID{a.appUUID})
	})
	t.Run("project filter", func(t *testing.T) {
		assertUUIDs(t, "project b",
			list(t, store.ListApplicationsPageParams{ProjectUuid: b.projectUUID}),
			[]pgtype.UUID{b.appUUID})
	})
	t.Run("server filter", func(t *testing.T) {
		assertUUIDs(t, "server a",
			list(t, store.ListApplicationsPageParams{ServerUuid: a.serverUUID}),
			[]pgtype.UUID{a.appUUID})
	})
}

func TestListDatabasesPageHonorsTheContractFilters(t *testing.T) {
	pool := testDB(t)
	q := store.New(pool)
	teamID := testTeam(t, pool)
	a := seedListFixture(t, pool, q, teamID, "seg-c-"+randomHex(4))
	b := seedListFixture(t, pool, q, teamID, "seg-d-"+randomHex(4))

	list := func(t *testing.T, params store.ListDatabasesPageParams) []pgtype.UUID {
		t.Helper()
		params.TeamID = teamID
		params.PageLimit = 10
		rows, err := q.ListDatabasesPage(context.Background(), params)
		if err != nil {
			t.Fatalf("ListDatabasesPage: %v", err)
		}
		uuids := make([]pgtype.UUID, 0, len(rows))
		for _, row := range rows {
			uuids = append(uuids, row.Resource.Uuid)
		}
		return uuids
	}

	t.Run("no filter returns the whole team", func(t *testing.T) {
		assertUUIDs(t, "unfiltered", list(t, store.ListDatabasesPageParams{}),
			[]pgtype.UUID{a.dbUUID, b.dbUUID})
	})
	t.Run("environment filter", func(t *testing.T) {
		assertUUIDs(t, "environment a",
			list(t, store.ListDatabasesPageParams{EnvironmentUuid: a.environmentUUID}),
			[]pgtype.UUID{a.dbUUID})
	})
	t.Run("server filter", func(t *testing.T) {
		assertUUIDs(t, "server b",
			list(t, store.ListDatabasesPageParams{ServerUuid: b.serverUUID}),
			[]pgtype.UUID{b.dbUUID})
	})
}

func TestListDeploymentsForResourceHonorsTheStatusFilter(t *testing.T) {
	pool := testDB(t)
	q := store.New(pool)
	teamID := testTeam(t, pool)
	fx := seedListFixture(t, pool, q, teamID, "seg-e-"+randomHex(4))
	ctx := context.Background()

	mint := func(status store.DeploymentStatus) pgtype.UUID {
		t.Helper()
		d, err := q.CreateDeployment(ctx, store.CreateDeploymentParams{
			Uuid: mustUUID(t), ResourceID: fx.appID, Trigger: store.DeploymentTriggerManual,
			ServerID: fx.serverID, ConfigSnapshot: []byte("{}"),
		})
		if err != nil {
			t.Fatalf("creating deployment: %v", err)
		}
		if err := q.SetDeploymentStatus(ctx, store.SetDeploymentStatusParams{ID: d.ID, Status: status}); err != nil {
			t.Fatalf("setting deployment status: %v", err)
		}
		return d.Uuid
	}
	failed := mint(store.DeploymentStatusFailed)
	succeeded := mint(store.DeploymentStatusSucceeded)

	list := func(status *store.DeploymentStatus) []pgtype.UUID {
		t.Helper()
		rows, err := q.ListDeploymentsForResource(ctx, store.ListDeploymentsForResourceParams{
			ResourceID: fx.appID, Status: status, PageLimit: 10,
		})
		if err != nil {
			t.Fatalf("ListDeploymentsForResource: %v", err)
		}
		uuids := make([]pgtype.UUID, 0, len(rows))
		for _, row := range rows {
			uuids = append(uuids, row.Deployment.Uuid)
		}
		return uuids
	}

	assertUUIDs(t, "unfiltered", list(nil), []pgtype.UUID{failed, succeeded})
	only := store.DeploymentStatusFailed
	assertUUIDs(t, "failed only", list(&only), []pgtype.UUID{failed})
}
