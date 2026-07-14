// Command akerdock is the AkerDock control plane.
//
// Per ADR-021 it ships as a single static binary. The run mode comes from
// the first CLI argument, falling back to AKERDOCK_MODE (instance-config
// §2.1): all-in-one (default), api, worker or scheduler. The extra
// "healthcheck" subcommand performs a local health probe with exit code
// 0/1 — it is the compose healthcheck, since the distroless image has no
// shell (§6.6).
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
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/bootstrap"
	"github.com/deepteams/akerdock/internal/config"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/events"
	"github.com/deepteams/akerdock/internal/handlers"
	"github.com/deepteams/akerdock/internal/httpserver"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/notify"
	"github.com/deepteams/akerdock/internal/postgres"

	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/scheduler"
	"github.com/deepteams/akerdock/internal/session"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/telemetry"
	"github.com/deepteams/akerdock/internal/web"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && (args[0] == "-version" || args[0] == "--version" || args[0] == "version") {
		fmt.Println(version)
		return 0
	}
	if len(args) > 0 && args[0] == "healthcheck" {
		return healthcheck()
	}

	vars := environMap()
	// The first CLI argument takes precedence over AKERDOCK_MODE (§2.1).
	if len(args) > 0 {
		vars["AKERDOCK_MODE"] = args[0]
	}

	cfg, warnings, err := config.Load(vars, os.ReadFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL %v\n", err)
		return 1
	}

	logger := newLogger(cfg)
	logger.Info("akerdock starting", "version", version, "mode", string(cfg.Mode), "port", cfg.Port)
	for _, w := range warnings {
		logger.Warn(w)
	}

	if err := ensureDataDir(cfg.DataDir); err != nil {
		logger.Error("fatal", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	// Traces and metrics go to OTLP or nowhere (ADR-008): with no
	// OTEL_EXPORTER_OTLP_ENDPOINT the providers are no-ops, so every
	// instrumented call site below stays unconditional.
	tel := telemetry.Init(ctx, version, logger)
	defer tel.Shutdown(context.WithoutCancel(ctx))
	metrics := telemetry.NewMetrics(tel.Meter)
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
	recorder := &audit.Recorder{Store: q, Logger: logger}
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
		worker.Register(jobs.TypeServerValidate, (&jobs.ServerValidate{Store: q, Keyring: keyring, Logger: logger}).Execute)
		worker.Register(jobs.TypeDeploymentRun, (&jobs.DeploymentRun{Store: q, Keyring: keyring, Audit: recorder, Logger: logger}).Execute)
		worker.Register(jobs.TypeApplicationDelete, (&jobs.ApplicationDelete{Store: q, Keyring: keyring, Logger: logger}).Execute)
		worker.Register(jobs.TypeApplyRouting, (&jobs.ApplyRouting{Store: q, Keyring: keyring, Logger: logger}).Execute)
		db := &jobs.DatabaseRun{Store: q, Keyring: keyring, Logger: logger}
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
		worker.Register(jobs.TypePreviewDestroy, (&jobs.PreviewDestroy{Store: q, Keyring: keyring, Logger: logger}).Execute)
		worker.Register(jobs.TypeBackupExecute, backup.Execute)
		worker.Register(jobs.TypeBackupRestore, backup.Execute)
		worker.Register(jobs.TypeBackupDrill, backup.Execute)
		worker.Register(jobs.TypeScheduledTaskRun, (&jobs.ScheduledTaskRun{Store: q, Keyring: keyring, Audit: recorder, Logger: logger}).Execute)
		lifecycle := &jobs.ApplicationLifecycle{Store: q, Keyring: keyring, Logger: logger}
		proxyLifecycle := &jobs.ProxyLifecycle{Store: q, Keyring: keyring, Logger: logger}
		for _, t := range []string{jobs.TypeProxyStart, jobs.TypeProxyStop, jobs.TypeProxyRestart} {
			worker.Register(t, proxyLifecycle.Execute)
		}
		worker.Register(jobs.TypeApplicationStart, lifecycle.Execute)
		worker.Register(jobs.TypeApplicationStop, lifecycle.Execute)
		worker.Register(jobs.TypeApplicationRestart, lifecycle.Execute)
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

		apiHandler = handlers.NewRouter(&handlers.API{
			Sessions: sessions,
			Passkeys: passkeys,
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

func newLogger(cfg *config.Config) *slog.Logger {
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
	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler)
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
