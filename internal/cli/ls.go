package cli

import (
	"context"
	"fmt"
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

// listKinds maps the `ls <kind>` argument to its API path.
var listKinds = map[string]string{
	"apps":      "/applications",
	"databases": "/databases",
	"services":  "/services",
	"previews":  "", // handled specially (per-application), not in v1 transversal
	"servers":   "/servers",
}

func lsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:       "ls [apps|databases|services|servers]",
		Short:     "List resources (default: applications, databases and services)",
		ValidArgs: []string{"apps", "databases", "services", "servers"},
		Args:      cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient(flags.context)
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
				if !ok || path == "" {
					return fmt.Errorf("cannot list %q", kind)
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
				typ := r.SourceType
				if typ == "" {
					typ = r.Engine
				}
				rows = append(rows, []string{r.Kind, r.Name, typ, r.DesiredStatus, r.ObservedStatus, r.Uuid})
			}
			table([]string{"KIND", "NAME", "TYPE", "DESIRED", "OBSERVED", "UUID"}, rows)
			return nil
		},
	}
	return cmd
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
