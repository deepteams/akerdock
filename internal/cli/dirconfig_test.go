package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDirConfigWalksUp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, dirConfigName),
		[]byte("context: prod\napplication: varuna\ncomponent: web\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested) // invoked from deep inside the repo — the file is found by walking up.

	dc, err := loadDirConfig()
	if err != nil {
		t.Fatal(err)
	}
	if dc.Context != "prod" || dc.Application != "varuna" || dc.Component != "web" {
		t.Fatalf("dir config = %#v", dc)
	}
}

func TestLoadDirConfigAbsentIsEmpty(t *testing.T) {
	t.Chdir(t.TempDir())
	dc, err := loadDirConfig()
	if err != nil || dc == nil || dc.Context != "" {
		t.Fatalf("expected an empty dir config, got %#v (err %v)", dc, err)
	}
}

func TestResolveSettingsPrecedence(t *testing.T) {
	cfg := &Config{
		CurrentContext: "global-ctx",
		Contexts: map[string]Context{
			"global-ctx": {URL: "https://g", TeamUUID: "global-team"},
			"dir-ctx":    {URL: "https://d", TeamUUID: "dir-ctx-team"},
		},
	}
	dir := &DirConfig{Context: "dir-ctx", Team: "dir-team", Application: "dir-app", Component: "dir-comp"}

	t.Run(".akerdock overrides global", func(t *testing.T) {
		s := resolveSettings(globalFlags{}, cfg, dir)
		if s.ContextName != "dir-ctx" || s.Team != "dir-team" ||
			s.Application != "dir-app" || s.Component != "dir-comp" {
			t.Fatalf("settings = %#v", s)
		}
	})

	t.Run("env overrides .akerdock", func(t *testing.T) {
		t.Setenv(envContext, "global-ctx")
		t.Setenv(envTeam, "env-team")
		s := resolveSettings(globalFlags{}, cfg, dir)
		if s.ContextName != "global-ctx" || s.Team != "env-team" {
			t.Fatalf("settings = %#v", s)
		}
	})

	t.Run("flags override env", func(t *testing.T) {
		t.Setenv(envContext, "global-ctx")
		t.Setenv(envTeam, "env-team")
		s := resolveSettings(globalFlags{context: "dir-ctx", team: "flag-team", application: "flag-app"}, cfg, dir)
		if s.ContextName != "dir-ctx" || s.Team != "flag-team" || s.Application != "flag-app" {
			t.Fatalf("settings = %#v", s)
		}
	})

	t.Run("team falls back to the context's team_uuid", func(t *testing.T) {
		s := resolveSettings(globalFlags{context: "global-ctx"}, cfg, &DirConfig{})
		if s.Team != "global-team" {
			t.Fatalf("expected the context team as last resort, got %q", s.Team)
		}
	})
}
