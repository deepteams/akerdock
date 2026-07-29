package config

import (
	"errors"
	"net/netip"
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
		cfg.LogLevel != "info" || cfg.LogFormat != "json" || cfg.DataDir != "/var/lib/akerdock" ||
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

func TestAllSupportedOverrides(t *testing.T) {
	vars := base()
	for key, value := range map[string]string{
		"AKERDOCK_MODE":                  "worker",
		"AKERDOCK_PORT":                  "9443",
		"AKERDOCK_ROOT_EMAIL":            "root@example.com",
		"AKERDOCK_ROOT_NAME":             "  Platform Owner  ",
		"AKERDOCK_ROOT_PASSWORD":         "a-long-enough-password",
		"AKERDOCK_TIMEZONE":              "Europe/Paris",
		"AKERDOCK_LOG_LEVEL":             "warn",
		"AKERDOCK_LOG_FORMAT":            "text",
		"AKERDOCK_DATA_DIR":              "/srv/akerdock",
		"AKERDOCK_WORKER_CONCURRENCY":    "24",
		"AKERDOCK_SHUTDOWN_TIMEOUT":      "45s",
		"AKERDOCK_SCHEDULER_TICK":        "2s",
		"AKERDOCK_RETRY_BASE":            "750ms",
		"AKERDOCK_TERMINAL_IDLE_TIMEOUT": "20m",
		"AKERDOCK_TERMINAL_MAX_DURATION": "6h",
	} {
		vars[key] = value
	}

	cfg, _, err := Load(vars, noFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if cfg.Mode != ModeWorker || cfg.Port != 9443 || cfg.RootName != "Platform Owner" ||
		cfg.Timezone != "Europe/Paris" || cfg.LogLevel != "warn" || cfg.LogFormat != "text" ||
		cfg.DataDir != "/srv/akerdock" || cfg.WorkerConcurrency != 24 ||
		cfg.ShutdownTimeout != 45*time.Second || cfg.SchedulerTick != 2*time.Second ||
		cfg.RetryBase != 750*time.Millisecond || cfg.TerminalIdleTimeout != 20*time.Minute ||
		cfg.TerminalMaxDuration != 6*time.Hour {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestInvalidOptionalValuesAreCollected(t *testing.T) {
	vars := base()
	for key, value := range map[string]string{
		"AKERDOCK_MODE":                  "sidecar",
		"AKERDOCK_PORT":                  "not-a-port",
		"AKERDOCK_ROOT_EMAIL":            "not an email",
		"AKERDOCK_ROOT_NAME":             strings.Repeat("x", 256),
		"AKERDOCK_ROOT_PASSWORD":         "too-short",
		"AKERDOCK_TIMEZONE":              "Mars/Olympus",
		"AKERDOCK_LOG_LEVEL":             "verbose",
		"AKERDOCK_LOG_FORMAT":            "xml",
		"AKERDOCK_WORKER_CONCURRENCY":    "0",
		"AKERDOCK_SHUTDOWN_TIMEOUT":      "0s",
		"AKERDOCK_SCHEDULER_TICK":        "never",
		"AKERDOCK_RETRY_BASE":            "-1s",
		"AKERDOCK_TERMINAL_IDLE_TIMEOUT": "0",
		"AKERDOCK_TERMINAL_MAX_DURATION": "-2h",
	} {
		vars[key] = value
	}

	_, _, err := Load(vars, noFile)
	var errs Errors
	if !errors.As(err, &errs) {
		t.Fatalf("expected an exhaustive Errors value, got %v", err)
	}
	if len(errs) != 14 {
		t.Fatalf("expected all 14 invalid values, got %d: %v", len(errs), errs)
	}
}

func TestInvalidConfigYAML(t *testing.T) {
	vars := base()
	vars["AKERDOCK_CONFIG_FILE"] = "/etc/akerdock.yaml"
	_, _, err := Load(vars, func(string) ([]byte, error) {
		return []byte("port: ["), nil
	})
	if err == nil || !strings.Contains(err.Error(), "invalid YAML") {
		t.Fatalf("malformed YAML should be fatal, got %v", err)
	}
}

func TestUnknownVariableWithoutSuggestion(t *testing.T) {
	vars := base()
	vars["AKERDOCK_COMPLETELY_UNRELATED"] = "x"
	_, warnings, err := Load(vars, noFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || strings.Contains(warnings[0], "did you mean") {
		t.Fatalf("distant variables should be warned without a misleading suggestion: %v", warnings)
	}
}

// The 00-control-plane route must target the port published ON THE HOST
// (proxy-contract §5.7). Under compose the process listens on 8080 while the
// host publishes AKERDOCK_PORT — the compose passes the published value via
// AKERDOCK_INSTANCE_PORT; a binary on the host simply inherits Port.
func TestInstancePort(t *testing.T) {
	cfg, _, err := Load(base(), noFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if cfg.InstancePort != cfg.Port {
		t.Fatalf("InstancePort must default to Port, got %d vs %d", cfg.InstancePort, cfg.Port)
	}

	vars := base()
	vars["AKERDOCK_PORT"] = "9443"
	cfg, _, err = Load(vars, noFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if cfg.InstancePort != 9443 {
		t.Fatalf("InstancePort must follow an overridden Port, got %d", cfg.InstancePort)
	}

	vars["AKERDOCK_INSTANCE_PORT"] = "9995"
	cfg, _, err = Load(vars, noFile)
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if cfg.InstancePort != 9995 || cfg.Port != 9443 {
		t.Fatalf("AKERDOCK_INSTANCE_PORT must win without touching Port: %+v", cfg)
	}

	vars["AKERDOCK_INSTANCE_PORT"] = "not-a-port"
	if _, _, err := Load(vars, noFile); err == nil || !strings.Contains(err.Error(), "AKERDOCK_INSTANCE_PORT") {
		t.Fatalf("an invalid AKERDOCK_INSTANCE_PORT must be a named fatal error, got %v", err)
	}
}

// The list an operator actually writes: a bare proxy address, a CIDR, and the
// `private` shorthand for "the bridge network this thing sits on", whose exact
// prefix nobody should have to look up.
func TestTrustedProxies(t *testing.T) {
	cases := map[string]struct {
		value   string
		want    []string // addresses that must be recognised as a proxy
		reject  []string // and those that must not
		invalid bool
	}{
		"a bare address":    {value: "172.18.0.1", want: []string{"172.18.0.1"}, reject: []string{"172.18.0.2"}},
		"a CIDR":            {value: "172.18.0.0/16", want: []string{"172.18.9.9"}, reject: []string{"203.0.113.7"}},
		"a mixed list":      {value: "10.0.0.0/8, 192.168.1.5", want: []string{"10.1.2.3", "192.168.1.5"}, reject: []string{"192.168.1.6"}},
		"private":           {value: "private", want: []string{"172.18.0.1", "10.0.0.5", "127.0.0.1", "::1"}, reject: []string{"203.0.113.7"}},
		"nonsense":          {value: "not-an-ip", invalid: true},
		"one bad entry":     {value: "10.0.0.0/8,nope", invalid: true},
		"absent by default": {value: "", reject: []string{"172.18.0.1", "127.0.0.1"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			vars := base()
			if tc.value != "" {
				vars["AKERDOCK_TRUSTED_PROXIES"] = tc.value
			}
			cfg, _, err := Load(vars, noFile)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "AKERDOCK_TRUSTED_PROXIES") {
					t.Fatalf("expected a trusted-proxies error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			contains := func(s string) bool {
				addr := netip.MustParseAddr(s)
				for _, p := range cfg.TrustedProxies {
					if p.Contains(addr) {
						return true
					}
				}
				return false
			}
			for _, s := range tc.want {
				if !contains(s) {
					t.Errorf("%s should be recognised as a trusted proxy", s)
				}
			}
			for _, s := range tc.reject {
				if contains(s) {
					t.Errorf("%s must NOT be trusted to speak for someone else", s)
				}
			}
		})
	}
}
