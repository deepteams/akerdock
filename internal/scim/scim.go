// Package scim implements the JSON contract of SCIM 2.0 (RFC 7643/7644) used to
// provision and deprovision users from an identity provider (ISO A.5.16/A.5.18).
// It is deliberately transport-agnostic: the HTTP handlers live in the handlers
// package; here are the wire types and small helpers, unit-testable on their own.
package scim

import (
	"encoding/json"
	"time"
)

// SCIM schema URNs (RFC 7643 §8, RFC 7644 §3).
const (
	SchemaUser                  = "urn:ietf:params:scim:schemas:core:2.0:User"
	SchemaListResponse          = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	SchemaError                 = "urn:ietf:params:scim:api:messages:2.0:Error"
	SchemaPatchOp               = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	SchemaServiceProviderConfig = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
)

// ContentType is the media type SCIM responses use.
const ContentType = "application/scim+json"

// Name is a SCIM user's structured name.
type Name struct {
	Formatted  string `json:"formatted,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}

// Email is one of a user's email addresses.
type Email struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary,omitempty"`
}

// Meta is the SCIM resource metadata block.
type Meta struct {
	ResourceType string     `json:"resourceType"`
	Location     string     `json:"location,omitempty"`
	Created      *time.Time `json:"created,omitempty"`
}

// User is a SCIM core User resource. id is AkerDock's user UUID; userName is the
// email (the login identifier).
type User struct {
	Schemas     []string `json:"schemas"`
	ID          string   `json:"id,omitempty"`
	ExternalID  string   `json:"externalId,omitempty"`
	UserName    string   `json:"userName"`
	Name        *Name    `json:"name,omitempty"`
	DisplayName string   `json:"displayName,omitempty"`
	Emails      []Email  `json:"emails,omitempty"`
	Active      bool     `json:"active"`
	Meta        *Meta    `json:"meta,omitempty"`
}

// PrimaryEmail returns the login email: emails[primary] or emails[0], falling
// back to userName (Okta sends userName, Azure often sends emails).
func (u User) PrimaryEmail() string {
	for _, e := range u.Emails {
		if e.Primary && e.Value != "" {
			return e.Value
		}
	}
	if len(u.Emails) > 0 && u.Emails[0].Value != "" {
		return u.Emails[0].Value
	}
	return u.UserName
}

// DisplayNameOr returns the best human name for the account.
func (u User) DisplayNameOr(fallback string) string {
	switch {
	case u.DisplayName != "":
		return u.DisplayName
	case u.Name != nil && u.Name.Formatted != "":
		return u.Name.Formatted
	case u.Name != nil && (u.Name.GivenName != "" || u.Name.FamilyName != ""):
		return trimSpace(u.Name.GivenName + " " + u.Name.FamilyName)
	default:
		return fallback
	}
}

func trimSpace(s string) string {
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}

// ListResponse wraps a page of resources (RFC 7644 §3.4.2).
type ListResponse struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    []any    `json:"Resources"`
}

// NewListResponse builds a SCIM ListResponse.
func NewListResponse(resources []any, startIndex int) ListResponse {
	return ListResponse{
		Schemas:      []string{SchemaListResponse},
		TotalResults: len(resources),
		StartIndex:   startIndex,
		ItemsPerPage: len(resources),
		Resources:    resources,
	}
}

// Error is a SCIM error response (RFC 7644 §3.12).
type Error struct {
	Schemas []string `json:"schemas"`
	Detail  string   `json:"detail"`
	Status  string   `json:"status"`
}

// NewError builds a SCIM error with the given HTTP status.
func NewError(status int, detail string) Error {
	return Error{Schemas: []string{SchemaError}, Detail: detail, Status: itoa(status)}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// PatchRequest is a SCIM PATCH body (RFC 7644 §3.5.2).
type PatchRequest struct {
	Schemas    []string         `json:"schemas"`
	Operations []PatchOperation `json:"Operations"`
}

// PatchOperation is one op of a PATCH: op is add/replace/remove, path is the
// attribute, value its new content.
type PatchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// Group is a SCIM core Group resource. AkerDock exposes ROLES as virtual groups:
// a group maps to a team role, its members are the members holding that role.
type Group struct {
	Schemas     []string      `json:"schemas"`
	ID          string        `json:"id"`
	DisplayName string        `json:"displayName"`
	Members     []GroupMember `json:"members"`
	Meta        *Meta         `json:"meta,omitempty"`
}

// GroupMember references a user in a group.
type GroupMember struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
}

// SchemaGroup is the SCIM core Group URN.
const SchemaGroup = "urn:ietf:params:scim:schemas:core:2.0:Group"

// SystemRoleGroupID is the stable group id for a built-in role (e.g.
// "role:admin"). Custom roles use their UUID as the group id instead.
func SystemRoleGroupID(role string) string { return "role:" + role }

// ParseGroupID splits a group id into either a system role name ("role:admin" →
// "admin") or a custom-role id (any other value, returned as customID).
func ParseGroupID(id string) (systemRole, customID string) {
	if rest, ok := stripPrefix(id, "role:"); ok {
		return rest, ""
	}
	return "", id
}

func stripPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):], true
	}
	return "", false
}

// MemberValuesFromOp extracts the user ids a PATCH operation adds or removes
// from a group, handling both shapes IdPs send:
//   - value array:  {op, path:"members", value:[{"value":"id"}, …]}
//   - filter path:  {op:"remove", path:`members[value eq "id"]`}
func MemberValuesFromOp(op PatchOperation) []string {
	if id, ok := memberIDFromFilterPath(op.Path); ok {
		return []string{id}
	}
	var arr []GroupMember
	if json.Unmarshal(op.Value, &arr) == nil && len(arr) > 0 {
		out := make([]string, 0, len(arr))
		for _, m := range arr {
			if m.Value != "" {
				out = append(out, m.Value)
			}
		}
		return out
	}
	return nil
}

// memberIDFromFilterPath reads `members[value eq "X"]` → X.
func memberIDFromFilterPath(path string) (string, bool) {
	rest, ok := stripPrefix(path, "members[value eq ")
	if !ok {
		return "", false
	}
	rest = trimSpace(rest)
	rest = trimSuffix(rest, "]")
	rest = trimSpace(rest)
	if len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"' {
		return rest[1 : len(rest)-1], true
	}
	return "", false
}

func trimSuffix(s, suf string) string {
	if len(s) >= len(suf) && s[len(s)-len(suf):] == suf {
		return s[:len(s)-len(suf)]
	}
	return s
}

// ServiceProviderConfig advertises supported features (RFC 7643 §5). IdPs fetch
// it before provisioning; we advertise a modest, honest set.
func ServiceProviderConfig() map[string]any {
	supported := func(v bool) map[string]any { return map[string]any{"supported": v} }
	return map[string]any{
		"schemas":               []string{SchemaServiceProviderConfig},
		"patch":                 supported(true),
		"bulk":                  map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":                map[string]any{"supported": true, "maxResults": 200},
		"changePassword":        supported(false),
		"sort":                  supported(false),
		"etag":                  supported(false),
		"authenticationSchemes": []any{},
	}
}
