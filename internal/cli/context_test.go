package cli

import (
	"strings"
	"testing"
)

func setupTwoContexts(t *testing.T) {
	t.Helper()
	setupHome(t)
	cfg := &Config{
		CurrentContext: "prod",
		Contexts: map[string]Context{
			"prod":    {URL: "https://prod.example.com", TeamUUID: "team-p"},
			"staging": {URL: "https://staging.example.com"},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := setToken("prod", "akd_p"); err != nil {
		t.Fatal(err)
	}
}

func TestContextList(t *testing.T) {
	setupTwoContexts(t)
	out, _ := captureOutput(t, func() {
		if err := runCmd(contextCmd(), "list"); err != nil {
			t.Errorf("list: %v", err)
		}
	})
	// The current context is starred; both URLs are shown.
	if !strings.Contains(out, "*") || !strings.Contains(out, "prod.example.com") || !strings.Contains(out, "staging.example.com") {
		t.Fatalf("list output = %q", out)
	}
}

func TestContextListJSON(t *testing.T) {
	setupTwoContexts(t)
	flags.output = "json"
	out, _ := captureOutput(t, func() {
		if err := runCmd(contextCmd(), "list"); err != nil {
			t.Errorf("list: %v", err)
		}
	})
	if !strings.Contains(out, `"CurrentContext": "prod"`) {
		t.Fatalf("json output = %q", out)
	}
}

func TestContextCurrent(t *testing.T) {
	setupTwoContexts(t)
	out, _ := captureOutput(t, func() {
		if err := runCmd(contextCmd(), "current"); err != nil {
			t.Errorf("current: %v", err)
		}
	})
	if strings.TrimSpace(out) != "prod" {
		t.Fatalf("current = %q", out)
	}
}

func TestContextCurrentNoneIsAnError(t *testing.T) {
	setupHome(t)
	if err := runCmd(contextCmd(), "current"); err == nil || !strings.Contains(err.Error(), "no current context") {
		t.Fatalf("err = %v", err)
	}
}

func TestContextUse(t *testing.T) {
	setupTwoContexts(t)
	if err := runCmd(contextCmd(), "use", "ghost"); err == nil || !strings.Contains(err.Error(), `unknown context "ghost"`) {
		t.Fatalf("err = %v", err)
	}
	out, _ := captureOutput(t, func() {
		if err := runCmd(contextCmd(), "use", "staging"); err != nil {
			t.Errorf("use: %v", err)
		}
	})
	if !strings.Contains(out, `switched to "staging"`) {
		t.Fatalf("out = %q", out)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "staging" {
		t.Fatalf("current = %q", cfg.CurrentContext)
	}
}

func TestContextRemove(t *testing.T) {
	setupTwoContexts(t)
	if err := runCmd(contextCmd(), "remove", "ghost"); err == nil || !strings.Contains(err.Error(), `unknown context "ghost"`) {
		t.Fatalf("err = %v", err)
	}
	// Removing the current context clears the pointer and the stored token.
	_, _ = captureOutput(t, func() {
		if err := runCmd(contextCmd(), "remove", "prod"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "" {
		t.Fatalf("current should be cleared, got %q", cfg.CurrentContext)
	}
	if _, ok := cfg.Contexts["prod"]; ok {
		t.Fatal("context should be gone")
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := creds.Tokens["prod"]; ok {
		t.Fatal("token should be gone with the context")
	}
}

// Every subcommand starts by loading the config: a broken $HOME must fail
// loudly rather than acting on an empty config.
func TestContextSubcommandsSurfaceConfigErrors(t *testing.T) {
	setupHome(t)
	t.Setenv("HOME", "")
	for _, args := range [][]string{{"list"}, {"current"}, {"use", "x"}, {"remove", "x"}} {
		if err := runCmd(contextCmd(), args...); err == nil {
			t.Errorf("context %v should fail without a home", args)
		}
	}
}
