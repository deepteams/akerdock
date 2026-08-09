package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"
)

func contextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Manage the instances the CLI talks to (one context = one instance + its token's team)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: listAliases(),
			Short:   "List configured contexts",
			RunE: func(_ *cobra.Command, _ []string) error {
				cfg, err := LoadConfig()
				if err != nil {
					return err
				}
				if flags.output == "json" {
					return printJSON(cfg)
				}
				names := make([]string, 0, len(cfg.Contexts))
				for n := range cfg.Contexts {
					names = append(names, n)
				}
				sort.Strings(names)
				rows := make([][]string, 0, len(names))
				for _, n := range names {
					marker := ""
					if n == cfg.CurrentContext {
						marker = "*"
					}
					rows = append(rows, []string{marker, n, cfg.Contexts[n].URL, cfg.Contexts[n].TeamUUID})
				}
				table([]string{"", "NAME", "URL", "TEAM"}, rows)
				return nil
			},
		},
		&cobra.Command{
			Use:   "current",
			Short: "Print the current context",
			RunE: func(_ *cobra.Command, _ []string) error {
				cfg, err := LoadConfig()
				if err != nil {
					return err
				}
				if cfg.CurrentContext == "" {
					return fmt.Errorf("no current context — run `akerdock login`")
				}
				fmt.Println(cfg.CurrentContext)
				return nil
			},
		},
		&cobra.Command{
			Use:   "use NAME",
			Short: "Switch the current context",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				cfg, err := LoadConfig()
				if err != nil {
					return err
				}
				if _, ok := cfg.Contexts[args[0]]; !ok {
					return fmt.Errorf("unknown context %q", args[0])
				}
				cfg.CurrentContext = args[0]
				if err := cfg.Save(); err != nil {
					return err
				}
				fmt.Printf("switched to %q\n", args[0])
				return nil
			},
		},
		&cobra.Command{
			Use:   "remove NAME",
			Short: "Remove a context and its stored token",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				cfg, err := LoadConfig()
				if err != nil {
					return err
				}
				if _, ok := cfg.Contexts[args[0]]; !ok {
					return fmt.Errorf("unknown context %q", args[0])
				}
				delete(cfg.Contexts, args[0])
				if cfg.CurrentContext == args[0] {
					cfg.CurrentContext = ""
				}
				if err := cfg.Save(); err != nil {
					return err
				}
				if err := setToken(args[0], ""); err != nil {
					return err
				}
				fmt.Printf("removed %q\n", args[0])
				return nil
			},
		},
	)
	return cmd
}
