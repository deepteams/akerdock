package cli

import (
	"context"
	"fmt"
	"strings"
)

// ref is a parsed resource reference: `type/name-or-uuid` (kubectl-style).
type ref struct {
	kind string // apps | databases | services | previews
	name string
}

// refKinds maps the REF prefix to a list kind.
var refKinds = map[string]string{
	"app": "apps", "application": "apps",
	"db": "databases", "database": "databases",
	"svc": "services", "service": "services",
	"preview": "previews", "pr": "previews",
}

// pathForKind maps a kind to its API collection path.
var pathForKind = map[string]string{
	"apps": "/applications", "databases": "/databases", "services": "/services",
}

func parseRef(s string) (ref, error) {
	prefix, name, ok := strings.Cut(s, "/")
	if !ok || name == "" {
		return ref{}, fmt.Errorf("invalid REF %q — expected type/name (e.g. app/varuna, db/pg)", s)
	}
	kind, ok := refKinds[strings.ToLower(prefix)]
	if !ok {
		return ref{}, fmt.Errorf("unknown resource type %q in %q", prefix, s)
	}
	return ref{kind: kind, name: name}, nil
}

// resolve turns a REF into a resource UUID, matching by UUID or by name within
// the active team.
func (c *Client) resolve(ctx context.Context, r ref) (resource, error) {
	path, ok := pathForKind[r.kind]
	if !ok {
		return resource{}, fmt.Errorf("resolving %q is not supported", r.kind)
	}
	items, err := c.listAll(ctx, path)
	if err != nil {
		return resource{}, err
	}
	var byName []resource
	for _, it := range items {
		if it.Uuid == r.name {
			return it, nil
		}
		if it.Name == r.name {
			byName = append(byName, it)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return resource{}, fmt.Errorf("no %s named %q", r.kind, r.name)
	default:
		return resource{}, fmt.Errorf("several %s named %q — use the UUID", r.kind, r.name)
	}
}
