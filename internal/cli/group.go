package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// resourceKind is one command group of the typed tree (ADR-070 §1): a family of
// resources sharing an API collection and a verb space. The group name IS the
// type, which is why no argument repeats it — `akerdock app logs varuna`, never
// `app logs app/varuna`.
type resourceKind struct {
	group  string // command name and what the user types: app | db | svc
	label  string // human singular, for messages: application
	plural string
	path   string // API collection path: /applications
	// dirDefault says whether a name may be omitted because `.akerdock` (or
	// --application) supplies one. Only applications have such a default: a
	// repository declares the app it deploys, never the database it talks to.
	dirDefault bool
}

var (
	kindApp = resourceKind{group: "app", label: "application", plural: "applications", path: "/applications", dirDefault: true}
	kindDB  = resourceKind{group: "db", label: "database", plural: "databases", path: "/databases"}
	kindSvc = resourceKind{group: "svc", label: "compose stack", plural: "services", path: "/services"}

	// Not command groups: these two are resolved by name inside `tunnel` and
	// `ingress`, whose target is a declared endpoint rather than a deployed
	// resource (ADR-045/ADR-060).
	kindEndpoint = resourceKind{group: "tunnel", label: "external endpoint", plural: "external endpoints", path: "/external-endpoints"}
	kindIngress  = resourceKind{group: "ingress", label: "ingress endpoint", plural: "ingress endpoints", path: "/ingress-endpoints"}
)

// target resolves the resource a group's verb acts on: the positional name when
// given, otherwise the directory default for the kinds that have one.
//
// It is also where the removed REF is caught. `akerdock app logs app/varuna`
// would otherwise look for an application literally named "app/varuna" and fail
// with "no applications named app/varuna" — true, useless, and silent about the
// spelling that replaced it (ADR-070 §5).
func (c *Client) target(ctx context.Context, k resourceKind, args []string) (resource, error) {
	name, err := targetName(k, args)
	if err != nil {
		return resource{}, err
	}
	return c.resolveNamed(ctx, k, name)
}

// targetName applies the precedence without touching the network, so the REF
// refusal and the missing-default message can be unit-tested on their own.
func targetName(k resourceKind, args []string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		return checkNotARef(k, args[0])
	}
	if k.dirDefault {
		s, err := settings()
		if err != nil {
			return "", err
		}
		if s.Application != "" {
			return s.Application, nil
		}
		return "", usageErrorf("no %s given and no default set — pass a name (e.g. akerdock %s <name>) or set `application:` in a .akerdock file",
			k.label, k.group)
	}
	return "", usageErrorf("no %s given — pass its name (e.g. akerdock %s <verb> <name>)", k.label, k.group)
}

// refPrefixes are the type prefixes the REF used to carry, kept for one purpose:
// recognising the old spelling in order to refuse it by name.
var refPrefixes = map[string]string{
	"app": "app", "application": "app",
	"db": "db", "database": "db",
	"svc": "svc", "service": "svc",
	"preview": "", "pr": "", // `preview/…` had no group of its own: --pr does that
	"endpoint": "", "ep": "", "ingress": "",
}

// checkNotARef rejects a `type/name` argument with the command that replaced it.
func checkNotARef(k resourceKind, arg string) (string, error) {
	prefix, name, ok := strings.Cut(arg, "/")
	if !ok {
		return arg, nil
	}
	group, known := refPrefixes[strings.ToLower(prefix)]
	if !known || name == "" {
		// Not a REF we ever accepted: a name with a slash in it is not something
		// this API produces, so say what shape is expected rather than guess.
		return "", usageErrorf("invalid %s name %q — pass the name alone (e.g. akerdock %s <verb> <name>)", k.label, arg, k.group)
	}
	if group == "" {
		group = k.group
	}
	return "", usageErrorf("the type/name form is gone — use: akerdock %s <verb> %s", group, name)
}

// resolveNamed turns a bare name (or a UUID) into a resource of this kind.
func (c *Client) resolveNamed(ctx context.Context, k resourceKind, name string) (resource, error) {
	items, err := c.listAll(ctx, k.path)
	if err != nil {
		return resource{}, err
	}
	var byName []resource
	for _, it := range items {
		if it.Uuid == name {
			return it, nil
		}
		if it.Name == name {
			byName = append(byName, it)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return resource{}, fmt.Errorf("no %s named %q", k.plural, name)
	default:
		return resource{}, fmt.Errorf("several %s named %q — use the UUID", k.plural, name)
	}
}

// targetArgs is the positional contract shared by every targeted verb: at most
// one name, optional wherever a directory default can stand in for it.
func targetArgs(k resourceKind) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) > 1 {
			return usageErrorf("usage: akerdock %s <verb> [NAME] — one name at most, got %d arguments", k.group, len(args))
		}
		return nil
	}
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
