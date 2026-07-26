// Package config loads and validates the instance configuration following
// docs/specs/instance-config.md: environment variables take precedence over
// an optional YAML file (AKERDOCK_CONFIG_FILE), which takes precedence over
// compiled defaults. Errors are collected exhaustively (§7.1) so the operator
// can fix everything in one cycle.
package config

import (
	"fmt"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // IANA timezone validation inside distroless images

	"gopkg.in/yaml.v3"
)

// Mode is the modular-monolith run mode (instance-config §2.1).
type Mode string

// Run modes: all-in-one serves the API and consumes the queue, api serves
// HTTP only, worker consumes the queue only, scheduler runs crons under a
// PostgreSQL advisory lock.
const (
	ModeAllInOne  Mode = "all-in-one"
	ModeAPI       Mode = "api"
	ModeWorker    Mode = "worker"
	ModeScheduler Mode = "scheduler"
)

// Defaults (instance-config §2.1, "Défaut" column).
const (
	DefaultPort              = 8080
	DefaultTimezone          = "UTC"
	DefaultLogLevel          = "info"
	DefaultLogFormat         = "json"
	DefaultDataDir           = "/var/lib/akerdock"
	DefaultWorkerConcurrency = 10
	// DefaultSchedulerTick is how often the scheduler looks for due work: cron
	// backups, expiring certificates, notifications. Lower means more reactive
	// and more idle queries; the E2E suite lowers it so its assertions do not
	// spend most of their time waiting for the next tick.
	DefaultSchedulerTick = 30 * time.Second
	// DefaultRetryBase is the first retry delay of a failed job; it doubles at
	// each attempt, capped and jittered (§22.1). Lowering it stops a suite that
	// deliberately fails jobs from waiting on arithmetic.
	DefaultRetryBase       = 5 * time.Second
	DefaultShutdownTimeout = 30 * time.Second
	// DefaultTerminalIdleTimeout / DefaultTerminalMaxDuration bound web
	// terminal sessions (§24.4): idle means no keystroke, the max duration
	// applies regardless of activity. Both configurable — a forgotten root
	// shell is the failure mode these exist for.
	DefaultTerminalIdleTimeout = 15 * time.Minute
	DefaultTerminalMaxDuration = 4 * time.Hour
	// DefaultLocalhostHost is where the pre-registered localhost server points
	// (§6.2): the compose distribution resolves it to the host gateway via
	// extra_hosts, and Docker Desktop resolves it natively.
	DefaultLocalhostHost = "host.docker.internal"
	DefaultLocalhostUser = "root"
)

// Config holds the resolved instance configuration.
type Config struct {
	DatabaseURL   string
	MasterKeyFile string
	MasterKey     string
	Mode          Mode
	Port          int
	// InstancePort is the port at which the instance is reachable ON ITS HOST
	// — what the 00-control-plane proxy route must target (proxy-contract
	// §5.7). It differs from Port under the compose distribution, where the
	// host mapping is `${AKERDOCK_PORT}:8080` and the process itself always
	// listens on 8080. Defaults to Port, which is correct for a binary
	// running directly on the host.
	InstancePort int
	InstanceFQDN string
	// ACMEEmail is the Let's Encrypt contact (§4.3). Seeded here, then owned by
	// instance_settings: an issuance failure is silent, so the address must be
	// a deliberate choice, never a guess.
	SchedulerTick     time.Duration
	RetryBase         time.Duration
	ACMEEmail         string
	RootEmail         string
	RootName          string
	RootPassword      string
	// LocalhostHost/LocalhostUser seed the pre-registered localhost server at
	// first bootstrap (§6.2); afterwards the server row is authoritative.
	LocalhostHost     string
	LocalhostUser     string
	Timezone          string
	LogLevel          string
	LogFormat         string
	DataDir           string
	WorkerConcurrency int
	ShutdownTimeout   time.Duration
	// Terminal session bounds (§24.4, ADR-024).
	TerminalIdleTimeout time.Duration
	TerminalMaxDuration time.Duration
	// Image is this AkerDock release's own container image (ADR-036): the
	// scale-to-zero waker is deployed as a helper container from it (same binary,
	// `akerdock waker` mode). AKERDOCK_IMAGE sets it explicitly; on a release
	// build it otherwise falls back (in main) to the image baked in via
	// -ldflags. Empty everywhere disables waker provisioning — scale-to-zero then
	// stays inert with a clear error, never a guessed registry.
	Image string
}

// HasRootBootstrap reports whether the AKERDOCK_ROOT_* trio was provided.
func (c *Config) HasRootBootstrap() bool {
	return c.RootEmail != "" || c.RootName != "" || c.RootPassword != ""
}

// FieldError is one fatal configuration error, attached to a variable name.
type FieldError struct {
	Var string
	Msg string
}

// Errors is the exhaustive list of fatal configuration errors (§7.1).
type Errors []FieldError

func (e Errors) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d error(s)):", len(e))
	for _, fe := range e {
		fmt.Fprintf(&b, "\n  - %s: %s", fe.Var, fe.Msg)
	}
	return b.String()
}

// envKeys are the variables read by the binary (instance-config §2.1).
// YAML file keys are the same names in snake_case without the prefix.
var envKeys = []string{
	"AKERDOCK_DATABASE_URL",
	"AKERDOCK_MASTER_KEY_FILE",
	"AKERDOCK_MASTER_KEY",
	"AKERDOCK_MODE",
	"AKERDOCK_PORT",
	"AKERDOCK_INSTANCE_PORT",
	"AKERDOCK_INSTANCE_FQDN",
	"AKERDOCK_ACME_EMAIL",
	"AKERDOCK_SCHEDULER_TICK",
	"AKERDOCK_RETRY_BASE",
	"AKERDOCK_ROOT_EMAIL",
	"AKERDOCK_ROOT_NAME",
	"AKERDOCK_ROOT_PASSWORD",
	"AKERDOCK_LOCALHOST_HOST",
	"AKERDOCK_LOCALHOST_USER",
	"AKERDOCK_GITHUB_CA_FILE",
	"AKERDOCK_TIMEZONE",
	"AKERDOCK_LOG_LEVEL",
	"AKERDOCK_LOG_FORMAT",
	"AKERDOCK_DATA_DIR",
	"AKERDOCK_WORKER_CONCURRENCY",
	"AKERDOCK_SHUTDOWN_TIMEOUT",
	"AKERDOCK_TERMINAL_IDLE_TIMEOUT",
	"AKERDOCK_TERMINAL_MAX_DURATION",
	"AKERDOCK_IMAGE",
	"AKERDOCK_CONFIG_FILE",
}

// Load resolves the configuration from vars (the full process environment as
// a map) and readFile (used for AKERDOCK_CONFIG_FILE). It returns the config,
// startup warnings (§7.2), and the exhaustive list of fatal errors, if any.
func Load(vars map[string]string, readFile func(string) ([]byte, error)) (*Config, []string, error) {
	var errs Errors
	var warnings []string

	fileVals := map[string]string{}
	if path := vars["AKERDOCK_CONFIG_FILE"]; path != "" {
		data, err := readFile(path)
		if err != nil {
			errs = append(errs, FieldError{"AKERDOCK_CONFIG_FILE", fmt.Sprintf("unreadable config file %q: %v", path, err)})
		} else {
			raw := map[string]any{}
			if err := yaml.Unmarshal(data, &raw); err != nil {
				errs = append(errs, FieldError{"AKERDOCK_CONFIG_FILE", fmt.Sprintf("invalid YAML in %q: %v", path, err)})
			}
			for k, v := range raw {
				fileVals["AKERDOCK_"+strings.ToUpper(k)] = fmt.Sprintf("%v", v)
			}
		}
	}

	// Environment takes precedence over the file, which beats defaults (§1.1).
	get := func(key string) string {
		if v, ok := vars[key]; ok && v != "" {
			return v
		}
		return fileVals[key]
	}

	cfg := &Config{
		DatabaseURL:   get("AKERDOCK_DATABASE_URL"),
		MasterKeyFile: get("AKERDOCK_MASTER_KEY_FILE"),
		MasterKey:     get("AKERDOCK_MASTER_KEY"),
		InstanceFQDN:  get("AKERDOCK_INSTANCE_FQDN"),
		ACMEEmail:     get("AKERDOCK_ACME_EMAIL"),
		RootEmail:     get("AKERDOCK_ROOT_EMAIL"),
		RootName:      strings.TrimSpace(get("AKERDOCK_ROOT_NAME")),
		RootPassword:  get("AKERDOCK_ROOT_PASSWORD"),
	}

	if cfg.DatabaseURL == "" {
		errs = append(errs, FieldError{"AKERDOCK_DATABASE_URL", "missing (required — PostgreSQL DSN, spec instance-config §2)"})
	}

	// Exactly one master key source (§2.1 note 1).
	switch {
	case cfg.MasterKeyFile == "" && cfg.MasterKey == "":
		errs = append(errs, FieldError{"AKERDOCK_MASTER_KEY_FILE", "missing (required — master key file, spec instance-config §3; see runbook install.md step 2)"})
	case cfg.MasterKeyFile != "" && cfg.MasterKey != "":
		errs = append(errs, FieldError{"AKERDOCK_MASTER_KEY", "conflicts with AKERDOCK_MASTER_KEY_FILE (ambiguous — provide exactly one key source)"})
	case cfg.MasterKey != "":
		warnings = append(warnings, "AKERDOCK_MASTER_KEY: prefer AKERDOCK_MASTER_KEY_FILE — process environments are more exposed than a 0600 file (spec instance-config §2.1)")
	}

	cfg.Mode = ModeAllInOne
	if v := get("AKERDOCK_MODE"); v != "" {
		if m := Mode(v); m == ModeAllInOne || m == ModeAPI || m == ModeWorker || m == ModeScheduler {
			cfg.Mode = m
		} else {
			errs = append(errs, FieldError{"AKERDOCK_MODE", fmt.Sprintf("invalid value %q (expected: all-in-one|api|worker|scheduler)", v)})
		}
	}

	cfg.Port = DefaultPort
	if v := get("AKERDOCK_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err != nil || p < 1 || p > 65535 {
			errs = append(errs, FieldError{"AKERDOCK_PORT", fmt.Sprintf("invalid value %q (expected an integer in 1–65535)", v)})
		} else {
			cfg.Port = p
		}
	}

	cfg.InstancePort = cfg.Port
	if v := get("AKERDOCK_INSTANCE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err != nil || p < 1 || p > 65535 {
			errs = append(errs, FieldError{"AKERDOCK_INSTANCE_PORT", fmt.Sprintf("invalid value %q (expected an integer in 1–65535)", v)})
		} else {
			cfg.InstancePort = p
		}
	}

	// AKERDOCK_ROOT_* is an all-or-nothing trio (§2.1 note 2, §6.3).
	rootSet := 0
	for _, v := range []string{cfg.RootEmail, cfg.RootName, cfg.RootPassword} {
		if v != "" {
			rootSet++
		}
	}
	switch {
	case rootSet == 0: // not provided — fine, onboarding UI can create the root
	case rootSet < 3:
		errs = append(errs, FieldError{"AKERDOCK_ROOT_EMAIL", "AKERDOCK_ROOT_EMAIL/NAME/PASSWORD form an all-or-nothing trio — provide all three or none"})
	default:
		if _, err := mail.ParseAddress(cfg.RootEmail); err != nil || strings.ContainsAny(cfg.RootEmail, " <>") {
			errs = append(errs, FieldError{"AKERDOCK_ROOT_EMAIL", "invalid email address"})
		}
		if cfg.RootName == "" || len(cfg.RootName) > 255 {
			errs = append(errs, FieldError{"AKERDOCK_ROOT_NAME", "must be non-empty after trim and at most 255 characters"})
		}
		if len(cfg.RootPassword) < 12 {
			errs = append(errs, FieldError{"AKERDOCK_ROOT_PASSWORD", "must be at least 12 characters (PRD §10.2)"})
		}
	}

	cfg.LocalhostHost = DefaultLocalhostHost
	if v := strings.TrimSpace(get("AKERDOCK_LOCALHOST_HOST")); v != "" {
		cfg.LocalhostHost = v
	}
	cfg.LocalhostUser = DefaultLocalhostUser
	if v := strings.TrimSpace(get("AKERDOCK_LOCALHOST_USER")); v != "" {
		cfg.LocalhostUser = v
	}

	cfg.Timezone = DefaultTimezone
	if v := get("AKERDOCK_TIMEZONE"); v != "" {
		if _, err := time.LoadLocation(v); err != nil {
			errs = append(errs, FieldError{"AKERDOCK_TIMEZONE", fmt.Sprintf("unknown IANA timezone %q", v)})
		} else {
			cfg.Timezone = v
		}
	}

	cfg.LogLevel = DefaultLogLevel
	if v := get("AKERDOCK_LOG_LEVEL"); v != "" {
		switch v {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = v
		default:
			errs = append(errs, FieldError{"AKERDOCK_LOG_LEVEL", fmt.Sprintf("invalid value %q (expected: debug|info|warn|error)", v)})
		}
	}

	cfg.LogFormat = DefaultLogFormat
	if v := get("AKERDOCK_LOG_FORMAT"); v != "" {
		switch v {
		case "json", "text":
			cfg.LogFormat = v
		default:
			errs = append(errs, FieldError{"AKERDOCK_LOG_FORMAT", fmt.Sprintf("invalid value %q (expected: json|text)", v)})
		}
	}

	cfg.DataDir = DefaultDataDir
	if v := get("AKERDOCK_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}

	cfg.Image = get("AKERDOCK_IMAGE")

	cfg.WorkerConcurrency = DefaultWorkerConcurrency
	if v := get("AKERDOCK_WORKER_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 {
			errs = append(errs, FieldError{"AKERDOCK_WORKER_CONCURRENCY", fmt.Sprintf("invalid value %q (expected an integer >= 1)", v)})
		} else {
			cfg.WorkerConcurrency = n
		}
	}

	cfg.ShutdownTimeout = DefaultShutdownTimeout
	if v := get("AKERDOCK_SHUTDOWN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			errs = append(errs, FieldError{"AKERDOCK_SHUTDOWN_TIMEOUT", fmt.Sprintf("invalid value %q (expected a Go duration such as 30s or 2m)", v)})
		} else {
			cfg.ShutdownTimeout = d
		}
	}

	cfg.SchedulerTick = DefaultSchedulerTick
	if v := get("AKERDOCK_SCHEDULER_TICK"); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			errs = append(errs, FieldError{"AKERDOCK_SCHEDULER_TICK", fmt.Sprintf("invalid value %q (expected a Go duration such as 30s)", v)})
		} else {
			cfg.SchedulerTick = d
		}
	}

	cfg.RetryBase = DefaultRetryBase
	if v := get("AKERDOCK_RETRY_BASE"); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			errs = append(errs, FieldError{"AKERDOCK_RETRY_BASE", fmt.Sprintf("invalid value %q (expected a Go duration such as 5s)", v)})
		} else {
			cfg.RetryBase = d
		}
	}

	cfg.TerminalIdleTimeout = DefaultTerminalIdleTimeout
	if v := get("AKERDOCK_TERMINAL_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			errs = append(errs, FieldError{"AKERDOCK_TERMINAL_IDLE_TIMEOUT", fmt.Sprintf("invalid value %q (expected a Go duration such as 15m)", v)})
		} else {
			cfg.TerminalIdleTimeout = d
		}
	}

	cfg.TerminalMaxDuration = DefaultTerminalMaxDuration
	if v := get("AKERDOCK_TERMINAL_MAX_DURATION"); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			errs = append(errs, FieldError{"AKERDOCK_TERMINAL_MAX_DURATION", fmt.Sprintf("invalid value %q (expected a Go duration such as 4h)", v)})
		} else {
			cfg.TerminalMaxDuration = d
		}
	}

	if strings.Contains(cfg.DatabaseURL, "sslmode=disable") {
		warnings = append(warnings, "AKERDOCK_DATABASE_URL: sslmode=disable — acceptable only inside the compose network (spec instance-config §7.2)")
	}

	warnings = append(warnings, unknownVarWarnings(vars)...)

	if len(errs) > 0 {
		return nil, warnings, errs
	}
	return cfg, warnings, nil
}

// unknownVarWarnings flags AKERDOCK_* variables the binary does not read,
// with a closest-match suggestion (typo detection, §7.2).
func unknownVarWarnings(vars map[string]string) []string {
	known := map[string]bool{}
	for _, k := range envKeys {
		known[k] = true
	}
	var names []string
	for k := range vars {
		if strings.HasPrefix(k, "AKERDOCK_") && !known[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		w := fmt.Sprintf("%s: unknown variable, ignored", name)
		if s := closestKey(name); s != "" {
			w += fmt.Sprintf(" (did you mean %s?)", s)
		}
		out = append(out, w)
	}
	return out
}

func closestKey(name string) string {
	best, bestDist := "", 4 // suggest only close typos
	for _, k := range envKeys {
		if d := levenshtein(name, k); d < bestDist {
			best, bestDist = k, d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
