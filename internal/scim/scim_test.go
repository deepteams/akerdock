package scim

import (
	"encoding/json"
	"testing"
)

func TestPrimaryEmail(t *testing.T) {
	cases := []struct {
		name string
		user User
		want string
	}{
		{"primary flag wins", User{UserName: "u", Emails: []Email{{Value: "a@x"}, {Value: "b@x", Primary: true}}}, "b@x"},
		{"first when no primary", User{UserName: "u", Emails: []Email{{Value: "a@x"}}}, "a@x"},
		{"falls back to userName", User{UserName: "u@x"}, "u@x"},
	}
	for _, c := range cases {
		if got := c.user.PrimaryEmail(); got != c.want {
			t.Errorf("%s: PrimaryEmail = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDisplayNameOr(t *testing.T) {
	if got := (User{DisplayName: "Alice"}).DisplayNameOr("x"); got != "Alice" {
		t.Errorf("displayName = %q", got)
	}
	if got := (User{Name: &Name{GivenName: "Al", FamilyName: "Ice"}}).DisplayNameOr("x"); got != "Al Ice" {
		t.Errorf("composed name = %q", got)
	}
	if got := (User{}).DisplayNameOr("fallback"); got != "fallback" {
		t.Errorf("fallback = %q", got)
	}
}

func TestErrorAndListShapes(t *testing.T) {
	raw, _ := json.Marshal(NewError(404, "nope"))
	var e map[string]any
	_ = json.Unmarshal(raw, &e)
	if e["status"] != "404" || e["detail"] != "nope" {
		t.Errorf("error shape = %v", e)
	}
	list := NewListResponse([]any{1, 2, 3}, 1)
	if list.TotalResults != 3 || list.ItemsPerPage != 3 || list.StartIndex != 1 {
		t.Errorf("list shape = %+v", list)
	}
	if len(list.Schemas) != 1 || list.Schemas[0] != SchemaListResponse {
		t.Errorf("list schema = %v", list.Schemas)
	}
}
