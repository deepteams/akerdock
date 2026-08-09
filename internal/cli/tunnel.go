package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The bastion's own command (ADR-069). ADR-045 shipped its client work under
// `port-forward`, the verb that already existed; the two products share a
// transport (ADR-064) and a session table and nothing else, so they now share
// no verb either. `open` is this group's bastion-specific act; `ls` and `close`
// are the first CLI clients of two endpoints that shipped with ADR-045 and
// could only be reached from a browser.
func tunnelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Tunnel to a declared external endpoint, and manage open tunnels",
		Long: "Tunnels to targets AkerDock does not run — a managed database, an internal " +
			"API — that an admin declared as an external endpoint (ADR-045). The endpoint " +
			"froze its host and port at declaration, so the only address you choose is the " +
			"local one.\n\n" +
			"For a container AkerDock deploys, use `akerdock port-forward`; to relay a public " +
			"URL to your own machine, `akerdock ingress`.",
	}
	cmd.AddCommand(tunnelOpenCmd(), tunnelLsCmd(), tunnelCloseCmd())
	return cmd
}

func tunnelOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open ENDPOINT [LOCAL_PORT]",
		Short: "Open a tunnel to a declared external endpoint",
		Long: "Opens a TCP tunnel from your machine to a declared external endpoint. " +
			"Without LOCAL_PORT the OS picks a free port, which the CLI announces. A " +
			"`sensitive` endpoint needs a live access grant: the CLI opens the page that " +
			"issues one and keeps replaying the request until it goes through.",
		Example: "  akerdock tunnel open prod-replica          # on a port the OS picks\n" +
			"  akerdock tunnel open prod-replica 15432   # on a port you choose",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := endpointName(args[0])
			if err != nil {
				return err
			}
			var localPort int
			if len(args) == 2 {
				if localPort, err = parseLocalPort(args[1]); err != nil {
					return err
				}
			}
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			res, err := c.resolve(cmd.Context(), ref{kind: "endpoints", name: name})
			if err != nil {
				return err
			}
			return c.runBastionTunnel(cmd.Context(), res.Uuid, localPort)
		},
	}
}

// endpointName rejects the REF spelling this command replaced (ADR-069 §2).
// `tunnel open` accepts exactly one kind of target, so a `type/` prefix carries
// no information — and the hand that types the old form deserves the new one
// rather than "no endpoints named endpoint/prod-replica".
func endpointName(arg string) (string, error) {
	prefix, name, ok := strings.Cut(arg, "/")
	if !ok {
		return arg, nil
	}
	if kind := refKinds[strings.ToLower(prefix)]; kind == "endpoints" && name != "" {
		return "", fmt.Errorf("name the endpoint alone: akerdock tunnel open %s", name)
	}
	return "", fmt.Errorf("invalid endpoint name %q — pass the endpoint's name (e.g. prod-replica)", arg)
}

// runBastionTunnel opens the session ADR-045 mints with an empty body: the
// endpoint froze host and port at declaration, so there is no remote port to
// name and none to announce.
func (c *Client) runBastionTunnel(ctx context.Context, endpointUUID string, localPort int) error {
	return c.openTunnelSession(ctx, "/external-endpoints/"+endpointUUID+"/port-forwards", nil, nil, localPort, 0)
}

// tunnelSession is the subset of PortForwardSessionInfo the CLI renders. The
// mint's token is deliberately absent from that schema and so from here.
type tunnelSession struct {
	Uuid            string     `json:"uuid"`
	TargetKind      string     `json:"target_kind"`
	TargetName      string     `json:"target_name"`
	TargetComponent string     `json:"target_component,omitempty"`
	TargetPort      int        `json:"target_port"`
	UserEmail       string     `json:"user_email,omitempty"`
	ClientIP        string     `json:"client_ip,omitempty"`
	Active          bool       `json:"active"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at"`
	EndReason       string     `json:"end_reason,omitempty"`
}

func tunnelLsCmd() *cobra.Command {
	var endpoint string
	var all bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List the team's tunnel sessions",
		Long: "Every tunnel currently forwarded out of this team, whatever it targets — " +
			"application, database, service, preview or external endpoint. The operational " +
			"question is what is open right now, not what is open on one endpoint (ADR-045).",
		Example: "  akerdock tunnel ls\n" +
			"  akerdock tunnel ls --endpoint prod-replica\n" +
			"  akerdock tunnel ls --all                    # closed sessions too",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			q := url.Values{}
			if all {
				q.Set("active", "false")
			}
			if endpoint != "" {
				res, err := c.resolve(cmd.Context(), ref{kind: "endpoints", name: endpoint})
				if err != nil {
					return err
				}
				q.Set("external_endpoint_uuid", res.Uuid)
			}
			sessions, err := c.listTunnelSessions(cmd.Context(), q)
			if err != nil {
				return err
			}
			if flags.output == "json" {
				return printJSON(sessions)
			}
			rows := make([][]string, 0, len(sessions))
			for _, s := range sessions {
				rows = append(rows, []string{
					tunnelTarget(s), tunnelPort(s), s.UserEmail,
					tunnelState(s), s.CreatedAt.Local().Format("2006-01-02 15:04"), s.Uuid,
				})
			}
			table([]string{"TARGET", "PORT", "USER", "STATE", "OPENED", "UUID"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "restrict to the sessions targeting this declared external endpoint")
	cmd.Flags().BoolVar(&all, "all", false, "walk the history too, not only the sessions still open")
	return cmd
}

// tunnelTarget names what the session points at, kind included: the listing
// spans every target kind, so a bare name would be ambiguous between an app and
// the endpoint of the same name.
func tunnelTarget(s tunnelSession) string {
	target := s.TargetKind + "/" + s.TargetName
	if s.TargetComponent != "" {
		target += ":" + s.TargetComponent
	}
	return target
}

// tunnelPort renders the remote port, which an external endpoint has none of
// that the caller ever named (ADR-045 §2).
func tunnelPort(s tunnelSession) string {
	if s.TargetPort == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", s.TargetPort)
}

// tunnelState folds the three fields that describe a session's life into the
// one word an operator reads: minted but unredeemed, carrying traffic, or over
// — and when it is over, why.
func tunnelState(s tunnelSession) string {
	switch {
	case s.Active && s.StartedAt != nil:
		return "attached"
	case s.Active:
		return "pending"
	case s.EndReason != "":
		return s.EndReason
	default:
		return "closed"
	}
}

// listTunnelSessions follows the cursor pagination of GET /port-forward-sessions.
func (c *Client) listTunnelSessions(ctx context.Context, filters url.Values) ([]tunnelSession, error) {
	var out []tunnelSession
	cursor := ""
	for {
		q := url.Values{"limit": {"100"}}
		for k, vs := range filters {
			q[k] = vs
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Data       []tunnelSession `json:"data"`
			NextCursor *string         `json:"next_cursor"`
		}
		if err := c.do(ctx, http.MethodGet, "/port-forward-sessions", q, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Data...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			return out, nil
		}
		cursor = *page.NextCursor
	}
}

func tunnelCloseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "close SESSION_UUID",
		Short: "Close a live tunnel session",
		Long: "Cuts a tunnel, wherever it was opened from. Closing your own needs nothing " +
			"more than the permission that opened it; closing someone else's is an " +
			"administrative act. Closing an already-closed session is a no-op.",
		Example: "  akerdock tunnel ls\n  akerdock tunnel close 3f2b1c8e-…",
		Args:    usageArgs(1, "tunnel close SESSION_UUID", "tunnel close 3f2b1c8e-0000-4000-8000-000000000000"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(flags.context)
			if err != nil {
				return err
			}
			if err := c.do(cmd.Context(), http.MethodDelete, "/port-forward-sessions/"+args[0], nil, nil, nil); err != nil {
				return err
			}
			if !flags.quiet {
				_, _ = fmt.Fprintln(os.Stdout, "session closed")
			}
			return nil
		},
	}
}
