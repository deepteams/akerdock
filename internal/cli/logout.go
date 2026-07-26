package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/spf13/cobra"
)

func logoutCmd() *cobra.Command {
	var revoke bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored token for a context",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			name := cfg.resolveContextName(flags.context)
			if name == "" {
				return fmt.Errorf("no context selected")
			}
			if revoke {
				if err := revokeOwnCliToken(cmd.Context(), name); err != nil {
					fmt.Fprintf(os.Stderr, "note: could not revoke on the server (%v); revoke it in the panel if needed\n", err)
				}
			}
			if err := setToken(name, ""); err != nil {
				return err
			}
			fmt.Printf("logged out of %q\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke", false, "also revoke the token on the server")
	return cmd
}

// revokeOwnCliToken finds this machine's CLI token by its recognizable name
// and deletes it. The token in use lists and revokes itself (rbac §: a token
// may revoke itself).
func revokeOwnCliToken(ctx context.Context, contextName string) error {
	c, err := newClient(contextName)
	if err != nil {
		return err
	}
	if c.team == "" {
		return fmt.Errorf("no team on this context")
	}
	host, _ := os.Hostname()
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	want := fmt.Sprintf("cli — %s@%s", user, host)

	var page struct {
		Data []struct {
			Uuid string `json:"uuid"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/teams/"+c.team+"/tokens", url.Values{"limit": {"100"}}, nil, &page); err != nil {
		return err
	}
	for _, t := range page.Data {
		if t.Name == want {
			return c.do(ctx, http.MethodDelete, "/teams/"+c.team+"/tokens/"+t.Uuid, nil, nil, nil)
		}
	}
	return fmt.Errorf("no matching CLI token found")
}
