package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// persistent flags shared by every client command.
type globalFlags struct {
	context string
	output  string // table | json
	quiet   bool
}

var flags globalFlags

// AddCommands registers the client subcommands on the root command (ADR-033).
func AddCommands(root *cobra.Command, version string) {
	pf := root.PersistentFlags()
	pf.StringVar(&flags.context, "context", "", "context to use (default: current, or $AKERDOCK_CONTEXT)")
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
				fmt.Fprint(w, "\t")
			}
			fmt.Fprint(w, h)
		}
		fmt.Fprintln(w)
	}
	for _, row := range rows {
		for i, c := range row {
			if i > 0 {
				fmt.Fprint(w, "\t")
			}
			fmt.Fprint(w, c)
		}
		fmt.Fprintln(w)
	}
	_ = w.Flush()
}
