package accessroute

import (
	"reflect"
	"testing"
)

func TestValidateTemplate(t *testing.T) {
	got, err := Validate(Route{
		Path:    "/webhook/:provider/handler",
		Match:   MatchTemplate,
		Methods: []string{"post", "POST"},
		Parameters: map[string][]string{
			"provider": {"stripe", "github"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Methods, []string{"POST"}) {
		t.Fatalf("methods = %v", got.Methods)
	}
	want := "PathRegexp(`^/webhook/(github|stripe)/handler$`)"
	if expression := PathExpression(got); expression != want {
		t.Fatalf("expression = %q, want %q", expression, want)
	}
}

func TestPrefixIsSegmentBounded(t *testing.T) {
	route, err := Validate(Route{Path: "/hooks", Match: MatchPrefix, Methods: []string{"POST"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "(Path(`/hooks`) || PathPrefix(`/hooks/`))"
	if got := PathExpression(route); got != want {
		t.Fatalf("expression = %q, want %q", got, want)
	}
}

func TestUnrestrictedTemplateStillRejectsEncodedPathSeparators(t *testing.T) {
	route, err := Validate(Route{
		Path: "/webhook/:provider/handler", Match: MatchTemplate, Methods: []string{"POST"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "PathRegexp(`^/webhook/[A-Za-z0-9._~-]+/handler$`)"
	if got := PathExpression(route); got != want {
		t.Fatalf("expression = %q, want %q", got, want)
	}
}

func TestValidateRejectsBroadOrAmbiguousPatterns(t *testing.T) {
	tests := []Route{
		{Path: "hooks", Match: MatchExact, Methods: []string{"POST"}},
		{Path: "/hooks/**", Match: MatchTemplate, Methods: []string{"POST"}},
		{Path: "/hooks/*", Match: MatchPrefix, Methods: []string{"POST"}},
		{Path: "/hooks/new line", Match: MatchExact, Methods: []string{"POST"}},
		{Path: `/hooks\admin`, Match: MatchExact, Methods: []string{"POST"}},
		{Path: "/hooks/:id", Match: MatchExact, Methods: []string{"POST"}},
		{Path: "/hooks/:id", Match: MatchTemplate},
		{Path: "/hooks/%2f/admin", Match: MatchExact, Methods: []string{"GET"}},
		{Path: "/hooks/:id", Match: MatchTemplate, Methods: []string{"GET"}, Parameters: map[string][]string{"other": {"x"}}},
		{Path: "/hooks/:id", Match: MatchTemplate, Methods: []string{"GET"}, Parameters: map[string][]string{"id": {".."}}},
	}
	for _, route := range tests {
		if _, err := Validate(route); err == nil {
			t.Errorf("Validate(%+v) unexpectedly succeeded", route)
		}
	}
}

func TestCoversBaseRoute(t *testing.T) {
	under := Route{Path: "/api/webhook/:provider", Match: MatchTemplate}
	if !Covers("/api", under) {
		t.Fatal("template under /api should be covered")
	}
	if Covers("/admin", under) {
		t.Fatal("unrelated base path should not be covered")
	}
}
