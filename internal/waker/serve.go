package waker

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Serve runs the waker HTTP server on addr, forwarding for the routing table in
// dir/routes.json and waking targets on demand. The routing file is reloaded
// when its modification time changes, so the control plane can add or remove
// scale-to-zero resources without restarting the container. agent, when
// enabled (ADR-040 enrollment injected at container creation), pushes
// outbound observations alongside — its failure modes never touch the wake
// path.
func Serve(ctx context.Context, dir, addr string, docker Docker, agentCfg AgentConfig, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	activity := FileActivity{Dir: dir}

	var agent *Agent
	if agentCfg.Enabled() {
		events, _ := docker.(eventStreamer) // the socket client streams; fakes may not
		agent = NewAgent(agentCfg, events, logger)
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
		logger.Info("waker: routing config loaded", "routes", len(cfg.Routes), "resources", len(cfg.Resources))
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
