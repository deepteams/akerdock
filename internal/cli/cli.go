package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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
		return usageErrorf("usage: akerdock %s\n  example: akerdock %s", usage, example)
	}
}

// usageError marks a failure that lies in what the caller typed, not in what the
// platform did. The spec (§3.2) promises exit code 2 for those and the binary
// answered 1 for everything, so no script could tell "I misspelled the command"
// from "the deployment failed" — the one distinction an exit code exists for.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, a ...any) error {
	return &usageError{msg: fmt.Sprintf(format, a...)}
}

// IsUsageError reports whether err is a usage failure, so the entry point can
// exit 2. Cobra does not type its own parse failures, so the two it produces
// before any of our code runs — an unknown command, and flag errors routed
// through SetFlagErrorFunc in AddCommands — are recognised by prefix here.
func IsUsageError(err error) bool {
	var ue *usageError
	if errors.As(err, &ue) {
		return true
	}
	return err != nil && strings.HasPrefix(err.Error(), "unknown command")
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
	// -a/-e/-p are the short forms every CLI of the domain uses (ADR-070 §4).
	// They carry a *default*: the positional name of a typed verb wins over them.
	pf.StringVarP(&flags.project, "project", "p", "", "default project (default: .akerdock or $AKERDOCK_PROJECT)")
	pf.StringVarP(&flags.application, "application", "a", "", "default application (default: .akerdock or $AKERDOCK_APPLICATION)")
	pf.StringVarP(&flags.environment, "environment", "e", "", "default environment (default: .akerdock or $AKERDOCK_ENVIRONMENT)")
	pf.StringVarP(&flags.output, "output", "o", "table", "output format: table|json")
	pf.BoolVar(&flags.quiet, "quiet", false, "print only essential output")

	// An unknown or malformed flag is a spelling failure, not a platform one.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageErrorf("%v", err)
	})

	// A flag with an enumerated contract must enforce it: `-o bogus` used to fall
	// back to a table and exit 0, which made a scripted `-o json` silently return
	// something no parser could read.
	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		switch flags.output {
		case "table", "json":
			return nil
		default:
			return usageErrorf("invalid --output %q — expected table or json", flags.output)
		}
	}

	root.AddCommand(
		// Typed groups: everything that acts on one kind of resource (ADR-070 §1).
		appGroup(),
		dbGroup(),
		svcGroup(),

		// Transversal: these target no type.
		loginCmd(),
		logoutCmd(),
		contextCmd(),
		whoamiCmd(),
		listCmd(),
		tunnelCmd(),
		ingressCmd(),
		mcpCmd(),
	)
}

// appGroup is the verb space of an application (ADR-070 §1).
func appGroup() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "app",
		Short:   "Applications: deploy, inspect, debug",
		Long:    "Everything an application can do. The name is the last argument and may be omitted when a .akerdock file names a default application.",
		Aliases: []string{"apps", "application"},
	}
	cmd.AddCommand(
		listGroupCmd(kindApp),
		infoCmd(kindApp),
		logsCmd(kindApp),
		shellCmd(kindApp),
		portForwardCmd(kindApp),
		openCmd(),
		deployCmd(kindApp),
		envCmd(kindApp),
		previewCmd(),
		tasksCmd(),
	)
	cmd.AddCommand(lifecycleCmds(kindApp)...)
	return cmd
}

// dbGroup is the verb space of a managed database. No logs: the API serves none
// for a database, and the dashboard has no such view either (ADR-070 §1).
func dbGroup() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "db",
		Short:   "Databases: connect, inspect, back up",
		Aliases: []string{"databases", "database"},
	}
	cmd.AddCommand(
		listGroupCmd(kindDB),
		infoCmd(kindDB),
		shellCmd(kindDB),
		consoleCmd(),
		portForwardCmd(kindDB),
		backupsCmd(),
	)
	cmd.AddCommand(lifecycleCmds(kindDB)...)
	return cmd
}

// svcGroup is the verb space of a compose stack. Narrower than the others
// because the API is: a stack has no logs, no terminal, no port-forward and no
// rollback of its own (ADR-070 §1).
func svcGroup() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "svc",
		Short:   "Compose stacks: deploy, inspect",
		Aliases: []string{"services", "service"},
	}
	cmd.AddCommand(
		listGroupCmd(kindSvc),
		infoCmd(kindSvc),
		deployCmd(kindSvc),
		envCmd(kindSvc),
	)
	cmd.AddCommand(lifecycleCmds(kindSvc)...)
	return cmd
}

// listName spells every listing `list`, with `ls` as a registered alias
// (ADR-070 §4) — the alias exists for the hands that learned the old one.
func listAliases() []string { return []string{"ls"} }

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
