package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The tree is the decision (ADR-070 §1), so it is asserted rather than trusted:
// a verb added to one group does not silently appear on another, a group does
// not grow a verb whose endpoint does not exist, and the two absences that ARE
// decisions stay absences.
func TestCommandTree(t *testing.T) {
	root := &cobra.Command{Use: "akerdock"}
	AddCommands(root, "test")

	want := map[string][]string{
		"app": {"deploy", "env", "info", "list", "logs", "open", "port-forward", "preview", "restart", "shell", "start", "stop", "tasks"},
		// No logs: a database has no logs endpoint, and inventing the verb would
		// move the failure from --help to runtime.
		"db": {"backups", "console", "info", "list", "port-forward", "restart", "shell", "start", "stop"},
		// A compose stack has neither terminal nor port-forward nor logs of its
		// own — its containers are debugged through the application that owns
		// them — and no rollback endpoint.
		"svc": {"deploy", "env", "info", "list", "restart", "start", "stop"},
	}
	for group, verbs := range want {
		cmd := findCommand(t, root, group)
		if got := verbNames(cmd); !slices.Equal(got, verbs) {
			t.Errorf("akerdock %s verbs = %v, want %v", group, got, verbs)
		}
	}

	// Transversal commands target no type and stay at the top level.
	for _, name := range []string{"login", "logout", "context", "whoami", "list", "tunnel", "ingress", "mcp"} {
		findCommand(t, root, name)
	}
}

// Two verbs are absent because ADR-070 §2 decided they should be, not because
// nobody got to them. A test is the only thing that keeps a decision from being
// "completed" by a well-meaning future change.
func TestDecidedAbsences(t *testing.T) {
	root := &cobra.Command{Use: "akerdock"}
	AddCommands(root, "test")

	// Authorizing a fork preview to run is project governance; this CLI is a
	// runtime and debugging tool.
	preview := findCommand(t, findCommand(t, root, "app"), "preview")
	if slices.Contains(verbNames(preview), "approve") {
		t.Error("`app preview approve` must not exist (ADR-070 §2)")
	}

	// Overwriting a production database does not belong behind a one-line
	// terminal confirmation; `download` has no endpoint at all.
	backups := findCommand(t, findCommand(t, root, "db"), "backups")
	for _, forbidden := range []string{"restore", "download"} {
		if slices.Contains(verbNames(backups), forbidden) {
			t.Errorf("`db backups %s` must not exist (ADR-070 §2)", forbidden)
		}
	}

	// Rollback exists for applications only: the endpoint does not exist for a
	// stack, so the verb must not be offered there.
	svcDeploy := findCommand(t, findCommand(t, root, "svc"), "deploy")
	if slices.Contains(verbNames(svcDeploy), "rollback") {
		t.Error("`svc deploy rollback` must not exist — no such endpoint (ADR-070 §1)")
	}
	appDeploy := findCommand(t, findCommand(t, root, "app"), "deploy")
	if !slices.Contains(verbNames(appDeploy), "rollback") {
		t.Error("`app deploy rollback` is missing")
	}
}

// Every listing is spelled `list`, and every one of them answers to `ls` too
// (ADR-070 §4) — the alias is registered, never the displayed name.
func TestListingsAreSpelledListWithLsAliased(t *testing.T) {
	root := &cobra.Command{Use: "akerdock"}
	AddCommands(root, "test")

	var checked int
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			switch sub.Name() {
			case "ls":
				t.Errorf("%q is spelled `ls`; it must be `list` with `ls` as an alias", sub.CommandPath())
			case "list":
				if !slices.Contains(sub.Aliases, "ls") {
					t.Errorf("%q has no `ls` alias", sub.CommandPath())
				}
				checked++
			}
			walk(sub)
		}
	}
	walk(root)
	if checked == 0 {
		t.Fatal("no listing found — the walk is broken, not the tree")
	}
}

// The global short flags of the domain (ADR-070 §4).
func TestGlobalShorthands(t *testing.T) {
	root := &cobra.Command{Use: "akerdock"}
	AddCommands(root, "test")
	for name, short := range map[string]string{"application": "a", "environment": "e", "project": "p", "output": "o"} {
		f := root.PersistentFlags().Lookup(name)
		if f == nil {
			t.Fatalf("--%s is missing", name)
		}
		if f.Shorthand != short {
			t.Errorf("--%s shorthand = %q, want %q", name, f.Shorthand, short)
		}
	}
}

// `-o` carries an enumerated contract, and a contract that is not enforced is a
// comment: an invalid value used to fall back to a table and exit 0, which made
// a scripted `-o json` return something no parser could read.
func TestOutputFlagIsValidated(t *testing.T) {
	root := &cobra.Command{Use: "akerdock", SilenceUsage: true, SilenceErrors: true}
	AddCommands(root, "test")
	t.Cleanup(func() { flags.output = "table" })

	root.SetArgs([]string{"list", "-o", "bogus"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "expected table or json") {
		t.Fatalf("err = %v, want a refusal naming the accepted values", err)
	}
	if !IsUsageError(err) {
		t.Error("an invalid flag value is a usage failure (exit code 2), not a platform one")
	}
}

// The exit-code contract of the spec (§3.2), which the binary never honoured.
func TestUsageErrorsAreTyped(t *testing.T) {
	root := &cobra.Command{Use: "akerdock", SilenceUsage: true, SilenceErrors: true}
	AddCommands(root, "test")

	t.Run("unknown command", func(t *testing.T) {
		root.SetArgs([]string{"nosuchcommand"})
		if err := root.Execute(); !IsUsageError(err) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		root.SetArgs([]string{"list", "--nosuchflag"})
		if err := root.Execute(); !IsUsageError(err) {
			t.Fatalf("err = %v", err)
		}
	})

	// A target the caller spelled wrong is theirs to fix, so it owes the same
	// exit code as an unknown command — that is the whole point of having two.
	t.Run("the removed REF form", func(t *testing.T) {
		setupHome(t)
		_, err := targetName(kindApp, []string{"app/varuna"})
		if !IsUsageError(err) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a missing name with no default", func(t *testing.T) {
		setupHome(t)
		_, err := targetName(kindDB, nil)
		if !IsUsageError(err) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a platform failure is not a usage failure", func(t *testing.T) {
		if IsUsageError(errPlatform) {
			t.Fatal("an ordinary error must not be reported as a usage error")
		}
	})
}

var errPlatform = &apiError{Message: "the server is unreachable"}

// findCommand fails the test rather than returning nil: every caller here is
// asserting the tree, so a missing command is the failure, not a nil deref.
func findCommand(t *testing.T, parent *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, sub := range parent.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	t.Fatalf("%s has no %q command", parent.CommandPath(), name)
	return nil
}

func verbNames(cmd *cobra.Command) []string {
	var names []string
	for _, sub := range cmd.Commands() {
		if sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		names = append(names, sub.Name())
	}
	slices.Sort(names)
	return names
}
