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

// refFromArgs resolves the REF to act on: the positional argument when present,
// otherwise the default application from the resolved settings (flag > env >
// .akerdock, spec §4), as `app/<name>`. This is what lets `akerdock logs` run
// with no argument inside a repository that carries a .akerdock.
func refFromArgs(args []string) (ref, error) {
	if len(args) >= 1 {
		return parseRef(args[0])
	}
	s, err := settings()
	if err != nil {
		return ref{}, err
	}
	if s.Application != "" {
		return ref{kind: "apps", name: s.Application}, nil
	}
	return ref{}, fmt.Errorf("no target given and no default application set — pass a REF (e.g. app/varuna) or set `application:` in a .akerdock file")
}

// defaultComponent returns the explicit --component when set, else the resolved
// default (env or .akerdock).
func defaultComponent(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if s, err := settings(); err == nil {
		return s.Component
	}
	return ""
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

// previewInfo is the subset of a preview the CLI needs to target it.
type previewInfo struct {
	Uuid   string `json:"uuid"`
	PrID   int    `json:"pr_id"`
	Status string `json:"status"`
}

// resolvePreview finds the preview of an application by PR number.
func (c *Client) resolvePreview(ctx context.Context, appUUID string, pr int) (previewInfo, error) {
	var page struct {
		Data []previewInfo `json:"data"`
	}
	if err := c.do(ctx, "GET", "/applications/"+appUUID+"/previews", nil, nil, &page); err != nil {
		return previewInfo{}, err
	}
	for _, p := range page.Data {
		if p.PrID == pr {
			if p.Status == "destroyed" {
				return previewInfo{}, fmt.Errorf("preview of PR #%d is destroyed — reopen or /deploy it first", pr)
			}
			return p, nil
		}
	}
	return previewInfo{}, fmt.Errorf("no preview for PR #%d on this application", pr)
}
