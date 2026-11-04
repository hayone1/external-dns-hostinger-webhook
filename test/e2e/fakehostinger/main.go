// Command fakehostinger serves the in-memory fake Hostinger API from
// internal/hostinger/hostingertest over HTTP. The end-to-end test runs it
// inside the cluster, so CI never needs a real Hostinger account.
//
// It is a test tool only: no TLS, a fixed token, data lost on restart.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hayone1/external-dns-hostinger-webhook/internal/hostinger/hostingertest"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	zones := flag.String("zones", "example.com", "comma separated zones to create at start")
	defaults := flag.Bool("default-records", true, "give each zone the records of a fresh Hostinger zone (apex A, www CNAME)")
	rateLimit := flag.Int("rate-limit", 0, "answer HTTP 429 after this many requests per minute (0 = no limit)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fake := hostingertest.NewHandler()
	for _, z := range strings.Split(*zones, ",") {
		if z = strings.ToLower(strings.TrimSpace(z)); z != "" {
			if *defaults {
				fake.AddZone(z)
			} else {
				fake.AddBareZone(z)
			}
			log.Info("zone created", "zone", z, "defaultRecords", *defaults)
		}
	}
	if *rateLimit > 0 {
		fake.SetRateLimit(*rateLimit, time.Minute)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(fake.Handler(), log),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("fake Hostinger API listening", "addr", *addr, "token", hostingertest.Token)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "error", err)
		stop()
		os.Exit(1) //nolint:gocritic // stop() already ran
	}
}

func logRequests(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &status{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/healthz" {
			log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.code)
		}
	})
}

type status struct {
	http.ResponseWriter
	code int
}

func (s *status) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}
