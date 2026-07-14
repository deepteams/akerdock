package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeEncryptionRotate re-encrypts every row still using an older master
// key version towards the active one (ADR-003, §23.2).
const TypeEncryptionRotate = "encryption.rotate"

// rotateBatch bounds each pass: the rewrite happens in batches, never as
// one blocking update (§19.2).
const rotateBatch = 200

// EncryptionRotate rewrites the *_enc columns of the data dictionary §12
// inventory. Idempotent: rows already on the active version are skipped, so
// a crashed rotation simply resumes.
type EncryptionRotate struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
}

// Execute rotates every encrypted column towards the active key version.
func (h *EncryptionRotate) Execute(ctx context.Context, _ store.Job, rec *queue.StepRecorder) (any, error) {
	active := int32(h.Keyring.ActiveVersion())
	total := 0

	rec.Start(ctx, "private_keys")
	n, err := h.rotatePrivateKeys(ctx, active)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	total += n
	rec.Succeed(ctx, fmt.Sprintf("%d rows re-encrypted", n))

	rec.Start(ctx, "environment_variables")
	n, err = h.rotateEnvVars(ctx, active)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	total += n
	rec.Succeed(ctx, fmt.Sprintf("%d rows re-encrypted", n))

	rec.Start(ctx, "database_credentials")
	n, err = h.rotateDatabaseCredentials(ctx, active)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	total += n
	rec.Succeed(ctx, fmt.Sprintf("%d rows re-encrypted", n))

	rec.Start(ctx, "s3_storages")
	n, err = h.rotateS3Storages(ctx, active)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	total += n
	rec.Succeed(ctx, fmt.Sprintf("%d rows re-encrypted", n))

	rec.Start(ctx, "notification_channels")
	n, err = h.rotateNotificationChannels(ctx, active)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	total += n
	rec.Succeed(ctx, fmt.Sprintf("%d rows re-encrypted", n))

	rec.Start(ctx, "webhook_endpoints")
	n, err = h.rotateWebhookEndpoints(ctx, active)
	if err != nil {
		rec.Fail(ctx, err.Error())
		return nil, err
	}
	total += n
	rec.Succeed(ctx, fmt.Sprintf("%d rows re-encrypted", n))

	h.Logger.Info("encryption rotation complete", "active_version", active, "rows", total)
	return map[string]any{"active_key_version": active, "rows_reencrypted": total}, nil
}

// rewrite decrypts with whichever version the row carries and re-encrypts
// with the active one, preserving the AAD binding (table, column, row uuid).
func (h *EncryptionRotate) rewrite(table, column, rowUUID string, ciphertext []byte) ([]byte, error) {
	plaintext, err := h.Keyring.Decrypt(table, column, rowUUID, ciphertext)
	if err != nil {
		return nil, err
	}
	return h.Keyring.Encrypt(table, column, rowUUID, plaintext)
}

func (h *EncryptionRotate) rotatePrivateKeys(ctx context.Context, active int32) (int, error) {
	rotated := 0
	for {
		rows, err := h.Store.ListPrivateKeysToRotate(ctx, store.ListPrivateKeysToRotateParams{
			ActiveVersion: active, Limit: rotateBatch,
		})
		if err != nil || len(rows) == 0 {
			return rotated, err
		}
		for _, row := range rows {
			enc, err := h.rewrite("private_keys", "private_key_enc", pguuid.String(row.Uuid), row.PrivateKeyEnc)
			if err != nil {
				return rotated, fmt.Errorf("private_keys id=%d: %w", row.ID, err)
			}
			if err := h.Store.RotatePrivateKeyEnc(ctx, store.RotatePrivateKeyEncParams{ID: row.ID, PrivateKeyEnc: enc}); err != nil {
				return rotated, err
			}
			rotated++
		}
	}
}

func (h *EncryptionRotate) rotateEnvVars(ctx context.Context, active int32) (int, error) {
	rotated := 0
	for {
		rows, err := h.Store.ListEnvVarsToRotate(ctx, store.ListEnvVarsToRotateParams{
			ActiveVersion: active, Limit: rotateBatch,
		})
		if err != nil || len(rows) == 0 {
			return rotated, err
		}
		for _, row := range rows {
			enc, err := h.rewrite("environment_variables", "value_enc", pguuid.String(row.Uuid), row.ValueEnc)
			if err != nil {
				return rotated, fmt.Errorf("environment_variables id=%d: %w", row.ID, err)
			}
			if err := h.Store.RotateEnvVarEnc(ctx, store.RotateEnvVarEncParams{ID: row.ID, ValueEnc: enc}); err != nil {
				return rotated, err
			}
			rotated++
		}
	}
}

func (h *EncryptionRotate) rotateDatabaseCredentials(ctx context.Context, active int32) (int, error) {
	rotated := 0
	for {
		rows, err := h.Store.ListDatabaseCredentialsToRotate(ctx, store.ListDatabaseCredentialsToRotateParams{
			ActiveVersion: active, Limit: rotateBatch,
		})
		if err != nil || len(rows) == 0 {
			return rotated, err
		}
		for _, row := range rows {
			enc, err := h.rewrite("database_credentials", "password_enc", pguuid.String(row.Uuid), row.PasswordEnc)
			if err != nil {
				return rotated, fmt.Errorf("database_credentials id=%d: %w", row.ID, err)
			}
			if err := h.Store.RotateDatabaseCredentialEnc(ctx, store.RotateDatabaseCredentialEncParams{ID: row.ID, PasswordEnc: enc}); err != nil {
				return rotated, err
			}
			rotated++
		}
	}
}

// rotateS3Storages rewrites both credential columns of a storage in one pass:
// they share a row, so re-encrypting them separately would leave the row
// half-rotated if the job died in between.
func (h *EncryptionRotate) rotateS3Storages(ctx context.Context, active int32) (int, error) {
	rotated := 0
	for {
		rows, err := h.Store.ListS3StoragesToRotate(ctx, store.ListS3StoragesToRotateParams{
			ActiveVersion: active, Limit: rotateBatch,
		})
		if err != nil || len(rows) == 0 {
			return rotated, err
		}
		for _, row := range rows {
			uuid := pguuid.String(row.Uuid)
			access, err := h.rewrite("s3_storages", "access_key_enc", uuid, row.AccessKeyEnc)
			if err != nil {
				return rotated, fmt.Errorf("s3_storages id=%d: %w", row.ID, err)
			}
			secret, err := h.rewrite("s3_storages", "secret_key_enc", uuid, row.SecretKeyEnc)
			if err != nil {
				return rotated, fmt.Errorf("s3_storages id=%d: %w", row.ID, err)
			}
			if err := h.Store.RotateS3StorageEnc(ctx, store.RotateS3StorageEncParams{
				ID: row.ID, AccessKeyEnc: access, SecretKeyEnc: secret,
			}); err != nil {
				return rotated, err
			}
			rotated++
		}
	}
}

// rotateNotificationChannels rewrites the encrypted configuration blobs
// (webhook URLs, bot tokens, SMTP credentials).
func (h *EncryptionRotate) rotateNotificationChannels(ctx context.Context, active int32) (int, error) {
	rotated := 0
	for {
		rows, err := h.Store.ListNotificationChannelsToRotate(ctx, store.ListNotificationChannelsToRotateParams{
			ActiveVersion: active, Limit: rotateBatch,
		})
		if err != nil || len(rows) == 0 {
			return rotated, err
		}
		for _, row := range rows {
			enc, err := h.rewrite("notification_channels", "config_enc", pguuid.String(row.Uuid), row.ConfigEnc)
			if err != nil {
				return rotated, fmt.Errorf("notification_channels id=%d: %w", row.ID, err)
			}
			if err := h.Store.RotateNotificationChannelEnc(ctx, store.RotateNotificationChannelEncParams{
				ID: row.ID, ConfigEnc: enc,
			}); err != nil {
				return rotated, err
			}
			rotated++
		}
	}
}

// rotateWebhookEndpoints rewrites the HMAC secrets of the incoming Git
// webhooks. A secret left on an old key version would make every future
// delivery fail its signature check.
func (h *EncryptionRotate) rotateWebhookEndpoints(ctx context.Context, active int32) (int, error) {
	rotated := 0
	for {
		rows, err := h.Store.ListWebhookEndpointsToRotate(ctx, store.ListWebhookEndpointsToRotateParams{
			ActiveVersion: active, Limit: rotateBatch,
		})
		if err != nil || len(rows) == 0 {
			return rotated, err
		}
		for _, row := range rows {
			enc, err := h.rewrite("webhook_endpoints", "secret_enc", pguuid.String(row.Uuid), row.SecretEnc)
			if err != nil {
				return rotated, fmt.Errorf("webhook_endpoints id=%d: %w", row.ID, err)
			}
			if err := h.Store.RotateWebhookEndpointEnc(ctx, store.RotateWebhookEndpointEncParams{
				ID: row.ID, SecretEnc: enc,
			}); err != nil {
				return rotated, err
			}
			rotated++
		}
	}
}
