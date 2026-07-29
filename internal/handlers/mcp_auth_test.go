package handlers

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

type mcpTokenStore struct {
	row       store.GetActiveApiTokensByPrefixRow
	authority *store.GetTokenCreatorAuthorityRow
}

func (s *mcpTokenStore) GetActiveApiTokensByPrefix(context.Context, string) ([]store.GetActiveApiTokensByPrefixRow, error) {
	return []store.GetActiveApiTokensByPrefixRow{s.row}, nil
}

func (s *mcpTokenStore) TouchApiTokenLastUsed(context.Context, int64) error { return nil }

func (s *mcpTokenStore) GetTokenCreatorAuthority(context.Context, store.GetTokenCreatorAuthorityParams) (store.GetTokenCreatorAuthorityRow, error) {
	if s.authority == nil {
		return store.GetTokenCreatorAuthorityRow{}, pgx.ErrNoRows
	}
	return *s.authority, nil
}

func mcpAPITokenFixture(t *testing.T) (string, store.GetActiveApiTokensByPrefixRow) {
	t.Helper()
	token, prefix, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	creator := int64(7)
	return token, store.GetActiveApiTokensByPrefixRow{
		ID:          1,
		Uuid:        pguuid.MustParse("11111111-1111-4111-8111-111111111111"),
		TeamID:      42,
		TeamUuid:    pguuid.MustParse("22222222-2222-4222-8222-222222222222"),
		CreatedBy:   &creator,
		TokenPrefix: prefix,
		TokenHash:   hash,
		Permissions: []string{string(auth.PermRead)},
		LastUsedAt:  pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
}

func mcpAuthMiddleware(s auth.TokenStore) *auth.Middleware {
	return &auth.Middleware{
		Store:  s,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestMcpAPITokenUsesCentralIPAllowlist(t *testing.T) {
	token, row := mcpAPITokenFixture(t)
	row.IpAllowlist = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	api := &API{TokenAuth: mcpAuthMiddleware(&mcpTokenStore{row: row})}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.RemoteAddr = "192.0.2.10:1234"

	if identity, ok := api.mcpAPIToken(req, token); ok || identity != nil {
		t.Fatal("MCP accepted an API token outside its IP allowlist")
	}
}

func TestMcpAPITokenIsBoundToCurrentCreatorAuthority(t *testing.T) {
	token, row := mcpAPITokenFixture(t)
	creator := *row.CreatedBy

	t.Run("demotion narrows MCP tools", func(t *testing.T) {
		api := &API{TokenAuth: mcpAuthMiddleware(&mcpTokenStore{
			row: row,
			authority: &store.GetTokenCreatorAuthorityRow{
				UserID: creator,
				Role:   store.TeamRoleReviewer,
			},
		})}
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		identity, ok := api.mcpAPIToken(req, token)
		if !ok {
			t.Fatal("reviewer token should remain authenticated for its permitted reads")
		}
		if auth.Has(identity.Permissions, auth.PermServersRead) ||
			!auth.Has(identity.Permissions, auth.PermPreviewsRead) {
			t.Fatalf("demoted creator permissions = %v", identity.Permissions)
		}
	})

	t.Run("removed creator invalidates MCP access", func(t *testing.T) {
		api := &API{TokenAuth: mcpAuthMiddleware(&mcpTokenStore{row: row})}
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if identity, ok := api.mcpAPIToken(req, token); ok || identity != nil {
			t.Fatal("MCP accepted a token whose creator left the team")
		}
	})
}

func TestMcpOAuthIdentityUsesCurrentGranularRole(t *testing.T) {
	base := store.GetMcpAccessTokenByHashRow{
		UserID: 11, TeamID: 42, ClientName: "assistant",
	}

	reviewer := base
	reviewer.Role = store.TeamRoleReviewer
	reviewerIdentity := mcpOAuthIdentity(reviewer)
	if !auth.Has(reviewerIdentity.Permissions, auth.PermPreviewsRead) ||
		auth.Has(reviewerIdentity.Permissions, auth.PermServersRead) {
		t.Fatalf("reviewer MCP permissions = %v", reviewerIdentity.Permissions)
	}

	custom := base
	custom.Role = store.TeamRoleMember
	customRoleID := int64(99)
	custom.CustomRoleID = &customRoleID
	custom.CustomPermissions = []string{string(auth.PermApplicationsRead)}
	customIdentity := mcpOAuthIdentity(custom)
	if !auth.Has(customIdentity.Permissions, auth.PermApplicationsRead) ||
		auth.Has(customIdentity.Permissions, auth.PermDatabasesRead) {
		t.Fatalf("custom-role MCP permissions = %v", customIdentity.Permissions)
	}

	emptyCustom := base
	emptyCustom.Role = store.TeamRoleMember
	emptyCustom.CustomRoleID = &customRoleID
	if permissions := mcpOAuthIdentity(emptyCustom).Permissions; len(permissions) != 0 {
		t.Fatalf("empty custom role fell back to member permissions: %v", permissions)
	}
}
