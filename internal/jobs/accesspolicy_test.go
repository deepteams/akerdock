package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

func TestResourceAccessPolicyBasicAuth(t *testing.T) {
	keyring, err := envelope.Parse([]byte("1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"))
	if err != nil {
		t.Fatal(err)
	}
	var uuid pgtype.UUID
	if err := uuid.Scan("0b6f7f3a-1111-2222-3333-444455556666"); err != nil {
		t.Fatal(err)
	}
	encrypted, err := keyring.Encrypt("applications", "access_basic_auth_enc",
		pguuid.String(uuid), []byte("hook-user:secret-password"))
	if err != nil {
		t.Fatal(err)
	}
	app := store.GetApplicationByIDRow{
		Resource: store.Resource{Uuid: uuid},
		Application: store.Application{
			AccessProtection:   store.PreviewProtectionBasicAuth,
			AccessBasicAuthEnc: encrypted,
		},
	}

	policy, err := resourceAccessPolicy(context.Background(), nil, keyring, app, nil, store.Server{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if policy == nil || policy.Mode != "basic_auth" {
		t.Fatalf("policy = %+v", policy)
	}
	user, hash, ok := strings.Cut(policy.BasicAuthHash, ":")
	if !ok || user != "hook-user" {
		t.Fatalf("basic auth material = %q", policy.BasicAuthHash)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("secret-password")); err != nil {
		t.Fatalf("proxy hash does not match password: %v", err)
	}
}

func TestResourceAccessPolicyFailsClosedWithoutCredentials(t *testing.T) {
	app := store.GetApplicationByIDRow{
		Application: store.Application{AccessProtection: store.PreviewProtectionBasicAuth},
	}
	policy, err := resourceAccessPolicy(
		context.Background(), nil, nil, app, nil, store.Server{}, 0,
	)
	if err == nil || policy != nil {
		t.Fatalf("policy=%+v err=%v, want a closed failure", policy, err)
	}
}
