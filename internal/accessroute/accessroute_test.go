package accessroute

import (
	"reflect"
	"strings"
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

func TestValidateDefaultsToExactMatch(t *testing.T) {
	got, err := Validate(Route{Path: "/healthz", Methods: []string{"GET"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Match != MatchExact {
		t.Fatalf("match = %q, want %q", got.Match, MatchExact)
	}
	if want := "Path(`/healthz`)"; PathExpression(got) != want {
		t.Fatalf("expression = %q, want %q", PathExpression(got), want)
	}
}

func TestValidateRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		route Route
	}{
		{"unknown match mode", Route{Path: "/hooks", Match: "glob", Methods: []string{"GET"}}},
		{"empty path", Route{Match: MatchExact, Methods: []string{"GET"}}},
		{"oversized path", Route{Path: "/" + strings.Repeat("a", maxPathBytes), Match: MatchExact, Methods: []string{"GET"}}},
		{"empty segment", Route{Path: "/hooks//x", Match: MatchExact, Methods: []string{"GET"}}},
		{"dot segment", Route{Path: "/hooks/./x", Match: MatchExact, Methods: []string{"GET"}}},
		{"dot-dot segment", Route{Path: "/hooks/..", Match: MatchExact, Methods: []string{"GET"}}},
		{"blank method", Route{Path: "/hooks", Match: MatchExact, Methods: []string{"  "}}},
		{"malformed method", Route{Path: "/hooks", Match: MatchExact, Methods: []string{"G=T"}}},
		{"parameters without template", Route{Path: "/hooks", Match: MatchPrefix, Methods: []string{"GET"}, Parameters: map[string][]string{"id": {"x"}}}},
		{"parameter with no values", Route{Path: "/hooks/:id", Match: MatchTemplate, Methods: []string{"GET"}, Parameters: map[string][]string{"id": {}}}},
		{"mid-segment colon", Route{Path: "/hooks/a:b", Match: MatchTemplate, Methods: []string{"GET"}}},
		{"invalid parameter name", Route{Path: "/hooks/:1bad", Match: MatchTemplate, Methods: []string{"GET"}}},
		{"template without placeholder", Route{Path: "/hooks/static", Match: MatchTemplate, Methods: []string{"GET"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Validate(test.route); err == nil {
				t.Errorf("Validate(%+v) unexpectedly succeeded", test.route)
			}
		})
	}
}

func TestPathExpressionPrefixWithTrailingSlash(t *testing.T) {
	for _, path := range []string{"/", "/hooks/"} {
		route := Route{Path: path, Match: MatchPrefix, Methods: []string{"GET"}}
		want := "PathPrefix(`" + path + "`)"
		if got := PathExpression(route); got != want {
			t.Errorf("PathExpression(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestMethodExpression(t *testing.T) {
	if got, want := MethodExpression([]string{"POST"}), "Method(`POST`)"; got != want {
		t.Fatalf("single method = %q, want %q", got, want)
	}
	got := MethodExpression([]string{"GET", "HEAD"})
	if want := "(Method(`GET`) || Method(`HEAD`))"; got != want {
		t.Fatalf("multiple methods = %q, want %q", got, want)
	}
}

func TestIsNavigationMethod(t *testing.T) {
	for method, want := range map[string]bool{"GET": true, "HEAD": true, "POST": false, "get": false} {
		if got := IsNavigationMethod(method); got != want {
			t.Errorf("IsNavigationMethod(%q) = %v, want %v", method, got, want)
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
	if !Covers("", under) || !Covers("/", under) {
		t.Fatal("an empty or root base covers everything")
	}
	if !Covers("/api/", Route{Path: "/api", Match: MatchExact}) {
		t.Fatal("a trailing-slash base should cover its own path")
	}
}
