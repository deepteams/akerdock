package jobs

import (
	"context"
	"fmt"
	"regexp"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// Shared variables (§5.4, §3.1): {{team.VAR}} / {{project.VAR}} /
// {{environment.VAR}} references interpolated inside resource variables,
// plus server-scoped variables injected into every resource deployed on
// that server.

// sharedRefRe matches one {{scope.KEY}} reference.
var sharedRefRe = regexp.MustCompile(`\{\{(team|project|environment)\.([A-Za-z_][A-Za-z0-9_]*)\}\}`)

// sharedEnv is the resolved inheritance of one resource.
type sharedEnv struct {
	// refs maps "scope.KEY" to its plaintext value.
	refs map[string]string
	// server holds the server-scoped variables, injected as plain variables
	// unless the resource defines the key itself (the resource wins).
	server map[string]string
}

// resolveSharedEnv loads and decrypts everything the resource inherits.
// Cheap when the team uses no shared variables: one indexed query, no rows.
func resolveSharedEnv(ctx context.Context, q *store.Queries, keyring *envelope.Keyring, resourceID int64) (sharedEnv, error) {
	out := sharedEnv{refs: map[string]string{}, server: map[string]string{}}
	rows, err := q.ListSharedVariablesForResource(ctx, resourceID)
	if err != nil {
		return out, err
	}
	for _, v := range rows {
		plaintext, err := keyring.Decrypt("shared_variables", "value_enc", pguuid.String(v.Uuid), v.ValueEnc)
		if err != nil {
			return out, fmt.Errorf("decrypt shared variable %s: %w", v.Key, err)
		}
		if v.Scope == store.SharedVariableScopeServer {
			out.server[v.Key] = string(plaintext)
			continue
		}
		out.refs[string(v.Scope)+"."+v.Key] = string(plaintext)
	}
	return out, nil
}

// interpolate replaces the {{scope.KEY}} references of one value. An unknown
// reference stays verbatim: a visibly unresolved placeholder in the container
// beats a silently empty value nobody can explain.
func (s sharedEnv) interpolate(value string) string {
	if len(s.refs) == 0 {
		return value
	}
	return sharedRefRe.ReplaceAllStringFunc(value, func(m string) string {
		parts := sharedRefRe.FindStringSubmatch(m)
		if v, ok := s.refs[parts[1]+"."+parts[2]]; ok {
			return v
		}
		return m
	})
}
