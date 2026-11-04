// Command webhook is an external-dns webhook provider for Hostinger DNS.
//
// Usage:
//
//	webhook              run the webhook (the default)
//	webhook check        test the configuration and the API token, list zones, exit
//	webhook healthcheck  exit 0 if the local /healthz answers ok (for Docker HEALTHCHECK)
//	webhook version      print the version
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"sigs.k8s.io/external-dns/endpoint"

	"github.com/hayone1/external-dns-hostinger-webhook/internal/config"
	"github.com/hayone1/external-dns-hostinger-webhook/internal/hostinger"
	"github.com/hayone1/external-dns-hostinger-webhook/internal/metrics"
	"github.com/hayone1/external-dns-hostinger-webhook/internal/provider"
	"github.com/hayone1/external-dns-hostinger-webhook/internal/server"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "none"
)

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "", "serve":
		os.Exit(serve())
	case "check":
		os.Exit(check())
	case "healthcheck":
		os.Exit(healthcheck())
	case "version", "--version", "-v":
		fmt.Printf("external-dns-hostinger-webhook %s (commit %s)\n", version, commit)
	case "help", "--help", "-h":
		fmt.Println("usage: webhook [serve|check|healthcheck|version]")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\nusage: webhook [serve|check|healthcheck|version]\n", cmd)
		os.Exit(2)
	}
}

func serve() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		return 2
	}
	log := newLogger(cfg, os.Stderr)
	log.Info("starting external-dns-hostinger-webhook", "version", version, "commit", commit,
		"dryRun", cfg.DryRun, "domainFilter", cfg.DomainFilter, "excludeDomains", cfg.ExcludeDomainFilter,
		"rateLimit", cfg.RateLimit, "defaultTTL", cfg.DefaultTTL)
	if cfg.DryRun {
		log.Warn("DRY_RUN is on: changes are logged and validated, but never written to Hostinger")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := metrics.New(version)
	status := &server.Status{}
	status.SetHealthy(true)

	healthErr := make(chan error, 1)
	healthSrv := server.New(cfg.HealthAddress(), server.HealthHandler(status, m.Registry))
	go func() { healthErr <- server.Serve(ctx, healthSrv, nil) }()
	log.Info("health and metrics server listening", "address", cfg.HealthAddress())

	client, err := newClient(cfg, log, m)
	if err != nil {
		log.Error("cannot create Hostinger client", "error", err)
		return 1
	}

	// Wait for a working token. Network problems are retried while the pod
	// stays alive but not ready; a refused token stops the process with a
	// clear message.
	if err := waitForToken(ctx, client, log); err != nil {
		if errors.Is(err, hostinger.ErrInvalidToken) {
			log.Error("Hostinger refused the API token. Check HOSTINGER_API_TOKEN: the token may be wrong, "+
				"revoked or expired. Create a new one in hPanel under Profile > API", "error", err)
			return 1
		}
		log.Info("stopped before startup finished", "reason", err)
		return 0
	}
	log.Info("Hostinger API token accepted")

	p := newProvider(cfg, client, log, m)

	started := make(chan struct{})
	webhookErr := make(chan error, 1)
	webhookSrv := server.New(cfg.WebhookAddress(), server.WebhookHandler(p, log))
	go func() { webhookErr <- server.Serve(ctx, webhookSrv, started) }()

	select {
	case <-started:
		status.SetReady(true)
		log.Info("webhook server listening", "address", cfg.WebhookAddress())
	case err := <-webhookErr:
		log.Error("webhook server failed to start", "address", cfg.WebhookAddress(), "error", err)
		return 1
	}

	code := 0
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received, stopping")
	case err := <-webhookErr:
		log.Error("webhook server stopped", "error", err)
		code = 1
	case err := <-healthErr:
		log.Error("health server stopped", "error", err)
		code = 1
	}
	status.SetReady(false)
	status.SetHealthy(false)
	stop()
	// Give both servers time to finish in-flight requests.
	for _, ch := range []chan error{webhookErr, healthErr} {
		select {
		case err := <-ch:
			if err != nil {
				log.Warn("server did not shut down cleanly", "error", err)
			}
		case <-time.After(35 * time.Second):
		}
	}
	log.Info("stopped")
	return code
}

func waitForToken(ctx context.Context, client *hostinger.Client, log *slog.Logger) error {
	wait := 5 * time.Second
	for {
		err := client.CheckToken(ctx)
		if err == nil || errors.Is(err, hostinger.ErrInvalidToken) {
			return err
		}
		log.Warn("cannot reach Hostinger yet, retrying", "error", err, "retryIn", wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}

// check validates the configuration and the token, then prints the zones
// the webhook would manage. It is meant for people setting things up.
func check() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		return 2
	}
	log := newLogger(cfg, os.Stderr)
	client, err := newClient(cfg, log, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := client.CheckToken(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "token check failed: %v\n", err)
		return 1
	}
	fmt.Println("token ok")

	p := newProvider(cfg, client, log, nil)
	managed, all, err := p.Zones(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listing domains failed: %v\n", err)
		return 1
	}
	usable := 0
	for _, z := range all {
		if !slices.Contains(managed, z) {
			fmt.Printf("zone %-40s skipped (domain filter)\n", z)
			continue
		}
		sets, err := client.GetZone(ctx, z)
		if err != nil {
			fmt.Printf("zone %-40s cannot be read: %v\n", z, err)
			continue
		}
		usable++
		fmt.Printf("zone %-40s managed, %d record sets\n", z, len(sets))
		if d, err := client.GetDomain(ctx, z); err == nil && !usesHostingerNameServers(d.NameServers) {
			fmt.Printf("     warning: %s does not use Hostinger's name servers (it uses %s),\n"+
				"     so the records the webhook writes are not seen on the internet.\n"+
				"     Set the name servers to %s at your domain registrar.\n",
				z, nameServerList(d.NameServers), strings.Join(hostinger.HostingerNameServers, " and "))
		}
	}
	if len(all) == 0 {
		fmt.Println("no domains found in this Hostinger account")
	}
	fmt.Printf("%d zone(s) found, %d managed\n", len(all), usable)
	if usable == 0 {
		return 1
	}
	return 0
}

func usesHostingerNameServers(ns map[string]string) bool {
	if len(ns) == 0 {
		return true // unknown, so do not warn
	}
	for _, v := range ns {
		if v == "" {
			continue
		}
		if !slices.Contains(hostinger.HostingerNameServers, strings.ToLower(strings.TrimSuffix(v, "."))) {
			return false
		}
	}
	return true
}

func nameServerList(ns map[string]string) string {
	keys := make([]string, 0, len(ns))
	for k := range ns {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	vals := make([]string, 0, len(keys))
	for _, k := range keys {
		if ns[k] != "" {
			vals = append(vals, ns[k])
		}
	}
	return strings.Join(vals, ", ")
}

// healthcheck calls the local health endpoint. Distroless images have no
// curl or wget, so the binary checks itself.
func healthcheck() int {
	port := config.DefaultHealthPort
	if v, ok := os.LookupEnv("HEALTH_PORT"); ok {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+"/healthz", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "health check returned HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func newClient(cfg *config.Config, log *slog.Logger, m *metrics.Metrics) (*hostinger.Client, error) {
	return hostinger.New(hostinger.Options{
		APIURL:         cfg.APIURL,
		Token:          cfg.APIToken,
		RequestTimeout: cfg.RequestTimeout,
		RateLimit:      cfg.RateLimit,
		Burst:          cfg.RateBurst,
		MaxRetries:     cfg.MaxRetries,
		UserAgent:      "external-dns-hostinger-webhook/" + version,
		Logger:         log,
		Metrics:        m,
	})
}

func newProvider(cfg *config.Config, client *hostinger.Client, log *slog.Logger, m *metrics.Metrics) *provider.Provider {
	// Plain DOMAIN_FILTER entries double as zone names, so a domain that is
	// registered elsewhere but uses Hostinger DNS can still be managed.
	extra := make([]string, 0, len(cfg.DomainFilter))
	for _, d := range cfg.DomainFilter {
		extra = append(extra, strings.TrimPrefix(d, "."))
	}
	cacheTTL := cfg.ZoneCacheTTL
	if cacheTTL == 0 {
		cacheTTL = -1 // 0 in the config means "do not cache"
	}
	return provider.New(client, provider.Options{
		DomainFilter: domainFilter(cfg),
		ExtraZones:   extra,
		DefaultTTL:   cfg.DefaultTTL,
		DryRun:       cfg.DryRun,
		ZoneCacheTTL: cacheTTL,
		Logger:       log,
		Metrics:      m,
	})
}

func domainFilter(cfg *config.Config) *endpoint.DomainFilter {
	if cfg.RegexDomainFilter != nil || cfg.RegexDomainExclude != nil {
		return endpoint.NewRegexDomainFilter(cfg.RegexDomainFilter, cfg.RegexDomainExclude)
	}
	return endpoint.NewDomainFilterWithExclusions(cfg.DomainFilter, cfg.ExcludeDomainFilter)
}

// newLogger sets up slog for our code and logrus for the external-dns
// handlers, with the same level and format.
func newLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler = slog.NewTextHandler(w, opts)
	if cfg.LogFormat == "json" {
		h = slog.NewJSONHandler(w, opts)
		logrus.SetFormatter(&logrus.JSONFormatter{})
	}
	logrus.SetOutput(w)
	if lv, err := logrus.ParseLevel(cfg.LogLevel); err == nil {
		logrus.SetLevel(lv)
	}
	return slog.New(h)
}
