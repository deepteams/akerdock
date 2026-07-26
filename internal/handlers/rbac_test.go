package handlers

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
)

// The RBAC matrix is generated from the CONTRACT, not from a hand-kept list.
// Every operation declares `x-required-permission`; this test walks the spec
// and checks that the implementation actually enforces what it advertises.
//
// A hand-written list would drift: a new endpoint added without an auth check
// would also be forgotten in the list, and the gap would test as green. Driving
// the test from the spec means a new operation is covered the moment it exists.
//
// No database is involved: `require` rejects on identity and permission before
// any handler touches the store, which is exactly the property under test.

// The spec is parsed loosely: a path item mixes operations with a `parameters`
// list, so a typed struct would fail on the list. What matters is only the two
// fields below, when they are there.
type openAPISpec struct {
	Paths map[string]map[string]any `yaml:"paths"`
}

type operation struct {
	method, path, id, permission string
}

func contractOperations(t *testing.T) []operation {
	t.Helper()
	raw, err := os.ReadFile("../../docs/specs/openapi-v1.yaml")
	if err != nil {
		t.Fatalf("cannot read the contract: %v", err)
	}
	var spec openAPISpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("cannot parse the contract: %v", err)
	}

	var ops []operation
	for path, methods := range spec.Paths {
		for method, raw := range methods {
			switch method {
			case "get", "post", "patch", "put", "delete":
			default:
				continue // `parameters`, and anything else that is not an operation
			}
			fields, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			perm, _ := fields["x-required-permission"].(string)
			if perm == "" {
				continue // public by contract (health, the webhook receivers)
			}
			id, _ := fields["operationId"].(string)
			ops = append(ops, operation{
				method: strings.ToUpper(method), path: path,
				id: id, permission: perm,
			})
		}
	}
	if len(ops) < 50 {
		t.Fatalf("only %d operations found in the contract — the parser is wrong", len(ops))
	}
	return ops
}

// request builds a call that reaches the handler: the generated wrapper
// validates mandatory parameters (If-Match, required query params) BEFORE the
// handler runs, and a 400 for a missing header would say nothing about
// authorization. So the request carries what the contract demands — and the
// UUIDs are fake on purpose, since authorization is decided before any lookup.
func request(op operation) *http.Request {
	url := concreteURL(op.path)
	if strings.Contains(op.path, "webhook-endpoint") && op.method == http.MethodDelete {
		url += "?provider=github"
	}
	req := httptest.NewRequest(op.method, url, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	// Sensitive PATCHes require If-Match (§24.1); without it the wrapper answers 400.
	req.Header.Set("If-Match", `"1"`)
	return req
}

// concreteURL turns /applications/{application_uuid}/envs into a real URL. The
// UUID does not need to exist: authorization is decided before any lookup.
func concreteURL(path string) string {
	var b strings.Builder
	b.WriteString("/api/v1")
	for _, segment := range strings.Split(path, "/") {
		if segment == "" {
			continue
		}
		b.WriteString("/")
		if strings.HasPrefix(segment, "{") {
			b.WriteString("00000000-0000-0000-0000-000000000000")
			continue
		}
		b.WriteString(segment)
	}
	return b.String()
}

// routerWithIdentity builds the generated router with a middleware that injects
// a fixed identity — standing in for the bearer middleware, whose job (looking
// a token up in the database) is not what this test is about.
func routerWithIdentity(id *auth.Identity) http.Handler {
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id != nil {
				r = r.WithContext(auth.WithIdentity(r.Context(), id))
			}
			next.ServeHTTP(w, r)
		})
	}
	return api.HandlerWithOptions(&API{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, api.ChiServerOptions{
		BaseURL:     "/api/v1",
		Middlewares: []api.MiddlewareFunc{inject},
	})
}

// Without a token, every declared operation must answer 401 — never 200, and
// never a 500 from a handler that started working before checking.
func TestEveryOperationRefusesAnonymous(t *testing.T) {
	router := routerWithIdentity(nil)
	for _, op := range contractOperations(t) {
		t.Run(op.id, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, request(op))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s (%s) answered %d without a token, want 401",
					op.method, op.path, op.id, rec.Code)
			}
		})
	}
}

// A read-only token must be refused on every operation that declares it needs
// more than read. This is the check that catches an endpoint wired to the wrong
// permission — the failure mode nobody notices, because it works.
func TestReadTokenCannotWrite(t *testing.T) {
	router := routerWithIdentity(&auth.Identity{
		TokenID: 1, TeamID: 1, TokenUUID: "t", TeamUUID: "team",
		Permissions: []string{string(auth.PermRead)},
	})
	for _, op := range contractOperations(t) {
		if op.permission == string(auth.PermRead) {
			continue
		}
		t.Run(op.id, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, request(op))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s (%s) requires %q but answered %d to a read-only token, want 403",
					op.method, op.path, op.id, op.permission, rec.Code)
			}
		})
	}
}

// A root token carries every permission: no operation may refuse it on
// authorization grounds. Whatever comes next (404, 422, 500 for a nil store) is
// out of scope — what matters is that it is not 401 or 403.
func TestRootTokenIsNeverRefused(t *testing.T) {
	router := routerWithIdentity(&auth.Identity{
		TokenID: 1, TeamID: 1, TokenUUID: "t", TeamUUID: "team",
		Permissions: []string{string(auth.PermRoot)},
	})
	for _, op := range contractOperations(t) {
		// Instance-wide settings (/system/*, permission `root`) are the deliberate
		// exception: they are reserved to a SESSION of the instance root, never a
		// token — even a root one (rbac-matrix §3.5). Covered by
		// TestInstanceSettingsRequireInstanceRootSession instead. Other
		// root-permission endpoints (e.g. OIDC provider management) are NOT
		// instance-gated and must still accept a root token.
		if strings.HasPrefix(op.path, "/system") && op.permission == string(auth.PermRoot) {
			continue
		}
		t.Run(op.id, func(t *testing.T) {
			rec := httptest.NewRecorder()
			defer func() {
				// A nil store makes handlers panic once they get past auth —
				// which is itself proof that authorization let them through.
				_ = recover()
			}()
			router.ServeHTTP(rec, request(op))
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("%s %s (%s) refused a root token with %d",
					op.method, op.path, op.id, rec.Code)
			}
		})
	}
}

// Instance-wide settings (/system/*) are reserved to a SESSION of the instance
// root (users.is_root) — the platform administrator, outside the team-role model
// (rbac-matrix §3.5). A team owner/admin's team-scoped `root` permission is not
// enough, and API tokens are team-bound so are always refused here.
func TestInstanceSettingsRequireInstanceRootSession(t *testing.T) {
	rootToken := &auth.Identity{
		TokenID: 1, TeamID: 1, TokenUUID: "t", TeamUUID: "team",
		Permissions: []string{string(auth.PermRoot)},
	}
	teamOwnerSession := &auth.Identity{
		TeamID: 1, TeamUUID: "team", Session: true,
		Permissions: []string{string(auth.PermRoot)}, // owner role grants `root`, but not is_root
	}
	instanceRootSession := &auth.Identity{
		TeamID: 1, TeamUUID: "team", Session: true, InstanceRoot: true,
		Permissions: []string{string(auth.PermRoot)},
	}
	for _, op := range contractOperations(t) {
		if !strings.HasPrefix(op.path, "/system") || op.permission != string(auth.PermRoot) {
			continue
		}
		t.Run(op.id, func(t *testing.T) {
			for name, id := range map[string]*auth.Identity{"root token": rootToken, "team owner session": teamOwnerSession} {
				rec := httptest.NewRecorder()
				routerWithIdentity(id).ServeHTTP(rec, request(op))
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s %s: %s got %d, want 403", op.method, op.path, name, rec.Code)
				}
			}
			// The instance-root session clears authorization (whatever the nil
			// store then does — panic/500 — is out of scope).
			rec := httptest.NewRecorder()
			defer func() { _ = recover() }()
			routerWithIdentity(instanceRootSession).ServeHTTP(rec, request(op))
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("%s %s: instance-root session refused with %d", op.method, op.path, rec.Code)
			}
		})
	}
}
