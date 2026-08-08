package agent

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/hostops"
	"github.com/deepteams/akerdock/internal/proxy"
)

// Serve runs the waker HTTP server on addr, forwarding for the routing table in
// dir/routes.json and waking targets on demand. The routing file is reloaded
// when its modification time changes, so the control plane can add or remove
// scale-to-zero resources without restarting the container. agent, when
// enabled (ADR-040 enrollment injected at container creation), pushes
// outbound observations alongside — its failure modes never touch the wake
// path.
func Serve(ctx context.Context, dir, addr string, rt dockerruntime.Runtime, agentCfg Enrollment, logger *slog.Logger) error {
	return ServeWithTelemetry(ctx, dir, addr, rt, agentCfg, logger, nil)
}

// ServeWithTelemetry is Serve with the optional Prometheus handler produced
// by telemetry.Init. The helper is not published on the host, but collectors
// on its destination network can scrape the same standard /metrics endpoint.
func ServeWithTelemetry(ctx context.Context, dir, addr string, rt dockerruntime.Runtime, agentCfg Enrollment, logger *slog.Logger, metrics http.Handler) error {
	if logger == nil {
		logger = slog.Default()
	}
	activity := FileActivity{Dir: dir}
	docker := NewRuntimeDocker(rt)

	// The ingress module (ADR-060) lives as long as the process: a routing
	// reload updates its host table but must never drop a live tunnel.
	ingress := NewIngress(logger)

	var agent *Agent
	if agentCfg.Enabled() {
		agent = NewAgent(agentCfg, docker, logger)
		// The ADR-052 command channel: enrolled agents execute the control
		// plane's typed commands against the local runtime — and, when the
		// host tree is mounted (ADR-054, spec 7), the file primitives on it.
		host := hostops.DetectLocal(rt)
		if host == nil {
			logger.Info("agent: no host tree mounted — host-ops disabled until this helper is recreated")
		}
		exec := NewExecutor(rt, host, logger)
		exec.Ingress = ingress
		agent.Executor = exec
		ingress.Notify = agent.Push
		go agent.Run(ctx)
	}

	var current atomic.Pointer[Waker]
	load := func() {
		cfg, err := LoadConfig(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				logger.Warn("waker: routing config unreadable", "error", err)
			}
			return
		}
		wk := New(cfg, docker, activity, nil)
		wk.Logger = logger
		if agent != nil {
			wk.OnWake = func(resourceUUID string) {
				agent.Push(Observation{Type: "stz_woken", At: time.Now(), ResourceUUID: resourceUUID})
			}
		}
		current.Store(wk)
		ingress.SetRoutes(cfg.Ingress)
		logger.Info("waker: routing config loaded", "routes", len(cfg.Routes),
			"resources", len(cfg.Resources), "ingress", len(cfg.Ingress))
	}
	load()

	// Reload on mtime change (§8.2: the control plane deposits the file; the
	// waker never generates it).
	go func() {
		routes := filepath.Join(dir, RoutesFile)
		var last time.Time
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if info, err := os.Stat(routes); err == nil && info.ModTime().After(last) {
					last = info.ModTime()
					load()
				}
			}
		}
	}()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if metrics != nil && r.URL.Path == "/metrics" && isAgentMetricsHost(hostname(r.Host)) {
			metrics.ServeHTTP(w, r)
			return
		}
		// Ingress hosts first (ADR-060): they are declared, never scale-to-zero.
		if ingress.Handles(hostname(r.Host)) {
			ingress.ServeHTTP(w, r)
			return
		}
		wk := current.Load()
		if wk == nil {
			http.Error(w, "waker not configured", http.StatusServiceUnavailable)
			return
		}
		wk.ServeHTTP(w, r)
	})

	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	logger.Info("waker: listening", "addr", addr, "dir", dir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func isAgentMetricsHost(host string) bool {
	switch host {
	case proxy.AgentContainerName, "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}
