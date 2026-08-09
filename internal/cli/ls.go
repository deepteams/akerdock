package cli

import (
	"context"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

// resource is the subset of fields the CLI shows for any listed resource.
type resource struct {
	Uuid           string `json:"uuid"`
	Name           string `json:"name"`
	SourceType     string `json:"source_type,omitempty"`
	Engine         string `json:"engine,omitempty"`
	DesiredStatus  string `json:"desired_status,omitempty"`
	ObservedStatus string `json:"observed_status,omitempty"`
}

type resourcePage struct {
	Data       []resource `json:"data"`
	NextCursor *string    `json:"next_cursor"`
}

// listKinds maps the transversal `list <kind>` argument to its API path.
var listKinds = map[string]string{
	"apps":      "/applications",
	"databases": "/databases",
	"services":  "/services",
	"servers":   "/servers",
}

// listCmd is the one listing that belongs to no group (ADR-070 §1): its subject
// is the team, not a kind. `akerdock list` with no argument walks applications,
// databases and services at once — the question "what do I have here".
func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "list [apps|databases|services|servers]",
		Aliases:   listAliases(),
		Short:     "List resources across kinds (default: applications, databases and services)",
		ValidArgs: []string{"apps", "databases", "services", "servers"},
		Args:      cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			kinds := []string{"apps", "databases", "services"}
			if len(args) == 1 {
				kinds = []string{args[0]}
			}

			type row struct {
				Kind string `json:"kind"`
				resource
			}
			var all []row
			for _, kind := range kinds {
				path, ok := listKinds[kind]
				if !ok {
					return usageErrorf("cannot list %q — expected one of apps, databases, services, servers", kind)
				}
				items, err := c.listAll(cmd.Context(), path)
				if err != nil {
					return err
				}
				for _, it := range items {
					all = append(all, row{Kind: kind, resource: it})
				}
			}

			if flags.output == "json" {
				return printJSON(all)
			}
			rows := make([][]string, 0, len(all))
			for _, r := range all {
				rows = append(rows, []string{r.Kind, r.Name, resourceType(r.resource), r.DesiredStatus, r.ObservedStatus, r.Uuid})
			}
			table([]string{"KIND", "NAME", "TYPE", "DESIRED", "OBSERVED", "UUID"}, rows)
			return nil
		},
	}
}

// listGroupCmd is the same listing narrowed to one group, so `akerdock db list`
// answers without making the reader translate the type into an argument of
// another command.
func listGroupCmd(k resourceKind) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: listAliases(),
		Short:   "List the team's " + k.plural,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			items, err := c.listAll(cmd.Context(), k.path)
			if err != nil {
				return err
			}
			if flags.output == "json" {
				return printJSON(items)
			}
			rows := make([][]string, 0, len(items))
			for _, it := range items {
				rows = append(rows, []string{it.Name, resourceType(it), it.DesiredStatus, it.ObservedStatus, it.Uuid})
			}
			table([]string{"NAME", "TYPE", "DESIRED", "OBSERVED", "UUID"}, rows)
			return nil
		},
	}
}

// resourceType renders the column that means a different thing per kind — the
// source of an application, the engine of a database.
func resourceType(r resource) string {
	if r.SourceType != "" {
		return r.SourceType
	}
	return r.Engine
}

// listAll follows the cursor pagination of a list endpoint.
func (c *Client) listAll(ctx context.Context, path string) ([]resource, error) {
	var out []resource
	cursor := ""
	for {
		q := url.Values{"limit": {"100"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page resourcePage
		if err := c.do(ctx, http.MethodGet, path, q, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Data...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		cursor = *page.NextCursor
	}
	return out, nil
}
