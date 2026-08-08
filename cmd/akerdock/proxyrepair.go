package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/deepteams/akerdock/internal/config"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/logredact"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/postgres"
	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/serverdial"
	"github.com/deepteams/akerdock/internal/store"
)

// proxyCommand is the break-glass side of ADR-062: the dashboard administers
// the proxy, and on the server that hosts the instance the proxy serves the
// dashboard. When that circle breaks, every in-product path is closed by
// construction — passkey and OIDC sign-in are bound to the FQDN the proxy
// routes, so even a port-forward to the control plane authenticates nobody.
// This runs where the control plane runs, on the instance's own configuration.
func proxyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Operate a server's managed proxy from the host (recovery path)",
	}
	var target string
	repair := &cobra.Command{
		Use:   "repair",
		Short: "Converge a server's proxy without the dashboard",
		Long: "Render the proxy's static configuration and converge its container over SSH, " +
			"using this instance's own configuration (database and master key) — no API session, " +
			"no agent channel. This is the way back when the dashboard is unreachable because the " +
			"proxy that serves it is down.\n\n" +
			"Without --server, it repairs the proxy of the server that hosts this instance.",
		Args:    cobra.NoArgs,
		Example: "  akerdock proxy repair\n  akerdock proxy repair --server prod-eu",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return proxyRepairRun(cmd.Context(), target)
		},
	}
	repair.Flags().StringVar(&target, "server", "",
		"Server to repair, by UUID or name (default: the server hosting this instance)")
	cmd.AddCommand(repair)
	return cmd
}

func proxyRepairRun(ctx context.Context, target string) error {
	cfg, _, err := config.Load(environMap(), os.ReadFile)
	if err != nil {
		return err
	}
	logger := slog.New(logredact.Wrap(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	// No migration here: a repair converges one container, it does not carry
	// the instance forward a schema version while the operator is locked out.
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	keyring, err := loadKeyring(cfg, logger)
	if err != nil {
		return err
	}

	q := store.New(pool)
	servers, err := q.ListServersWithProxy(ctx)
	if err != nil {
		return err
	}
	server, err := pickProxyServer(servers, target)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "repairing the proxy of %s (%s)...\n", server.Name, server.Host)
	client, err := serverdial.Open(ctx, q, keyring, server)
	if err != nil {
		return fmt.Errorf("SSH to %s: %w", server.Host, err)
	}
	defer func() { _ = client.Close() }()

	if err := jobs.RepairProxy(ctx, q, keyring, client, server, cfg.Port); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s is running on %s — sign in again at the dashboard.\n",
		proxy.ContainerName, server.Host)
	return nil
}

// pickProxyServer resolves --server, or falls back to the server hosting this
// instance: on a single-server install that is the only sensible target, and
// it is the one whose proxy can lock the operator out.
func pickProxyServer(servers []store.Server, target string) (store.Server, error) {
	if target == "" {
		for _, server := range servers {
			if server.IsLocalhost {
				return server, nil
			}
		}
		return store.Server{}, fmt.Errorf("no server hosts this instance — name one with --server (%s)",
			strings.Join(proxyServerNames(servers), ", "))
	}
	for _, server := range servers {
		if strings.EqualFold(server.Name, target) || pguuid.String(server.Uuid) == target {
			return server, nil
		}
	}
	return store.Server{}, fmt.Errorf("no server named %q with a managed proxy (%s)",
		target, strings.Join(proxyServerNames(servers), ", "))
}

func proxyServerNames(servers []store.Server) []string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, server.Name)
	}
	if len(names) == 0 {
		return []string{"this instance manages no proxy"}
	}
	return names
}
