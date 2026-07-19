package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/config"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/store"
)

type fakeBootstrapStore struct {
	settings       store.InstanceSetting
	settingsErr    error
	settingsCalls  int
	inserted       int64
	insertErr      error
	acmeRows       int64
	acmeErr        error
	acme           *string
	teamID         int64
	teamIDErr      error
	privateKey     store.PrivateKey
	privateKeyErr  error
	createKeyArg   store.CreatePrivateKeyParams
	createKeyErr   error
	localhostRows  int64
	localhostErr   error
	localhostArg   store.CreateLocalhostServerIfAbsentParams
	seedRows       int64
	seedErr        error
	userCount      int64
	countErr       error
	user           store.User
	createUserErr  error
	createUserArg  store.CreateUserParams
	team           store.Team
	createTeamErr  error
	createTeamName string
	memberErr      error
	memberArg      store.AddTeamMemberParams
}

func (f *fakeBootstrapStore) GetInstanceSettings(context.Context) (store.InstanceSetting, error) {
	f.settingsCalls++
	return f.settings, f.settingsErr
}
func (f *fakeBootstrapStore) GetOldestTeamID(context.Context) (int64, error) {
	return f.teamID, f.teamIDErr
}
func (f *fakeBootstrapStore) GetInstancePrivateKey(context.Context) (store.PrivateKey, error) {
	return f.privateKey, f.privateKeyErr
}
func (f *fakeBootstrapStore) CreateLocalhostServerIfAbsent(_ context.Context, arg store.CreateLocalhostServerIfAbsentParams) (int64, error) {
	f.localhostArg = arg
	return f.localhostRows, f.localhostErr
}
func (f *fakeBootstrapStore) SetLocalhostSeeded(context.Context) (int64, error) {
	return f.seedRows, f.seedErr
}
func (f *fakeBootstrapStore) CreatePrivateKey(_ context.Context, arg store.CreatePrivateKeyParams) (store.PrivateKey, error) {
	f.createKeyArg = arg
	if f.privateKey.ID == 0 {
		f.privateKey = store.PrivateKey{ID: 5, PublicKey: arg.PublicKey, FingerprintSha256: arg.FingerprintSha256}
	}
	return f.privateKey, f.createKeyErr
}
func (f *fakeBootstrapStore) InsertInstanceSettingsIfAbsent(_ context.Context, arg store.InsertInstanceSettingsIfAbsentParams) (int64, error) {
	return f.inserted, f.insertErr
}
func (f *fakeBootstrapStore) SetAcmeEmailIfAbsent(_ context.Context, email *string) (int64, error) {
	f.acme = email
	return f.acmeRows, f.acmeErr
}
func (f *fakeBootstrapStore) CountUsers(context.Context) (int64, error) {
	return f.userCount, f.countErr
}
func (f *fakeBootstrapStore) CreateUser(_ context.Context, arg store.CreateUserParams) (store.User, error) {
	f.createUserArg = arg
	return f.user, f.createUserErr
}
func (f *fakeBootstrapStore) CreatePersonalTeam(_ context.Context, name string) (store.Team, error) {
	f.createTeamName = name
	return f.team, f.createTeamErr
}
func (f *fakeBootstrapStore) AddTeamMember(_ context.Context, arg store.AddTeamMemberParams) error {
	f.memberArg = arg
	return f.memberErr
}

type fakeBootstrapPool struct {
	count int64
	err   error
}

func (f fakeBootstrapPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeCountRow{count: f.count, err: f.err}
}

type fakeCountRow struct {
	count int64
	err   error
}

func (r fakeCountRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*int64)) = r.count
	return nil
}

func bootstrapLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func bootstrapKeyring(t *testing.T) *envelope.Keyring {
	t.Helper()
	line := "1:" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

func TestSeedInstanceSettings(t *testing.T) {
	t.Run("created", func(t *testing.T) {
		database := &fakeBootstrapStore{inserted: 1}
		cfg := &config.Config{InstanceFQDN: "dock.example.test", Timezone: "UTC", ACMEEmail: "ops@example.test"}
		if err := seedInstanceSettings(context.Background(), database, cfg, bootstrapLogger()); err != nil {
			t.Fatal(err)
		}
		if database.settingsCalls != 0 {
			t.Fatal("created settings were needlessly read back")
		}
	})
	t.Run("existing seeds only missing ACME", func(t *testing.T) {
		existingFQDN := "database.example.test"
		database := &fakeBootstrapStore{settings: store.InstanceSetting{
			Fqdn: &existingFQDN, Timezone: "Europe/Paris",
		}}
		cfg := &config.Config{InstanceFQDN: "env.example.test", Timezone: "America/Toronto", ACMEEmail: "ops@example.test"}
		if err := seedInstanceSettings(context.Background(), database, cfg, bootstrapLogger()); err != nil {
			t.Fatal(err)
		}
		if database.acme == nil || *database.acme != cfg.ACMEEmail {
			t.Fatalf("seeded ACME = %v", database.acme)
		}
	})
	t.Run("errors", func(t *testing.T) {
		errBoom := errors.New("boom")
		cases := []*fakeBootstrapStore{
			{insertErr: errBoom},
			{settingsErr: errBoom},
			{settings: store.InstanceSetting{}, acmeErr: errBoom},
		}
		configs := []*config.Config{
			{Timezone: "UTC"},
			{Timezone: "UTC"},
			{Timezone: "UTC", ACMEEmail: "ops@example.test"},
		}
		for i, database := range cases {
			if err := seedInstanceSettings(context.Background(), database, configs[i], bootstrapLogger()); err == nil {
				t.Errorf("case %d: error was hidden", i)
			}
		}
	})
}

func TestEnsureInstanceSSHKeyExistingAndGenerated(t *testing.T) {
	t.Run("existing writes public key once", func(t *testing.T) {
		database := &fakeBootstrapStore{privateKey: store.PrivateKey{
			ID: 1, PublicKey: "ssh-ed25519 existing",
		}}
		cfg := &config.Config{DataDir: t.TempDir()}
		if err := ensureInstanceSSHKey(context.Background(), database, cfg, bootstrapKeyring(t), bootstrapLogger()); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(cfg.DataDir, "ssh", "instance_ed25519.pub")
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != "ssh-ed25519 existing\n" {
			t.Fatalf("public key = %q, %v", raw, err)
		}
		if err := os.WriteFile(path, []byte("operator copy\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ensureInstanceSSHKey(context.Background(), database, cfg, bootstrapKeyring(t), bootstrapLogger()); err != nil {
			t.Fatal(err)
		}
		raw, _ = os.ReadFile(path)
		if string(raw) != "operator copy\n" {
			t.Fatalf("existing operator copy was overwritten: %q", raw)
		}
	})
	t.Run("generates encrypted key", func(t *testing.T) {
		database := &fakeBootstrapStore{privateKeyErr: pgx.ErrNoRows}
		cfg := &config.Config{DataDir: t.TempDir()}
		keyring := bootstrapKeyring(t)
		if err := ensureInstanceSSHKey(context.Background(), database, cfg, keyring, bootstrapLogger()); err != nil {
			t.Fatal(err)
		}
		if !database.createKeyArg.Uuid.Valid || !database.createKeyArg.IsInstance ||
			len(database.createKeyArg.PrivateKeyEnc) == 0 {
			t.Fatalf("create key args = %#v", database.createKeyArg)
		}
		plain, err := keyring.Decrypt("private_keys", "private_key_enc",
			uuidString(database.createKeyArg.Uuid), database.createKeyArg.PrivateKeyEnc)
		if err != nil || !strings.Contains(string(plain), "PRIVATE KEY") {
			t.Fatalf("encrypted private key = %v, %v", string(plain), err)
		}
	})
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return strings.ToLower(strings.Join([]string{
		hex(u.Bytes[0:4]), hex(u.Bytes[4:6]), hex(u.Bytes[6:8]), hex(u.Bytes[8:10]), hex(u.Bytes[10:16]),
	}, "-"))
}

func hex(raw []byte) string { return fmt.Sprintf("%x", raw) }

func TestEnsureInstanceSSHKeyErrors(t *testing.T) {
	errBoom := errors.New("boom")
	cases := []*fakeBootstrapStore{
		{privateKeyErr: errBoom},
		{privateKeyErr: pgx.ErrNoRows, createKeyErr: errBoom},
	}
	for i, database := range cases {
		if err := ensureInstanceSSHKey(context.Background(), database,
			&config.Config{DataDir: t.TempDir()}, bootstrapKeyring(t), bootstrapLogger()); err == nil {
			t.Errorf("case %d: error was hidden", i)
		}
	}
	dataFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dataFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureInstanceSSHKey(context.Background(), &fakeBootstrapStore{
		privateKey: store.PrivateKey{ID: 1, PublicKey: "ssh-ed25519 x"},
	}, &config.Config{DataDir: dataFile}, bootstrapKeyring(t), bootstrapLogger()); err == nil {
		t.Fatal("directory creation failure was hidden")
	}
}

func TestSeedLocalhostServer(t *testing.T) {
	cfg := &config.Config{LocalhostHost: "127.0.0.1", LocalhostUser: "deploy"}
	t.Run("already seeded", func(t *testing.T) {
		database := &fakeBootstrapStore{settings: store.InstanceSetting{LocalhostSeeded: true}}
		if err := seedLocalhostServer(context.Background(), database, cfg, bootstrapLogger()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("waits for team", func(t *testing.T) {
		database := &fakeBootstrapStore{teamIDErr: pgx.ErrNoRows}
		if err := seedLocalhostServer(context.Background(), database, cfg, bootstrapLogger()); err != nil {
			t.Fatal(err)
		}
	})
	for _, inserted := range []int64{0, 1} {
		database := &fakeBootstrapStore{
			teamID: 4, privateKey: store.PrivateKey{ID: 5}, localhostRows: inserted,
		}
		if err := seedLocalhostServer(context.Background(), database, cfg, bootstrapLogger()); err != nil {
			t.Fatal(err)
		}
		if database.localhostArg.TeamID != 4 || database.localhostArg.PrivateKeyID != 5 ||
			database.localhostArg.Host != cfg.LocalhostHost {
			t.Fatalf("localhost args = %#v", database.localhostArg)
		}
	}
}

func TestSeedLocalhostServerErrors(t *testing.T) {
	errBoom := errors.New("boom")
	cases := []*fakeBootstrapStore{
		{settingsErr: errBoom},
		{teamIDErr: errBoom},
		{teamID: 1, privateKeyErr: errBoom},
		{teamID: 1, privateKey: store.PrivateKey{ID: 2}, localhostErr: errBoom},
		{teamID: 1, privateKey: store.PrivateKey{ID: 2}, seedErr: errBoom},
	}
	for i, database := range cases {
		if err := seedLocalhostServer(context.Background(), database, &config.Config{}, bootstrapLogger()); err == nil {
			t.Errorf("case %d: error was hidden", i)
		}
	}
}

func rootConfig() *config.Config {
	return &config.Config{
		RootEmail: "root@example.test", RootName: "Root", RootPassword: "long-enough-test-password",
	}
}

func TestBootstrapRootUser(t *testing.T) {
	t.Run("existing and guided onboarding", func(t *testing.T) {
		for _, tc := range []struct {
			database *fakeBootstrapStore
			cfg      *config.Config
		}{
			{&fakeBootstrapStore{userCount: 1}, rootConfig()},
			{&fakeBootstrapStore{}, &config.Config{}},
		} {
			if err := bootstrapRootUser(context.Background(), fakeBootstrapPool{}, tc.database, tc.cfg, bootstrapLogger()); err != nil {
				t.Fatal(err)
			}
			if tc.database.createUserArg.Email != "" {
				t.Fatal("root user was unexpectedly created")
			}
		}
	})
	t.Run("creates usable root", func(t *testing.T) {
		database := &fakeBootstrapStore{
			user: store.User{ID: 10, Uuid: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}},
			team: store.Team{ID: 20},
		}
		cfg := rootConfig()
		if err := bootstrapRootUser(context.Background(), fakeBootstrapPool{}, database, cfg, bootstrapLogger()); err != nil {
			t.Fatal(err)
		}
		if database.createUserArg.PasswordHash == nil || !database.createUserArg.IsRoot ||
			database.createTeamName != cfg.RootName ||
			database.memberArg.TeamID != 20 || database.memberArg.UserID != 10 ||
			database.memberArg.Role != store.TeamRoleOwner {
			t.Fatalf("user=%#v team=%q member=%#v", database.createUserArg, database.createTeamName, database.memberArg)
		}
	})
	t.Run("concurrent creator wins", func(t *testing.T) {
		database := &fakeBootstrapStore{createUserErr: errors.New("duplicate")}
		if err := bootstrapRootUser(context.Background(), fakeBootstrapPool{count: 1},
			database, rootConfig(), bootstrapLogger()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBootstrapRootUserErrors(t *testing.T) {
	errBoom := errors.New("boom")
	cases := []struct {
		database *fakeBootstrapStore
		pool     fakeBootstrapPool
	}{
		{&fakeBootstrapStore{countErr: errBoom}, fakeBootstrapPool{}},
		{&fakeBootstrapStore{createUserErr: errBoom}, fakeBootstrapPool{err: errBoom}},
		{&fakeBootstrapStore{user: store.User{ID: 1}, createTeamErr: errBoom}, fakeBootstrapPool{}},
		{&fakeBootstrapStore{user: store.User{ID: 1}, team: store.Team{ID: 2}, memberErr: errBoom}, fakeBootstrapPool{}},
	}
	for i, tc := range cases {
		if err := bootstrapRootUser(context.Background(), tc.pool, tc.database, rootConfig(), bootstrapLogger()); err == nil {
			t.Errorf("case %d: error was hidden", i)
		}
	}
}

func TestRunStopsAtEachStageAndCompletes(t *testing.T) {
	errBoom := errors.New("boom")
	cases := []*fakeBootstrapStore{
		{insertErr: errBoom},
		{inserted: 1, privateKeyErr: errBoom},
		{inserted: 1, privateKey: store.PrivateKey{ID: 1, PublicKey: "x"}, countErr: errBoom},
		{inserted: 1, privateKey: store.PrivateKey{ID: 1, PublicKey: "x"}, userCount: 1, settingsErr: errBoom},
	}
	for i, database := range cases {
		cfg := &config.Config{DataDir: t.TempDir()}
		if err := run(context.Background(), fakeBootstrapPool{}, database, cfg, bootstrapKeyring(t), bootstrapLogger()); err == nil {
			t.Errorf("case %d: stage error was hidden", i)
		}
	}

	database := &fakeBootstrapStore{
		inserted: 1, privateKey: store.PrivateKey{ID: 1, PublicKey: "x"},
		userCount: 1, settings: store.InstanceSetting{LocalhostSeeded: true},
	}
	if err := run(context.Background(), fakeBootstrapPool{}, database,
		&config.Config{DataDir: t.TempDir()}, bootstrapKeyring(t), bootstrapLogger()); err != nil {
		t.Fatal(err)
	}
}

func TestPtrAndSentinel(t *testing.T) {
	if got := ptr("x"); got == nil || *got != "x" {
		t.Fatal("ptr failed")
	}
	if ErrNotBootstrapped == nil {
		t.Fatal("missing sentinel")
	}
}
