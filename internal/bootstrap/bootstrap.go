// Package bootstrap implements the first-start seeding of instance-config
// §6.2/§6.3: instance_settings singleton and root user. Every action is
// individually idempotent and detected from database state, never from a
// marker file.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepteams/akerdock/internal/config"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/password"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/sshkey"
	"github.com/deepteams/akerdock/internal/store"
)

// Store is the persistence used to seed the instance root and settings.
type Store interface {
	GetInstanceSettings(context.Context) (store.InstanceSetting, error)
	GetOldestTeamID(context.Context) (int64, error)
	GetInstancePrivateKey(context.Context) (store.PrivateKey, error)
	CreateLocalhostServerIfAbsent(context.Context, store.CreateLocalhostServerIfAbsentParams) (int64, error)
	SetLocalhostSeeded(context.Context) (int64, error)
	CreatePrivateKey(context.Context, store.CreatePrivateKeyParams) (store.PrivateKey, error)
	InsertInstanceSettingsIfAbsent(context.Context, store.InsertInstanceSettingsIfAbsentParams) (int64, error)
	SetAcmeEmailIfAbsent(context.Context, *string) (int64, error)
	CountUsers(context.Context) (int64, error)
	CreateUser(context.Context, store.CreateUserParams) (store.User, error)
	CreatePersonalTeam(context.Context, string) (store.Team, error)
	AddTeamMember(context.Context, store.AddTeamMemberParams) error
}

type bootstrapPool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Run executes the first-start seeding. It is safe to call on every boot.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, keyring *envelope.Keyring, logger *slog.Logger) error {
	return run(ctx, pool, store.New(pool), cfg, keyring, logger)
}

func run(ctx context.Context, pool bootstrapPool, q Store, cfg *config.Config, keyring *envelope.Keyring, logger *slog.Logger) error {
	if err := seedInstanceSettings(ctx, q, cfg, logger); err != nil {
		return err
	}
	if err := ensureInstanceSSHKey(ctx, q, cfg, keyring, logger); err != nil {
		return err
	}
	if err := bootstrapRootUser(ctx, pool, q, cfg, logger); err != nil {
		return err
	}
	return seedLocalhostServer(ctx, q, cfg, logger)
}

// seedLocalhostServer pre-registers the machine hosting the instance as a
// server named "localhost" (PRD §3 parity, instance-config §6.2). It runs
// AFTER the root user bootstrap so the root team exists on the very first
// boot of the reference install.
//
// The seed happens once in the LIFETIME of the instance, not once per boot:
// instance_settings.localhost_seeded records the fact, so an operator who
// deletes the server never finds it resurrected. Until a team exists (UI
// onboarding not done yet), the seed just waits for a later boot.
func seedLocalhostServer(ctx context.Context, q Store, cfg *config.Config, logger *slog.Logger) error {
	settings, err := q.GetInstanceSettings(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: localhost server: %w", err)
	}
	if settings.LocalhostSeeded {
		return nil
	}

	teamID, err := q.GetOldestTeamID(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		// No team yet — nothing to attach the server to (INV-002). Not seeded,
		// so a boot after the UI onboarding will pick it up.
		return nil
	}
	if err != nil {
		return fmt.Errorf("bootstrap: localhost server: %w", err)
	}
	key, err := q.GetInstancePrivateKey(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: localhost server: instance key: %w", err)
	}

	inserted, err := q.CreateLocalhostServerIfAbsent(ctx, store.CreateLocalhostServerIfAbsentParams{
		TeamID:       teamID,
		Host:         cfg.LocalhostHost,
		SshUser:      cfg.LocalhostUser,
		PrivateKeyID: key.ID,
	})
	if err != nil {
		return fmt.Errorf("bootstrap: localhost server: %w", err)
	}
	if _, err := q.SetLocalhostSeeded(ctx); err != nil {
		return fmt.Errorf("bootstrap: localhost server: %w", err)
	}
	if inserted > 0 {
		logger.Info("localhost server pre-registered — authorize the instance public key on this host, then validate it",
			"host", cfg.LocalhostHost, "user", cfg.LocalhostUser)
	} else {
		// A server named "localhost" already existed in that team: the
		// operator's object wins, the seed only records that it ran.
		logger.Info("localhost server not seeded: a server with that name already exists")
	}
	return nil
}

// ensureInstanceSSHKey generates the ed25519 instance key on first boot and
// keeps its public part available on disk for the operator (§6.2).
func ensureInstanceSSHKey(ctx context.Context, q Store, cfg *config.Config, keyring *envelope.Keyring, logger *slog.Logger) error {
	key, err := q.GetInstancePrivateKey(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		material, err := sshkey.GenerateEd25519("akerdock-instance")
		if err != nil {
			return err
		}
		u, err := pguuid.New()
		if err != nil {
			return err
		}
		enc, err := keyring.Encrypt("private_keys", "private_key_enc", pguuid.String(u), []byte(material.PrivatePEM))
		if err != nil {
			return err
		}
		key, err = q.CreatePrivateKey(ctx, store.CreatePrivateKeyParams{
			Uuid:              u,
			TeamID:            nil,
			Name:              "instance",
			Description:       ptr("Instance SSH key, generated at first start (instance-config §6.2)"),
			FingerprintSha256: material.Fingerprint,
			PublicKey:         material.PublicKey,
			PrivateKeyEnc:     enc,
			IsInstance:        true,
		})
		if err != nil {
			return fmt.Errorf("bootstrap: instance ssh key: %w", err)
		}
		logger.Info("instance ssh key generated", "fingerprint", key.FingerprintSha256)
	case err != nil:
		return fmt.Errorf("bootstrap: instance ssh key: %w", err)
	}

	// Idempotent copy of the public key for the operator.
	sshDir := filepath.Join(cfg.DataDir, "ssh")
	pubPath := filepath.Join(sshDir, "instance_ed25519.pub")
	if _, err := os.Stat(pubPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("bootstrap: instance ssh key: inspect public key: %w", err)
		}
		if err := os.MkdirAll(sshDir, 0o700); err != nil {
			return fmt.Errorf("bootstrap: instance ssh key: %w", err)
		}
		if err := os.WriteFile(pubPath, []byte(key.PublicKey+"\n"), 0o644); err != nil {
			return fmt.Errorf("bootstrap: instance ssh key: %w", err)
		}
		logger.Info("instance public key written", "path", pubPath)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func seedInstanceSettings(ctx context.Context, q Store, cfg *config.Config, logger *slog.Logger) error {
	var fqdn *string
	if cfg.InstanceFQDN != "" {
		fqdn = &cfg.InstanceFQDN
	}
	var acmeEmail *string
	if cfg.ACMEEmail != "" {
		acmeEmail = &cfg.ACMEEmail
	}
	inserted, err := q.InsertInstanceSettingsIfAbsent(ctx, store.InsertInstanceSettingsIfAbsentParams{
		Fqdn:      fqdn,
		Timezone:  cfg.Timezone,
		AcmeEmail: acmeEmail,
	})
	if err != nil {
		return fmt.Errorf("bootstrap: instance settings: %w", err)
	}
	if inserted > 0 {
		logger.Info("instance settings created", "timezone", cfg.Timezone)
		return nil
	}

	// The database is authoritative after the first start: the variables
	// never overwrite it, a divergence only warns (§6.2, §7.2).
	settings, err := q.GetInstanceSettings(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: instance settings: %w", err)
	}
	// An instance created before this setting existed has no ACME contact: seed
	// it, without ever overwriting a value the database already holds.
	if cfg.ACMEEmail != "" && settings.AcmeEmail == nil {
		if _, err := q.SetAcmeEmailIfAbsent(ctx, &cfg.ACMEEmail); err != nil {
			return fmt.Errorf("bootstrap: acme email: %w", err)
		}
		logger.Info("acme contact email seeded", "email", cfg.ACMEEmail)
	}
	if cfg.InstanceFQDN != "" && (settings.Fqdn == nil || *settings.Fqdn != cfg.InstanceFQDN) {
		logger.Warn("AKERDOCK_INSTANCE_FQDN diverges from the database value, which is authoritative — update it in the UI or remove the variable")
	}
	if settings.Timezone != cfg.Timezone && cfg.Timezone != config.DefaultTimezone {
		logger.Warn("AKERDOCK_TIMEZONE diverges from the database value, which is authoritative — update it in the UI or remove the variable")
	}
	return nil
}

func bootstrapRootUser(ctx context.Context, pool bootstrapPool, q Store, cfg *config.Config, logger *slog.Logger) error {
	count, err := q.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: count users: %w", err)
	}
	if count > 0 {
		if cfg.HasRootBootstrap() {
			logger.Warn("AKERDOCK_ROOT_* variables are still set while users exist — they are ignored, remove them (runbook install.md step 4)")
		}
		return nil
	}
	if !cfg.HasRootBootstrap() {
		// No variables: the guided UI onboarding creates the root (§6.3).
		return nil
	}

	hash, err := password.Hash(cfg.RootPassword)
	if err != nil {
		return err
	}
	// Guard against a concurrent first boot of another instance: the partial
	// unique index on email makes the second insert fail, which we treat as
	// "already bootstrapped".
	user, err := q.CreateUser(ctx, store.CreateUserParams{
		Email:        cfg.RootEmail,
		Name:         cfg.RootName,
		PasswordHash: &hash,
		IsRoot:       true,
	})
	if err != nil {
		var already int64
		if countErr := pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&already); countErr == nil && already > 0 {
			logger.Info("root user already bootstrapped by another instance")
			return nil
		}
		return fmt.Errorf("bootstrap: create root user: %w", err)
	}
	// A user with no team cannot do anything: every resource hangs off a team
	// (INV-002), and a session must act in one. The root user was created
	// without a personal team, so it could authenticate and then be told it
	// belongs nowhere — an account that exists and cannot be used.
	team, err := q.CreatePersonalTeam(ctx, cfg.RootName)
	if err != nil {
		return fmt.Errorf("bootstrap: personal team: %w", err)
	}
	// The team creator is `admin`, the top team role (ADR-038 — `owner` merged in).
	if err := q.AddTeamMember(ctx, store.AddTeamMemberParams{
		TeamID: team.ID, UserID: user.ID, Role: store.TeamRoleAdmin,
	}); err != nil {
		return fmt.Errorf("bootstrap: team membership: %w", err)
	}

	if user.Uuid.Valid {
		logger.Info("root user created", "uuid", fmt.Sprintf("%x-%x-%x-%x-%x", user.Uuid.Bytes[0:4], user.Uuid.Bytes[4:6], user.Uuid.Bytes[6:8], user.Uuid.Bytes[8:10], user.Uuid.Bytes[10:16]))
		logger.Info("personal team created", "team_id", team.ID)
	}
	return nil
}

// ErrNotBootstrapped is reserved for callers that require a root user.
var ErrNotBootstrapped = errors.New("bootstrap: no user exists yet")
