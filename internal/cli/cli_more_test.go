package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestUsageArgs(t *testing.T) {
	check := usageArgs(2, "ingress ENDPOINT LOCAL_PORT", "ingress dev 3000")
	if err := check(nil, []string{"a", "b"}); err != nil {
		t.Fatalf("exact count should pass, got %v", err)
	}
	err := check(nil, []string{"a"})
	if err == nil {
		t.Fatal("wrong count must fail")
	}
	// The message must teach, not just refuse: usage line plus a runnable example.
	if !strings.Contains(err.Error(), "usage: akerdock ingress") || !strings.Contains(err.Error(), "example: akerdock ingress dev 3000") {
		t.Fatalf("unhelpful message: %v", err)
	}
}

func TestAddCommandsRegistersEverySubcommand(t *testing.T) {
	resetFlags(t)
	root := &cobra.Command{Use: "akerdock"}
	AddCommands(root, "test")
	want := []string{"login", "logout", "context", "whoami", "list", "app", "db", "svc", "tunnel", "ingress", "mcp"}
	for _, name := range want {
		found := false
		for _, c := range root.Commands() {
			if c.Name() == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("subcommand %q not registered", name)
		}
	}
	for _, flag := range []string{"context", "team", "project", "application", "environment", "output", "quiet"} {
		if root.PersistentFlags().Lookup(flag) == nil {
			t.Errorf("persistent flag %q not registered", flag)
		}
	}
}

func TestPrintJSON(t *testing.T) {
	out, _ := captureOutput(t, func() {
		if err := printJSON(map[string]string{"name": "varuna"}); err != nil {
			t.Errorf("printJSON: %v", err)
		}
	})
	if !strings.Contains(out, `"name": "varuna"`) {
		t.Fatalf("output = %q", out)
	}
	// An unencodable value must surface as an error, not a silent half-write.
	if err := printJSON(make(chan int)); err == nil {
		t.Fatal("expected an encoding error")
	}
}

func TestTable(t *testing.T) {
	resetFlags(t)
	out, _ := captureOutput(t, func() {
		table([]string{"KIND", "NAME"}, [][]string{{"apps", "varuna"}, {"databases", "pg"}})
	})
	if !strings.Contains(out, "KIND") || !strings.Contains(out, "varuna") || !strings.Contains(out, "pg") {
		t.Fatalf("table output = %q", out)
	}

	// --quiet drops the header, keeps the data: the output stays scriptable.
	flags.quiet = true
	out, _ = captureOutput(t, func() {
		table([]string{"KIND", "NAME"}, [][]string{{"apps", "varuna"}})
	})
	if strings.Contains(out, "KIND") {
		t.Fatalf("quiet output still has a header: %q", out)
	}
	if !strings.Contains(out, "varuna") {
		t.Fatalf("quiet output lost the data: %q", out)
	}
}
