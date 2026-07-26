// Command akerdock is both the AkerDock control plane and its local CLI
// (ADR-033). The command tree is Cobra: `serve <mode>` runs the server
// (all-in-one/api/worker/scheduler, falling back to AKERDOCK_MODE), the
// distroless compose probe is `healthcheck`, and the client subcommands
// (login, ls, logs, shell, port-forward…) live in internal/cli. A legacy
// bare mode (`akerdock all-in-one`) is still accepted, with a deprecation
// warning, for one release.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/bootstrap"
	"github.com/deepteams/akerdock/internal/cli"
	"github.com/deepteams/akerdock/internal/config"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/events"
	"github.com/deepteams/akerdock/internal/handlers"
	"github.com/deepteams/akerdock/internal/httpserver"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/notify"
	"github.com/deepteams/akerdock/internal/postgres"

	"github.com/deepteams/akerdock/internal/oidc"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/scheduler"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/telemetry"
	"github.com/deepteams/akerdock/internal/web"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// serverModes are the run modes accepted both by `serve <mode>` and, for one
// release, as a bare legacy argument (`akerdock all-in-one`).
var serverModes = map[string]bool{
	string(config.ModeAllInOne): true, string(config.ModeAPI): true,
	string(config.ModeWorker): true, string(config.ModeScheduler): true,
}

func main() {
	// Legacy fallback (ADR-033): a bare server mode as the first argument is
	// rewritten to `serve <mode>` with a deprecation warning, so existing
	// compose files and runbooks keep working until the next major.
	args := os.Args[1:]
	if len(args) > 0 && serverModes[args[0]] {
		fmt.Fprintf(os.Stderr,
			"warning: `akerdock %s` is deprecated — use `akerdock serve %s` (ADR-033)\n", args[0], args[0])
		os.Args = append([]string{os.Args[0], "serve"}, args...)
	}
	if err := rootCommand().Execute(); err != nil {
		// SilenceErrors keeps Cobra from double-printing usage on a runtime
		// failure; we print the error ourselves so it is never swallowed.
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// rootCommand assembles the Cobra tree: server commands here, client
// subcommands from internal/cli.
func rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "akerdock",
		Short:         "AkerDock — control plane and local CLI",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	serve := &cobra.Command{
		Use:       "serve [all-in-one|api|worker|scheduler]",
		Short:     "Run the control plane (mode falls back to AKERDOCK_MODE)",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"all-in-one", "api", "worker", "scheduler"},
		RunE: func(_ *cobra.Command, args []string) error {
			mode := ""
			if len(args) == 1 {
				mode = args[0]
			}
			if code := serveRun(mode); code != 0 {
				return fmt.Errorf("server exited with code %d", code)
			}
			return nil
		},
	}

	healthcheckCmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe the local health endpoint (compose healthcheck)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if code := healthcheck(); code != 0 {
				return fmt.Errorf("unhealthy")
			}
			return nil
		},
	}

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Println(version)
			return nil
		},
	}

	root.AddCommand(serve, healthcheckCmd, versionCmd)
	cli.AddCommands(root, version)
	return root
}

// serveRun boots the control plane in the given mode ("" = AKERDOCK_MODE or
// the default). Returns a process exit code.
func serveRun(mode string) int {
	vars := environMap()
	// An explicit `serve <mode>` argument takes precedence over AKERDOCK_MODE (§2.1).
	if mode != "" {
		vars["AKERDOCK_MODE"] = mode
	}

	cfg, warnings, err := config.Load(vars, os.ReadFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL %v\n", err)
		return 1
	}

	baseHandler := loggerHandler(cfg)
	logger := slog.New(baseHandler)
	logger.Info("akerdock starting", "version", version, "mode", string(cfg.Mode), "port", cfg.Port)
	for _, w := range warnings {
		logger.Warn(w)
	}

	if err := ensureDataDir(cfg.DataDir); err != nil {
		logger.Error("fatal", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	queue.RetryBase = cfg.RetryBase
	defer stop()

	// Startup sequence, each step blocking (instance-config §6.1).
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		logger.Error("fatal", "error", err)
		return 1
	}
	defer pool.Close()

	if err := postgres.Migrate(ctx, cfg.DatabaseURL, logger); err != nil {
		logger.Error("fatal", "error", err)
		return 1
	}

	keyring, err := loadKeyring(cfg, logger)
	if err != nil {
		logger.Error("fatal", "error", err)
		return 1
	}
	logger.Info("master key loaded", "active_version", keyring.ActiveVersion())

	if err := bootstrap.Run(ctx, pool, cfg, keyring, logger); err != nil {
		logger.Error("fatal", "error", err)
		return 1
	}

	q := store.New(pool)
	settings := instance.NewCache(q)

	// Telemetry is initialized here — AFTER the DB and keyring — so the OTLP
	// export config stored in the instance settings (encrypted, §14.2) can be
	// read; it falls back to the OTEL_* environment otherwise. A change to the
	// stored config takes effect at the next restart (ADR-008/§27.8).
	otlp := telemetry.EnvConfig()
	if st, err := settings.Get(ctx); err == nil {
		if stored, ok := handlers.DecodeOtlpConfig(st.OtlpConfigEnc, keyring); ok {
			stored.PromEnabled = otlp.PromEnabled // the /metrics scrape stays env-driven
			otlp = stored
		}
	}
	tel := telemetry.Init(ctx, version, otlp, logger)
	defer tel.Shutdown(context.WithoutCancel(ctx))
	metrics := telemetry.NewMetrics(tel.Meter)
	// With the LoggerProvider now set (if any), fan logs to the OTLP bridge too;
	// the bridge captures the provider at construction, hence after Init.
	if tel.Enabled() {
		logger = slog.New(multiHandler{baseHandler, otelslog.NewHandler(telemetry.ScopeName())})
	}

	recorder := &audit.Recorder{Store: q, Logger: logger, Metrics: metrics}
	broker := events.NewBroker()
	// The outbox publisher runs wherever the API serves SSE, and in workers
	// (a worker-only deployment still has to publish its events).
	go (&events.Publisher{Store: q, Broker: broker, Logger: logger}).Run(ctx)

	// Maintenance crons run in scheduler and all-in-one modes, under a
	// PostgreSQL advisory lock so only one instance is active (§18.2).
	if cfg.Mode == config.ModeScheduler || cfg.Mode == config.ModeAllInOne {
		dispatcher := &notify.Dispatcher{Store: q, Keyring: keyring, Sender: notify.New(), Logger: logger}
		go (&scheduler.Scheduler{
			Tick: cfg.SchedulerTick,
			Pool: pool, Store: q, Keyring: keyring, Audit: recorder,
			Dispatcher: dispatcher, Logger: logger,
			TerminalMaxDuration: cfg.TerminalMaxDuration,
		}).Run(ctx)
	}

	// Queue consumption runs in worker and all-in-one modes (§18.2).
	var worker *queue.Worker
	if cfg.Mode == config.ModeWorker || cfg.Mode == config.ModeAllInOne {
		worker = queue.NewWorker(q, cfg.WorkerConcurrency, logger)
		worker.Metrics, worker.Tracer = metrics, tel.Tracer
		worker.Register(jobs.TypeServerValidate, (&jobs.ServerValidate{Store: q, Keyring: keyring, Logger: logger, ControlPlanePort: cfg.InstancePort}).Execute)
		worker.Register(jobs.TypeDeploymentRun, (&jobs.DeploymentRun{Store: q, Keyring: keyring, Audit: recorder, Logger: logger, ControlPlanePort: cfg.InstancePort}).Execute)
		worker.Register(jobs.TypeApplicationDelete, (&jobs.ApplicationDelete{Store: q, Keyring: keyring, Logger: logger}).Execute)
		worker.Register(jobs.TypeApplyRouting, (&jobs.ApplyRouting{Store: q, Keyring: keyring, Logger: logger}).Execute)
		db := &jobs.DatabaseRun{Store: q, Keyring: keyring, Logger: logger, ControlPlanePort: cfg.InstancePort}
		for _, t := range []string{jobs.TypeDatabaseProvision, jobs.TypeDatabaseStart, jobs.TypeDatabaseStop, jobs.TypeDatabaseRestart, jobs.TypeDatabaseDelete} {
			worker.Register(t, db.Execute)
		}
		worker.Register(jobs.TypeEncryptionRotate, (&jobs.EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).Execute)
		certs := &jobs.CertificateSync{Store: q, Keyring: keyring, Logger: logger}
		worker.Register(jobs.TypeCertificateSync, certs.Execute)
		worker.Register(jobs.TypeCertificateRenew, certs.Execute)
		backup := &jobs.BackupRun{Store: q, Keyring: keyring, Audit: recorder, Logger: logger}
		webhookProcess := &jobs.WebhookProcess{Store: q, Keyring: keyring, Logger: logger}
		worker.Register(jobs.TypeWebhookProcess, webhookProcess.Execute)
		worker.Register(jobs.TypeGithubAppPush, (&jobs.GithubAppPush{Store: q, Logger: logger}).Execute)
		worker.Register(jobs.TypeGithubAppPullRequest, (&jobs.GithubAppPullRequest{Store: q, Keyring: keyring, Logger: logger}).Execute)
		worker.Register(jobs.TypeGithubAppIssueComment, (&jobs.GithubAppIssueComment{Store: q, Keyring: keyring, Logger: logger}).Execute)
		worker.Register(jobs.TypePreviewDestroy, (&jobs.PreviewDestroy{Store: q, Keyring: keyring, Logger: logger, Audit: recorder}).Execute)
		worker.Register(jobs.TypeBackupExecute, backup.Execute)
		worker.Register(jobs.TypeBackupRestore, backup.Execute)
		worker.Register(jobs.TypeBackupDrill, backup.Execute)
		worker.Register(jobs.TypeScheduledTaskRun, (&jobs.ScheduledTaskRun{Store: q, Keyring: keyring, Audit: recorder, Logger: logger}).Execute)
		lifecycle := &jobs.ApplicationLifecycle{Store: q, Keyring: keyring, Logger: logger}
		proxyLifecycle := &jobs.ProxyLifecycle{Store: q, Keyring: keyring, Logger: logger, ControlPlanePort: cfg.InstancePort}
		for _, t := range []string{jobs.TypeProxyStart, jobs.TypeProxyStop, jobs.TypeProxyRestart} {
			worker.Register(t, proxyLifecycle.Execute)
		}
		worker.Register(jobs.TypeApplicationStart, lifecycle.Execute)
		worker.Register(jobs.TypeApplicationStop, lifecycle.Execute)
		worker.Register(jobs.TypeApplicationRestart, lifecycle.Execute)
		adoptionJobs := &jobs.Adoption{Store: q, Pool: pool, Keyring: keyring, Logger: logger}
		worker.Register(jobs.TypeAdoptionScan, adoptionJobs.ExecuteScan)
		worker.Register(jobs.TypeAdoptionAdopt, adoptionJobs.ExecuteAdopt)
		worker.Register(jobs.TypeResourceDisown, adoptionJobs.ExecuteDisown)
		worker.Register(jobs.TypeServerCleanup, (&jobs.ServerCleanup{Store: q, Keyring: keyring, Audit: recorder, Logger: logger}).Execute)
		go worker.Run(ctx)
	}

	// Secure cookies and HSTS require HTTPS. Deriving this from the instance
	// FQDN rather than hardcoding true matters: a Secure cookie is simply never
	// sent over plain HTTP, so on an instance reached by IP the operator would
	// log in successfully and then be bounced straight back to the login page,
	// with nothing in the logs to explain why.
	secureInstance := false
	if st, err := settings.Get(ctx); err == nil && st.Fqdn != nil && *st.Fqdn != "" {
		secureInstance = true
	}

	var apiHandler http.Handler
	switch cfg.Mode {
	case config.ModeWorker, config.ModeScheduler:
		// Pure background modes: the port only serves the health endpoint
		// (§6.1 step 6).
		apiHandler = httpserver.HealthOnly(pool.Ping, logger)
	default: // all-in-one, api
		secureCookies := secureInstance
		sessions := &session.Manager{Store: q, Secure: secureCookies}
		if !secureCookies {
			logger.Warn("session cookies are not marked Secure — set AKERDOCK_INSTANCE_FQDN and serve over HTTPS")
		}

		// Passkeys (WebAuthn): the relying party is PINNED to the instance FQDN,
		// never derived from the Host header — a derived RP ID would let anyone
		// who can reach the server under another name mint credentials for it.
		// Without an FQDN the fallback is localhost, the one origin browsers
		// treat as secure over plain HTTP: passkeys keep working on a dev
		// instance and nowhere else.
		fqdn := ""
		if st, err := settings.Get(ctx); err == nil && st.Fqdn != nil {
			fqdn = *st.Fqdn
		}
		rpID, origins := session.RelyingParty(fqdn, cfg.Port)
		if fqdn == "" {
			logger.Warn("passkeys pinned to localhost — set AKERDOCK_INSTANCE_FQDN to enrol passkeys on a real origin")
		}
		passkeys, err := session.NewPasskeys(q, sessions, rpID, "AkerDock", origins)
		if err != nil {
			// A broken RP config must not take the control plane down: password
			// login still works, passkey endpoints answer 404.
			logger.Warn("passkeys disabled", "error", err)
		}

		// OAuth/OIDC login (§10.2): the callback URL is pinned to the instance
		// FQDN for the same reason the passkey RP is — anything answering
		// under another name must not be able to finish a login. Same
		// localhost fallback for TLS-less dev instances.
		baseURL := "https://" + fqdn
		if fqdn == "" {
			baseURL = fmt.Sprintf("http://localhost:%d", cfg.Port)
		}

		apiHandler = handlers.NewRouter(&handlers.API{
			Sessions: sessions,
			Passkeys: passkeys,
			MFA:      &session.TOTP{Store: q, Sessions: sessions, Keyring: keyring},
			OAuth: &session.OAuth{
				Store: q, Sessions: sessions, Keyring: keyring,
				Settings: settings, Client: oidc.New(), BaseURL: baseURL,
			},
			Store:    q,
			Pool:     pool,
			Settings: settings,
			Keyring:  keyring,
			Audit:    recorder,
			Events:   broker,
			Version:  version,
			Logger:   logger,

			TerminalIdleTimeout: cfg.TerminalIdleTimeout,
			TerminalMaxDuration: cfg.TerminalMaxDuration,
		}, &auth.Middleware{Store: q, Settings: settings, Sessions: sessions, Logger: logger})
	}
	// otelhttp wraps the whole handler: one server span per request, with the
	// route as the span name — not the raw path, which would explode the
	// cardinality with one span name per UUID.
	if tel.Enabled() {
		apiHandler = otelhttp.NewHandler(apiHandler, "akerdock",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				if route := chi.RouteContext(r.Context()); route != nil && route.RoutePattern() != "" {
					return r.Method + " " + route.RoutePattern()
				}
				return r.Method
			}))
	}
	// The dashboard, /metrics and the API share the single port of the control
	// plane (§27.1, ADR-021). The mux routes by prefix: everything the API and
	// the webhooks do not claim falls through to the SPA.
	ui := web.Handler()
	if tel.PromHandler != nil || ui != nil {
		mux := http.NewServeMux()
		if tel.PromHandler != nil {
			// Unauthenticated and revealing: opt-in only (AKERDOCK_METRICS_ENABLED).
			mux.Handle("/metrics", tel.PromHandler)
		}
		// Everything the server owns keeps its path; only what is left falls
		// through to the SPA. Forgetting one of these prefixes means the
		// dashboard silently answers an endpoint with HTML — which is exactly
		// what happened to /auth the first time.
		mux.Handle("/api/", apiHandler)
		mux.Handle("/webhooks/", apiHandler)
		mux.Handle("/auth/", apiHandler)
		mux.Handle("/terminal/", apiHandler)
		// The CLI TCP tunnel WebSocket (ADR-032), like /terminal/ws: outside
		// the OpenAPI contract, served by the API.
		mux.Handle("/tunnel/", apiHandler)
		// The preview SSO callback (ADR-030) arrives under the PREVIEW's
		// host, proxied here by its dedicated router — served by the API,
		// never by the dashboard.
		mux.Handle("/.akerdock/", apiHandler)
		if ui != nil {
			mux.Handle("/", ui)
			logger.Info("dashboard embedded and served on /")
		} else {
			mux.Handle("/", apiHandler)
		}
		apiHandler = mux
	}
	// Security headers wrap the WHOLE port — dashboard, /auth, API, /metrics:
	// a header applied per-route is a header forgotten on the next route.
	apiHandler = httpserver.SecurityHeaders(secureInstance)(apiHandler)
	srv := httpserver.New(cfg.Port, apiHandler)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		logger.Error("fatal", "error", err)
		return 1
	case <-ctx.Done():
	}

	// Graceful shutdown: stop accepting requests, drain for at most
	// AKERDOCK_SHUTDOWN_TIMEOUT (§6.5).
	logger.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown incomplete", "error", err)
	}
	if worker != nil {
		// Drain in-flight jobs; heartbeats keep their leases alive (§6.5).
		worker.Wait(cfg.ShutdownTimeout)
	}
	return 0
}

// loadKeyring loads the master key from the configured source and runs the
// AEAD self-test (§6.1 step 4).
func loadKeyring(cfg *config.Config, logger *slog.Logger) (*envelope.Keyring, error) {
	var kr *envelope.Keyring
	var err error
	if cfg.MasterKeyFile != "" {
		var warnings []string
		kr, warnings, err = envelope.LoadFile(cfg.MasterKeyFile)
		for _, w := range warnings {
			logger.Warn(w)
		}
	} else {
		kr, err = envelope.Parse([]byte(cfg.MasterKey))
	}
	if err != nil {
		return nil, err
	}
	if err := kr.SelfTest(); err != nil {
		return nil, err
	}
	return kr, nil
}

func ensureDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("AKERDOCK_DATA_DIR %q: not creatable: %w", dir, err)
	}
	probe, err := os.CreateTemp(dir, ".writable-*")
	if err != nil {
		return fmt.Errorf("AKERDOCK_DATA_DIR %q: not writable: %w", dir, err)
	}
	_ = probe.Close()
	return os.Remove(probe.Name())
}

// healthcheck probes the local health endpoint (instance-config §6.6).
func healthcheck() int {
	port := config.DefaultPort
	if v := os.Getenv("AKERDOCK_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

func loggerHandler(cfg *config.Config) slog.Handler {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.NewJSONHandler(os.Stderr, opts)
}

// multiHandler fans one slog record out to several handlers — here the local
// stderr handler and the OTLP log bridge, so logs stay on the console AND ship
// to the collector once its LoggerProvider is set.
type multiHandler []slog.Handler

func (m multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}

func environMap() map[string]string {
	vars := make(map[string]string)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	return vars
}
