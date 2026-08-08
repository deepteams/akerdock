package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDirConfigInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dirConfigName), []byte("{not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if _, err := loadDirConfig(); err == nil || !strings.Contains(err.Error(), "invalid "+dirConfigName) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadDirConfigUnreadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, dirConfigName)
	if err := os.WriteFile(path, []byte("context: x\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if _, err := loadDirConfig(); err == nil {
		t.Fatal("expected a read error")
	}
}

// settings() is the one-stop resolution every command uses; it must merge the
// global config and the nearest .akerdock.
func TestSettings(t *testing.T) {
	setupHome(t)
	cfg := &Config{
		CurrentContext: "prod",
		Contexts:       map[string]Context{"prod": {URL: "https://p", TeamUUID: "team-p"}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dirConfigName), []byte("application: varuna\ncomponent: web\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	s, err := settings()
	if err != nil {
		t.Fatal(err)
	}
	if s.ContextName != "prod" || s.Team != "team-p" || s.Application != "varuna" || s.Component != "web" {
		t.Fatalf("settings = %+v", s)
	}

	t.Run("config error surfaces", func(t *testing.T) {
		t.Setenv("HOME", "")
		if _, err := settings(); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("dir config error surfaces", func(t *testing.T) {
		broken := t.TempDir()
		if err := os.WriteFile(filepath.Join(broken, dirConfigName), []byte("{not yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(broken)
		if _, err := settings(); err == nil {
			t.Fatal("expected an error")
		}
	})
}
