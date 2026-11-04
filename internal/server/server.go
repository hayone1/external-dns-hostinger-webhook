// Package server runs the two HTTP servers of the webhook: the provider API
// that external-dns calls, and the health and metrics server for Kubernetes
// and Prometheus.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"sigs.k8s.io/external-dns/provider"
	"sigs.k8s.io/external-dns/provider/webhook/api"
)

// MaxBodySize is the largest request body the provider API accepts. It
// matches the external-dns default.
const MaxBodySize = 32 << 20

// Status tracks whether the process is alive and ready for traffic.
type Status struct {
	healthy atomic.Bool
	ready   atomic.Bool
}

// SetHealthy sets the liveness flag.
func (s *Status) SetHealthy(v bool) { s.healthy.Store(v) }

// SetReady sets the readiness flag.
func (s *Status) SetReady(v bool) { s.ready.Store(v) }

// HealthHandler serves /healthz (liveness), /readyz (readiness) and /metrics.
func HealthHandler(status *Status, registry *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeFlag(w, status.healthy.Load())
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeFlag(w, status.healthy.Load() && status.ready.Load())
	})
	if registry != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry}))
	}
	return mux
}

func writeFlag(w http.ResponseWriter, ok bool) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ok\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// WebhookHandler serves the external-dns webhook API. It reuses the upstream
// handlers so that content negotiation and JSON handling always match what
// external-dns expects.
func WebhookHandler(p provider.Provider, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ws := &api.WebhookServer{Provider: p, MaxBodySize: MaxBodySize}
	mux := http.NewServeMux()
	mux.HandleFunc("/", ws.NegotiateHandler)
	mux.HandleFunc(api.UrlRecords, ws.RecordsHandler)
	mux.HandleFunc(api.UrlAdjustEndpoints, ws.AdjustEndpointsHandler)
	return logRequests(mux, log)
}

func logRequests(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := slog.LevelDebug
		if rec.status >= 500 {
			level = slog.LevelWarn
		}
		log.Log(r.Context(), level, "webhook request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// New builds an http.Server with safe timeouts.
//
// WriteTimeout is long on purpose: applying a big batch of changes can take
// a while because of Hostinger's rate limit.
func New(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
}

// Serve listens on srv.Addr and serves until ctx is done, then shuts down
// gracefully. started is closed once the listener is open.
func Serve(ctx context.Context, srv *http.Server, started chan<- struct{}) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", srv.Addr)
	if err != nil {
		return err
	}
	if started != nil {
		close(started)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
