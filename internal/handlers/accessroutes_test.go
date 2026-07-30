package handlers

import (
	"reflect"
	"testing"

	"github.com/deepteams/akerdock/internal/accessroute"
	"github.com/deepteams/akerdock/internal/api"
)

func TestNormalizeAPIPublicRoutes(t *testing.T) {
	match := api.Template
	parameters := map[string][]string{"provider": {"stripe", "github"}}
	routes, details := normalizeAPIPublicRoutes([]api.AccessPublicRoute{{
		Path:       "/webhook/:provider/handler",
		Match:      &match,
		Methods:    []string{"post", "POST"},
		Parameters: &parameters,
	}}, "access_public_routes")

	if len(details) != 0 {
		t.Fatalf("unexpected validation details: %+v", details)
	}
	want := []accessroute.Route{{
		Path:       "/webhook/:provider/handler",
		Match:      accessroute.MatchTemplate,
		Methods:    []string{"POST"},
		Parameters: map[string][]string{"provider": {"github", "stripe"}},
	}}
	if !reflect.DeepEqual(routes, want) {
		t.Fatalf("routes = %#v, want %#v", routes, want)
	}
}

func TestNormalizeAPIPublicRoutesReportsIndexedField(t *testing.T) {
	match := api.Template
	routes, details := normalizeAPIPublicRoutes([]api.AccessPublicRoute{{
		Path:    "/webhook/*",
		Match:   &match,
		Methods: []string{"POST"},
	}}, "access_public_routes")

	if len(routes) != 0 || len(details) != 1 {
		t.Fatalf("routes=%+v details=%+v", routes, details)
	}
	if details[0].Field == nil || *details[0].Field != "access_public_routes[0]" {
		t.Fatalf("field = %v", details[0].Field)
	}
}

func TestValidBasicAuthCredentials(t *testing.T) {
	for _, value := range []string{"user:password", "user:password:with:colons"} {
		if !validBasicAuthCredentials(value) {
			t.Errorf("%q should be valid", value)
		}
	}
	for _, value := range []string{"", "missing-colon", ":password", "user:"} {
		if validBasicAuthCredentials(value) {
			t.Errorf("%q should be invalid", value)
		}
	}
}
