package jobs

// Coverage tests for encryptionrotate.go: the rotation walks every *_enc
// inventory table, so each table gets a success pass, a decrypt failure and
// an update failure.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// servercovRotateTables describes the six rotated inventories: the list
// query, the update statement, and the AAD-bound blob(s) of one row.
var servercovRotateTables = []struct {
	list    string
	update  string
	table   string
	columns []string
	encIdx  []int // indices of the enc columns in the list row
}{
	{"ListPrivateKeysToRotate", "RotatePrivateKeyEnc", "private_keys", []string{"private_key_enc"}, []int{2}},
	{"ListEnvVarsToRotate", "RotateEnvVarEnc", "environment_variables", []string{"value_enc"}, []int{2}},
	{"ListDatabaseCredentialsToRotate", "RotateDatabaseCredentialEnc", "database_credentials", []string{"password_enc"}, []int{2}},
	{"ListS3StoragesToRotate", "RotateS3StorageEnc", "s3_storages", []string{"access_key_enc", "secret_key_enc"}, []int{2, 3}},
	{"ListNotificationChannelsToRotate", "RotateNotificationChannelEnc", "notification_channels", []string{"config_enc"}, []int{2}},
	{"ListWebhookEndpointsToRotate", "RotateWebhookEndpointEnc", "webhook_endpoints", []string{"secret_enc"}, []int{2}},
}

func TestServercovEncryptionRotateRewritesEveryInventory(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	for _, tc := range servercovRotateTables {
		overrides := map[int]func(any){}
		for i, col := range tc.columns {
			overrides[tc.encIdx[i]] = servercovBytes(servercovEncrypt(t, keyring, tc.table, col, "plain-"+col))
		}
		db.queryFor[tc.list] = [][]func([]any){{servercovFill(overrides)}}
	}
	job := store.Job{ID: 10, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
	result, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err != nil {
		t.Fatal(err)
	}
	payload := result.(map[string]any)
	if payload["rows_reencrypted"] != 6 {
		t.Fatalf("rotation result = %#v", result)
	}
}

func TestServercovEncryptionRotateReportsDecryptFailurePerInventory(t *testing.T) {
	for _, tc := range servercovRotateTables {
		t.Run(tc.list, func(t *testing.T) {
			q, keyring, _, logger, db := servercovDeps(t)
			overrides := map[int]func(any){}
			// Every enc column but the LAST decrypts: the failure lands on the
			// final rewrite of the row (covers the second column of s3).
			for i, col := range tc.columns {
				if i == len(tc.columns)-1 {
					overrides[tc.encIdx[i]] = servercovBytes([]byte("not-a-ciphertext"))
					continue
				}
				overrides[tc.encIdx[i]] = servercovBytes(servercovEncrypt(t, keyring, tc.table, col, "ok"))
			}
			db.queryFor[tc.list] = [][]func([]any){{servercovFill(overrides)}}
			job := store.Job{ID: 11, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
			_, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
				Execute(context.Background(), job, queue.NewStepRecorder(q, job))
			if err == nil || !strings.Contains(err.Error(), tc.table) {
				t.Fatalf("error = %v, want the failing table named", err)
			}
		})
	}
}

func TestServercovEncryptionRotateReportsUpdateFailurePerInventory(t *testing.T) {
	for _, tc := range servercovRotateTables {
		t.Run(tc.update, func(t *testing.T) {
			q, keyring, _, logger, db := servercovDeps(t)
			overrides := map[int]func(any){}
			for i, col := range tc.columns {
				overrides[tc.encIdx[i]] = servercovBytes(servercovEncrypt(t, keyring, tc.table, col, "ok"))
			}
			db.queryFor[tc.list] = [][]func([]any){{servercovFill(overrides)}}
			db.execErr[tc.update] = errors.New("update refused")
			job := store.Job{ID: 12, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
			_, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
				Execute(context.Background(), job, queue.NewStepRecorder(q, job))
			if err == nil || !strings.Contains(err.Error(), "update refused") {
				t.Fatalf("error = %v, want the update failure surfaced", err)
			}
		})
	}
}

func TestServercovEncryptionRotateReportsS3AccessKeyFailure(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	db.queryFor["ListS3StoragesToRotate"] = [][]func([]any){{servercovFill(map[int]func(any){
		2: servercovBytes([]byte("not-a-ciphertext")),
		3: servercovBytes(servercovEncrypt(t, keyring, "s3_storages", "secret_key_enc", "ok")),
	})}}
	job := store.Job{ID: 14, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
	_, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err == nil || !strings.Contains(err.Error(), "s3_storages") {
		t.Fatalf("error = %v", err)
	}
}

func TestServercovEncryptionRotateReportsListFailure(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	db.queryErr["ListPrivateKeysToRotate"] = errors.New("list unavailable")
	job := store.Job{ID: 13, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
	_, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err == nil || !strings.Contains(err.Error(), "list unavailable") {
		t.Fatalf("error = %v", err)
	}
}
