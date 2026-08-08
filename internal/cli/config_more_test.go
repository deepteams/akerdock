package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigInvalidYAML(t *testing.T) {
	setupHome(t)
	dir, err := configDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte("{not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "invalid "+configFileName) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadConfigUnreadableFile(t *testing.T) {
	setupHome(t)
	dir, err := configDir()
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the file should be: ReadFile fails with a non-NotExist
	// error that must be surfaced, not treated as "no config yet".
	if err := os.MkdirAll(filepath.Join(dir, configFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected a read error")
	}
}

func TestLoadConfigNilContextsBecomesEmptyMap(t *testing.T) {
	setupHome(t)
	dir, _ := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte("current_context: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Contexts == nil {
		t.Fatal("Contexts must never be nil")
	}
}

func TestLoadCredentialsErrors(t *testing.T) {
	setupHome(t)
	dir, _ := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, credsFileName), []byte("{not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCredentials(); err == nil || !strings.Contains(err.Error(), "invalid "+credsFileName) {
		t.Fatalf("err = %v", err)
	}

	// A file declaring `tokens: null` still yields a usable map.
	if err := os.WriteFile(filepath.Join(dir, credsFileName), []byte("tokens: null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if creds.Tokens == nil {
		t.Fatal("Tokens must never be nil")
	}

	// Unreadable file (a directory in its place).
	if err := os.Remove(filepath.Join(dir, credsFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, credsFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCredentials(); err == nil {
		t.Fatal("expected a read error")
	}
}

func TestSetTokenClears(t *testing.T) {
	setupHome(t)
	if err := setToken("prod", "akd_1"); err != nil {
		t.Fatal(err)
	}
	if err := setToken("prod", ""); err != nil {
		t.Fatal(err)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := creds.Tokens["prod"]; ok {
		t.Fatal("token should be gone")
	}
}

// With no $HOME there is no config directory: every accessor must fail with
// the actionable "cannot locate the home directory" instead of guessing.
func TestConfigNoHome(t *testing.T) {
	setupHome(t)
	t.Setenv("HOME", "")
	if _, err := configDir(); err == nil {
		t.Fatal("configDir should fail")
	}
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig should fail")
	}
	if err := (&Config{}).Save(); err == nil {
		t.Fatal("Save should fail")
	}
	if _, err := loadCredentials(); err == nil {
		t.Fatal("loadCredentials should fail")
	}
	if err := setToken("x", "y"); err == nil {
		t.Fatal("setToken should fail")
	}
}

// $HOME pointing at a regular file: MkdirAll cannot create ~/.akerdock, and
// both save paths must report it.
func TestSaveMkdirFails(t *testing.T) {
	setupHome(t)
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", file)
	if err := (&Config{}).Save(); err == nil {
		t.Fatal("Save should fail")
	}
	if err := (&credentials{Tokens: map[string]string{}}).save(); err == nil {
		t.Fatal("credentials save should fail")
	}
}

func TestResolveContextNameEnv(t *testing.T) {
	setupHome(t)
	cfg := &Config{CurrentContext: "from-config"}
	if got := cfg.resolveContextName("from-flag"); got != "from-flag" {
		t.Fatalf("flag should win, got %q", got)
	}
	t.Setenv(envContext, "  from-env  ")
	if got := cfg.resolveContextName(""); got != "from-env" {
		t.Fatalf("env should win over config, got %q", got)
	}
	t.Setenv(envContext, "")
	if got := cfg.resolveContextName(""); got != "from-config" {
		t.Fatalf("config is the fallback, got %q", got)
	}
}
