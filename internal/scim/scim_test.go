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
	if got := (User{Name: &Name{Formatted: "Alice Q. Person", GivenName: "Al"}}).DisplayNameOr("x"); got != "Alice Q. Person" {
		t.Errorf("formatted name = %q", got)
	}
	if got := (User{Name: &Name{GivenName: "Al", FamilyName: "Ice"}}).DisplayNameOr("x"); got != "Al Ice" {
		t.Errorf("composed name = %q", got)
	}
	// Only one of the two parts set: the joining space must be trimmed.
	if got := (User{Name: &Name{GivenName: "Al"}}).DisplayNameOr("x"); got != "Al" {
		t.Errorf("given only = %q", got)
	}
	if got := (User{Name: &Name{FamilyName: "Ice"}}).DisplayNameOr("x"); got != "Ice" {
		t.Errorf("family only = %q", got)
	}
	if got := (User{}).DisplayNameOr("fallback"); got != "fallback" {
		t.Errorf("fallback = %q", got)
	}
}

func TestParseGroupID(t *testing.T) {
	if sys, custom := ParseGroupID("role:admin"); sys != "admin" || custom != "" {
		t.Errorf("role id = %q/%q", sys, custom)
	}
	if sys, custom := ParseGroupID("11111111-1111-1111-1111-111111111111"); sys != "" || custom != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("custom id = %q/%q", sys, custom)
	}
}

func TestSystemRoleGroupID(t *testing.T) {
	if got := SystemRoleGroupID("admin"); got != "role:admin" {
		t.Errorf("SystemRoleGroupID = %q, want %q", got, "role:admin")
	}
	// Round-trips through ParseGroupID.
	if sys, custom := ParseGroupID(SystemRoleGroupID("member")); sys != "member" || custom != "" {
		t.Errorf("round-trip = %q/%q", sys, custom)
	}
}

func TestMemberValuesFromOp(t *testing.T) {
	// Azure-style value array.
	arr := MemberValuesFromOp(PatchOperation{Op: "add", Path: "members", Value: []byte(`[{"value":"u1"},{"value":"u2"}]`)})
	if len(arr) != 2 || arr[0] != "u1" || arr[1] != "u2" {
		t.Errorf("value array = %v", arr)
	}
	// Okta-style filter path on remove.
	rm := MemberValuesFromOp(PatchOperation{Op: "remove", Path: `members[value eq "u9"]`})
	if len(rm) != 1 || rm[0] != "u9" {
		t.Errorf("filter path = %v", rm)
	}
	// Nothing extractable.
	if got := MemberValuesFromOp(PatchOperation{Op: "replace", Path: "displayName", Value: []byte(`"x"`)}); got != nil {
		t.Errorf("unexpected members = %v", got)
	}
	// Filter path with an unquoted id is rejected.
	if got := MemberValuesFromOp(PatchOperation{Op: "remove", Path: `members[value eq u9]`}); got != nil {
		t.Errorf("unquoted filter = %v", got)
	}
	// Filter path missing the closing bracket still yields the quoted id.
	if got := MemberValuesFromOp(PatchOperation{Op: "remove", Path: `members[value eq "u9"`}); len(got) != 1 || got[0] != "u9" {
		t.Errorf("no closing bracket = %v", got)
	}
	// Entries with an empty value are skipped.
	arr2 := MemberValuesFromOp(PatchOperation{Op: "add", Path: "members", Value: []byte(`[{"value":""},{"value":"u3"}]`)})
	if len(arr2) != 1 || arr2[0] != "u3" {
		t.Errorf("empty values skipped = %v", arr2)
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
	// Status 0 must not render as an empty string.
	raw0, _ := json.Marshal(NewError(0, "zero"))
	var e0 map[string]any
	_ = json.Unmarshal(raw0, &e0)
	if e0["status"] != "0" {
		t.Errorf("zero status = %v", e0["status"])
	}
}

func TestServiceProviderConfig(t *testing.T) {
	cfg := ServiceProviderConfig()
	schemas, ok := cfg["schemas"].([]string)
	if !ok || len(schemas) != 1 || schemas[0] != SchemaServiceProviderConfig {
		t.Errorf("schemas = %v", cfg["schemas"])
	}
	patch, ok := cfg["patch"].(map[string]any)
	if !ok || patch["supported"] != true {
		t.Errorf("patch = %v", cfg["patch"])
	}
	bulk, ok := cfg["bulk"].(map[string]any)
	if !ok || bulk["supported"] != false {
		t.Errorf("bulk = %v", cfg["bulk"])
	}
	filter, ok := cfg["filter"].(map[string]any)
	if !ok || filter["supported"] != true || filter["maxResults"] != 200 {
		t.Errorf("filter = %v", cfg["filter"])
	}
	for _, key := range []string{"changePassword", "sort", "etag"} {
		m, ok := cfg[key].(map[string]any)
		if !ok || m["supported"] != false {
			t.Errorf("%s = %v", key, cfg[key])
		}
	}
	// The whole document must serialize cleanly.
	if _, err := json.Marshal(cfg); err != nil {
		t.Errorf("marshal: %v", err)
	}
}
