// Package httpserver serves the single control-plane port (ADR-021,
// instance-config §2.1 AKERDOCK_PORT).
package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// HealthCheck reports process health: nil means database reachable and
// master key loaded (instance-config §6.6).
type HealthCheck func(ctx context.Context) error

// New builds the HTTP server for the given port and handler.
func New(port int, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// HealthOnly is the handler of pure worker/scheduler modes: the port
// serves nothing but /api/v1/health (instance-config §6.1 step 6).
func HealthOnly(health HealthCheck, logger *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/v1/health", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := health(req.Context()); err != nil {
			logger.Warn("health check failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unavailable"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	return r
}
