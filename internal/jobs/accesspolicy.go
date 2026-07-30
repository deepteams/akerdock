package jobs

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/store"
)

// resourceAccessPolicy resolves the application/inline-stack wall into the
// proxy IR. Any unavailable protection material is an error: rendering a
// claimed protected resource as public is never an acceptable fallback.
func resourceAccessPolicy(
	ctx context.Context,
	q *store.Queries,
	keyring *envelope.Keyring,
	app store.GetApplicationByIDRow,
	service *store.Service,
	server store.Server,
	controlPlanePort int,
) (*proxy.AccessPolicy, error) {
	protection := app.Application.AccessProtection
	encrypted := app.Application.AccessBasicAuthEnc
	table := "applications"
	if service != nil {
		protection = service.AccessProtection
		encrypted = service.AccessBasicAuthEnc
		table = "services"
	}
	switch protection {
	case "", store.PreviewProtectionNone:
		return nil, nil
	case store.PreviewProtectionBasicAuth:
		if len(encrypted) == 0 {
			return nil, fmt.Errorf("access_protection basic_auth has no configured credentials")
		}
		plaintext, err := keyring.Decrypt(table, "access_basic_auth_enc",
			pguuid.String(app.Resource.Uuid), encrypted)
		if err != nil {
			return nil, fmt.Errorf("decrypt access credentials: %w", err)
		}
		user, password, ok := strings.Cut(string(plaintext), ":")
		if !ok || user == "" || password == "" {
			return nil, fmt.Errorf("access credentials are not valid user:password")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash access credentials: %w", err)
		}
		return &proxy.AccessPolicy{
			Mode: "basic_auth", BasicAuthHash: user + ":" + string(hash),
		}, nil
	case store.PreviewProtectionSso:
		settings, err := q.GetInstanceSettings(ctx)
		if err != nil {
			return nil, err
		}
		if settings.Fqdn == nil || *settings.Fqdn == "" {
			return nil, fmt.Errorf("access_protection sso requires the instance FQDN — set it in the instance settings")
		}
		baseURL := "https://" + *settings.Fqdn
		if server.IsLocalhost && controlPlanePort > 0 {
			baseURL = fmt.Sprintf("http://host.docker.internal:%d", controlPlanePort)
		}
		resourceUUID := pguuid.String(app.Resource.Uuid)
		return &proxy.AccessPolicy{
			Mode:           "sso",
			ForwardAuthURL: baseURL + "/webhooks/applications/forward-auth?resource=" + resourceUUID,
			CallbackURL:    baseURL,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported access_protection %q", protection)
	}
}
