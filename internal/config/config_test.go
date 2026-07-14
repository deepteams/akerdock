package config

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func noFile(string) ([]byte, error) { return nil, os.ErrNotExist }

func base() map[string]string {
	return map[string]string{
		"AKERDOCK_DATABASE_URL":    "postgres://akerdock:s3cret@db:5432/akerdock",
		"AKERDOCK_MASTER_KEY_FILE": "/run/secrets/master.key",
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, warnings, err := Load(base(), noFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if cfg.Mode != ModeAllInOne || cfg.Port != 8080 || cfg.Timezone != "UTC" ||
		cfg.LogLevel != "info" || cfg.LogFormat != "json" || cfg.DataDir != "/data/akerdock" ||
		cfg.WorkerConcurrency != 10 || cfg.ShutdownTimeout != 30*time.Second ||
		cfg.LocalhostHost != "host.docker.internal" || cfg.LocalhostUser != "root" {
		t.Fatalf("wrong defaults: %+v", cfg)
	}
}

func TestLocalhostServerVariables(t *testing.T) {
	vars := base()
	vars["AKERDOCK_LOCALHOST_HOST"] = " 172.17.0.1 "
	vars["AKERDOCK_LOCALHOST_USER"] = "deploy"
	cfg, _, err := Load(vars, noFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if cfg.LocalhostHost != "172.17.0.1" || cfg.LocalhostUser != "deploy" {
		t.Fatalf("localhost overrides not applied: %+v", cfg)
	}

	// Blank values must fall back to the defaults, never seed an empty host:
	// a server row with host "" would be unfixable noise.
	vars["AKERDOCK_LOCALHOST_HOST"] = "   "
	vars["AKERDOCK_LOCALHOST_USER"] = ""
	cfg, _, err = Load(vars, noFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if cfg.LocalhostHost != DefaultLocalhostHost || cfg.LocalhostUser != DefaultLocalhostUser {
		t.Fatalf("blank localhost variables must use defaults: %+v", cfg)
	}
}

func TestMissingRequiredCollectsAllErrors(t *testing.T) {
	vars := map[string]string{
		"AKERDOCK_MODE": "workers", // typo'd value
		"AKERDOCK_PORT": "99999",
	}
	_, _, err := Load(vars, noFile)
	var errs Errors
	if !errors.As(err, &errs) {
		t.Fatalf("expected Errors, got %v", err)
	}
	if len(errs) != 4 { // database_url, master key, mode, port
		t.Fatalf("expected 4 collected errors, got %d: %v", len(errs), errs)
	}
}

func TestMasterKeySourceExclusivity(t *testing.T) {
	vars := base()
	vars["AKERDOCK_MASTER_KEY"] = "1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	_, _, err := Load(vars, noFile)
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("expected conflict error, got %v", err)
	}

	delete(vars, "AKERDOCK_MASTER_KEY_FILE")
	_, warnings, err := Load(vars, noFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "prefer AKERDOCK_MASTER_KEY_FILE") {
		t.Fatalf("expected env-key warning, got %v", warnings)
	}
}

func TestRootTrioAllOrNothing(t *testing.T) {
	vars := base()
	vars["AKERDOCK_ROOT_EMAIL"] = "root@example.com"
	_, _, err := Load(vars, noFile)
	if err == nil || !strings.Contains(err.Error(), "all-or-nothing") {
		t.Fatalf("expected trio error, got %v", err)
	}

	vars["AKERDOCK_ROOT_NAME"] = "Root"
	vars["AKERDOCK_ROOT_PASSWORD"] = "short"
	_, _, err = Load(vars, noFile)
	if err == nil || !strings.Contains(err.Error(), "at least 12 characters") {
		t.Fatalf("expected password error, got %v", err)
	}

	vars["AKERDOCK_ROOT_PASSWORD"] = "a-long-enough-password"
	cfg, _, err := Load(vars, noFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if !cfg.HasRootBootstrap() {
		t.Fatal("expected root bootstrap to be detected")
	}
}

func TestConfigFilePrecedence(t *testing.T) {
	vars := base()
	vars["AKERDOCK_CONFIG_FILE"] = "/etc/akerdock.yaml"
	vars["AKERDOCK_PORT"] = "9090" // env wins over file
	readFile := func(path string) ([]byte, error) {
		if path != "/etc/akerdock.yaml" {
			return nil, os.ErrNotExist
		}
		return []byte("port: 8081\nlog_level: debug\n"), nil
	}
	cfg, _, err := Load(vars, readFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if cfg.Port != 9090 {
		t.Fatalf("env must take precedence over file, got port %d", cfg.Port)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("file must take precedence over defaults, got %q", cfg.LogLevel)
	}
}

func TestUnreadableConfigFileIsFatal(t *testing.T) {
	vars := base()
	vars["AKERDOCK_CONFIG_FILE"] = "/nope.yaml"
	_, _, err := Load(vars, noFile)
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("expected fatal unreadable file, got %v", err)
	}
}

func TestUnknownVariableSuggestion(t *testing.T) {
	vars := base()
	vars["AKERDOCK_LOGLEVEL"] = "debug"
	_, warnings, err := Load(vars, noFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "AKERDOCK_LOGLEVEL") && strings.Contains(w, "AKERDOCK_LOG_LEVEL") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected typo suggestion, got %v", warnings)
	}
}

func TestSSLModeDisableWarns(t *testing.T) {
	vars := base()
	vars["AKERDOCK_DATABASE_URL"] = "postgres://u:p@db:5432/akerdock?sslmode=disable"
	_, warnings, err := Load(vars, noFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "sslmode=disable") {
		t.Fatalf("expected sslmode warning, got %v", warnings)
	}
}

func TestInvalidTimezone(t *testing.T) {
	vars := base()
	vars["AKERDOCK_TIMEZONE"] = "Mars/Olympus"
	_, _, err := Load(vars, noFile)
	if err == nil || !strings.Contains(err.Error(), "timezone") {
		t.Fatalf("expected timezone error, got %v", err)
	}
}
