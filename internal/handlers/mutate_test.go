package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"My Project":          "my-project",
		"  Prod / EU-West  ":  "prod-eu-west",
		"déjà_vu":             "dj-vu", // non-ASCII dropped
		"---":                 "",
		"Production":          "production",
		"a..b":                "a-b",
		"UPPER":               "upper",
		"123":                 "123",
		"trailing dash-":      "trailing-dash",
		"multi   spaces here": "multi-spaces-here",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEtagAndIfMatch(t *testing.T) {
	if etagFor(3) != `"3"` {
		t.Fatalf("etagFor(3) = %s", etagFor(3))
	}
	r := httptest.NewRequest(http.MethodPatch, "/", nil)
	if v := ifMatchVersion(r, 7); v != 7 {
		t.Fatalf("absent If-Match must fall back to current version, got %d", v)
	}
	r.Header.Set("If-Match", `"7"`)
	if v := ifMatchVersion(r, 7); v != 7 {
		t.Fatalf("quoted If-Match not parsed, got %d", v)
	}
	r.Header.Set("If-Match", "6")
	if v := ifMatchVersion(r, 7); v != 6 {
		t.Fatalf("unquoted If-Match not parsed, got %d", v)
	}
	r.Header.Set("If-Match", "garbage")
	if v := ifMatchVersion(r, 7); v != 0 {
		t.Fatalf("malformed If-Match must never match, got %d", v)
	}
}

func TestDecodePatchPresence(t *testing.T) {
	var into struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	r := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"description": null}`))
	patch, ok := decodePatch(httptest.NewRecorder(), r, &into)
	if !ok {
		t.Fatal("decode failed")
	}
	if patch.Has("name") {
		t.Fatal("name was absent")
	}
	if !patch.Has("description") || !patch.IsNull("description") {
		t.Fatal("description was present and null")
	}
}
