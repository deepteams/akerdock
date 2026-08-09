package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// whoamiInfo is what identifies the invocation about to be made: which
// instance, as which team, with a credential or without one.
//
// There is deliberately no token field, and no scopes field either. The token
// is never printed — a `whoami` pasted into an issue must stay pasteable — and
// the field's absence is what guarantees it rather than a formatting rule
// somewhere below. Scopes are not here because they are not stored: `login`
// *requests* them (--scopes) but only the token comes back and only the token
// is written to credentials.yaml, so printing them would mean an HTTP call —
// the one thing this command promises not to make (ADR-070 §2).
type whoamiInfo struct {
	Context       string `json:"context"`
	URL           string `json:"url"`
	Fqdn          string `json:"fqdn,omitempty"`
	TeamUUID      string `json:"team_uuid,omitempty"`
	Authenticated bool   `json:"authenticated"`
}

// whoamiCmd answers "where am I pointed, as whom" — the question worth asking
// before `stop`, and worthless if it can only be answered while the network is
// up. Everything it prints comes from ~/.akerdock and the .akerdock file, so it
// works on a plane and against an instance that is down, which is exactly when
// someone is about to type something destructive.
func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the active context: instance, team, credential (no network call)",
		Long: "Prints the context the next command would use, resolved through the full " +
			"precedence chain (--context, $AKERDOCK_CONTEXT, .akerdock, then the global " +
			"config).\n\n" +
			"Local only: no request is made, so the answer is the same whether the instance " +
			"is reachable or not. The token itself is never printed.",
		Example: "  akerdock whoami\n  akerdock whoami -o json",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			s, err := settings()
			if err != nil {
				return err
			}
			if s.ContextName == "" {
				return fmt.Errorf("no context selected — run `akerdock login`, or set `context:` in a .akerdock file")
			}
			ctx, ok := cfg.Contexts[s.ContextName]
			if !ok {
				return fmt.Errorf("unknown context %q — see `akerdock context list`", s.ContextName)
			}
			creds, err := loadCredentials()
			if err != nil {
				return err
			}
			info := whoamiInfo{
				Context:       s.ContextName,
				URL:           ctx.URL,
				Fqdn:          ctx.Fqdn,
				TeamUUID:      s.Team,
				Authenticated: creds.Tokens[s.ContextName] != "",
			}
			if flags.output == "json" {
				return printJSON(info)
			}
			if flags.quiet {
				_, _ = fmt.Fprintln(os.Stdout, info.Context)
				return nil
			}
			rows := [][]string{
				{"context", info.Context},
				{"instance", info.URL},
			}
			if info.Fqdn != "" {
				rows = append(rows, []string{"fqdn", info.Fqdn})
			}
			rows = append(rows,
				[]string{"team", whoamiTeam(info.TeamUUID)},
				[]string{"credential", whoamiCredential(info)},
			)
			table(nil, rows)
			return nil
		},
	}
}

// whoamiTeam names the effective team. Empty is a real state, not a bug: a
// token whose account has one team was stored without a team_uuid and acts in
// the server's default — saying so beats printing a blank cell.
func whoamiTeam(uuid string) string {
	if uuid == "" {
		return "(the token's default team)"
	}
	return uuid
}

// whoamiCredential reports the presence of a token, never its value, and turns
// its absence into the command that fixes it.
func whoamiCredential(info whoamiInfo) string {
	if info.Authenticated {
		return "stored"
	}
	return fmt.Sprintf("none — run `akerdock login --context %s`", info.Context)
}
