package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// usageArgs enforces an exact argument count with an actionable message and an
// example, instead of Cobra's terse "accepts N arg(s), received M".
func usageArgs(n int, usage, example string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) == n {
			return nil
		}
		return fmt.Errorf("usage: akerdock %s\n  example: akerdock %s", usage, example)
	}
}

// persistent flags shared by every client command. context/team and the target
// defaults each override the .akerdock file and the global config (spec §4:
// flags > env > .akerdock > global).
type globalFlags struct {
	context     string
	team        string
	project     string
	application string
	environment string
	component   string
	output      string // table | json
	quiet       bool
}

var flags globalFlags

// AddCommands registers the client subcommands on the root command (ADR-033).
func AddCommands(root *cobra.Command, _ string) {
	pf := root.PersistentFlags()
	pf.StringVar(&flags.context, "context", "", "context to use (default: .akerdock or current, or $AKERDOCK_CONTEXT)")
	// Not a team switch: a token is bound to its team at creation (rbac-matrix
	// §4.1), so every command acts in the token's team. This only tells
	// `logout --revoke` where to look for the token to delete.
	pf.StringVar(&flags.team, "team", "", "team of the token, used by logout --revoke (a token is bound to its team: this does not switch teams)")
	pf.StringVar(&flags.project, "project", "", "default project (default: .akerdock or $AKERDOCK_PROJECT)")
	pf.StringVar(&flags.application, "application", "", "default application/target (default: .akerdock or $AKERDOCK_APPLICATION)")
	pf.StringVar(&flags.environment, "environment", "", "default environment (default: .akerdock or $AKERDOCK_ENVIRONMENT)")
	pf.StringVarP(&flags.output, "output", "o", "table", "output format: table|json")
	pf.BoolVar(&flags.quiet, "quiet", false, "print only essential output")

	root.AddCommand(
		loginCmd(),
		logoutCmd(),
		contextCmd(),
		lsCmd(),
		logsCmd(),
		shellCmd(),
		portForwardCmd(),
		dbCmd(),
		mcpCmd(),
	)
}

// printJSON writes v as indented JSON to stdout.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table renders rows as an aligned table with a header. Used for the human
// `-o table` output; `-o json` bypasses it.
func table(header []string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	if len(header) > 0 && !flags.quiet {
		for i, h := range header {
			if i > 0 {
				_, _ = fmt.Fprint(w, "\t")
			}
			_, _ = fmt.Fprint(w, h)
		}
		_, _ = fmt.Fprintln(w)
	}
	for _, row := range rows {
		for i, c := range row {
			if i > 0 {
				_, _ = fmt.Fprint(w, "\t")
			}
			_, _ = fmt.Fprint(w, c)
		}
		_, _ = fmt.Fprintln(w)
	}
	_ = w.Flush()
}
