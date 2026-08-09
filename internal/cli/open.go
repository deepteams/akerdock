package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// browserOpener is the seam the tests replace: everything above it is resolution
// logic worth asserting, and launching a real browser out of `go test` is not.
// The launcher itself is login.go's, unchanged — one process spawn in the
// package is enough.
var browserOpener = openBrowser

// openCmd is the honest complement of ADR-070: the whole point of the typed
// tree is to stop sending developers to the dashboard, so the one command that
// does send them there does it on purpose and with a URL that is right.
//
// An application carries no URL of its own. Routing lives on the server's
// proxy, so the public address is resolved application → its server → that
// server's domains → the entry whose resource_uuid is this application's. When
// no entry matches, the command says so instead of composing a plausible URL:
// a guessed hostname that resolves to someone else's app is worse than a
// refusal.
func openCmd() *cobra.Command {
	var dashboard bool
	cmd := &cobra.Command{
		Use:   "open [NAME]",
		Short: "Open the application in a browser",
		Long: "Opens the application's public URL — the first domain its server's proxy " +
			"routes to it — or, with --dashboard, its page on this instance.\n\n" +
			"The URL is printed as well as opened, so it can be read (or piped) on a machine " +
			"with no browser to launch.",
		Example: "  akerdock app open varuna\n" +
			"  akerdock app open varuna --dashboard\n" +
			"  akerdock app open            # the .akerdock default application",
		Args: targetArgs(kindApp),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			res, err := c.target(cmd.Context(), kindApp, args)
			if err != nil {
				return err
			}
			target := ""
			if dashboard {
				// The instance the active context points at — the same host the
				// API calls just went to, which is by construction the dashboard
				// that owns this uuid.
				target = strings.TrimRight(c.base, "/") + "/applications/" + res.Uuid
			} else if target, err = c.publicURL(cmd.Context(), res); err != nil {
				return err
			}

			if flags.output == "json" {
				// Not an API object: there is no endpoint that answers "the URL of
				// this application". The single field is the composition this
				// command exists to perform.
				return printJSON(struct {
					URL string `json:"url"`
				}{URL: target})
			}
			if !flags.quiet {
				_, _ = fmt.Fprintln(os.Stdout, target)
			}
			if err := browserOpener(target); err != nil {
				// A missing launcher is not a failed command: the URL is already
				// on stdout and that is the deliverable.
				_, _ = fmt.Fprintf(os.Stderr, "could not launch a browser (%v) — open the URL above\n", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dashboard, "dashboard", false, "open the application's page on the AkerDock instance instead of its public URL")
	return cmd
}

// serverDomainEntry is one row of GET /servers/{uuid}/domains: the domains the
// server's proxy routes, grouped by the resource they reach.
type serverDomainEntry struct {
	ResourceUuid string   `json:"resource_uuid"`
	ResourceType string   `json:"resource_type"`
	Domains      []string `json:"domains"`
}

// publicURL walks the two hops between an application and its address. The list
// endpoint the target came from does not carry server_uuid, so the application
// is re-read in full for that one field.
//
// Note the permission asymmetry this crosses: the domains live under
// `servers:read`, which a reader of applications does not necessarily hold. The
// API's refusal is passed through as-is rather than reworded — it names the
// permission, which is the actionable part.
func (c *Client) publicURL(ctx context.Context, res resource) (string, error) {
	var app struct {
		ServerUuid string `json:"server_uuid"`
	}
	if err := c.do(ctx, http.MethodGet, kindApp.path+"/"+res.Uuid, nil, nil, &app); err != nil {
		return "", err
	}
	if app.ServerUuid == "" {
		return "", fmt.Errorf("%s is not attached to a server — nothing routes to it yet", res.Name)
	}
	var page struct {
		Data []serverDomainEntry `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/servers/"+app.ServerUuid+"/domains", nil, nil, &page); err != nil {
		return "", err
	}
	for _, entry := range page.Data {
		if entry.ResourceUuid != res.Uuid {
			continue
		}
		for _, d := range entry.Domains {
			if d = strings.TrimSpace(d); d != "" {
				return normalizeDomainURL(d), nil
			}
		}
	}
	return "", fmt.Errorf("%s has no domain on its server — add one in its settings, or use `akerdock app open %s --dashboard`",
		res.Name, res.Name)
}

// normalizeDomainURL turns a domain into something a browser will follow. The
// schema allows the entry to already carry a scheme, a port and a path (§4.2);
// only the scheme is ever missing, and https is what AkerDock serves.
func normalizeDomainURL(domain string) string {
	if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
		return domain
	}
	return "https://" + domain
}
